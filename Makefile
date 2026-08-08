# ─── Uptime Phoenix Makefile ─────────────────────────────────────────────────────────
# All targets use tabs for indentation (Makefile requirement).

APP_NAME := uptime-phoenix
# Release version stamped into the binary: make build VERSION=v1.2.3
# (the -X target lands in internal/version; the Go linker silently ignores
# -X for a symbol that does not exist yet, so this is safe before that
# package lands).
VERSION ?= dev
GO_BUILD_FLAGS := -trimpath -ldflags="-s -w -X github.com/fiztoz/uptime-phoenix/internal/version.Version=$(VERSION)"
DOCKER_TAG := $(APP_NAME):latest
COMPOSE_DEPS := docker-compose.deps.yml
GO_BIN := $(shell go env GOPATH)/bin
AIR := $(GO_BIN)/air
GOLANGCI_LINT := $(GO_BIN)/golangci-lint
GOLANGCI_LINT_VERSION := v2.12.2
GOVULNCHECK := $(GO_BIN)/govulncheck
export GOTOOLCHAIN := go1.25.12
export PATH := $(GO_BIN):$(PATH)

# Defaults — override via .env or: make dev-split MARIADB_PASSWORD=secret
MARIADB_PASSWORD ?= change_me_app
MARIADB_ROOT_PASSWORD ?= change_me_root
JWT_SECRET ?= change_me_jwt
BOOTSTRAP_USERNAME ?= admin
BOOTSTRAP_PASSWORD ?= ChangeMe123!
LOG_LEVEL ?= debug

# Local API env when Docker runs MariaDB + Redis (split dev).
DEV_SPLIT_DB_DSN := phoenix:$(MARIADB_PASSWORD)@tcp(127.0.0.1:3306)/phoenix?parseTime=true&loc=UTC&multiStatements=true

# ── Development ───────────────────────────────────────────────────────────────

.PHONY: ensure-air
ensure-air: ## Install air hot-reload tool if missing
	@test -x '$(AIR)' || go install github.com/air-verse/air@latest

.PHONY: dev
dev: ensure-air ## Run Go backend + frontend dev server (hot-reload, backend :3000, frontend :5173)
	@echo "Starting Phoenix dev environment (backend :3000, frontend :5173)..."
	@test -d web/node_modules || (cd web && bun install --frozen-lockfile)
	@trap 'kill 0' INT TERM; \
		(cd web && bun run dev) & \
		env WS_ALLOWED_ORIGINS='localhost:5173' '$(AIR)' -c .air.toml; \
		wait

.PHONY: dev-backend
dev-backend: ensure-air ## Run only the Go backend with hot-reload
	env WS_ALLOWED_ORIGINS='localhost:5173' '$(AIR)' -c .air.toml

.PHONY: dev-frontend
dev-frontend: ## Run only the frontend dev server
	cd web && bun run dev

# ── Split development (Docker deps + local API/frontend) ─────────────────────
# MariaDB :3306, Redis :6379, and worker in Docker; API + Vite on the host.
# Open http://localhost:5173 (login: admin / ChangeMe123! on a fresh DB).

.PHONY: dev-split-up
dev-split-up: ## Start Docker MariaDB + Redis (published ports)
	docker compose -f $(COMPOSE_DEPS) up -d mariadb redis
	@$(MAKE) dev-split-wait

.PHONY: dev-split-wait
dev-split-wait: ## Wait until Docker MariaDB and Redis are healthy
	@echo "Waiting for MariaDB and Redis..."
	@timeout=90; \
	while [ $$timeout -gt 0 ]; do \
		mariadb_ok=0; redis_ok=0; \
		docker compose -f $(COMPOSE_DEPS) ps mariadb 2>/dev/null | grep -q "(healthy)" && mariadb_ok=1; \
		docker compose -f $(COMPOSE_DEPS) ps redis 2>/dev/null | grep -q "(healthy)" && redis_ok=1; \
		if [ $$mariadb_ok -eq 1 ] && [ $$redis_ok -eq 1 ]; then \
			echo "MariaDB (:3306) and Redis (:6379) are healthy."; \
			exit 0; \
		fi; \
		sleep 2; \
		timeout=$$((timeout - 2)); \
	done; \
	echo "Timed out waiting for Docker deps. Check: docker compose -f $(COMPOSE_DEPS) ps" >&2; \
	exit 1

.PHONY: dev-split-worker
dev-split-worker: ## Start Docker worker (start local API first on a fresh DB)
	docker compose -f $(COMPOSE_DEPS) up -d --build uptime-phoenix-worker

.PHONY: dev-split-down
dev-split-down: ## Stop Docker deps (keeps MariaDB volume)
	docker compose -f $(COMPOSE_DEPS) down

.PHONY: dev-split-down-v
dev-split-down-v: ## Stop Docker deps and delete MariaDB volume
	docker compose -f $(COMPOSE_DEPS) down -v

.PHONY: dev-split-api
dev-split-api: ensure-air ## Run local API :3000 against Docker MariaDB + Redis (hot-reload)
	env \
		DB_ENGINE=mariadb \
		DB_DSN='$(DEV_SPLIT_DB_DSN)' \
		REDIS_URL='redis://127.0.0.1:6379/0' \
		JWT_SECRET='$(JWT_SECRET)' \
		BOOTSTRAP_USERNAME='$(BOOTSTRAP_USERNAME)' \
		BOOTSTRAP_PASSWORD='$(BOOTSTRAP_PASSWORD)' \
		LOG_LEVEL='$(LOG_LEVEL)' \
		PORT=3000 \
		WEBAUTHN_RP_ORIGINS='http://localhost:5173,http://localhost:3000' \
		WS_ALLOWED_ORIGINS='localhost:5173' \
		'$(AIR)' -c .air.api.toml

.PHONY: dev-split-frontend
dev-split-frontend: dev-frontend ## Run local frontend :5173 (proxies /api + /ws to :3000)

.PHONY: dev-split
dev-split: ensure-air dev-split-up ## Docker deps + local API :3000 + frontend :5173 + worker
	@echo "Starting local API (:3000) and frontend (:5173)..."
	@echo "Login on a fresh DB: $(BOOTSTRAP_USERNAME) / $(BOOTSTRAP_PASSWORD)"
	@test -d web/node_modules || (cd web && bun install --frozen-lockfile)
	@trap 'kill 0' INT TERM; \
		( \
			until curl -sf http://localhost:3000/api/health/ready >/dev/null 2>&1; do sleep 2; done; \
			echo "API ready — starting Docker worker..."; \
			docker compose -f $(COMPOSE_DEPS) up -d --build uptime-phoenix-worker \
		) & \
		(cd web && bun run dev) & \
		env \
			DB_ENGINE=mariadb \
			DB_DSN='$(DEV_SPLIT_DB_DSN)' \
			REDIS_URL='redis://127.0.0.1:6379/0' \
			JWT_SECRET='$(JWT_SECRET)' \
			BOOTSTRAP_USERNAME='$(BOOTSTRAP_USERNAME)' \
			BOOTSTRAP_PASSWORD='$(BOOTSTRAP_PASSWORD)' \
			LOG_LEVEL='$(LOG_LEVEL)' \
			PORT=3000 \
			WEBAUTHN_RP_ORIGINS='http://localhost:5173,http://localhost:3000' \
			WS_ALLOWED_ORIGINS='localhost:5173' \
			'$(AIR)' -c .air.api.toml; \
		wait

# ── Build ─────────────────────────────────────────────────────────────────────

.PHONY: build
build: build-backend build-frontend ## Build Go binary + frontend

.PHONY: build-backend
build-backend: ## Build Go binary (CGO_ENABLED=0, static)
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -o bin/$(APP_NAME) ./cmd/app

.PHONY: build-frontend
build-frontend: ## Build SvelteKit frontend (production)
	cd web && bun install --frozen-lockfile && bun run build

.PHONY: run
run: build ## Build + run locally (SQLite default, zero external deps, embedded UI)
	@echo "Starting Phoenix (MODE=all, SQLite, login admin / ChangeMe123!)..."
	@echo "Open http://localhost:3000"
	@mkdir -p bin
	DB_ENGINE=sqlite DB_DSN='file:phoenix.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)' BOOTSTRAP_USERNAME=admin BOOTSTRAP_PASSWORD=ChangeMe123! MODE=all LOG_LEVEL=info PORT=3000 ./bin/$(APP_NAME)

# ── Test ──────────────────────────────────────────────────────────────────────

.PHONY: test
test: test-backend test-frontend ## Run all tests

.PHONY: test-backend
test-backend: ## Run Go tests with race detector
	go test -race -count=1 ./...

.PHONY: test-frontend
test-frontend: ## Run frontend tests (vitest)
	cd web && bun run test

# ── Gate ──────────────────────────────────────────────────────────────────────
# CI is restored (owner, 2026-07-28): `.github/workflows/ci.yml` runs the gate on
# PR/main. These targets remain the offline/local gate — run them before every
# merge even when CI is green. See docs/TESTING.md and docs/RELEASING.md.

.PHONY: gate
gate: ## Fast dev-loop gate: build, vet, gofmt, race tests, frontend check/test/build
	go build ./...
	go vet ./internal/...
	@out="$$(gofmt -l internal/)"; if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi
	go test -race -count=1 ./internal/...
	@test -d web/node_modules || (cd web && bun install --frozen-lockfile)
	cd web && bun run check && bun run test && bun run build

.PHONY: gate-fast
gate-fast: ## Run the gate without the race detector (faster feedback)
	go build ./...
	go vet ./internal/...
	@out="$$(gofmt -l internal/)"; if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi
	go test -count=1 ./internal/...
	@test -d web/node_modules || (cd web && bun install --frozen-lockfile)
	cd web && bun run check && bun run test && bun run build

.PHONY: gate-full
gate-full: ## The complete local pre-merge gate (CI also runs this surface on PR/main). Run before every merge.
	go build ./...
	go vet ./internal/...
	@out="$$(gofmt -l internal/)"; if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi
	go test -race -count=1 ./...
	@test -x '$(GOLANGCI_LINT)' || go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	$(GOLANGCI_LINT) run
	@test -d web/node_modules || (cd web && bun install --frozen-lockfile)
	cd web && bun run check && bun run test && bun run build && bun run lint && bun run test:e2e
	helm lint charts/uptime-phoenix
	helm template uptime-phoenix charts/uptime-phoenix > /dev/null
	@test -x '$(GOVULNCHECK)' || go install golang.org/x/vuln/cmd/govulncheck@latest
	$(GOVULNCHECK) ./...
	git diff --check
	@echo ""
	@echo "gate-full does NOT include: the MariaDB repository contract (needs TEST_MARIADB_DSN;"
	@echo "CI runs that in the mariadb-contract job), the fresh-DB smoke suites in scripts/,"
	@echo "or the k6 load ramp. See docs/TESTING.md. Local gate-full remains required for"
	@echo "thoroughness and works offline even when GitHub Actions is unavailable."

# ── Lint ──────────────────────────────────────────────────────────────────────

.PHONY: lint
lint: lint-backend lint-frontend ## Run all linters

.PHONY: lint-backend
lint-backend: ## Run golangci-lint
	@test -x '$(GOLANGCI_LINT)' || go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	$(GOLANGCI_LINT) run

.PHONY: lint-frontend
lint-frontend: ## Run eslint + prettier check
	cd web && bun run lint

.PHONY: govulncheck
govulncheck: ## Run govulncheck (installs it if missing)
	@test -x '$(GOVULNCHECK)' || go install golang.org/x/vuln/cmd/govulncheck@latest
	$(GOVULNCHECK) ./...

.PHONY: fmt
fmt: fmt-backend fmt-frontend ## Format all code

.PHONY: fmt-backend
fmt-backend: ## Format Go code
	gofmt -s -w .
	goimports -w .

.PHONY: fmt-frontend
fmt-frontend: ## Format frontend code
	cd web && bun run format

# ── Docker ────────────────────────────────────────────────────────────────────

.PHONY: build-docker
build-docker: ## Build Docker image
	docker build -t $(DOCKER_TAG) .

.PHONY: build-docker-multiarch
build-docker-multiarch: ## Build multi-arch Docker image
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		-t $(DOCKER_TAG) .

.PHONY: docker-run
docker-run: ## Run Docker container locally
	docker run --rm -p 3000:3000 $(DOCKER_TAG)

# ── Helm ──────────────────────────────────────────────────────────────────────

.PHONY: helm-lint
helm-lint: ## Lint Helm chart
	helm lint charts/uptime-phoenix

.PHONY: helm-template
helm-template: ## Render Helm templates (dry-run)
	helm template uptime-phoenix charts/uptime-phoenix

.PHONY: helm-template-debug
helm-template-debug: ## Render Helm templates with debug output
	helm template uptime-phoenix charts/uptime-phoenix --debug

.PHONY: helm-install
helm-install: ## Install Helm chart (default: single-pod mode)
	helm install uptime-phoenix ./charts/uptime-phoenix

.PHONY: helm-install-multi
helm-install-multi: ## Install Helm chart in multi-pod mode (Phase 2)
	helm install uptime-phoenix ./charts/uptime-phoenix \
		--set scaling.mode=multi \
		--set redis.enabled=true

.PHONY: helm-upgrade
helm-upgrade: ## Upgrade Helm chart release
	helm upgrade uptime-phoenix ./charts/uptime-phoenix

.PHONY: helm-uninstall
helm-uninstall: ## Uninstall Helm chart release
	helm uninstall uptime-phoenix

.PHONY: helm-test
helm-test: ## Run Helm tests
	helm test uptime-phoenix

# ── Database ──────────────────────────────────────────────────────────────────

.PHONY: migrate-up
migrate-up: ## Run database migrations up
	go run ./cmd/migrate up

.PHONY: migrate-down
migrate-down: ## Rollback last database migration
	go run ./cmd/migrate down

.PHONY: migrate-status
migrate-status: ## Show migration status
	go run ./cmd/migrate status

# ── Cleanup ───────────────────────────────────────────────────────────────────

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin/
	rm -rf web/.svelte-kit
	rm -rf web/.output
	rm -rf web/build
	rm -rf web/dist
	rm -rf tmp/

# ── Helpers ───────────────────────────────────────────────────────────────────

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-25s\033[0m %s\n", $$1, $$2}'

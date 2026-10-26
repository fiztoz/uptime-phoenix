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
export GOTOOLCHAIN := go1.27.3
export PATH := $(GO_BIN):$(PATH)

# Defaults — override via .env or: make dev-split MARIADB_PASSWORD=secret
MARIADB_PASSWORD ?= change_me_app
MARIADB_ROOT_PASSWORD ?= change_me_root
# JWT_SECRET has no published default: dev targets read a per-checkout random
# key from .secrets/dev_jwt_secret (gitignored), generated on first use.
JWT_SECRET ?= $(shell mkdir -p .secrets && test -s .secrets/dev_jwt_secret || openssl rand -base64 48 > .secrets/dev_jwt_secret; cat .secrets/dev_jwt_secret)
# DEPS_COMPOSE runs docker compose on the deps file with that key in the
# environment: its worker service requires JWT_SECRET, and compose
# interpolates the whole file even for `up mariadb redis`, `ps` or `down`.
# Recipe lines using it (or JWT_SECRET) start with @ so the key is not echoed.
DEPS_COMPOSE = JWT_SECRET='$(JWT_SECRET)' docker compose -f $(COMPOSE_DEPS)
BOOTSTRAP_USERNAME ?= admin
BOOTSTRAP_PASSWORD ?= ChangeMe123!
LOG_LEVEL ?= debug

# Extra flags appended to `make helm-install-mariadb`, e.g.
#   make helm-install-mariadb HELM_FLAGS=--wait
HELM_FLAGS ?=

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
		env WS_ALLOWED_ORIGINS='localhost:5173' JWT_SECRET='$(JWT_SECRET)' '$(AIR)' -c .air.toml; \
		wait

.PHONY: dev-backend
dev-backend: ensure-air ## Run only the Go backend with hot-reload
	@echo "Starting Phoenix backend with hot-reload (:3000, dev JWT_SECRET from .secrets/)..."
	@env WS_ALLOWED_ORIGINS='localhost:5173' JWT_SECRET='$(JWT_SECRET)' '$(AIR)' -c .air.toml

.PHONY: dev-frontend
dev-frontend: ## Run only the frontend dev server
	cd web && bun run dev

# ── Split development (Docker deps + local API/frontend) ─────────────────────
# MariaDB :3306, Redis :6379, and worker in Docker; API + Vite on the host.
# Open http://localhost:5173 (login: admin / ChangeMe123! on a fresh DB).

.PHONY: dev-split-up
dev-split-up: ## Start Docker MariaDB + Redis (published ports)
	@echo "docker compose -f $(COMPOSE_DEPS) up -d mariadb redis"
	@$(DEPS_COMPOSE) up -d mariadb redis
	@$(MAKE) dev-split-wait

.PHONY: dev-split-wait
dev-split-wait: ## Wait until Docker MariaDB and Redis are healthy
	@echo "Waiting for MariaDB and Redis..."
	@timeout=90; \
	while [ $$timeout -gt 0 ]; do \
		mariadb_ok=0; redis_ok=0; \
		$(DEPS_COMPOSE) ps mariadb 2>/dev/null | grep -q "(healthy)" && mariadb_ok=1; \
		$(DEPS_COMPOSE) ps redis 2>/dev/null | grep -q "(healthy)" && redis_ok=1; \
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
	@echo "docker compose -f $(COMPOSE_DEPS) up -d --build uptime-phoenix-worker"
	@$(DEPS_COMPOSE) up -d --build uptime-phoenix-worker

.PHONY: dev-split-down
dev-split-down: ## Stop Docker deps (keeps MariaDB volume)
	@echo "docker compose -f $(COMPOSE_DEPS) down"
	@$(DEPS_COMPOSE) down

.PHONY: dev-split-down-v
dev-split-down-v: ## Stop Docker deps and delete MariaDB volume
	@echo "docker compose -f $(COMPOSE_DEPS) down -v"
	@$(DEPS_COMPOSE) down -v

.PHONY: dev-split-api
dev-split-api: ensure-air ## Run local API :3000 against Docker MariaDB + Redis (hot-reload)
	@echo "Starting local API (:3000) against Docker MariaDB + Redis (dev JWT_SECRET from .secrets/)..."
	@env \
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
			$(DEPS_COMPOSE) up -d --build uptime-phoenix-worker \
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

.PHONY: build-probe-key
build-probe-key: ## Build the explicit probe snapshot key provisioning tool
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -o bin/phoenix-probe-key ./cmd/phoenix-probe-key

.PHONY: build-frontend
build-frontend: ## Build SvelteKit frontend (production)
	cd web && bun install --frozen-lockfile && bun run build

.PHONY: run
run: build ## Build + run locally (SQLite default, zero external deps, embedded UI)
	@echo "Starting Phoenix (MODE=all, SQLite, login admin / ChangeMe123!)..."
	@echo "Open http://localhost:3000"
	@mkdir -p bin
	@echo "(dev JWT_SECRET from .secrets/dev_jwt_secret; set JWT_SECRET to override)"
	@JWT_SECRET='$(JWT_SECRET)' DB_ENGINE=sqlite DB_DSN='file:phoenix.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)' BOOTSTRAP_USERNAME=admin BOOTSTRAP_PASSWORD=ChangeMe123! MODE=all LOG_LEVEL=info PORT=3000 ./bin/$(APP_NAME)

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
	go test -race -count=1 -timeout=2400s ./...
	@test -x '$(GOLANGCI_LINT)' || go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	$(GOLANGCI_LINT) run
	@test -d web/node_modules || (cd web && bun install --frozen-lockfile)
	cd web && bun run check && bun run test && bun run build && bun run lint && bun run test:e2e
	helm lint charts/uptime-phoenix
	helm template uptime-phoenix charts/uptime-phoenix > /dev/null
	helm template uptime-phoenix charts/uptime-phoenix --set mode=split --set database.engine=mariadb --set mariadb.enabled=true --set valkey.enabled=true > /dev/null
	helm template uptime-phoenix charts/uptime-phoenix -f charts/uptime-phoenix/values-production-split.yaml --set mariadbExternal.password=ci > /dev/null
	helm template uptime-phoenix charts/uptime-phoenix --set redis.enabled=true --set redis.existingSecret=phoenix-redis > /dev/null
	@$(MAKE) --no-print-directory helm-validate
	@$(MAKE) --no-print-directory action-pins
	@$(MAKE) --no-print-directory m6-backend-harness-gate
	@$(MAKE) --no-print-directory release-image-gate
	@test -x '$(GOVULNCHECK)' || go install golang.org/x/vuln/cmd/govulncheck@latest
	$(GOVULNCHECK) ./...
	git diff --check
	@echo ""
	@echo "gate-full does NOT include: the MariaDB repository contract (needs TEST_MARIADB_DSN;"
	@echo "CI runs that in the mariadb-contract job), external UAT/load campaigns,"
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

.PHONY: action-pins
action-pins: ## Fail when a workflow uses an action by tag instead of commit SHA
	python3 -B scripts/check-action-pins.py

.PHONY: secrets-scan
secrets-scan: ## Scan git history for secrets with gitleaks (allowlist in .gitleaks.toml)
	@command -v gitleaks >/dev/null || { echo "gitleaks not found: https://github.com/gitleaks/gitleaks#installing"; exit 1; }
	gitleaks git --no-banner -c .gitleaks.toml .

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

.PHONY: m6-backend-harness-gate
m6-backend-harness-gate: ## Verify actual-engine coverage and exhaustive CI shards without database access
	python3 -B -m unittest discover -s scripts -p test_m6_dual_engine_gate.py -v
	python3 -B -m unittest discover -s scripts -p test_repository_test_shards.py -v

.PHONY: release-image-gate
release-image-gate: ## Verify image/chart operations and local image correspondence controls offline
	python3 -B -m unittest discover -s scripts -p test_m8_image_correspondence_gate.py -v
	bash -n scripts/release/dry-run.sh scripts/release/test-image-failures.sh
	bash scripts/release/test-image-failures.sh
	python3 scripts/release/test-chart-publish.py
	python3 scripts/release/test-binary-sboms.py
	python3 scripts/release/test-ci-gate.py

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

# Render-matrix assertions for the in-release MariaDB topology. The plain
# `helm template` calls in gate-full only prove the chart renders; this target
# proves the chart renders WHAT IT CLAIMS. mariadb.enabled used to emit a PVC and
# a DSN pointing at <release>-mariadb without ever creating that server, and
# every smoke test passed while Phoenix crash-looped (AGENTS.md rule 7).
.PHONY: helm-validate
helm-validate: ## Assert the MariaDB topology renders its workload and the guards fail loudly
	helm template uptime-phoenix charts/uptime-phoenix -f charts/uptime-phoenix/values-internal-mariadb.yaml > /dev/null
	helm template uptime-phoenix charts/uptime-phoenix --set database.engine=mariadb --set mariadb.enabled=true --set mode=worker > /dev/null
	@echo "==> extension-only image changes must not roll Phoenix or MariaDB"
	@./scripts/helm-checksum-scope.sh
	@bash scripts/issue67-ext-token-checksum.sh
	@echo "==> remote-probe flag renders only with an explicit secret reference"
	@./scripts/helm-probes-check.sh
	@echo "==> in-release MariaDB must render StatefulSet + PVC + NetworkPolicy"
	@out="$$(helm template uptime-phoenix charts/uptime-phoenix \
		--set database.engine=mariadb --set mariadb.enabled=true --set networkPolicy.enabled=true)"; \
	for want in "kind: StatefulSet" "kind: PersistentVolumeClaim" "kind: NetworkPolicy"; do \
		printf '%s\n' "$$out" | grep -q "^$$want$$" || { echo "MISSING: $$want"; exit 1; }; \
	done; \
	printf '%s\n' "$$out" | python3 -B scripts/helm-db-secret-check.py || exit 1; \
	printf '%s\n' "$$out" | grep -q '^  name: uptime-phoenix-mariadb$$' \
		|| { echo "MariaDB workload/Service is not named <release>-mariadb"; exit 1; }
	@echo "==> the Phoenix NetworkPolicy must keep cloud metadata out of HTTP egress (#88)"
	@helm template uptime-phoenix charts/uptime-phoenix --set networkPolicy.enabled=true \
		--show-only templates/networkpolicy.yaml | grep -q -- '- 169.254.0.0/16$$' \
		|| { echo "MISSING: 169.254.0.0/16 egress exception"; exit 1; }
	@echo "==> the Phoenix NetworkPolicy must keep in-cluster HTTP(S) monitor targets reachable (#88)"
	@helm template uptime-phoenix charts/uptime-phoenix --set networkPolicy.enabled=true \
		--show-only templates/networkpolicy.yaml | grep -q -- '- namespaceSelector: {}$$' \
		|| { echo "MISSING: namespaceSelector peer on the HTTP(S) egress rule"; exit 1; }
	@echo "==> OUTBOUND_ALLOWED_CIDRS must render from outbound.allowedCIDRs (#88)"
	@helm template uptime-phoenix charts/uptime-phoenix --set 'outbound.allowedCIDRs={10.20.0.0/16,192.168.1.0/24}' \
		--show-only templates/configmap.yaml | grep -q 'outbound-allowed-cidrs: "10.20.0.0/16,192.168.1.0/24"' \
		|| { echo "MISSING: outbound.allowedCIDRs -> OUTBOUND_ALLOWED_CIDRS"; exit 1; }
	@echo "==> the MariaDB Pod must not carry the labels the all-in-one Service selects on"
	@helm template uptime-phoenix charts/uptime-phoenix --set database.engine=mariadb --set mariadb.enabled=true \
		| awk '/^kind: StatefulSet/{ss=1} ss&&/^  template:/{tpl=1} tpl&&/^      labels:/{lab=1;next} lab&&/^      [^ ]/{lab=0} lab&&/app.kubernetes.io\/name: uptime-phoenix$$/{bad=1} END{exit bad?1:0}' \
		|| { echo "MariaDB Pod labels collide with the Phoenix Service selector"; exit 1; }
	@echo "==> CronJob / db-migrate Pods must not carry those labels either"
	@helm template uptime-phoenix charts/uptime-phoenix -f charts/uptime-phoenix/values-internal-mariadb.yaml \
		--set dbMigration.enabled=true --set dbMigration.source.host=src.example --set dbMigration.source.existingSecret=src \
		| awk '/^kind: (CronJob|Job)$$/{k=1;tpl=0;lab=0} k&&/^kind: /&&!/^kind: (CronJob|Job)$$/{k=0} k&&/template:/{tpl=1} tpl&&/^[[:space:]]+labels:/{lab=1;next} lab&&/^[[:space:]]+[^ ]/{lab=0} lab&&/app.kubernetes.io\/name: uptime-phoenix$$/{bad=1} END{exit bad?1:0}' \
		|| { echo "Job/CronJob Pod labels collide with the Phoenix Service selector"; exit 1; }
	@echo "==> helm-test Pods must not join the Phoenix or MariaDB Services"
	@helm template uptime-phoenix charts/uptime-phoenix -f charts/uptime-phoenix/values-internal-mariadb.yaml \
		| awk '/^kind: Pod$$/{p=1;hook=0;lab=0} p&&/^kind: /&&!/^kind: Pod$$/{p=0} p&&/helm.sh\/hook: test/{hook=1} p&&/^  labels:/{lab=1;next} lab&&/^  [^ ]/{lab=0} hook&&lab&&(/app.kubernetes.io\/name: uptime-phoenix$$/||/app.kubernetes.io\/name: uptime-phoenix-mariadb$$/){bad=1} END{exit bad?1:0}' \
		|| { echo "helm-test Pod labels collide with a Service selector"; exit 1; }
	@echo "==> ambiguous database configs must fail the render"
	@if helm template uptime-phoenix charts/uptime-phoenix --set mariadb.enabled=true >/dev/null 2>&1; then \
		echo "EXPECTED FAILURE: mariadb.enabled=true without database.engine=mariadb"; exit 1; fi
	@if helm template uptime-phoenix charts/uptime-phoenix --set database.engine=mariadb --set mariadb.enabled=true \
		--set mariadbExternal.host=db.example.com >/dev/null 2>&1; then \
		echo "EXPECTED FAILURE: mariadb.enabled=true together with mariadbExternal.host"; exit 1; fi
	@if helm template uptime-phoenix charts/uptime-phoenix --set database.engine=mariadb --set mariadb.enabled=true \
		--set mariadb.persistence.enabled=false >/dev/null 2>&1; then \
		echo "EXPECTED FAILURE: mariadb.persistence.enabled=false without mariadb.allowEphemeralData"; exit 1; fi
	@echo "==> every workload runs non-root with RuntimeDefault seccomp and no added capabilities (#91)"
	@helm template uptime-phoenix charts/uptime-phoenix | python3 -B scripts/helm-security-context-check.py
	@helm template uptime-phoenix charts/uptime-phoenix --set mode=split --set web.split=true \
		--set database.engine=mariadb --set mariadb.enabled=true --set valkey.enabled=true \
		--set networkPolicy.enabled=true --set cloudflareTunnel.enabled=true --set cloudflareTunnel.token=ci \
		| python3 -B scripts/helm-security-context-check.py
	@helm template uptime-phoenix charts/uptime-phoenix -f charts/uptime-phoenix/values-internal-mariadb.yaml \
		--set cronJobs.backup.enabled=true --set cronJobs.partition.enabled=true --set cronJobs.rollup.enabled=true \
		--set dbMigration.enabled=true --set dbMigration.source.host=src.example --set dbMigration.source.existingSecret=src \
		| python3 -B scripts/helm-security-context-check.py
	@helm template uptime-phoenix charts/uptime-phoenix -f charts/uptime-phoenix/values-production-split.yaml \
		--set mariadbExternal.password=ci | python3 -B scripts/helm-security-context-check.py
	@echo "==> every Phoenix server Deployment (all, api, worker) reads the same JWT_SECRET"
	@helm template uptime-phoenix charts/uptime-phoenix | python3 -B scripts/helm-jwt-secret-check.py --expect 1
	@helm template uptime-phoenix charts/uptime-phoenix --set mode=split --set database.engine=mariadb \
		--set mariadb.enabled=true --set valkey.enabled=true | python3 -B scripts/helm-jwt-secret-check.py --expect 2
	@helm template uptime-phoenix charts/uptime-phoenix -f charts/uptime-phoenix/values-production-split.yaml \
		--set mariadbExternal.password=ci --set secret.existingSecret=phoenix-jwt --set secret.existingSecretKey=signing-key \
		| python3 -B scripts/helm-jwt-secret-check.py --expect 2
	@echo "==> NetworkPolicy opens in-release ports (3000/8080) only toward this release's pods"
	@helm template uptime-phoenix charts/uptime-phoenix --set mode=split --set web.split=true \
		--set networkPolicy.enabled=true --set cloudflareTunnel.enabled=true --set cloudflareTunnel.token=ci \
		| python3 -B scripts/helm-networkpolicy-check.py
	@helm template uptime-phoenix charts/uptime-phoenix --set networkPolicy.enabled=true \
		| python3 -B scripts/helm-networkpolicy-check.py
	@echo "==> the read-only web pod keeps nginx's writable paths (stock and unprivileged images)"
	@for p in /tmp /run /var/cache/nginx; do \
		helm template uptime-phoenix charts/uptime-phoenix --set web.split=true --show-only templates/deployment-web.yaml \
			| grep -q "mountPath: $$p$$" || { echo "MISSING web mount: $$p"; exit 1; }; done
	@echo "==> a supplied secret.jwt shorter than 32 bytes must fail the render"
	@if helm template uptime-phoenix charts/uptime-phoenix --set secret.jwt=change_me_jwt >/dev/null 2>&1; then \
		echo "EXPECTED FAILURE: secret.jwt shorter than 32 bytes"; exit 1; fi
	@echo "==> default chart runs in production mode with no default admin password (#92)"
	@out="$$(helm template uptime-phoenix charts/uptime-phoenix)"; \
	printf '%s\n' "$$out" | grep -q '^  production: "true"$$' \
		|| { echo "default chart does not set production: \"true\""; exit 1; }; \
	if printf '%s\n' "$$out" | grep -q 'BOOTSTRAP_PASSWORD'; then \
		echo "default chart seeds a bootstrap admin password"; exit 1; fi
	@if helm template uptime-phoenix charts/uptime-phoenix --set bootstrap.enabled=true >/dev/null 2>&1; then \
		echo "EXPECTED FAILURE: bootstrap.enabled without a password or existingSecret"; exit 1; fi
	@echo "==> helm-validate OK"

.PHONY: helm-template-mariadb
helm-template-mariadb: ## Render single-pod mode with the in-release, PVC-backed MariaDB
	helm template uptime-phoenix charts/uptime-phoenix --set database.engine=mariadb --set mariadb.enabled=true

.PHONY: helm-install-mariadb
helm-install-mariadb: ## Install single-pod Phoenix on the in-release, persistent MariaDB
	helm upgrade --install uptime-phoenix ./charts/uptime-phoenix \
		--namespace uptime-phoenix --create-namespace \
		-f charts/uptime-phoenix/values-internal-mariadb.yaml \
		$(HELM_FLAGS)

.PHONY: helm-template
helm-template: ## Render Helm templates (dry-run)
	helm template uptime-phoenix charts/uptime-phoenix

.PHONY: helm-template-valkey
helm-template-valkey: ## Render split mode with the in-release Valkey subchart
	helm template uptime-phoenix charts/uptime-phoenix --set mode=split --set database.engine=mariadb --set mariadb.enabled=true --set valkey.enabled=true

.PHONY: helm-template-production-split
helm-template-production-split: ## Render the production split+shard+Valkey overlay
	helm template uptime-phoenix charts/uptime-phoenix -f charts/uptime-phoenix/values-production-split.yaml --set mariadbExternal.password=ci

.PHONY: helm-template-debug
helm-template-debug: ## Render Helm templates with debug output
	helm template uptime-phoenix charts/uptime-phoenix --debug

.PHONY: helm-install
helm-install: ## Install Helm chart (default: single-pod mode)
	helm install uptime-phoenix ./charts/uptime-phoenix

.PHONY: helm-install-multi
helm-install-multi: ## Install Helm chart in multi-pod mode with in-release Valkey
	helm install uptime-phoenix ./charts/uptime-phoenix \
		--set mode=split \
		--set database.engine=mariadb \
		--set mariadb.enabled=true \
		--set valkey.enabled=true

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

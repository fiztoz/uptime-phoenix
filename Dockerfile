# ─── Stage 1: Frontend assets ────────────────────────────────────────────────
# USE_PREBUILT_WEB=1 (default for release dry-run): copy host-built web/dist.
# USE_PREBUILT_WEB=0: install deps with bun, build with real Node (vite).
# Avoids `bun run` under QEMU/Colima linux/amd64 (SIGILL / no AVX).
ARG USE_PREBUILT_WEB=0

FROM node:22-alpine AS web-from-source
WORKDIR /app/web
RUN npm install -g bun@1.3.14
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
# Vite may print "done" then crash under QEMU (libuv); accept if dist exists.
RUN npm run build; test -f dist/index.html

FROM alpine:3.20 AS web-from-context
COPY web/dist /app/web/dist
RUN test -f /app/web/dist/index.html

FROM web-from-source AS web-builder-0
FROM web-from-context AS web-builder-1
FROM web-builder-${USE_PREBUILT_WEB} AS web-builder

# ─── Stage 2: Build Go binary ────────────────────────────────────────────────
# Wait for web-builder before go mod download so BuildKit does not run both
# heavy stages in parallel (peak RAM spike → BuildKit EOF on Colima/low-memory hosts).
FROM golang:1.25-alpine AS go-builder

# Multi-arch: buildx sets TARGETOS/TARGETARCH; never hardcode amd64.
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev

RUN apk add --no-cache git
WORKDIR /app
COPY --from=web-builder /app/web/dist ./web/dist
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOMAXPROCS=2 \
    go build -trimpath \
      -ldflags="-s -w -X github.com/fiztoz/uptime-phoenix/internal/version.Version=${VERSION}" \
      -o /phoenix ./cmd/app && \
    go build -trimpath \
      -ldflags="-s -w -X github.com/fiztoz/uptime-phoenix/internal/version.Version=${VERSION}" \
      -o /phoenix-api ./cmd/api && \
    go build -trimpath \
      -ldflags="-s -w -X github.com/fiztoz/uptime-phoenix/internal/version.Version=${VERSION}" \
      -o /phoenix-worker ./cmd/worker

# ─── Stage 3: Final image (distroless, ~25 MB) ──────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64
LABEL org.opencontainers.image.title="Phoenix" \
      org.opencontainers.image.description="Self-hosted K8s-native monitoring" \
      org.opencontainers.image.source="https://github.com/Fiztoz/phoenix" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.vendor="Phoenix"
COPY --from=go-builder /phoenix /phoenix
EXPOSE 3000
USER 65532
# Phase 3: MODE=api|worker|all (default all = single-pod). Set via Helm/env.
ENV MODE=all
ENTRYPOINT ["/phoenix"]

# Single-container image (T1/T3): one static-ish Go binary that serves /api/v1
# AND the embedded React SPA. CGO is required by mattn/go-sqlite3 (T1), so the
# builder uses gcc and the final stage ships a glibc base (distroless).
#
# IMPORTANT — build context is the REPOSITORY ROOT, not backend/, because the
# frontend (../frontend) is built in-image and embedded (B.1). Build with:
#
#   docker build -f backend/Dockerfile \
#     --build-arg VERSION=$(git describe --tags --always --dirty) \
#     --build-arg COMMIT=$(git rev-parse --short HEAD) \
#     --build-arg BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) .
#
# .github/workflows/publish-images.yml builds exactly this on a vX.Y.Z tag and
# pushes ghcr.io/resetsmith/cronomicon; your own pipeline sets context: . and
# passes the same args.

# ── Stage 1: build the frontend so the embed is fresh, not a stale committed dist.
# Pin by digest in a hardened pipeline; tag-pinned here for readability.
FROM node:24-bookworm AS frontend
WORKDIR /repo/frontend
# Install deps against the lockfile first for layer caching.
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
# Build. vite's outDir is ../backend/web/dist (frontend/vite.config.ts), so the
# bundle lands at /repo/backend/web/dist. Pre-create the (out-of-root) parent and
# copy documentation/ so the publishManuals plugin can read the manuals + runner
# guides it bundles at build time (frontend/vite-manuals-plugin.js reads
# ../documentation/*.html — user/administrator manuals + runner-*.html guides).
# The plugin's FILES class also publishes backend/deploy/runner-install.sh to
# /runner-install.sh (the Runners view's Download button + install one-liner) and
# backend/deploy/cronomicon-runner.env.example to /cronomicon-runner.env.example,
# so those files must exist in this stage too — the plugin fails the build if a
# FILES source is missing.
COPY frontend/ ./
COPY documentation/ /repo/documentation/
COPY backend/deploy/runner-install.sh /repo/backend/deploy/runner-install.sh
COPY backend/deploy/cronomicon-runner.env.example /repo/backend/deploy/cronomicon-runner.env.example
# ── Bundle the frontend. NOTE: `vite build`, not `npm run build` (v0.52.27).
#
# The npm script is `tsc -b && vite build`. Type-checking is a SOURCE gate — it
# runs locally and in CI, where a type error should fail — and putting it in the
# image build made the deploy fragile in a way that bit us:
#
# `tsc -b` checks every file under src/ whether anything imports it or not. The
# deploy host's clone still contained frontend/src/views/envvars/ScopesTab.tsx,
# deleted from git in b384ad2 (v0.41.6) when Scopes moved to its own page. It
# type-checked for eleven minor versions because it passes a scope id into
# /scopes/{scopeId}, which the spec wrongly declared `integer`. K-3 corrected
# that to `string` (scope ids are UUIDv7) — and an orphan file nothing imports
# broke the deploy with seven errors, while every clean checkout built fine.
#
# `vite build` bundles only what is reachable from the entry, so an unimported
# stray file cannot fail the build and cannot enter the bundle (verified both
# ways). Dropping the type-check here also takes ~700 MB of peak RSS and ~14s off
# the image build.
#
# This does NOT make a dirty deploy clone safe: a stale file that IS imported
# would still be bundled, shipping old code silently. Keep the checkout clean.
#
# The v0.52.26 diagnostic scaffolding that found the orphan file above — a
# fallback to the committed dist plus the build log served at /build-error.txt —
# was removed in v0.52.28 once a deploy came up clean. It lives in git at a8c594e
# if a future deploy ever fails somewhere this opaque again; `git show a8c594e`
# has both halves, and reverting it is faster than rebuilding the idea.
RUN mkdir -p /repo/backend/web && npx vite build

# ── Stage 2: cross-compile the runner-agent binaries the server distributes
# at GET /agents/{filename} (runner provisioning D1). CGO off → static binaries
# (the same recipe a standalone runner image uses); D5: linux amd64 + arm64 only.
# SHA256SUMS is generated alongside so runner-install.sh --download can verify.
FROM golang:1.26-bookworm AS agents
WORKDIR /src
COPY backend/go.mod backend/go.sum* ./
RUN go mod download
COPY backend/ ./
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/agents/cronomicon-runner-linux-amd64 ./cmd/cronomicon-runner && \
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/agents/cronomicon-runner-linux-arm64 ./cmd/cronomicon-runner && \
    cd /out/agents && sha256sum cronomicon-runner-linux-amd64 cronomicon-runner-linux-arm64 > SHA256SUMS

# ── Stage 3: compile the Go binary with the freshly built UI overlaid in.
FROM golang:1.26-bookworm AS build
WORKDIR /src
# Cache modules first.
COPY backend/go.mod backend/go.sum* ./
RUN go mod download
# Source, then replace any committed web/dist with the fresh build so
# //go:embed all:dist picks up a UI matching source HEAD (B.1 exit criterion).
COPY backend/ ./

RUN rm -rf web/dist
COPY --from=frontend /repo/backend/web/dist ./web/dist

# Build metadata, surfaced at /version and /healthz.
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=1 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
      -o /out/cronomicon ./cmd/cronomicon

# Pre-create data directory with nonroot UID/GID (65532)
RUN mkdir -p /out/var/lib/cronomicon && chown 65532:65532 /out/var/lib/cronomicon

# ── Stage 4: minimal runtime. Distroless has glibc (cgo sqlite) and NO shell.
FROM gcr.io/distroless/base-debian12
WORKDIR /app
COPY --from=build /out/cronomicon /app/cronomicon
COPY --from=build --chown=nonroot:nonroot /out/var/lib/cronomicon /var/lib/cronomicon
# Runner-agent binaries + SHA256SUMS, served at GET /agents/{filename}
# (CRONOMICON_AGENT_DIR defaults here; see internal/api/agents.go).
COPY --from=agents /out/agents /usr/share/cronomicon/agents
# Data volume: SQLite file + WAL + per-run logs + GitLab clone cache + local
# backups (S13). One writer — single replica (SQLite single-node).
VOLUME ["/var/lib/cronomicon"]
ENV CRONOMICON_DB_PATH=/var/lib/cronomicon/cronomicon.db

EXPOSE 8080

# The image has no shell/curl, so the binary probes itself over HTTP (B.5).
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/app/cronomicon", "healthcheck", "-ready"]

USER nonroot:nonroot
ENTRYPOINT ["/app/cronomicon"]

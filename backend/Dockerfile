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
# (docker-compose.yml at the repo root sets context: . and passes these args.)

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
# backend/deploy/amadeus-runner.env.example to /amadeus-runner.env.example,
# so those files must exist in this stage too — the plugin fails the build if a
# FILES source is missing.
COPY frontend/ ./
COPY documentation/ /repo/documentation/
COPY backend/deploy/runner-install.sh /repo/backend/deploy/runner-install.sh
COPY backend/deploy/amadeus-runner.env.example /repo/backend/deploy/amadeus-runner.env.example
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
# (same recipe as deploy/Dockerfile.runner); D5: linux amd64 + arm64 only.
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
      -o /out/agents/amadeus-runner-linux-amd64 ./cmd/amadeus-runner && \
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/agents/amadeus-runner-linux-arm64 ./cmd/amadeus-runner && \
    cd /out/agents && sha256sum amadeus-runner-linux-amd64 amadeus-runner-linux-arm64 > SHA256SUMS

# ── Stage 3: compile the Go binary with the freshly built UI overlaid in.
FROM golang:1.26-bookworm AS build
WORKDIR /src
# Cache modules first.
COPY backend/go.mod backend/go.sum* ./
RUN go mod download
# Source, then replace any committed web/dist with the fresh build so
# //go:embed all:dist picks up a UI matching source HEAD (B.1 exit criterion).
COPY backend/ ./

# STALE-CHECKOUT REPAIR (2026-09-18). The deploy host does not build a clean
# checkout: it keeps a working copy across deploys and RETAINS files that have
# been deleted from git. Confirmed here by exit 82 — internal/gitlab/migrate.go,
# deleted in v1.5.41, was still in the context — and previously in v0.52.26
# (a8c594e), where a file deleted eleven minor versions earlier broke the
# frontend build the same way.
#
# A retained .go file is not inert: migrate.go re-declares readBodyAt and
# hashBodyAt, which body.go now owns, so the package stops compiling. Deleting
# the known-dead paths here is a no-op on a clean checkout and repairs a dirty
# one, so the image is built from the tree this commit actually describes.
#
# This is a WORKAROUND, not a fix. It cannot repair a file that is merely OUT OF
# DATE (only ones that should not exist), and the next deletion anywhere in the
# tree will fail the same way until the deploy host clones clean. The assertions
# in the build step below are what catch the half this cannot repair.
RUN rm -f internal/gitlab/migrate.go internal/gitlab/migrate_test.go \
          internal/sshkeys/backfill.go internal/sshkeys/backfill_test.go \
          SEAMS.md

RUN rm -rf web/dist
COPY --from=frontend /repo/backend/web/dist ./web/dist

# Build metadata, surfaced at /version and /healthz.
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
# TEMPORARY DIAGNOSTIC SCAFFOLDING (2026-09-18), round 4. Not a feature.
#
# Round 3 answered 82: the deploy host retained internal/gitlab/migrate.go,
# deleted in v1.5.41. The repair step after the COPY above now removes the
# known-dead paths; these assertions stay to catch the staleness it cannot fix.
#
# WHAT THE FIRST TWO ROUNDS ESTABLISHED. Round 1 answered 44: not disk, not
# memory, not an empty web/dist. Round 2 answered 55 — "undefined:" — on a
# builder confirmed amd64 and Go 1.26. That combination is diagnostic: the same
# commit compiles from a clean `git archive` export with a cold GOCACHE, so a
# symbol can only be undefined here if THE CHECKOUT THIS IMAGE IS BUILT FROM IS
# NOT THAT COMMIT. Both partial-checkout shapes were reproduced deliberately and
# both give exactly this error:
#
#   • new internal/notify/notify.go beside a pre-band internal/settings/global.go
#     → undefined: settings.ParseAppriseTargets
#   • new internal/gitlab/sync.go with internal/gitlab/body.go absent
#     → undefined: readBodyAt / hashBodyAt
#
# This is the same failure class as v0.52.26 (a8c594e), where the deploy host's
# clone still held a file deleted from git eleven minor versions earlier. The
# repo is not the cause: there is no .gitattributes, no sparse-checkout, and no
# backend source file is gitignored.
#
# So this round stops guessing from error text and ASSERTS THE CHECKOUT, naming
# the stale path in the exit code before the compiler is ever invoked.
#
#   81  internal/gitlab/body.go MISSING          (a new file never landed)
#   82  internal/gitlab/migrate.go PRESENT       (the repair rm above failed)
#   83  internal/sshkeys/backfill.go PRESENT     (the repair rm above failed)
#   84  internal/settings/global.go is PRE-BAND  (no func ParseAppriseTargets)
#   85  migrations/1150_*.up.sql MISSING         (a new file never landed)
#   86  internal/runner/poll.go is PRE-BAND      (no protocol_too_old)
#   87  internal/notify/notify.go is PRE-BAND    (still parses bare-string targets)
#
#   90  GOARCH is not amd64      1xx  Go toolchain is go1.<xx>, not 1.26
#
#   71  undefined: settings.ParseAppriseTargets  72  undefined: readBodyAt/hashBodyAt
#   74  some other undefined symbol              73  a redeclared symbol
#   50-70 as round 2 (module/type/cgo/etc.)      44  nothing matched
#
# ANY REMAINING 8x CODE MEANS A FILE IS OUT OF DATE RATHER THAN MERELY EXTRA,
# which the repair step above cannot fix — only a clean checkout can. Look for a
# "clear cache" / "force rebuild" / "recreate" option in the deploy tool, or
# delete and re-create the deployment so it clones fresh. Worth doing either
# way: a workspace that silently keeps old source will keep producing failures
# that read like code bugs.
#
# TO REMOVE: delete this block and restore
#   RUN CGO_ENABLED=1 go build -trimpath \
#       -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
#       -o /out/amadeus ./cmd/amadeus
RUN set -u; \
    if [ ! -f internal/gitlab/body.go ]; then exit 81; fi; \
    if [ -f internal/gitlab/migrate.go ]; then exit 82; fi; \
    if [ -f internal/sshkeys/backfill.go ]; then exit 83; fi; \
    if ! grep -q "func ParseAppriseTargets" internal/settings/global.go; then exit 84; fi; \
    if [ ! -f internal/db/migrations/1150_apprise_targets_object_form.up.sql ]; then exit 85; fi; \
    if ! grep -q "protocol_too_old" internal/runner/poll.go; then exit 86; fi; \
    if grep -q "legacy bare-URL string" internal/notify/notify.go; then exit 87; fi; \
    ARCH=$(go env GOARCH 2>/dev/null || echo unknown); \
    if [ "$ARCH" != "amd64" ]; then exit 90; fi; \
    GOV=$(go env GOVERSION 2>/dev/null || echo unknown); \
    MINOR=$(echo "$GOV" | sed -n "s/^go1\.\([0-9][0-9]*\).*/\1/p"); \
    if [ -n "$MINOR" ] && [ "$MINOR" != "26" ]; then exit $((100 + MINOR)); fi; \
    CGO_ENABLED=1 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
      -o /out/amadeus ./cmd/amadeus > /tmp/b.log 2>&1; \
    status=$?; \
    cat /tmp/b.log; \
    if [ "$status" -eq 0 ]; then echo "go build OK"; exit 0; fi; \
    echo "=== GO BUILD FAILED (exit $status) ==="; \
    m() { grep -qiE "$1" /tmp/b.log; }; \
    if m "undefined: settings\.ParseAppriseTargets"; then exit 71; fi; \
    if m "undefined: (readBodyAt|hashBodyAt)"; then exit 72; fi; \
    if m "redeclared in this block"; then exit 73; fi; \
    if m "undefined:"; then exit 74; fi; \
    if m "missing go\.sum entry"; then exit 50; fi; \
    if m "no required module provides"; then exit 51; fi; \
    if m "updates to go\.mod needed"; then exit 52; fi; \
    if m "cannot find module providing"; then exit 53; fi; \
    if m "proxy\.golang\.org|dial tcp|module lookup disabled|i/o timeout|certificate"; then exit 54; fi; \
    if m "imported and not used"; then exit 56; fi; \
    if m "declared and not used"; then exit 57; fi; \
    if m "cannot use .* as .* value|incompatible type"; then exit 58; fi; \
    if m "unknown field"; then exit 59; fi; \
    if m "not enough arguments|too many arguments"; then exit 60; fi; \
    if m "build constraints exclude all Go files"; then exit 61; fi; \
    if m "cgo:|C compiler|gcc.*not found"; then exit 62; fi; \
    if m "is not in std|is not in GOROOT"; then exit 63; fi; \
    if m "no Go files"; then exit 64; fi; \
    if m "go\.mod requires|requires go >=|invalid go version"; then exit 65; fi; \
    if m "internal compiler error"; then exit 66; fi; \
    if m "cannot load"; then exit 67; fi; \
    if m "permission denied"; then exit 68; fi; \
    if m "\.go:[0-9]+"; then exit 70; fi; \
    exit 44

# Pre-create data directory with nonroot UID/GID (65532)
RUN mkdir -p /out/var/lib/amadeus && chown 65532:65532 /out/var/lib/amadeus

# ── Stage 4: minimal runtime. Distroless has glibc (cgo sqlite) and NO shell.
FROM gcr.io/distroless/base-debian12
WORKDIR /app
COPY --from=build /out/amadeus /app/amadeus
COPY --from=build --chown=nonroot:nonroot /out/var/lib/amadeus /var/lib/amadeus
# Runner-agent binaries + SHA256SUMS, served at GET /agents/{filename}
# (AMADEUS_AGENT_DIR defaults here; see internal/api/agents.go).
COPY --from=agents /out/agents /usr/share/amadeus/agents
# Data volume: SQLite file + WAL + per-run logs + GitLab clone cache + local
# backups (S13). One writer — single replica (SQLite single-node).
VOLUME ["/var/lib/amadeus"]
ENV AMADEUS_DB_PATH=/var/lib/amadeus/amadeus.db

EXPOSE 8080

# The image has no shell/curl, so the binary probes itself over HTTP (B.5).
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/app/amadeus", "healthcheck", "-ready"]

USER nonroot:nonroot
ENTRYPOINT ["/app/amadeus"]

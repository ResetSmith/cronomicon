# Cronomicon backend — developer tasks.
# The build is OpenAPI-first (T5): openapi.yaml is the single source of truth.
# Backend spec-drift is guarded by `make verify` (go test): the spec-conformance
# tests (internal/api/spec_conformance_test.go) + the two-copy byte-identity check
# (internal/api/openapi_mirror_test.go), plus the frontend `npm run gen`. The
# oapi-codegen `generate` target was removed (CC.26 / CC-D5): oapi-codegen v2 can't
# parse the OpenAPI 3.1 spec (upstream #373), so it produced no usable output — a
# broken-but-advertised CI gate is worse than none.

BIN          := bin/amadeus

# Build metadata stamped into the binary (mirrors the Dockerfile B.1 ldflags).
# VERSION prefers an exact tag; falls back to the version const in main.go so
# feature branches (which are never tagged) show the correct current version
# rather than the last reachable tag (e.g. v0.26.0 when source says 0.36.5).
VERSION    ?= $(shell git describe --tags --exact-match 2>/dev/null \
               || sed -n 's/^[[:space:]]*version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' \
                    cmd/amadeus/main.go | head -1)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

.PHONY: all build run dev stop test test-fast vet vuln lint tidy verify clean deadcode

# Local preview DB — kept outside /tmp so it survives reboots. The directory is
# created on first use by the run/dev recipes below.
DEV_DB ?= $(HOME)/.local/share/amadeus/amadeus-dev.db

# Optional env file that `make run` sources if present (e.g. the compose stack's
# amadeus.env, or any KEY=value file). The binary itself has no .env autoloading
# — it reads only the process environment — so this is how `make run` picks up
# things like CRONOMICON_TRUSTED_PROXIES. Override with `make run ENV_FILE=path`.
ENV_FILE ?= amadeus.env

all: build

build:
	CGO_ENABLED=1 go build -ldflags="$(LDFLAGS)" -o $(BIN) ./cmd/amadeus

# Stop any amadeus instance left over from a previous make run/dev. Without
# this, a stale process keeps port 8080 and the new instance dies with
# "bind: address already in use" — while the browser silently keeps talking
# to the OLD binary (stale code, NetworkErrors on restart).
stop:
	-@pkill -f "$(BIN)" 2>/dev/null; sleep 0.5; true

# Local run on a writable DB path (the prod default /var/lib/amadeus is the
# container's mounted volume and needs root to create on a dev box). This is a
# developer convenience only — production runs the binary directly with its own
# env. Sources $(ENV_FILE) if present (default amadeus.env) so a trusted-header
# config can boot; heads-up that file targets the compose stack (KEK file path,
# cookie-secure, proxy CIDRs), so prefer `make dev` for a no-proxy local preview.
# Without an env file and without OIDC/dev-auth, login is disabled.
run: build stop
	@mkdir -p $(dir $(DEV_DB))
	@set -a; \
		if [ -f "$(ENV_FILE)" ]; then echo ">> loading env from $(ENV_FILE)"; . "./$(ENV_FILE)"; fi; \
		set +a; \
		CRONOMICON_DB_PATH=$(DEV_DB) ./$(BIN)

# Local preview: dev-login bypass + demo seed, on a writable DB path. Browse the
# UI before SSO/GitLab are wired up. NEVER use these flags in production.
dev: build stop
	@mkdir -p $(dir $(DEV_DB))
	CRONOMICON_DEV_AUTH=true CRONOMICON_DEV_SEED=true CRONOMICON_COOKIE_SECURE=false \
	CRONOMICON_DB_PATH=$(DEV_DB) ./$(BIN)

# -race matches CI (the pipeline runs `go test -race ./...`). It is ~2-3x
# slower on the internal/db and internal/api suites; that is the price of the
# local gate telling the truth. This tree has ~30 non-test goroutine launch
# sites, 20 mutexes, 14 tickers and a bounded-parallelism workflow engine —
# races here surface as mis-dispatched runs, so they must not wait for CI.
# Use `make test-fast` for the inner loop.
test:
	go test -race ./...

# Inner-loop convenience: no -race, so it is NOT the gate. `make verify` is.
test-fast:
	go test ./...

vet:
	go vet ./...

# GM-1 found nine advisories reachable from our code (x/crypto ssh + go-git)
# that had gone unnoticed because nothing ever ran this. Deliberately NOT part
# of `verify`: it fetches the vulnerability DB from vuln.go.dev, and `verify`
# must work on an air-gapped build box. CI runs it as its own job, and it is a
# pre-release step in deploy/go-no-go.md.
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Advisory, not a gate (DD-4): unreachable Go with the test suite counted as
# roots. Read a symbol before deleting it — build tags and reflection produce
# false positives. Needs network the first time (module download).
deadcode:
	go run golang.org/x/tools/cmd/deadcode@latest -test ./...

# GM-4. Reports the FULL backlog (~168 findings at introduction); CI gates only
# what an MR touches (--new-from-rev), so this is a survey tool, not a gate.
# Not in `verify` for the same reason as `vuln`: it fetches the tool.
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1 run --timeout=15m ./...

tidy:
	go mod tidy

# Full local gate, mirrors CI (vet + race-enabled tests + build). Network-free:
# `make vuln` is separate on purpose — see above.
verify: tidy vet test build

clean:
	rm -rf bin

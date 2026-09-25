# Cronomicon backend

Go backend for Cronomicon (single static binary, single process — T1/T3). Serves
the API + embedded frontend from one container, backed by SQLite.

> Two binaries: `cmd/amadeus` (server) and `cmd/amadeus-runner` (the
> out-of-process runner agent). The release of record is the top entry of
> `../CHANGELOG.md`; the package map is in `../AGENTS.md`.

## Layout

```
cmd/amadeus/        entrypoint + `validate` subcommand (T11)
internal/
  config/           env → typed Config
  db/               SQLite open, golang-migrate, retention sweep, UUIDv7 ids
    migrations/     numbered NNN_name.{up,down}.sql (T4)
  auth/             OIDC RP + trusted-header SSO, session cookie + CSRF (T8), runner bearer (T9),
                    role derivation (A3.1), ResolveAllowedScopes (A5, single call-site)
  api/              net/http router, health/readiness (T13), per-feature mount files
  httpx/            error envelope + JSON helpers
web/                embedded frontend build (dist/) — produced by Vite in B7
openapi.yaml        the contract (canonical, single-owner — T5)
```

## Develop

```bash
make build      # CGO_ENABLED=1 (mattn/go-sqlite3 needs cgo)
make test       # go test -race ./...  (the gate)
make test-fast  # no -race — inner loop only, not the gate
make run        # serves :8080
make verify     # tidy + vet + race-enabled test + build (mirrors CI)
make vuln       # govulncheck — needs network, so NOT part of verify
make deadcode   # unreachable-symbol report (tests as roots) — advisory, not a gate
```

There is no codegen step: the `oapi-codegen` target was removed (CC.26 / CC-D5)
because v2 cannot parse this OpenAPI 3.1 spec. The Go types are hand-written and
held to `openapi.yaml` by the conformance tests in `internal/api/` instead.

## Configuration (environment)

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_ADDR` | `:8080` | listen address |
| `CRONOMICON_DB_PATH` | `/var/lib/amadeus/amadeus.db` | SQLite file (mounted volume, S13) |
| `CRONOMICON_LOG_LEVEL` / `_FORMAT` | `info` / `json` | |
| `CRONOMICON_LOG_FILE_ENABLED` / `CRONOMICON_LOG_FILE` / `_MAX_MB` / `_KEEP` | `true` / — / `64` / `5` | Process log written to disk **in addition to stdout** (stdout is never replaced). Empty `CRONOMICON_LOG_FILE` ⇒ `amadeus.log` in the run-log directory, following it live when that setting changes; an explicit path must be **absolute** and attaches earlier in boot (captures the build banner + config warnings). Rotates at `_MAX_MB` keeping `_KEEP` generations (`amadeus.log.1` … `.N`), so disk ≤ `(KEEP+1) × MAX_MB`. **Not** governed by `_RETENTION_LOG_FILES_DAYS`, and **not** redacted — see `deploy/security-review.md`. |
| `CRONOMICON_AUDIT_LOG_ENABLED` / `CRONOMICON_AUDIT_LOG` | `true` / — | Compliance **audit stream** (`audit.log`) — a separate file from the process log: one JSON-Lines record per audited event (`change_log`, a named subset of `activity`, and every auth event), keys fixed by a versioned schema (`"v": 1`). Empty path ⇒ `audit.log` in the run-log directory, following it live; an explicit path must be **absolute**. Rotates **daily by UTC date** to `audit.log.YYYYMMDD`; lifetime is the `auditLogFiles` retention knob (default 730 days), not a keep count. The **database is authoritative** — the file is an export of it, so `false` loses the stream, not the trail. Durable and not redacted by default — see `deploy/security-review.md`. |
| `CRONOMICON_OIDC_ISSUER` / `_CLIENT_ID` / `_CLIENT_SECRET` / `_REDIRECT_URL` | — | OIDC relying party (A3.1/T8). Unset ⇒ login disabled (degraded). |
| `CRONOMICON_SESSION_HASH_KEY` / `_BLOCK_KEY` | — | base64 32 bytes; unset ⇒ ephemeral keys (dev only) |
| `CRONOMICON_COOKIE_SECURE` | `true` | set `false` for local HTTP |
| `CRONOMICON_DEV_AUTH` | `false` | **dev only** — exposes `GET /auth/dev-login`, a one-click bypass that mints a synthetic admin session without the identity provider. Never enable in a deployed environment. |
| `CRONOMICON_DEV_SEED` | `false` | **dev only** — loads representative demo data into an *empty* DB on boot so every view renders. No-op once the DB has data. |
| `CRONOMICON_RUNNER_BOOTSTRAP_TOKEN` | — | shared runner registration token (A6.1) |
| `CRONOMICON_KEK_FILE` / `CRONOMICON_KEK` | — | stored-secret KEK: mounted file preferred, env fallback (S14). **Back up separately from the DB.** The `CRONOMICON_SECRET_KEK*` aliases were removed in v1.5.41 — a deployment still setting them fails its first stored-secret operation with "no KEK configured". |
| `CRONOMICON_RETENTION_RUNS_DAYS` / `_CHANGELOG_DAYS` / `_LOG_FILES_DAYS` | `90` / `365` / `90` | A4 retention **bootstrap defaults only** — they seed the `auditCompliance` settings blob on the first boot that finds it unset; afterwards the eleven per-table knobs in Settings → Audit & Compliance are authoritative (the knobs with no env var — `auditLogFiles`, `recycleBin`, `definitionRevisions`, `runnerPlacementHistory`, `archivedLogFiles` — start at their own defaults). `_LOG_FILES_DAYS` is new and reaps **on-disk run logs**, which nothing deleted before. |
| `CRONOMICON_BACKUP_S3_BUCKET` | — | nightly VACUUM-INTO upload target (A4); empty disables upload |
| `CRONOMICON_BACKUP_S3_ENDPOINT` / `_REGION` / `_ACCESS_KEY` / `_SECRET_KEY` / `_USE_SSL` | — / `us-east-1` / — / — / `true` | S3-compatible target (AWS/MinIO/Ceph/R2). See `deploy/backup-restore.md` |

> **Namespace note.** Names under the reserved prefixes `CRONOMICON_VAR_`,
> `CRONOMICON_SECRET_`, `CRONOMICON_KEY_`, and `CRONOMICON_RUN_` are **not** configuration —
> they are references the app resolves and injects into runs. Everything else
> under `CRONOMICON_*` (the table above) is server/runner config. This is why the KEK
> now reads `CRONOMICON_KEK*` rather than `CRONOMICON_SECRET_KEK*`: it is config, not a
> secret named "KEK" in the store. See `deploy/env-matrix.md` and the full
> contract in `20260720-namespace-update.md`.

## Local preview (before SSO)

To browse the UI with realistic content before OIDC/GitLab/runners are wired up:

```bash
CRONOMICON_DEV_AUTH=true CRONOMICON_DEV_SEED=true CRONOMICON_COOKIE_SECURE=false \
  CRONOMICON_DB_PATH=/tmp/amadeus-dev.db ./bin/amadeus
```

Open the app and click **"Developer login (bypass SSO)"** on the sign-in
screen. Both flags are off by default and the dev-login route is not even
mounted unless `CRONOMICON_DEV_AUTH=true`, so production never exposes it. The seed
is a no-op once the DB has data, so it never clobbers a real (GitLab-synced) DB.

## Health

- `GET /healthz` — liveness (always 200 if the process is up).
- `GET /readyz` — readiness: DB reachable + migrations clean; identity check
  added when OIDC is configured (T12/T13). Both unauthenticated.

## Deployment & storage layout (S13)

Single container, single process (T3). All durable state lives under one mounted
volume at `/var/lib/amadeus` (the `Dockerfile` declares it as a `VOLUME`):

| Path | Contents |
|---|---|
| `/var/lib/amadeus/amadeus.db` (+ `-wal`/`-shm`) | SQLite database (`CRONOMICON_DB_PATH`) |
| `/var/lib/amadeus/logs/<code>/<traceId>.log` | per-run log files (redacted at ingest, T7/S7), grouped into one folder per job/workflow. `<code>` is an opaque 8-hex-character code from migration `710`'s `entity_codes` registry; runs owned by no definition (the SSH connection test) use `_system`. Each folder carries a `_meta.json` sidecar naming the owning entity. Runs enqueued before `710` stay at the flat `/var/lib/amadeus/logs/<traceId>.log` path — nothing is migrated and both layouts coexist. |
| `/var/lib/amadeus/logs/amadeus.log` (+ `amadeus.log.1` … `.N`) | **process log** — the same slog stream written to stdout, teed to disk so a crash is diagnosable where nothing collects stdout (`CRONOMICON_LOG_FILE`). Mode `0640` in a `0750` directory. Rotated by size, not date; the generations don't end in `.log` and the live file is excluded by name, so the run-log reaper touches neither. **Not redacted** — see `deploy/security-review.md`. |
| `/var/lib/amadeus/logs/audit.log` (+ `audit.log.YYYYMMDD`) | compliance **audit stream** — JSON Lines, one record per audited event, rotated daily by UTC date (`CRONOMICON_AUDIT_LOG`). Mode `0640` in a `0750` directory. The dated generations don't end in `.log`, so the run-log reaper skips them; the live file is excluded from it by name. Governed by the `auditLogFiles` retention knob (730 days), not `logFiles`. Also **not redacted**. |
| `/var/lib/amadeus/backups/` | nightly `VACUUM INTO` snapshots before S3 upload (A4) |
| `/var/lib/amadeus/git-cache/job-definitions/` | cached clone of the job-definitions repo (B3, `CRONOMICON_GIT_CACHE_DIR`) |

The three log paths above are the **defaults**. The run-log directory is
operator-editable (Settings → Log Storage) and since LU-5 a change is applied to
every writer immediately, no restart — so on a deployment that has moved it, all
three follow it unless `CRONOMICON_LOG_FILE` / `CRONOMICON_AUDIT_LOG` pin an absolute
path. Existing files are never moved.

The KEK for stored secrets is supplied **out of band** — a mounted secret file
(`CRONOMICON_KEK_FILE`) or env (`CRONOMICON_KEK`), **never** on this
volume, and its backup must not share the A4 S3 bucket (S14).

Degradation (T12): only the identity provider is hard-required (`/readyz` fails without it
when OIDC is configured). GitLab, Vault, Apprise/SMTP, S3, and runners all
degrade gracefully — the server boots and serves with them absent.

### Backups & CI

- **Backups (A4):** nightly `VACUUM INTO` snapshot + S3 upload (`internal/backup`,
  minio-go — AWS or any S3-compatible endpoint). Restore runbook:
  `deploy/backup-restore.md`.
- **CI-time validation (V1.1-1 / T11):** `deploy/ci-validate-template.yml` is the
  drop-in `.gitlab-ci.yml` snippet for the job-definitions repo that runs
  `cronomicon validate` on every MR (fail-fast, line-numbered; opt-in MR-comment
  job included). Operator walkthrough — copy, pin `CRONOMICON_IMAGE`, read a
  failure — in **`deploy/README.md` → "CI Setup"**. The CLI is covered by an
  e2e test (`cmd/amadeus/validate_test.go`).

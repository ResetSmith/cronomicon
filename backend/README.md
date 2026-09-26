# Cronomicon backend

Go backend for Cronomicon (single static binary, single process). Serves
the API + embedded frontend from one container, backed by SQLite.

> Two binaries: `cmd/cronomicon` (server) and `cmd/cronomicon-runner` (the
> out-of-process runner agent). The release of record is the top entry of
> `../CHANGELOG.md`; the layout below maps the main packages.

## Layout

```
cmd/cronomicon/        entrypoint + `validate`, `restore`, `rewrap-secrets` subcommands
internal/
  config/           env → typed Config
  db/               SQLite open, golang-migrate, retention sweep, UUIDv7 ids
    migrations/     numbered NNN_name.{up,down}.sql
  auth/             OIDC RP + trusted-header SSO, session cookie + CSRF, runner bearer,
                    role derivation, ResolveGrants (the single access resolver)
  api/              net/http router, health/readiness, per-feature mount files
  httpx/            error envelope + JSON helpers
web/                embedded frontend build (dist/) — produced by Vite
openapi.yaml        the contract (canonical, single-owner)
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

There is no Go codegen step: the Go request/response types are hand-written and
held to `openapi.yaml` by the conformance tests in `internal/api/`; only the
frontend generates a client (`npm run gen`).

## Configuration (environment)

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_ADDR` | `:8080` | listen address |
| `CRONOMICON_DB_PATH` | `/var/lib/cronomicon/cronomicon.db` | SQLite file (mounted volume) |
| `CRONOMICON_LOG_LEVEL` / `_FORMAT` | `info` / `json` | |
| `CRONOMICON_LOG_FILE_ENABLED` / `CRONOMICON_LOG_FILE` / `_MAX_MB` / `_KEEP` | `true` / — / `64` / `5` | Process log written to disk **in addition to stdout** (stdout is never replaced). Empty `CRONOMICON_LOG_FILE` ⇒ `cronomicon.log` in the run-log directory, following it live when that setting changes; an explicit path must be **absolute** and attaches earlier in boot (captures the build banner + config warnings). Rotates at `_MAX_MB` keeping `_KEEP` generations (`cronomicon.log.1` … `.N`), so disk ≤ `(KEEP+1) × MAX_MB`. **Not** governed by `_RETENTION_LOG_FILES_DAYS`, and **not** redacted — see `deploy/security-review.md`. |
| `CRONOMICON_AUDIT_LOG_ENABLED` / `CRONOMICON_AUDIT_LOG` | `true` / — | Compliance **audit stream** (`audit.log`) — a separate file from the process log: one JSON-Lines record per audited event (`change_log`, a named subset of `activity`, and every auth event), keys fixed by a versioned schema (`"v": 1`). Empty path ⇒ `audit.log` in the run-log directory, following it live; an explicit path must be **absolute**. Rotates **daily by UTC date** to `audit.log.YYYYMMDD`; lifetime is the `auditLogFiles` retention knob (default 730 days), not a keep count. The **database is authoritative** — the file is an export of it, so `false` loses the stream, not the trail. Durable; masked by the process-wide redaction dictionary before each row is stored — see `deploy/security-review.md`. |
| `CRONOMICON_OIDC_ISSUER` / `_CLIENT_ID` / `_CLIENT_SECRET` / `_REDIRECT_URL` | — | OIDC relying party. Unset ⇒ login disabled (degraded). |
| `CRONOMICON_SESSION_HASH_KEY` / `_BLOCK_KEY` | — | base64 32 bytes; unset ⇒ ephemeral keys (dev only) |
| `CRONOMICON_COOKIE_SECURE` | `true` | set `false` for local HTTP |
| `CRONOMICON_DEV_AUTH` | `false` | **dev only** — exposes `GET /auth/dev-login`, a one-click bypass that mints a synthetic admin session without the identity provider. Never enable in a deployed environment. |
| `CRONOMICON_DEV_SEED` | `false` | **dev only** — loads representative demo data into an *empty* DB on boot so every view renders. No-op once the DB has data. |
| `CRONOMICON_RUNNER_BOOTSTRAP_TOKEN` | — | shared runner registration token |
| `CRONOMICON_KEK_FILE` / `CRONOMICON_KEK` | — | stored-secret KEK: mounted file preferred, env fallback. **Back up separately from the DB.** |
| `CRONOMICON_RETENTION_RUNS_DAYS` / `_CHANGELOG_DAYS` / `_LOG_FILES_DAYS` | `90` / `365` / `90` | Retention **bootstrap defaults only** — they seed the `auditCompliance` settings blob on the first boot that finds it unset; afterwards the eleven per-table knobs in Settings → Audit & Compliance are authoritative (the knobs with no env var — `auditLogFiles`, `recycleBin`, `definitionRevisions`, `runnerPlacementHistory`, `archivedLogFiles` — start at their own defaults). `_LOG_FILES_DAYS` reaps **on-disk run logs**. |
| `CRONOMICON_BACKUP_S3_BUCKET` | — | nightly VACUUM-INTO upload target; empty disables upload |
| `CRONOMICON_BACKUP_S3_ENDPOINT` / `_REGION` / `_ACCESS_KEY` / `_SECRET_KEY` / `_USE_SSL` | — / `us-east-1` / — / — / `true` | S3-compatible target (AWS/MinIO/Ceph/R2). See `deploy/backup-restore.md` |

> **Namespace note.** Names under the reserved prefixes `CRONOMICON_VAR_`,
> `CRONOMICON_SECRET_`, `CRONOMICON_KEY_`, and `CRONOMICON_RUN_` are **not** configuration —
> they are references the app resolves and injects into runs. Everything else
> under `CRONOMICON_*` (the table above) is server/runner config. This is why the KEK
> reads `CRONOMICON_KEK*`: it is config, not a secret named "KEK" in the store.
> See `deploy/env-matrix.md`.

## Local preview (before SSO)

To browse the UI with realistic content before OIDC/GitLab/runners are wired up:

```bash
CRONOMICON_DEV_AUTH=true CRONOMICON_DEV_SEED=true CRONOMICON_COOKIE_SECURE=false \
  CRONOMICON_DB_PATH=/tmp/cronomicon-dev.db ./bin/cronomicon
```

Open the app and click **"Developer login (bypass SSO)"** on the sign-in
screen. Both flags are off by default and the dev-login route is not even
mounted unless `CRONOMICON_DEV_AUTH=true`, so production never exposes it. The seed
is a no-op once the DB has data, so it never clobbers a real (GitLab-synced) DB.

## Health

- `GET /healthz` — liveness (always 200 if the process is up).
- `GET /readyz` — readiness: DB reachable + migrations clean; identity check
  added when OIDC is configured. Both unauthenticated.

## Deployment & storage layout

Single container, single process. All durable state lives under one mounted
volume at `/var/lib/cronomicon` (the `Dockerfile` declares it as a `VOLUME`):

| Path | Contents |
|---|---|
| `/var/lib/cronomicon/cronomicon.db` (+ `-wal`/`-shm`) | SQLite database (`CRONOMICON_DB_PATH`) |
| `/var/lib/cronomicon/logs/<code>/<traceId>.log` | per-run log files (redacted at ingest), grouped into one folder per job/workflow. `<code>` is an opaque 8-hex-character code from the `entity_codes` registry; runs owned by no definition (the SSH connection test) use `_system`. Each folder carries a `_meta.json` sidecar naming the owning entity. |
| `/var/lib/cronomicon/logs/cronomicon.log` (+ `cronomicon.log.1` … `.N`) | **process log** — the same slog stream written to stdout, teed to disk so a crash is diagnosable where nothing collects stdout (`CRONOMICON_LOG_FILE`). Mode `0640` in a `0750` directory. Rotated by size, not date; the generations don't end in `.log` and the live file is excluded by name, so the run-log reaper touches neither. **Not redacted** — see `deploy/security-review.md`. |
| `/var/lib/cronomicon/logs/audit.log` (+ `audit.log.YYYYMMDD`) | compliance **audit stream** — JSON Lines, one record per audited event, rotated daily by UTC date (`CRONOMICON_AUDIT_LOG`). Mode `0640` in a `0750` directory. The dated generations don't end in `.log`, so the run-log reaper skips them; the live file is excluded from it by name. Governed by the `auditLogFiles` retention knob (730 days), not `logFiles`. Masked by the process-wide redaction dictionary before each row is stored. |
| `/var/lib/cronomicon/backups/` | nightly `VACUUM INTO` snapshots before S3 upload |
| `/var/lib/cronomicon/git-cache/job-definitions/` | cached clone of the job-definitions repo (`CRONOMICON_GIT_CACHE_DIR`) |

The three log paths above are the **defaults**. The run-log directory is
operator-editable (Settings → Log Storage) and a change is applied to
every writer immediately, no restart — so on a deployment that has moved it, all
three follow it unless `CRONOMICON_LOG_FILE` / `CRONOMICON_AUDIT_LOG` pin an absolute
path. Existing files are never moved.

The KEK for stored secrets is supplied **out of band** — a mounted secret file
(`CRONOMICON_KEK_FILE`) or env (`CRONOMICON_KEK`), **never** on this
volume, and its backup must not share the S3 backup bucket.

Degradation: only the identity provider is hard-required (`/readyz` fails without it
when OIDC is configured). GitLab, Vault, Apprise/SMTP, S3, and runners all
degrade gracefully — the server boots and serves with them absent.

### Backups & CI

- **Backups:** nightly `VACUUM INTO` snapshot + S3 upload (`internal/backup`,
  minio-go — AWS or any S3-compatible endpoint). Restore runbook:
  `deploy/backup-restore.md`.
- **CI-time validation:** `deploy/ci-validate-template.yml` is the
  drop-in `.gitlab-ci.yml` snippet for the job-definitions repo that runs
  `cronomicon validate` on every MR (fail-fast, line-numbered; opt-in MR-comment
  job included). Operator walkthrough — copy, pin `CRONOMICON_IMAGE`, read a
  failure — in **`deploy/README.md` → "CI Setup"**. The CLI is covered by an
  e2e test (`cmd/cronomicon/validate_test.go`).

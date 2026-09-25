# Cronomicon deployment (Phase B)

Reproducible container image + a Docker Compose stack that puts Cronomicon behind
TLS and Authelia Trusted Header SSO. After this phase the app is genuinely
deployable. See the production-readiness plan (in the local-only planning archive) for the locked decisions
and topology.

```
            ┌──────── TLS (443) ────────┐
 browser ──▶│ traefik                   │
            │  • terminates TLS         │
            │  • forward-auth ─▶ authelia (Trusted Header SSO; LDAP/AD)
            │  • injects Remote-*       │
            └──────────┬────────────────┘
                       │ internal docker network (amadeus NOT published)
                       ▼
              amadeus (Go binary + embedded SPA)  ──▶ apprise (Phase C)
                       │
                       ▼  /var/lib/amadeus  (SQLite + WAL, run logs, git cache, backups)
```

## Files

| Path | Purpose |
|---|---|
| `docker-compose.yml` | The stack: traefik · authelia · amadeus · apprise. |
| `amadeus.env.example` | Copy to `amadeus.env` (gitignored) — the app's runtime env. |
| `env-matrix.md` | Every env var, default, and which are required. |
| `traefik/dynamic/dynamic.yml` | TLS + edge `Remote-*` stripping middleware. |
| `traefik/certs/` | Mount your `amadeus.crt`/`amadeus.key` here (gitignored). |
| `authelia/configuration.yml` | Authelia example (align with your existing instance). |
| `authelia/users_database.yml.example` | File-backend users for the standalone example. |
| `migrations-runbook.md` | Forward-only policy + dirty-migration recovery. |
| `backup-restore.md` | Nightly snapshot + restore drill. |
| `Dockerfile.runner` | Slim runner-agent image (SSH-onward; static/distroless). |
| `Dockerfile.runner.fat` | Fat runner-agent image (+ ansible/terraform toolchains). |
| `amadeus-runner.service` | systemd unit for the runner agent (non-root, hardened). |
| `amadeus-runner.env.example` | Annotated `CRONOMICON_RUNNER_*` env template. |

The runner **Install / Config / Security guides** now live in `documentation/`
(`runner-install.html`, `runner-manage.html`, `runner-security.html`). They are the
single source for the in-app **Runner Guides** — the build wraps each fragment into a
standalone styled page served at `/runner-<name>.html` and linked (new tab) from the
Runners view. (`runner-install.sh`, the fast-path install script, stays in this dir;
the frontend build publishes it into the app at `/runner-install.sh`.)

The server image also **bundles the runner-agent binaries**: a Dockerfile stage
cross-compiles `amadeus-runner` for linux amd64/arm64 (+ `SHA256SUMS`) into
`/usr/share/amadeus/agents/`, served unauthenticated at `GET /agents/{filename}`
(override the directory with `CRONOMICON_AGENT_DIR`; see `env-matrix.md` and the
rationale in `security-review.md`). `runner-install.sh --download` consumes this —
a runner host needs nothing but curl + reachability to the Cronomicon server.

The image is built from `../Dockerfile` with **build context = repo root** so the
React frontend is built in-image and embedded (`docker build .` from a clean
checkout embeds a UI matching source HEAD — B.1). Compose sets `context: ../..`.

## First deploy

1. **Certs** → drop `amadeus.crt` + `amadeus.key` in `traefik/certs/` (or switch
   `traefik/dynamic/dynamic.yml` + the traefik command to an ACME resolver).
2. **Hostnames** → replace `amadeus.example.com` / `auth.example.com` /
   `example.com` in `docker-compose.yml` and `authelia/configuration.yml`.
3. **Authelia** → point at your existing LDAP/AD backend (or use the file
   backend: `cp authelia/users_database.yml.example authelia/users_database.yml`
   and generate a password hash). Provision its secrets under
   `authelia/secrets/{jwt_secret,session_secret,storage_encryption_key}`.
   Ensure AD group membership surfaces in `Remote-Groups`.
4. **App env** → `cp amadeus.env.example amadeus.env` and fill it in. Keep
   `CRONOMICON_TRUSTED_PROXIES` = Traefik's static internal IP (`172.28.0.2/32` in
   this stack — the load-bearing half of the Phase A trusted-proxy control).
5. **Secret KEK** → write the base64 KEK to `secrets/amadeus_kek` (mounted at
   `/run/secrets/amadeus_kek`). **Back this up separately from the S3 DB backup**
   (S14) — losing it makes stored secrets unrecoverable.
6. **Bootstrap admin** → leave `CRONOMICON_BOOTSTRAP_ADMIN_GROUP=cronomicon-admins` set
   for the first deploy. Bring the stack up:
   ```
   docker compose up -d --build
   ```
7. **Seed real mappings** → log in (via Authelia) as a member of that group; you
   land as admin. In Settings, create the real group→role `ad_group_mappings`.
8. **Lock down** → remove `CRONOMICON_BOOTSTRAP_ADMIN_GROUP` from `amadeus.env` and
   `docker compose up -d` again. Admin now comes only from DB mappings.

## Build provenance

Compose passes version/commit/date as build args (defaults `dev`/`none`). For a
stamped image:
```
docker compose build \
  --build-arg VERSION=$(git -C ../.. describe --tags --always --dirty) \
  --build-arg COMMIT=$(git -C ../.. rev-parse --short HEAD) \
  --build-arg BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
```
Verify at `GET /version` (and surfaced in `/healthz`).

## Persistence & volume layout (B.4)

All durable state lives on the `amadeus-data` volume at `/var/lib/amadeus`:

| Path | Contents |
|---|---|
| `amadeus.db` (+ `-wal`, `-shm`) | SQLite database (`CRONOMICON_DB_PATH`). |
| `git-cache/` | GitLab clone cache (`CRONOMICON_GIT_CACHE_DIR`). |
| `backups/` | Nightly local `VACUUM INTO` snapshots. |
| run logs | Per-run execution logs. |

**Single writer** — exactly one `amadeus` replica (SQLite is single-node; no HA).
Data survives `docker compose down && up` because it's on a named volume.

## Health & readiness (B.5)

- `GET /healthz` — liveness (process up; also returns version/commit).
- `GET /readyz` — readiness: DB reachable + migrations applied + (oidc mode)
  identity provider up. 503 until ready.
- Container HEALTHCHECK: `amadeus healthcheck -ready` — the distroless image has
  no shell/curl, so the binary probes itself over HTTP.

## Backups & retention (B.7)

Nightly retention sweep + `VACUUM INTO` snapshot are already wired
(`internal/backup`). Configure the S3 target (or accept local-only snapshots) and
retention windows via env — see `backup-restore.md` and `env-matrix.md`.

## CI Setup — validate job definitions (V1.1-1 / T11)

The Cronomicon binary ships an `amadeus validate` subcommand that runs the **same
parser the runtime uses** (YAML `apiVersion`, kinds, schedules, inventory
pragmas, and cross-file `script_ref` resolution), so CI and runtime never
disagree. Wire it into the **job-definitions repo** (the GitLab repo holding
`jobs/`, `workflows/`, `scripts/`, `inventory/`) so malformed definitions are
caught on the merge request instead of at the next sync.

1. **Copy the template.** `ci-validate-template.yml` (next to this README) is a
   drop-in `.gitlab-ci.yml` for the job-definitions repo. Copy its contents into
   that repo's `.gitlab-ci.yml` (or `include:` it).
2. **Pin the image.** Set `CRONOMICON_IMAGE` to the same Cronomicon image tag your
   deployment runs (e.g. `registry.example.com/cronomicon:v0.19.0`) — pinning keeps
   the validator and the runtime parser in lock-step. Verify the tag at
   `GET /version`.
3. **(Optional) MR comments.** The base job already fails the pipeline — blocking
   the merge and printing line-numbered errors in the job log. To *also* post the
   errors as an MR comment, uncomment the `validate-and-comment` job in the
   template and add a masked `GITLAB_MR_TOKEN` CI/CD variable (a Project/Group
   Access Token or PAT with the `api` scope — `CI_JOB_TOKEN` can't post notes).

**Reading a failure.** A failed job prints one `path:line: message` per problem,
e.g.:

```
validating jobs/backup.yaml
jobs/backup.yaml:2: unsupported apiVersion "cronomicon.io/v2"
```

Fix the file at the reported line and push; the pipeline re-runs on the MR.
Reproduce any failure locally with `amadeus validate jobs/backup.yaml` (or
`amadeus validate .` for a whole-repo cross-`script_ref` check).

## Verifying the stack (exit criteria)

- `docker compose up -d --build` brings the stack up; the UI is reachable **only**
  over TLS through Traefik — `curl http://localhost:8080` from the host fails
  (amadeus is not published).
- Hitting `https://amadeus.example.com` redirects through Authelia; after login
  you arrive authenticated with roles from `Remote-Groups`.
- `docker compose down && docker compose up -d` preserves the DB, logs, git cache.

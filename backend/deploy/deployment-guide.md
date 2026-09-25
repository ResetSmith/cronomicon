# Cronomicon Deployment Guide

End-to-end walkthrough for deploying Cronomicon to a Docker test or production
environment. Written against v0.36.5 and the compose stack in this directory.

---

## Overview

Cronomicon is a single Go binary that serves both the API and the embedded React
SPA. It uses SQLite, so it is single-node, single-replica — no clustering.

### Deploy topology options

**Option A — Bundled stack (self-contained)**

The compose file ships with Traefik (TLS termination) + Authelia (SSO) bundled:

```
browser ──TLS 443──▶ traefik ──forward-auth──▶ authelia (LDAP/AD → Remote-Groups)
                          │ injects Remote-*
                          ▼
                       amadeus:8080  (internal Docker network; NOT published to host)
                          │
                          ├──▶ /var/lib/amadeus volume
                          └──▶ apprise
```

In this topology `cronomicon` is not exposed on the host — only Traefik publishes
80/443. Use `CRONOMICON_TRUSTED_PROXIES=172.28.0.2/32` (Traefik's static IP).

**Option B — External proxy (our current setup)**

If you already run a reverse proxy (e.g. Nginx Proxy Manager) and SSO layer,
remove Traefik and Authelia from the compose file and publish cronomicon directly:

```
browser ──TLS──▶ Nginx Proxy Manager (existing; injects Remote-*)
                          │
                          ▼ proxied to amadeus:8080
                       cronomicon (published on host port, e.g. 8080)
                          │
                          └──▶ /var/lib/amadeus volume
```

Publish cronomicon on the host by adding a `ports` entry to the `cronomicon` service
and removing the `internal: true` constraint from the network so NPM can reach
it. Use `CRONOMICON_TRUSTED_PROXIES=10.0.0.0/8` (or NPM's specific IP).

**Our `amadeus.env` is already configured for Option B** (NPM on `10.x.x.x`,
external Apprise at `apprise.example.com`). The steps below call out where
the two options diverge.

---

## Prerequisites

On the Docker host:

- Docker Engine 24+ and Docker Compose v2 (`docker compose`, not `docker-compose`)
- The repository checked out at the host (or the pre-built image pushed to a registry)
- DNS entries for your FQDNs pointing at the host
- TLS certificate + key for the cronomicon hostname (wildcard or SAN cert)

If you are building remotely (CI push to registry, then deploy), see
[Remote / CI build](#remote--ci-build) below. The instructions that follow
assume you are building on the Docker host directly.

---

## Step 1 — Clone and switch to the release

```bash
git clone git@gitlab.example.com:ops/amadeus.git
cd cronomicon
git checkout v0.36.5
```

Verify you are on the tagged commit:

```bash
git describe --tags --exact-match   # should print v0.36.5
```

---

## Step 2 — Generate the Secret KEK

The KEK (Key Encryption Key) is a 32-byte AES-256 key stored as base64. It
encrypts every secret the app holds (SMTP passwords, stored credentials). It is
**not** in the repo and must be provisioned before first boot.

```bash
cd backend/deploy
mkdir -p secrets
openssl rand -base64 32 > secrets/amadeus_kek
chmod 600 secrets/amadeus_kek
```

> **Critical:** Back this file up independently and separately from any database
> backup. If you lose the KEK and your S3/volume backup together, every stored
> secret is permanently unrecoverable. The S3 backup alone is useless without it.

The compose file mounts `secrets/amadeus_kek` read-only into the container at
`/run/secrets/amadeus_kek`, which `CRONOMICON_KEK_FILE` points at. (The KEK is app
config, not a store secret, so it lives under `CRONOMICON_KEK*`; the older
`CRONOMICON_SECRET_KEK*` spelling stopped reading in v1.5.41.)

If you already have a KEK from a previous instance (e.g. migrating from a dev
box), copy the existing value here instead of generating a new one, or your
stored secrets will fail to decrypt.

---

## Step 3 — TLS certificates

Drop your TLS certificate and key into `traefik/certs/`:

```bash
cp /path/to/amadeus.crt traefik/certs/amadeus.crt
cp /path/to/amadeus.key traefik/certs/amadeus.key
chmod 600 traefik/certs/amadeus.key
```

The Traefik dynamic config at `traefik/dynamic/dynamic.yml` references these
paths. TLS 1.2 minimum is enforced; SNI strict mode is on.

If you prefer ACME (Let's Encrypt / internal CA), replace the `tls.certificates`
block in `traefik/dynamic/dynamic.yml` with a `certificatesResolver` section in
the Traefik startup command in `docker-compose.yml`. See the Traefik v3 docs.

---

## Step 4 — Set hostnames

Replace the placeholder FQDNs in two files:

**`docker-compose.yml`** — search for `example.com` and replace with your real
FQDNs in the three Traefik label rules:

```yaml
# authelia service
- "traefik.http.routers.authelia.rule=Host(`auth.YOUR-DOMAIN`)"

# cronomicon service
- "traefik.http.routers.amadeus.rule=Host(`amadeus.YOUR-DOMAIN`)"
```

**`authelia/configuration.yml`** — replace `example.com` / `auth.example.com` /
`amadeus.example.com` throughout:

```yaml
access_control:
  rules:
    - domain: auth.YOUR-DOMAIN
      policy: bypass
    # Runner endpoints bypass the browser SSO — a runner host has no Authelia
    # session (it uses its own crn_run_*/crn_reg_* bearer, still enforced by the
    # app on the /api paths). WITHOUT this, `curl .../install/<token>` and the
    # runner's poll/register are redirected to the login page and return HTML,
    # not the script — the "syntax error near `<!doctype html>'" install failure.
    # Must come BEFORE the one_factor rule.
    - domain: amadeus.YOUR-DOMAIN
      policy: bypass
      resources:
        - "^/healthz$"
        - "^/readyz$"
        - "^/version$"
        - "^/runner-install\\.sh$"
        - "^/agents/[^/]+$"
        - "^/install/[^/]+$"
        - "^/api/v1/runners/register$"
        - "^/api/v1/runners/[^/]+/poll$"
        - "^/api/v1/runners/[^/]+/redeclare$"
        - "^/api/v1/runners/[^/]+/hostkeys$"
        - "^/api/v1/runs/[^/]+/manifest$"
        - "^/api/v1/runs/[^/]+/log$"
    - domain: amadeus.YOUR-DOMAIN
      policy: one_factor
      subject:
        - "group:cronomicon-users"

session:
  cookies:
    - domain: YOUR-DOMAIN
      authelia_url: https://auth.YOUR-DOMAIN
      default_redirection_url: https://amadeus.YOUR-DOMAIN
```

---

## Step 5 — Configure Authelia

The compose stack ships its own Authelia instance. If you already run Authelia
elsewhere you have two options:

**Option A: use the bundled Authelia (recommended for test)**

Point it at your existing LDAP/AD. Edit `authelia/configuration.yml` and swap
the `authentication_backend.file` block for your LDAP backend:

```yaml
authentication_backend:
  ldap:
    address: ldaps://dc.your-domain.com
    base_dn: DC=your-domain,DC=com
    users_filter: (&({username_attribute}={input})(objectClass=user))
    groups_filter: (&(member={dn})(objectClass=group))
    user: CN=authelia,OU=ServiceAccounts,DC=your-domain,DC=com
    # bind password: AUTHELIA_AUTHENTICATION_BACKEND_LDAP_PASSWORD_FILE env
```

The `Remote-Groups` header will carry the user's AD group memberships, which
Cronomicon maps to operator roles via **Settings → Group Mappings**.

**Option B: file backend (standalone, for dev/test without LDAP)**

```bash
cp authelia/users_database.yml.example authelia/users_database.yml
```

Generate a password hash for each test user:

```bash
docker run --rm authelia/authelia:4.38 \
  authelia crypto hash generate argon2 --password 'TestPassword123'
```

Paste the output into `users_database.yml` under the appropriate user entry.

**Provision Authelia secrets** (required regardless of backend):

```bash
mkdir -p authelia/secrets
openssl rand -hex 64 > authelia/secrets/jwt_secret
openssl rand -hex 64 > authelia/secrets/session_secret
openssl rand -hex 64 > authelia/secrets/storage_encryption_key
# if using LDAP:
echo 'your-ldap-bind-password' > authelia/secrets/ldap_password
chmod 600 authelia/secrets/*
```

These are mounted read-only by the `authelia` service (see `docker-compose.yml`).

**Option C: use an existing external Authelia**

Remove the `authelia` service from `docker-compose.yml` entirely and remove the
`authelia` `depends_on` from the `cronomicon` service. Update the Traefik middleware
label on `cronomicon` to point `forwardauth.address` at your existing Authelia URL.
Ensure your Authelia's access policy passes `Remote-User,Remote-Groups,Remote-Email,Remote-Name`
in `authResponseHeaders`.

---

## Step 6 — Configure `amadeus.env`

The file `amadeus.env` (gitignored) is already partially filled in. Verify and
complete each section:

### Auth

```bash
CRONOMICON_AUTH_MODE=trusted-header
```

**`CRONOMICON_TRUSTED_PROXIES` — this is the most important setting and the #1
cause of login failures if wrong.** It must exactly match the IP (or CIDR) that
cronomicon sees as the source of requests from your reverse proxy.

**Our setup uses an existing Nginx Proxy Manager (NPM) on the `10.x.x.x`
network, not the bundled Traefik.** The env file is already set correctly for
this:

```bash
CRONOMICON_TRUSTED_PROXIES=10.0.0.0/8
```

If you are deploying behind a different edge, use the specific IP or CIDR of
that proxy. For the bundled Traefik compose stack the value would instead be
`172.28.0.2/32` (Traefik's static internal address). When in doubt, check what
IP cronomicon logs as the peer on a request:

```bash
docker compose logs cronomicon | grep "peer not in trusted"
# or enable debug logging to see the remote addr on every request
```

The app is **fail-closed**: an empty value causes it to refuse to boot in
`trusted-header` mode (unless `CRONOMICON_DEV_AUTH=true`, which must never be set
in production).

```bash
CRONOMICON_LOGOUT_REDIRECT_URL=https://auth.example.com/logout
CRONOMICON_BOOTSTRAP_ADMIN_GROUP=cronomicon-admins   # FIRST DEPLOY ONLY — remove after step 10
CRONOMICON_COOKIE_SECURE=true
```

### KEK

```bash
CRONOMICON_KEK_FILE=/run/secrets/amadeus_kek   # already set; matches the compose mount
```

### GitLab integration

```bash
CRONOMICON_GITLAB_BASE_URL=https://gitlab.example.com/ops/amadeus-ops.git
CRONOMICON_GITLAB_TOKEN=<read-scoped PAT — already set in amadeus.env>
CRONOMICON_GITLAB_WEBHOOK_SECRET=<already generated in amadeus.env>
```

The webhook secret is optional but recommended: it validates that push events
come from GitLab. If left empty, sync still works on a timer and via manual
trigger, but push-triggered syncs are unauthenticated.

> **Note:** While `CRONOMICON_GITLAB_WEBHOOK_SECRET` is set in env, the
> webhook-secret rotation API returns 409. Rotate only via env (redeploy), not
> the UI, when using env-pinned secrets.

### Runner bootstrap token

```bash
CRONOMICON_RUNNER_BOOTSTRAP_TOKEN=<already generated in amadeus.env>
```

Runners use this token to register themselves. It is valid for 24 hours per
registration. See [Registering a runner](#registering-a-runner) below.

### Notifications

```bash
CRONOMICON_APPRISE_URL=https://apprise.example.com   # already set
```

SMTP server settings are configured in the UI (**Settings → Notifications**),
not via env. The SMTP password is stored envelope-encrypted with the KEK.

### Backups

Leave S3 fields empty to keep local-only snapshots on the volume:

```bash
CRONOMICON_BACKUP_S3_BUCKET=       # empty = local snapshots only
```

To enable nightly S3 uploads (MinIO or AWS), fill in endpoint, region, and
credentials. See `backup-restore.md`.

### Dev flags — must stay commented out in production

```bash
# CRONOMICON_DEV_AUTH=true     # one-click admin login with no Authelia — NEVER in prod
# CRONOMICON_DEV_SEED=true     # seeds demo data — NEVER in prod
```

---

## Step 7 — Adjust compose file for your topology

**Option A (bundled Traefik):** no changes needed. Replace hostnames per Step 4
and proceed to Step 8.

**Option B (external NPM — our setup):** edit `docker-compose.yml` to publish
cronomicon on the host and remove Traefik/Authelia:

1. On the `cronomicon` service add a `ports` entry:
   ```yaml
   amadeus:
     ports:
       - "8080:8080"
   ```

2. Remove or comment out the `traefik` and `authelia` services entirely.

3. Remove the `depends_on: authelia` block from the `cronomicon` service.

4. Change the `internal` network to a regular (non-isolated) network by
   removing `internal: true`:
   ```yaml
   networks:
     internal:
       # internal: true   ← remove this line
       ipam:
         config:
           - subnet: 172.28.0.0/24
   ```

5. Remove the `edge` network and its reference from `traefik` (since there is
   no traefik).

NPM then proxies to `http://<docker-host>:8080` and injects `Remote-*` headers.
Ensure NPM strips any client-supplied `Remote-*` headers before injecting its
own — this is the strip-remote-headers middleware that `traefik/dynamic/dynamic.yml`
handles in Option A. In NPM, set custom request headers:
`Remote-User: `, `Remote-Groups: `, `Remote-Email: `, `Remote-Name: ` (blank =
stripped) before the SSO-injected values are added.

---

## Step 8 — Build and bring the stack up

From `backend/deploy/`:

```bash
# Stamp the version from the git tag so /version shows v0.36.5
export CRONOMICON_VERSION=$(git -C ../.. describe --tags --exact-match 2>/dev/null \
  || git -C ../.. describe --tags --always --dirty)
export CRONOMICON_COMMIT=$(git -C ../.. rev-parse --short HEAD)
export CRONOMICON_BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)

docker compose up -d --build
```

The build takes a few minutes — Node builds the React frontend first (Stage 1),
then Go compiles the binary with the freshly built UI embedded (Stage 2), then
the distroless runtime image is assembled (Stage 3). The build context is the
**repository root**, not `backend/`, so the frontend is built in-image and
embedded fresh — never shipped as a stale committed asset.

Watch the logs:

```bash
docker compose logs -f cronomicon
```

On a healthy first boot you will see:

```
cronomicon build version=v0.36.5 commit=<sha> built=<date>
database migrations applied
git sync started
listening on :8080
```

If the app refuses to boot with `trusted proxies required`, check
`CRONOMICON_TRUSTED_PROXIES` (Step 6).

---

## Step 9 — Verify the deployment

Run the verification script against the live stack:

```bash
APP_URL=https://amadeus.YOUR-DOMAIN ./verify-deployment.sh
```

Expected output:

```
== Reachability ==
  PASS: /healthz returns 200
  PASS: /readyz returns 200
  PASS: /version returns 200

== Auth surface (through the proxy) ==
  PASS: /api/v1/me not reachable unauthenticated (got 302)
  PASS: /api/v1/auth/dev-login absent (404)

== Trusted-proxy enforcement (THE critical control) ==
  SKIP: set APP_DIRECT_URL to test spoofed-header rejection directly.

== Summary: 4 passed, 0 failed
```

**The spoofed-header check is the single most important security control.**
Because we use an external NPM rather than the bundled Traefik, cronomicon is
accessible on the Docker host at port 8080 (the internal network is not
`internal: true` in this topology). Always run the direct-port probe:

```bash
APP_URL=https://amadeus.YOUR-DOMAIN \
APP_DIRECT_URL=http://127.0.0.1:8080 \
./verify-deployment.sh
```

`/api/v1/me` with spoofed `Remote-User: attacker` must **not** return 200. If it
does, `CRONOMICON_TRUSTED_PROXIES` is misconfigured.

Also verify manually:

```bash
# cronomicon must NOT be reachable on the host directly — only Traefik publishes ports
curl http://localhost:8080        # must fail / connection refused
curl https://amadeus.YOUR-DOMAIN  # must redirect to Authelia login
```

---

## Step 10 — First login and bootstrap admin

1. Navigate to `https://amadeus.YOUR-DOMAIN` in a browser.
2. Authelia redirects you to its login page.
3. Log in as a user who is a member of the AD group set in
   `CRONOMICON_BOOTSTRAP_ADMIN_GROUP` (`cronomicon-admins` in the example env).
4. You land in Cronomicon as an admin.
5. Go to **Settings → Group Mappings** and create permanent `ad_group → role`
   mappings for your real groups (e.g. `cronomicon-admins → admin`,
   `cronomicon-operators → operator`).

---

## Step 11 — Remove the bootstrap admin group

Once real group mappings are in place:

1. Open `amadeus.env` and remove (or comment out) `CRONOMICON_BOOTSTRAP_ADMIN_GROUP`.
2. Redeploy:
   ```bash
   docker compose up -d
   ```
3. Verify admin access still works via the real group mapping (not the bootstrap).

Leaving `CRONOMICON_BOOTSTRAP_ADMIN_GROUP` set after seeding is a security risk —
it grants admin to any member of that group regardless of the DB mappings.

---

## Step 12 — Configure GitLab webhook (optional but recommended)

For push-triggered syncs (jobs update immediately on merge):

1. In GitLab, go to your `amadeus-ops` repo → **Settings → Webhooks**.
2. URL: `https://amadeus.YOUR-DOMAIN/api/v1/gitlab/webhook`
3. Secret token: the value you set in `CRONOMICON_GITLAB_WEBHOOK_SECRET`
4. Trigger: **Push events**
5. SSL verification: enabled

Without the webhook, Cronomicon syncs on a timer (~every 5 minutes) and on manual
trigger from the UI. Push-triggered sync is a convenience, not a requirement.

---

## Step 13 — Registering a runner

Cronomicon needs at least one runner to execute jobs. Runners are separate
processes that poll the cronomicon API, claim work, and report results.

See `documentation/runner-install.html` for full instructions (and
`documentation/runner-manage.html` for day-2 ops) — these also render as the in-app
**Install / Config Guide** pages from the Runners view. Brief summary:

```bash
# On the runner host
CRONOMICON_RUNNER_URL=https://amadeus.YOUR-DOMAIN \
CRONOMICON_RUNNER_BOOTSTRAP_TOKEN=<value from amadeus.env> \
CRONOMICON_RUNNER_NAME=runner-1 \
CRONOMICON_RUNNER_OS=Linux \
CRONOMICON_RUNNER_CAPABILITIES=bash,python \
./amadeus-runner  # or use the systemd unit: amadeus-runner.service
```

Two runner images are available:

| Image | Use case |
|---|---|
| `Dockerfile.runner` | Slim — SSH executor only; static binary |
| `Dockerfile.runner.fat` | Ansible + Terraform toolchains pre-installed |

The bootstrap token is single-use per registration and expires after 24 hours.
Regenerate via `CRONOMICON_RUNNER_BOOTSTRAP_TOKEN` (redeploy) or via
**Settings → Runners** in the UI (manual re-issue).

---

## Persistence and data safety

All durable state lives on the `amadeus-data` named Docker volume at
`/var/lib/amadeus`:

| Path | Contents |
|---|---|
| `amadeus.db` (+ `-wal`, `-shm`) | SQLite database |
| `git-cache/` | GitLab clone cache (survives restarts) |
| `backups/` | Nightly local `VACUUM INTO` snapshots |
| run logs | Per-run execution output |

Named volumes survive `docker compose down && docker compose up -d`. They do
**not** live in `/tmp` — that was the local dev setup only. In production, data
persists across reboots automatically.

To inspect volume data from the host:

```bash
docker run --rm -v amadeus_amadeus-data:/data busybox ls -la /data
```

---

## Upgrading to a new version

```bash
git fetch --tags
git checkout v<new-version>

cd backend/deploy

export CRONOMICON_VERSION=$(git -C ../.. describe --tags --exact-match)
export CRONOMICON_COMMIT=$(git -C ../.. rev-parse --short HEAD)
export CRONOMICON_BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)

docker compose up -d --build
```

> Preferred since 2026-09-18: push a tag and let CI build the image and
> DockHand pull it — see "CI-fronted deployment" below. The build-on-host
> path above assumes a clean clone.

Migrations are forward-only and run automatically on startup. The app will
apply any new migrations before accepting traffic — `/readyz` returns 503 until
they complete. See `migrations-runbook.md` for recovery procedures if a
migration fails.

**There is no automated rollback.** If you need to roll back: restore the DB
from a pre-upgrade snapshot (`backup-restore.md`) and re-deploy the previous
image tag. Keep at least one snapshot from before every upgrade.

---

## CI-fronted deployment (DockHand pulls, never builds)

Since 2026-09-18 the image is built by the CI pipeline and pushed to
`registry.example.com/amadeus`. DockHand no longer builds anything: the pipeline calls
the stack's webhook after the push and DockHand re-pulls `:latest`.

Why: DockHand built from a working copy it kept across deploys; that copy
retained files git had deleted and, after a bad fetch, mixed file revisions,
so builds failed on the host that passed everywhere else (v0.52.26, v1.5.43).
CI clones fresh for every job, so the image is always the commit it claims.

Pipeline:

| Event | Jobs | Result |
|-------|------|--------|
| Merge request / push to `main` | `backend-test`, `frontend-test` | gate only |
| Push tag `vX.Y.Z` | tests → `build-image` → `dockhand-webhook` | `:vX.Y.Z` + `:latest` pushed; DockHand redeploys |

### One-time DockHand stack setup

In DockHand → the cronomicon git stack → Edit:

1. **Deploy options:** Build images on deploy **OFF**; Re-pull images **ON**;
   Force redeployment **ON**. With Build left on, DockHand runs
   `compose up --build` from its stale checkout and the old failure returns.
2. **Webhook:** Enable webhook ON. Its URL is the `DOCKHAND_WEBHOOK` CI
   variable. Do **not** also point a Git-host push webhook at
   it — the pipeline calls it only after a tag's image has been pushed, and a
   push-triggered call would redeploy before the new image exists.
3. **Environment variables:** remove `CRONOMICON_IMAGE` and `CRONOMICON_VERSION` if
   present. The compose file defaults to `registry.example.com/amadeus:latest`
   and CI stamps the version from the tag. (Set `CRONOMICON_IMAGE` only to pin a
   specific tag, e.g. to roll back to `…:v1.5.44`.)
4. **Registry credential:** only if the internal registry requires authentication
   to pull — Settings → Registries → Add Registry. The sibling stacks pull
   without one.
5. **Data volume:** the compose file declares `amadeus-data` as
   `external: true` with the name `amadeus_amadeus-data`. Confirm with
   `docker volume ls` that this is the volume the running stack uses; if
   DockHand's project name differs, correct the `name:` under `volumes:` —
   compose refuses to start rather than creating an empty one.

### Releasing

```bash
git tag v1.6.0 && git push origin v1.6.0
```

CI tests, builds, pushes, and calls the webhook; DockHand pulls and restarts.
Watch the stack in DockHand until `/readyz` is green (migrations run at
startup and cannot be rolled back — take a snapshot first, `backup-restore.md`).

### Rolling back

Set `CRONOMICON_IMAGE=registry.example.com/cronomicon:v<previous>` in the
DockHand stack's environment and Save and deploy; restore the DB snapshot if
the upgrade ran migrations. Remove the variable again after the next good
release so `:latest` resumes.

### Building on the host (break-glass)

```bash
CRONOMICON_IMAGE=amadeus:local docker compose up -d --build   # from a FRESH clone
```

Supported only from a clean checkout; not the normal path.

---

## Health endpoints

| Endpoint | Purpose | Auth required |
|---|---|---|
| `GET /healthz` | Liveness — process up; returns version/commit | No |
| `GET /readyz` | Readiness — DB reachable + migrations applied + OIDC (if configured) | No |
| `GET /version` | Build metadata: version, commit, date | No |
| `GET /metrics` | Prometheus metrics — internal network only | No (internal) |

The container HEALTHCHECK probes `/readyz` via `cronomicon healthcheck -ready`.
Traefik's `depends_on` waits for this to pass before routing traffic.

### Protecting `/metrics`

The bundled stack does not route `/metrics` through Traefik at all — Prometheus
scrapes it on the internal network, and `verify-deployment.sh` warns if it ever
answers via the public URL. If you must expose it, choose one of:

- **Bearer token in the app** — Settings → Integrations → Observability,
  Auth Type = *Bearer token*. Checked live per scrape; Prometheus sends it via
  `authorization: { credentials: ... }` in the scrape config.
- **Basic auth at the reverse proxy** — the app has no basic-auth mode on
  purpose; that is the proxy's job. With Traefik, add a `basicAuth` middleware
  and a dedicated router for the path, and keep Authelia's forward-auth **off**
  that router — a forward-auth middleware would inject `Remote-User` for the
  scraper's login, and `/metrics` has no session check to refuse it:

  ```yaml
  http:
    middlewares:
      metrics-basic:
        basicAuth:
          users:
            - "prometheus:$apr1$..."   # htpasswd -nB prometheus
    routers:
      amadeus-metrics:
        rule: "Host(`amadeus.YOUR-DOMAIN`) && Path(`/metrics`)"
        entryPoints: [websecure]
        tls: {}
        middlewares: [strip-remote-headers, metrics-basic]
        service: cronomicon   # whatever service the `cronomicon` router points at
  ```

  Because Traefik picks the most specific rule, this router wins over the
  catch-all app router for that one path. Leave Auth Type = *None* in the
  Observability card so the app does not demand a second credential.

If the metrics path is changed in Settings, it is resolved once at startup —
restart the app and update the router rule to match.

---

## Troubleshooting

**App refuses to boot: `trusted proxies required`**
`CRONOMICON_TRUSTED_PROXIES` is empty or not set. Set it to the Traefik container's
static IP (`172.28.0.2/32` for the bundled compose stack).

**Login redirects to Authelia but comes back unauthenticated / loops**
`CRONOMICON_TRUSTED_PROXIES` does not match the actual source IP reaching the app.
Check cronomicon logs for `peer not in trusted proxies`. Run
`docker network inspect amadeus_internal` to see Traefik's actual IP.

**`/readyz` returns 503 after startup**
Migrations are still running (normal for the first boot after an upgrade) or
the DB volume is not writable. Check `docker compose logs cronomicon`.

**Version shows `dev` instead of `v0.36.5`**
The `CRONOMICON_VERSION` build arg was not passed. Run the stamped build from
Step 7. Confirm with `curl https://amadeus.YOUR-DOMAIN/version`.

**Stored secrets fail to decrypt after moving from dev**
The KEK in `secrets/amadeus_kek` does not match the one used to encrypt the
secrets in the DB. Replace with the original KEK, or re-enter the secrets in
the UI after deploying with the correct KEK.

**`CRONOMICON_GITLAB_WEBHOOK_SECRET` set but rotation API returns 409**
This is expected behaviour — the env value pins the secret and blocks the
UI-based rotation API. To rotate: change the value in `amadeus.env` and
`docker compose up -d`. Update the GitLab webhook to match.

---

## Go / no-go gate

Before signing off on a production deploy, all operator items in
`go-no-go.md` must be checked. The automated (CI) items are already green.
The outstanding operator items are:

- [ ] Real Authelia login resolves roles from `Remote-Groups`; under-privileged
  user gets 403 on a role-gated route.
- [ ] `docker compose up -d --build` brings the stack up; UI reachable only via
  TLS through the proxy; app port not reachable from host; data survives
  `down && up`.
- [ ] `verify-deployment.sh` clean, including the `APP_DIRECT_URL` spoofed-header
  probe.
- [ ] Restore drill: restore from a DB snapshot with the KEK intact
  (`backup-restore.md`).
- [ ] Staging smoke path green: GitLab sync + webhook, schedule publish
  412-on-conflict, runner poll/claim/log-stream/redaction, manual + workflow
  runs, activity/audit feeds.
- [ ] A failing run delivers a real notification (email or Apprise push).
- [ ] Bootstrap admin group removed and real group mappings verified.

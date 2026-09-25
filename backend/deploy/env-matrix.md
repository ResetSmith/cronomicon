# Cronomicon environment-variable matrix

Every runtime knob (T3: config is env-only, no config file). Source of truth is
`internal/config/config.go`, plus two vars read directly by the GitLab slice and
the Phase C additions noted at the bottom. Defaults marked **fail-closed** refuse
to boot or disable a feature rather than guessing.

Legend: **req** = required for a production (trusted-header) deploy · **rec** =
recommended · **opt** = optional · **dev** = local preview only, never in prod.

## Namespace (which `CRONOMICON_*` names are config vs. references)

> **Rule:** If an `CRONOMICON_*` name starts with `VAR_`, `SECRET_`, `KEY_`, or
> `RUN_`, it is a **reference** the app resolves and injects into runs. **Every
> other `CRONOMICON_*` name is server/runner configuration** — that is, everything
> in the tables below.

The four prefixes are a **closed, reserved allowlist** — an operator reading a
script, inventory, or env file can tell config from an injected reference at a
glance. They are deliberately **absent from this config matrix**: no
configuration knob may live under a reserved prefix (the KEK family below was
the one exception, and it finished being evicted _out_ of the reserved prefix in
v1.5.41).

| Prefix | Source (Env Vars section) | Who defines the set | Resolves to |
|---|---|---|---|
| `CRONOMICON_VAR_*` | Variables | user | plaintext **value** (log-safe) |
| `CRONOMICON_SECRET_*` | Secrets (stored or Vault-backed — not distinguished) | user | sensitive **value** (always redacted) |
| `CRONOMICON_KEY_*` | SSH Keys | user | **file path** to key material on the executing host |
| `CRONOMICON_RUN_*` | dispatcher | Cronomicon (fixed set) | read-only run **context** (injected since v0.49.1) |

**The `CRONOMICON_RUN_*` fixed set** (injected into every run by the executor; not
bindable, never from a store row — a run's own metadata, always log-safe):
`CRONOMICON_RUN_ID` (run/trace id), `CRONOMICON_RUN_JOB` (job name), `CRONOMICON_RUN_JOB_SOURCE`
(`git`|`cronomicon`), `CRONOMICON_RUN_SCOPE` (`""` = global), `CRONOMICON_RUN_TYPE` (run type),
`CRONOMICON_RUN_TRIGGERED_BY` (actor), `CRONOMICON_RUN_EXECUTOR` (`ssh`|`runner`). The SSH
executor injects these today (P1.3); the runner executor follows (P1.4).

**Derived references (no renames, ever).** Rows keep their bare names
(`ansible_rh8_key`, `NWD_BECOME_PASS`); the reference is derived at use time by
prefixing — `reference = CRONOMICON_<SECTION>_<row name, verbatim>` — and resolution
strips the known prefix verbatim (case-preserved) to locate the row/file. Nothing
in the DB or on the runner hosts migrates; only reference _sites_ (scripts,
inventories, host key fields) move to the derived form, at the operator's pace.

**KEK eviction (the only config renames in this scheme).** The KEK is app config,
not a store secret, so it lives outside the reserved `CRONOMICON_SECRET_*` prefix
as `CRONOMICON_KEK`, `CRONOMICON_KEK_FILE`, `CRONOMICON_KEK_VERSION` and
`CRONOMICON_KEK_<N>` / `_<N>_FILE`.

### Rollout (incremental, no flag-day)

The reference half can be adopted at your own pace — the server resolves both
bare and derived references.

1. **Server KEK env.** Set `CRONOMICON_KEK*` (see **Stored-secret encryption**
   below).
2. **Reference sites (in your job-definitions repo + SSH Targets).** Move
   reference *uses* to the derived form at your pace — nothing server-side or on a
   runner host is renamed:
   - **Scripts** → `CRONOMICON_SECRET_<key>` / `CRONOMICON_VAR_<key>` (copy the exact
     reference from the Env Vars page).
   - **Inventories** → `lookup('env','CRONOMICON_KEY_<label>')` /
     `lookup('env','CRONOMICON_SECRET_<key>')`.
   - **Host / bastion `authKeyEnvVar` fields** → `CRONOMICON_KEY_<label>`.

   Each site is independent — verify one with a **doctor auth-chain probe** or a
   single **test run** before moving the next. Bare-name references keep resolving
   via the legacy fallback chain throughout the transition.
3. **Out of scope here:** the Vault Agent sidecar's rendered `secrets.env` names
   (`secrets.env.ctmpl`) — a separate effort (N-D5). The agent's prefixed→bare
   fallback lets an inventory move to `CRONOMICON_SECRET_<key>` even before the
   sidecar is touched.

Full contract: `20260720-namespace-update.md`.

## HTTP & logging

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_ADDR` | `:8080` | Listen address. Not published to the host (proxy-only). |
| `CRONOMICON_LOG_LEVEL` | `info` | `debug\|info\|warn\|error`. |
| `CRONOMICON_LOG_FORMAT` | `json` | `json\|text`. **rec** `json` in prod. |
| `CRONOMICON_LOG_FILE_ENABLED` | `true` | Also write the **process log** to a file on disk. **stdout is always written** — the file is strictly additive, never a replacement, so journald/docker log collection is unaffected. `false` turns the file off entirely (stdout only). |
| `CRONOMICON_LOG_FILE` | _(empty)_ | Absolute path of the process log. Empty ⇒ `cronomicon.log` in the **run-log directory** (Settings → Log Storage), and it follows that directory when the setting changes. A **relative path is rejected at boot**. Setting it explicitly also pins the file *and* attaches it **earlier in boot** (before the DB opens), so the build banner and config warnings land in the file too; the derived default attaches just after migrations. Worth pinning if the run-log tree lives on slow/shared storage — a continuously appended file maps badly onto object storage. |
| `CRONOMICON_LOG_FILE_MAX_MB` | `64` | Size at which the live file rotates. Must be **≥ 1**. |
| `CRONOMICON_LOG_FILE_KEEP` | `5` | Rotated generations retained, named `cronomicon.log.1` … `cronomicon.log.N` (`.1` = newest). Must be **≥ 0**; `0` truncates in place with no generations kept. |
| `CRONOMICON_AUDIT_LOG_ENABLED` | `true` | Write the **compliance audit stream** (`audit.log`) to disk. This is a *different* file from the process log: one JSON-Lines record per audited event, keys fixed by a versioned schema. `false` turns the file off entirely — audit rows are still written to the database, which is authoritative either way. |
| `CRONOMICON_AUDIT_LOG` | _(empty)_ | Absolute path of the audit stream. Empty ⇒ `audit.log` in the **run-log directory** (Settings → Log Storage), and it follows that directory when the setting changes. A **relative path is rejected at boot**. |
| `CRONOMICON_MAX_RUN_LOG_BYTES` | `536870912` | Cap on a single run's ingested log (512 MiB). The log-ingest endpoint is exempt from the 2 MiB body cap (runs stream many chunks); this bounds per-run growth so a rogue/compromised runner can't fill the disk. On reaching the cap, further ingest is refused with `413`. `0` disables the cap. |
| `CRONOMICON_OUTBOUND_ALLOW_PRIVATE` | `true` | SSRF egress guard (SU-7) on operator-configured outbound targets (Vault, GitLab, S3/MinIO, Apprise, OIDC). Cloud-metadata / loopback / link-local are **always** blocked. `true` (default) permits RFC-1918 / unique-local — required here because Vault/GitLab are internal hosts. Set `false` only when every outbound target is public. The S3 IAM-role provider (IMDS) is exempt. |
| `CRONOMICON_OUTBOUND_ALLOW_LOOPBACK` | `false` | Re-permit loopback outbound past the SSRF guard. Set `true` **only** for a Vault-agent loopback sidecar (Vault addr on `127.0.0.1`); it weakens the guard for every outbound client. |

**The process log.** Total disk cost is bounded by **`(KEEP + 1) × MAX_MB`** — 384 MiB
at the defaults. Rotated generations deliberately do **not** end in `.log`, so the
run-log reaper (`CRONOMICON_RETENTION_LOG_FILES_DAYS`, § Retention below) never sees
them, and the live `cronomicon.log` is explicitly excluded from that reaper as well —
removing it out from under the open handle would send every later line to an
unlinked inode. **Process-log retention is the keep count, not a day window.** A
file-side failure (unwritable path, full disk) is reported once per minute on stderr
and degrades to stdout-only; it never stops the process.

**The audit stream** carries the `change_log` spine, a named subset of `activity`
(`run-end`, `workflow-end`, `config`, `gitsync`, `push` — pure run telemetry such as
`run-start` is excluded), and every auth event. It rotates **daily by UTC date** to
`audit.log.YYYYMMDD`; the dated generations deliberately do not end in `.log`, so the
run-log reaper never sees them, and the live `audit.log` is excluded from that reaper
by name. Its lifetime is the `auditLogFiles` retention knob (§ Retention), not a keep
count. **The database is authoritative and the file is an export of it** — the row and
the line are not written in one transaction, so a crash between them can leave a row
with no line; a missing line is recoverable from the tables, the reverse would not be.
A write failure is reported (rate-limited) to the process log and never stops the server.

## Storage

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_DB_PATH` | `/var/lib/cronomicon/cronomicon.db` | SQLite file on the mounted volume. |
| `CRONOMICON_GIT_CACHE_DIR` | `/var/lib/cronomicon/git-cache/job-definitions` | GitLab clone cache. Keep on the volume so it survives restarts. |

## Auth — Trusted Header SSO (Phase A, primary)

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_AUTH_MODE` | `trusted-header` | `trusted-header\|oidc`. |
| `CRONOMICON_TRUSTED_PROXIES` | _(empty)_ | **req** in trusted-header mode — CIDR/IP allowlist of the reverse proxy. **Fail-closed: empty ⇒ refuse to boot** (unless `CRONOMICON_DEV_AUTH`). In the compose stack this is Traefik's static internal IP, e.g. `172.28.0.2/32`. |
| `CRONOMICON_TRUSTED_HEADER_USER` | `Remote-User` | Header-name override. |
| `CRONOMICON_TRUSTED_HEADER_EMAIL` | `Remote-Email` | Header-name override. |
| `CRONOMICON_TRUSTED_HEADER_NAME` | `Remote-Name` | Header-name override. |
| `CRONOMICON_TRUSTED_HEADER_GROUPS` | `Remote-Groups` | Header-name override (comma-separated groups). |
| `CRONOMICON_BOOTSTRAP_ADMIN_GROUP` | _(empty)_ | **First deploy only.** Any user in this group gets admin regardless of `ad_group_mappings`. Logs a loud warning while set. Remove + redeploy after seeding real mappings. |
| `CRONOMICON_LOGOUT_REDIRECT_URL` | _(empty)_ | **rec** Identity-provider logout endpoint the SPA navigates to on sign-out. |

**`CRONOMICON_TRUSTED_PROXIES` now has a second job: it also gates client-IP attribution
in the auth audit trail.** Each auth event records two addresses — `remote_addr` (the
immediate peer, i.e. the proxy on every request behind one) and `client_ip`, derived by
walking `X-Forwarded-For` **right-to-left** and discarding hops that fall inside this
allowlist. The walk only begins when the immediate peer is itself a trusted proxy; with
the list empty, or a peer outside it, the peer address is recorded verbatim and any
forwarding header it sent is ignored. Widening this list therefore widens who can
influence the recorded client address — keep it to the proxies you actually run.

## Auth — OIDC (Phase A, optional mode)

Only consulted when `CRONOMICON_AUTH_MODE=oidc`.

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_OIDC_ISSUER` | _(empty)_ | OIDC issuer URL. |
| `CRONOMICON_OIDC_CLIENT_ID` | _(empty)_ | |
| `CRONOMICON_OIDC_CLIENT_SECRET` | _(empty)_ | Provide via secret file/env, never committed. |
| `CRONOMICON_OIDC_REDIRECT_URL` | _(empty)_ | |
| `CRONOMICON_SESSION_HASH_KEY` | _(empty)_ | base64, 32+ bytes — signs the session cookie. **Required in oidc mode** for sessions to survive restart (ephemeral random key otherwise). Irrelevant in trusted-header mode. |
| `CRONOMICON_SESSION_BLOCK_KEY` | _(empty)_ | base64, 16/24/32 bytes — encrypts the session cookie. |

## Cookies / TLS

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_COOKIE_SECURE` | `true` | **req** `true` in prod (HTTPS via the proxy). |

## Developer bypass (NEVER in production)

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_DEV_AUTH` | `false` | **dev** One-click synthetic-admin login, no identity provider. Also exempts the trusted-proxies boot check. Leave unset in prod. |
| `CRONOMICON_DEV_SEED` | `false` | **dev** Seeds demo data into an empty DB. Leave unset in prod. |

## Integrations

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_GITLAB_BASE_URL` | _(empty)_ | GitLab repo URL for job-definition sync (B3). |
| `CRONOMICON_GITLAB_TOKEN` | _(empty)_ | Read token for private repos; unauthenticated clone if empty. Secret. |
| `CRONOMICON_GITLAB_WEBHOOK_SECRET` | _(empty)_ | Validates `X-Gitlab-Token` on the webhook. Secret. |
| `CRONOMICON_GITLAB_WRITE_BRANCH` | _(empty)_ | The GitOps branch used for **both** sync-read and publish-write (V1.1-10) — there is one branch, not a read/write pair. Empty ⇒ fall back to the DB-backed `gitlab_config.write_branch` (**Settings → GitLab**), then `main`. Resolved **fresh per operation**, so a DB-side change applies without a restart; setting it here pins the branch and the panel value is ignored. |
| `CRONOMICON_RUNNER_BOOTSTRAP_TOKEN` | _(empty)_ | Out-of-band bootstrap registration token (T9/A6.1). Multi-use, env-configured; unlike UI-minted tokens (single-use per install since v0.47.4) it is never consumed. Secret. |
| `CRONOMICON_AGENT_DIR` | `/usr/share/cronomicon/agents` | Directory of runner-agent binaries + `SHA256SUMS` served unauthenticated at `GET /agents/{filename}` (provisioning D1; the container image bakes them in). Missing dir ⇒ clean 404 with a build-it-yourself hint — bare-metal deploys can point this at their own build output. |

### Stale-runner reaper (R3 / D4)

`last_seen_at` is written on every runner long-poll and nothing else sweeps it, so a
crashed runner would otherwise stay `online` forever and its `running` runs hang
forever. A background sweep runs **once a minute** (not configurable) and applies the
two windows below in order: mark offline, reconcile that runner's orphaned runs to
`failure` / `queued_reason=runner_lost`, then deregister.

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_RUNNER_OFFLINE_AFTER` | `5m` | Go duration. An `online`/`draining` runner whose last heartbeat is older than this is marked **offline**. Agents poll once a minute (D7), so the default is ≈5 missed polls. Shortening it below the poll interval will flap runners offline between polls. |
| `CRONOMICON_RUNNER_DEREGISTER_AFTER` | `336h` (14 days) | Go duration. A runner that stays **offline** this long is fully removed — its tokens revoked and its row deleted (D4). "Offline since" is derived from `last_seen_at` (or `registered_at` when it never polled), so this window is measured from the last heartbeat, not from the moment it was marked offline. A host that comes back later must re-register. |

## SSH executor (opt-in — `execution-update.md` EX.6)

Off by default. When enabled, **this process holds SSH private keys and opens
outbound SSH to job targets** itself, rather than handing the work to a runner
agent — so enabling it changes the server's blast radius, and it logs a loud
warning at startup while on. Left off, `executor='ssh'` runs simply queue and are
never claimed.

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_SSH_EXECUTOR_ENABLED` | `false` | Master switch for the in-process SSH worker pool. `false` ⇒ the claim loop and the periodic orphan reaper never start and ssh runs stay queued. Note the **startup orphan sweep runs either way**: an `executor='ssh'` run left `running` by a crash holds a global concurrency slot until it is reconciled, so it is reconciled to `executor_lost` at boot whether or not the executor is enabled. |
| `CRONOMICON_SSH_EXECUTOR_CONCURRENCY` | `4` | Simultaneous ssh runs claimed by this process. `≤ 0` is treated as the default rather than "unbounded" or "none". Within a single run, fan-out across targets is separately bounded (EX-D5). |
| `CRONOMICON_SSH_EXECUTOR_STALE_AFTER` | `24h` | Go duration bounding the **periodic** orphan reaper (PP-H2), which runs once a minute while the executor is enabled: an `executor='ssh'` run still `running` longer than this is reconciled to failure (`executor_lost`). Keep it safely **above the longest plausible job** (its A12 timeout) — this window is a safety net, not the crash-recovery path, and a value below a real job's runtime will kill live runs. `≤ 0` falls back to the default. |

## Stored-secret encryption (S14)

> **Naming:** the KEK is **app config, not a store secret**, so as of v0.48.0 it
> lives under `CRONOMICON_KEK*` — out of the reserved `CRONOMICON_SECRET_*` reference
> prefix (see **Namespace** above).

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_KEK_FILE` | _(empty)_ | **rec** Mounted file holding the base64 KEK (preferred over env). **Back up SEPARATELY from the SQLite S3 backup** — losing it makes stored secrets unrecoverable. |
| `CRONOMICON_KEK` | _(empty)_ | Fallback: base64 KEK supplied directly. File takes precedence if both set. |
| `CRONOMICON_KEK_VERSION` | `1` | Active KEK version written into each newly created/updated secret. Bump when rotating (see below). |
| `CRONOMICON_KEK_<N>` / `CRONOMICON_KEK_<N>_FILE` | _(empty)_ | Historical KEK for version `N`, supplied during/after a rotation so secrets still wrapped under the old key remain decryptable. Same base64/file rules as the active KEK. |

**KEK rotation (zero-downtime):** to rotate from version 1 to 2 — (1) move the
current key to `CRONOMICON_KEK_1` (or `_1_FILE`), (2) set the new key in
`CRONOMICON_KEK`/`CRONOMICON_KEK_FILE` and `CRONOMICON_KEK_VERSION=2`,
(3) restart. Existing secrets keep decrypting via the v1 key and re-wrap to v2 the
next time each is saved; new secrets are wrapped with v2 immediately.

Rotation is **lazy** — a value moves only when it is rewritten — so step (4) is
to finish it with `cronomicon rewrap-secrets`, which covers all three encrypted
stores (`secrets`, `ssh_credentials`, and the `EncryptString` settings
integrations: GitLab token + webhook secret, S3 log-storage key, Vault
credentials, SMTP password, observability bearer token). Run it **inside the
deployment** so the container's mounted KEK is used and nobody handles key
material; it works against the live database and is idempotent.

```
docker exec <cronomicon> cronomicon rewrap-secrets --dry-run   # counts per store and version
docker exec <cronomicon> cronomicon rewrap-secrets             # move everything to the active version
```

Only once `--dry-run` reports rotation complete can the `_1` key be retired. The
old `SELECT DISTINCT kek_version FROM secrets` check covered one of the three
stores and would have declared victory with the other two still on the old key.

> Re-wrapping is hygiene, not remediation. If the old KEK was exposed, whoever
> held it also held the plaintexts — rotate the underlying credentials (SSH keys,
> tokens, AppRole) and let entering the replacements re-seal them.

**KEK file permissions.** A KEK file with any *other* permission bit (e.g. `0644`)
is **refused at startup** — the process will not boot until it is fixed; a
*group* bit (e.g. `0640`) logs a warning and continues. Use `0400`, owned by the
account the server runs as. An env-supplied `CRONOMICON_KEK` has no mode and is not
checked, but the file form is preferred for the reasons above.

## Retention

Retention is **per-table** and lives in the DB: seven independent day knobs (`runs`,
`activity`, `workflowRuns`, `changeLog`, `schedulePushes`, `logFiles`,
`auditLogFiles`) stored in the `auditCompliance` settings blob and edited under
**Settings → Audit & Compliance**. The vars below are **bootstrap defaults only** — on the first boot
that finds the blob unset they seed it (each var fanning out to the tables it
covered before), and from then on the blob is authoritative. Editing these on an
already-seeded deployment does nothing. The nightly sweep re-reads the blob, so a
panel change applies on the next sweep, not the next restart. `0` = keep forever.

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_RETENTION_RUNS_DAYS` | `90` | Seeds `runs`/`activity`/`workflowRuns` on **first run only**; the stored blob is authoritative thereafter. |
| `CRONOMICON_RETENTION_CHANGELOG_DAYS` | `365` | Seeds `changeLog`/`schedulePushes` on **first run only**; the stored blob is authoritative thereafter. |
| `CRONOMICON_RETENTION_LOG_FILES_DAYS` | `90` | Seeds `logFiles` — the **on-disk run-log** window — on first run only. **Operationally significant:** before this knob nothing ever deleted log files, so after upgrading, the first nightly sweep reaps run logs older than this window. Set `0` (here before first boot, or in the panel afterwards) to keep them forever. |

**`auditLogFiles` (default `730` days) has no env var — it is a settings knob only**,
edited under Settings → Audit & Compliance like the rest of the blob. It bounds the
on-disk **audit stream** (`audit.log` and its dated `audit.log.YYYYMMDD` generations,
§ HTTP & logging). Two years is deliberately longer than `changeLog`'s one, so the
exported stream outlives the rows it exports — that is the point of the file, and
shortening it below `changeLog` throws away the tamper-evident tail early. New
deployments and upgrades alike start at the default; there was no pre-upgrade
behaviour to preserve, so nothing seeds it from env. `0` = keep forever.

**`logFiles` governs per-run logs only.** The process log and the audit stream share
the same directory but neither is on a day window: both are excluded from this
reaper by name, and their lifetimes are `(KEEP + 1) × MAX_MB` and `auditLogFiles`
respectively (§ HTTP & logging). So setting `logFiles` to `0` does **not** make the
process log grow without limit, and shortening it prunes neither of them.

**The reaper walks the per-entity folders.** Since migration `710` a run log lives at
`{run-log dir}/{code}/{traceId}.log`, where `{code}` is an opaque 8-hex-character
code assigned to the owning job or workflow (`_system` for runs that belong to no
definition, today only the SSH connection test). The sweep is recursive and still
matches `*.log` only, so each folder's `_meta.json` sidecar survives the logs it
describes. Runs enqueued before that migration keep the flat
`{run-log dir}/{traceId}.log` path; nothing is moved, and the two layouts coexist
permanently. Deleting a job or workflow leaves its folder in place (the audit trail
outlives the definition) — the retention window is what keeps that growth bounded,
so `logFiles = 0` on a deployment that churns definitions means the tree only grows.

## Backups (nightly `VACUUM INTO` + S3 upload — `internal/backup`)

Leave the bucket empty to keep **local-only** snapshots on the volume.

One nightly sweep does both jobs in order: prune aged rows per the retention blob
above (including the on-disk log reapers), then `VACUUM INTO` a snapshot and upload
it. `CRONOMICON_BACKUP_AT` therefore also decides when retention runs.

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_BACKUP_AT` | `02:00` | Daily **wall-clock UTC** `HH:MM` for the retention + backup sweep (PP-H6). Always UTC — *not* the application timezone from Settings → General, which governs scheduling and display only. An unset or unparseable value falls back to `02:00` rather than failing to boot. Wall-clock anchoring (rather than a 24h-from-boot ticker) is deliberate: a process restarted more often than daily would never have reached the end of a 24h ticker, so a frequently-redeployed deployment could go indefinitely without a durable backup. A **boot catch-up** sweep also runs immediately when the last recorded success is more than 24h old, gated on that persisted timestamp so a crash-loop still backs up at most about once a day. |
| `CRONOMICON_BACKUP_S3_BUCKET` | _(empty)_ | Empty ⇒ local snapshots only (no upload). |
| `CRONOMICON_BACKUP_S3_ENDPOINT` | _(empty)_ | `host:port`; empty ⇒ AWS S3. Set for MinIO. |
| `CRONOMICON_BACKUP_S3_REGION` | `us-east-1` | |
| `CRONOMICON_BACKUP_S3_ACCESS_KEY` | _(empty)_ | Secret. |
| `CRONOMICON_BACKUP_S3_SECRET_KEY` | _(empty)_ | Secret. |
| `CRONOMICON_BACKUP_S3_USE_SSL` | `true` | |
| `CRONOMICON_BACKUP_S3_CA_FILE` | _(empty)_ | Optional PEM bundle for a backup bucket on a private S3 node signed by an internal CA (SL band). The log archive's equivalent is the pasted CA Bundle in Settings → Log Storage; both feed the same client. |

## Notifications (Phase C.1)

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_APPRISE_URL` | _(empty)_ | Apprise gateway base URL, e.g. `http://apprise:8000` (the Phase B sidecar). Used as the push gateway when an alert rule includes a push/apprise channel. The per-config gateway override (`apprise_url` in notification settings) takes precedence if set. |

> SMTP server settings (host/port/encryption/username/from/recipients) and the
> Apprise target URLs are operator-managed in **Settings → Notifications**, not
> env. The **SMTP password is stored envelope-encrypted with the KEK** (so a KEK
> is required to use authenticated SMTP — see `CRONOMICON_KEK_FILE`).

## Metrics (Phase C.2)

`/metrics` is always on (unauthenticated, like `/healthz`); no env needed. It is
served only on the internal network — no proxy route exposes it publicly.

## Vault (Phase C.3 / Phase 2) — built, disabled unless configured

The Vault client (AppRole, KV v2) stays a no-op stub unless `CRONOMICON_VAULT_ADDR`
**and** both role/secret IDs are present (env **or** DB `vault_config`); then
vault-source **secrets** and vault-source **SSH credentials** resolve live through
one shared client, and the SPA shows the vault path option. No Vault configured ⇒
behaves exactly as before (local-KEK only). Operator runbook:
the cron-ops repo's `vault-runbook.md`.

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_VAULT_ADDR` | _(empty)_ | Vault base URL, e.g. `https://vault.internal:8200`. Empty ⇒ Vault disabled. |
| `CRONOMICON_VAULT_ROLE_ID` | _(empty)_ | AppRole role id. |
| `CRONOMICON_VAULT_ROLE_ID_FILE` | _(empty)_ | Mounted file with the role id (preferred; overrides the inline value). |
| `CRONOMICON_VAULT_SECRET_ID` | _(empty)_ | AppRole secret id. Secret. |
| `CRONOMICON_VAULT_SECRET_ID_FILE` | _(empty)_ | Mounted file with the secret id (preferred). |

**Client hardening (Phase 2, D4) — each knob dormant until set; unset = prior behavior:**

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_VAULT_NAMESPACE` | _(empty; DB `vault_config.namespace`)_ | Sent as `X-Vault-Namespace` on every request (Vault Enterprise / HCP). Env overrides the DB value. |
| `CRONOMICON_VAULT_CA_FILE` | _(empty)_ | PEM CA bundle; pins the client transport instead of system roots. Missing/invalid ⇒ **fails loud** (Vault stays disabled), never a silent system-root fall-back. |
| `CRONOMICON_VAULT_SECRET_ID_WRAPPED` | `false` | When true, the secret id is a single-use response-wrapping token, unwrapped once via `sys/wrapping/unwrap` and cached. Static long-lived secret id is the default. |

Always-on (no config): transient failures (transport errors, `429`, `5xx`) are
retried with bounded exponential backoff; `4xx` is terminal. Token refresh is
lazy re-login at 80% of the AppRole lease (request-driven; no background goroutine).

**Run-injection kill-switch (D5):**

| Var | Default | Notes |
|---|---|---|
| `CRONOMICON_SECRETS_INJECTION_ENABLED` | `true` | Dispatch-time reference injection (bound `CRONOMICON_SECRET_/VAR_/KEY_*` resolved and injected into runs). Set false to hard-disable for a release. Plural `SECRETS_` sits outside the reserved `CRONOMICON_SECRET_*` reference prefix, so it is config, not an injected value. |

---

## Env-vs-DB precedence (Settings endpoints)

Several settings are DB-backed and operator-editable in the UI
(`/settings/{gitlab,vault,log-storage,observability}`, plus the retention and
timezone blobs). The precedence rule is applied uniformly (endpoint-update E.3):
**env, when set, overrides DB**; the DB value is the operator-editable default.

**When a change applies** is per-setting, not uniform — the "When applied" column
below is the answer, and the PUT handler logs a restart warning only for the ones
that genuinely need one. The connection-shaped settings (GitLab repo/PAT, Vault
addr/AppRole) are resolved once at startup, because the client built from them is
constructed at boot. The rest are re-read closer to use.

| Setting | Env override | DB source | When applied |
|---|---|---|---|
| GitLab repo URL / PAT | `CRONOMICON_GITLAB_BASE_URL` / `CRONOMICON_GITLAB_TOKEN` | `gitlab_config` | restart |
| GitLab webhook secret | `CRONOMICON_GITLAB_WEBHOOK_SECRET` (pins validation; **rotation API returns 409 while set**) | `gitlab_config` (+ rotation overlap pair) | restart |
| GitLab write branch | `CRONOMICON_GITLAB_WRITE_BRANCH` | `gitlab_config.write_branch` | next sync/publish (read per operation) |
| Vault addr / AppRole | `CRONOMICON_VAULT_ADDR` / `CRONOMICON_VAULT_ROLE_ID[_FILE]` / `CRONOMICON_VAULT_SECRET_ID[_FILE]` | `vault_config` | restart |
| Vault namespace | `CRONOMICON_VAULT_NAMESPACE` | `vault_config.namespace` | restart |
| Run-log dir | — (no env) | `log_storage_config.local_path` | **immediately** (LU-5) — pushed to every writer on save; in-flight runs finish in the old directory and existing files are not moved |
| Retention windows | seeded from `CRONOMICON_RETENTION_*` on first boot only | `auditCompliance` settings blob | next nightly sweep |
| Application timezone | `TZ` (fallback only) | settings KV `timezone` | **immediately** — the cron engine is rebuilt in the new zone |
| Metrics enabled/auth | — (no env) | settings KV `obs.*` | enabled/bearer checked **live per scrape**; path change needs restart |

## Timezone (scheduling + display)

The **application timezone** (Settings → General, stored in settings KV
`timezone`) is the source of truth: the cron scheduler fires in it **and** the
SPA renders every absolute timestamp in it. Editing the setting reschedules every
job/workflow live (the cron engine is rebuilt in the new zone). Stored instants
stay UTC RFC3339 — only the evaluation/display zones change, so there is no
migration.

| Var | Default | Notes |
|---|---|---|
| `TZ` | _(host default)_ | **Fallback only.** Used (via Go's `time.Local`) when the `timezone` setting is unset or unloadable. Once an operator picks a zone in the UI, `TZ` no longer governs scheduling/display. |

- The binary **embeds the IANA zone database** (`time/tzdata`), so
  `America/...`-style zones resolve even in a slim image with no
  `/usr/share/zoneinfo` — no image `tzdata` package required.
- An invalid `timezone` is rejected at save with **422**; a bad value that
  somehow reaches load time degrades to `TZ`/`time.Local` with a logged warning
  (it never wedges the scheduler).
- The resolved effective zone is logged once at startup
  (`scheduler timezone resolved zone=...`).

## Build-time (Dockerfile args, not runtime env)

| Arg | Default | Notes |
|---|---|---|
| `VERSION` | `dev` | Stamped into the binary → `/version`, `/healthz`. |
| `COMMIT` | `none` | |
| `BUILD_DATE` | `unknown` | |

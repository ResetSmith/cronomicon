# Security review

Each control, how it's enforced, and how it's verified. "Automated" = a Go test
that fails CI if the control regresses. "Operator" = run against the live stack
(the spoofed-header probe in the administrator manual, section 8.4, or manual).

| # | Control | Enforced by | Verification |
|---|---|---|---|
| D.1.1 | **Trusted-proxy enforcement** — Remote-* honored only from an allowlisted peer; spoofed headers from elsewhere are stripped ⇒ unauthenticated | `auth.StripUntrustedHeaders` (global middleware) + `CRONOMICON_TRUSTED_PROXIES`; default-deny boot if unset | **Automated:** `TestSpoofedHeaderRejectedAtServer`, `auth.TestTrustedProxyStripsSpoofedHeaders`. **Operator:** `verify-deployment.sh` with `APP_DIRECT_URL`. |
| D.1.2 | **CSRF** double-submit on state-changing operator routes; runner bearer routes excluded | `auth.RequireCSRF` on POST/PUT/PATCH/DELETE; runner routes use `RequireRunner` (no CSRF) | **Automated:** `auth.TestRequireCSRF`, `auth.TestRequireRunner`. |
| D.1.3 | **Cookies** — CSRF cookie attrs; `CRONOMICON_COOKIE_SECURE=true` behind TLS; (OIDC mode) session cookie HttpOnly/Secure/SameSite | `auth.issueCSRF`, `session.go` codec; `CRONOMICON_COOKIE_SECURE` | **Automated:** session round-trip tests. **Operator:** inspect `Set-Cookie` on a live response. |
| D.1.4 | **Secrets at rest** — stored-secret values + SMTP password envelope-encrypted (AES-256-GCM) under a KEK; KEK mounted, backed up separately from the DB | `secrets` envelope scheme; `CRONOMICON_KEK_FILE` | **Automated:** `secrets` round-trip + `TestEncryptDecryptStringRoundTrip`. **Operator:** confirm KEK not in image/repo/S3 backup bucket. |
| D.1.5 | **Dev bypass off** — `/api/v1/auth/dev-login` not mounted unless `CRONOMICON_DEV_AUTH` | route mounted conditionally in `auth_mount.go` | **Automated:** `TestDevLoginMountedOnlyWhenEnabled`. **Operator:** `verify-deployment.sh` (expects 404). |
| D.1.6 | **Surface check** — only `/healthz`, `/readyz`, `/version`, `/metrics`, `/auth/providers` (+ OIDC `/login`,`/callback` in oidc mode; token-gated `/webhooks/gitlab`) are unauthenticated | per-route middleware in the mount files | **Automated:** `TestUnauthenticatedSurface`. |
| D.1.7 | **`/metrics` not public** | no Traefik label routes `/metrics`; served on the internal network only | **Operator:** `verify-deployment.sh` warns if `/metrics` answers via the public URL. |
| D.1.8 | **Bootstrap admin removed** — `CRONOMICON_BOOTSTRAP_ADMIN_GROUP` unset after seeding access grants | env; loud warning logged while active | **Operator:** confirm the var is unset and the warning no longer logs (see runbook for the re-enable procedure). |

## Running the automated security suite

```sh
cd backend
go test ./internal/api/ ./internal/auth/ ./internal/secrets/ -run \
  'Spoofed|Surface|DevLogin|CSRF|Runner|Trusted|EncryptDecrypt|Session' -v
```

## Residual risks / notes

- **Trusted-proxy IP must be exact.** The compose stack pins Traefik to a static
  internal IP and trusts only that `/32`. If the proxy IP changes, update
  `CRONOMICON_TRUSTED_PROXIES` — a too-wide CIDR weakens D.1.1.
- **Header stripping happens in-app too.** Even though Traefik strips client
  `Remote-*` at the edge (defense in depth), the app independently strips from any
  untrusted peer — both layers must hold.
- **KEK loss = unrecoverable stored secrets.** Back it up separately from the S3
  DB backup; a single bucket compromise must not yield both.
- **Token-in-URL install endpoint** (`GET /install/{token}`). Serves `runner-install.sh` with the server URL + token
  baked in for a one-click `curl … | sudo bash`. A DUMB endpoint: it does not
  read the DB, so it is not a token-validity oracle — a used/expired token still
  gets a script, which then fails cleanly at registration (the sole enforcement
  point: single-use, atomic, audited). Only the token SYNTAX is checked
  (`crn_reg_` + 64 hex), which also makes shell-meta injection into the baked
  assignment impossible; the reconstructed server URL is constrained to
  URL-safe chars for the same reason. The one exposure is that tokens
  appear in request **URLs → access logs**: treat reverse-proxy logs
  accordingly (the token is single-use + 24h-expiry, so a logged token's window
  is already bounded, and its exposure is equivalent to the token leaking —
  which the single-use design already contains). Response is `Cache-Control:
  no-store`. Same unauthenticated-at-root posture as `/agents/*` and
  `/runner-install.sh` below. **Proxy note:** behind a browser-SSO forward-auth
  (Authelia/Traefik), this path — like the runner bearer-authed API
  (`/api/v1/runners/register|{id}/poll|{id}/redeclare|{id}/hostkeys`,
  `/api/v1/runs/{id}/manifest|log`), `/agents/*`, and `/runner-install.sh` —
  must be on the proxy's auth-bypass allowlist, or a runner (no SSO session)
  gets a 302-to-login HTML page instead of the script. The app still enforces
  the runner bearer on the `/api` paths, so the bypass skips only the SSO.
  The path list is in the administrator manual, section 8.3.
- **Host-key trust is human-approved TOFU, never automatic.** A runner
  refuses an unknown/changed target key (no fall-open). The scan → approve →
  trust flow lets an
  operator trust a key without hand-assembling `known_hosts`: the agent scans
  the host from its own vantage and uploads the presented key; the operator
  approves it (the UI shows the full SHA256 for out-of-band comparison); the
  next poll delivers a `trust-hosts` op and the agent appends it. The trust
  decision is ALWAYS a human approval — the server never auto-trusts a scanned
  key, and the scan carries no credential (it captures only the public host
  key). Approving without out-of-band verification is still TOFU, and the docs
  say so plainly (approval dialog + security guide §8). Every registered agent
  understands the keyscan/trust-hosts ops (the server's protocol floor tracks
  the current protocol version, so an older agent is refused at registration);
  the upload endpoint is runner-key-authed + ownership-guarded; scan requests and
  approve/reject decisions are audited. The scanned key never bypasses
  verification — it only becomes a candidate for a human to approve.
- **Server manages a runner's operational settings — but never its secrets.**
  The operator can push
  `maxConcurrent`, the sandbox caps, the checkout policy (`allowCheckout` +
  `checkoutRepos` allowlist), and a subtract-only capability mask to a runner
  from the UI; they ride the poll channel and apply in-memory. This follows the
  server→runner authority direction resync already has: the
  server decides *which jobs* a runner receives, so naming *which repos*
  it may clone is not a materially larger grant — and the credentials that make
  checkout dangerous (the deploy token, the vault password) remain **runner-local
  custody** (installed via the installer's file / stdin flags, below), never server-managed,
  so a compromised server still cannot exfiltrate or inject secret bytes. The
  capability mask is subtract-only (it can only *narrow* a runner's claimed set,
  never widen it) and is enforced server-side at claim. Managed settings are
  excluded from the config digest, so they cannot be used to force a
  re-registration loop. An operator without ConfigureApp cannot reach the PATCH
  endpoint (session → CSRF → perm gate, activity-audited). A defense-in-depth
  alternative, the agent intersecting the server allowlist with a runner-local
  one, is deferred; it can be added without a protocol change if a review
  insists.
- **Runner secrets are never accepted as installer flag VALUES.**
  `runner-install.sh` can place the checkout deploy
  token and the Ansible vault password, but only via a file path
  (`--checkout-token-file` / `--vault-pass-file`) or a hidden stdin prompt
  (`--checkout-token -` / `--vault-pass -`, echo off) — passing the secret as a
  flag value is explicitly refused, because it would leak via `ps(1)` and shell
  history. The installed files get `0640 root:cronomicon-runner` (same custody as
  `runner.env`); the bytes never transit the Cronomicon server. A stdin prompt requires a TTY, so a piped
  `curl … | sudo bash` install (script on stdin) is rejected with a pointer to
  the file flag rather than silently reading the wrong stream.
- **Unauthenticated agent/installer serving is by design.** `GET /agents/{filename}` (allowlisted: the two linux
  `cronomicon-runner` binaries + `SHA256SUMS`, from `CRONOMICON_AGENT_DIR`) and
  `/runner-install.sh` are served without auth: neither artifact is a secret
  (both are buildable from source), the install flow runs before any credential
  exists on the host, and registration itself still requires a token — serving
  these grants no access. The handler serves only exact allowlisted names (no
  directory listing / traversal), and `runner-install.sh --download` verifies
  the binary against `SHA256SUMS` fetched over the same TLS channel (integrity
  against corruption; TLS provides the authenticity — the panel's one-liner
  itself is the curl-pipe trust decision, with a download-inspect-run variant
  offered for shops that reject it).
- **Resync cannot reconfigure a runner from the server side** (automatic
  drift detection: the agent polls with a canonical digest of its declared config and the
  server requests a re-declare on mismatch; the digest is a hash of what the
  agent already declares, never a channel for new config, and delivery is
  flap-guarded so a disagreeing agent cannot be driven into a re-register
  loop). The `re-register` poll control op carries no
  configuration: the agent re-reads its OWN local config and re-declares it via
  `POST /runners/{id}/redeclare`, authenticated with its existing `crn_run_*`
  key. A compromised server (or operator session) therefore cannot use resync
  to grant itself capabilities on a runner — the declared set is always derived
  from the runner host's local files. Redeclare is ownership-guarded (a runner
  key can only redeclare its own row, 404 otherwise, mirroring the poll guard)
  and never touches key material or agency membership; registration tokens stay
  first-contact-only credentials (single-use per install: each
  minted token is consumed atomically by its first successful registration —
  two hosts cannot register from one token — and its row records which runner
  consumed it, an audit trail the shared token could not give; a leaked unused
  token is revocable and expires in 24h, and a leaked used token is worthless).
  The runner→server direction is the accepted
  tradeoff: a compromised `crn_run_*` key can re-declare its OWN row
  (widen advertised capabilities, rename, flip inventory mode) without operator
  action. It still cannot
  change agency membership (operator-assigned) or touch other rows, and claim
  eligibility stays bounded by its agencies; the remedy for a compromised key
  is Deregister, which revokes it immediately.
- **Capability auto-detection broadens by default.** With `CRONOMICON_RUNNER_CAPABILITIES` unset, the agent probes the
  host's PATH at startup (`bash`, `perl`, `pwsh`, `python3`/`python`,
  `ansible-playbook`, `terraform`) and claims every run-type it finds — so
  installing a toolchain on a runner host silently widens what that runner will
  execute after its next restart (surfaced in the registry via the drift
  resync, but not gated on approval). This is deliberate: the declared set is
  still derived exclusively from the runner host's local state (never from the
  server), and job routing remains bounded by agencies and scopes. The
  narrowing lever is the explicit override: set `-capabilities` /
  `CRONOMICON_RUNNER_CAPABILITIES` on hosts that carry toolchains they must not
  execute for Cronomicon. The server-side subtract-only capability mask (above)
  is the operator-controlled narrowing lever that survives host changes.
- **Of the two durable log artifacts, `cronomicon.log` is NOT redacted and
  `audit.log` IS.** They are assessed together here because they share the
  property that makes either one a risk — **durability**: each is a persistent
  on-disk file beside the run logs, so its contents land in backups and host
  snapshots of the data volume, rather than expiring on whatever schedule
  journald/docker was configured with.

    - **The process log has no redactor.** `internal/logsink` is a plain tee, so
      whatever a caller puts in a log line is written verbatim. The most
      plausible leak is not a deliberate log of a secret but a credential inside
      an *error string* a library formatted from a URL, a DSN, or an auth header.
      A masking writer for this file over the process-wide dictionary (the one
      the audit stream uses) is a bounded piece of work and is deferred; until
      it lands the exposure stands as written.
    - **The audit stream is masked by the process-wide dictionary**
      (`internal/redactdict`, installed at boot by `redactdict.Install`): every
      stored secret, stored SSH credential, encrypted settings column and
      multi-line env_var the server can decrypt — regardless of scope — applied
      to each caller-supplied text column (`target`, `summary`, `details`,
      `reason`) on all three writers, BEFORE the row is stored, so the row, the
      streamed line and the CSV export carry the same text. The dictionary
      rebuilds after every write to those sources (`secrets.RedactionSourceChanged`,
      pinned by a source scan) with a 5-minute TTL backstop. It **fails open in
      exactly one shape**: whatever was last built — complete, partial, or the
      previous good build — is always applied; only a process in which no build
      has yet produced anything writes unmasked. A degraded build (a KEK version
      not configured, undecryptable rows) is itself audited, once per outage, as
      `system / Audit / redactor-unavailable`. **Residuals:** Vault-sourced
      values (dispatch-time only, never in a global set) and the dictionary's
      deliberate edges (values under 5 chars or equal to a common literal are
      never added).

  Mitigations that apply to both: each file is created `0640` in a `0750`
  directory — the same custody as the run logs beside them, so no new reader gains
  access — and each has an off switch that costs nothing else.
  `CRONOMICON_LOG_FILE_ENABLED=false` leaves stdout exactly as it was;
  `CRONOMICON_AUDIT_LOG_ENABLED=false` costs the shipper-friendly export, not the
  audit trail, because **the database is authoritative and the file is an export of
  it**. Deployments that treat the data volume as a lower-trust artifact than their
  log collector should set both to `false`.

  Growth is bounded, but only one of the two bounds is short. The process log
  cannot become an unbounded credential archive: it is capped at
  `(CRONOMICON_LOG_FILE_KEEP + 1) × CRONOMICON_LOG_FILE_MAX_MB`, 384 MiB at the defaults.
  The audit stream bounds each *record* rather than the tail — `details` is
  length-bounded on all three paths (`auditlog.TrimForAudit`, 4096 bytes), the
  activity `summary` alongside it, and the caller-controlled `User-Agent` is capped
  at 512 bytes (`httpx.UserAgent`), so one hostile value cannot decide how large a
  record is — while its **retention window
  is deliberately long** (`auditLogFiles`, 730 days). Anything that does leak into
  `details` therefore persists for two years by default.
- **Client-IP attribution cannot be poisoned by a forwarded header** — *a
  control that exists, recorded here so a reviewer does not have to rediscover
  it.* Auth events record two addresses: `remote_addr` (the
  immediate peer verbatim) and `client_ip`. The derivation of `client_ip`
  (`httpx.ClientIP`) walks `X-Forwarded-For` **right-to-left**, discarding hops
  that fall inside `CRONOMICON_TRUSTED_PROXIES` and stopping at the first address
  that is not ours — and the walk **only begins when the immediate peer is itself
  a trusted proxy**. Taking `XFF[0]`, which is the common implementation, would
  record an entirely attacker-chosen value into the audit trail, which is strictly
  worse than recording nothing: it invites an investigator to trust an address the
  attacker wrote. A malformed entry ends the walk rather than being skipped, so an
  attacker cannot hide a hop behind garbage and shift which entry is believed.
  `CRONOMICON_TRUSTED_PROXIES` therefore carries a second job beyond the
  `Remote-*` identity headers, and a too-wide CIDR weakens both.
  **Residual:** with **no** trusted proxies configured (or a peer outside the
  list), there is nothing to strip and the peer address is used **verbatim** — so
  a deployment fronted by an untrusted or unlisted proxy records the proxy on
  every auth event, and the real client address is not recovered from the
  forwarding header. That is the intended fail-safe (record what we can prove, not
  what we were told), but it means an unset `CRONOMICON_TRUSTED_PROXIES` yields an
  auth trail with no client attribution rather than a wrong one.
- **Git-sourced job/workflow names are entirely unvalidated.** The GitLab sync inserts the YAML's `metadata.name` **verbatim** —
  no regex, no allowlist, no length limit, not even a trim — so a definition
  named `../../etc/cron.d/x` is reachable by anyone with merge rights on the
  synced repo, and an empty name falls back to a value that resolves to `.`.
  Run-log folders are therefore named by an **opaque server-assigned code**
  (8 hex characters from the `entity_codes` registry, `_system` for runs owned
  by no definition) rather than by the entity itself, and both `runner.LogPath`
  and `runner.EnsureLogDir` reject any folder name that fails
  `entitycode.Valid` — the same posture as `runner.ValidTraceID`. That is a
  deliberate choice **not** to make log storage the enforcement point for a
  problem it does not own: a path built from the code cannot traverse,
  regardless of what the repo names things. **Residual:** the names themselves
  are still unchecked everywhere else — they flow into the DB, out through the
  API and into the UI as-is. Nothing above depends on that being fixed, but it
  deserves its own validation pass (a syntactic allowlist + length bound at the
  sync boundary, applied to both sources).

## Output-marker secret leak guard

| # | Control | Enforced by | Verification |
|---|---|---|---|
| SU-1 | **Both executors fail closed on an `::cronomicon-output::` value that leaks an injected secret** — an `echo "::cronomicon-output name=X::$CRONOMICON_SECRET_*"` idiom drops the outputs and fails the run (`output_secret_leak`) rather than persisting the secret into `outputs_json` / a child step's `env_json` / the runs API. The SSH executor and the runner ingest path share one check | shared `execspec.FirstOutputLeakingSecret`; `sshexec.execute` guard + `finalizeReason`; `runner` ingest | **Automated:** `sshexec.TestSSHExecutorRefusesOutputLeakingSecret`, `execspec.TestFirstOutputLeakingSecret`, `runner.TestIngestRefusesOutputLeakingSecret`. |

## Scope-filtered reads

| # | Control | Enforced by | Verification |
|---|---|---|---|
| SU-2 | **Job/schedule read endpoints are scope-filtered** — a restricted actor cannot read an out-of-scope job's detail (script body + plaintext env) or list out-of-scope jobs / workflow-runs / schedules. Out-of-scope DETAIL returns **404** (no existence oracle); lists filter rows | `getJob` + `pauseJob`/`resumeJob` gate (they return the same detail) + `scopeWhereFragment` (listJobs, listWorkflowRuns); `drainScheduleRows` owner→`jobs.scope` resolution + `scheduleOwnerReadable` (listSchedules, listUpcomingSchedules) | **Automated:** `api.TestIDORScopeGates` (getJob/pause-resume/listJobs/listWorkflowRuns/listSchedules subtests). |

Accepted notes:

- **Out-of-scope job/schedule DETAIL returns 404, while run/log/binding detail
  (`getRun`, `getWorkflowRun`, `getJobBindings`) returns 403.** This asymmetry is
  deliberate: job/schedule detail follows the no-existence-oracle convention
  `putJobBindings` uses, while the run-path guards keep the plain 403.
- **First-class schedule-defs (`GET /api/v1/schedule-defs`, the `schedules` table)
  are intentionally NOT scope-filtered.** They are standalone git-authored catalog
  entries with no owning scope — the same unscoped-by-design posture as the scripts
  catalog. Only the per-owner `definition_schedules` projections (which carry a
  scoped job owner's plaintext env) are filtered.
- **`workflow_runs.scope` is filtered for consistency but is NULL for engine-created
  rows** (the engine INSERT omits it), so the real restriction there is latent
  until the column is populated; NULL (global) rows stay visible to everyone.

## Per-run log-ingest byte cap

| # | Control | Enforced by | Verification |
|---|---|---|---|
| SU-9 | **Per-run log-ingest byte cap** — the log-ingest endpoint (exempt from the 2 MiB body cap) is bounded by `CRONOMICON_MAX_RUN_LOG_BYTES` (default 512 MiB); a rogue runner streaming past the cap gets `413` and nothing further is persisted | `runner.HandleIngestLog` ceiling check + `MaxRunLogBytes` config | **Automated:** `runner.TestIngestCapsRunLogSize`. |

Accepted note: **the cap returns 413 without finalizing the run**, so a capped run
stays `running` until the orphan reaper reconciles it — an accepted tradeoff for
a LOW-severity disk-fill guard with a 512 MiB default.

## Session key validation

| # | Control | Enforced by | Verification |
|---|---|---|---|
| SU-6 | **Session hash-key length validated; raw-key fallback disabled in prod** — a `<32`-byte `CRONOMICON_SESSION_HASH_KEY` is substituted with a random key (ephemeral, warned) instead of weakening HMAC; a non-base64 value is rejected in a production auth mode rather than used as raw bytes | `auth.newSessionCodec` (`len<32`); `auth.decodeKey(allowRaw=cfg.DevAuth)` | **Automated:** `auth.TestNewSessionCodecSubstitutesWeakKeys`, `auth.TestDecodeKeyRejectsRawInProd`. |

## Credentialed outbound clients

| # | Control | Enforced by | Verification |
|---|---|---|---|
| SU-8 | **Credentialed outbound clients refuse redirects** — the Vault (`X-Vault-Token`) and GitLab REST (`Private-Token`) clients set `CheckRedirect = ErrUseLastResponse`, since Go's stdlib does NOT strip custom headers on a cross-host redirect. The guarded go-git client refuses redirects too | `secrets/vault_http.go`, `settings/gitlab.go`, `gitlab/sync.go` | **Automated:** `secrets.TestVaultClientDoesNotFollowRedirectWithToken`, `settings.TestRotateWebhookDoesNotFollowRedirectWithPAT`. |
| SU-7 | **SSRF guard on operator-configured outbound targets** — cloud-metadata/loopback/link-local blocked (RFC-1918 allowed by default); the resolved IP is checked and dialed directly (closes DNS-rebind). Wired into all 7 server clients + Apprise; the S3 IAM-role/IMDS provider is exempt | `httpx.GuardedDialContext`/`SafeTransport`; wired in `secrets/vault_http.go`, `settings/vault.go`, `settings/gitlab.go`, `gitlab/sync.go` (go-git `InstallProtocol`), `backup/s3.go`, `auth/service.go`+`handlers.go`, `notify/notify.go` | **Automated:** `httpx.TestEgressBlocked`, `httpx.TestGuardedDialContextRefusesMetadata`, `settings.TestRotateWebhookRefusesMetadataTarget`. |

Accepted notes:

- **RFC-1918 is allowed by default** (`CRONOMICON_OUTBOUND_ALLOW_PRIVATE=true`) —
  required because Vault/GitLab are internal hosts. The residual SSRF surface is
  therefore reach to other internal RFC-1918 services from an admin-controlled
  target URL; the admin-config write is already the trust boundary. `false` blocks
  private ranges.
- **Loopback is blocked by default**; a Vault-agent loopback sidecar needs
  `CRONOMICON_OUTBOUND_ALLOW_LOOPBACK=true`, which weakens the guard for every client.
- **Because go-git's guarded client refuses redirects**, a legitimate http→https
  redirect fails; configure GitLab as `https://` directly.

## Checkout deploy-token custody on the runner

| # | Control | Enforced by | Verification |
|---|---|---|---|
| SU-3 | **Checkout deploy token kept out of git argv** — the credential is delivered via `GIT_ASKPASS` (password from the helper's env, not argv → `/proc/cmdline`/auditd); only the non-secret username rides in the URL. Git stderr is scrubbed of `://user:pass@` (the runner-local token is not in the server redactor) | `agent.ensureMirror` (askpass helper + username-only URL); `agent.scrubURLCreds` on `runGit`/`gitArchiveInto` | **Automated:** `agent.TestEnsureMirrorKeepsTokenOutOfArgv`, `agent.TestScrubURLCreds`, `agent.TestSplitCheckoutCredential`. |

Accepted note: **the `GIT_CONFIG_*` env-var alternative is NOT used** (it needs
git ≥ 2.31); `GIT_ASKPASS` is version-agnostic for RHEL8 runner hosts.

## Bastion host-key pinning

| # | Control | Enforced by | Verification |
|---|---|---|---|
| SU-4 | **Bastion SSH host key pinned & verified** — the jump hop strict-compares a pinned `bastions.host_key` (mismatch → abort), else TOFU-captures the first-seen key (mirrors the target hop). A secret-injecting run over a bastion to an *unpinned target* is refused | `sshexec.bastionHostKeyCallback` (conn.go, probe.go); the secret-injection guard in `sshexec.execute`; migration `640` | **Automated:** `sshexec.TestBastionHostKeyCallback`, `TestSSHExecutorRefusesSecretsOverUnpinnedBastion`, `db.TestMigrate640RoundTrip`. |

Accepted note: **bastion pinning is TOFU, matching the target host-key path** —
the *first* connect trusts whatever key is presented (a first-connect MITM could
seed a malicious key, the same residual targets already carry). The
secret-injection guard closes the worst case (no secrets flow to an unpinned
target behind a bastion). A human scan/approve affordance is deliberately NOT
offered for bastions — it would be a net-new UI inconsistent with how in-app
target keys are already handled silently.

## Session revocation

| # | Control | Enforced by | Verification |
|---|---|---|---|
| SU-5 | **Server-side session revocation** — OIDC sessions carry an epoch stamped at login; an RBAC change bumps a global counter, rejecting pre-change sessions on next request. The acting admin keeps their session via a same-request cookie re-issue. Session TTL 8h | `auth.Service` epoch (`readSession`/`RevokeOtherSessions`); bumped by every RBAC-mutating handler in `access_mount.go`, `access_grants_mount.go`, `agencies_mount.go` and the scope rename in `settings_mount.go`; migration `641` | **Automated:** `auth.TestSessionEpochRevocation`, `db.TestMigrate641RoundTrip`, the `access_mount` integration flow. |

Accepted notes:

- **The session epoch is GLOBAL, not per-user** — an RBAC change revokes *all* OIDC
  sessions (over-revokes), not just the affected user's. Per-user revocation isn't
  cleanly possible (server-side we don't know which users map to a changed role/group
  without their tokens). The bump fires on every RBAC-mutating handler: role
  create / update / delete, access-grant create / update / delete, agency
  membership changes, and a scope **rename** (which cascades into every session's
  frozen scope set, closing the name-reuse vector). Assumes a single server
  instance (SQLite); a multi-instance deploy would need the epoch read from the DB
  per request rather than the in-memory mirror.
- **The acting admin's own session is preserved via cookie re-issue** (they aren't
  logged out of their own session). Accepted tradeoff: an admin who de-privileges
  *themselves* keeps their prior grants until the 8h TTL or a manual re-login — only
  *other* users are revoked immediately.

## Key zeroization

| # | Control | Enforced by | Verification |
|---|---|---|---|
| SU-10 | **Best-effort key zeroization** — the unwrapped DEK and loaded KEK are wiped on return from each envelope op | `secrets.zero`; `defer zero(...)` in `kek.go`/`seal.go`/`blob.go` | **Automated:** existing `secrets` round-trip suite. |

Accepted note: **zeroization is best-effort** — Go offers no guaranteed secure
erase (GC copies, heap growth); the wipes narrow, not eliminate, the exposure
window. No `mlock`.

## Audit-stream masking

The compliance audit stream (`audit.log` and the `change_log` / `activity` /
`auth_events` tables it exports) is masked by the process-wide redaction
dictionary. The process log is not (see the durable-log note above).

| # | Control | Enforced by | Verification |
|---|---|---|---|
| AM-1 | **Mask before trim** — the masker runs before the 4096-byte length bound on every audit writer, so a secret straddling the cut cannot survive as a prefix | `auditlog.WriteChangeLogAt` / `WriteActivity` / `WriteAuthEvent` | **Automated:** `auditlog.TestMaskRunsBeforeTrim`. |
| AM-2 | **Every caller-supplied text column is masked** — `target` on all three writers and `reason` on auth events, not only `details`/`summary`; before the INSERT, so row, stream line and CSV export agree | same writers | **Automated:** `auditlog.TestRedactorMasksTheDatabaseRowAndNotOnlyTheStream` (eight columns). |
| AM-3 | **One writer per audit table** — no raw `INSERT INTO activity/change_log/auth_events` outside `internal/auditlog` (including the break-glass `grant-admin` CLI) | source scan | **Automated:** `auditlog.TestAuditWriterConformance_OnlyThisPackageInserts`. |
| AM-4 | **Process-wide redaction dictionary** — stored secrets + SSH credentials + the seven encrypted settings columns (ONE table, `secrets.EncryptedSettingsColumns`, shared with `rewrap-secrets`) + multi-line env_vars, every scope; rebuilt lazily after each source write, 5-minute TTL backstop, last-good kept on a failed rebuild, partial-with-error on undecryptable rows | `internal/redactdict`; `secrets.RedactionReport`; `secrets.RedactionSourceChanged` at every source write site | **Automated:** `redactdict.TestBuildUnionsEverySourceAndKeepsVariablesVisible`, `TestBuildReportsUndecryptableAsPartial`, `TestStoreLifecycle`, `TestStoreConcurrentReadersNeverBlockOnRebuild` (`-race`), `TestEveryRedactionSourceWriterNotifies`, `secrets.TestRedactionReportCoversEverySettingsColumn`. |
| AM-5 | **Installed at boot, degraded builds audited** — `redactdict.Install` after migrate and before the first audit write; the healthy→degraded transition writes one `system / Audit / redactor-unavailable` row (+ WARN), recovery one `redactor-restored` row; unconditional, no knob | `cmd/cronomicon/main.go`; `redactdict.Install` | **Automated:** `redactdict.TestInstallMasksAuditRowsEndToEnd`, `TestInstallReportsAnOutageOnceAndItsRecoveryOnce`. |

Accepted notes:

- **Vault-sourced values are not in the global dictionary** — they exist only at
  dispatch time, per run. A Vault value echoed into an audit `details` field
  (there is no writer that does so today) would not be masked.
- **The dictionary's edges apply**: a genuine secret under 5 characters or equal
  to a common literal is never added, on purpose.
- **Fail-open before the first build.** An audit row written between boot and
  the first successful or partial build is unmasked. `Install` runs before every
  boot-time writer, so in practice that window is the first `Get`, which blocks.
- **Run logs and the audit stream mask the same settings columns.** Both
  consumers read the one `secrets.EncryptedSettingsColumns` table, so the GitLab
  webhook secret, S3 log-storage key, Vault role id and observability bearer
  token are masked in both places.
- **The process log is not masked** (deferred; see the durable-log note above).

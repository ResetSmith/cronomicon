# Security review

Each control, how it's enforced, and how it's verified. "Automated" = a Go test
that fails `make test` if the control regresses. "Operator" = a check you run
against your live deployment from outside the trust boundary; the probes are in
the administrator manual, section 8.4 ("Health, persistence & verification").

| # | Control | Enforced by | Verification |
|---|---|---|---|
| D.1.1 | **Trusted-proxy enforcement** — Remote-* honored only from an allowlisted peer; spoofed headers from elsewhere are stripped ⇒ unauthenticated | `auth.StripUntrustedHeaders` (global middleware) + `CRONOMICON_TRUSTED_PROXIES`; default-deny boot if unset | **Automated:** `TestSpoofedHeaderRejectedAtServer`, `auth.TestTrustedProxyStripsSpoofedHeaders`. **Operator:** manual §8.4 check 2 — send spoofed `Remote-User`/`Remote-Groups` headers straight to the app port; expect 401/403, never 200. |
| D.1.2 | **CSRF** double-submit on state-changing operator routes; runner bearer routes excluded | `auth.RequireCSRF` on POST/PUT/PATCH/DELETE; runner routes use `RequireRunner` (no CSRF) | **Automated:** `auth.TestRequireCSRF`, `auth.TestRequireRunner`. |
| D.1.3 | **Cookies** — CSRF cookie attrs; `CRONOMICON_COOKIE_SECURE=true` behind TLS; (OIDC mode) session cookie HttpOnly/Secure/SameSite | `auth.issueCSRF`, `session.go` codec; `CRONOMICON_COOKIE_SECURE` | **Automated:** session round-trip tests. **Operator:** inspect `Set-Cookie` on a live response. |
| D.1.4 | **Secrets at rest** — stored-secret values + SMTP password envelope-encrypted (AES-256-GCM) under a KEK; KEK mounted, backed up separately from the DB | `secrets` envelope scheme; `CRONOMICON_KEK_FILE` | **Automated:** `secrets` round-trip + `TestEncryptDecryptStringRoundTrip`. **Operator:** confirm KEK not in image/repo/S3 backup bucket. |
| D.1.5 | **Dev bypass off** — `/api/v1/auth/dev-login` not mounted unless `CRONOMICON_DEV_AUTH` | route mounted conditionally in `auth_mount.go` | **Automated:** `TestDevLoginMountedOnlyWhenEnabled`. **Operator:** `curl -s -o /dev/null -w '%{http_code}' https://<public-host>/api/v1/auth/dev-login` returns 404. |
| D.1.6 | **Surface check** — only `/healthz`, `/readyz`, `/version`, `/metrics`, `/auth/providers` (+ OIDC `/login`,`/callback` in oidc mode; token-gated `/webhooks/gitlab`) are unauthenticated | per-route middleware in the mount files | **Automated:** `TestUnauthenticatedSurface`. |
| D.1.7 | **`/metrics` not public** | your reverse proxy does not route `/metrics`; scrape it on the internal network only | **Operator:** manual §8.4 check 3 — `/metrics` through the public hostname returns 404. |
| D.1.8 | **Bootstrap admin removed** — `CRONOMICON_BOOTSTRAP_ADMIN_GROUP` unset after seeding access grants | env; loud warning logged while active | **Operator:** confirm the var is unset and the warning no longer logs (the lockout recovery, `cronomicon grant-admin` or a temporary re-enable, is in the administrator manual, §8.7). |

## Running the automated security suite

```sh
cd backend
go test ./internal/api/ ./internal/auth/ ./internal/secrets/ -run \
  'Spoofed|Surface|DevLogin|CSRF|Runner|Trusted|EncryptDecrypt|Session' -v
```

## Residual risks / notes

- **Trusted-proxy IP must be exact.** Give your reverse proxy a stable address
  and trust only that address (a `/32`). If the proxy IP changes, update
  `CRONOMICON_TRUSTED_PROXIES` — a too-wide CIDR weakens D.1.1. The administrator
  manual, §8.5, covers finding the address the app actually sees.
- **Header stripping happens in-app too.** Configure your proxy to strip client
  `Remote-*` headers at the edge (defense in depth); the app independently strips
  them from any untrusted peer — both layers must hold.
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
  (for example Authelia behind Traefik), this path — like the runner bearer-authed API
  (`/api/v1/runners/register|{id}/poll|{id}/redeclare|{id}/hostkeys`,
  `/api/v1/runs/{id}/manifest|log`), `/agents/*`, and `/runner-install.sh` —
  must be on the proxy's auth-bypass allowlist, or a runner (no SSO session)
  gets a 302-to-login HTML page instead of the script. The app still enforces
  the runner bearer on the `/api` paths, so the bypass skips only the SSO.
  The path list is in the administrator manual, section 8.3.
- **Host-key trust is an operator's approval, never automatic — for every
  runner, the server included.** A runner connects only to a host whose key an
  operator has approved for it, and refuses an unknown or changed key (no
  fall-open). Nothing is trusted on first connect.

  Until 2.3.0 that held for agents only. The server kept a key on each host
  and bastion record and captured it the first time it connected, in a run or
  in **Test connection**. Those columns are gone (migration `1270`). The
  **local runner** — the server running shell jobs itself — verifies every hop
  against the keys in force for it in `host_key_ledger` (approved and not
  superseded). The ledger is its whole trust store: there is no `known_hosts`
  file on the server, and an approval for the local runner is in force the
  moment it is written. A hop with no approved key is not connected to
  (`host_key_unverified`), whether or not the run injects secrets; a different
  key is refused as a mismatch; an unreadable ledger refuses; and the client
  asks a hop only for the key algorithms approved for it, so a host cannot be
  made to present a key of another type. A bastion is verified the same way,
  under the address its record is dialled at and never under its name (two
  agencies may each have a `jump`). **Test connection** on a host with no
  approved key reports `unverified` and writes nothing. Enforced by
  `sshexec/hostkey.go` (`verifyHostKey`, `hostTrust`, `bastionTrust`);
  **Automated:** `sshexec.TestARunConnectsOnlyToAHostWithAnApprovedKey`,
  `TestBastionHostKeyCallback`, `TestProbeHost_UnknownKeyIsReportedNotCaptured`.

  The keys the server had captured before 2.3.0 are parked by migration `1270`
  and carried into the local runner's ledger at the first start
  (`runner.CarryServerHostKeys`), so a host it was connecting to goes on
  working. A carried key is recorded as what it is — source `carried`, actor
  the upgrade — not as a reviewed approval, and it never replaces a key an
  operator has approved since. Where two records for one address held
  different keys, one is in force and a `host_key_conflict` notice names the
  other. **Residual:** a carried key was captured on a first connection nobody
  reviewed; the upgrade preserves that trust rather than re-examining it.
  Remove or replace it in the Host keys dialog if it was never verified.

  The scan → review → approve flow lets an
  operator trust a key without hand-assembling `known_hosts`: the runner scans
  hosts (a typed list, or a whole scope expanded on the server) from its own
  vantage — an agent on its host, the local runner in the server process — and
  the presented keys become candidates; an operator may instead paste lines
  they already hold, or copy the keys another runner trusts. Every source ends
  on one review screen (the UI shows the full SHA256 of every key for
  out-of-band comparison, and classifies each as new, already trusted or
  CHANGED, noting where it matches the key the local runner trusts for that
  host); for an agent the
  next poll delivers a `trust-hosts` op and the agent appends it. The trust
  decision is ALWAYS a human approval — the server never auto-trusts a scanned
  key, and the scan carries no credential (it reads only the public host
  key). Approving a fingerprint without checking it out of band trusts whatever
  the host presented to the scan, and the docs
  say so plainly (review screen + security guide §5). Every registered agent
  understands the host-key ops (the server's protocol floor tracks
  the current protocol version, 14 since 2.2.0, so an older agent is refused at
  registration, at redeclare and on every poll);
  the upload endpoints are runner-key-authed + ownership-guarded. The scanned key
  never bypasses
  verification — it only becomes a candidate for a human to approve.
  Since 2.2.0 (SB band) the flow is also hardened in five ways. (1) **The server
  does not take a runner's or an operator's word for a key**: uploaded and
  pasted lines are parsed server-side, the fingerprint is computed from the key,
  and the line delivered to a runner is rendered by the server for one validated
  host — wildcard, negated and marker (`@cert-authority`/`@revoked`) lines are
  refused, as is any host the runner's verifier could not load, and a paste is
  accepted whole or not at all. (2) **A commit is bound to the review**: it names
  the reviewed rows by host, key type, fingerprint and shown status, and a key
  that would replace a trusted one must be acknowledged as changed or the whole
  commit is refused (409 `review_stale`). (3) **Trust is revocable**: approving a
  changed key supersedes the old approval and an `untrust-hosts` op deletes the
  old line from the runner's file; an operator can remove a key outright. Before
  protocol 14 the file only ever grew. (4) **The record is a ledger, not a
  change-log line**: `host_key_ledger` is append-only for decisions and keeps
  runner (id and name, no FK — it outlives the runner), scope, host, source,
  previous fingerprint, actor and batch; a row is never pruned while its key is
  in force, and rejected/removed/replaced rows age out on the `hostKeyLedger`
  retention knob (365 days by default). Each batch also writes one change-log
  row naming the runner, and the audit export carries one row per key.
  (5) **Delivery is confirmed, not assumed**: the agent reports what its
  `known_hosts` holds (per line: host patterns, hashed flag, marker, key type,
  fingerprint — never the file, never a hostname hash) at startup, after every
  change and on request; the report is believed over the ledger's own stamps,
  and lines the app did not approve are shown, separately, as such. (The local
  runner has no file: its ledger rows are what it verifies against, so there
  is nothing to deliver or to report.) Every
  per-runner host-key route, reads included, needs `configureApp` on the agency
  that **owns** the runner (`requireRunnerOwner`); a runner that is Global's —
  the local runner, a Global-owned agent, a legacy placement — is a global
  administrator's. There is one narrow exception (LR-63,
  `requireRunnerOwnerOrHostKeyGuest`), for a runner that serves an agency
  which does not own it: an administrator of that agency may queue a scan of
  their own agency's scopes on it, see what that scan found, and approve the
  **first** key for a host — never a key that replaces one, never a second key
  type for a host that already has one, never typed hosts, a paste, a removal
  or the ledger (403 `owner_required`). On the local runner the exception
  stops at the scan: the server's trust in an address is one for every agency
  whose runs it takes, so only a global administrator decides its keys.
  **Automated:** `TestG3_EveryRunnerRouteIsTheOwners`,
  `TestG3_TheHostKeyExceptionIsPerScope`,
  `TestG3_AHostKeyGuestAddsAKeyAndNeverReplacesOne`,
  `TestHostKeyGuest_ASecondKeyOfAnotherTypeIsNotAFirstKey`,
  `TestLocalRunner_ItsHostKeysAreAGlobalAdministratorsToDecide`.
  Residual: a line present in an
  agent's file that Cronomicon did not approve is still trusted by that agent
  — it is made visible, not governed; a host behind a bastion cannot be
  scanned (the scan dials directly), so its key is always operator-supplied
  (the bastion itself is scanned); and the local runner holds one key per
  address and key type, so two machines that share an address behind
  different bastions cannot both be trusted by it.
- **Runner placement within an agency is operator-set on the scope, and fails
  closed** (SB band, 2.2.0). A scope may name the runners allowed to serve it
  (`scope_runners`); a bound scope's runs are claimable only by those runners,
  ANDed with the agency, capability and secret-injection rules — the binding
  narrows and never widens. It replaced the runner-tag pin, which was free text
  set by the job's author and enforced on the manual/token trigger only; a
  leftover `runner_tag:` in Git YAML is ignored with a warning, and no dispatch,
  gate or warning reads any tag (`runner/dispatch_reads_no_tags_test.go`). The
  binding is keyed on the runner **id**, never the name (a name is
  self-declared by the agent at registration), is never parsed from Git, and
  needs `configureApp` on the scope's own agency (`requireScopeAgency`) and a
  runner that already serves that agency (422 `runner_not_eligible`). Since
  2.3.0 it does **not** need authority over the runner (LR-62): a runner's
  placement is made once, by whoever owns it, and a binding only narrows which
  of the runners already serving the agency the scope uses. It
  has **no foreign key to `runners`**: deleting or reaping a bound runner leaves
  the scope bound and its runs waiting with a stated reason, rather than
  reopening the scope to its whole agency; only an operator unbinds, replaces,
  or restores the placement. An unreadable binding stops the producer rather
  than reading as unbound.
  A bound scope with runs waiting cannot be renamed or deleted (409
  `scope_bound_busy`), because a run carries its scope by name.

  Since 2.3.0 the binding is also the **only** control over where a scope's
  jobs run, and nothing goes round it. There is one claim statement
  (`runner.Claim`), used by an agent's poll and by the local runner alike, and
  the binding is a clause of it; before 2.3.0 the server's SSH pool claimed
  with a query of its own that knew nothing of agencies, bindings or
  requirements, and a job, a script, a global default or a run request could
  choose that executor. None of those is read any more (`spec.executor` is
  ignored with a warning, and the refusal `scope_requires_runner` is gone
  because nothing can ask for the server by name). The local runner is bound
  like any other runner. A run left queued for the SSH executor by a release
  before 2.3.0 is claimed by nothing and says so; it is cancelled and run
  again. **Automated:** `runner.TestClaimRuleMirrorsAgreeWithTheClaim`,
  `TestG3_BindingTakesTheScopeNotTheRunner`.

  Residual: an unbound scope's shell jobs are taken by **whichever** eligible
  runner asks first — the local runner or an agent, when both serve the
  scope's agency — so a job that ran from the server before 2.3.0 may now run
  on an agent, and the reverse (the upgrade raises `may_run_on_agent`,
  `may_run_on_server` and `mixed_scope` notices for the scopes affected);
  any administrator of a scope's agency may unbind it; and a
  binding governs a **scope**, not a host — a run on no scope, or on another
  scope, that reaches the same machine through its own host record is not
  confined by it.
- **A runner serves the agency that owns it, and the agent never says which**
  (2.3.0, migration `1250`). An agent's owner is the agency its registration
  token was minted for (`registration_tokens.agency_id`), written with its one
  serve row in the registration transaction; nothing in the register or
  redeclare body is read for it. Minting a token takes `configureApp` on that
  agency, and a token for Global takes a global administrator. A token whose
  agency has been deleted enrols nothing (`agency_gone`) rather than falling
  back to Global. The environment bootstrap token
  (`CRONOMICON_RUNNER_BOOTSTRAP_TOKEN`) has no row and so names no agency: it
  enrols a **Global**-owned agent that serves Global. Every writer of an
  owner or a serve list goes through one invariant
  (`settings.CheckRunnerPlacement`): after a write an agent's serve list is
  exactly its owner, or a non-empty subset of what it was — never wider, never
  empty. So there are no shared agents, and a claim (`runner.Claim`) takes
  only a run whose agency the runner serves; a run with no scope is Global's
  and only a runner that serves Global takes it. **Automated:**
  `runner.TestRegistrationSetsTheOwnerAndTheOneServeRow`,
  `TestRegistrationRefusesADeletedAgencyAndConsumesNothing`,
  `settings.TestNoWriterWidensOrEmptiesAnAgentsServeList`,
  `TestG3_MintingATokenTakesAuthorityOverItsAgency`, `TestG3_NobodyWidensAServeList`.
  Residual: a runner that served several agencies before 2.3.0 is a **legacy
  placement** — Global-owned, its serve list unchanged. It keeps claiming
  those agencies' runs (`runner.TestALegacyPlacementStillClaimsItsAgenciesRuns`),
  can be narrowed and never widened, is a global administrator's to manage,
  and is listed by a `legacy_placement` notice until it is narrowed to one
  agency and handed to it, or replaced by agents each agency owns. And the
  bootstrap token is multi-use and never consumed: any holder can enrol an
  agent that serves Global, the agency of every run with no scope. Treat it as
  a standing credential, or leave the variable unset.
- **The server runs jobs itself only when a global administrator says so, and
  never with a bound SSH key.** The **local runner** is the server executing
  shell jobs (`bash`, `perl`, `powershell`, `python`) over SSH from its own
  process. It is a row in the runner list, Global's, off on a new
  installation, and turned on or off under **Settings → Local runner**
  (`PUT /local-runner`, `requireGlobal(configureApp)`, audited). Turned on, the
  server process holds SSH private keys in memory and opens outbound SSH to job
  targets, and logs a warning saying so each time it starts; that is
  a larger blast radius than a server that only hands work to agents.
  `CRONOMICON_LOCAL_RUNNER=forbid` keeps it off whatever the setting says, and
  any value other than `allow` or `forbid` refuses to start. It is the one
  runner with a serve list — the agencies a global administrator names; Global
  alone on a new installation — and it claims through the same statement as an agent,
  under the same agency, binding, capability and requirement clauses, plus one
  of its own (`execspec.RunBindsKeySQL`): it **never takes a run that binds an
  SSH key**, declared on the job or script or added to the one run. A bound key
  is delivered as a file on the machine that runs the job, which only an agent
  does. Such a shell run waits for an agent, and is refused at enqueue when no
  registered agent serves its scope (`runref.KeyBindingsNeedAgent`: 422
  `key_binding_requires_runner` on a manual or token trigger, a `skipped` row
  for a scheduled fire, a failed workflow step). The identity a run connects
  as (`ssh_credential`) is not a key binding and does work on the local
  runner; it is loaded under the run's agencies (GC-21 below). **Automated:**
  `TestLocalRunner_TheRowExistsAndTheSwitchIsAGlobalAdministrators`,
  `TestLocalRunner_TheHostCanForbidIt`,
  `sshexec.TestTheLocalRunnerTakesAnAgencysRunOnlyOnceItServesThatAgency`,
  `runner.TestClaimRuleMirrorsAgreeWithTheClaim`, `runref.TestKeyBindingsNeedAgent`,
  `api.TestRunOfKeyBoundJobWithNoAgentIsRefused`. Residuals: the local runner
  resolves secret references on the server and loads the SSH keys of every
  agency it serves into one process, so serving an agency from it is a decision
  to trust the server host with that agency's targets (the upgrade raises an
  `agency_placed` notice for each agency it made the local runner serve); and
  `forbid` is about running jobs — **Test connection** on an SSH target still
  dials from the server with the record's key.
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
  re-registration loop. An operator without `configureApp` on the agency that
  owns the runner cannot reach the PATCH
  endpoint (session → CSRF → perm gate → `requireRunnerOwner`, activity-audited). A defense-in-depth
  alternative, the agent intersecting the server allowlist with a runner-local
  one, is deferred; it can be added without a protocol change if a review
  insists.
- **Runner secrets are never accepted as installer flag VALUES.**
  `runner-install.sh` can place the checkout deploy
  token and the Ansible vault password, but only via a file path
  (`--checkout-token-file` / `--vault-pass-file`) or a hidden stdin prompt
  (`--checkout-token -` / `--vault-pass -`, echo off) — passing the secret as a
  flag value is explicitly refused, because it would leak via `ps(1)` and shell
  history. The installed files get `0640 root:<the install's group>` — `cronomicon-runner`, or
  `cronomicon-runner-<name>` for an agent installed with `--instance` — (same custody as
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
  and never touches key material, the runner's owner or the agency it serves;
  registration tokens stay
  first-contact-only credentials (single-use per install: each
  minted token is consumed atomically by its first successful registration —
  two hosts cannot register from one token — and its row records which runner
  consumed it, an audit trail the shared token could not give; a leaked unused
  token is revocable and expires in 24h, and a leaked used token is worthless).
  The runner→server direction is the accepted
  tradeoff: a compromised `crn_run_*` key can re-declare its OWN row
  (widen advertised capabilities, rename, flip inventory mode) without operator
  action. It still cannot
  change its owner or the agency it serves (set by its registration token,
  never declared by the agent) or touch other rows, and claim
  eligibility stays bounded by that agency; the remedy for a compromised key
  is Deregister, which revokes it immediately.
- **Capability auto-detection broadens by default.** With `CRONOMICON_RUNNER_CAPABILITIES` unset, the agent claims the
  four shell run-types (`bash`, `perl`, `powershell`, `python`) whatever its
  own host holds, and probes its PATH at startup only for the two toolchains it
  runs locally (`ansible-playbook`, `terraform`). A shell job is executed on
  the TARGET, over SSH, with the target's interpreter, so the agent's host
  never bounded those types: what bounds them is the hosts the agent can reach
  and the keys it holds. (Before 2.3.0 the agent probed its own PATH for the
  shell interpreters too, which made a missing local `pwsh` look like a
  narrowing control; it never was one, and an agent that relied on it now
  claims the type.) Installing ansible or terraform on a runner host still
  silently widens what that runner will execute after its next restart
  (surfaced in the registry via the drift resync, but not gated on approval).
  This is deliberate: the declared set is still derived exclusively from the
  runner host's local state (never from the server), and job routing remains
  bounded by agencies and scopes. The narrowing lever is the explicit override:
  set `-capabilities` / `CRONOMICON_RUNNER_CAPABILITIES` on an agent that must
  not take a run-type. The server-side subtract-only capability mask (above)
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
| SU-1 | **Both kinds of runner fail closed on an `::cronomicon-output::` value that leaks an injected secret** — an `echo "::cronomicon-output name=X::$CRONOMICON_SECRET_*"` idiom drops the outputs and fails the run (`output_secret_leak`) rather than persisting the secret into `outputs_json` / a child step's `env_json` / the runs API. The local runner and the agent log-ingest path share one check | shared `execspec.FirstOutputLeakingSecret`; `sshexec.execute` guard + `finalizeReason`; `runner` ingest | **Automated:** `sshexec.TestSSHExecutorRefusesOutputLeakingSecret`, `execspec.TestFirstOutputLeakingSecret`, `runner.TestIngestRefusesOutputLeakingSecret`. |

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

## Bastion host-key verification

| # | Control | Enforced by | Verification |
|---|---|---|---|
| SU-4 | **The bastion hop is verified against an approved key** — the local runner connects through a bastion only when a key is in force for it in the local runner's ledger, under the address the bastion's record is dialled at; a key approved under the bastion's *name* does not count, a different key aborts, and nothing is captured on first connect. The target behind it is verified the same way | `sshexec.bastionHostKeyCallback` / `bastionTrust` (hostkey.go; used by conn.go and probe.go); `execspec.BastionByRef`; migration `1270` | **Automated:** `sshexec.TestBastionHostKeyCallback`, `TestSSHExecutorDoesNotConnectThroughAnUnapprovedBastion`, `db.TestMigrate1270ParksTheServersHostKeys`. |

**Status changed in 2.3.0.** Before it, the bastion's key was stored on its
record (`bastions.host_key`, migration `640`) and, when empty, captured on the
first connection — the accepted residual being that a first-connect MITM could
seed a key. An interim guard closed the worst case by refusing to inject
secrets over a bastion to a target whose key had not been captured yet
(`unpinned_bastion_target`). Both are gone: the column was dropped by
migration `1270`, so no connection captures a key any more, and the guard was removed
because the condition it guarded cannot arise — a hop with no approved key is
not connected to at all, with or without secrets
(`host_key_unverified`; `TestSSHExecutorDoesNotConnectThroughAnUnapprovedBastion`
runs a secret-injecting job through an unapproved bastion and asserts the
target is never reached and the secret is nowhere in the log). A bastion's key
is approved on the same review screen as any host's: the bastion is dialled
directly, so a scope scan includes it.

Accepted notes:

- **A host behind a bastion is not scanned.** The scan dials directly, so the
  target's key is supplied by an operator (pasted, or copied from a runner
  that already trusts it) and reviewed like any other.
- **A key carried by the upgrade was captured, not reviewed** (see the
  host-key note above).

## Sessions, grants and revocation

| # | Control | Enforced by | Verification |
|---|---|---|---|
| SU-5 | **Grants are resolved per request; the cookie carries no authority** (2.3.0) — a session cookie (`cronomicon_session_v4`) holds who the user is and which groups the identity provider asserted at login, and nothing about what those groups may do. Session, trusted-header and service-token requests all resolve their grants from one in-memory snapshot of `access_grants`, scope membership and agency names. Every writer of those tables calls `auth.GrantsChanged()` after its commit, so the request that follows a grant change is authorized on it; a 30-second TTL is the backstop for a writer this process cannot see (`cronomicon grant-admin`, a row edited by hand). Session TTL 8h | `auth/snapshot.go` (`grantSnapshotTTL`, `GrantsChanged`, `loadGrantSnapshot`); `auth/session.go` (`sessionPayload`) | **Automated:** `auth.TestTheCookieCarriesNoGrants`, `TestAGrantChangeReachesALiveSessionOnItsNextRequest`, `TestAScopeChangeReachesALiveSessionOnItsNextRequest`, `TestTheTTLFindsAChangeNobodyAnnounced`, `TestAFailedRebuildKeepsTheLastGoodSnapshotForABoundedTime`, `TestACancelledRequestDoesNotKeepARevokedGrantAlive`, `api.TestEveryGrantWriterIsInForceOnTheNextRequest`. |
| SU-5b | **Sessions can be revoked on purpose** — `POST /api/v1/auth/sessions/revoke` (a global administrator, `requireGlobal(manageRoles)`) advances a global session epoch; every cookie stamped below it is rejected on its next request. The caller's own cookie is re-issued. A revocation that could not be recorded answers 500 and is audited as a failure | `auth.Service.RevokeSessions` / `BumpSessionEpoch` / `readSession`; migration `641` | **Automated:** `auth.TestSessionEpochRevocation`, `TestRevokeSessionsSignsOutEveryoneButTheCaller`, `TestRevokeSessionsReportsAFailedRevocation`, `db.TestMigrate641RoundTrip`. |

**Status changed in 2.3.0.** Until then a user's grants were expanded to scope
names at login and frozen into the cookie, and every RBAC write bumped the
global epoch to keep a frozen grant from outliving a change: an edit in one
agency signed out every user of every other, and an administrator who
de-privileged themselves kept their old grants until the TTL. Both are gone.
No RBAC write signs anyone out; a grant, a scope's agency or a scope's name is
in force on the next request, for the acting administrator too. The cookie's
name changed (`_v3` → `_v4`) because its contents did, so **upgrading to 2.3.0
signs everyone out once**.

Accepted notes:

- **Group membership is as old as the session.** A cookie carries the groups
  the identity provider asserted at login, for up to eight hours. Changing
  what a group may do reaches a live session at once; removing a *person* from
  a group at the identity provider does not, until they sign in again. The
  revoke route is the lever for that case, and it is global: it signs out
  every other cookie session, not one user's. (In trusted-header mode the
  proxy asserts the groups on every request and there is no cookie to go
  stale.)
- **A failed rebuild serves the last good snapshot, for a bounded time.** A
  database that cannot be read must not turn every signed-in user into one
  with no access, so the previous snapshot stays in force while rebuilds fail
  — for at most two minutes (`grantSnapshotMaxStale`), after which resolution
  fails and requests are refused. With no snapshot at all it fails at once.
  Within that window a revocation written just before the outage may not yet
  be in force.
- **The rebuild does not run on the request that triggered it.** A rebuild
  cancelled with an aborted request would leave the stale snapshot in force
  for everyone, a way to keep a revoked grant alive.
- **The epoch assumes a single server instance** (SQLite): it is mirrored in
  memory. The grant snapshot is per process for the same reason.

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

## Install-wide and cross-agency gates

An administrator of one agency cannot change the installation or another
agency (GC band, v2.2.2). Before it, most write routes asked only whether the
caller held the permission on *some* agency. 2.3.0 keeps every gate below and
changes what several of them ask, because the objects changed: **Global** is a
real agency (what was "no agency" is Global's, and a row in Global is every
agency's to use and a global administrator's to change), every scope, secret,
variable, SSH key, host record, bastion and runner belongs to exactly one
agency, and an agency administers more of its own (its host records and
bastions, its Vault-backed secrets inside its own path prefixes, its agents).
Rows marked 2.3.0 say what changed.

| # | Control | Enforced by | Verification |
|---|---|---|---|
| GC-1 | **Install-wide writes need a global administrator** — an unrestricted grant that itself carries the permission: every install-wide setting, the audit export, Git sync and scope resync, the agency catalog, alert rules and, from 2.3.0, an agency's Vault path prefixes, the local runner's setting and signing every session out. (Bastions and hand-written host records left this list in 2.3.0: they have an owner, GC-7) | `api.requireGlobal` (`Identity.GlobalAdmin(perm)`) | **Automated:** `TestInstallWideRoutesRefuseAnAdminOfOneAgency` probes every route the table classes as global |
| GC-3 | **"Unrestricted" is never checked without the permission** — a viewer on all scopes who administers one agency is not a global administrator | `requireRoleTemplateAdmin`, `requireGrantWritable`, script bindings, placement | **Automated:** `TestGC_UnrestrictedViewerWhoAdministersOneAgencyIsNotAGlobalAdmin`, `TestAViewerOfEveryScopeCanChangeNothing` |
| GC-4 | **No route decides on a role's name** — the shared authoring surfaces (reusable schedules, calendars, reactions, revisions, recycle bin) need compose and configureApp on one unrestricted grant | `api.requireComposeAdmin` | **Automated:** `TestGC_SharedAuthoringNeedsComposeAndConfigureOnEveryAgency` |
| GC-5 | **A service account is a grant** — mint and revoke obey own-agency, no-all-scopes and no-amplification; the list is filtered | `requireGrantWritable` in `createServiceAccount` / `revokeServiceAccount` | **Automated:** `TestGC_ServiceAccountsFollowTheDelegationRules` |
| GC-6 | **A scope is administered by its own agency** — edit, inventory, delete, tags and runner binding; a new scope is born in its creator's agency; a scope in Global is a global administrator's; a runner replace needs every bound scope. **2.3.0:** a scope has one agency, and moving it is a two-sided act — `configureApp` on the agency it is in (on *every* one, for a scope still shared from before 2.3.0) and on the agency it is going to, with Global on either side taking a global administrator. Until 2.3.0 any move was a global administrator's. The same rule moves a secret, a variable and an SSH key, and the move carries the row's owner in the same transaction (409 `owner_conflict` on a name the target already owns) | `requireScopeAgency`, `requireMove`, `requireCreationAgencies`; `settings.SetAgencyMembership` | **Automated:** `TestGC_ScopesAreAdministeredByTheirOwnAgency`, `TestGC_ANewScopeLandsInItsCreatorsAgency`, `TestGC_ReplacingARunnerNeedsEveryBoundScope`, `TestGlobalAgency_OnlyAGlobalAdministratorMovesARowInOrOut`, `TestOneAgency_ASecretMovesWithItsOwner` |
| GC-7 | **Host and bastion records have an owner** — a record imported for a scope follows that scope. **2.3.0:** a hand-written host record and a bastion belong to one agency (`owner_agency`, migration `1230`): its administrators change it, a global administrator changes Global's, and a new one is born in an agency its author administers. Until 2.3.0 both belonged to nobody, applied to every scope and were a global administrator's alone. A scope resolves only its own agency's records and Global's, and a host's `via` only its own agency's bastion or Global's | `requireHostOwner`, `requireBastionOwner`, `requireRecordOwnerChoice`; `execspec.HostRecordForScopeSQL`, `execspec.BastionByRef` | **Automated:** `TestGC_HostAndBastionRecords`, `execspec.TestAScopeResolvesItsOwnAgencysHostRecordsAndGlobalsOnly` |
| GC-8 | **A Vault path is named only inside the owner agency's prefixes** — create, edit, migrate and move of a Vault-backed secret or SSH key; the gate asks the store's question (a secret: anything not exactly `stored`, or any path; a key: exactly `vault`, or any reference). **2.3.0:** in 2.2.2 every Vault path was a global administrator's to name. Now a row that is an agency's may name a path inside a prefix a global administrator assigned to that agency (`PUT /agencies/{agencyId}/vault-prefixes`), judged by whole path segments with no `..`, `.`, empty segment or character a URL could decode into one; an agency with no prefix can name none (422 `vault_path_not_allowed`); the rule binds a global administrator too; and the caller needs the permission on the row's *owner* agency, not on any agency a legacy shared row is in. A row that is Global's is still a global administrator's, any path | `requireVaultPath`, `internal/vaultpath`; `secretNamesVault`, `credentialNamesVault` | **Automated:** `TestGC_VaultPathsAreAGlobalAdministrators`, `TestGC_VaultGateCannotBeSidesteppedBySourceSpelling`, `TestGC_AVaultBackedSSHKeyIsAGlobalAdministrators`, `TestVaultPrefixes_AnAgencyNamesPathsInsideItsOwnAndNoOthers`, `TestVaultPrefixes_OnlyTheOwnerNamesASharedRowsPath`, `TestVaultPrefixes_AreJudgedAsWrittenAndReplacedOnlyOnPurpose`, `vaultpath.TestUnderMatchesWholeSegments`, `TestMalformedPathsAreRefusedNotRepaired`, `settings.TestNarrowingASharedVaultRowDoesNotGiveAwayAPathItsNewOwnerMayNotName` |
| GC-9 | **Publish is authorized per file** — the scope in the incoming content, the scope of the file it replaces, and any Git job already using the name; schedule and workflow files, unscoped jobs and nameless jobs are a global publisher's | `Server.authorizePublish` | **Automated:** `TestGC_PublishIsCheckedPerFile`, `TestGC_PublishRefusesANamelessJob` |
| GC-10 | **Workflow authorization follows sub-workflows** — compose, trigger, pause and cancel are checked against every job the tree runs, resolved by the function the engine itself uses | `workflow.Engine.JobScopes` / `SubWorkflowJobScopes` / `DescendantRunScopes` | **Automated:** `TestGC_WorkflowAuthorizationFollowsSubWorkflows`, `TestGC_AnotherAgencysSameNamedWorkflowIsNotAVeto`, `workflow.TestSubWorkflowJobScopesReWalksAtAShallowerDepth` |
| GC-11 | **Cancelling a pending run needs a run verb** on every scope it would touch | `cancelPendingRun` | **Automated:** `TestGC_CancellingAPendingRunNeedsARunVerb` |
| GC-12 | **Tags and annotations need a readable row** | `requireJobVisible`, `requireWorkflowVisible` | **Automated:** `TestGC_TagsAndNotesNeedAReadableRow` |
| GC-13 | **A reaction matches its upstream by identity**, not by a name another agency may share; the name is a fallback only while it is unambiguous | `Scheduler.deliverEvent` | **Automated:** `scheduler.TestReactionMatchesTheUpstreamByIdentityNotName`, `TestReactionNameFallbackNeedsAnUnambiguousName` |
| GC-20 | **A notice is read and dismissed by the agency it is about** — a scope-binding notice by its own scope's administrator; **2.3.0:** every kind in the Notices inbox by `configureApp` on the notice's agency, with an install-wide notice filed under Global and so a global administrator's. A dismissal that mixes the caller's own notices with another agency's is refused whole | `handleDismissScopeBindingNotices`; `handleListNotices`, `handleDismissNotices` | **Automated:** `TestGC_DismissingANoticeNeedsItsScope`, `TestNotices_AreReadAndDismissedByTheirAgency` |
| GC-14 | **Notification target URLs are returned only to a global administrator** | `handleGetNotifications` | **Automated:** `TestGC_NotificationTargetURLsAreMasked` |
| GC-21 | **A host uses only a key its agency may use** (v2.2.3) — a host record may name only a key of its scope's agency or one that is Global's, and the local runner loads every key a run connects with, by id and by name, with the run's agency snapshot, as an agent's manifest does; one refusal for another agency's key and for a missing one. **2.3.0:** the rule covers hand-written host records and bastions too, against their owner agency, and binds a global administrator (a Global record may not name an agency's key); a record's own key — a bastion's, a host's under Test connection — is loaded under its owner, so no key is loaded unchecked any more. A record that already names a key its owner may not use is a `record_key_outside_owner` notice | `requireKeyUsableBy`, `sshexec.keyGuard` / `ownerKeyGuard` / `loadSignerChecked`, `runref.KeyIDUsable` | **Automated:** `TestGC_AHostRecordMayNotNameAnotherAgencysKey`, `TestGC_ARecordNamesOnlyAKeyItsOwnerMayUse`, `sshexec.TestKeyGuardChecksAKeyNamedByID`, `TestKeyGuardChecksAKeyNamedByName`, `TestHostKeyGuardFollowsTheRecordsScope`, `TestARunDoesNotConnectWithAnotherAgencysKey` |
| GC-22 | **A run's single target host must be in its scope** — v2.2.3 held only a restricted actor's per-run override to the scope's host list. **2.3.0:** the rule covers the host the job itself declares as well, for every caller, a global administrator included, and on every producer: the run fails that host when its targets are resolved, and a manual or token trigger gets the same answer early (422 `scope_membership`). A job with no scope, or one whose scope lists no hosts (they live in a runner's own inventory), has no membership to ask about; a job already authored against a host outside its scope is a `target_host_outside_scope` notice | `execspec.HostInScope`, in `execspec.ResolveTargets` and `runJobWithKind` | **Automated:** `TestGC_ARunsTargetHostMustBeInTheScope`, `execspec.TestAFixedTargetHostMustBeAMemberOfTheScope` |
| GC-23 | **Reference bindings need the permission on the job's own scope** (v2.2.3), from one grant; an unscoped job's are a global administrator's | `putJobBindings` | **Automated:** `TestGC_ReferenceBindingsNeedThePermissionOnTheJobsScope` |
| GC-24 | **Pausing or resuming a job with no scope needs the verb unbound** (v2.2.3) — a job with no scope is Global's, and `killJobs` on one agency is not authority over it; a job in a scope is unchanged | `requirePauseAuthority` | **Automated:** `TestGC_PausingAJobWithNoScopeNeedsTheVerbUnbound` |
| GC-18 | **Every write route is classified** — a route that is not a GET cannot be registered without an entry saying who may call it | `writeRouteGates` | **Automated:** `TestEveryWriteRouteIsClassified` |

Accepted notes:

- **Vault paths are divided by prefix, and the division is enforced on write
  only** (2.3.0; they were closed to every agency in 2.2.2). The installation
  still has one Vault connection with one credential, so the prefix rule is
  the whole of the separation between agencies there. It is applied when a
  path is written and when a row is moved, **never when a run resolves it**:
  a row written before the rule, or whose agency later lost a prefix, keeps
  resolving. Removing a prefix revokes nothing
  (`TestVaultPrefixes_RemovingOneRaisesANoticeAndRevokesNothing`); what falls
  outside is listed by a `vault_path_outside_prefix` notice for someone to
  correct.
- **A row in no agency is damage, never a meaning** (2.3.0, migration `1220`).
  Triggers give every new scope, secret, variable and key a Global row until
  an agency is named (a runner is born serving its owner) and refuse
  Global beside a named agency; the setters refuse to leave a row in neither
  (422 `agency_required`) or in both (422 `global_mixed`). Where a row is
  nevertheless found with no agency, the per-object gate answers 500, dispatch
  matches nothing and the producer refuses, rather than reading it as
  "everyone's"; a global administrator re-homes it. No grant or service
  account may name Global — the way to be a global administrator is an
  all-agencies grant. **Automated:**
  `TestGlobalAgency_ARowIsInGlobalOrADepartmentNeverBothNeverNeither`,
  `TestGlobalAgency_ARowInNoAgencyIsAFaultNotGlobal`,
  `TestGlobalAgency_CannotBeGranted`,
  `execspec.TestAScopeWithNoAgencyIsAnErrorNotGlobal`.
- **Rows shared by several agencies before 2.3.0 stay shared until someone
  settles them.** One agency per row is a writer rule, not a constraint on old
  data: such a row keeps working, moving it takes every agency it is in (or a
  global administrator), and a `scope_several_agencies` notice lists the
  scopes. Until then each of those agencies' grants reaches it.
- **Shared objects still have no owner.** A reusable schedule, a calendar and
  an alert rule can still affect every agency; the control is that only a
  global administrator can write one.
- **A sub-workflow is authorized as it resolves at that moment.** A child that
  is disabled when its parent is triggered and enabled before its step is
  reached runs on an authorization that did not see it.
- **Reads are not covered here.** Activity, the change log, workflow
  definitions, schedules and script bodies are readable by any session.
- **A workflow has no token opt-in.** A service account can trigger any
  workflow its grant covers; only jobs have `requestable`.

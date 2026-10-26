# Security review

As of 2.5.3. Each control, how it's enforced, and how it's verified. "Automated" = a Go test
that fails `make test` if the control regresses. "Operator" = a check you run
against your live deployment from outside the trust boundary; the probes are in
the administrator manual, section 8.4 ("Health, persistence & verification").

| # | Control | Enforced by | Verification |
|---|---|---|---|
| D.1.1 | **Trusted-proxy enforcement** — Remote-* honored only from an allowlisted peer; spoofed headers from elsewhere are stripped ⇒ unauthenticated | `auth.StripUntrustedHeaders` (global middleware) + `CRONOMICON_TRUSTED_PROXIES`; default-deny boot if unset | **Automated:** `TestSpoofedHeaderRejectedAtServer`, `auth.TestTrustedProxyStripsSpoofedHeaders`. **Operator:** manual §8.4 check 2 — send spoofed `Remote-User`/`Remote-Groups` headers straight to the app port; expect 401/403, never 200. |
| D.1.2 | **CSRF** double-submit on state-changing operator routes; runner bearer routes excluded | `auth.RequireCSRF` on POST/PUT/PATCH/DELETE; runner routes use `RequireRunner` (no CSRF) | **Automated:** `auth.TestRequireCSRF`, `auth.TestRequireRunner`. |
| D.1.3 | **Cookies** — CSRF cookie attrs; `CRONOMICON_COOKIE_SECURE=true` behind TLS; (OIDC mode) session cookie HttpOnly/Secure/SameSite | `auth.issueCSRF`, `session.go` codec; `CRONOMICON_COOKIE_SECURE` | **Automated:** session round-trip tests. **Operator:** inspect `Set-Cookie` on a live response. |
| D.1.4 | **Secrets at rest** — stored-secret values, SSH keys and every sealed setting (`secrets.EncryptedSettingsColumns`) envelope-encrypted (AES-256-GCM) under a KEK; KEK mounted, backed up separately from the DB. **2.5.1:** each value is bound to its row (SG-a below) | `secrets` envelope scheme; `CRONOMICON_KEK_FILE` | **Automated:** `secrets` round-trip + `TestEncryptDecryptStringRoundTrip`, `TestProductionSealsAreBound`. **Operator:** confirm KEK not in image/repo/S3 backup bucket. |
| D.1.5 | **Dev bypass off** — `/api/v1/auth/dev-login` not mounted unless `CRONOMICON_DEV_AUTH` | route mounted conditionally in `auth_mount.go` | **Automated:** `TestDevLoginMountedOnlyWhenEnabled`. **Operator:** `curl -s -o /dev/null -w '%{http_code}' https://<public-host>/api/v1/auth/dev-login` returns 404. |
| D.1.6 | **Surface check** — only `/healthz`, `/readyz`, `/version`, `/metrics`, `/auth/providers` (+ OIDC `/login`,`/callback` in oidc mode; token-gated `/webhooks/gitlab/{repoId}` (from 2.4.0), checked against its own repository's secret; the id-less `/webhooks/gitlab`, which addressed the built-in agency's repository, answers 401 as an unknown repository from 2.5.3) are unauthenticated | per-route middleware in the mount files | **Automated:** `TestUnauthenticatedSurface`. |
| D.1.7 | **`/metrics` not public** | your reverse proxy does not route `/metrics`; scrape it on the internal network only | **Operator:** manual §8.4 check 3 — `/metrics` through the public hostname returns 404. |
| D.1.8 | **Bootstrap admin removed** — `CRONOMICON_BOOTSTRAP_ADMIN_GROUP` unset after seeding access grants | env; loud warning logged while active | **Operator:** confirm the var is unset and the warning no longer logs (the lockout recovery, `cronomicon grant-admin` or a temporary re-enable, is in the administrator manual, §8.7). |

## Running the automated security suite

```sh
cd backend
go test ./internal/api/ ./internal/auth/ ./internal/secrets/ -run \
  'Spoofed|Surface|DevLogin|CSRF|Runner|Trusted|EncryptDecrypt|Session|CA1|CA2|CA3' -v
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
  that **owns** the runner (`requireRunnerOwner`); a runner of the built-in
  agency, **Central** (on an installation made before 2.5.2 it is called
  Global until a global administrator renames it), is a global
  administrator's: an agent enrolled for it before 2.5.3, a legacy placement,
  the local runner while it is unassigned. From 2.5.3 the local runner belongs to one
  agency (below), and that agency's administrators scan with it and decide
  its keys (question 204). There is one narrow exception (LR-63,
  `requireRunnerOwnerOrHostKeyGuest`), for a runner whose serve list names an
  agency which does not own it: an administrator of that agency may queue a
  scan of their own agency's scopes on it, see what that scan found, and
  approve the **first** key for a host — never a key that replaces one, never
  a second key type for a host that already has one, never typed hosts, a
  paste, a removal or the ledger (403 `owner_required`). Since 2.4.0 only a
  legacy placement can be such a runner: the local runner's serve list is
  its owner alone (below), so the exception no longer reaches it.
  **Automated:** `TestG3_EveryRunnerRouteIsTheOwners`,
  `TestG3_TheHostKeyExceptionIsPerScope`,
  `TestG3_AHostKeyGuestAddsAKeyAndNeverReplacesOne`,
  `TestHostKeyGuest_ASecondKeyOfAnotherTypeIsNotAFirstKey`,
  `TestLocalRunner_ItsHostKeysAreAGlobalAdministratorsToDecide` (unassigned),
  `api.TestCA3TheLocalRunnersAgencyReviewsItsHostKeys`.
  Residual: a line present in an
  agent's file that Cronomicon did not approve is still trusted by that agent
  — it is made visible, not governed; a host behind a bastion cannot be
  scanned (the scan dials directly), so its key is always operator-supplied
  (the bastion itself is scanned); and the local runner holds one key per
  address and key type, so two machines that share an address behind
  different bastions cannot both be trusted by it. And (2.5.3, documented,
  not engineered) the server's ledger and its pins (`serverPin`,
  `trustedKeySQL`) were a global administrator's to write; now the local
  runner's agency's administrators write them, and a key they approve for no
  scope is shown to other agencies' reviewers as the key the server trusts.
  Using that to reach another agency needs an administrator acting in bad
  faith, which the separation model (cooperating departments) documents
  rather than engineers against. A change of the local runner's agency
  deletes its ledger, pending keys and scan requests (below), so one agency's
  approvals never stand for another's hosts.
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

  Residual: an unbound scope's jobs are taken by **whichever** eligible
  runner of its agency asks first — for the shell jobs of a scope of the local
  runner's agency, the local runner or one of that agency's agents (from 2.5.3
  the local runner serves the one agency it belongs to, so every other
  agency's scope is taken by its own agents alone). The 2.3.0 upgrade pass
  that recorded where each scope's shell jobs had run, and raised
  `may_run_on_agent`, `may_run_on_server` and `mixed_scope` from it, is
  retired in 2.5.0: an installation upgrading from 2.2.x gets no such notice.
  Any administrator of a scope's agency may unbind it; and a
  binding governs a **scope**, not a host — a run on no scope, or on another
  scope, that reaches the same machine through its own host record is not
  confined by it.
- **A runner serves the agency that owns it, and the agent never says which**
  (2.3.0, migration `1250`). An agent's owner is the agency its registration
  token was minted for (`registration_tokens.agency_id`), written with its one
  serve row in the registration transaction; nothing in the register or
  redeclare body is read for it. Minting a token takes `configureApp` on that
  agency. **2.5.3:** no token is minted for Central (422
  `central_runs_nothing`, CA-a below), and one minted for it before enrols
  nothing and is not consumed (401 `token_for_central`). A token whose
  agency has been deleted enrols nothing (`agency_gone`) rather than falling
  back to Central. The environment bootstrap token
  (`CRONOMICON_RUNNER_BOOTSTRAP_TOKEN`) had no row and so named no agency: it
  enrolled agents as the built-in agency's. **Retired in 2.5.3:** it is
  refused (401 `bootstrap_token_retired`), enrols nothing, and while it is
  still set the server warns at start and files the
  `environment_settings_ignored` notice. Every writer of an
  owner or a serve list goes through one invariant
  (`settings.CheckRunnerPlacement`): after a write an agent's serve list is
  exactly its owner, or a non-empty subset of what it was — never wider, never
  empty. So there are no shared agents, and a claim (`runner.Claim`) takes
  only a run whose agency the runner serves; a run's agency is its scope's,
  and from 2.5.3 a job with no scope runs as its own agency's (CA-5; until
  then such a run was the built-in agency's). **Automated:**
  `runner.TestRegistrationSetsTheOwnerAndTheOneServeRow`,
  `TestRegistrationRefusesADeletedAgencyAndConsumesNothing`,
  `settings.TestNoWriterWidensOrEmptiesAnAgentsServeList`,
  `TestG3_MintingATokenTakesAuthorityOverItsAgency`, `TestG3_NobodyWidensAServeList`,
  `runner.TestBootstrapTokenUnchanged` (the refusal), `runner.TestRegistrationToken`
  (`token_for_central`), `runref.TestCheckoutNeedsAgentForAnAgencysScopelessRun`.
  Since 2.4.0 the claim joins on the runner's **owner** as well as its serve
  row, so every runner takes its owner's runs only.
  Residual: a runner that served several agencies before 2.3.0 is a **legacy
  placement** — owned by the built-in agency, its serve list unchanged. Since
  2.4.0 it claims its owner's runs only, and since 2.5.3 its owner, Central,
  runs nothing, so it takes no work; the other agencies on its list wait for
  agents of their own (`runner.TestALegacyPlacementClaimsOnlyItsOwnersRuns`); its list
  still opens the host-key exception above to those agencies' administrators.
  It can be narrowed and never widened, is a global administrator's to manage,
  and is listed by a `legacy_placement` notice until it is narrowed to one
  agency and handed to it, or replaced by agents each agency owns. An agent
  of Central's left from before 2.5.3 (one enrolled with the bootstrap token,
  for instance) takes no work and is listed by the `central_holds_work`
  notice: it is deregistered and enrolled again with a token minted in its
  agency. Until 2.5.3 the bootstrap token was a standing, multi-use
  credential that enrolled an agent of the agency of every run with no scope;
  that residual is closed. After a database restore, an agent enrolled since
  the backup is enrolled again with a token minted in its agency
  (`backup-restore.md`).
- **The server runs jobs itself only when a global administrator says so, and
  never with a bound SSH key.** The **local runner** is the server executing
  shell jobs (`bash`, `perl`, `powershell`, `python`) over SSH from its own
  process. It is a row in the runner list, off on a new installation, and
  given an agency and turned on or off under **Settings → Local runner**
  (`PUT /local-runner`, `requireGlobal(configureApp)`, audited). Turned on, the
  server process holds SSH private keys in memory and opens outbound SSH to job
  targets, and logs a warning saying so each time it starts; that is
  a larger blast radius than a server that only hands work to agents.
  `CRONOMICON_LOCAL_RUNNER=forbid` keeps it off whatever the setting says, and
  any value other than `allow` or `forbid` refuses to start. From 2.4.0 to
  2.5.2 it served the built-in agency only (migration `1380`). **From 2.5.3**
  (CA-8 to CA-10) it belongs to one ordinary agency, chosen by a global
  administrator (`settings.SetLocalRunnerOwner`, `PUT /local-runner`
  `ownerAgency`), or to none, and serves exactly its owner: no other write
  can widen it (`settings.CheckRunnerPlacement`, 422
  `local_runner_serves_owner`; `SetRunnerOwner` refuses the local runner).
  With none it is off whatever the stored switch says
  (`settings.LocalRunnerEnabled`), and turning it on is 422
  `local_runner_unassigned`; a new installation's is born so, and the upgrade
  leaves an existing one so, with the `local_runner_unassigned` notice if it
  was on. A change of its agency is one transaction, refused while it runs a
  run (409 `local_runner_busy`) or while scopes are bound to it (409
  `local_runner_bound`, naming them: deleting the bindings would open those
  scopes to every runner of their agency); it deletes the host-key ledger,
  the pending keys and the scan requests, turns the switch off, and is
  recorded for the installation and for both agencies. The route judges a
  request whole before writing anything. The `local_runner_served` notice is
  retired. It
  claims through the same statement as an agent,
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
  `settings.TestCA3TheLocalRunnerIsGivenToOneAgencyOrNone`,
  `settings.TestAnUnassignedLocalRunnerServesItsOwnerOnly`,
  `api.TestCA3AChangeOfTheLocalRunnersAgencyIsJudgedWhole`,
  `sshexec.TestTheLocalRunnerTakesGlobalsRunsOnly`,
  `db.TestMigrate1380LocalRunnerGlobalOnly`,
  `runner.TestClaimRuleMirrorsAgreeWithTheClaim`, `runref.TestKeyBindingsNeedAgent`,
  `api.TestRunOfKeyBoundJobWithNoAgentIsRefused`. Residuals: the local runner
  resolves its agency's secret references on the server and loads that
  agency's SSH keys into its process, so giving it an agency and turning it
  on is a decision to trust the server host with that agency's targets (it
  reaches no other agency's); between a committed change of agency and the
  engine being told of it there is a window of one claim (the claim loop does
  not re-read the switch); and
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
  so a compromised server still cannot exfiltrate or inject secret bytes. A
  checkout run's manifest names the repository its run was pinned to at
  enqueue (`runs.checkout_repo`; since 2.4.0 the job's own agency's
  repository, and an agent serves only its own agency), and the agent refuses
  a URL its allowlist does not hold exactly as written
  (`agent.materializeCheckout`). The
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
  own host holds, and probes its PATH at startup only for the toolchains it
  runs locally (`ansible-playbook` among them). A shell job is executed on
  the TARGET, over SSH, with the target's interpreter, so the agent's host
  never bounded those types: what bounds them is the hosts the agent can reach
  and the keys it holds. (Before 2.3.0 the agent probed its own PATH for the
  shell interpreters too, which made a missing local `pwsh` look like a
  narrowing control; it never was one, and an agent that relied on it now
  claims the type.) Installing a local toolchain such as ansible on a runner host still
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
- **Git-sourced job/workflow names are entirely unvalidated.** Repository sync inserts the YAML's `metadata.name` **verbatim** —
  no regex, no allowlist, no length limit, not even a trim — so a definition
  named `../../etc/cron.d/x` is reachable by anyone with merge rights on any
  agency's synced repository (every agency has one since 2.4.0), and an empty name falls back to a value that resolves to `.`.
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
- **The validation image is a CI tool, not a server** (2.5.0).
  `ghcr.io/resetsmith/cronomicon-validate:<version>` (`backend/Dockerfile`,
  `--target validate`) is Debian 12 with a shell, `curl`, CA certificates and
  the `cronomicon` binary, so that `deploy/ci-validate-template.yml` can run
  `cronomicon validate` in a CI job's shell; the distroless server image has
  none. It is the same binary as the server's, but a CI job runs only
  `validate` over its checkout: no database, KEK or server credential is in
  the image. The template's optional merge-request comment needs a masked
  `GITLAB_MR_TOKEN` with the `api` scope; that token is the CI project's to
  hold and scope, not Cronomicon's.

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
- **Status changed in 2.4.0 and 2.5.0.** First-class schedule-defs
  (`GET /api/v1/schedule-defs`, the `schedules` table) and the scripts catalog
  were unscoped by design until then. A schedule, a calendar and a script are
  now an agency's (2.4.0), and from 2.5.0 they, like every list, are read by
  the reading rule (CF-1, "Install-wide and cross-agency gates" below), not by
  scope.
- **Workflow runs are read by their agency** (2.5.0, migration `1520`):
  `workflow_runs.agency_id` is filled from the workflow by the trigger
  `workflow_runs_agency_stamped` (a refused child takes its parent's), and the
  list applies the reading rule. Until then `workflow_runs.scope` was NULL for
  engine-created rows and those rows were visible to every session.

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
| AM-4 | **Process-wide redaction dictionary** — stored secrets + SSH credentials + the encrypted settings entries (ONE table, `secrets.EncryptedSettingsColumns`, shared with `rewrap-secrets`; since 2.4.0 three of them — a repository's token, its webhook secret and its previous webhook secret — cover every row of `git_repos`, one per agency; since 2.5.1 a push target's URL covers every row of `alert_targets` and an agency's Vault role and secret ids every row of `agency_vaults`) + multi-line env_vars, every scope; rebuilt lazily after each source write, 5-minute TTL backstop, last-good kept on a failed rebuild, partial-with-error on undecryptable rows | `internal/redactdict`; `secrets.RedactionReport`; `secrets.RedactionSourceChanged` at every source write site | **Automated:** `redactdict.TestBuildUnionsEverySourceAndKeepsVariablesVisible`, `TestBuildReportsUndecryptableAsPartial`, `TestStoreLifecycle`, `TestStoreConcurrentReadersNeverBlockOnRebuild` (`-race`), `TestEveryRedactionSourceWriterNotifies`, `secrets.TestRedactionReportCoversEverySettingsColumn`. |
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
- **Run logs and the audit stream mask the same settings columns, but not the
  same agencies' values** (2.5.1). Both consumers read the one
  `secrets.EncryptedSettingsColumns` table. The audit stream is masked with
  every agency's values; a run's log with its own agency's and Central's
  secrets and keys, the installation's settings, and of the per-agency
  settings only its own agency's (SG-c below).
- **The process log is not masked** (deferred; see the durable-log note above).

## Secrets guarded (2.5.1)

An agency can review what happens to its secrets, and the stored values are
harder to take or move. The decisions are SG-1 to SG-40 in the plan.

| # | Control | Enforced by | Verification |
|---|---|---|---|
| SG-a | **Each sealed value is bound to its row** — a secret's and an SSH key's row (table and id), a sealed setting's row (table and key) are the AES-GCM additional data of both envelope operations, so a value copied into another row (another agency's secret, another repository's token) does not open there, even with the KEK. Values from before are re-sealed at the first start; once every one is bound, unbound values are refused. **This makes 2.5.1 one-way** (no earlier release, 2.2.3 or a build of 2.5.0, can open a bound value) | `secrets.Binding`, `RowBinding`, `SettingsBinding`; `secrets.BindSeals` (marker `upgrade.2.5.1.bindSeals`), `RefuseUnboundSeals`; `rewrap-secrets` honours the marker | **Automated:** `TestProductionSealsAreBound` (no unbound seal or open in production code), `secrets.TestBindSeals` (a copied value does not open; an unbound one is refused after the pass), `TestBindSealsCountsAnUnreadableColumn`, `TestBindSealsLeavesWhatItCannotOpen`, `TestRewrapRefusesUnboundAfterTheMarker` |
| SG-b | **Backups can be encrypted to keys the server never holds** — optional; age X25519 public keys in Settings → Backups; the plaintext snapshot lives only in a 0700 staging directory and is removed on every path; a failed encryption keeps and uploads nothing; turning it on needs the custody acknowledgement | `internal/backup/seal.go`; `db.writeSnapshot`; `settings.SetBackupRecipients`; `cronomicon restore --identity` | **Automated:** `TestSnapshotSealedWhenAKeyIsSet`, `TestSnapshotSealFailureKeepsNothing`, `TestSnapshotUploadFailureLeavesNoPlaintext`, `TestStagingClearedAtStart`, `TestDecryptWrongKeyRefusedWritesNothing`, `TestParseRecipientRefusals`, `TestInstallEncryptedSnapshot`, `TestBackupSettingsRoutes`. **Operator:** after the first encrypted backup, confirm the bucket object ends in `.db.age` and delete the unencrypted ones from before |
| SG-c | **A run's log is masked with its own agency's secrets** — and Central's and the installation's settings; not another agency's, which a run could otherwise probe by printing a guess | `secrets.RedactionValuesFor`; `runner.NewRedactor` with the run's agencies | **Automated:** `secrets.TestRedactionValuesForKeepsOtherAgenciesOut`, `runner.TestRedactMasksRealSecrets` |
| SG-d | **Push-target URLs are sealed** — the URL is the push service's credential | `settings.SealAlertTargetURLs`, the `alert_targets` entry of `EncryptedSettingsColumns`; migration 1610 empties the unread plaintext list | **Automated:** `settings.TestPushTargetURLsAreSealed`, `TestRedactionReportCoversEverySettingsColumn` |
| SG-e | **Every use of a stored secret or SSH key is recorded** — written with the injection audit under the same one-time slot (a failure writes neither and the run does not start); the local runner records the key it connects each host and bastion with; read by the row's agency as its Usage and its secrets report | `runref.RecordUses`; `sshexec` `withKeyUse`; `secret_uses` (migration 1600, retention knob `secretUses`) | **Automated:** `runref.TestAuditInjectionRecordsUses`, `TestAuditInjectionFailsClosedWhenUsesCannotBeRecorded`, `sshexec.TestTheLocalRunnerRecordsTheKeysARunConnectsWith`, `api.TestSecretUsage`, `TestSecretsReport` |
| SG-f | **Who acted is recorded against the agency** — every change-log row says whether the actor was a member of the row's agency, a global administrator with no grant there, someone outside it, or the system; alerts on a secret revealed or changed go to its own agency's rules | `auth.Identity.RelationTo`, `auditlog.WithActorRelation`; `notify` secret triggers | **Automated:** `auth.TestRelationTo`, `TestSecretAlertRulesAreRefusedWhenTheyCouldNeverFire`, `notify.TestSecretAlertsReachTheirAgency` |
| SG-g | **A secret can be made unrevealable** — one way; nobody may reveal it afterwards, global administrators included, before anything is decrypted; runs still use it | `POST /env-secrets/{secretId}/lock-reveal`; `secrets.Service.Reveal` (409 `reveal_locked`) | **Automated:** `api.TestUnrevealableSecrets` |
| SG-h | **An agency's own Vault, and no fallback** — a Vault-backed secret or key resolves in its OWNER's Vault; an agency whose own connection cannot be read or used fails, and is never given the installation's Vault, where its paths could name another agency's secret. The address must be `https` and on the installation's allowed Vault hosts (on save, test and every client build; an empty list allows none). Disconnecting is refused while the agency has Vault-backed rows. A preview reads each reference and returns no value | `secrets.VaultForOwner`; `settings.dynamicVaultClient.resolve`, `CheckVaultURL`; `api/agency_vault_mount.go` (configureApp on the agency) | **Automated:** `settings.TestSG3_ARowResolvesInItsOwnersVault`, `TestSG3_AKeyResolvesInItsOwnersVault`, `TestSG3_AnUnreadableConnectionNeverFallsBack`, `TestSG3_AllowedHostsOnSaveAndOnUse`, `TestSG3_DisconnectRefusedWhileInUse`, `api.TestSG3_AgencyVaultRoutes` (the two-agency cast; no value or credential in any answer) |

Accepted notes:

- **The bind marker is a database row.** Whoever can write the database can
  delete it; the next start then accepts unbound values until it has bound
  them again. Such a writer can already do worse.
- **Encryption of backups is not retroactive.** Snapshots written before keys
  were added stay unencrypted, locally and in the bucket, until deleted.
- **A killed restore** can leave its decrypted copy (`<db>.restore-decrypted`)
  beside the database.
- **An agency's own Vault is reached from the server.** Its AppRole
  credentials are sealed like any other setting, and the allowed-hosts list is
  what keeps an agency's administrator from pointing the server at an
  arbitrary address. Runners still receive the value from the server; they do
  not reach the agency's Vault themselves (declined in the plan, option 1).

## Install-wide and cross-agency gates

An administrator of one agency cannot change the installation or another
agency (GC band, v2.2.2). Before it, most write routes asked only whether the
caller held the permission on *some* agency. 2.3.0 keeps every gate below and
changes what several of them ask, because the objects changed: the built-in
agency, **Central**, is a real agency (what was "no
agency" is Central's, and a row in it was, until 2.4.0, every agency's to use
and a global administrator's to change), every scope, secret,
variable, SSH key, host record, bastion and runner belongs to exactly one
agency, and an agency administers more of its own (its host records and
bastions, its Vault-backed secrets inside its own path prefixes, its agents).

**As of 2.5.0 agencies are independent** (§0a of the plan, built in 2.4.0 and
2.5.0). Central is not a library: nothing Central owns is another agency's
to use, and nothing an agency runs depends on another agency or on Central.
**From 2.5.3 Central runs nothing** (the section below). Every ordinary
agency has its own repository, and Central none; names are
unique within an agency and every reference resolves within it; a move
between agencies is a global administrator's repair; and an agency's rows are
**read** by its own people and by those whose access reaches every agency,
and by nobody else (CF-1). A global administrator owns the installation's
settings and hands a definition to an agency as one commit into its
repository. Rows marked 2.3.0, 2.4.0, 2.5.0 or 2.5.3 say what changed.

| # | Control | Enforced by | Verification |
|---|---|---|---|
| GC-1 | **Install-wide writes need a global administrator** — an unrestricted grant that itself carries the permission: every install-wide setting, the agency catalog, role templates and, from 2.3.0, an agency's Vault path prefixes, the local runner's setting and signing every session out; from 2.4.0 the installation-wide freeze; from 2.5.0 an agency's group list (GA-1), its concurrency cap (QT-1) and giving a definition to an agency (IN-12, `POST /agencies/{agencyId}/assign`). (Bastions and hand-written host records left this list in 2.3.0: they have an owner, GC-7. **2.4.0:** Git sync and scope resync left it: each works on one repository, `configureApp` on its agency (the built-in agency's a global administrator's until 2.5.3, when it lost its repository). **2.5.0:** alert rules left it (AL-3, below), and so did the audit export, now any `configureApp` holder's, filtered to the agencies they administer) | `api.requireGlobal` (`Identity.GlobalAdmin(perm)`) | **Automated:** `TestInstallWideRoutesRefuseAnAdminOfOneAgency` probes every route the table classes as global |
| GC-3 | **"Unrestricted" is never checked without the permission** — a viewer on all scopes who administers one agency is not a global administrator | `requireRoleTemplateAdmin`, `requireGrantWritable`, script bindings, placement | **Automated:** `TestGC_UnrestrictedViewerWhoAdministersOneAgencyIsNotAGlobalAdmin`, `TestAViewerOfEveryScopeCanChangeNothing` |
| GC-4 | **No route decides on a role's name.** **2.4.0 and 2.5.0:** there are no shared authoring surfaces any more. A reusable schedule and a calendar are compose on their owner agency (2.4.0); a definition's reactions compose on its agency (2.4.2); the recycle bin and a definition's revisions compose on the definition's agency, a name resolving among the definitions the caller composes on and a purge by uid (2.5.0, SO-2). Until then they needed compose and configureApp on one unrestricted grant (`requireComposeAdmin`, removed in 2.5.0) | the routes' owner gates (`writeRouteGates`) | **Automated:** `TestGC_SharedAuthoringNeedsComposeAndConfigureOnEveryAgency`, `TestC3_AnAgencyRestoresItsOwnBinnedJobAndSeesNoOther` |
| GC-5 | **A service account is a grant** — mint and revoke obey own-agency, no-all-scopes and no-amplification; the list is filtered | `requireGrantWritable` in `createServiceAccount` / `revokeServiceAccount` | **Automated:** `TestGC_ServiceAccountsFollowTheDelegationRules` |
| GC-6 | **A scope is administered by its own agency** — edit, inventory, delete, tags and runner binding; a new scope is born in its creator's agency, never in Central (2.5.3); a scope left in Central is a global administrator's; a runner replace needs every bound scope. **2.3.0:** a scope has one agency, and moving it is a two-sided act — `configureApp` on the agency it is in (on *every* one, for a scope still shared from before 2.3.0) and on the agency it is going to, with the built-in agency on either side taking a global administrator (from 2.5.3 nothing moves into it, CA-a). Until 2.3.0 any move was a global administrator's. The same rule moves a secret, a variable and an SSH key, and the move carries the row's owner in the same transaction (409 `owner_conflict` on a name the target already owns). **2.4.0:** there are no moves between agencies (IN-7). Any change of an object's agency, a workflow's included, is a global administrator's **repair**, with `repair: true` and a reason, audited; another agency gets an object by a copy (a scope by its clone, a definition by assignment). A scope of a repository is fixed to that repository's agency (409 `scope_agency_fixed`; for the built-in agency's repository from 2.4.3) | `requireScopeAgency`, `requireMove` / `requireRepair`, `requireCreationAgencies`; `settings.SetAgencyMembership` | **Automated:** `TestGC_ScopesAreAdministeredByTheirOwnAgency`, `TestGC_ANewScopeLandsInItsCreatorsAgency`, `TestGC_ReplacingARunnerNeedsEveryBoundScope`, `TestGlobalAgency_OnlyAGlobalAdministratorMovesARowInOrOut`, `TestOneAgency_ASecretMovesWithItsOwner`, `TestI4_AMoveIsAGlobalAdministratorsRepairWithAReason`, `TestI4_AWorkflowChangesAgencyOnlyByARepair`, `TestRV_AScopeOfGlobalsRepositoryIsClonedNotRepaired` |
| GC-7 | **Host and bastion records have an owner** — a record imported for a scope follows that scope. **2.3.0:** a hand-written host record and a bastion belong to one agency (`owner_agency`, migration `1230`): its administrators change it, a global administrator changes Central's, and a new one is born in an agency its author administers (never Central, from 2.5.3). Until 2.3.0 both belonged to nobody, applied to every scope and were a global administrator's alone. **2.4.0:** a scope resolves only its own agency's records, and a host's `via` only its own agency's bastion; the built-in agency's were its runs' only (until then every agency's scopes and records fell back to them), and from 2.5.3 they serve no run | `requireHostOwner`, `requireBastionOwner`, `requireRecordOwnerChoice`; `execspec.HostRecordForScopeSQL`, `execspec.BastionByRef` | **Automated:** `TestGC_HostAndBastionRecords`, `execspec.TestAScopeResolvesItsOwnAgencysHostRecordsAndGlobalsOnly` |
| GC-8 | **A Vault path is named only inside the owner agency's prefixes** — create, edit, migrate and move of a Vault-backed secret or SSH key; the gate asks the store's question (a secret: anything not exactly `stored`, or any path; a key: exactly `vault`, or any reference). **2.3.0:** in 2.2.2 every Vault path was a global administrator's to name. Now a row that is an agency's may name a path inside a prefix a global administrator assigned to that agency (`PUT /agencies/{agencyId}/vault-prefixes`), judged by whole path segments with no `..`, `.`, empty segment or character a URL could decode into one; an agency with no prefix can name none (422 `vault_path_not_allowed`); the rule binds a global administrator too; and the caller needs the permission on the row's *owner* agency, not on any agency a legacy shared row is in. A row that is Central's is still a global administrator's, any path; from 2.5.3 none is created, so a global administrator creates a Vault-backed row in an agency, inside that agency's prefixes | `requireVaultPath`, `internal/vaultpath`; `secretNamesVault`, `credentialNamesVault` | **Automated:** `TestGC_VaultPathsAreAGlobalAdministrators`, `TestGC_VaultGateCannotBeSidesteppedBySourceSpelling`, `TestGC_AVaultBackedSSHKeyIsAGlobalAdministrators`, `TestVaultPrefixes_AnAgencyNamesPathsInsideItsOwnAndNoOthers`, `TestVaultPrefixes_OnlyTheOwnerNamesASharedRowsPath`, `TestVaultPrefixes_AreJudgedAsWrittenAndReplacedOnlyOnPurpose`, `vaultpath.TestUnderMatchesWholeSegments`, `TestMalformedPathsAreRefusedNotRepaired`, `settings.TestNarrowingASharedVaultRowDoesNotGiveAwayAPathItsNewOwnerMayNotName` |
| GC-9 | **Publish is authorized per file** — the scope in the incoming content, the scope of the file it replaces, and any Git job already using the name; schedule and workflow files, unscoped jobs and nameless jobs are a global publisher's. **2.4.0:** a publish names a repository and is that repository's agency's (GR-24): an agency's takes `publishSchedule` on that agency, and for a job file the scope on both sides of the write; the built-in agency's repository was a global publisher's alone, and from 2.5.3 a publish naming no repository is 422 `no_repository`. A publish fetches first, never forces, and is refused when the branch has moved; a push refused for a protected branch is 409 `branch_protected`; one that fails before its push leaves the clone as it found it (2.5.0) | `Server.authorizePublish` | **Automated:** `TestGC_PublishIsCheckedPerFile`, `TestGC_PublishRefusesANamelessJob`, `TestAPublishNamesARepositoryAndIsGatedByItsAgency` |
| GC-10 | **Workflow authorization follows sub-workflows** — compose, trigger, pause and cancel are checked against every job the tree runs, resolved by the function the engine itself uses. **2.4.0:** a workflow's steps are its own agency's (a save that would span agencies is refused for everyone), and a step resolves within that agency only (2.4.2) | `workflow.Engine.JobAgencyScopesAt` / `SubWorkflowJobAgencyScopesAt` / `DescendantRunAgencyScopes` | **Automated:** `TestGC_WorkflowAuthorizationFollowsSubWorkflows`, `TestGC_AnotherAgencysSameNamedWorkflowIsNotAVeto`, `workflow.TestSubWorkflowJobScopesReWalksAtAShallowerDepth` |
| GC-11 | **Cancelling a pending run needs a run verb** on every scope it would touch | `cancelPendingRun` | **Automated:** `TestGC_CancellingAPendingRunNeedsARunVerb` |
| GC-12 | **Tags and annotations need a readable row** | `requireJobVisible`, `requireWorkflowVisible` | **Automated:** `TestGC_TagsAndNotesNeedAReadableRow` |
| GC-13 | **A reaction matches its upstream by identity**, not by a name another agency may share; the name is a fallback only while it is unambiguous. **2.4.0:** a reaction's upstream is its own agency's; a reaction to another agency's definition does not fire and is listed by `reaction_other_agency` | `Scheduler.deliverEvent` | **Automated:** `scheduler.TestReactionMatchesTheUpstreamByIdentityNotName`, `TestReactionNameFallbackNeedsAnUnambiguousName`, `scheduler.TestI1_AReactionToAnotherAgencysDefinitionDoesNotFire`, `api.TestI2_AReactionsUpstreamIsLookedForInItsOwnersAgency` |
| GC-20 | **A notice is read and dismissed by the agency it is about** — a scope-binding notice by its own scope's administrator; **2.3.0:** every kind in the Notices inbox by `configureApp` on the notice's agency, with an install-wide notice filed under Central and so a global administrator's. A dismissal that mixes the caller's own notices with another agency's is refused whole | `handleDismissScopeBindingNotices`; `handleListNotices`, `handleDismissNotices` | **Automated:** `TestGC_DismissingANoticeNeedsItsScope`, `TestNotices_AreReadAndDismissedByTheirAgency` |
| GC-14 | **A push target's URL is read only by those who may change it** — **2.5.0:** a target is one agency's (`/alert-targets`), and its URL, which is its credential, is returned only to `configureApp` on that agency; the relay's settings (`/settings/notifications`) carry no targets at all, and the gateway's address stays a global administrator's. Until 2.5.0 every target was install-wide and its URL a global administrator's | `/alert-targets` handlers; `handleGetNotifications` | **Automated:** `TestGC_NotificationTargetURLsAreMasked`, `TestPushTargetsBelongToAnAgency` |
| GC-21 | **A host uses only a key its agency may use** (v2.2.3) — a host record may name only a key of its scope's agency (until 2.4.0, or one that was the built-in agency's: since then such a key is its own agency's records' and runs' only, `runref.ownerInRunSQL`, and from 2.5.3 Central's serve no run), and the local runner loads every key a run connects with, by id and by name, with the run's agency snapshot, as an agent's manifest does; one refusal for another agency's key and for a missing one. **2.3.0:** the rule covers hand-written host records and bastions too, against their owner agency, and binds a global administrator (a record of Central's may not name an agency's key); a record's own key — a bastion's, a host's under Test connection — is loaded under its owner, so no key is loaded unchecked any more. A record that already names a key its owner may not use is a `record_key_outside_owner` notice | `requireKeyUsableBy`, `sshexec.keyGuard` / `ownerKeyGuard` / `loadSignerChecked`, `runref.KeyIDUsable` | **Automated:** `TestGC_AHostRecordMayNotNameAnotherAgencysKey`, `TestGC_ARecordNamesOnlyAKeyItsOwnerMayUse`, `sshexec.TestKeyGuardChecksAKeyNamedByID`, `TestKeyGuardChecksAKeyNamedByName`, `TestHostKeyGuardFollowsTheRecordsScope`, `TestARunDoesNotConnectWithAnotherAgencysKey` |
| GC-22 | **A run's single target host must be in its scope** — v2.2.3 held only a restricted actor's per-run override to the scope's host list. **2.3.0:** the rule covers the host the job itself declares as well, for every caller, a global administrator included, and on every producer: the run fails that host when its targets are resolved, and a manual or token trigger gets the same answer early (422 `scope_membership`). A job with no scope, or one whose scope lists no hosts (they live in a runner's own inventory), has no membership to ask about; a job already authored against a host outside its scope is a `target_host_outside_scope` notice | `execspec.HostInScope`, in `execspec.ResolveTargets` and `runJobWithKind` | **Automated:** `TestGC_ARunsTargetHostMustBeInTheScope`, `execspec.TestAFixedTargetHostMustBeAMemberOfTheScope` |
| GC-23 | **Reference bindings need the permission on the job's own scope** (v2.2.3), from one grant; an unscoped job's are a global administrator's | `putJobBindings` | **Automated:** `TestGC_ReferenceBindingsNeedThePermissionOnTheJobsScope` |
| GC-24 | **Pausing or resuming a job with no scope needs the verb unbound** (v2.2.3) — a job with no scope is its agency's (and from 2.5.3 so is its run), but pausing it still takes `killJobs` unbound: `killJobs` on one agency is not authority over it; a job in a scope is unchanged | `requirePauseAuthority` | **Automated:** `TestGC_PausingAJobWithNoScopeNeedsTheVerbUnbound` |
| GC-18 | **Every write route is classified** — a route that is not a GET cannot be registered without an entry saying who may call it | `writeRouteGates` | **Automated:** `TestEveryWriteRouteIsClassified` |
| CF-1 | **The reading rule** (2.5.0) — an agency's rows are read by its own people and by those whose access reaches every agency (a global administrator, a viewer of every agency); Central's by the latter only. Every list adds it to its own filter, the Activity and change logs included (each row is its object's agency's, migration `1520`); a single object the caller may not read answers **404**, not 403; a schedule or calendar named by a reference resolves among readable agencies only. Until 2.5.0 the built-in agency's rows were every session's, and several lists answered every agency's rows to anyone signed in | `agencyReadFilter`, `notReadable` (`api/audit_agency.go`, `api/authz.go`) | **Automated:** `TestEveryGetRouteIsClassified` (fails on a GET route with no class), `TestC2_OneObjectOfAnotherAgencyReadsAsAbsent`, `TestC2_TheListsOutsideTheSwitcherPinAnswerByAgency`, `TestC2_TheSettledRoutesAnswerTheirClass` |
| CF-5 | **The audit export is per agency** (2.5.0) — any `configureApp` holder may export, and gets the rows of the agencies they administer, without sign-in events; a global administrator's export holds every row. Rows and stream lines carry `agency` | `/audit/export` handler | **Automated:** `TestC1_AnAgencyAdministratorExportsTheirAgencysRows` |
| AL-4 | **An agency's push target names no generic webhook or relay** (2.5.0 C12, defect 95, questions 112 and 117) — an agency's administrator could aim a target at Apprise's `json`/`xml`/`form` webhooks or its `apprise` relay, which the installation's gateway sends to any address it reaches, with `+Header=` values of the URL's choosing (a cloud metadata service, Vault on the loopback): the server's egress guard never sees it, as the gateway makes the request. Refused (422 `target_scheme_not_allowed`) unless a global administrator writes it; Central's targets are not limited. The scheme is found where Apprise finds it, not by splitting the URL. **Residual, documented:** schemes of self-hosted services (`ntfy`, `gotify`, `mmost`, `rocket`, `nextcloud`, `nextcloudtalk`, `synology`, `signal_api`, `enigma2`, `notica`, `evolution`, …) still take the host an agency names, and several accept `+` headers; under the cooperating-departments model that is an administrator's deliberate act. Narrow the gateway's own egress where it matters (an allowlist of schemes is a 2.5.1 option) | `pushTargetSchemeAllowed`, `genericPushScheme` | **Automated:** `TestC12_AnAgencysPushTargetNamesNoGenericWebhook` |
| AL-3 | **Alert rules and push targets are an agency's** (2.5.0, migrations `1530`, `1540`) — written with `configureApp` on their owner; an agency's rule matches only its own agency's runs and names only its own agency's job and targets (422 `unknown_job`, `unknown_target`); a rule of Central's matches every agency's (from 2.5.3 an alert rule and a push target are the only creations Central may still own); a push goes to the targets the matched rule names, no longer to every target; the relay's default recipients are on Central's rules' mail only. Until 2.5.0 every rule was install-wide and a global administrator's | `requireRecordOwnerChoice`; `notify.Dispatcher.resolveEventAgency` | **Automated:** `TestAlertRulesBelongToAnAgency`, `TestPushTargetsBelongToAnAgency` |
| GA-1 | **An agency's delegates grant only to listed groups** (2.5.0, migration `1550`) — a global administrator lists the directory groups each agency's grants may name (`PUT /agencies/{agencyId}/groups`); an agency's administrator of access grants only to a listed group, matched exactly (403 `group_not_listed`); a global administrator is not bound. The lists start empty. A grant made before, or naming a group since unlisted, keeps working, may be deleted but not changed by a delegate, and is listed by `grant_group_unlisted` | `requireGroupListed` | **Automated:** `TestADelegateGrantsOnlyToListedGroups`, `TestAnAgencysGroupList` |
| IN-12 | **A definition reaches another agency only as a commit into its repository** (2.5.0) — a global administrator's `POST /agencies/{agencyId}/assign` writes the job or workflow and what it needs that the agency lacks as one commit (new files only, fetched first, no force, the clone put back on failure), each file stamped `metadata.origin`; the agency's sync and its own review then see it like any other change. Bindings, credentials and calendars it lacks are listed or refuse the assignment; nothing is created in the app | `gitlab.Registry.Assign`, `gitlab.CommitFiles` | **Automated:** `gitlab.TestAnAssignmentIsOneCommitCarryingTheWholeSet`, `TestAWorkflowsAssignmentCarriesWhatItRuns` |
| — | **`/settings/gitlab` is gone** (2.5.0) — `GET`/`PUT /settings/gitlab` and `POST /settings/gitlab/webhook-secret/rotate`, which addressed the built-in agency's repository, are removed; its repository became `/agencies/global/git-repo` like every other agency's, under the same gate. **2.5.3:** Central has no repository: `GET` shows only a leftover (`leftover: true`, else 404 `not_connected`), `PUT` is 422 `central_runs_nothing`, and `DELETE` removes a leftover as a global administrator's repair with a reason | `api/settings_mount.go` | **Automated:** `TestTheSettingsGitlabRoutesAreGone` |

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
  Triggers give every new scope, secret, variable and key a row in the
  built-in agency until an agency is named (a runner is born serving its
  owner) and refuse the built-in agency beside a named one; the setters refuse to leave a row in neither
  (422 `agency_required`) or in both (422 `global_mixed`). Where a row is
  nevertheless found with no agency, the per-object gate answers 500, dispatch
  matches nothing and the producer refuses, rather than reading it as
  "everyone's"; a global administrator re-homes it. No grant or service
  account may name Central — the way to be a global administrator is an
  all-agencies grant. **Automated:**
  `TestGlobalAgency_ARowIsInGlobalOrADepartmentNeverBothNeverNeither`,
  `TestGlobalAgency_ARowInNoAgencyIsAFaultNotGlobal`,
  `TestGlobalAgency_CannotBeGranted`,
  `execspec.TestAScopeWithNoAgencyIsAnErrorNotGlobal`.
- **Rows shared by several agencies before 2.3.0 stay shared until someone
  settles them.** One agency per row is a writer rule, not a constraint on old
  data: such a row keeps its memberships, settling it is a global
  administrator's repair (since 2.4.0), and a `scope_several_agencies` notice
  lists the scopes. Since 2.4.0 a credential, a key and a record are usable by
  their **owner's** runs only (`runref.ownerInRunSQL`), whatever memberships
  they kept; what agencies shared before 2.4.0 stays the built-in agency's
  (and from 2.5.3 runs nothing) and is listed in the Notices inbox for an administrator to copy into each agency that needs
  it (IN-14), not migrated.
- **No object is shared any more** (2.4.0 and 2.5.0). A schedule and a
  calendar are their agency's, and so are an alert rule and a push target
  (2.5.0). What still acts on every agency is an alert rule of Central's,
  which matches every agency's runs, and the installation's settings (the freeze,
  the concurrency ceiling, retention, sign-in, the relay); both are a global
  administrator's to write.
- **A sub-workflow is authorized as it resolves at that moment.** A child that
  is disabled when its parent is triggered and enabled before its step is
  reached runs on an authorization that did not see it.
- **Reads follow the reading rule** (CF-1, 2.5.0). Until 2.5.0 activity, the
  change log, workflow definitions, schedules and script bodies were
  readable by any session. What is still readable by any session is what the
  route classification marks `any-session`: the caller's own context, the
  agency catalog, role templates, and five installation settings (general,
  the freeze, audit and compliance, the local runner, the relay) —
  `TestEveryGetRouteIsClassified` holds the list.
- **A global administrator's assignment is a commit into the agency's
  repository, and the agency keeps the last word.** Giving a definition again
  replaces only what changed at the origin and is refused where the agency
  changed its copy (`changed_by_agency`); an update is announced to the
  agency (`assignment_update_available`), never pushed unasked.
- **A workflow has no token opt-in.** A service account can trigger any
  workflow its grant covers; only jobs have `requestable`.

## Central administers, agencies run (2.5.3)

The built-in agency, Central, is the installation's administration and runs
nothing (CA-1 to CA-11 in the plan). It keeps the installation settings, the
agency catalog, global administrators, the installation's change log,
Activity and notices, the installation freeze, its alert rules (which match
every agency's runs) and push targets, and the calendar catalogue, which no
run reads. It owns no scope, runner, registration token, schedule, script,
job, workflow, reaction, secret, variable, SSH key, host record, bastion or
repository connection. Nothing moves automatically at the upgrade: what
Central owned stays, does not run, and is listed by the `central_holds_work`
notice with the way out for each kind.

| # | Control | Enforced by | Verification |
|---|---|---|---|
| CA-a | **Nothing that runs is created in Central, or moved into it** — every creation that chooses an owner refuses Central whether it is named, defaulted, or the caller works in Central (ids de-duplicated first, so naming it twice is one id), except an alert rule and a push target; a job with no scope, or on a scope no agency of the caller's holds, is refused as Central's; a new workflow, or an edit that would make one Central's; the schedule composer; a move, a repair or a scope clone into Central. The refusal comes before the permission check: an agency administrator who names Central gets 422 where it was 403, and nothing is disclosed. A registration token is a creation like the others | `api.failCentralRunsNothing` (422 `central_runs_nothing`), `requireCreationAgencies` with `centralMayOwn`, `composeOwnerFor`, `workflowSaveOwner`, `requireMove` | **Automated:** `api.TestCA1NothingThatRunsIsCreatedInCentral`, `TestCA1NothingMovesIntoCentral`, `TestCA1AScopelessJobComposedInAnAgencyIsThatAgencys`, `TestCA1ARestrictedComposerStillCannotAuthorAScopelessJob`. **Operator:** as a global administrator, `POST /api/v1/runners/registration-tokens` with `{"agencyId":"global"}` answers 422 `central_runs_nothing` |
| CA-b | **What Central owns does not run** — a definition whose owner is Central is refused on every origin: the gate (`Gate central`; a person's Run and a token trigger 409 `run_refused`, a promotion holds the parked row), the cron fire of a job and of a workflow (a skipped run recorded with the reason), a reaction (`suppressed_disabled` with its own detail) and the manual workflow trigger, deferred or not. Missed-run detection leaves Central's definitions out | `scheduler.DefinitionIsCentral` (true only when the definition exists and its owner is `global`; a live definition before a binned one) in `Evaluate`, `fire`, `fireWorkflow`, the reaction path; `scheduleEntriesForDetection` | **Automated:** `api.TestCA1ALeftoverOfCentralsIsRefusedWhenRun`, `scheduler.TestCA1ADefinitionOfCentralsIsNotWatchedForMissedFires` |
| CA-c | **A run's agency comes from its definition** — a run with no scope, or a scope its agency does not hold, is stamped with its job's agency; the claim mirrors ask for that agency's agent, a scopeless workflow step is inside its workflow's agency, and an unbound run's credentials resolve in that agency only (422 `scope_required`: "this job consumes credentials its run cannot use … create it in the job's agency, or bind the job to the scope it belongs to") | `execspec.ScopeAgencies` (`runAgencyName`), `runref.CheckoutNeedsAgent`, `runref.KeyBindingsNeedAgent`, `runref.StepOutsideWorkflowRepository`, `runref.UnboundRefusal` | **Automated:** `runref.TestCheckoutNeedsAgentForAnAgencysScopelessRun`, `TestRefuseDispatchAScopelessStepOfAnAgencysWorkflow`, `TestUnboundRefusalNamesTheReference` |
| CA-d | **The database refuses to make a row Central's** — migration `1670`: `*_not_made_central` triggers, BEFORE UPDATE OF `owner_agency` on secrets, variables, SSH keys, host records, bastions, runners (not the local runner), schedules, workflows and jobs, abort when the new owner is `global` and the old one a real agency. Deliberately the narrow half (the owner's choice): the app's refusals (CA-a, CA-b) are the control, this is a backstop | `db/migrations/1670_central_runs_nothing.up.sql` | **Automated:** `db.TestMigrate1670CentralRunsNothing` (up, down, up) |
| CA-e | **Central has no repository, and the environment configures none** — the registry starts no Service for `global`; Central's repository row is kept as an inert record; `CRONOMICON_GITLAB_*` are not read, and a set one raises a start-up warning and the `environment_settings_ignored` notice; the id-less webhook answers 401; a checkout run that records no repository, or Central's, is refused (409 `no_checkout_repo`); a leftover is removed only by a global administrator's repair, with a reason, and refused while one of its scopes has work queued | `gitlab.Registry` (`restart`, `repoIDs`), `settings.RepoOfAgency`, `PutAgencyRepo` (`ErrCentralHasNoRepository`), `settings.EnvSettingsIgnored`, `gitlab.RemoveCentralLeftover` | **Automated:** `api.TestCA2CentralHasNoRepository`, `TestCA2CentralsLeftoverRepositoryIsRemovedByARepair`, `TestCA2ALeftoverIsNotRemovedWhileItsScopeHasWorkQueued`, `TestCA2ADefinitionInTheLeftoverIsNotAssignedFrom`, `settings.TestGR2_TheEnvironmentOverridesAreReadByNoRepository`, `gitlab.TestWebhookSecretsOfTheEnvironmentOpenNoRepository` |
| CA-f | **Central enrols no agent** — the environment bootstrap token is refused (401 `bootstrap_token_retired`); a registration token minted for Central before 2.5.3 enrols nothing and is not consumed (401 `token_for_central`); the API mints none for it (CA-a). An agent serving Central alone takes no work, since a runner claims its owner's runs only | `runner.checkRegistrationToken` (`runner/token.go`); the claim's owner join (`runner/poll.go`) | **Automated:** `runner.TestBootstrapTokenUnchanged`, `runner.TestRegistrationToken`, `runner.TestALegacyPlacementClaimsOnlyItsOwnersRuns`. **Operator:** an agent started with the old environment token logs `register: server returned 401 …` naming `bootstrap_token_retired`; unset the variable on the server |
| CA-g | **The local runner belongs to one agency, or to none and is off** — a global administrator gives it an agency; unassigned it is off and cannot be turned on; a change of agency is refused while it runs a run or while scopes are bound to it, deletes its host-key ledger, pending keys and scan requests, turns it off, and is recorded for the installation and both agencies; that agency's administrators then approve the keys of its hosts. `CRONOMICON_LOCAL_RUNNER=forbid` still wins | `settings.SetLocalRunnerOwner`, `settings.LocalRunnerEnabled`, `PUT /local-runner` (`requireGlobal(configureApp)`), `requireRunnerOwner` | **Automated:** `settings.TestCA3TheLocalRunnerIsGivenToOneAgencyOrNone`, `api.TestCA3TheLocalRunnersAgencyReviewsItsHostKeys`, `api.TestCA3AChangeOfTheLocalRunnersAgencyIsJudgedWhole`. **Operator:** after the upgrade, `GET /api/v1/local-runner` shows `owner` null and the runner off until a global administrator gives it an agency |
| CA-h | **What Central still holds is listed until it is gone** — one finding per kind (jobs, workflows and schedules built in the app, in-app scopes, secrets, variables, SSH keys, hand-written host records, bastions, agents serving Central alone, the leftover repository), each with the rows and the way out; each clears when Central holds none of its kind. Moving a secret, variable or key out is a global administrator's repair with a reason | `notices.checkCentralHoldsWork` (`central_holds_work`) | **Automated:** `notices.TestCA4WhatCentralHoldsIsListedAndClearsWhenItIsGone`, `TestCA4RetiredNoticesClear` |

Accepted notes:

- **The database backstop is narrow on purpose** (CA-4, the owner's choice).
  Migration `1670` refuses a change of owner to Central; it does not refuse a
  row **born** with no owner and filled in as Central's by the birth
  triggers of `1220` and `1400`/`1420`. The app never writes such a row
  (CA-a), and one written past the app (raw SQL, some 1,150 test inserts) is
  refused when it would run (CA-b). Nor does it cover scopes (their owner
  follows `scope_agencies` through transit triggers, and the app refuses the
  move), calendars (Central's are the catalogue, which no run reads), the
  local runner (Central's while unassigned, and then off), or a row that is
  Central's already (a leftover may be edited, deleted and moved out).
- **A Git job whose repository is gone has no owner.** The definition check
  neither refuses it nor stamps it Central's; its run is stamped the
  built-in agency's by the old fallbacks, and only an agent of Central's
  left from before 2.5.3 could take it (left as found in CA1; CA2 removed Central's repository,
  and `central_holds_work` lists leftovers).
- **Runs already queued, and steps of a Central workflow already running, at
  the upgrade go on.** The refusals act on new starts.
- **The local runner's approvals are an agency's to write** (class B,
  documented, not engineered): see the host-key bullet under *Residual risks*.
  Between a committed change of the local runner's agency and the engine
  being told there is a window of one claim.

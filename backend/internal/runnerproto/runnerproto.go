// Package runnerproto holds the runner↔server JSON wire types and the protocol
// version, so the server (internal/runner) and the future agent
// (cmd/cronomicon-runner) share one contract and can't silently drift
// (runners-update.md R0.2 / D3 — the agent imports this package directly).
//
// Wire format is load-bearing: the json tags here are the on-the-wire field
// names. Changing a tag is a breaking protocol change and must bump
// ProtocolVersion.
package runnerproto

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// ProtocolVersion is the wire-protocol version this server/agent speaks.
//
//	v1 — base manifest/poll contract.
//	v2 — adds the optional ManifestResponse.Inventory + .Limit fields (Ansible
//	     inventory support). Both are omitempty, so a v2 server talking to a v1
//	     agent is still wire-compatible (the agent drops the unknown keys); the
//	     server gates inventory-required ansible runs on the agent being >= v2
//	     (manifest.go) so an old agent never silently runs unscoped.
//	v3 — adds ManifestResponse.Checkout (runner-side pinned checkout of a
//	     playbook PROJECT at a server-pinned SHA — ansible-update.md RX.1/RX.2)
//	     and .EnvPassthrough (scoped-env allowlist, RX.9; shipped since v2 as an
//	     additive back-compat field). Checkout is omitempty; the server gates a
//	     checkout run on the agent being >= v3 (manifest.go) so an old agent
//	     never silently ignores the checkout spec and runs the display Body.
//	v4 — adds the "re-register" poll control op + the id-preserving redeclare
//	     endpoint (POST /runners/{id}/redeclare, runner-key auth) it triggers
//	     (runner-install-update.md Phase 4). On receipt the agent re-declares
//	     its CURRENT local config in place — same runner id, same API key, no
//	     orphan row — killing the deregister/re-register Resync dance. The
//	     server sent the op only to runners registered with protocol >= 4
//	     (poll.go) — one of the per-feature gates retired in v1.5.40 once the
//	     floor caught up. Additive within v4 (Phase 5): the
//	     poll request may carry a configDigest query param (ConfigDigest
//	     below) enabling automatic drift detection — absent means a
//	     pre-Phase-5 agent and the server simply skips the check, so no
//	     version bump is needed. Also additive within v4 (plan 2 Phase 4):
//	     the poll response may carry an optional Settings object and the poll
//	     request a settingsVersion ack param — server-managed operational
//	     settings. Same convention: the server only sends Settings to an agent
//	     whose poll carried the settingsVersion param, so an older agent is
//	     untouched (and encoding/json would drop the key anyway).
//	v5 — adds the "keyscan" and "trust-hosts" poll control ops (plan 2 Phase 5,
//	     host-key scan & approve). keyscan carries a Hosts list the agent scans
//	     from its own vantage, uploading what it saw to POST /runners/{id}/
//	     hostkeys; trust-hosts carries approved known_hosts Entries the agent
//	     appends to its known_hosts. Both are semantics an old agent must not
//	     half-have (it would silently drop the op and strand the operator), so
//	     unlike the additive settings/digest extensions this is a real version
//	     bump: the server gates both ops on protocol_version >= 5 (keyscan.go),
//	     the v2/v3/v4 gate precedent.
//	v6 — adds the manifest Secrets block (vault-integration.md P1.4): dispatch-time
//	     resolved CRONOMICON_SECRET_*/CRONOMICON_VAR_* reference values shipped to a
//	     secret-injection-flagged runner. An older agent would ignore the field and
//	     run WITHOUT the secrets the job declared, so — like the v2/v3 gates — the
//	     server hard-failed such a run below v6 (manifest.go), until the floor
//	     replaced every one of those gates in v1.5.40. The Keys field ships
//	     alongside (reserved for D8 key delivery).
//	v7 — adds ManifestResponse.SSHUser / .SSHKeyRef: the run's frozen "connect as"
//	     identity override carried EXPLICITLY for a local-toolchain (ansible) run
//	     (run-parity.md RP-8). The ssh-family path applies the same override by
//	     rewriting ManifestTarget.User/AuthKeyEnvVar, which an agent of any version
//	     honors; ansible cannot use that channel — the agent builds an argv rather
//	     than dialing the targets itself, so it needs to be TOLD the override is an
//	     override in order to emit `-e ansible_user=…` (RP-Q1: extra-vars beat
//	     inventory-authored identity; CLI -u would lose to it). An older agent would
//	     drop the unknown keys and run as the INVENTORY's identity while the UI
//	     showed the operator's — running as the wrong user, silently. So, like the
//	     v2/v3/v6 gates, the server hard-fails an identity-carrying ansible run
//	     assigned to a protocol < 7 agent (manifest.go) rather than shipping a lie.
//	v8 — adds ManifestResponse.AnsibleOptions: the per-run advanced ansible flags
//	     (check/diff/tags/skip-tags/verbosity/become/extra-vars) an operator set at
//	     trigger time (run-parity.md Phase 3). Gated for the same reason as v2 and
//	     v7, and more sharply: an older agent drops the field, so a run the operator
//	     asked to DRY-RUN (--check) would apply for real while the console labelled
//	     it a check run — and a --skip-tags exclusion would run the very task it was
//	     meant to skip. Rather than encode which of the flags are dangerous (a matrix
//	     that rots as flags are added), the server refuses ANY option-carrying run
//	     assigned to a protocol < 8 agent. A run carrying no advanced option is wire-
//	     and behavior-identical to a pre-v8 run and stays claimable by any agent.
//	v9 — adds ManifestResponse.SecretFiles and .BecomePasswordRef (run-as plan
//	     RA-12): secrets delivered as 0600 FILES off the run tree rather than as
//	     environment values, and the reference whose materialized path the agent
//	     passes to `ansible-playbook --become-password-file`. Gated like v6 and for
//	     a sharper reason than v8: an older agent drops both fields, so a run that
//	     declared a become password would run WITHOUT it — and `become: true` with
//	     no password does not fail cleanly, it hangs at a sudo prompt or escalates
//	     as whatever passwordless rule happens to exist. Rather than let that
//	     happen the server refuses a file-bearing run assigned to a protocol < 9
//	     agent. A run carrying no secret files is wire- and behavior-identical to a
//	     pre-v9 run and stays claimable by any agent.
//
//	     Claim-gating comes FIRST though (RA-20a): a job with a become password
//	     declares the `become-file` requirement token, which only a v9 agent with an
//	     ansible-core new enough for the flag advertises. So such a run WAITS for a
//	     capable runner rather than being assigned to an old one and 409ing — with
//	     one runner the two are indistinguishable, with five a manifest-time refusal
//	     burns an assignment a capable runner could have taken.
//	v10 — adds PollResponse.Watches (ET-D): the file-arrival watch specs this
//	      agent should poll. Gated for the same reason as v6 and v9 rather than
//	      merely additive: an older agent drops the field silently, so an
//	      operator who authored a watch would see a job that is configured to
//	      run on arrival and simply never runs — the file lands, nothing
//	      happens, and nothing anywhere says why. A missing capability is
//	      visible (the server records that no eligible runner can watch); a
//	      dropped field is not.
//
//	      Watch specs are sent on EVERY poll rather than as a one-shot control
//	      op, because they are standing configuration: a job's watch edited in
//	      Git must reach the agent without an operator action, and a restarted
//	      agent must re-acquire the full set with no replay. That is the
//	      PollSettingsValues pattern, not the keyscan pattern.
//	v11 — adds WatchSpec.JobUID (R2-3/R2-4, the rbac2 plan): the
//	      permanent identity of the job a watch belongs to, echoed back with the
//	      sighting. Purely ADDITIVE and NOT gated, unlike v10: a v10 agent omits
//	      the field and the server falls back to the (jobSource, jobName) pair,
//	      which stays exactly correct for as long as names are unique. So there
//	      is nothing an older agent silently fails to do here — the v10 gate's
//	      reasoning does not apply.
//
//	      It exists because the watch echo is the ONE place in this protocol
//	      where a CLIENT-SUPPLIED NAME selects which job the server runs. Once
//	      per-agency naming lands, that pair stops identifying one job, and a
//	      file landing in one department's directory could start another's job
//	      of the same name. The uid is what keeps the echo unambiguous; the
//	      final AF-4b stage turns the name fallback into a refusal when the pair
//	      matches more than one job.
//	v12 — adds PollAssignment.LiveLog + the X-Log-Partial ingest header (live
//	      log tailing). A LiveLog assignment invites the agent to flush its
//	      pending log buffer to POST /runs/{id}/log periodically DURING the run,
//	      marking each mid-run POST with `X-Log-Partial: 1` so the server
//	      persists + advances the offset without treating the chunk as the whole
//	      stream (an unmarked chunk whose last line is not the trailing envelope
//	      finalizes the run log_stream_lost — the pre-v12 contract, unchanged).
//	      Purely ADDITIVE and NOT gated, in BOTH directions: an old agent drops
//	      the unknown field and keeps its single sealed end-of-run flush; a new
//	      agent talking to an old server never sees LiveLog and so never sends a
//	      partial chunk the old server would misread as a lost stream. The flag
//	      rides the assignment (not a capability handshake) because that is the
//	      one message where the server can say, per run, "I understand partial
//	      chunks for this trace id".
//	v13 — the rebrand (RN). The injected run namespace prefix, the output
//	      marker (::cronomicon-output), the token prefixes (crn_/crnsvc_) and
//	      the agent binary name (cronomicon-runner) all changed. No wire
//	      SHAPE changed; the bump exists so an agent built before the rename
//	      is refused at registration (426) instead of assembling a run env
//	      and scanning for a marker that no script emits any more.
const ProtocolVersion = 13

// MinProtocolVersion is the oldest agent protocol this server accepts, checked
// at registration and redeclare (426 protocol_too_old). It TRACKS
// ProtocolVersion: server and agent ship from one repository and the runner
// upgrade command moves a fleet with its server, so the per-feature back-compat
// gates that a lower floor needed (the v2..v10 `agent_too_old` refusals on
// manifest, keyscan, resync and file-watch delivery) were deleted in the DD
// band (v1.5.40). Everything past registration may assume the current wire
// shape. An absent protocolVersion counts as 0 and is refused the same way.
const MinProtocolVersion = ProtocolVersion

// ConfigDigest is the canonical SHA-256 over a runner's DECLARED config set
// (runner-install-update.md Phase 5, D2 stage 2): name, os, capability tokens,
// maxConcurrent, inventory, version, protocolVersion. The agent computes it
// from its current local config at startup/redeclare and sends it with every
// poll; the server computes it from the SAME declared values at (re-)
// registration and stores it on the row — a poll-time mismatch means the
// server's view has drifted, and it requests a re-register.
//
// Canonicalization rules — both sides MUST agree by construction, so the
// server-side normalization is baked in here:
//   - capabilities are sorted (order-insensitive) — the agent's detection
//     order is not stable across runs;
//   - maxConcurrent <= 0 normalizes to 5 (the register handler's default);
//   - inventory "" normalizes to "cronomicon" (D8 default);
//   - fields are newline-framed with a field prefix so values can't bleed
//     into each other ("ab"+"c" != "a"+"bc").
func ConfigDigest(name, os string, capabilities []string, maxConcurrent int,
	inventory, version string, protocolVersion int) string {

	caps := append([]string(nil), capabilities...)
	sort.Strings(caps)
	if maxConcurrent <= 0 {
		maxConcurrent = 5
	}
	if inventory == "" {
		inventory = "cronomicon"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "name=%s\n", name)
	fmt.Fprintf(&b, "os=%s\n", os)
	fmt.Fprintf(&b, "capabilities=%s\n", strings.Join(caps, ","))
	fmt.Fprintf(&b, "maxConcurrent=%d\n", maxConcurrent)
	fmt.Fprintf(&b, "inventory=%s\n", inventory)
	fmt.Fprintf(&b, "version=%s\n", version)
	fmt.Fprintf(&b, "protocolVersion=%d\n", protocolVersion)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// PollResponse mirrors the openapi.yaml PollResponse schema — the body the
// long-poll endpoint returns to a runner.
type PollResponse struct {
	Assignment  *PollAssignment `json:"assignment"`
	Control     []PollControl   `json:"control"`
	PollAfterMs int             `json:"pollAfterMs"`

	// Settings carries server-managed operational settings the agent applies
	// in-memory (runner-install-update-2.md Phase 4, D3/D5 — additive within
	// v4). omitempty + only sent to an ack-capable agent (one whose poll carried
	// a settingsVersion query param), and re-sent every poll until the agent
	// acks the current version. A pre-Phase-4 agent never sends the param, so it
	// never receives this — and even if it did, encoding/json drops the unknown
	// key, so behavior is unchanged (no version bump needed).
	Settings *PollSettings `json:"settings,omitempty"`

	// Watches are the file-arrival specs this agent should poll (ET-D, v10).
	// Sent on EVERY poll, so an edited or removed watch reaches the agent
	// without an operator action and a restarted agent re-acquires the full set
	// with no replay. An empty list means "watch nothing" and is meaningful:
	// it is how a deleted watch is retracted.
	//
	// Sent only to a v10+ agent that advertises the `watch` capability. An older
	// agent would drop the field and silently never watch, so the server records
	// the shortfall against the job instead (see watchEligibility).
	Watches []WatchSpec `json:"watches,omitempty"`
}

// PollSettings is the versioned managed-settings payload. Version is the
// server's current settings_version; the agent echoes the version it has
// APPLIED as the poll's settingsVersion param, and the server stops sending
// once acked == Version.
type PollSettings struct {
	Version int                `json:"version"`
	Values  PollSettingsValues `json:"values"`
}

// PollSettingsValues is the tri-state override set. A non-nil field means the
// server has an opinion (override the agent's local value); nil means "no
// opinion — keep the local/declared value". CapabilityMask is subtract-only:
// the run-types it lists are removed from the runner's effective claim set
// (server-enforced at claim; honored agent-side for symmetry). These values do
// NOT feed the config digest — the digest hashes DECLARED config, so a managed
// override never triggers drift re-registration.
// WatchSpec is one file-arrival watch the agent should poll (ET-D, v10).
//
// The agent POLLS rather than using inotify (decision, 2026-08-11): the
// stability window below already requires polling, and inotify frequently never
// fires on NFS/CIFS/SMB — which is exactly where drop directories live. A
// mechanism that silently does nothing on network mounts is worse than a
// slightly slower one that works everywhere.
type WatchSpec struct {
	// JobSource/JobName identify the job to start. The agent echoes them back
	// with a sighting; it never resolves a job itself.
	JobSource string `json:"jobSource"`
	JobName   string `json:"jobName"`
	// JobUID is that job's permanent identity (v11). The agent echoes it back
	// untouched, exactly as it does the pair above, and the server prefers it
	// when resolving which job an arrival starts — the pair remains the
	// fallback for a v10 agent, and stays correct while names are unique.
	//
	// This field is the reason v11 exists: everywhere else in the protocol a
	// name is display only (a run is keyed by trace id), but here a value the
	// CLIENT hands back chooses which job the server runs.
	JobUID string `json:"jobUid,omitempty"`
	// Path is a glob (filepath.Match syntax) evaluated on the agent's own
	// filesystem. The agent refuses any path outside its -watch-paths allowlist,
	// so the server cannot direct a runner to read an arbitrary directory.
	Path string `json:"path"`
	// StableSeconds is how long a file's size must stay unchanged before it
	// counts as arrived. Zero means the server's default. It exists because a
	// large file being written is visible long before it is complete, and firing
	// on a half-written CSV is the failure this feature would otherwise cause on
	// its first day.
	StableSeconds int `json:"stableSeconds,omitempty"`
}

type PollSettingsValues struct {
	MaxConcurrent    *int      `json:"maxConcurrent,omitempty"`
	SandboxMemoryMax *string   `json:"sandboxMemoryMax,omitempty"`
	SandboxCPUQuota  *string   `json:"sandboxCpuQuota,omitempty"`
	SandboxTasksMax  *string   `json:"sandboxTasksMax,omitempty"`
	AllowCheckout    *bool     `json:"allowCheckout,omitempty"`
	CheckoutRepos    *[]string `json:"checkoutRepos,omitempty"`
	CapabilityMask   []string  `json:"capabilityMask,omitempty"`
}

// PollAssignment is returned when a run is claimed.
type PollAssignment struct {
	TraceID string         `json:"traceId"`
	JobName string         `json:"jobName"`
	RunType string         `json:"type"`
	Scope   string         `json:"scope"`
	Payload map[string]any `json:"payload"`
	// LiveLog (v12) tells the agent this server accepts mid-run partial log
	// chunks (X-Log-Partial: 1) for this run, enabling live tailing. Absent on
	// an older server, so a v12 agent falls back to the single sealed flush.
	LiveLog bool `json:"liveLog,omitempty"`
}

// PollControl carries server→runner messages (kill, drain, re-register, and the
// v5 host-key ops). The extra payload fields are omitempty, so an op that
// doesn't use them stays wire-clean.
type PollControl struct {
	Op      string  `json:"op"` // "kill" | "drain" | "re-register" (v4) | "keyscan" | "trust-hosts" (v5)
	TraceID *string `json:"traceId,omitempty"`
	// Hosts is the target list for a "keyscan" op — the agent scans each from
	// its own vantage and uploads the presented host keys (v5).
	Hosts []string `json:"hosts,omitempty"`
	// Entries is the list of approved known_hosts lines for a "trust-hosts" op —
	// the agent appends each to its known_hosts file (v5).
	Entries []string `json:"entries,omitempty"`
}

// TrailingEnvelope is the JSON object the runner appends as the last line of the
// log stream (§6.3). exitCode -1 means the field was absent.
type TrailingEnvelope struct {
	ExitCode   int    `json:"exitCode"`
	DurationMs int64  `json:"durationMs"`
	EndedAt    string `json:"endedAt"`
	// Reason, when set, is a short terminal-failure code the server stores as the
	// run's status reason (v5). Used for host_key_unverified so the run detail
	// can offer "Scan & approve host keys". omitempty keeps old envelopes clean.
	Reason string `json:"reason,omitempty"`
}

// ManifestResponse is the body of GET /runs/{traceId}/manifest (R1.1/R1.2,
// D2). Fetched by a runner right after it claims a run, it carries everything
// the agent needs to execute — resolved command + env + targets — with one
// hard rule (D1, credential model b): host *references only*. AuthKeyEnvVar is
// the NAME of the env var holding the private key, never the key bytes. Cronomicon
// never decrypts or ships key material; the agent holds its own keys.
type ManifestResponse struct {
	TraceID  string   `json:"traceId"`
	JobName  string   `json:"jobName"`
	RunType  string   `json:"runType"`
	Executor string   `json:"executor"`
	Interp   []string `json:"interp"`
	Body     string   `json:"body"`
	// Env is the run's env snapshot (runs.env_json), parsed to a map. These are
	// plain config values, not secrets — secrets are referenced by env-var name
	// via the host's AuthKeyEnvVar and resolved by the agent (D1).
	Env map[string]string `json:"env"`
	// InventoryMode is the owning runner's inventory canonicality (D8):
	//   "cronomicon" — Targets is fully resolved from scope_hosts→ssh_hosts.
	//   "local"   — Targets is empty; the agent resolves Scope against its own
	//               inventory (T-b network-isolated segments).
	InventoryMode  string           `json:"inventoryMode"`
	Scope          string           `json:"scope"`
	Targets        []ManifestTarget `json:"targets"`
	TimeoutSeconds int              `json:"timeoutSeconds"`

	// Inventory is the Ansible inventory file shipped to an cronomicon-mode runner
	// for `ansible-playbook -i` (protocol v2). It is byte-exact and SECRET-FREE
	// by invariant — secret-bearing vars are rejected at ingest (Path A / D1), so
	// this never carries a decrypted secret value. nil for non-ansible runs and
	// for local-mode runners (which hold their own inventory; the host list is
	// never shipped to them). omitempty keeps it wire-compatible with v1 agents.
	Inventory *ManifestInventory `json:"inventory,omitempty"`
	// SSHUser / SSHKeyRef carry the run's frozen "connect as" identity override
	// EXPLICITLY, for a local-toolchain (ansible) run (protocol v7, RP-8). Names
	// only, never material (D1): SSHKeyRef is the derived CRONOMICON_KEY_<label>
	// reference whose bytes travel — if at all — through the D8 Keys channel, and
	// the agent resolves it to a delivered 0600 file path.
	//
	// Why these exist when ManifestTarget.User already carries a user: an ssh-family
	// run is dialed BY the agent, so rewriting the resolved targets is enough. An
	// ansible run is dialed by ansible-playbook, and the agent's only handles are
	// argv and env — it cannot tell an overridden target from an inventory-wired one,
	// and localCommand never reads Target.User at all. These fields say "this is an
	// override" so the agent can emit it as connection extra-vars, which BEAT
	// inventory-authored ansible_user/ansible_ssh_private_key_file (RP-Q1).
	//
	// Set only when the run actually carries an override, and only for ansible;
	// the ssh-family path keeps using the target overlay, and terraform never
	// carries either (it authenticates through its providers).
	SSHUser   string `json:"sshUser,omitempty"`
	SSHKeyRef string `json:"sshKeyRef,omitempty"`
	// AnsibleOptions carries the per-run advanced ansible flags (protocol v8).
	// nil ⇒ the operator set none, which is the overwhelmingly common case and is
	// byte-identical on the wire to a pre-v8 manifest. Values are flags, NAMES and
	// operator-typed literals — nothing here is resolved from stored material.
	AnsibleOptions *ManifestAnsibleOptions `json:"ansibleOptions,omitempty"`
	// Limit is an Ansible --limit expression (host/group NAMES only, no secret,
	// no server-side projection dependency) for a targeted run; empty ⇒ no
	// --limit. Populated by group/limit targeting (milestone M3).
	Limit string `json:"limit,omitempty"`

	// EnvPassthrough is the allowlist of env-var NAMES (never values — D1) the
	// agent resolves from its OWN environment into a local-toolchain child
	// process (ansible-update.md RX.9, Phase 1). Presence of the field — even
	// empty — flips the agent from legacy full-environment inheritance to the
	// scoped child env (explicit base safe-list + manifest Env + these names);
	// absence (nil) keeps legacy behavior, so a new agent stays back-compatible
	// with an older server. Deliberately NOT omitempty: an empty-but-present
	// list must still ship, because "no extra names" and "no scoping" are
	// different contracts. Additive on protocol v2 — an older agent ignores the
	// unknown key and keeps its (legacy) behavior, so no version gate is needed.
	// The server populates it for local-toolchain run-types (ansible/terraform)
	// from the union of: inventory {{ lookup('env', NAME) }} references, target
	// AuthKeyEnvVars, and the job's env_passthrough spec list.
	EnvPassthrough []string `json:"envPassthrough"`

	// Checkout, when set, switches the agent from body-only delivery to
	// runner-side pinned checkout (protocol v3, ansible-update.md RX.1): the
	// agent fetches the playbooks repo, verifies the pinned SHA, materializes an
	// ephemeral working tree, and runs Entry from it (cwd = tree root) instead
	// of piping Body to ansible-playbook /dev/stdin. nil ⇒ body-only mode
	// (unchanged). Body stays populated with the entry file's content for
	// display/debug parity; the agent ignores it in checkout mode. (A checkout run
	// assigned to a protocol < 3 agent used to be hard-failed here; that gate, and
	// the v2 inventory gate it mirrored, went with the protocol floor in v1.5.40.)
	Checkout *ManifestCheckout `json:"checkout,omitempty"`

	// Secrets carries dispatch-time RESOLVED reference VALUES (vault-integration.md
	// P1.4, D1 = 1C): the derived CRONOMICON_SECRET_*/CRONOMICON_VAR_* keys mapped to
	// their values, resolved server-side from the run's declared reference bindings
	// (reference_bindings, migration 590). This DELIBERATELY breaks the historical
	// "the manifest never carries secret bytes" invariant for the runner path — it
	// is why the run is gated behind the operator's per-runner allow_secret_injection
	// flag (only such a runner ever claims a binding-bearing run). A secret-bearing
	// run assigned to a protocol < 6 agent was hard-failed until v1.5.40, when the
	// floor made "an older agent" unreachable: it cannot register. The
	// agent injects these into the child/remote process env; they never touch the
	// run tree. omitempty keeps a no-secrets manifest wire-identical to v5.
	Secrets map[string]string `json:"secrets,omitempty"`
	// Keys carries resolved SSH-key MATERIAL for CRONOMICON_KEY_* references the agent
	// writes to a 0600 file and points CRONOMICON_KEY_<name> at (D8, shipped). Populated
	// for a flagged v6 runner whose run binds a key; the agent materializes each to a
	// tmpfs 0600 file off the run tree (wiped at run end) and exposes its PATH — the
	// material never travels beyond this field. omitempty keeps a no-keys manifest
	// wire-identical to a keyless v6.
	Keys []ManifestKey `json:"keys,omitempty"`
	// SecretFiles are secrets delivered as 0600 files rather than env values
	// (RA-12, protocol v9). nil when none, keeping a file-free manifest wire-
	// identical to v8.
	SecretFiles []ManifestSecretFile `json:"secretFiles,omitempty"`
	// BecomePasswordRef is the DERIVED REFERENCE (e.g. CRONOMICON_SECRET_BECOME_PASSWORD)
	// whose materialized file path the agent passes to
	// `ansible-playbook --become-password-file` (RA-12). Empty ⇒ no become password,
	// which is every pre-v9 run.
	//
	// A reference rather than a path because the server has no idea where the agent
	// will put the file — the same names-only discipline (D1) that makes SSHKeyRef a
	// reference. The agent resolves it against the files it just materialized and
	// FAILS the run if it cannot, rather than dropping the flag: a run that asked to
	// escalate and silently did not is the class of bug the v8 gate exists to prevent.
	BecomePasswordRef string `json:"becomePasswordRef,omitempty"`
}

// ManifestKey is one resolved SSH-key reference: the bare row name, its derived
// CRONOMICON_KEY_<name> reference, and the decrypted private-key material. Ships only
// to allow_secret_injection runners (D8). The agent writes Material to a 0600
// file OFF the run tree, exposes CRONOMICON_KEY_<name>=<that path>, and wipes it at
// run end — the material never lands under the working tree.
type ManifestKey struct {
	Name      string `json:"name"`
	Reference string `json:"reference"`
	Material  string `json:"material"`
}

// ManifestSecretFile is one resolved secret the agent must deliver as a FILE
// (RA-12): the bare row name, its derived reference (alias-aware), and the value.
// Ships only to allow_secret_injection runners, exactly like ManifestKey — this is
// secret material on the wire and carries every gate the Secrets block does.
//
// The agent writes Value to a 0600 file OFF the run tree, exposes
// <Reference>=<that path> in the run env, and wipes it at run end. The value never
// enters the environment itself, which is the entire point: an env var is readable
// from /proc for anything that can see the process, and an operator piping it into
// `sudo -S` puts it in the target's process table where redaction cannot reach.
type ManifestSecretFile struct {
	Name      string `json:"name"`
	Reference string `json:"reference"`
	Value     string `json:"value"`
}

// ManifestCheckout is the runner-side pinned-checkout spec (RX.1/RX.2). It
// carries repo identity + a FULL commit SHA (never a ref) + the repo-relative
// entry playbook. No credential ever rides it (D1): the agent holds its own
// read-only deploy credential (RX.11) and resolves it locally from ITS config.
type ManifestCheckout struct {
	Repo  string `json:"repo"`  // repo identity; the agent matches it against its -checkout-repos allowlist and fetches it with its own deploy credential
	SHA   string `json:"sha"`   // full 40-hex commit SHA — never a ref; the agent verifies it exists before archiving (RX.2)
	Entry string `json:"entry"` // repo-relative playbook path, run with cwd = the materialized tree root
	// ReqPath is the repo-relative requirements.yml the agent installs per run
	// into the ephemeral workdir (RX.4). Populated in Phase 3; empty ⇒ none.
	ReqPath string `json:"requirementsPath,omitempty"`
	// UsesVault derives from the job's `vault` requirement token (RX.13, Phase
	// 3): when true the agent passes --vault-password-file from ITS OWN config —
	// the password never travels (D1). Empty until Phase 3 ships vault opt-in.
	UsesVault bool `json:"usesVault,omitempty"`
}

// ManifestAnsibleOptions is the per-run advanced ansible option set (protocol
// v8). It mirrors execspec.AnsibleOpts; the server validates every field at the
// trigger boundary (tag charset, verbosity range, become-user charset, extra-var
// names) so the agent can render argv without re-validating.
//
// ExtraVars is deliberately NOT a channel for secrets: `-e key=value` lands in
// the runner's process table, readable by any local user on that host. Secret
// material must travel as a reference (Secrets/Keys), never here.
type ManifestAnsibleOptions struct {
	Check      bool              `json:"check,omitempty"`
	Diff       bool              `json:"diff,omitempty"`
	Tags       []string          `json:"tags,omitempty"`
	SkipTags   []string          `json:"skipTags,omitempty"`
	Verbosity  int               `json:"verbosity,omitempty"`
	Become     bool              `json:"become,omitempty"`
	BecomeUser string            `json:"becomeUser,omitempty"`
	ExtraVars  map[string]string `json:"extraVars,omitempty"`
}

// ManifestInventory is the inventory-file payload carried by a v2 manifest for an
// cronomicon-mode ansible run. Raw is the -i material; it is secret-free because
// ingest (internal/inventory.ValidateSecrets) rejects secret-bearing inventory.
type ManifestInventory struct {
	Raw    string   `json:"raw"`              // byte-exact inventory content (D3)
	Format string   `json:"format,omitempty"` // ini|yaml — picks the runner temp-file extension
	Groups []string `json:"groups,omitempty"` // advisory parsed group set (M2+; for --limit validation/UI)
}

// ManifestTarget is one resolved host reference in a ManifestResponse. It is a
// reference only — no key bytes (D1). AuthKeyEnvVar names the env var / secret
// key the agent looks up locally to obtain the private key.
type ManifestTarget struct {
	Name          string `json:"name"`
	Address       string `json:"address"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	Via           string `json:"via"`           // bastion id/name; empty ⇒ direct
	AuthKeyEnvVar string `json:"authKeyEnvVar"` // env-var NAME only, never the key
	// ResolveErr is non-empty when a scope host could not be resolved to an
	// ssh_hosts record — surfaced as a per-host failure, not a silent skip.
	ResolveErr string `json:"resolveErr,omitempty"`
}

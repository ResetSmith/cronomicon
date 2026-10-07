// Package execspec holds the single source of truth for resolving "what runs
// where" — the command/interpreter for a job and the host targets for a run.
//
// v1's in-app SSH executor (internal/sshexec) and the future runner-manifest
// endpoint must resolve identically, from one place, so the two execution paths
// can't drift (runners-update.md §3; the EX.4 warning that a parallel resolution
// path is how drift and leaks happen). This package is importable by both the
// server and cmd/cronomicon-runner because they share the Go module.
//
// These pieces are resolution only. The sshexec-specific shell-building helpers
// (remoteCommand, env injection, quoting) stay in sshexec because they build the
// remote shell command, not the resolution.
package execspec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// outputMarkerRe matches an A12 inter-job output marker emitted on a job's stdout:
//
//	::cronomicon-output name=KEY::VALUE
//
// KEY is an env-var-style identifier; VALUE is the rest of the line. Both the
// in-app SSH executor and the runner log-ingest seam parse these from the RAW
// line (before redaction) and accumulate them into runs.outputs_json (Phase 5).
var outputMarkerRe = regexp.MustCompile(`^::cronomicon-output\s+name=([A-Za-z_][A-Za-z0-9_]*)::(.*)$`)

// ParseOutputMarker returns (key, value, true) if line is an A12 output marker.
func ParseOutputMarker(line string) (key, value string, ok bool) {
	m := outputMarkerRe.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// AgentPrefixesOutput reports whether a runner agent prefixes every line of a
// run of this type with "[host] ". It does for the types it runs over SSH on
// the targets (every shell type), and does not for the two it runs with a local
// toolchain on its own host. The agent's side of this is agent.localRunTypes;
// the two lists must agree.
func AgentPrefixesOutput(runType string) bool {
	return runType != "ansible" && runType != "terraform"
}

// ParseRunnerOutputMarker is ParseOutputMarker for a line that came through a
// runner agent's log upload. A shell job the agent runs over SSH reaches the
// server as "[host] line" — the agent prefixes every remote line with the host
// it came from, for one target as for several (agent/ssh.go) — so a marker on
// that path never started the line and was never captured: inter-job outputs
// from a shell job on an agent were silently empty. The in-app executor parses
// each line before it adds the same prefix; this is the matching step for the
// agent path, done on the server so the wire does not change.
//
// prefixed says the run is one whose lines the agent prefixes
// (AgentPrefixesOutput). Only then is one leading "[…] " removed, once. On
// such a run every line the job prints arrives behind the agent's prefix, so
// removing one restores exactly "a marker starts the job's line": a job line
// "[INFO] ::cronomicon-output…" arrives as "[host] [INFO] ::…" and is not a
// marker, as it would not be on the in-app executor. On a run the agent does
// NOT prefix (ansible, terraform) nothing is removed — there a leading "[…] "
// is the tool's or the job's own text, and reading past it would let data a
// job merely echoes after a bracketed tag set an output.
func ParseRunnerOutputMarker(line string, prefixed bool) (key, value string, ok bool) {
	if key, value, ok = ParseOutputMarker(line); ok {
		return key, value, true
	}
	if !prefixed || !strings.HasPrefix(line, "[") {
		return "", "", false
	}
	_, rest, found := strings.Cut(line, "] ")
	if !found {
		return "", "", false
	}
	return ParseOutputMarker(rest)
}

// FirstOutputLeakingSecret reports the name of the first captured output whose
// value contains (or equals) one of the run's injected secret values, or "" when
// none do (H2/DEC-2). injected is the per-run injected redaction dictionary
// (secret values only — variables are log-safe and are not in it). Names are
// scanned in sorted order so the same run reports the same offender.
//
// Both execution paths call this at the choke point BEFORE outputs are persisted:
// the runner log-ingest seam (internal/runner) and the in-app SSH executor
// (internal/sshexec). It lives here so the two paths cannot drift — an
// ::cronomicon-output:: value carrying an injected secret must fail the run closed
// on either path, or the value would propagate verbatim into outputs_json, a
// child step's plaintext env_json, and the run-detail API.
func FirstOutputLeakingSecret(outputs map[string]string, injected []string) string {
	if len(outputs) == 0 || len(injected) == 0 {
		return ""
	}
	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		v := outputs[name]
		for _, secret := range injected {
			if secret != "" && strings.Contains(v, secret) {
				return name
			}
		}
	}
	return ""
}

// SSHRunTypes is the set of run-types the in-app SSH executor can run (they
// execute on the remote host, so no local toolchain is needed). ansible /
// terraform need a local toolchain the distroless image lacks → runner-only
// (execution-update.md EX.2).
var SSHRunTypes = map[string]bool{
	"bash":       true,
	"perl":       true,
	"powershell": true,
	"python":     true,
}

// SupportedRunType reports whether the SSH executor can run the given run-type.
func SupportedRunType(runType string) bool { return SSHRunTypes[runType] }

// IdentityCapableRunType reports whether a run-type can honor a per-run/per-job
// "connect as" identity override (RP-7, the run-parity plan).
//
// It is deliberately WIDER than SupportedRunType and deliberately NOT
// "everything but terraform": the ssh-family types apply the override to the
// in-app SSH client's targets, and ansible applies it as connection extra-vars
// (`-e ansible_user=…`), which is a real, honored mechanism. terraform
// authenticates through its providers, not SSH, so accepting the fields there
// would silently no-op — the thing CA-2 refused to do. An UNKNOWN run-type
// answers false for the same reason: a type gains identity support only when
// something has actually been taught to apply it.
//
// This predicate is the single fold gate for all four producers (manual
// trigger, scheduler, workflow engine) plus the two authoring boundaries, so
// they cannot drift apart — the TG-class lesson.
func IdentityCapableRunType(runType string) bool {
	return SSHRunTypes[runType] || runType == "ansible"
}

// Target is one resolved host the executor connects to.
type Target struct {
	ID               string // ssh_hosts row id of the resolved row (M4 — for the source/scope-qualified TOFU write); empty ⇒ unresolved
	Name             string // inventory/host identifier
	Address          string // dial address (falls back to Name)
	Port             int
	User             string
	Via              string // bastion id or name; empty ⇒ direct
	AuthKeyEnvVar    string // key NAME (env-var / secret) — for inventory/git-imported + runner-resolved hosts; used when no credential
	AuthCredentialID string // first-class ssh_credentials id; resolved before AuthKeyEnvVar (SK.5)
	HostKey          string // stored known host key (authorized-key form); empty ⇒ TOFU
	ResolveErr       string // non-empty ⇒ this target could not be resolved; reported as a per-host failure
	// Owners are the ids of the agencies this record answers to (LR-70): its
	// own owner for a record written by hand, its scope's agencies for one
	// imported from an inventory. A target routes only through a bastion that
	// one of them — or Global — owns. Empty on an unresolved target.
	Owners []string
}

// ApplyIdentityOverride returns targets with the run's frozen "connect as"
// identity applied (CA, the ssh-user plan §2.2). A non-empty user
// replaces every target's login; a credential replaces the target's key
// selection outright — credentialID on the in-app SSH path (AuthKeyEnvVar
// cleared so the legacy fallback can't race the override, SK.5), keyEnvVar
// (the derived CRONOMICON_KEY_<label> reference, CA-3b) on the runner path where
// the manifest carries names only (D1). Exactly one of credentialID/keyEnvVar
// may be non-empty. Bastion hops are untouched (CA-Q4): the override changes
// who logs in with which key, not how the connection is routed. Unresolved
// targets (ResolveErr) pass through unchanged.
func ApplyIdentityOverride(targets []Target, user, credentialID, keyEnvVar string) []Target {
	if user == "" && credentialID == "" && keyEnvVar == "" {
		return targets
	}
	out := make([]Target, len(targets))
	for i, t := range targets {
		if t.ResolveErr == "" {
			if user != "" {
				t.User = user
			}
			if credentialID != "" {
				t.AuthCredentialID = credentialID
				t.AuthKeyEnvVar = ""
			}
			if keyEnvVar != "" {
				t.AuthKeyEnvVar = keyEnvVar
				t.AuthCredentialID = ""
			}
		}
		out[i] = t
	}
	return out
}

// ValidEnvName reports whether name is a valid POSIX environment-variable NAME:
// letters, digits and '_', not starting with a digit (RP-14). Used to validate
// authored env_passthrough lists — these become lookup keys in the agent's own
// environment, so anything a shell could not export is a typo, not a variable.
func ValidEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		isLetter := (c|0x20) >= 'a' && (c|0x20) <= 'z'
		isDigit := c >= '0' && c <= '9'
		if i == 0 && !isLetter && c != '_' {
			return false
		}
		if !isLetter && !isDigit && c != '_' {
			return false
		}
	}
	return true
}

// ValidSSHUser reports whether a per-run/per-job "connect as" username is
// acceptable (CA-2): a conservative POSIX-ish charset, because the value
// becomes an SSH auth string and rides audit surfaces verbatim.
func ValidSSHUser(user string) bool {
	if user == "" || len(user) > 32 {
		return false
	}
	for i := 0; i < len(user); i++ {
		c := user[i]
		lower := c | 0x20 // ASCII case fold for the letter check only
		isLetter := lower >= 'a' && lower <= 'z'
		if i == 0 {
			if !isLetter && c != '_' {
				return false
			}
			continue
		}
		if !isLetter && c != '_' && (c < '0' || c > '9') && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

// DialAddr returns the host:port dial string, defaulting address→name and
// port→22.
func (t Target) DialAddr() string {
	addr := t.Address
	if addr == "" {
		addr = t.Name
	}
	port := t.Port
	if port == 0 {
		port = 22
	}
	return fmt.Sprintf("%s:%d", addr, port)
}

// ResolveTargets expands a run's target into the host list both executors share
// (EX.5). Precedence: an explicit targetHost wins (single host); else a non-empty
// hosts subset resolves only those hosts (F2 — each MUST be a member of the bound
// scope, else a per-host ResolveErr; the trigger boundary also rejects non-members
// up front with 422); else the whole scope fans out. A scope host with no matching
// ssh_hosts record becomes a per-host failure (a Target with ResolveErr set), not a
// silent skip.
//
// LR-71: a fixed targetHost must be a MEMBER of the run's scope, like every host
// of a subset. It was resolved by name alone until 2.3.0, so a job in one
// agency's scope could be pointed at any host record at all by naming it; this
// is the one place all five producers resolve through, so it is the one place
// the rule is enforced. A job with no scope (or one naming a scope the catalog
// does not hold) is Global's work and resolves against Global's records only,
// which HostByName does by itself; a scope that lists no hosts at all has no
// membership to ask about (HostInScope).
func ResolveTargets(ctx context.Context, db *sql.DB, scope, targetHost string, hosts []string) ([]Target, error) {
	if targetHost != "" {
		if in, known, err := HostInScope(ctx, db, scope, targetHost); err != nil {
			return nil, err
		} else if known && !in {
			return []Target{{Name: targetHost, ResolveErr: "target_host " + targetHost + " is not a member of scope " + scope +
				" — a job runs only against hosts of its own scope"}}, nil
		}
		t, err := HostByName(ctx, db, scope, targetHost)
		if err != nil {
			return nil, err
		}
		if t == nil {
			return []Target{{Name: targetHost, ResolveErr: "no SSH host record for target_host " + targetHost}}, nil
		}
		return []Target{*t}, nil
	}
	if scope == "" {
		return nil, nil
	}

	names, err := ScopeHosts(ctx, db, scope)
	if err != nil {
		return nil, err
	}

	// F2 — a per-run host subset restricts the fan-out to the chosen members.
	if len(hosts) > 0 {
		member := make(map[string]bool, len(names))
		for _, n := range names {
			member[n] = true
		}
		var targets []Target
		for _, h := range hosts {
			if !member[h] {
				// Defensive: the trigger boundary already 422s a non-member, so
				// reaching here means a stale subset — surface it as a per-host
				// failure, never silently widen to the full scope.
				targets = append(targets, Target{Name: h, ResolveErr: "host " + h + " is not a member of scope " + scope})
				continue
			}
			t, err := HostByName(ctx, db, scope, h)
			if err != nil {
				return nil, err
			}
			if t == nil {
				targets = append(targets, Target{Name: h, ResolveErr: "no SSH host record for inventory host " + h})
				continue
			}
			targets = append(targets, *t)
		}
		return targets, nil
	}

	var targets []Target
	for _, name := range names {
		t, err := HostByName(ctx, db, scope, name)
		if err != nil {
			return nil, err
		}
		if t == nil {
			targets = append(targets, Target{Name: name, ResolveErr: "no SSH host record for inventory host " + name})
			continue
		}
		targets = append(targets, *t)
	}
	return targets, nil
}

// HostInScope answers "is this host a member of that scope" (LR-71). known is
// false when there is no membership to ask about, and the caller then treats
// the run as it would one with no scope:
//
//   - the catalog holds no scope of that name (an empty name, or a job's
//     free-text scope that matches nothing);
//   - the scope lists NO hosts at all. A scope whose hosts live only in a
//     runner's own inventory file (local-inventory mode) has none here, and a
//     scope used purely as a label for jobs that each pin a host has none
//     either. Nothing can be "outside" an empty list, and refusing every pinned
//     host of such a scope would stop jobs that have always run. The owner
//     filter of HostByName still applies to them: they reach their own agency's
//     records and Global's, and no one else's.
func HostInScope(ctx context.Context, db *sql.DB, scope, host string) (in, known bool, err error) {
	if scope == "" {
		return false, false, nil
	}
	var hosts int
	err = db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM scope_hosts sh WHERE sh.scope_id = s.id AND sh.host = ?),
		       (SELECT COUNT(*) FROM scope_hosts sh WHERE sh.scope_id = s.id)
		  FROM scopes s WHERE s.name = ?`, host, scope).Scan(&in, &hosts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return in, hosts > 0, nil
}

// ScopeHosts returns the host names belonging to a scope (scope_hosts → scopes) —
// the single membership source shared by ResolveTargets' fan-out, the F2 subset
// intersection, and the trigger-boundary membership check in runJob. Source-
// agnostic: git scopes are materialized into scope_hosts during sync, cronomicon
// scopes via settings.
func ScopeHosts(ctx context.Context, db *sql.DB, scope string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT sh.host
		FROM scope_hosts sh
		JOIN scopes s ON s.id = sh.scope_id
		WHERE s.name = ?
		ORDER BY sh.host`, scope)
	if err != nil {
		return nil, fmt.Errorf("read scope hosts: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		names = append(names, h)
	}
	return names, rows.Err()
}

// OverrideHosts extracts the chosen host subset from a run's override_json envelope
// (architecture-update.md F2/F3) so both executors can feed it to ResolveTargets.
// Returns nil for an empty/absent/malformed envelope.
func OverrideHosts(overrideJSON string) []string {
	if overrideJSON == "" {
		return nil
	}
	var env struct {
		Hosts []string `json:"hosts"`
	}
	if err := json.Unmarshal([]byte(overrideJSON), &env); err != nil {
		return nil
	}
	return env.Hosts
}

// HostByName loads an ssh_hosts row as a dial Target, keyed by hostname (the
// executor's by-name path). Returns (nil, nil) when no row matches.
// Precedence (D4 — the single deterministic resolution point). Candidate rows are
// scope-qualified by scope_id, NOT just by hostname, so a job in scope A never
// dials scope B's same-named host:
//   - A row with NULL scope_id is a GLOBAL operator overlay (a manually-authored
//     cronomicon host); it is a candidate for every scope and WINS.
//   - A row whose scope_id belongs to the requested scope (a git import OR an
//     cronomicon import for THIS scope) is a candidate; other scopes' rows are not.
//   - Within candidates: cronomicon over git, global overlay (scope_id NULL) over a
//     scoped import, then most-recent, then id (a total, deterministic order).
//
// The TOFU host-key capture must write back to the SAME row id this resolves (see
// sshexec hostKeyCallback) or the stored key and the dialed row drift.
func HostByName(ctx context.Context, db *sql.DB, scope, hostname string) (*Target, error) {
	// LR-70: a scope sees the records imported for it, the records its own
	// agency wrote by hand, and Global's — never another agency's, which it
	// could shadow or borrow until 2.3.0 by a matching hostname. A run with no
	// scope (or an unknown one) sees Global's alone. The order among them is the
	// one it always was, with Global first among the hand-written: a global
	// administrator's record still wins everywhere, deliberately.
	row := db.QueryRowContext(ctx, `
		SELECT h.id, h.hostname, h.address, h.port, h.username, h.via, h.auth_key_env_var, h.auth_credential_id, h.host_key,
		       h.scope_id, h.owner_agency
		FROM ssh_hosts h
		WHERE h.hostname = ?
		  AND `+HostRecordForScopeSQL+`
		ORDER BY `+HostRecordOrderSQL+`
		LIMIT 1`, hostname, scope, scope)
	var id, name, owner string
	var address, user, via, authKeyEnvVar, authCredentialID, hostKey, scopeID sql.NullString
	var port sql.NullInt64
	if err := row.Scan(&id, &name, &address, &port, &user, &via, &authKeyEnvVar, &authCredentialID, &hostKey, &scopeID, &owner); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	t := &Target{
		ID:               id,
		Name:             name,
		Address:          address.String,
		Port:             int(port.Int64),
		User:             user.String,
		Via:              via.String,
		AuthKeyEnvVar:    authKeyEnvVar.String,
		AuthCredentialID: authCredentialID.String,
		HostKey:          hostKey.String,
		Owners:           []string{owner},
	}
	if scopeID.Valid && scopeID.String != "" {
		// An imported record answers to its scope's agencies, not to the column.
		owners, err := scopeAgencyIDs(ctx, db, scopeID.String)
		if err != nil {
			return nil, err
		}
		t.Owners = owners
	}
	return t, nil
}

// HostRecordForScopeSQL is THE rule for which ssh_hosts rows (alias h) a scope
// may resolve (LR-70), as a predicate with two parameters, the scope's name
// twice: the records imported for it, the hand-written records of its own
// agency, and Global's. HostRecordOrderSQL is the order among several. They are
// exported because the host-key review asks the same question for the same host
// (runner.serverPin) and had a copy of the predicate from before records had
// owners: it went on reading every agency's records after this one stopped.
const (
	HostRecordForScopeSQL = `(h.scope_id IN (SELECT id FROM scopes WHERE name = ?)
		       OR (h.scope_id IS NULL AND (h.owner_agency = '` + agencyid.Global + `'
		           OR h.owner_agency IN (SELECT sa.agency_id FROM scope_agencies sa
		                                   JOIN scopes sc ON sc.id = sa.scope_id WHERE sc.name = ?))))`
	HostRecordOrderSQL = `(h.source='cronomicon') DESC, (h.scope_id IS NULL) DESC, (h.owner_agency = '` + agencyid.Global + `') DESC,
		         h.last_modified_at DESC, h.id DESC`
)

// scopeAgencyIDs returns the ids of the agencies a scope (by id) belongs to.
func scopeAgencyIDs(ctx context.Context, db *sql.DB, scopeID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT agency_id FROM scope_agencies WHERE scope_id = ? ORDER BY agency_id`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ScopeAgencies resolves the FULL agency set a scope belongs to, from the
// migration-670 join table (the agencies plan Phase 2, T2.5). It is the
// N:M successor to ScopeAgency, and the source of the runs.agencies_json snapshot
// written alongside the scalar at enqueue.
//
// Through Phase 2 the join table is a 1:1 mirror of scopes.agency_id, so this
// returns the same single name ScopeAgency does — the dual-write is what lets
// Phase 3 flip claimRun onto the set with the data already in place. Names, sorted,
// so the snapshot is deterministic and two enqueues of the same scope produce
// byte-identical JSON.
//
// Never empty (LR-22, LR-24, migration 1220). A run with no scope is Global's,
// and so is a run whose job names a scope the catalog does not hold (a job's
// scope is free text; such a run has always been general-pool work). A scope
// that EXISTS and has no agency is not "global": every scope is born with a
// Global row and can only trade it for another, so that state is corruption, and
// it is returned as ErrScopeHasNoAgency for the producer to refuse on.
func ScopeAgencies(ctx context.Context, db *sql.DB, scope string) ([]string, error) {
	out := []string{}
	if scope == "" {
		return []string{agencyid.GlobalName}, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT a.name FROM scope_agencies sa
		JOIN scopes   s ON s.id = sa.scope_id
		JOIN agencies a ON a.id = sa.agency_id
		WHERE s.name = ?
		ORDER BY a.name`, scope)
	if err != nil {
		// The error is the caller's to act on. This used to answer "no agencies"
		// — written for a pre-670 schema that cannot exist at run time, since
		// migrations run before anything serves — and a producer that got it
		// stamped the run as belonging to no agency: claimable by the wrong
		// runners, and resolving none of its own agency's secrets. An unreadable
		// membership is not an empty one; every producer refuses to enqueue on it.
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 0 {
		return out, nil
	}
	var known bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM scopes WHERE name = ?)`, scope).Scan(&known); err != nil {
		return nil, err
	}
	if known {
		return nil, fmt.Errorf("%w: %q", ErrScopeHasNoAgency, scope)
	}
	return []string{agencyid.GlobalName}, nil
}

// ErrScopeHasNoAgency reports a scope row with no membership at all. The
// database gives every scope a Global row at birth (migration 1220) and the
// setters refuse to leave one with none, so this is a row something deleted by
// hand. It is never read as "global".
var ErrScopeHasNoAgency = errors.New("scope belongs to no agency")

// MarshalAgencies renders an agency set as the runs.agencies_json snapshot. Always
// a well-formed array — never NULL, never "" — because json_each runs over this
// column inside the claim query from Phase 3 on, and malformed JSON there would
// throw mid-UPDATE and stop dispatch fleet-wide.
func MarshalAgencies(names []string) string {
	if len(names) == 0 {
		return "[]"
	}
	b, err := json.Marshal(names)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// AgenciesHaveOnlineRunner reports whether at least one ONLINE runner is eligible
// to claim a run for the given agency SET (agency-support.md M4, widened for
// AG-Q2b): an online runner that serves ANY of them. A queued runner run for
// which this is false will wait indefinitely — the stuck-run signal.
//
// One rule since migration 1220. There used to be a second arm for the empty
// set ("the general pool": a runner with no agencies); a run with no agency is
// now Global's, a runner with none now serves Global, and the membership arm
// says both. An EMPTY set is no agency at all and nothing serves it.
func AgenciesHaveOnlineRunner(ctx context.Context, db *sql.DB, agencies []string) (bool, error) {
	if len(agencies) == 0 {
		return false, nil
	}
	var exists bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM runner_agencies ra
			JOIN agencies a  ON a.id  = ra.agency_id
			JOIN runners  rn ON rn.id = ra.runner_id
			WHERE a.name IN (SELECT value FROM json_each(?)) AND rn.status = 'online'
		)`, MarshalAgencies(agencies)).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// EligibleOnlineRunnerForRun reports whether some ONLINE runner can actually
// claim the given run — the requirements-aware stuck-run probe (ansible-update.md
// §5, review R3). It is claimRun's predicate seen from the run's side, in one
// statement: run_type ∈ the runner's EFFECTIVE capability tokens (declared minus
// the mask), the run's requires ⊆ those tokens, the runner serves one of the
// run's agencies, the SB-1 scope binding, and the injection gate. The pieces it
// shares with the claim are in claimrule.go. It also returns the run's
// requirement tokens so a caller can name the unmet ones in a stuck-run hint. A
// run that does not exist returns (false, nil, nil).
func EligibleOnlineRunnerForRun(ctx context.Context, db *sql.DB, runID string) (ok bool, requires []string, err error) {
	var agenciesJSON, runType, requiresJSON, scope sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT COALESCE(agencies_json,'[]'), run_type, requires_json, scope FROM runs WHERE id = ?`, runID).
		Scan(&agenciesJSON, &runType, &requiresJSON, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if requiresJSON.Valid && requiresJSON.String != "" {
		_ = json.Unmarshal([]byte(requiresJSON.String), &requires)
	}

	// Secret-injection gate parity with claimRun (P1.4): a run that will have
	// material injected needs an allow_secret_injection runner, so the stuck-run
	// hint must not claim an ordinary online runner could take it. A failed read
	// is "needs one" — the hint may then name a gate that is not the cause, which
	// is better than promising a claim that will not come.
	needsInjection, ierr := runNeedsInjectionRunner(ctx, db, runID)
	if ierr != nil {
		needsInjection = true
	}

	// The run's frozen agency SET (mig. 680). One membership arm since migration
	// 1220, where a run with no scope is Global's and "[]" is no agency at all.
	ag := agenciesJSON.String
	if ag == "" {
		ag = "[]"
	}
	reqJSON := requiresJSON.String
	if reqJSON == "" {
		reqJSON = "[]"
	}
	caps := effectiveCapsSQL("rn")
	err = db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM runners rn
			WHERE rn.status = 'online'
			  AND ? IN `+caps+`
			  AND NOT EXISTS (
			    SELECT 1 FROM json_each(?) je
			    WHERE je.value NOT IN `+caps+`)
			  AND EXISTS (SELECT 1 FROM runner_agencies ra JOIN agencies a ON a.id = ra.agency_id
			               WHERE ra.runner_id = rn.id AND a.name IN (SELECT value FROM json_each(?)))
			  -- SB-1 binding parity (mig. 1180): an unrestricted scope passes, a
			  -- restricted one needs THIS runner named. Correlated here where
			  -- claimRun's is not, because this probe walks runners for one run
			  -- rather than runs for one runner — the rule is the same.
			  AND (NOT EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
			                    WHERE sc.name = ?)
			       OR EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
			                   WHERE sc.name = ? AND sr.runner_id = rn.id))
			  AND (? = 0 OR rn.allow_secret_injection = 1)
		)`, runType.String, reqJSON, ag, scope.String, scope.String, needsInjection).Scan(&ok)
	if err != nil {
		return false, requires, err
	}
	return ok, requires, nil
}

// ResolveCommand returns the interpreter argv and the script body for a job
// (EX.1). Exactly one of command/script/script_path is set (parse-time
// validated); script_path is read from the synced clone.
func ResolveCommand(ctx context.Context, db *sql.DB, gitCacheDir, jobName, jobSource, runType string) (interp []string, body string, err error) {
	if jobSource == "" {
		jobSource = "git" // legacy/git runs (NULL job_source) resolve the git-source job (A9)
	}
	var command, script, scriptPath sql.NullString
	row := db.QueryRowContext(ctx,
		`SELECT command, script, script_path FROM jobs WHERE name = ? AND source = ?`, jobName, jobSource)
	if err := row.Scan(&command, &script, &scriptPath); err != nil {
		return nil, "", fmt.Errorf("read job command: %w", err)
	}

	switch {
	case command.Valid && command.String != "":
		body = command.String
	case script.Valid && script.String != "":
		body = script.String
	case scriptPath.Valid && scriptPath.String != "":
		// Execution needs the FULL body (limit 0 = uncapped); the 1 MiB cap is a
		// display-only concern handled by the catalog content endpoint.
		data, _, rerr := SafeReadRepoFile(gitCacheDir, scriptPath.String, 0)
		if rerr != nil {
			return nil, "", rerr
		}
		body = string(data)
	default:
		return nil, "", fmt.Errorf("job %q has no command/script/scriptPath", jobName)
	}

	switch runType {
	case "bash":
		interp = []string{"bash", "-c"}
	case "perl":
		interp = []string{"perl"}
	case "powershell":
		interp = []string{"powershell", "-NonInteractive", "-Command"}
	case "python":
		interp = []string{"python3", "-c"}
	case "ansible", "terraform":
		// Local-toolchain run-types (runner-only). The agent builds the argv
		// itself (agent.localCommand) from runType — ansible-playbook / terraform —
		// so there is no server-side interpreter; ship the body (playbook /
		// tf-args) with a nil interp. The in-app SSH executor never reaches here
		// for these types (executor resolution rejects them upstream), so this is
		// purely the runner-manifest path.
		interp = nil
	default:
		return nil, "", fmt.Errorf("run-type %q has no command resolution", runType)
	}
	return interp, body, nil
}

// SafeReadRepoFile reads a repo-relative file from gitCacheDir behind the
// canonical path-traversal guard (Clean → reject ../abs → Join → EvalSymlinks →
// Rel re-check). It is the single reader shared by the SSH executor
// (ResolveCommand), the sync hash/scan (gitlab.readBodyAt), and the catalog
// content endpoint, so the guard cannot drift across the three.
//
// When limit > 0 the read is bounded: at most limit bytes are returned and
// truncated reports whether the file was larger (the returned data is exactly
// limit bytes). limit <= 0 reads the whole file (truncated always false) — which
// execution and content-hashing require, since they must see the full body; only
// the display endpoint passes a cap.
//
// The error embeds the absolute resolved path (useful in server logs); any caller
// that surfaces it to an HTTP client MUST map it to a clean status without echoing
// the path.
func SafeReadRepoFile(gitCacheDir, relPath string, limit int64) (data []byte, truncated bool, err error) {
	clean := filepath.Clean(relPath)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return nil, false, fmt.Errorf("unsafe scriptPath %q", relPath)
	}
	targetPath := filepath.Join(gitCacheDir, clean)
	resolvedPath, err := filepath.EvalSymlinks(targetPath)
	if err != nil {
		return nil, false, fmt.Errorf("resolve scriptPath %q: %w", relPath, err)
	}
	rel, err := filepath.Rel(gitCacheDir, resolvedPath)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return nil, false, fmt.Errorf("scriptPath %q escapes repository root: resolves to %q", relPath, resolvedPath)
	}
	if limit <= 0 {
		data, err = os.ReadFile(resolvedPath)
		if err != nil {
			return nil, false, fmt.Errorf("read scriptPath %q: %w", relPath, err)
		}
		return data, false, nil
	}
	f, err := os.Open(resolvedPath)
	if err != nil {
		return nil, false, fmt.Errorf("read scriptPath %q: %w", relPath, err)
	}
	defer f.Close()
	// Read one byte past the cap so an exactly-at-cap file is not falsely flagged.
	data, err = io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, false, fmt.Errorf("read scriptPath %q: %w", relPath, err)
	}
	if int64(len(data)) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}

// SyncRunAgencies materializes a run's agency snapshot into run_agencies
// (migration 690), the lookup structure claimRun probes. runs.agencies_json remains
// authoritative; this is derived from it and written in the SAME enqueue so the two
// cannot drift.
//
// A FAILURE HERE MUST FAIL THE ENQUEUE. The claim query reads run_agencies, so a
// run whose index rows are missing looks like a general-pool run to the predicate —
// it would be refused by every agency-bound runner and offered to the general pool
// instead. That is an isolation breach, not a performance blip, and silently
// swallowing this error is the one thing that would make the optimization unsafe.
//
// Every enqueue path must call this: the manual run handler, the scheduler, AND the
// workflow engine's child-run insert. The workflow path is easy to miss — it builds
// its own INSERT rather than going through scheduler.EnqueueParams.
func SyncRunAgencies(ctx context.Context, db *sql.DB, runID, agenciesJSON string) error {
	if agenciesJSON == "" || agenciesJSON == "[]" {
		// No agency, so no rows — and nothing will claim it. Only a terminal row
		// (a skip, a connection test) is written this way since migration 1220; a
		// run that will execute always names an agency, Global at the least.
		return nil
	}
	var names []string
	if err := json.Unmarshal([]byte(agenciesJSON), &names); err != nil {
		return fmt.Errorf("run %s: agency snapshot is not a JSON array: %w", runID, err)
	}
	for _, n := range names {
		if n == "" {
			continue
		}
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO run_agencies (run_id, agency) VALUES (?, ?)`, runID, n); err != nil {
			return fmt.Errorf("materialize run agency %q for %s: %w", n, runID, err)
		}
	}
	return nil
}

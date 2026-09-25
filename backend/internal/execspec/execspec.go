// Package execspec holds the single source of truth for resolving "what runs
// where" — the command/interpreter for a job and the host targets for a run.
//
// v1's in-app SSH executor (internal/sshexec) and the future runner-manifest
// endpoint must resolve identically, from one place, so the two execution paths
// can't drift (runners-update.md §3; the EX.4 warning that a parallel resolution
// path is how drift and leaks happen). This package is importable by both the
// server and cmd/amadeus-runner because they share the Go module.
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
func ResolveTargets(ctx context.Context, db *sql.DB, scope, targetHost string, hosts []string) ([]Target, error) {
	if targetHost != "" {
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

// ScopeHosts returns the host names belonging to a scope (scope_hosts → scopes) —
// the single membership source shared by ResolveTargets' fan-out, the F2 subset
// intersection, and the trigger-boundary membership check in runJob. Source-
// agnostic: git scopes are materialized into scope_hosts during sync, amadeus
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
//     amadeus host); it is a candidate for every scope and WINS.
//   - A row whose scope_id belongs to the requested scope (a git import OR an
//     amadeus import for THIS scope) is a candidate; other scopes' rows are not.
//   - Within candidates: amadeus over git, global overlay (scope_id NULL) over a
//     scoped import, then most-recent, then id (a total, deterministic order).
//
// The TOFU host-key capture must write back to the SAME row id this resolves (see
// sshexec hostKeyCallback) or the stored key and the dialed row drift.
func HostByName(ctx context.Context, db *sql.DB, scope, hostname string) (*Target, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, hostname, address, port, username, via, auth_key_env_var, auth_credential_id, host_key
		FROM ssh_hosts
		WHERE hostname = ?
		  AND (scope_id IS NULL OR scope_id IN (SELECT id FROM scopes WHERE name = ?))
		ORDER BY (source='amadeus') DESC, (scope_id IS NULL) DESC, last_modified_at DESC, id DESC
		LIMIT 1`, hostname, scope)
	var id, name string
	var address, user, via, authKeyEnvVar, authCredentialID, hostKey sql.NullString
	var port sql.NullInt64
	if err := row.Scan(&id, &name, &address, &port, &user, &via, &authKeyEnvVar, &authCredentialID, &hostKey); err != nil {
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
	}
	return t, nil
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
// byte-identical JSON. Empty (non-nil) for an unscoped scope, a scope with no
// agency, or a pre-670 schema.
func ScopeAgencies(ctx context.Context, db *sql.DB, scope string) ([]string, error) {
	out := []string{}
	if scope == "" {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT a.name FROM scope_agencies sa
		JOIN scopes   s ON s.id = sa.scope_id
		JOIN agencies a ON a.id = sa.agency_id
		WHERE s.name = ?
		ORDER BY a.name`, scope)
	if err != nil {
		// Best-effort on a pre-670 schema: an enqueue must never fail because the
		// membership table is absent. The scalar snapshot still carries the truth.
		return out, nil
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return out, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

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
// to claim a run for the given agency SET, under the disjoint matrix
// (agency-support.md M4, widened for AG-Q2b): for a non-empty set, an online member
// of ANY of them; for the EMPTY set (the general pool), an online runner with NO
// agencies. A queued runner run for which this is false will wait indefinitely —
// the stuck-run signal. Best-effort on a pre-440 schema.
func AgenciesHaveOnlineRunner(ctx context.Context, db *sql.DB, agencies []string) (bool, error) {
	var exists bool
	var err error
	if len(agencies) == 0 {
		err = db.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM runners rn
				WHERE rn.status = 'online'
				  AND NOT EXISTS (SELECT 1 FROM runner_agencies ra WHERE ra.runner_id = rn.id)
			)`).Scan(&exists)
	} else {
		err = db.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM runner_agencies ra
				JOIN agencies a  ON a.id  = ra.agency_id
				JOIN runners  rn ON rn.id = ra.runner_id
				WHERE a.name IN (SELECT value FROM json_each(?)) AND rn.status = 'online'
			)`, MarshalAgencies(agencies)).Scan(&exists)
	}
	if err != nil {
		return false, err
	}
	return exists, nil
}

// EligibleOnlineRunnerForRun reports whether some ONLINE runner can actually
// claim the given run — the requirements-aware stuck-run probe (ansible-update.md
// §5, review R3). It mirrors claimRun's gates entirely in SQL (json_each subset
// shape, no Go-side set intersection): run_type ∈ the runner's capability tokens,
// the run's requires ⊆ those tokens, the agency-eligibility split (a tagged
// run needs a member of that agency; an untagged run needs a runner with no
// agencies), and the RT-1 runner-tag pin. It also returns the run's requirement
// tokens so a caller can name the unmet requirements in a stuck-run hint. A run
// that does not exist / is not a runner run returns (false, nil, nil).
func EligibleOnlineRunnerForRun(ctx context.Context, db *sql.DB, runID string) (ok bool, requires []string, err error) {
	var agenciesJSON, runType, requiresJSON, jobName, jobSource, scriptRef, runnerTag sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT COALESCE(agencies_json,'[]'), run_type, requires_json, job_name, job_source, script_ref, runner_tag FROM runs WHERE id = ?`, runID).
		Scan(&agenciesJSON, &runType, &requiresJSON, &jobName, &jobSource, &scriptRef, &runnerTag)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if requiresJSON.Valid && requiresJSON.String != "" {
		_ = json.Unmarshal([]byte(requiresJSON.String), &requires)
	}

	// Secret-injection gate parity with claimRun (P1.4): a run whose job/script
	// declares reference bindings needs an allow_secret_injection runner, so the
	// stuck-run hint must not claim an ordinary online runner could take it.
	var bindsSecrets int
	_ = db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM reference_bindings rb
			WHERE (rb.owner_kind = 'job' AND rb.owner_source = COALESCE(NULLIF(?, ''), 'git') AND rb.owner_name = ?)
			   OR (rb.owner_kind = 'script' AND rb.owner_name = ?))`,
		jobSource.String, jobName.String, scriptRef.String).Scan(&bindsSecrets)

	// The run's frozen agency SET (mig. 680). "[]" ⇒ the general pool. This mirrors
	// claimRun's predicate exactly — including the deliberately unchanged disjoint
	// general-pool branch (AG-Q3a) — because a stuck-run hint that disagreed with
	// the claim query would send an operator looking in the wrong place.
	ag := agenciesJSON.String
	if ag == "" {
		ag = "[]"
	}
	reqJSON := requiresJSON.String
	if reqJSON == "" {
		reqJSON = "[]"
	}
	err = db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM runners rn
			WHERE rn.status = 'online'
			  AND ? IN (SELECT value FROM json_each(rn.capabilities))
			  AND NOT EXISTS (
			    SELECT 1 FROM json_each(?) je
			    WHERE je.value NOT IN (SELECT value FROM json_each(rn.capabilities)))
			  AND (
			    (? <> '[]' AND EXISTS (SELECT 1 FROM runner_agencies ra JOIN agencies a ON a.id = ra.agency_id
			                           WHERE ra.runner_id = rn.id AND a.name IN (SELECT value FROM json_each(?))))
			    OR (? = '[]' AND NOT EXISTS (SELECT 1 FROM runner_agencies ra WHERE ra.runner_id = rn.id))
			  )
			  -- RT-1 pin parity (mig. 1070): an unpinned run short-circuits, a
			  -- pinned one needs THIS runner to carry the tag. ANDed with the
			  -- agency branch for the same reason claimRun ANDs it (RT-Q2) — a
			  -- probe that ORed it would report a run claimable by a runner the
			  -- claim query will never offer it to.
			  AND (? = '' OR EXISTS (SELECT 1 FROM runner_tags rt
			                         WHERE rt.runner_id = rn.id AND rt.tag = ?))
			  AND (? = 0 OR rn.allow_secret_injection = 1)
		)`, runType.String, reqJSON, ag, ag, ag, runnerTag.String, runnerTag.String, bindsSecrets).Scan(&ok)
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
		return nil // general pool — no rows, which is what the predicate expects
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

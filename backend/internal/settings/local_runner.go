package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// The local runner (LR-1, LR-38 to LR-46; v2.3.0).
//
// The server can run shell jobs itself, over SSH, from inside its own process.
// That engine (internal/sshexec) is the LOCAL RUNNER: one row in `runners` of
// kind `server`, owned by Global, that claims what it is placed to serve like
// any other runner. This file is the row and the switch.
//
//   - The row always exists once the server has started (EnsureLocalRunner).
//     Bindings, placements and host-key ledger rows key on its id, so "off" is
//     a status and never an absence (LR-39).
//   - On or off is an app setting, `localRunner.enabled`, read through ONE
//     function (LocalRunnerEnabled). The host can forbid it outright with
//     CRONOMICON_LOCAL_RUNNER=forbid, which wins (LR-17).
//   - It serves its owner and nothing else (CheckRunnerPlacement): since 2.5.3
//     the one agency a global administrator gives it (SetLocalRunnerOwner,
//     CA-8), or none, which is off (CA-9). (Global only from §0a, IN-3,
//     migration 1380; from MA-11 until then a global administrator edited it.)
//   - It claims through the one claim (runner.Claim), as this row: agencies,
//     scope bindings, requirements and priority apply to it as to an agent.

const (
	// RunnerKindAgent is every runner that registers with a token.
	RunnerKindAgent = "agent"
	// RunnerKindServer is the local runner. Not "local": runners.inventory
	// already has a `local`, which the Runners view badges.
	RunnerKindServer = "server"

	// LocalRunnerName is the row's name. It is not self-declared and is not
	// unique by constraint; the KIND is what identifies the row.
	LocalRunnerName = "Local runner"

	localRunnerEnabledKey = "localRunner.enabled"
	// localRunnerSeedKey records what the switch was SEEDED with: whether the
	// SSH executor was on when 2.3.0 first started. The upgrade pass reads
	// this, not the switch — the switch can be changed, or overruled by the
	// host's forbid, before the pass has run.
	localRunnerSeedKey = "localRunner.sshExecutorWasOn"
)

// LocalRunnerCapabilities are fixed (LR-41): the server image carries no
// toolchains, so the local runner runs the shell types and nothing else.
var LocalRunnerCapabilities = []string{"bash", "perl", "powershell", "python"}

var (
	// ErrLocalRunnerForbidden is returned when the app is asked to turn the
	// local runner on and the host forbids it. Mapped 409 `local_runner_forbidden`.
	ErrLocalRunnerForbidden = errors.New("the local runner is forbidden on this host (CRONOMICON_LOCAL_RUNNER=forbid), so it cannot be turned on from the app")
	// ErrNoLocalRunner is returned when the row does not exist: the server has
	// not started against this database yet.
	ErrNoLocalRunner = errors.New("the local runner has not been set up yet")
	// ErrLocalRunnerUnassigned is returned when the app is asked to turn the
	// local runner on while it belongs to no agency (2.5.3, CA-9). Mapped 422
	// `local_runner_unassigned`.
	ErrLocalRunnerUnassigned = errors.New("the local runner belongs to no agency, so it cannot be turned on: " +
		"assign it to the agency whose work it is to run first")
	// ErrLocalRunnerBusy is returned when its agency would change while it is
	// running a run (2.5.3, CA-10). Mapped 409 `local_runner_busy`.
	ErrLocalRunnerBusy = errors.New("the local runner is running a run: let it finish (or stop it) before it changes agency")
)

// LocalRunnerBoundError is the refusal of a change of agency while scopes are
// bound to the local runner (the review of CA3): deleting the bindings would
// OPEN such a scope to every runner of its agency, the opposite of what the
// binding asked ("closed, not silently widened"). The scope's agency unbinds
// it, or binds it to a runner of its own, first. Mapped 409
// `local_runner_bound`.
type LocalRunnerBoundError struct{ Scopes []string }

func (e *LocalRunnerBoundError) Error() string {
	return "scopes are bound to the local runner (" + strings.Join(e.Scopes, ", ") + "): unbind them, or bind them " +
		"to a runner of their agency's, before the local runner changes agency"
}

// LocalRunnerOwnerAfter is the agency the local runner would belong to after
// a request naming ownerAgency (nil: as it is), for a caller that validates a
// request before it writes anything: the built-in agency's id when none.
func LocalRunnerOwnerAfter(ctx context.Context, database *sql.DB, ownerAgency *string) (string, error) {
	if ownerAgency != nil {
		if t := strings.TrimSpace(*ownerAgency); t != "" {
			return t, nil
		}
		return agencyid.Global, nil
	}
	return localRunnerOwner(ctx, database)
}

// LocalRunner is the card's view of the local runner.
type LocalRunner struct {
	RunnerID string `json:"runnerId"`
	// Enabled is the EFFECTIVE state: the setting, unless the host forbids it.
	Enabled bool `json:"enabled"`
	// Forbidden is CRONOMICON_LOCAL_RUNNER=forbid: off, and not changeable here.
	Forbidden     bool        `json:"forbidden"`
	Status        string      `json:"status"`
	MaxConcurrent int         `json:"maxConcurrent"`
	Capabilities  []string    `json:"capabilities"`
	Serves        []AgencyRef `json:"serves"`
	// Owner is the agency the local runner belongs to and serves, chosen by a
	// global administrator (2.5.3, CA-8); null when it belongs to none, which
	// is off (CA-9).
	Owner *AgencyRef `json:"owner"`
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// LocalRunnerID returns the local runner's id, or "" when the row does not
// exist (a database the server has not started against).
func LocalRunnerID(ctx context.Context, q queryRower) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM runners WHERE kind = ?`, RunnerKindServer).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// IsLocalRunner reports whether runnerID is the local runner.
func IsLocalRunner(ctx context.Context, q queryRower, runnerID string) (bool, error) {
	var kind string
	err := q.QueryRowContext(ctx, `SELECT kind FROM runners WHERE id = ?`, runnerID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return kind == RunnerKindServer, err
}

// LocalRunnerEnabled is THE read of the switch. The host's "forbid" wins; with
// no stored value the local runner is off; and since 2.5.3 it is off while it
// belongs to no agency (CA-9: unassigned is off), whatever the stored value.
func LocalRunnerEnabled(ctx context.Context, database *sql.DB, forbid bool) (bool, error) {
	if forbid {
		return false, nil
	}
	on, err := localRunnerSwitch(ctx, database)
	if err != nil || !on {
		return false, err
	}
	owner, err := localRunnerOwner(ctx, database)
	if err != nil {
		return false, err
	}
	return owner != "" && owner != agencyid.Global, nil
}

// localRunnerSwitch is the stored switch, as it was written.
func localRunnerSwitch(ctx context.Context, q queryRower) (bool, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, localRunnerEnabledKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == "true", nil
}

// localRunnerOwner is the local runner's owner: "" when there is no row, the
// built-in agency's id when it is unassigned.
func localRunnerOwner(ctx context.Context, q queryRower) (string, error) {
	var owner string
	err := q.QueryRowContext(ctx, `SELECT owner_agency FROM runners WHERE kind = ?`, RunnerKindServer).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return owner, err
}

// LocalRunnerUnassignedButSwitchedOn reports the state an installation from
// before 2.5.3 arrives in when its local runner was on: the switch is stored
// on, and the local runner, which was Global's, belongs to no agency, so it is
// off (CA-9). The notice local_runner_unassigned reads it.
func LocalRunnerUnassignedButSwitchedOn(ctx context.Context, database *sql.DB) (bool, error) {
	on, err := localRunnerSwitch(ctx, database)
	if err != nil || !on {
		return false, err
	}
	owner, err := localRunnerOwner(ctx, database)
	return owner == agencyid.Global, err
}

// EnsureLocalRunner creates the local runner's row and seeds its switch, once.
// It is idempotent and is called at every boot before anything claims.
//
// The seeds come from the environment names the SSH executor had until 2.3.0
// (LR-44, LR-45): whether it was on, and how many runs it took at once. They
// are read only when the row (or the setting) does not exist yet; afterwards
// the app's own values are the truth and the old names are ignored. seeded
// reports that the switch was written by this call, so the caller can log the
// state an upgrade arrived in.
func EnsureLocalRunner(ctx context.Context, database *sql.DB, seedEnabled bool, seedConcurrency int) (id string, seeded bool, err error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback() //nolint:errcheck

	id, err = LocalRunnerID(ctx, tx)
	if err != nil {
		return "", false, err
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	if id == "" {
		if seedConcurrency <= 0 {
			seedConcurrency = 4
		}
		// The seed obeys the bound the card enforces, or the card could not
		// save anything until the number was lowered.
		seedConcurrency = min(seedConcurrency, LocalRunnerMaxConcurrentCap)
		caps, _ := json.Marshal(LocalRunnerCapabilities)
		id = db.NewID()
		// Owned by Global, and born serving Global (the trigger of 1250 reads
		// the owner). Offline until the engine says otherwise. Secret injection
		// is on and stays on (LR-46): this process is the secret store.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO runners (id, name, kind, status, os, capabilities, load, max_concurrent, version,
			                     inventory, registered_at, created_at, owner_agency, allow_secret_injection)
			VALUES (?, ?, ?, 'offline', 'Linux', ?, 0, ?, '', 'cronomicon', ?, ?, ?, 1)`,
			id, LocalRunnerName, RunnerKindServer, string(caps), seedConcurrency, ts, ts, agencyid.Global); err != nil {
			return "", false, fmt.Errorf("create the local runner: %w", err)
		}
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key = ?`, localRunnerEnabledKey).Scan(&n); err != nil {
		return "", false, err
	}
	if n == 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO settings (key, value, last_modified_by, last_modified_at) VALUES (?, ?, 'system', ?)`,
			localRunnerEnabledKey, boolStr(seedEnabled), ts); err != nil {
			return "", false, fmt.Errorf("seed the local runner setting: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO settings (key, value, last_modified_by, last_modified_at) VALUES (?, ?, 'system', ?)`,
			localRunnerSeedKey, boolStr(seedEnabled), ts); err != nil {
			return "", false, fmt.Errorf("record the local runner seed: %w", err)
		}
		seeded = true
	}
	return id, seeded, tx.Commit()
}

// GetLocalRunner reads the card. ErrNoLocalRunner when the row is not there.
func GetLocalRunner(ctx context.Context, database *sql.DB, forbid bool) (*LocalRunner, error) {
	out := &LocalRunner{Forbidden: forbid, Capabilities: []string{}, Serves: []AgencyRef{}}
	var caps string
	err := database.QueryRowContext(ctx, `
		SELECT id, status, max_concurrent, COALESCE(capabilities, '[]') FROM runners WHERE kind = ?`, RunnerKindServer).
		Scan(&out.RunnerID, &out.Status, &out.MaxConcurrent, &caps)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoLocalRunner
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(caps), &out.Capabilities)
	if out.Enabled, err = LocalRunnerEnabled(ctx, database, forbid); err != nil {
		return nil, err
	}
	var ownerID, ownerName string
	if err := database.QueryRowContext(ctx, `
		SELECT a.id, a.name FROM runners r JOIN agencies a ON a.id = r.owner_agency WHERE r.id = ?`, out.RunnerID).
		Scan(&ownerID, &ownerName); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if ownerID != "" && ownerID != agencyid.Global {
		out.Owner = &AgencyRef{ID: ownerID, Name: ownerName}
	}
	rows, err := database.QueryContext(ctx, `
		SELECT a.id, a.name FROM runner_agencies ra JOIN agencies a ON a.id = ra.agency_id
		 WHERE ra.runner_id = ? ORDER BY a.name`, out.RunnerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a AgencyRef
		if err := rows.Scan(&a.ID, &a.Name); err != nil {
			return nil, err
		}
		out.Serves = append(out.Serves, a)
	}
	return out, rows.Err()
}

// LocalRunnerMaxConcurrentCap bounds the setting: the engine holds one
// goroutine and several SSH sessions per run.
const LocalRunnerMaxConcurrentCap = 64

// SetLocalRunner turns the local runner on or off and sets how many runs it
// takes at once. nil leaves a value as it is. Both changes are audited in the
// transaction that makes them (LR-43): turning on makes this process hold SSH
// keys and open connections to job targets, and a change of that without its
// record must not exist.
//
// It changes the stored state only. The caller tells the engine (the API layer
// holds it), which is what starts or drains the claim loop.
func SetLocalRunner(ctx context.Context, database *sql.DB, forbid bool, enabled *bool, maxConcurrent *int, actor string) (*LocalRunner, error) {
	if enabled != nil && *enabled && forbid {
		return nil, ErrLocalRunnerForbidden
	}
	if enabled != nil && *enabled {
		// Unassigned is off (CA-9): it is assigned first (SetLocalRunnerOwner).
		owner, err := localRunnerOwner(ctx, database)
		if err != nil {
			return nil, err
		}
		if owner == agencyid.Global {
			return nil, ErrLocalRunnerUnassigned
		}
	}
	if maxConcurrent != nil && (*maxConcurrent < 1 || *maxConcurrent > LocalRunnerMaxConcurrentCap) {
		return nil, fmt.Errorf("%w: maxConcurrent must be between 1 and %d", ErrValidation, LocalRunnerMaxConcurrentCap)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	id, err := LocalRunnerID(ctx, tx)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, ErrNoLocalRunner
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	owner, err := localRunnerOwner(ctx, tx)
	if err != nil {
		return nil, err
	}
	note := func(summary string) error {
		// The installation's change (its switch is a global administrator's),
		// and its agency's, whose work it runs (2.5.3, CA-8).
		return noteLocalRunner(ctx, tx, ts, actor, summary, agencyid.Global, owner)
	}
	if enabled != nil {
		var was string
		_ = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, localRunnerEnabledKey).Scan(&was)
		if was != boolStr(*enabled) {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO settings (key, value, last_modified_by, last_modified_at) VALUES (?, ?, ?, ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value,
				 last_modified_by = excluded.last_modified_by, last_modified_at = excluded.last_modified_at`,
				localRunnerEnabledKey, boolStr(*enabled), actor, ts); err != nil {
				return nil, err
			}
			summary := "local runner turned off: runs it is running finish, and it claims no more"
			if *enabled {
				summary = "local runner turned on: this server now holds SSH keys and connects to job targets"
			}
			if err := note(summary); err != nil {
				return nil, err
			}
		}
	}
	if maxConcurrent != nil {
		var was int
		if err := tx.QueryRowContext(ctx, `SELECT max_concurrent FROM runners WHERE id = ?`, id).Scan(&was); err != nil {
			return nil, err
		}
		if was != *maxConcurrent {
			if _, err := tx.ExecContext(ctx, `UPDATE runners SET max_concurrent = ? WHERE id = ?`, *maxConcurrent, id); err != nil {
				return nil, err
			}
			if err := note(fmt.Sprintf("local runner concurrency set to %d (was %d)", *maxConcurrent, was)); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return GetLocalRunner(ctx, database, forbid)
}

// noteLocalRunner writes one Activity row per agency named (the built-in
// agency's for the installation, and each other agency the change is about),
// once each.
func noteLocalRunner(ctx context.Context, tx *sql.Tx, ts, actor, summary string, agencies ...string) error {
	seen := map[string]bool{}
	for _, a := range agencies {
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		if err := auditlog.WriteActivity(ctx, tx, auditlog.ActivityParams{
			AgencyID: a, At: ts, Kind: "config", Outcome: "success", Actor: actor, Category: "Runners",
			Target: "runner:" + LocalRunnerName, RunnerName: LocalRunnerName, Summary: summary,
		}); err != nil {
			return err
		}
	}
	return nil
}

// SetLocalRunnerOwner gives the local runner to one ordinary agency, or to
// none ("" or the built-in agency's id: unassigned, which is off) (2.5.3,
// CA-8 to CA-10). It is a global administrator's (the route's gate).
//
// In one transaction: refused while the local runner is running a run
// (ErrLocalRunnerBusy); its owner and its serve list become exactly the agency
// (CheckRunnerPlacement); its scope bindings go, since they are the old
// agency's scopes; and its host-key ledger, pending keys and scan requests go,
// so that the new agency approves the keys of its own hosts (an approval was
// one agency's trust in a host, and the ledger has no agency to keep them
// apart). Refused while scopes are bound to it (LocalRunnerBoundError): their
// agency unbinds them first. And it is turned OFF (found while building CA3): with no key
// approved, a local runner that stayed on would claim the new agency's waiting
// runs at once and fail them on their hosts' keys, runs the agency's own agents
// would have run. A global administrator turns it on once the agency has
// approved its hosts' keys (one request may do both, deliberately). Runs of the
// old agency that were waiting for it wait on, as for a runner removed.
// Recorded for the installation and for both agencies.
//
// The caller tells the engine afterwards (the API layer holds it): an
// unassigned local runner is off.
func SetLocalRunnerOwner(ctx context.Context, database *sql.DB, agencyID, actor string) (*LocalRunner, bool, error) {
	target := strings.TrimSpace(agencyID)
	if target == "" {
		target = agencyid.Global
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback() //nolint:errcheck
	id, err := LocalRunnerID(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	if id == "" {
		return nil, false, ErrNoLocalRunner
	}
	cur, err := localRunnerOwner(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	if cur == target {
		return nil, false, tx.Rollback()
	}
	targetName := "no agency"
	if target != agencyid.Global {
		if err := tx.QueryRowContext(ctx, `SELECT name FROM agencies WHERE id = ?`, target).Scan(&targetName); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, false, ErrUnknownAgency
			}
			return nil, false, err
		}
	}
	var busy bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM runs WHERE runner_id = ? AND status = 'running')`, id).Scan(&busy); err != nil {
		return nil, false, err
	}
	if busy {
		return nil, false, ErrLocalRunnerBusy
	}
	bound, err := boundScopeNames(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}
	if len(bound) > 0 {
		return nil, false, &LocalRunnerBoundError{Scopes: bound}
	}
	before, err := runnerAgencyIDs(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}
	if err := CheckRunnerPlacement(true, target, before, []string{target}); err != nil {
		return nil, false, err
	}
	wasOn, err := localRunnerSwitch(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	for _, st := range []struct{ what, query string }{
		{"owner", `UPDATE runners SET owner_agency = ?2 WHERE id = ?1`},
		{"serve list", `DELETE FROM runner_agencies WHERE runner_id = ?1`},
		{"serve list", `INSERT INTO runner_agencies (runner_id, agency_id) VALUES (?1, ?2)`},
		{"host-key ledger", `DELETE FROM host_key_ledger WHERE runner_id = ?1`},
		{"pending host keys", `DELETE FROM pending_host_keys WHERE runner_id = ?1`},
		{"host-key scan requests", `DELETE FROM host_key_scan_targets WHERE runner_id = ?1`},
	} {
		args := []any{id}
		if st.what == "owner" || st.query == `INSERT INTO runner_agencies (runner_id, agency_id) VALUES (?1, ?2)` {
			args = append(args, target)
		}
		if _, err := tx.ExecContext(ctx, st.query, args...); err != nil {
			return nil, false, fmt.Errorf("give the local runner to %s: its %s: %w", targetName, st.what, err)
		}
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	if wasOn {
		if _, err := tx.ExecContext(ctx, `UPDATE settings SET value = 'false', last_modified_by = ?, last_modified_at = ? WHERE key = ?`,
			actor, ts, localRunnerEnabledKey); err != nil {
			return nil, false, fmt.Errorf("give the local runner to %s: turn it off: %w", targetName, err)
		}
	}
	fromName := "no agency"
	if cur != agencyid.Global {
		_ = tx.QueryRowContext(ctx, `SELECT name FROM agencies WHERE id = ?`, cur).Scan(&fromName)
	}
	summary := fmt.Sprintf("local runner given to %s (it was %s's): the host keys approved for it were cleared, for "+
		"the agency to approve its own hosts' keys", targetName, fromName)
	if wasOn {
		summary += "; it was turned off, and is turned on once they are approved"
	}
	if target == agencyid.Global {
		summary = fmt.Sprintf("local runner taken from %s: it belongs to no agency and is off", fromName)
	}
	if err := noteLocalRunner(ctx, tx, ts, actor, summary, agencyid.Global, cur, target); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	lr, err := GetLocalRunner(ctx, database, false)
	return lr, true, err
}

// boundScopeNames names the scopes bound to a runner, sorted.
func boundScopeNames(ctx context.Context, tx *sql.Tx, runnerID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT sc.name FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
		 WHERE sr.runner_id = ? ORDER BY sc.name`, runnerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

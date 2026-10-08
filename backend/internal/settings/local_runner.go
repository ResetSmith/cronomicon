package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
//   - It is the one runner with a serve list a global administrator edits
//     (MA-11, MA-14); see CheckRunnerPlacement.
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
)

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
// no stored value the local runner is off.
func LocalRunnerEnabled(ctx context.Context, database *sql.DB, forbid bool) (bool, error) {
	if forbid {
		return false, nil
	}
	var v string
	err := database.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, localRunnerEnabledKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == "true", nil
}

// SSHExecutorWasOn reports whether the SSH executor was turned on when 2.3.0
// first started against this database: the value the switch was seeded with.
// It is what the upgrade pass acts on. Neither the present switch nor the
// host's forbid answers that question — an operator may have changed the one,
// and the other says what the server may do from now on, not what it was
// doing. (A database seeded before this record existed has only the switch,
// and falls back to it.)
func SSHExecutorWasOn(ctx context.Context, database *sql.DB) (bool, error) {
	var v string
	err := database.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, localRunnerSeedKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return LocalRunnerEnabled(ctx, database, false)
	}
	if err != nil {
		return false, err
	}
	return v == "true", nil
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
	note := func(summary string) error {
		return auditlog.WriteActivity(ctx, tx, auditlog.ActivityParams{
			At: ts, Kind: "config", Outcome: "success", Actor: actor, Category: "Runners",
			Target: "runner:" + LocalRunnerName, RunnerName: LocalRunnerName, Summary: summary,
		})
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

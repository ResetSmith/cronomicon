package runref

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/envref"
)

// RunContext is the fixed AMADEUS_RUN_* set the dispatcher injects into every run
// (D7 / namespace plan N-D4). Unlike references it is not bound and not resolved
// from a store — it is the run's own metadata, always log-safe (never redacted).
// The executor fills the fields it has and calls Env() to get the injectable map.
type RunContext struct {
	ID          string // run / trace id
	Job         string // job name
	JobSource   string // git | amadeus
	Scope       string // "" = global
	Type        string // run_type
	TriggeredBy string // actor email
	Executor    string // ssh | runner
}

// Env renders the run context as its AMADEUS_RUN_* env map. Every field is
// emitted (empty string when unset) so a run author can rely on the key existing;
// Scope in particular is legitimately empty for a global run.
func (rc RunContext) Env() map[string]string {
	return map[string]string{
		envref.RunID:          rc.ID,
		envref.RunJob:         rc.Job,
		envref.RunJobSource:   rc.JobSource,
		envref.RunScope:       rc.Scope,
		envref.RunType:        rc.Type,
		envref.RunTriggeredBy: rc.TriggeredBy,
		envref.RunExecutor:    rc.Executor,
	}
}

// RunAgencies reads a run's FROZEN agency snapshot (migration 680), the set every
// Phase-3 resolution predicate intersects against.
//
// AG-Q8: resolution reads the run's SNAPSHOT, never live membership. Resolution
// happens at manifest fetch, after the claim — if a scope were re-homed in between,
// live membership and the snapshot would disagree, and a run's injectable set must
// be determined by the world as it was when the run was authorized. Reading live
// membership here would also mean a scope edit silently retargets what an in-flight
// run can reach, which is the security-relevant surprise AG-Q2(d) was rejected for.
//
// A run that does not exist, or a pre-680 schema, yields an empty set — which
// intersects nothing but still resolves every UNRESTRICTED row, matching the
// general-pool run's behavior rather than failing every reference.
func RunAgencies(ctx context.Context, database *sql.DB, runID string) ([]string, error) {
	out := []string{}
	var raw sql.NullString
	err := database.QueryRowContext(ctx, `SELECT agencies_json FROM runs WHERE id = ?`, runID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, nil // best-effort: a pre-680 schema has no column
	}
	if !raw.Valid || raw.String == "" || raw.String == "[]" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		return nil, fmt.Errorf("run %s: agency snapshot is not a JSON array: %w", runID, err)
	}
	return out, nil
}

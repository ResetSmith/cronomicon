package execspec

import (
	"context"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// The executor, as a value. Until 2.3.0 a run was written for one of two
// executors — the server's in-app SSH pool, or an out-of-process runner — and
// this file held the ONE precedence that chose between them (a per-run
// override, the job's own setting, a scope bound to runners, the global
// default, the run type). That choice is gone (LR-42): every run is written
// for the runner executor, and which runner takes it — an agent, or the server
// itself as the local runner — is decided when it is claimed, by agency, scope
// binding and capability. Nothing on a run or a job chooses.
//
// `runs.executor` on a row from before 2.3.0 still says "ssh" (the engine that
// ran it); nothing writes it. The constant for it, and the 2.3.0 upgrade pass
// that read what jobs used to say, were retired in 2.5.0 (question 60).
const ExecutorRunner = "runner"

// ScopeIsBound reports whether a scope (by name) is bound to at least one
// runner. An empty name, or one with no scopes row, is not.
func ScopeIsBound(ctx context.Context, q rowQueryer, agency, scope string) (bool, error) {
	if strings.TrimSpace(scope) == "" {
		return false, nil
	}
	var bound bool
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
		                WHERE `+agencyid.ScopeIn("sc", "?", "?")+`)`, scope, ownerOrGlobal(agency)).Scan(&bound)
	return bound, err
}

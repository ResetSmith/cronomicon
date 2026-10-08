package execspec

import (
	"context"
	"strings"
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
// The two values remain because history does: `runs.executor` on a row from
// before 2.3.0 says which engine ran it, and the 2.3.0 upgrade pass reads what
// jobs used to say (settings.shellServing22, a frozen copy of the old
// precedence, kept only there).
const (
	ExecutorSSH    = "ssh"
	ExecutorRunner = "runner"
)

// ScopeIsBound reports whether a scope (by name) is bound to at least one
// runner. An empty name, or one with no scopes row, is not.
func ScopeIsBound(ctx context.Context, q rowQueryer, scope string) (bool, error) {
	if strings.TrimSpace(scope) == "" {
		return false, nil
	}
	var bound bool
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
		                WHERE sc.name = ?)`, scope).Scan(&bound)
	return bound, err
}

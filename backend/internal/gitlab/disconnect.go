package gitlab

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/notices"
	"github.com/ResetSmith/cronomicon/internal/repoid"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// Removing a repository with its agency (2.4.0 Phase R6, GR-28; since Phase
// I3 only with its agency, §0a, IN-2 and IN-13: a repository is changed, not
// disconnected, and an agency is deleted when it is empty, with it).
//
// The removal takes every row the repository supplied, by the rules a sync
// prunes by when a file leaves the repository: its Git jobs and workflows with
// their schedule entries and pauses, its scripts, its Git schedules, its
// scopes with their host membership and imported host records. Then its sync
// problems and their notice, its connection, and its clone.
//
// What it leaves, as a prune leaves it: a job built in the app on one of its
// scripts is joined to nothing and does not run (the notice job_script_gone
// names it), and a definition built in the app on one of its schedules keeps
// the timing it has. The history of its syncs and publishes is kept: it is a
// record of what happened.
//
// A scope that is bound to runners and has work queued under its name is not
// pruned by a sync while that is so (it is "held", and goes at the first sync
// after its queue drains). A removal has no later sync to leave it to, so it
// is REFUSED while the repository has such a scope, and names them: the same
// answer renaming or deleting such a scope in the app gets.

// ErrGlobalRepo is the answer for Global's repository, which every
// installation has and Global, which is never deleted, keeps.
var ErrGlobalRepo = errors.New("Global's repository is never removed: Global is not deleted")

// ErrScopesBusy is the refusal while scopes of the repository are bound to
// runners and have work queued.
type ErrScopesBusy struct{ Scopes []string }

func (e *ErrScopesBusy) Error() string {
	return "the repository cannot be removed yet: " + strconv.Itoa(len(e.Scopes)) + " of its scopes are bound to runners and have runs " +
		"queued or parked (" + strings.Join(e.Scopes, ", ") + "). Cancel that work or let it finish, then delete the agency"
}

// RemoveWithAgency is the settings.RepoTake of an agency's delete. Called
// before the delete's transaction begins, it refuses a repository that is not
// there (settings.ErrRepoNotFound), Global's (ErrGlobalRepo) and one with a
// held scope (*ErrScopesBusy), then stops the repository's sync service and
// whatever it has in flight: nothing syncs into, or publishes from, a
// repository while its rows are removed, and a sync that wanted to write would
// otherwise wait on the transaction's write lock.
//
// remove deletes the repository's rows inside the transaction (asking about
// held scopes again there). finish must be called once, with whether the
// transaction committed: committed, it removes the clone and tells the
// scheduler; not, it starts the sync service again. Until finish, a Restart of
// the repository waits (restartMu): a connection saved meanwhile would
// otherwise start a Service for a row about to go.
func (r *Registry) RemoveWithAgency(ctx context.Context, repoID, actor string) (remove func(tx *sql.Tx) error, finish func(committed bool), err error) {
	if repoID == "" || repoID == repoid.Global {
		return nil, nil, ErrGlobalRepo
	}
	r.restartMu.Lock()
	if _, err := r.agencyOf(ctx, repoID); err != nil {
		r.restartMu.Unlock()
		return nil, nil, err
	}
	// Refused before anything is stopped: a refusal changes nothing.
	if busy, err := busyScopes(ctx, r.db, repoID); err != nil {
		r.restartMu.Unlock()
		return nil, nil, err
	} else if len(busy) > 0 {
		r.restartMu.Unlock()
		return nil, nil, &ErrScopesBusy{Scopes: busy}
	}
	r.Stop(repoID)

	var generation int64
	remove = func(tx *sql.Tx) error {
		// Asked again inside the transaction: a run queued since the first look.
		if busy, err := busyScopes(ctx, tx, repoID); err != nil {
			return err
		} else if len(busy) > 0 {
			return &ErrScopesBusy{Scopes: busy}
		}
		for _, st := range []struct{ what, query string }{
			// The statements of a sync's prune (syncNow), for the whole repository
			// and not for the rows a pass did not stamp. An agency's repository has
			// no row from before a repository was a row, so nothing is matched by
			// name.
			{"job schedules", `DELETE FROM definition_schedules WHERE owner_kind = 'job'
			     AND owner_uid IN (SELECT uid FROM jobs WHERE source = 'git' AND repo_id = ?1)`},
			{"jobs", `DELETE FROM jobs WHERE source = 'git' AND repo_id = ?1`},
			{"orphaned job pauses", `DELETE FROM paused_jobs WHERE owner_kind = 'job' AND source = 'git'
			     AND owner_uid IS NOT NULL AND owner_uid NOT IN (SELECT uid FROM jobs WHERE source = 'git') AND ?1 <> ''`},
			{"workflow schedules", `DELETE FROM definition_schedules WHERE owner_kind = 'workflow'
			     AND owner_uid IN (SELECT uid FROM workflows WHERE source = 'git' AND repo_id = ?1)`},
			{"workflows", `DELETE FROM workflows WHERE source = 'git' AND repo_id = ?1`},
			{"orphaned workflow pauses", `DELETE FROM paused_jobs WHERE owner_kind = 'workflow' AND source = 'git'
			     AND owner_uid IS NOT NULL AND owner_uid NOT IN (SELECT uid FROM workflows WHERE source = 'git') AND ?1 <> ''`},
			{"scripts", `DELETE FROM scripts WHERE repo_id = ?1`},
			{"schedules", `DELETE FROM schedules WHERE source = 'git' AND repo_id = ?1`},
			{"scope hosts", `DELETE FROM scope_hosts WHERE scope_id IN (SELECT id FROM scopes WHERE source = 'git' AND repo_id = ?1)`},
			{"imported ssh hosts", `DELETE FROM ssh_hosts WHERE source = 'git'
			     AND scope_id IN (SELECT id FROM scopes WHERE source = 'git' AND repo_id = ?1)`},
			{"scopes", `DELETE FROM scopes WHERE source = 'git' AND repo_id = ?1`},
			// A calendar records its agency, not its repository; an agency's Git
			// calendars are its repository's (2.4.0 Phase I3: left behind until
			// then, owned by an agency whose repository had gone).
			{"calendars", `DELETE FROM calendars WHERE source = 'git'
			     AND owner_agency = (SELECT agency_id FROM git_repos WHERE id = ?1)`},
			{"sync problems", `DELETE FROM git_sync_problems WHERE repo_id = ?1`},
			{"the connection", `DELETE FROM git_repos WHERE id = ?1`},
		} {
			if _, err := tx.ExecContext(ctx, st.query, repoID); err != nil {
				return fmt.Errorf("remove the repository's %s: %w", st.what, err)
			}
		}
		// The scheduler reloads on the definitions generation (GR-29): the jobs and
		// workflows that have gone stop firing.
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO definitions_generation (id, n) VALUES (1, 1)
			ON CONFLICT(id) DO UPDATE SET n = n + 1
			RETURNING n`).Scan(&generation); err != nil {
			return fmt.Errorf("remove the repository: advance the definitions generation: %w", err)
		}
		return nil
	}

	finish = func(committed bool) {
		r.restartMu.Unlock()
		if !committed {
			// Its Service was stopped and the delete then did not happen.
			if rerr := r.restart(repoID, false); rerr != nil {
				r.logWarn("the agency was not deleted, and its repository's sync service could not be started again", "repo_id", repoID, "err", rerr)
			}
			return
		}
		// After the commit, and none of it undoes the removal if it fails.
		if err := notices.Resolve(ctx, r.db, notices.KindGitSyncProblems, repoID); err != nil {
			r.logWarn("the repository's sync-problems notice was not resolved", "repo_id", repoID, "err", err)
		}
		if dir := r.cloneDir(repoID); dir != "" && dir != r.cloneDir(repoid.Global) {
			if err := os.RemoveAll(dir); err != nil {
				r.logWarn("the repository's clone was not removed", "repo_id", repoID, "dir", dir, "err", err)
			}
		}
		// Global's: its agency is deleted with it (C1).
		_ = settings.WriteChangeLog(ctx, r.db, agencyid.Global, actor, "Settings", "deleted", "Repository connection", repoID)
		r.mu.Lock()
		hook := r.onSyncComplete
		r.mu.Unlock()
		if hook != nil {
			hook(context.WithoutCancel(ctx), strconv.FormatInt(generation, 10))
		}
	}
	return remove, finish, nil
}

// busyScopes names the repository's scopes that a sync would hold back: bound
// to runners, with work queued under their name (settings.BoundScopeBusySQL).
func busyScopes(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, repoID string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name FROM scopes WHERE source = 'git' AND repo_id = ? AND `+settings.BoundScopeBusySQL, repoID)
	if err != nil {
		return nil, fmt.Errorf("read the repository's scopes: %w", err)
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
	sort.Strings(out)
	return out, rows.Err()
}

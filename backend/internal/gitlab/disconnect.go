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

	"github.com/ResetSmith/cronomicon/internal/notices"
	"github.com/ResetSmith/cronomicon/internal/repoid"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// Disconnecting a repository (2.4.0 Phase R6; GR-28).
//
// A disconnect removes every row the repository supplied, by the rules a sync
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
// after its queue drains). A disconnect has no later sync to leave it to, so
// it is REFUSED while the repository has such a scope, and names them: the
// same answer renaming or deleting such a scope in the app gets.

// ErrGlobalRepo is Disconnect's answer for Global's repository, which every
// installation has: its connection is emptied, not removed.
var ErrGlobalRepo = errors.New("Global's repository cannot be disconnected; clear its URL to stop syncing it")

// ErrScopesBusy is Disconnect's refusal while scopes of the repository are
// bound to runners and have work queued.
type ErrScopesBusy struct{ Scopes []string }

func (e *ErrScopesBusy) Error() string {
	return "the repository cannot be disconnected yet: " + strconv.Itoa(len(e.Scopes)) + " of its scopes are bound to runners and have runs " +
		"queued or parked (" + strings.Join(e.Scopes, ", ") + "). Cancel that work or let it finish, then disconnect"
}

// Disconnect removes a repository: its sync service, every row it supplied,
// its connection and its clone. It returns what was removed.
// settings.ErrRepoNotFound for a repository that is not there, ErrGlobalRepo
// for Global's, *ErrScopesBusy while one of its scopes is held.
func (r *Registry) Disconnect(ctx context.Context, repoID, actor string) (settings.RepoCounts, error) {
	var none settings.RepoCounts
	if repoID == "" || repoID == repoid.Global {
		return none, ErrGlobalRepo
	}
	removed, generation, putBack, err := r.removeRepo(ctx, repoID)
	if putBack {
		// Its Service was stopped and the disconnect was then refused.
		if rerr := r.restart(repoID, false); rerr != nil {
			r.logWarn("disconnect refused, and the repository's sync service could not be started again", "repo_id", repoID, "err", rerr)
		}
	}
	if err != nil {
		return none, err
	}

	// After the commit, and none of it undoes the disconnect if it fails.
	if err := notices.Resolve(ctx, r.db, notices.KindGitSyncProblems, repoID); err != nil {
		r.logWarn("disconnect: the repository's sync-problems notice was not resolved", "repo_id", repoID, "err", err)
	}
	if dir := r.cloneDir(repoID); dir != "" && dir != r.cloneDir(repoid.Global) {
		if err := os.RemoveAll(dir); err != nil {
			r.logWarn("disconnect: the repository's clone was not removed", "repo_id", repoID, "err", err)
		}
	}
	_ = settings.WriteChangeLog(ctx, r.db, actor, "Settings", "deleted", "Repository connection", repoID)
	r.mu.Lock()
	hook := r.onSyncComplete
	r.mu.Unlock()
	if hook != nil {
		hook(context.WithoutCancel(ctx), strconv.FormatInt(generation, 10))
	}
	return removed, nil
}

// removeRepo is the part of Disconnect that is one at a time with a Restart of
// the same repository (restartMu): a connection saved while it runs would
// otherwise start a Service for a row about to go. putBack: the Service was
// stopped and the disconnect then refused, so the caller starts it again.
func (r *Registry) removeRepo(ctx context.Context, repoID string) (removed settings.RepoCounts, generation int64, putBack bool, err error) {
	var none settings.RepoCounts
	r.restartMu.Lock()
	defer r.restartMu.Unlock()

	if _, err := r.agencyOf(ctx, repoID); err != nil {
		return none, 0, false, err
	}
	// Refused before anything is stopped: a refusal changes nothing.
	if busy, err := busyScopes(ctx, r.db, repoID); err != nil {
		return none, 0, false, err
	} else if len(busy) > 0 {
		return none, 0, false, &ErrScopesBusy{Scopes: busy}
	}

	// The Service first, and whatever it has in flight: nothing syncs into, or
	// publishes from, a repository while its rows are removed.
	r.Stop(repoID)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return none, 0, true, fmt.Errorf("disconnect: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Asked again inside the transaction: a run queued since the first look.
	if busy, err := busyScopes(ctx, tx, repoID); err != nil {
		return none, 0, true, err
	} else if len(busy) > 0 {
		return none, 0, true, &ErrScopesBusy{Scopes: busy}
	}

	if removed, err = settings.CountRepoRows(ctx, tx, repoID); err != nil {
		return none, 0, true, err
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
			return none, 0, true, fmt.Errorf("disconnect: remove the repository's %s: %w", st.what, err)
		}
	}
	// The scheduler reloads on the definitions generation (GR-29): the jobs and
	// workflows that have gone stop firing.
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO definitions_generation (id, n) VALUES (1, 1)
		ON CONFLICT(id) DO UPDATE SET n = n + 1
		RETURNING n`).Scan(&generation); err != nil {
		return none, 0, true, fmt.Errorf("disconnect: advance the definitions generation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return none, 0, true, fmt.Errorf("disconnect: %w", err)
	}
	return removed, generation, false, nil
}

// busyScopes names the repository's scopes that a sync would hold back: bound
// to runners, with work queued under their name (settings.BoundScopeBusySQL).
func busyScopes(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, repoID string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name FROM scopes WHERE source = 'git' AND repo_id = ? AND `+settings.BoundScopeBusySQL, repoID)
	if err != nil {
		return nil, fmt.Errorf("disconnect: read the repository's scopes: %w", err)
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

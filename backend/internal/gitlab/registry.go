package gitlab

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/repoid"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// Registry owns one Service per connected repository (2.4.0, GR-11).
//
// Until 2.4.0 there was one Service, built once when the routes were mounted
// and held by nothing: it resolved the repository's URL and token at start-up,
// so a changed connection took effect at the next restart of the server, and
// it could not be stopped. An agency administrator who connects their agency's
// repository cannot restart the server. The registry is what makes a
// connection a thing that can be written while the server runs:
//
//   - Start builds a Service for every row of git_repos and gives each a first
//     sync;
//   - Restart, called after a connection is written, stops the repository's
//     Service and builds a new one from the row as it now is, and syncs;
//   - Stop, called when a repository is disconnected, stops its Service.
//
// Every Service it builds shares ONE queue (the gate): a sync, a blocking
// sync and a publish, of any repository, run one at a time. That bounds what
// any number of repositories cost, and it is also what keeps two operations
// out of one clone at the same moment, which nothing did before: a publish and
// a sync, or a scope resync and a webhook's sync, could both be working in the
// one working tree.
type Registry struct {
	db  *sql.DB
	log *slog.Logger
	cfg *config.Config
	// cloneDir gives a repository's clone directory. CloneDirFor, unless a test
	// replaces it.
	cloneDir func(repoID string) string

	gate chan struct{}

	// restartMu makes a Restart one step: read the row, build, swap. Two
	// Restarts of one repository that overlapped could otherwise leave the
	// Service built from the OLDER read registered, or one of them unstopped.
	restartMu sync.Mutex
	// retired counts the Services a Restart has replaced and halted that have
	// not finished unwinding. Nobody waits for them when they are replaced;
	// Close does, so that a shutdown does not close the database under one.
	retired sync.WaitGroup

	mu             sync.Mutex
	services       map[string]*Service
	onSyncComplete func(ctx context.Context, sha string)
	// base is the context every Service's lifetime descends from: the process's,
	// so that a shutdown stops them all.
	base   context.Context
	closed bool
}

// NewRegistry builds an empty registry. Nothing runs until Start.
func NewRegistry(database *sql.DB, log *slog.Logger, cfg *config.Config) *Registry {
	return &Registry{
		db:       database,
		log:      log,
		cfg:      cfg,
		cloneDir: CloneDirFor,
		gate:     make(chan struct{}, 1),
		services: map[string]*Service{},
		base:     context.Background(),
	}
}

// SetOnSyncComplete installs the hook every Service calls after a sync (the
// scheduler's reload). Set it before Start.
func (r *Registry) SetOnSyncComplete(f func(ctx context.Context, sha string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onSyncComplete = f
	for _, svc := range r.services {
		svc.SetOnSyncComplete(f)
	}
}

// Start builds a Service for every repository and gives each its first sync
// (labelled "poll" in the history, as the start-up sync always was). ctx is the
// lifetime of everything the registry runs: when it ends, every Service stops.
func (r *Registry) Start(ctx context.Context) error {
	r.mu.Lock()
	r.base = ctx
	r.mu.Unlock()

	ids, err := r.repoIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := r.restart(id, true); err != nil {
			r.logWarn("git: a repository's sync service could not be started", "repo_id", id, "error", err)
		}
	}
	return nil
}

// repoIDs lists the repositories, Global's first. Global's is listed even when
// its row is missing (it cannot be, since migration 1310; a hand-built test
// database may have none): the routes that existed before 2.4.0 mean Global's
// repository and need a Service to answer them.
func (r *Registry) repoIDs(ctx context.Context) ([]string, error) {
	ids := []string{repoid.Global}
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM git_repos WHERE id <> ?`, repoid.Global)
	if err != nil {
		return ids, nil //nolint:nilerr // no table yet: Global's alone, as before 1310
	}
	defer rows.Close()
	var rest []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		rest = append(rest, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(rest)
	return append(ids, rest...), nil
}

// Service returns the running Service of a repository, or nil when it has none
// (never connected, disconnected, or the registry is closed).
func (r *Registry) Service(repoID string) *Service {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.services[repoID]
}

// Global returns Global's repository's Service.
func (r *Registry) Global() *Service { return r.Service(repoid.Global) }

// Restart replaces a repository's Service with a new one built from the
// repository's row as it is now. It is what a write of the connection calls,
// and it is why such a write needs no restart of the server: the new Service
// resolves the URL and the token afresh, and syncs at once, so a repository
// that has just been connected gets its first sync here.
//
// Three things about HOW, each of which was got wrong once:
//
//   - The row is read BEFORE the running Service is touched. If it cannot be
//     read, Restart fails and the repository keeps the Service it has. Taking
//     "could not read" for "not configured" would replace a working Service
//     with one that has no URL.
//   - The row is read on the registry's own context, not the caller's. The
//     caller is a request, and a request that is abandoned (the page reloaded,
//     a proxy timed out) must not decide what the repository syncs from.
//   - The old Service is halted, NOT waited for, and the swap is one step:
//     there is no moment in which the repository has no Service, and a save
//     does not hang behind the part of a sync that cannot be interrupted. Two
//     Services of one repository are never in its clone together, because both
//     wait in the one queue; the old one's sync, cancelled, rolls back.
//
// The caller's context is not used.
func (r *Registry) Restart(_ context.Context, repoID string) error {
	return r.restart(repoID, false)
}

// restart is Restart. atStart: the server is starting, and the repository gets
// its sync whether or not it has a URL, as the start-up sync always ran (an
// installation with none records "not configured" once per start). A written
// connection with no URL has nothing to sync and records nothing.
func (r *Registry) restart(repoID string, atStart bool) error {
	r.restartMu.Lock()
	defer r.restartMu.Unlock()

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return fmt.Errorf("the repository registry is closed")
	}
	base := r.base
	hook := r.onSyncComplete
	r.mu.Unlock()

	agency, err := r.agencyOf(base, repoID)
	if err != nil {
		return err
	}
	repoURL, token, err := settings.ResolveRepoRuntime(base, r.db, r.cfg, repoID)
	if err != nil {
		return err
	}
	svc := NewService(r.db, r.log, repoURL, token, r.cloneDir(repoID), "")
	if repoID == repoid.Global && r.cfg != nil {
		// The secret read from the environment at start-up is Global's alone (GR-21).
		svc.webhookSecret = r.cfg.WebhookSecret
	}
	svc.repoID = repoID
	svc.agencyID = agency
	svc.Cfg = r.cfg
	svc.gate = r.gate
	svc.ctx, svc.cancel = context.WithCancel(base)
	if hook != nil {
		svc.SetOnSyncComplete(hook)
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		svc.halt()
		return fmt.Errorf("the repository registry is closed")
	}
	old := r.services[repoID]
	r.services[repoID] = svc
	r.mu.Unlock()
	if old != nil {
		old.halt()
		// Under restartMu, which Close takes before it waits: no Add can come
		// after Close has started to wait.
		r.retired.Add(1)
		go func() {
			defer r.retired.Done()
			old.wg.Wait()
		}()
	}

	if atStart || repoURL != "" {
		svc.TriggerSync(base, "poll")
	} else {
		r.logInfo("git: the repository's connection has no URL; nothing to sync", "repo_id", repoID)
	}
	return nil
}

// agencyOf reads the agency a repository belongs to. Global's is Global's
// whether or not its row is there; any other repository without a row does not
// exist. A read that FAILED is an error for both: see Restart.
func (r *Registry) agencyOf(ctx context.Context, repoID string) (string, error) {
	var agency string
	err := r.db.QueryRowContext(ctx, `SELECT agency_id FROM git_repos WHERE id = ?`, repoID).Scan(&agency)
	switch {
	case err == nil:
		return agency, nil
	case errors.Is(err, sql.ErrNoRows) && repoID == repoid.Global:
		return "", nil // Service.agency() reads empty as Global
	case errors.Is(err, sql.ErrNoRows):
		return "", settings.ErrRepoNotFound
	default:
		return "", fmt.Errorf("read the repository's row: %w", err)
	}
}

// Stop stops a repository's Service and forgets it: the repository was
// disconnected. A sync in flight is cancelled and waited for.
func (r *Registry) Stop(repoID string) {
	r.mu.Lock()
	svc := r.services[repoID]
	delete(r.services, repoID)
	r.mu.Unlock()
	if svc != nil {
		svc.Stop()
	}
}

// Close stops every Service. The registry starts nothing after it.
func (r *Registry) Close() {
	// After any Restart that is under way: it either registers its Service
	// before this collects them, or finds the registry closed.
	r.restartMu.Lock()
	r.mu.Lock()
	r.closed = true
	all := make([]*Service, 0, len(r.services))
	for id, svc := range r.services {
		all = append(all, svc)
		delete(r.services, id)
	}
	r.mu.Unlock()
	r.restartMu.Unlock()
	for _, svc := range all {
		svc.halt()
	}
	for _, svc := range all {
		svc.Stop()
	}
	// And the ones a Restart replaced earlier, which were halted then.
	r.retired.Wait()
}

func (r *Registry) logWarn(msg string, args ...any) {
	if r.log != nil {
		r.log.Warn(msg, args...)
	}
}

func (r *Registry) logInfo(msg string, args ...any) {
	if r.log != nil {
		r.log.Info(msg, args...)
	}
}

// CloneDirFor is the directory a repository is cloned into (GR-12).
//
// Global's is the directory the one repository always had: DefaultCloneDir,
// which is CRONOMICON_GIT_CACHE_DIR when that is set. It is adopted in place,
// so an upgrade clones nothing again. Every other repository's is a directory
// named by its id BESIDE Global's: <cache>/<repo id>, where <cache> is the
// directory that holds Global's clone. (CRONOMICON_GIT_CACHE_DIR names Global's
// clone directory itself, not a parent, so "the cache" is its parent.)
func CloneDirFor(repoID string) string {
	global := DefaultCloneDir()
	if repoID == "" || repoID == repoid.Global {
		return global
	}
	return filepath.Join(filepath.Dir(filepath.Clean(global)), cloneDirName(repoID))
}

// cloneDirName is a repository's id as one path element. Ids are generated and
// have no separator; this makes sure of it, since the result is joined onto a
// directory that also holds Global's clone.
func cloneDirName(repoID string) string {
	name := filepath.Base(filepath.Clean(string(os.PathSeparator) + repoID))
	if name == "" || name == "." || name == string(os.PathSeparator) {
		return "repo"
	}
	if name == filepath.Base(DefaultCloneDir()) {
		// A repository whose id happens to be the name of Global's directory
		// must not be handed Global's clone.
		return name + ".repo"
	}
	return name
}

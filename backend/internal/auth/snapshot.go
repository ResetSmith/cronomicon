package auth

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// The grant snapshot (LR-78).
//
// Until v2.3.0 a user's grants were expanded to scope names at login and frozen
// into the session cookie (RB-Q10). Two things followed from that. An
// administrator who created or renamed a scope could not use it until they
// signed in again, and to keep frozen grants from outliving a change every RBAC
// write bumped one global session epoch — so an edit in one agency signed out
// every user of every other.
//
// Grants are now resolved per request, for all three kinds of caller (session
// cookie, trusted header, service token), from one in-memory copy of the three
// tables that decide them: access_grants, scope_agencies ⋈ scopes, and the
// agency names. The cookie carries who the user is and which groups the
// identity provider put them in, and nothing about what those groups may do.
//
// The copy is refreshed lazily. A writer of any of those tables calls
// GrantsChanged, which only advances a counter; the next request that needs
// grants sees a snapshot built at an older count and rebuilds before answering,
// so a write is visible to the request that follows it. A short TTL is the
// backstop for writers this process cannot see: `cronomicon grant-admin`, which
// runs in another process, and a row edited by hand.
//
// A rebuild that fails keeps the last good snapshot in force and logs once per
// outage: a database that cannot be read must not turn every signed-in user
// into one with no access. That tolerance is bounded (grantSnapshotMaxStale):
// a snapshot known to be out of date is not served indefinitely, because the
// change it is missing may be a revocation. With no snapshot at all (a failure
// before the first success) resolution fails at once. Either way the callers
// fail closed.
//
// The rebuild does not run on the request's context. The request that happens
// to trigger it may belong to a client that has already gone away, and a
// rebuild cancelled with it would leave the stale snapshot in force for
// everyone else — a way to keep a revoked grant alive by aborting requests.

// grantSnapshotTTL bounds how long a change this process did not make can go
// unseen.
const grantSnapshotTTL = 30 * time.Second

// grantRebuildBackoff spaces rebuild attempts while the database is failing,
// so an outage costs one failed read a second and not one per request.
const grantRebuildBackoff = time.Second

// grantRebuildTimeout bounds one rebuild. It is its own deadline, not the
// triggering request's.
const grantRebuildTimeout = 5 * time.Second

// grantSnapshotMaxStale is how long a snapshot may be served while every
// rebuild since has failed. Past it, resolution fails and requests are refused:
// by then the database has been unreadable for minutes and nothing else works
// either, so refusing costs little and serving an old answer could cost a
// revocation.
const grantSnapshotMaxStale = 2 * time.Minute

// grantsGeneration counts changes to the tables the snapshot is built from. It
// is process-wide rather than per Service because the writers (the settings
// and gitlab packages, the API handlers) hold a *sql.DB and no Service; a
// Service whose database was not the one written merely rebuilds once more
// than it needed to.
var grantsGeneration atomic.Uint64

// GrantsChanged tells every Service that access_grants, scope_agencies, a
// scope's existence or name, or an agency's name may have changed. Call it
// AFTER the write has committed. It is cheap and safe to call when nothing
// changed.
func GrantsChanged() { grantsGeneration.Add(1) }

// GrantsGeneration reports how many times GrantsChanged has been called. It
// exists so a writer's own package can test that it announces its changes
// without reaching into a Service.
func GrantsGeneration() uint64 { return grantsGeneration.Load() }

// grantRow is one access_grants row, unexpanded.
type grantRow struct {
	role, agencyID string
	all            bool
}

// grantSnapshot is an immutable copy of what decides a group's grants. Nothing
// reachable from it is modified after it is published; resolve hands out copies
// of the scope lists for that reason.
type grantSnapshot struct {
	gen            uint64
	builtAt        time.Time
	byGroup        map[string][]grantRow
	scopesByAgency map[string][]string // scope names, sorted
	agencyNames    map[string]string
}

// grantStore holds a Service's current snapshot. The zero value is ready to
// use, so a Service built as a struct literal (tests) needs no constructor.
type grantStore struct {
	cur atomic.Pointer[grantSnapshot]

	mu           sync.Mutex // serialises rebuilds
	lastFailed   time.Time  // guarded by mu
	failingSince time.Time  // guarded by mu; zero while healthy. One log line per outage.
}

func (g *grantSnapshot) fresh() bool {
	return g != nil && g.gen == grantsGeneration.Load() && time.Since(g.builtAt) < grantSnapshotTTL
}

// snapshot returns the snapshot to resolve against, rebuilding it first when a
// writer has announced a change or the TTL has passed.
func (s *Service) snapshot(ctx context.Context) (*grantSnapshot, error) {
	if cur := s.grants.cur.Load(); cur.fresh() {
		return cur, nil
	}
	s.grants.mu.Lock()
	defer s.grants.mu.Unlock()
	cur := s.grants.cur.Load()
	if cur.fresh() {
		return cur, nil // another request rebuilt while this one waited
	}
	failing := !s.grants.failingSince.IsZero()
	tooStale := func() bool {
		return cur == nil || time.Since(s.grants.failingSince) > grantSnapshotMaxStale
	}
	if failing && time.Since(s.grants.lastFailed) < grantRebuildBackoff {
		if tooStale() {
			return nil, errGrantSnapshotStale
		}
		return cur, nil
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grantRebuildTimeout)
	defer cancel()
	next, err := loadGrantSnapshot(rctx, s.db)
	if err != nil {
		s.grants.lastFailed = time.Now()
		if !failing {
			s.grants.failingSince = s.grants.lastFailed
			s.log.Error("grant snapshot rebuild failed; authorising on the last good snapshot",
				"error", err, "for_at_most", grantSnapshotMaxStale.String())
		}
		if tooStale() {
			return nil, err
		}
		return cur, nil
	}
	if failing {
		s.grants.failingSince = time.Time{}
		s.log.Info("grant snapshot rebuild recovered")
	}
	s.grants.cur.Store(next)
	return next, nil
}

// errGrantSnapshotStale is returned between rebuild attempts once the last
// good snapshot has outlived grantSnapshotMaxStale.
var errGrantSnapshotStale = errors.New("grant snapshot is stale and cannot be rebuilt")

// loadGrantSnapshot reads the three tables in ONE statement, so the copy is a
// state the database was actually in: read as three queries, a grant removed
// and a scope moved between them could combine into access nobody ever held.
//
// The generation is read BEFORE the query: a writer that commits while it runs
// leaves the snapshot stamped with the older count, so the next request
// rebuilds rather than trusting a copy that may have missed the write.
func loadGrantSnapshot(ctx context.Context, db *sql.DB) (*grantSnapshot, error) {
	snap := &grantSnapshot{
		gen:            grantsGeneration.Load(),
		builtAt:        time.Now(),
		byGroup:        map[string][]grantRow{},
		scopesByAgency: map[string][]string{},
		agencyNames:    map[string]string{},
	}
	rows, err := db.QueryContext(ctx, `
		SELECT 'grant', ad_group, role, COALESCE(agency_id,''), all_scopes FROM access_grants
		UNION ALL
		SELECT 'scope', sa.agency_id, s.name, '', 0
		  FROM scope_agencies sa JOIN scopes s ON s.id = sa.scope_id
		UNION ALL
		SELECT 'agency', id, name, '', 0 FROM agencies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, a, b, c string
		var all int
		if err := rows.Scan(&kind, &a, &b, &c, &all); err != nil {
			return nil, err
		}
		switch kind {
		case "grant":
			snap.byGroup[a] = append(snap.byGroup[a], grantRow{role: CanonRole(b), agencyID: c, all: all != 0})
		case "scope":
			snap.scopesByAgency[a] = append(snap.scopesByAgency[a], b)
		case "agency":
			snap.agencyNames[a] = b
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, scopes := range snap.scopesByAgency {
		sort.Strings(scopes)
	}
	return snap, nil
}

// ResolveGrants resolves a user's AD groups to their grants, expanding each
// agency-shaped grant to the scopes in that agency, straight from the database.
//
// Requests do not call it: a session, a trusted-header request and a service
// token resolve through a Service's cached snapshot (grantsFor). This is the
// same loader and the same expansion with no cache, for a caller that has a
// database and no Service — the `grant-admin` tests, and the unit tests of what
// a grant row means.
//
// Group matching is EXACT: ad_group is plain TEXT with no COLLATE NOCASE and
// the claim is trimmed but not case-folded, so a grant stored in a different
// case grants nothing.
func ResolveGrants(ctx context.Context, db *sql.DB, groups []string) ([]RoleGrant, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	snap, err := loadGrantSnapshot(ctx, db)
	if err != nil {
		return nil, err
	}
	return snap.resolve(groups), nil
}

// resolve expands a user's groups to their grants. Group matching is EXACT,
// for the reason ResolveGrants gives.
func (g *grantSnapshot) resolve(groups []string) []RoleGrant {
	var raw []grantRow
	for _, group := range groups {
		raw = append(raw, g.byGroup[group]...)
	}
	return expandGrants(raw, func(agencyID string) []string {
		// A copy: the snapshot's own slice is shared by every request, and an
		// Identity's scope list is a plain slice any holder may sort or append to.
		return slices.Clone(g.scopesByAgency[agencyID])
	})
}

// grantsFor resolves a user's groups through the snapshot.
func (s *Service) grantsFor(ctx context.Context, groups []string) ([]RoleGrant, error) {
	snap, err := s.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return snap.resolve(groups), nil
}

// agencyScopesFor returns the scope names an agency holds, through the
// snapshot. It is the service-token half: a token has no groups and no
// access_grants row, only an agency whose scopes it covers.
func (s *Service) agencyScopesFor(ctx context.Context, agencyID string) ([]string, error) {
	snap, err := s.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return slices.Clone(snap.scopesByAgency[agencyID]), nil
}

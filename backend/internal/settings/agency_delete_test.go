package settings

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// From the review of I3: an agency's delete names everything that keeps it —
// a workflow, a schedule or a calendar built in the app, and a Git job its
// repository does not hold — and what arrives between the counts and the
// delete, after its repository was taken, is named too (the trigger refused
// it, and the answer named nothing).
func TestI3_AnAgencysDeleteNamesWhatKeepsIt(t *testing.T) {
	pool := membershipDB(t)
	ctx := context.Background()
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-ops', 'Ops', 't')`)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-ops', 'ag-ops', 'https://git.example.com/ops.git', 'main')`)
	finished := []bool{}
	take := func(_ context.Context, repoID string) (func(*sql.Tx) error, func(bool), error) {
		rm := func(tx *sql.Tx) error {
			_, err := tx.Exec(`DELETE FROM git_repos WHERE id = ?`, repoID)
			return err
		}
		return rm, func(c bool) { finished = append(finished, c) }, nil
	}
	blocked := func(what string) *AgencyInUseError {
		t.Helper()
		_, err := DeleteAgencyWith(ctx, pool, "ag-ops", "root", take)
		var inUse *AgencyInUseError
		if !errors.As(err, &inUse) {
			t.Fatalf("deleting Ops with %s answered %v, want it refused", what, err)
		}
		return inUse
	}

	for _, c := range []struct{ what, add, remove string }{
		{"a workflow built in the app", `INSERT INTO workflows (uid, name, source, steps, synced_at, owner_agency) VALUES ('w-ops', 'ops-flow', 'cronomicon', '[]', 't', 'ag-ops')`, `DELETE FROM workflows WHERE uid = 'w-ops'`},
		{"a schedule built in the app", `INSERT INTO schedules (uid, name, source, cron, content_hash, owner_agency) VALUES ('s-ops', 'ops-nightly', 'cronomicon', '0 2 * * *', 'h', 'ag-ops')`, `DELETE FROM schedules WHERE uid = 's-ops'`},
		{"a Git job its repository does not hold", `INSERT INTO jobs (uid, name, source, run_type, synced_at, owner_agency) VALUES ('j-stray', 'stray', 'git', 'bash', 't', 'ag-ops')`, `DELETE FROM jobs WHERE uid = 'j-stray'`},
	} {
		exec(c.add)
		if inUse := blocked(c.what); inUse.Definitions != 1 {
			t.Errorf("%s: %d definitions named, want 1 (%v)", c.what, inUse.Definitions, inUse)
		}
		exec(c.remove)
	}
	exec(`INSERT INTO calendars (source, name, created_at, owner_agency) VALUES ('cronomicon', 'ops-days', 't', 'ag-ops')`)
	if inUse := blocked("a calendar built in the app"); inUse.Calendars != 1 || !strings.Contains(inUse.Error(), "calendar built in the app") {
		t.Errorf("a calendar built in the app: %v", inUse)
	}
	exec(`DELETE FROM calendars WHERE name = 'ops-days'`)
	if n := countRows(t, pool, `SELECT COUNT(*) FROM git_repos WHERE id = 'repo-ops'`); n != 1 {
		t.Fatalf("a refused delete took the repository")
	}

	// Something arrives after the counts, as the repository is being taken
	// (committed by another writer before the delete's transaction takes the
	// write lock): the trigger refuses the delete, the answer names it, and
	// the repository is NOT taken (present defect 63: it was, and the agency
	// was left without its repository).
	arrives := func(ctx context.Context, repoID string) (func(*sql.Tx) error, func(bool), error) {
		if _, err := pool.Exec(`INSERT INTO jobs (uid, name, source, run_type, synced_at, owner_agency) VALUES ('j-late', 'late', 'cronomicon', 'bash', 't', 'ag-ops')`); err != nil {
			return nil, nil, err
		}
		return take(ctx, repoID)
	}
	finished = nil
	_, err := DeleteAgencyWith(ctx, pool, "ag-ops", "root", arrives)
	var inUse *AgencyInUseError
	if !errors.As(err, &inUse) || inUse.Definitions != 1 || len(inUse.Blockers()) == 0 {
		t.Fatalf("a job that arrived as the repository was taken: %v, want it named", err)
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM git_repos WHERE id = 'repo-ops'`); n != 1 {
		t.Errorf("a delete the trigger refused took the repository (present defect 63)")
	}
	if len(finished) != 1 || finished[0] {
		t.Errorf("the take was finished %v, want once, not committed", finished)
	}
	exec(`DELETE FROM jobs WHERE uid = 'j-late'`)
	finished = nil
	if ok, err := DeleteAgencyWith(ctx, pool, "ag-ops", "root", take); err != nil || !ok {
		t.Errorf("deleting the empty Ops: %v %v", ok, err)
	}
	if len(finished) != 1 || !finished[0] {
		t.Errorf("the take was finished %v, want once, committed", finished)
	}
}

// Present defects 62 and 63 (found by the review of 2.4.x): what the delete
// would take with it silently is refused by the delete itself, for every
// caller. Access grants and service accounts were the route's own checks
// (the create route's rollback did not ask them); an unspent registration
// token and a runner the agency owns that serves nothing were asked by
// nobody.
func TestRV_AnAgencysDeleteRefusesWhatItWouldTakeSilently(t *testing.T) {
	pool := membershipDB(t)
	ctx := context.Background()
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-ops', 'Ops', 't')`)
	for _, c := range []struct {
		what, add, remove string
		got               func(*AgencyInUseError) int
	}{
		{"an access grant",
			`INSERT INTO access_grants (id, ad_group, role, agency_id, created_at) VALUES ('g-ops', 'ops-viewers', 'viewer', 'ag-ops', 't')`,
			`DELETE FROM access_grants WHERE agency_id = 'ag-ops'`, func(e *AgencyInUseError) int { return e.Grants }},
		{"an unspent registration token",
			`INSERT INTO registration_tokens (token_hash, created_by, created_at, expires_at, agency_id) VALUES ('h', 'root', 't', '2999-01-01T00:00:00Z', 'ag-ops')`,
			`DELETE FROM registration_tokens WHERE agency_id = 'ag-ops'`, func(e *AgencyInUseError) int { return e.Tokens }},
		{"a runner it owns that serves nothing",
			`INSERT INTO runners (id, name, status, registered_at, created_at, owner_agency) VALUES ('rn-ops', 'ops-1', 'offline', 't', 't', 'ag-ops');
			 DELETE FROM runner_agencies WHERE runner_id = 'rn-ops'`,
			`DELETE FROM runners WHERE id = 'rn-ops'`, func(e *AgencyInUseError) int { return e.OwnedRunners }},
	} {
		exec(c.add)
		_, err := DeleteAgencyWith(ctx, pool, "ag-ops", "root", nil)
		var inUse *AgencyInUseError
		if !errors.As(err, &inUse) || c.got(inUse) != 1 || len(inUse.Blockers()) == 0 {
			t.Errorf("deleting Ops with %s answered %v, want it refused and named", c.what, err)
		}
		exec(c.remove)
	}
	// An expired token is no blocker: it can enrol nothing.
	exec(`INSERT INTO registration_tokens (token_hash, created_by, created_at, expires_at, agency_id) VALUES ('h1', 'root', 't', '2000-01-01T00:00:00Z', 'ag-ops')`)
	if ok, err := DeleteAgencyWith(ctx, pool, "ag-ops", "root", nil); err != nil || !ok {
		t.Errorf("deleting the empty Ops: %v %v", ok, err)
	}
}

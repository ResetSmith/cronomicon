package settings

import (
	"context"
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
	take := func(repoID string) error {
		_, err := pool.Exec(`DELETE FROM git_repos WHERE id = ?`, repoID)
		return err
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

	// Something arrives after the counts, as the repository is taken: the
	// trigger refuses the delete, and the answer names it.
	arrives := func(repoID string) error {
		if err := take(repoID); err != nil {
			return err
		}
		_, err := pool.Exec(`INSERT INTO jobs (uid, name, source, run_type, synced_at, owner_agency) VALUES ('j-late', 'late', 'cronomicon', 'bash', 't', 'ag-ops')`)
		return err
	}
	_, err := DeleteAgencyWith(ctx, pool, "ag-ops", "root", arrives)
	var inUse *AgencyInUseError
	if !errors.As(err, &inUse) || inUse.Definitions != 1 || len(inUse.Blockers()) == 0 {
		t.Fatalf("a job that arrived as the repository was taken: %v, want it named", err)
	}
	exec(`DELETE FROM jobs WHERE uid = 'j-late'`)
	if ok, err := DeleteAgencyWith(ctx, pool, "ag-ops", "root", take); err != nil || !ok {
		t.Errorf("deleting the empty Ops: %v %v", ok, err)
	}
}

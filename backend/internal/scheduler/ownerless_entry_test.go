package scheduler

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// A schedule entry that records no owner is joined to its definition by the
// name, and only where ONE definition holds the name (2.4.0; the owner's
// decision of 2026-10-13, R5's question 11).
//
// A name can be held by a definition in each agency. An entry with no owner
// under such a name joined every definition of the name: each ran at the
// other's times, on its own hosts. The upgrade to schedule identity (migration
// 1300) leaves exactly such entries as it finds them, since which definition
// each belongs to is not recorded. It fires neither now, and the log says so,
// once. An entry with no owner under a name that one definition holds fires as
// it always has, and so does every entry that records its owner.
func TestAScheduleEntryWithNoOwnerFiresOnlyWhereOneDefinitionHoldsItsName(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	for _, q := range []string{
		// Two agencies' jobs called "twin", and one called "solo".
		`INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, enabled, synced_at) VALUES
		      ('j-fin',  'twin', 'cronomicon', 'bash', 'Allow', 1, 't'),
		      ('j-tax',  'twin', 'cronomicon', 'bash', 'Allow', 1, 't'),
		      ('j-solo', 'solo', 'cronomicon', 'bash', 'Allow', 1, 't')`,
		// The same for workflows.
		`INSERT INTO workflows (uid, name, source, steps, enabled, created_at) VALUES
		      ('w-fin',  'flow',   'cronomicon', '[]', 1, 't'),
		      ('w-tax',  'flow',   'cronomicon', '[]', 1, 't'),
		      ('w-solo', 'weekly', 'cronomicon', '[]', 1, 't')`,
		`INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, owner_uid) VALUES
		      ('cronomicon', 'job',      'twin',   'whose-is-this',  '0 1 * * *', 0, NULL),
		      ('cronomicon', 'job',      'twin',   'fins-own',       '0 2 * * *', 1, 'j-fin'),
		      ('cronomicon', 'job',      'solo',   'solos-by-name',  '0 3 * * *', 0, NULL),
		      ('cronomicon', 'workflow', 'flow',   'whose-flow',     '0 4 * * *', 0, NULL),
		      ('cronomicon', 'workflow', 'flow',   'taxs-own',       '0 5 * * *', 1, 'w-tax'),
		      ('cronomicon', 'workflow', 'weekly', 'weekly-by-name', '0 6 * * *', 0, NULL)`,
	} {
		if _, err := pool.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}

	var logs bytes.Buffer
	s := New(pool, slog.New(slog.NewTextHandler(&logs, nil)), nil)
	var mu sync.Mutex
	flows := map[string]string{} // schedule entry → the workflow it started
	s.SetWorkflowFirer(func(_ context.Context, f WorkflowFire) {
		mu.Lock()
		flows[f.ScheduleName] = f.UID
		mu.Unlock()
	})
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	for _, e := range s.cr.Entries() {
		e.Job.Run()
	}

	// Jobs: what was queued, by schedule entry.
	queued := map[string]string{}
	rows, err := pool.QueryContext(ctx, `SELECT schedule_name, job_uid FROM runs WHERE status = 'queued' ORDER BY schedule_name, job_uid`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var sched, uid string
		if err := rows.Scan(&sched, &uid); err != nil {
			t.Fatal(err)
		}
		queued[sched] += uid + " "
	}
	rows.Close()
	if got := queued["whose-is-this"]; got != "" {
		t.Errorf("the entry with no owner under a name two jobs hold started %q, want nothing", got)
	}
	if got := queued["fins-own"]; got != "j-fin " {
		t.Errorf("the entry that records its owner started %q, want j-fin alone", got)
	}
	if got := queued["solos-by-name"]; got != "j-solo " {
		t.Errorf("the entry with no owner under a name one job holds started %q, want j-solo", got)
	}
	mu.Lock()
	if uid, fired := flows["whose-flow"]; fired {
		t.Errorf("the entry with no owner under a name two workflows hold started %q, want nothing", uid)
	}
	if flows["taxs-own"] != "w-tax" || flows["weekly-by-name"] != "w-solo" {
		t.Errorf("the workflows' other entries started %v, want taxs-own → w-tax and weekly-by-name → w-solo", flows)
	}
	mu.Unlock()

	// The log names each entry that was left out, and what to do.
	said := logs.String()
	for _, want := range []string{"entry=whose-is-this", "entry=whose-flow", "it does not fire", "Save the definition it belongs to again"} {
		if !strings.Contains(said, want) {
			t.Errorf("the log does not say %q:\n%s", want, said)
		}
	}
	for _, not := range []string{"entry=fins-own", "entry=solos-by-name", "entry=taxs-own", "entry=weekly-by-name"} {
		if strings.Contains(said, not) {
			t.Errorf("the log names an entry that fires (%s):\n%s", not, said)
		}
	}
	// Once: a second reload says nothing more.
	logs.Reset()
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("second reload: %v", err)
	}
	if strings.Contains(logs.String(), "it does not fire") {
		t.Errorf("the second reload said it again:\n%s", logs.String())
	}

	// A twin in the recycle bin is still a definition the entry may belong to.
	if _, err := pool.ExecContext(ctx, `UPDATE jobs SET deleted_at = 't' WHERE uid = 'j-tax'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `DELETE FROM runs`); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	for _, e := range s.cr.Entries() {
		e.Job.Run()
	}
	var n int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE schedule_name = 'whose-is-this'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("with one twin in the recycle bin the entry with no owner started %d runs (%v), want none: it may be the binned job's", n, err)
	}
	// The other twin is removed for good: one job holds the name, and the
	// entry is its.
	if _, err := pool.ExecContext(ctx, `DELETE FROM jobs WHERE uid = 'j-tax'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	for _, e := range s.cr.Entries() {
		e.Job.Run()
	}
	var uid string
	if err := pool.QueryRowContext(ctx, `SELECT job_uid FROM runs WHERE schedule_name = 'whose-is-this'`).Scan(&uid); err != nil || uid != "j-fin" {
		t.Errorf("with one job left holding the name, the entry with no owner started %q (%v), want j-fin", uid, err)
	}
}

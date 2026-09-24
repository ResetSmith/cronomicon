package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// RT-1 — the runner-tag pin (the runner-targeting plan, mig. 1070).
//
// These tests exist to hold two lines that are easy to "simplify" apart:
//   - an UNPINNED run must dispatch exactly as it did pre-1070 (the whole
//     feature is opt-in, and a regression here breaks every existing job);
//   - a pin NARROWS and never widens — it is ANDed with agency eligibility, so
//     it can never reach a runner agency isolation denies (RT-Q2).

func addRunnerTag(t *testing.T, svc *Service, runnerID, tag string) {
	t.Helper()
	if _, err := svc.db.Exec(
		`INSERT INTO runner_tags(runner_id, tag) VALUES(?,?)`, runnerID, tag); err != nil {
		t.Fatalf("addRunnerTag: %v", err)
	}
	// The projection is derived from runners.tags, which stays authoritative — a
	// fixture that wrote only one of the two would not be reproducing any state the
	// application can actually produce.
	var raw string
	if err := svc.db.QueryRow(`SELECT COALESCE(tags,'[]') FROM runners WHERE id=?`, runnerID).Scan(&raw); err != nil {
		t.Fatalf("addRunnerTag: read tags: %v", err)
	}
	var tags []string
	_ = json.Unmarshal([]byte(raw), &tags)
	b, _ := json.Marshal(append(tags, tag))
	if _, err := svc.db.Exec(`UPDATE runners SET tags=? WHERE id=?`, string(b), runnerID); err != nil {
		t.Fatalf("addRunnerTag: write tags: %v", err)
	}
}

// insertQueuedRunPinned inserts a queued runner run with an optional agency set
// and an optional runner-tag pin. agency=="" is the general pool; pin=="" is
// unpinned.
func insertQueuedRunPinned(t *testing.T, svc *Service, traceID, runType, agency, pin string) {
	t.Helper()
	aj := "[]"
	if agency != "" {
		b, _ := json.Marshal([]string{agency})
		aj = string(b)
	}
	var pinArg any
	if pin != "" {
		pinArg = pin // NULL when unset, matching what the enqueue path writes
	}
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, agencies_json, runner_tag, created_at)
		VALUES (?, 'j', ?, 'prod', 'queued', 'test', 'manual', 'runner', ?, ?, ?)`,
		traceID, runType, aj, pinArg, now()); err != nil {
		t.Fatalf("insertQueuedRunPinned: %v", err)
	}
	if agency != "" {
		if _, err := svc.db.Exec(`INSERT INTO run_agencies(run_id, agency) VALUES(?,?)`, traceID, agency); err != nil {
			t.Fatalf("insertQueuedRunPinned: materialize: %v", err)
		}
	}
}

// TestClaimRunPinMatrix is RT-1's keystone. Every row is a claim attempt against
// a fresh run, so no case can bleed into the next.
func TestClaimRunPinMatrix(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	insertRunner(t, svc, "r-dmz", "dmz", "online", []string{"bash"})
	insertRunner(t, svc, "r-plain", "plain", "online", []string{"bash"})
	insertRunner(t, svc, "r-core", "core", "online", []string{"bash"})
	addRunnerTag(t, svc, "r-dmz", "vlan-dmz")
	addRunnerTag(t, svc, "r-core", "vlan-core")

	try := func(agency, pin, runnerID string) bool {
		trace := db.NewTraceID()
		insertQueuedRunPinned(t, svc, trace, "bash", agency, pin)
		got, err := svc.claimRun(ctx, runnerID, []string{"bash"}, true)
		if err != nil {
			t.Fatalf("claimRun: %v", err)
		}
		claimed := got != nil && got.TraceID == trace
		_, _ = svc.db.Exec(`DELETE FROM runs WHERE id=?`, trace)
		return claimed
	}

	cases := []struct {
		name, agency, pin, runner string
		want                      bool
	}{
		// The no-regression case. If this ever fails, the feature is not optional
		// and every pre-1070 job in the fleet is affected.
		{"unpinned → untagged runner", "", "", "r-plain", true},
		{"unpinned → tagged runner", "", "", "r-dmz", true},

		{"pinned → runner carrying the tag", "", "vlan-dmz", "r-dmz", true},
		{"pinned → runner with no tags", "", "vlan-dmz", "r-plain", false},
		{"pinned → runner with a different tag", "", "vlan-dmz", "r-core", false},
		// A tag nothing carries is legal (the runner may be enrolled tomorrow,
		// RT-Q4) — it simply never claims.
		{"pinned → tag nobody carries", "", "vlan-lab", "r-dmz", false},
	}
	for _, c := range cases {
		if got := try(c.agency, c.pin, c.runner); got != c.want {
			t.Errorf("%s: claimed=%v want=%v", c.name, got, c.want)
		}
	}
}

// TestClaimRunPinNarrowsNeverWidens is the RT-Q2 invariant as an executable
// contract. A run bound to agency alpha and pinned to vlan-dmz must be claimable
// ONLY by a runner that is both — and in particular a correctly-tagged runner in
// the WRONG agency must still be refused. Rewriting the pin predicate as an OR,
// or folding it inside the agency parenthesis, passes the matrix above and fails
// precisely here.
func TestClaimRunPinNarrowsNeverWidens(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	insertAgencyRow(t, svc, "a1", "alpha")
	insertAgencyRow(t, svc, "a2", "beta")

	insertRunner(t, svc, "r-alpha-dmz", "alpha-dmz", "online", []string{"bash"})
	insertRunner(t, svc, "r-beta-dmz", "beta-dmz", "online", []string{"bash"})
	insertRunner(t, svc, "r-alpha-plain", "alpha-plain", "online", []string{"bash"})
	addRunnerAgency(t, svc, "r-alpha-dmz", "a1")
	addRunnerAgency(t, svc, "r-beta-dmz", "a2")
	addRunnerAgency(t, svc, "r-alpha-plain", "a1")
	addRunnerTag(t, svc, "r-alpha-dmz", "vlan-dmz")
	addRunnerTag(t, svc, "r-beta-dmz", "vlan-dmz")

	try := func(runnerID string) bool {
		trace := db.NewTraceID()
		insertQueuedRunPinned(t, svc, trace, "bash", "alpha", "vlan-dmz")
		got, err := svc.claimRun(ctx, runnerID, []string{"bash"}, true)
		if err != nil {
			t.Fatalf("claimRun: %v", err)
		}
		claimed := got != nil && got.TraceID == trace
		_, _ = svc.db.Exec(`DELETE FROM runs WHERE id=?`, trace)
		return claimed
	}

	if !try("r-alpha-dmz") {
		t.Error("right agency + right tag: want claimable")
	}
	if try("r-beta-dmz") {
		t.Error("WRONG agency + right tag: the pin widened agency reach — RT-Q2 violated")
	}
	if try("r-alpha-plain") {
		t.Error("right agency + no tag: want refused")
	}
}

// TestClaimRunPinAfterTagRemoved — the pin is evaluated live at claim time, not
// snapshotted at enqueue. Pulling a tag off the only matching runner leaves the
// run queued for a runner that carries it, rather than stranding it on a runner
// that no longer qualifies.
func TestClaimRunPinAfterTagRemoved(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	insertRunner(t, svc, "r-dmz", "dmz", "online", []string{"bash"})
	addRunnerTag(t, svc, "r-dmz", "vlan-dmz")

	trace := db.NewTraceID()
	insertQueuedRunPinned(t, svc, trace, "bash", "", "vlan-dmz")

	// Untag it mid-flight — the operator's edit takes effect on the next poll.
	if _, err := svc.db.Exec(`DELETE FROM runner_tags WHERE runner_id='r-dmz'`); err != nil {
		t.Fatalf("untag: %v", err)
	}
	got, err := svc.claimRun(ctx, "r-dmz", []string{"bash"}, true)
	if err != nil {
		t.Fatalf("claimRun: %v", err)
	}
	if got != nil {
		t.Fatalf("claimed %s after its tag was removed; want the run left queued", got.TraceID)
	}

	var status string
	if err := svc.db.QueryRow(`SELECT status FROM runs WHERE id=?`, trace).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want queued", status)
	}
}

// TestRunnerTagsCascadeOnDeregister — the projection's FK is what stops a
// re-enrolled host from inheriting a dead registration's pins. Deregistration
// DELETEs the runners row (register.go, reaper.go); with _foreign_keys=on that
// must take the projection rows with it.
func TestRunnerTagsCascadeOnDeregister(t *testing.T) {
	svc := newTestService(t)

	insertRunner(t, svc, "r-gone", "gone", "online", []string{"bash"})
	addRunnerTag(t, svc, "r-gone", "vlan-dmz")

	if _, err := svc.db.Exec(`DELETE FROM runners WHERE id='r-gone'`); err != nil {
		t.Fatalf("deregister: %v", err)
	}
	var n int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM runner_tags WHERE runner_id='r-gone'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("runner_tags rows after deregister = %d, want 0 (FK cascade)", n)
	}
}

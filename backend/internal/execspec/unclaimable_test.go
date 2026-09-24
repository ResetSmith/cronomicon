package execspec

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// RA-20(b) — the queued-reason diagnosis (the runas-update plan §11.4).
//
// Every claim gate that saves correctness presents to an operator as the same
// symptom: the run sits queued. These tests pin that each distinct CAUSE produces a
// distinct, actionable sentence — a diagnosis that says "queued" for five different
// reasons is the failure being fixed, not the fix.

type unclaimFixture struct {
	pool *sql.DB
	exec func(string, ...any)
}

func newUnclaimFixture(t *testing.T) *unclaimFixture {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "unclaimable.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies(id,name,created_at) VALUES('a1','alpha','t')`)
	exec(`INSERT INTO jobs(name,source,run_type,synced_at) VALUES('j','git','ansible','t')`)
	return &unclaimFixture{pool: pool, exec: exec}
}

// seedRun inserts a queued run. agenciesJSON "[]" is the general pool.
func (f *unclaimFixture) seedRun(id, runType, agenciesJSON, requiresJSON string) {
	f.exec(`INSERT INTO runs(id, job_name, job_source, run_type, status, triggered_by, trigger_kind,
	                         agencies_json, requires_json, created_at)
	        VALUES(?, 'j', 'git', ?, 'queued', 'seed', 'manual', ?, ?, 't')`, id, runType, agenciesJSON, requiresJSON)
}

func (f *unclaimFixture) runner(id, status string, caps string, agency string, injection int) {
	f.exec(`INSERT INTO runners(id,name,status,capabilities,allow_secret_injection,registered_at,created_at)
	        VALUES(?,?,?,?,?,'t','t')`, id, id, status, caps, injection)
	if agency != "" {
		f.exec(`INSERT INTO runner_agencies(runner_id,agency_id) VALUES(?,?)`, id, agency)
	}
}

func (f *unclaimFixture) reason(t *testing.T, runID string) string {
	t.Helper()
	got, err := UnclaimableReason(context.Background(), f.pool, runID)
	if err != nil {
		t.Fatalf("UnclaimableReason: %v", err)
	}
	return got
}

// TestUnclaimableReasonNarrowsToTheRealCause walks the gates in order. Each case
// fixes the previous one's complaint, so the sentence must move on to the next
// genuine problem rather than repeating itself.
func TestUnclaimableReasonNarrowsToTheRealCause(t *testing.T) {
	f := newUnclaimFixture(t)
	f.seedRun("run-1", "ansible", `["alpha"]`, `["become-file"]`)

	// 1. Nothing online at all.
	if got := f.reason(t, "run-1"); !strings.Contains(got, "no runner is online") {
		t.Errorf("empty fleet = %q, want it to say nothing is online", got)
	}

	// 2. Online, but cannot run ansible.
	f.runner("r-bash", "online", `["bash"]`, "a1", 1)
	if got := f.reason(t, "run-1"); !strings.Contains(got, "ansible") {
		t.Errorf("wrong-type fleet = %q, want it to name the run type", got)
	}

	// 3. Runs ansible, but belongs to no agency (so it cannot take a tagged run).
	f.runner("r-gen", "online", `["ansible"]`, "", 1)
	got := f.reason(t, "run-1")
	if !strings.Contains(got, "alpha") {
		t.Errorf("no-member fleet = %q, want it to name the agency the run needs", got)
	}

	// 4. In the agency, but not flagged for secret injection — and the run's job
	// declares a binding, which is what makes the flag load-bearing.
	f.exec(`INSERT INTO reference_bindings(owner_kind,owner_source,owner_name,ref_kind,ref_name,created_at)
	        VALUES('job','git','j','secret','S','t')`)
	f.runner("r-noinject", "online", `["ansible"]`, "a1", 0)
	got = f.reason(t, "run-1")
	if !strings.Contains(got, "secret injection") {
		t.Errorf("un-flagged fleet = %q, want it to name the injection flag", got)
	}

	// 5. Flagged and in the agency, but missing the capability token.
	f.runner("r-noflag", "online", `["ansible"]`, "a1", 1)
	got = f.reason(t, "run-1")
	if !strings.Contains(got, "become-file") {
		t.Errorf("missing-token fleet = %q, want it to name the token", got)
	}

	// 6. Everything satisfied ⇒ no reason at all. A diagnosis that fires on a
	// healthy run would put a scary sentence on every normal queue.
	f.runner("r-good", "online", `["ansible","become-file"]`, "a1", 1)
	if got := f.reason(t, "run-1"); got != "" {
		t.Errorf("claimable run = %q, want no reason", got)
	}
}

// TestUnclaimableReasonExplainsTheUnboundTrap — the general-pool rule is DISJOINT
// (AG-Q3a): an untagged run is claimable ONLY by a runner with no agencies. On a
// fully departmentalised fleet that means an unbound run is claimable by nothing,
// which reads as a broken queue unless the message says so. This is RA-24's failure
// mode #2, met by an operator who somehow got the run enqueued anyway.
func TestUnclaimableReasonExplainsTheUnboundTrap(t *testing.T) {
	f := newUnclaimFixture(t)
	f.seedRun("run-unbound", "ansible", `[]`, "")
	f.runner("r-dept", "online", `["ansible"]`, "a1", 1) // every runner is departmental

	got := f.reason(t, "run-unbound")
	if !strings.Contains(got, "no scope") || !strings.Contains(got, "bind a scope") {
		t.Errorf("unbound run on a departmentalised fleet = %q, want it to explain that "+
			"only an un-agencied runner can take it and name the remedy", got)
	}
}

// TestStampUnclaimableReasonOnlyTouchesQueuedRuns — the hint is written after the
// INSERT, so a fast runner may already have claimed the run. Stamping a live run
// with "nobody can claim this" would be worse than silence.
func TestStampUnclaimableReasonOnlyTouchesQueuedRuns(t *testing.T) {
	f := newUnclaimFixture(t)
	f.seedRun("run-q", "ansible", `["alpha"]`, "")
	f.seedRun("run-live", "ansible", `["alpha"]`, "")
	f.exec(`UPDATE runs SET status='running' WHERE id='run-live'`)

	ctx := context.Background()
	if _, err := StampUnclaimableReason(ctx, f.pool, "run-q"); err != nil {
		t.Fatalf("stamp queued: %v", err)
	}
	if _, err := StampUnclaimableReason(ctx, f.pool, "run-live"); err != nil {
		t.Fatalf("stamp running: %v", err)
	}

	var queuedReason, liveReason string
	_ = f.pool.QueryRow(`SELECT COALESCE(queued_reason,'') FROM runs WHERE id='run-q'`).Scan(&queuedReason)
	_ = f.pool.QueryRow(`SELECT COALESCE(queued_reason,'') FROM runs WHERE id='run-live'`).Scan(&liveReason)
	if queuedReason == "" {
		t.Error("queued run got no reason, want one")
	}
	if liveReason != "" {
		t.Errorf("running run was stamped %q, want it left alone", liveReason)
	}
}

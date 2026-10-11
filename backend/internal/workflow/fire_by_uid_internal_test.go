package workflow

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/scheduler"
)

// The server's half of present defect 34: what starts a workflow for a
// schedule entry or a parked run found it by source and name, and the run was
// stamped with whichever workflow the name found. Two agencies may each hold a
// workflow of one name.

// twoNightlies seeds two workflows built in the app of one name, each with its
// own steps, and returns their rowids. The older row is w-fin, which is the
// one a lookup by name returns.
func twoNightlies(f *homeFixture) (fin, tax int64) {
	f.t.Helper()
	f.exec(`INSERT INTO workflows (uid, name, source, steps, enabled, created_at) VALUES
	        ('w-fin', 'nightly', 'cronomicon', '[{"type":"job","name":"fin-job"}]', 1, 't')`)
	f.exec(`INSERT INTO workflows (uid, name, source, steps, enabled, created_at) VALUES
	        ('w-tax', 'nightly', 'cronomicon', '[{"type":"job","name":"tax-job"}]', 1, 't')`)
	for uid, into := range map[string]*int64{"w-fin": &fin, "w-tax": &tax} {
		if err := f.e.db.QueryRow(`SELECT rowid FROM workflows WHERE uid = ?`, uid).Scan(into); err != nil {
			f.t.Fatal(err)
		}
	}
	return fin, tax
}

func TestLoadForFireFindsTheWorkflowByItsUid(t *testing.T) {
	f := newHomeFixture(t)
	ctx := context.Background()
	fin, tax := twoNightlies(f)

	for uid, want := range map[string]struct {
		row  int64
		step string
	}{"w-fin": {fin, "fin-job"}, "w-tax": {tax, "tax-job"}} {
		row, steps, err := LoadForFire(ctx, f.e.db, uid, "cronomicon", "nightly")
		if err != nil {
			t.Fatalf("LoadForFire(%s): %v", uid, err)
		}
		if row != want.row || len(steps) != 1 || steps[0].Name != want.step {
			t.Errorf("LoadForFire(%s) = row %d, steps %+v; want row %d and the step %s", uid, row, steps, want.row, want.step)
		}
	}

	// A uid that names no workflow is not replaced by the name's.
	if _, _, err := LoadForFire(ctx, f.e.db, "w-gone", "cronomicon", "nightly"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("LoadForFire of a uid that no workflow has: %v, want no row", err)
	}
	// With no uid, a name two workflows hold starts neither.
	if _, _, err := LoadForFire(ctx, f.e.db, "", "cronomicon", "nightly"); !errors.Is(err, ErrAmbiguousWorkflow) {
		t.Errorf("LoadForFire with no uid, of a name two workflows hold: %v, want ErrAmbiguousWorkflow", err)
	}
	// And a name one workflow holds is that workflow, as it always was.
	solo := f.workflowRow("w-solo", "weekly", "global", `[{"type":"job","name":"solo-job"}]`)
	if row, steps, err := LoadForFire(ctx, f.e.db, "", "git", "weekly"); err != nil || row != solo || len(steps) != 1 {
		t.Errorf("LoadForFire with no uid, of a name one workflow holds = row %d, %d steps, %v; want row %d", row, len(steps), err, solo)
	}
}

// A run is stamped with the workflow that is being run. It decides whose
// reactions the run fires and whose history it is in.
func TestAWorkflowRunIsStampedWithTheWorkflowItIsOf(t *testing.T) {
	f := newHomeFixture(t)
	ctx := context.Background()
	_, tax := twoNightlies(f)

	res, err := f.e.Trigger(ctx, TriggerParams{
		WorkflowName: "nightly", WorkflowSource: "cronomicon", WorkflowID: tax, TriggeredBy: "scheduler",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	var uid sql.NullString
	if err := f.e.db.QueryRow(`SELECT workflow_uid FROM workflow_runs WHERE id = ?`, res.TraceID).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if uid.String != "w-tax" {
		t.Fatalf("the run of w-tax is stamped %q, want w-tax", uid.String)
	}

	// A caller that names no row: the name's workflow when one holds it, and
	// nobody when two do.
	res, err = f.e.Trigger(ctx, TriggerParams{WorkflowName: "nightly", WorkflowSource: "cronomicon", TriggeredBy: "someone"})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if err := f.e.db.QueryRow(`SELECT workflow_uid FROM workflow_runs WHERE id = ?`, res.TraceID).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if uid.Valid {
		t.Errorf("a run started by a name two workflows hold is stamped %q, want nobody", uid.String)
	}
	f.workflowRow("w-solo", "weekly", "global", `[]`)
	res, err = f.e.Trigger(ctx, TriggerParams{WorkflowName: "weekly", WorkflowSource: "git", TriggeredBy: "someone"})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if err := f.e.db.QueryRow(`SELECT workflow_uid FROM workflow_runs WHERE id = ?`, res.TraceID).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if uid.String != "w-solo" {
		t.Errorf("a run started by a name one workflow holds is stamped %q, want w-solo", uid.String)
	}

	// A sub-workflow that was refused before it was resolved to a row.
	f.e.recordRefusedChild(ctx, "", "node-1", "nightly", "someone", "cronomicon", 1, "Refused: test")
	if err := f.e.db.QueryRow(`SELECT workflow_uid FROM workflow_runs WHERE id = 'node-1'`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if uid.Valid {
		t.Errorf("a refused child of a name two workflows hold is stamped %q, want nobody", uid.String)
	}
}

// The two callbacks the scheduler is given, end to end: a fire that carries a
// workflow's uid starts THAT workflow, with its steps, and the run is its.
func TestTheFirersStartTheWorkflowTheyAreHanded(t *testing.T) {
	f := newHomeFixture(t)
	ctx := context.Background()
	twoNightlies(f)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ScheduledFirer(f.e.db, log)(ctx, scheduler.WorkflowFire{
		UID: "w-tax", Source: "cronomicon", Name: "nightly", ScheduleName: "tax-at-three"})
	PendingFirer(f.e.db, log)(ctx, scheduler.PendingWorkflowFire{
		UID: "w-fin", Source: "cronomicon", Name: "nightly", TriggeredBy: "someone", TriggerKind: "manual"})

	for who, want := range map[string]struct{ uid, step string }{
		"scheduler": {"w-tax", "tax-job"},
		"someone":   {"w-fin", "fin-job"},
	} {
		var uid, snapshot sql.NullString
		if err := f.e.db.QueryRow(
			`SELECT workflow_uid, steps_snapshot FROM workflow_runs WHERE triggered_by = ?`, who).Scan(&uid, &snapshot); err != nil {
			t.Fatalf("the run started for %s: %v", who, err)
		}
		if uid.String != want.uid || !strings.Contains(snapshot.String, want.step) {
			t.Errorf("the run started for %s is of %q with the steps %s; want %s and its step %s",
				who, uid.String, snapshot.String, want.uid, want.step)
		}
	}

	// A fire that names no workflow, of a name two hold, starts nothing.
	var before, after int
	_ = f.e.db.QueryRow(`SELECT COUNT(*) FROM workflow_runs`).Scan(&before)
	ScheduledFirer(f.e.db, log)(ctx, scheduler.WorkflowFire{Source: "cronomicon", Name: "nightly", ScheduleName: "whose"})
	PendingFirer(f.e.db, log)(ctx, scheduler.PendingWorkflowFire{Source: "cronomicon", Name: "nightly", TriggeredBy: "whoever"})
	_ = f.e.db.QueryRow(`SELECT COUNT(*) FROM workflow_runs`).Scan(&after)
	if after != before {
		t.Errorf("%d runs were started by fires that name no workflow, of a name two workflows hold", after-before)
	}
}

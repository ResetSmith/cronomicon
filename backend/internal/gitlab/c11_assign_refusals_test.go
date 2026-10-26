package gitlab

import (
	"errors"
	"strings"
	"testing"
)

// 2.5.0 C11, defect 89: an assignment that commits a file the target's sync
// then refuses is refused itself, as question 94 does for a missing calendar.
// Until C11 it was committed: the target's sync refused the file, set
// jobsOK=false, and every later sync of the target left its jobs prune off, so
// a job the agency removed from Git kept running.

const reactingJob = "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: nightly\nspec:\n  run_type: bash\n  command: echo nightly\n" +
	"  reactions:\n    - {name: after-extract, onKind: job, onName: extract, onOutcome: success}\n"
const extractJob = "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: extract\nspec:\n  run_type: bash\n  command: echo extract\n"

func TestC11_AReactionToAnUpstreamTheTargetLacksIsRefused(t *testing.T) {
	f := newAssignFixture(t, map[string]string{"jobs/nightly.yaml": reactingJob, "jobs/extract.yaml": extractJob}, nil)
	before := f.head()

	_, err := f.assign()
	var refusal *AssignError
	if !errors.As(err, &refusal) || refusal.Code != "upstream_missing" || strings.Join(refusal.Upstreams, ",") != "job:extract" {
		t.Fatalf("Finance's repository has no job extract: want upstream_missing naming job:extract, got %v", err)
	}
	if f.head() != before {
		t.Fatal("a refused assignment pushed")
	}

	// Finance's repository holds a job extract of its own: the copy's reaction is
	// to that one, and the assignment goes through, and Finance's sync takes it.
	// (A job of an agency's repository names a scope of that agency's.)
	f.finChanges(map[string]string{"jobs/extract.yaml": strings.Replace(extractJob, "spec:\n", "spec:\n  scope: "+f.finScope+"\n", 1)})
	if _, err := f.assign(); err != nil {
		t.Fatalf("assign with Finance's own extract: %v", err)
	}
	if r := grSync(t, f.fin, "Finance's repository, with the copy"); r.Status != "success" || len(r.Errors) != 0 {
		t.Fatalf("Finance's sync of the copy: %s %v", r.Status, r.Errors)
	}
}

// It keeps to holidays, which the origin agency has (the fixture's calendar, not global).
const keepsToSchedule = "apiVersion: cronomicon.io/v1\nkind: Schedule\nmetadata:\n  name: nightly-cron\nspec:\n  cron: '0 2 * * *'\n  onlyCalendars:\n    - holidays\n"

func TestC11_KeepingToACalendarTheTargetHasAsGlobalIsRefused(t *testing.T) {
	f := newAssignFixture(t, map[string]string{"schedules/nightly-cron.yaml": keepsToSchedule}, nil)
	// Finance's holidays freezes its whole agency.
	if _, err := f.origin.db.Exec(`INSERT INTO calendars (source, name, global, created_by, created_at, owner_agency) VALUES ('cronomicon', 'holidays', 1, 't', 't', 'ag-fin')`); err != nil {
		t.Fatal(err)
	}
	before := f.head()

	_, err := f.assign()
	var refusal *AssignError
	if !errors.As(err, &refusal) || refusal.Code != "calendar_not_bindable" || strings.Join(refusal.Calendars, ",") != "holidays" {
		t.Fatalf("Finance's holidays is global: want calendar_not_bindable naming it, got %v", err)
	}
	if f.head() != before {
		t.Fatal("a refused assignment pushed")
	}

	if _, err := f.origin.db.Exec(`UPDATE calendars SET global = 0 WHERE name = 'holidays' AND owner_agency = 'ag-fin'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.assign(); err != nil {
		t.Fatalf("assign once Finance's calendar is not global: %v", err)
	}
}

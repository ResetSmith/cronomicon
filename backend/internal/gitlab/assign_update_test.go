package gitlab

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// "An update is available", and assigning again as the update (2.5.0 C10.3;
// questions 91 and 93).

const assignScriptV2 = "#!/bin/sh\necho nightly, version two\n"

// originChanges commits files to the origin's repository and syncs it.
func (f *assignFixture) originChanges(files map[string]string) {
	f.t.Helper()
	grCommitFiles(f.t, f.originRepo, f.originWork, files, "the origin changes its template")
	if r := grSync(f.t, f.origin, "the origin's repository, changed"); r.Status != "success" {
		f.t.Fatalf("the origin's sync: %s %s", r.Status, r.ErrorMessage)
	}
}

// finChanges commits to Finance's repository (writing files, removing paths)
// and syncs it.
func (f *assignFixture) finChanges(files map[string]string, remove ...string) {
	f.t.Helper()
	work := f.t.TempDir()
	sysGit(f.t, work, "clone", "-q", "--branch", f.branch, f.finBare, ".")
	for rel, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, rel)), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, rel), []byte(content), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	for _, rel := range remove {
		sysGit(f.t, work, "rm", "-q", rel)
	}
	sysGit(f.t, work, "add", "-A")
	sysGit(f.t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "Finance changes its copy")
	sysGit(f.t, work, "push", "-q", "origin", f.branch)
	grSync(f.t, f.fin, "Finance's repository, changed")
}

// updateNotice is the assignment notice for the fixture's job: its detail,
// whether it is open, and whether it exists.
func (f *assignFixture) updateNotice() (detail string, open, found bool) {
	f.t.Helper()
	if err := f.reg.CheckAssignmentUpdates(context.Background()); err != nil {
		f.t.Fatalf("CheckAssignmentUpdates: %v", err)
	}
	var agency string
	var resolved sql.NullString
	err := f.origin.db.QueryRow(`SELECT agency_id, detail, resolved_at FROM notices WHERE kind = ? AND subject = ?`,
		notices.KindAssignmentUpdate, "ag-fin:"+f.jobUID).Scan(&agency, &detail, &resolved)
	if err == sql.ErrNoRows {
		return "", false, false
	}
	if err != nil {
		f.t.Fatal(err)
	}
	if agency != "ag-fin" {
		f.t.Errorf("the notice is filed under %q, want the target agency", agency)
	}
	return detail, !resolved.Valid, true
}

func actions(res *AssignResult) map[string]string {
	out := map[string]string{}
	for _, file := range res.Files {
		out[file.Path] = file.Action
	}
	return out
}

// A change in the origin's copy raises the notice, under Finance, naming the file
// and saying Finance has not changed its copy. Assigning again replaces only
// that file, in one commit, and the record is the new one; the notice clears.
func TestAnOriginChangeRaisesTheNoticeAndAnUpdateClearsIt(t *testing.T) {
	f := newAssignFixture(t, nil, nil)
	f.finHasCalendar()
	if _, err := f.assign(); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, _, found := f.updateNotice(); found {
		t.Fatal("a notice right after the assignment")
	}

	f.originChanges(map[string]string{"scripts/nightly.sh": assignScriptV2})
	detail, open, _ := f.updateNotice()
	if !open || !strings.Contains(detail, "scripts/nightly.sh") || !strings.Contains(detail, "has not changed its copy") {
		t.Fatalf("notice: open %v, %q", open, detail)
	}
	if _, since, _ := strings.Cut(detail, "changed there since:"); strings.Contains(since, "jobs/nightly.yaml") {
		t.Errorf("the notice names a file that did not change: %q", detail)
	}

	res, err := f.assign()
	if err != nil {
		t.Fatalf("assign again: %v", err)
	}
	a := actions(res)
	if a["scripts/nightly.sh"] != "replaced" || a["jobs/nightly.yaml"] != "unchanged" || a["schedules/nightly-cron.yaml"] != "unchanged" {
		t.Errorf("actions %v", a)
	}
	if changed := sysGit(t, f.finBare, "diff-tree", "--no-commit-id", "--name-only", "-r", res.CommitSHA); changed != "scripts/nightly.sh" {
		t.Errorf("the update commit carries %q, want the script alone", changed)
	}
	if got := sysGit(t, f.finBare, "show", res.CommitSHA+":scripts/nightly.sh"); got+"\n" != assignScriptV2 {
		t.Errorf("the script is %q", got)
	}
	if n := grString(t, f.origin.db, `SELECT COUNT(*) FROM assignments`); n != "1" {
		t.Errorf("assignment records: %s, want the new one alone", n)
	}
	if n := grString(t, f.origin.db, `SELECT COUNT(*) FROM assignment_files WHERE assignment_id = ?`, res.AssignmentID); n != "3" {
		t.Errorf("the new record's files: %s, want 3", n)
	}
	if _, open, _ := f.updateNotice(); open {
		t.Error("the notice is still open after the update")
	}
}

// A file Finance has changed in its copy is reported, and an update does not
// overwrite it (question 91): refused, naming it, nothing pushed.
func TestAnAgencysChangeIsReportedAndNotOverwritten(t *testing.T) {
	f := newAssignFixture(t, nil, nil)
	f.finHasCalendar()
	if _, err := f.assign(); err != nil {
		t.Fatalf("assign: %v", err)
	}
	// Finance changes the script, which the origin changes too, and the schedule,
	// which the origin leaves alone: only the first stands in an update's way.
	schedule := sysGit(t, f.finBare, "show", f.branch+":schedules/nightly-cron.yaml") + "\n# Finance's note\n"
	f.finChanges(map[string]string{"scripts/nightly.sh": "#!/bin/sh\necho finance's own fix\n", "schedules/nightly-cron.yaml": schedule})
	f.originChanges(map[string]string{"scripts/nightly.sh": assignScriptV2})

	detail, open, _ := f.updateNotice()
	if !open || !strings.Contains(detail, "changed its own copy of scripts/nightly.sh:") {
		t.Fatalf("notice: open %v, %q", open, detail)
	}
	if strings.Contains(detail, "schedules/nightly-cron.yaml") {
		t.Errorf("the notice names Finance's change to a file the origin did not change: %q", detail)
	}
	before := f.head()
	_, err := f.assign()
	if refusalCode(err) != "changed_by_agency" {
		t.Fatalf("want changed_by_agency, got %v", err)
	}
	var e *AssignError
	if ae, ok := err.(*AssignError); ok {
		e = ae
	}
	if e == nil || len(e.Paths) != 1 || e.Paths[0] != "scripts/nightly.sh" {
		t.Errorf("paths %+v", e)
	}
	if f.head() != before {
		t.Error("a refused update pushed")
	}
}

// Finance's change to a file the origin did not change is Finance's: an update
// replaces what the origin changed and leaves that file as Finance has it.
func TestAnUpdateLeavesAnAgencysChangeToAFileTheOriginLeft(t *testing.T) {
	f := newAssignFixture(t, nil, nil)
	f.finHasCalendar()
	if _, err := f.assign(); err != nil {
		t.Fatalf("assign: %v", err)
	}
	schedule := sysGit(t, f.finBare, "show", f.branch+":schedules/nightly-cron.yaml") + "\n# Finance's note\n"
	f.finChanges(map[string]string{"schedules/nightly-cron.yaml": schedule})
	f.originChanges(map[string]string{"scripts/nightly.sh": assignScriptV2})
	res, err := f.assign()
	if err != nil {
		t.Fatalf("assign again: %v", err)
	}
	if a := actions(res); a["scripts/nightly.sh"] != "replaced" || a["schedules/nightly-cron.yaml"] != "unchanged" {
		t.Errorf("actions %v", a)
	}
	if got := sysGit(t, f.finBare, "show", res.CommitSHA+":schedules/nightly-cron.yaml"); !strings.Contains(got, "Finance's note") {
		t.Errorf("Finance's change was overwritten:\n%s", got)
	}
}

// The origin removing the job's file leaves nothing to update from: the copy is
// Finance's own, the record goes, and nothing is raised.
func TestTheOriginRemovingTheDefinitionEndsTheAssignment(t *testing.T) {
	f := newAssignFixture(t, nil, nil)
	f.finHasCalendar()
	if _, err := f.assign(); err != nil {
		t.Fatalf("assign: %v", err)
	}
	gitRemoveFile(t, f.originRepo, "jobs/nightly.yaml", "the origin withdraws the template")
	grSync(t, f.origin, "the origin's repository, without the job")
	if _, open, _ := f.updateNotice(); open {
		t.Error("a notice offering an update from a definition the origin no longer has")
	}
	if n := grString(t, f.origin.db, `SELECT COUNT(*) FROM assignments`); n != "0" {
		t.Errorf("assignment records: %s, want none", n)
	}
}

// A file the origin removes and adds again is a new row, with a new uid: the
// assignment is still found by its file, and assigning again is an update.
func TestAFileAddedAgainInTheOriginIsStillTheSameAssignment(t *testing.T) {
	f := newAssignFixture(t, nil, nil)
	f.finHasCalendar()
	if _, err := f.assign(); err != nil {
		t.Fatalf("assign: %v", err)
	}
	gitRemoveFile(t, f.originRepo, "jobs/nightly.yaml", "removed")
	grBackdateAll(t, f.origin) // so the sync's prune sees the row as stale
	grSync(t, f.origin, "the origin's repository, without the job")
	if n := grString(t, f.origin.db, `SELECT COUNT(*) FROM jobs WHERE name = 'nightly' AND repo_id = 'repo-a'`); n != "0" {
		t.Fatalf("the removed job was not pruned (count %s)", n)
	}
	f.originChanges(map[string]string{"jobs/nightly.yaml": strings.Replace(assignJob, "the nightly backup", "the nightly backup, again", 1)})
	newUID := grString(t, f.origin.db, `SELECT uid FROM jobs WHERE name = 'nightly' AND repo_id = 'repo-a'`)
	if newUID == f.jobUID {
		t.Fatal("the job kept its uid: this test is of one that does not")
	}
	f.jobUID = newUID
	res, err := f.assign()
	if err != nil {
		t.Fatalf("assign again: %v", err)
	}
	if a := actions(res); a["jobs/nightly.yaml"] != "replaced" || !res.Updated {
		t.Errorf("actions %v, updated %v", a, res.Updated)
	}
}

// Question 95: assigning again with another scope, the origin unchanged, moves
// the job's copy to it (its file rewritten; the script and the schedule left).
// A copy the agency has changed is refused, as for any update.
func TestReassigningWithAnotherScopeMovesTheCopy(t *testing.T) {
	f := newAssignFixture(t, nil, nil)
	f.finHasCalendar()
	if _, err := f.assign(); err != nil {
		t.Fatalf("assign: %v", err)
	}
	grAgencyScope(t, f.origin.db, "ag-fin", "fin-other")
	if _, err := f.origin.db.Exec(`UPDATE scopes SET owner_agency = 'ag-fin' WHERE id = 'id-fin-other'`); err != nil {
		t.Fatal(err)
	}
	f.finScope = "fin-other"
	res, err := f.assign()
	if err != nil {
		t.Fatalf("assign on another scope: %v", err)
	}
	if a := actions(res); a["jobs/nightly.yaml"] != "replaced" || a["scripts/nightly.sh"] != "unchanged" || a["schedules/nightly-cron.yaml"] != "unchanged" {
		t.Errorf("actions %v", a)
	}
	if got := sysGit(t, f.finBare, "show", res.CommitSHA+":jobs/nightly.yaml"); !strings.Contains(got, "scope: fin-other") {
		t.Errorf("the copy was not moved:\n%s", got)
	}

	// Finance changes the job, and asks for the first scope back: refused.
	job := sysGit(t, f.finBare, "show", f.branch+":jobs/nightly.yaml") + "# Finance's note\n"
	f.finChanges(map[string]string{"jobs/nightly.yaml": job})
	f.finScope = "hosts-of-repo-fin"
	if _, err := f.assign(); refusalCode(err) != "changed_by_agency" {
		t.Fatalf("want changed_by_agency, got %v", err)
	}
}

// Assigning again with nothing changed in the origin commits nothing.
func TestAnUpdateWithNothingChangedIsUpToDate(t *testing.T) {
	f := newAssignFixture(t, nil, nil)
	f.finHasCalendar()
	if _, err := f.assign(); err != nil {
		t.Fatalf("assign: %v", err)
	}
	before := f.head()
	if _, err := f.assign(); refusalCode(err) != "up_to_date" {
		t.Fatalf("want up_to_date, got %v", err)
	}
	if f.head() != before {
		t.Error("an update with nothing to update pushed")
	}
}

// Finance removing its copy of the job ends the assignment: its record goes,
// and an origin change raises nothing.
func TestAnAgencyRemovingItsCopyDropsTheRecord(t *testing.T) {
	f := newAssignFixture(t, nil, nil)
	f.finHasCalendar()
	if _, err := f.assign(); err != nil {
		t.Fatalf("assign: %v", err)
	}
	f.finChanges(nil, "jobs/nightly.yaml")
	f.originChanges(map[string]string{"scripts/nightly.sh": assignScriptV2})
	if _, open, _ := f.updateNotice(); open {
		t.Error("a notice for a copy the agency removed")
	}
	if n := grString(t, f.origin.db, `SELECT COUNT(*) FROM assignments`); n != "0" {
		t.Errorf("assignment records: %s, want none", n)
	}
}

// Until Finance's sync has the assignment's commit, the copy looks absent
// from Finance's last sync: it is not judged, and the record stays.
func TestAnAssignmentNotYetSyncedIsNotJudged(t *testing.T) {
	f := newAssignFixture(t, nil, nil)
	f.finHasCalendar()
	before := grString(t, f.origin.db, `SELECT last_sha FROM git_repos WHERE id = 'repo-fin'`)
	if _, err := f.assign(); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := f.origin.db.Exec(`UPDATE git_repos SET last_sha = ? WHERE id = 'repo-fin'`, before); err != nil {
		t.Fatal(err)
	}
	f.updateNotice()
	if n := grString(t, f.origin.db, `SELECT COUNT(*) FROM assignments`); n != "1" {
		t.Errorf("assignment records: %s, want the one, not judged", n)
	}
}

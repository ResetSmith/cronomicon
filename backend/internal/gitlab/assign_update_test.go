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

// globalChanges commits files to Global's repository and syncs it.
func (f *assignFixture) globalChanges(files map[string]string) {
	f.t.Helper()
	grCommitFiles(f.t, f.globalRepo, f.globalWork, files, "Global changes its template")
	if r := grSync(f.t, f.global, "Global's repository, changed"); r.Status != "success" {
		f.t.Fatalf("Global's sync: %s %s", r.Status, r.ErrorMessage)
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
	err := f.global.db.QueryRow(`SELECT agency_id, detail, resolved_at FROM notices WHERE kind = ? AND subject = ?`,
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

// A change in Global's copy raises the notice, under Finance, naming the file
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

	f.globalChanges(map[string]string{"scripts/nightly.sh": assignScriptV2})
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
	if n := grString(t, f.global.db, `SELECT COUNT(*) FROM assignments`); n != "1" {
		t.Errorf("assignment records: %s, want the new one alone", n)
	}
	if n := grString(t, f.global.db, `SELECT COUNT(*) FROM assignment_files WHERE assignment_id = ?`, res.AssignmentID); n != "3" {
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
	f.finChanges(map[string]string{"scripts/nightly.sh": "#!/bin/sh\necho finance's own fix\n"})
	f.globalChanges(map[string]string{"scripts/nightly.sh": assignScriptV2})

	detail, open, _ := f.updateNotice()
	if !open || !strings.Contains(detail, "changed its own copy of scripts/nightly.sh") {
		t.Fatalf("notice: open %v, %q", open, detail)
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
	f.globalChanges(map[string]string{"scripts/nightly.sh": assignScriptV2})
	if _, open, _ := f.updateNotice(); open {
		t.Error("a notice for a copy the agency removed")
	}
	if n := grString(t, f.global.db, `SELECT COUNT(*) FROM assignments`); n != "0" {
		t.Errorf("assignment records: %s, want none", n)
	}
}

// Until Finance's sync has the assignment's commit, the copy looks absent
// from Finance's last sync: it is not judged, and the record stays.
func TestAnAssignmentNotYetSyncedIsNotJudged(t *testing.T) {
	f := newAssignFixture(t, nil, nil)
	f.finHasCalendar()
	before := grString(t, f.global.db, `SELECT last_sha FROM git_repos WHERE id = 'repo-fin'`)
	if _, err := f.assign(); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := f.global.db.Exec(`UPDATE git_repos SET last_sha = ? WHERE id = 'repo-fin'`, before); err != nil {
		t.Fatal(err)
	}
	f.updateNotice()
	if n := grString(t, f.global.db, `SELECT COUNT(*) FROM assignments`); n != "1" {
		t.Errorf("assignment records: %s, want the one, not judged", n)
	}
}

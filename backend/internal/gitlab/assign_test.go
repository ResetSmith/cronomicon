package gitlab

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"gopkg.in/yaml.v3"
)

// Assignment by commit (2.5.0 C10.1, IN-12; questions 87 to 93).

const (
	assignScript   = "#!/bin/sh\necho nightly\n"
	assignSchedule = "apiVersion: cronomicon.io/v1\nkind: Schedule\nmetadata:\n  name: nightly-cron\nspec:\n  cron: '0 2 * * *'\n  skipCalendars:\n    - holidays\n"
	assignJob      = "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: nightly\nspec:\n  # the nightly backup\n  run_type: bash\n  script_ref: nightly.sh\n  scheduleRefs:\n    - nightly-cron\n  become_password_secret: DB_PASS\n"
)

// assignFixture is Global's repository holding the job nightly (its script and
// its schedule), synced; Finance (ag-fin) with a repository whose remote is
// bare, so it takes a push, synced, and a scope Finance owns alone; and a
// Registry holding both Services.
type assignFixture struct {
	t        *testing.T
	reg      *Registry
	global   *Service
	fin      *Service
	finBare  string
	branch   string
	jobUID   string
	finScope string
}

func newAssignFixture(t *testing.T, finFiles map[string]string) *assignFixture {
	t.Helper()
	a, repoA, remoteA := newSyncFixture(t)
	// Global's calendar, which its schedule skips (a sync refuses a schedule
	// naming a calendar its agency does not have).
	if _, err := a.db.Exec(`INSERT INTO calendars (source, name, created_by, created_at, owner_agency) VALUES ('cronomicon', 'holidays', 't', 't', 'global')`); err != nil {
		t.Fatal(err)
	}
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"scripts/nightly.sh":          assignScript,
		"schedules/nightly-cron.yaml": assignSchedule,
		"jobs/nightly.yaml":           assignJob,
	}, "the template")
	if r := grSync(t, a, "Global's repository"); r.Status != "success" {
		t.Fatalf("Global's sync: %s %s", r.Status, r.ErrorMessage)
	}

	fin, repoB, remoteB := grRepo(t, a, "repo-fin", "ag-fin")
	files := map[string]string{"README": "Finance\n"}
	for k, v := range finFiles {
		files[k] = v
	}
	grCommitFiles(t, repoB, remoteB, files, "Finance's own")
	bare := filepath.Join(t.TempDir(), "fin.git")
	sysGit(t, remoteB, "clone", "-q", "--bare", remoteB, bare)
	fin.repoURL = bare
	if _, err := a.db.Exec(`UPDATE git_repos SET url = ? WHERE id = 'repo-fin'`, bare); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE scopes SET owner_agency = 'ag-fin' WHERE id = 'id-hosts-of-repo-fin'`); err != nil {
		t.Fatal(err)
	}
	if r := grSync(t, fin, "Finance's repository"); r.Status != "success" {
		t.Fatalf("Finance's sync: %s %s", r.Status, r.ErrorMessage)
	}

	reg := NewRegistry(a.db, slog.New(slog.NewTextHandler(io.Discard, nil)), &config.Config{})
	reg.services["global"], reg.services["repo-fin"] = a, fin
	f := &assignFixture{t: t, reg: reg, global: a, fin: fin, finBare: bare, branch: a.Cfg.GitLabWriteBranch, finScope: "hosts-of-repo-fin"}
	f.jobUID = grString(t, a.db, `SELECT uid FROM jobs WHERE name = 'nightly' AND repo_id = 'global'`)
	if f.jobUID == "" {
		t.Fatal("Global's job nightly was not synced")
	}
	return f
}

func (f *assignFixture) head() string {
	return sysGit(f.t, f.finBare, "rev-parse", "refs/heads/"+f.branch)
}

func (f *assignFixture) assign() (*AssignResult, error) {
	res, err := f.reg.AssignJob(context.Background(), AssignJobRequest{
		JobUID: f.jobUID, TargetAgency: "ag-fin", Scope: f.finScope, Actor: "admin@example.com",
	})
	f.fin.wg.Wait() // the sync it started
	return res, err
}

func refusalCode(err error) string {
	var e *AssignError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// One commit carrying the whole set: the job (naming Finance's scope, stamped
// with its origin, its comment kept), its script and its schedule. The answer
// lists the secret it names and the calendar its schedule skips, neither of
// which Finance has. The record has one row per file. Finance's sync brings
// the job in as Finance's.
func TestAnAssignmentIsOneCommitCarryingTheWholeSet(t *testing.T) {
	f := newAssignFixture(t, nil)
	before := f.head()
	globalSHA := grString(t, f.global.db, `SELECT last_sha FROM git_repos WHERE id = 'global'`)

	res, err := f.assign()
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	after := f.head()
	if res.CommitSHA != after || sysGit(t, f.finBare, "rev-parse", after+"^") != before {
		t.Fatalf("not one commit on Finance's branch: %s (head %s, was %s)", res.CommitSHA, after, before)
	}
	changed := strings.Fields(sysGit(t, f.finBare, "diff-tree", "--no-commit-id", "--name-only", "-r", after))
	want := []string{"jobs/nightly.yaml", "schedules/nightly-cron.yaml", "scripts/nightly.sh"}
	if strings.Join(changed, " ") != strings.Join(want, " ") {
		t.Errorf("the commit carries %v, want %v", changed, want)
	}

	job := sysGit(t, f.finBare, "show", after+":jobs/nightly.yaml")
	var doc struct {
		Metadata struct {
			Name   string            `yaml:"name"`
			Origin map[string]string `yaml:"origin"`
		} `yaml:"metadata"`
		Spec struct {
			Scope string `yaml:"scope"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(job), &doc); err != nil {
		t.Fatalf("the committed job: %v\n%s", err, job)
	}
	if doc.Spec.Scope != f.finScope {
		t.Errorf("the copy names scope %q, want Finance's %q", doc.Spec.Scope, f.finScope)
	}
	if o := doc.Metadata.Origin; o["agency"] != "global" || o["path"] != "jobs/nightly.yaml" || o["commit"] != globalSHA {
		t.Errorf("metadata.origin = %v", o)
	}
	if !strings.Contains(job, "# the nightly backup") {
		t.Errorf("the job's comment was not kept:\n%s", job)
	}
	if got := sysGit(t, f.finBare, "show", after+":scripts/nightly.sh"); got+"\n" != assignScript {
		t.Errorf("the script was not copied as it is: %q", got)
	}

	lacks := map[string]bool{}
	for _, l := range res.Lacks {
		lacks[l.Kind+":"+l.Name] = true
	}
	if !lacks["secret:DB_PASS"] || !lacks["calendar:holidays"] || len(res.Lacks) != 2 {
		t.Errorf("lacks = %+v, want the secret DB_PASS and the calendar holidays", res.Lacks)
	}
	if !res.SyncStarted {
		t.Error("no sync of Finance's repository was started")
	}
	// Calendars are not files before 2.6.0 (question 88): until Finance has
	// the calendar, its sync refuses the schedule and the job with it. Once it
	// has one, the next sync brings the job in as Finance's.
	finJob := `SELECT COUNT(*) FROM jobs WHERE name = 'nightly' AND owner_agency = 'ag-fin' AND repo_id = 'repo-fin'`
	if n := grString(t, f.global.db, finJob); n != "0" {
		t.Errorf("the job was synced into Finance while Finance lacks its calendar (count %s)", n)
	}
	for _, l := range res.Lacks {
		if l.Kind == "calendar" && !strings.Contains(l.Why, "refuses") {
			t.Errorf("the calendar's lack does not say what it does: %+v", l)
		}
	}
	if _, err := f.global.db.Exec(`INSERT INTO calendars (source, name, created_by, created_at, owner_agency) VALUES ('cronomicon', 'holidays', 't', 't', 'ag-fin')`); err != nil {
		t.Fatal(err)
	}
	if r := grSync(t, f.fin, "Finance's repository, with the calendar"); r.Status != "success" {
		t.Fatalf("Finance's sync with the calendar: %s %s", r.Status, r.ErrorMessage)
	}
	if n := grString(t, f.global.db, finJob); n != "1" {
		t.Errorf("Finance's sync did not bring the job in as Finance's (count %s)", n)
	}

	if n := grString(t, f.global.db, `SELECT COUNT(*) FROM assignments WHERE id = ? AND origin_commit = ? AND commit_sha = ? AND target_agency = 'ag-fin'`,
		res.AssignmentID, globalSHA, after); n != "1" {
		t.Errorf("assignment rows: %s, want 1", n)
	}
	if n := grString(t, f.global.db, `SELECT COUNT(*) FROM assignment_files WHERE assignment_id = ? AND length(origin_hash) = 64 AND length(committed_hash) = 64`,
		res.AssignmentID); n != "3" {
		t.Errorf("file rows with both hashes: %s, want 3", n)
	}
	if same := grString(t, f.global.db, `SELECT COUNT(*) FROM assignment_files WHERE assignment_id = ? AND origin_hash = committed_hash`,
		res.AssignmentID); same != "1" {
		t.Errorf("files committed as they were in the origin: %s, want 1 (the script; the job and the schedule are stamped)", same)
	}
}

// A job name Finance already has refuses the assignment, naming the file, and
// nothing is pushed.
func TestATakenNameRefusesWithNothingPushed(t *testing.T) {
	f := newAssignFixture(t, map[string]string{"jobs/mine.yaml": jobYAML("nightly")})
	before := f.head()
	_, err := f.assign()
	if refusalCode(err) != "name_taken" {
		t.Fatalf("want name_taken, got %v", err)
	}
	if f.head() != before {
		t.Error("a refused assignment pushed")
	}
	if n := grString(t, f.global.db, `SELECT COUNT(*) FROM assignments`); n != "0" {
		t.Errorf("a refused assignment was recorded")
	}
}

// A scope that is Finance's but in several agencies is not one Finance's sync
// would accept for its job: refused before anything is committed.
func TestAScopeNotTheTargetsAloneRefusesWithNothingPushed(t *testing.T) {
	f := newAssignFixture(t, nil)
	grAgencyScope(t, f.global.db, "ag-tax", "tax-hosts")
	if _, err := f.global.db.Exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('id-hosts-of-repo-fin', 'ag-tax')`); err != nil {
		t.Fatal(err)
	}
	before := f.head()
	_, err := f.assign()
	if refusalCode(err) != "unknown_scope" {
		t.Fatalf("want unknown_scope, got %v", err)
	}
	if f.head() != before {
		t.Error("a refused assignment pushed")
	}
}

// A script Finance has by name is Finance's own: it is used, not added, and
// the answer says that it differs from Global's. It has no row in the record.
func TestADependencyTheTargetHasIsUsedAndReported(t *testing.T) {
	f := newAssignFixture(t, map[string]string{"scripts/nightly.sh": "#!/bin/sh\necho finances-own\n"})
	res, err := f.assign()
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	var script *AssignedFile
	for i := range res.Files {
		if res.Files[i].Role == "script" {
			script = &res.Files[i]
		}
	}
	if script == nil || script.Action != "usesOwn" || !script.Differs {
		t.Fatalf("the script: %+v, want usesOwn and differs", script)
	}
	if got := sysGit(t, f.finBare, "show", res.CommitSHA+":scripts/nightly.sh"); !strings.Contains(got, "finances-own") {
		t.Errorf("Finance's script was replaced: %q", got)
	}
	if n := grString(t, f.global.db, `SELECT COUNT(*) FROM assignment_files WHERE assignment_id = ? AND role = 'script'`, res.AssignmentID); n != "0" {
		t.Errorf("a file not written has a record row")
	}
}

// A job built in the app is not a file of a repository (question 87).
func TestAJobBuiltInTheAppIsRefused(t *testing.T) {
	f := newAssignFixture(t, nil)
	if _, err := f.global.db.Exec(`UPDATE jobs SET source = 'cronomicon' WHERE uid = ?`, f.jobUID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.assign(); refusalCode(err) != "not_in_git" {
		t.Fatalf("want not_in_git, got %v", err)
	}
}

package gitlab

import (
	"context"
	"strings"
	"testing"
)

// Assigning a workflow (2.5.0 C10.2): its jobs, their scripts and schedules,
// and its sub-workflows, followed through every kind of step.

const (
	wfRelease = "apiVersion: cronomicon.io/v1\nkind: Workflow\nmetadata:\n  name: release\nspec:\n  steps:\n" +
		"    - {type: job, name: build}\n" +
		"    - type: parallel\n      jobs:\n        - {type: job, name: nightly}\n        - {type: workflow, name: tell, workflow: notify}\n"
	wfNotify = "apiVersion: cronomicon.io/v1\nkind: Workflow\nmetadata:\n  name: notify\nspec:\n  steps:\n" +
		"    - type: branch\n      condition: {type: job_status, step: x, status: success}\n      pass:\n        steps:\n          - {type: job, name: ping}\n"
)

func workflowTemplate() map[string]string {
	return map[string]string{
		"workflows/release.yaml": wfRelease,
		"workflows/notify.yaml":  wfNotify,
		"jobs/build.yaml":        grJob("build", "echo build"),
		"jobs/ping.yaml":         grJob("ping", "echo ping"),
	}
}

func (f *assignFixture) assignWorkflow(name string, scope string, scopes map[string]string) (*AssignResult, error) {
	f.t.Helper()
	uid := grString(f.t, f.global.db, `SELECT uid FROM workflows WHERE name = ? AND repo_id = 'global'`, name)
	if uid == "" {
		f.t.Fatalf("Global's workflow %s was not synced", name)
	}
	res, err := f.reg.Assign(context.Background(), AssignRequest{
		Kind: "workflow", UID: uid, TargetAgency: "ag-fin", Scope: scope, Scopes: scopes, Actor: "admin@example.com",
	})
	f.fin.wg.Wait()
	return res, err
}

func (f *assignFixture) finHasCalendar() {
	f.t.Helper()
	if _, err := f.global.db.Exec(`INSERT INTO calendars (source, name, created_by, created_at, owner_agency) VALUES ('cronomicon', 'holidays', 't', 't', 'ag-fin')`); err != nil {
		f.t.Fatal(err)
	}
}

// One commit carries the workflow, the jobs its steps run (in a parallel, and
// in a branch of its sub-workflow), each job's script and schedule, and the
// sub-workflow. Every job's copy names Finance's scope. Finance's sync brings
// them all in as Finance's.
func TestAWorkflowsAssignmentCarriesWhatItRuns(t *testing.T) {
	f := newAssignFixture(t, workflowTemplate(), nil)
	f.finHasCalendar()
	res, err := f.assignWorkflow("release", f.finScope, nil)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	changed := strings.Fields(sysGit(t, f.finBare, "diff-tree", "--no-commit-id", "--name-only", "-r", res.CommitSHA))
	want := []string{"jobs/build.yaml", "jobs/nightly.yaml", "jobs/ping.yaml", "schedules/nightly-cron.yaml",
		"scripts/nightly.sh", "workflows/notify.yaml", "workflows/release.yaml"}
	if strings.Join(changed, " ") != strings.Join(want, " ") {
		t.Errorf("the commit carries %v, want %v", changed, want)
	}
	roles := map[string]string{}
	for _, file := range res.Files {
		roles[file.Path] = file.Role + "/" + file.Action
	}
	for p, r := range map[string]string{"workflows/release.yaml": "definition/added", "workflows/notify.yaml": "workflow/added",
		"jobs/build.yaml": "job/added", "jobs/ping.yaml": "job/added", "scripts/nightly.sh": "script/added"} {
		if roles[p] != r {
			t.Errorf("%s: %q, want %q", p, roles[p], r)
		}
	}
	for _, job := range []string{"build", "nightly", "ping"} {
		got := sysGit(t, f.finBare, "show", res.CommitSHA+":jobs/"+job+".yaml")
		if !strings.Contains(got, "scope: "+f.finScope) || !strings.Contains(got, "origin:") {
			t.Errorf("jobs/%s.yaml is not Finance's stamped copy:\n%s", job, got)
		}
	}
	if n := grString(t, f.global.db, `SELECT COUNT(*) FROM assignment_files WHERE assignment_id = ?`, res.AssignmentID); n != "7" {
		t.Errorf("record rows: %s, want 7", n)
	}
	if k := grString(t, f.global.db, `SELECT kind FROM assignments WHERE id = ?`, res.AssignmentID); k != "workflow" {
		t.Errorf("recorded kind %q", k)
	}
	for q, want := range map[string]string{
		`SELECT COUNT(*) FROM workflows WHERE owner_agency = 'ag-fin' AND repo_id = 'repo-fin' AND name IN ('release', 'notify')`:   "2",
		`SELECT COUNT(*) FROM jobs WHERE owner_agency = 'ag-fin' AND repo_id = 'repo-fin' AND name IN ('build', 'nightly', 'ping')`: "3",
	} {
		if n := grString(t, f.global.db, q); n != want {
			t.Errorf("Finance's sync: %s → %s, want %s", q, n, want)
		}
	}
}

// A job Finance's repository has by name is Finance's own: the workflow's step
// runs it, it is not added, and the answer says it differs.
func TestAWorkflowUsesAJobTheTargetsRepositoryHas(t *testing.T) {
	f := newAssignFixture(t, workflowTemplate(), map[string]string{"jobs/ping.yaml": grJob("ping", "echo finances-ping")})
	f.finHasCalendar()
	res, err := f.assignWorkflow("release", f.finScope, nil)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	var ping *AssignedFile
	for i := range res.Files {
		if res.Files[i].Path == "jobs/ping.yaml" {
			ping = &res.Files[i]
		}
	}
	if ping == nil || ping.Role != "job" || ping.Action != "usesOwn" || !ping.Differs {
		t.Fatalf("ping: %+v, want Finance's own, differing", ping)
	}
	if got := sysGit(t, f.finBare, "show", res.CommitSHA+":jobs/ping.yaml"); !strings.Contains(got, "finances-ping") {
		t.Errorf("Finance's job was replaced:\n%s", got)
	}
}

// A job Finance has only in the app would not be run by the copy's Git step,
// and its name is Finance's: refused, nothing pushed.
func TestAWorkflowNeedingANameTheTargetHasInTheAppIsRefused(t *testing.T) {
	f := newAssignFixture(t, workflowTemplate(), nil)
	if _, err := f.global.db.Exec(`INSERT INTO jobs (uid, name, source, run_type, owner_agency, created_by, created_at)
	                               VALUES ('fin-build', 'build', 'cronomicon', 'bash', 'ag-fin', 't', 't')`); err != nil {
		t.Fatal(err)
	}
	before := f.head()
	_, err := f.assignWorkflow("release", f.finScope, nil)
	if refusalCode(err) != "name_taken" {
		t.Fatalf("want name_taken, got %v", err)
	}
	if f.head() != before {
		t.Error("a refused assignment pushed")
	}
}

// A step pinned to a job built in the app is not a file to copy (question 87).
func TestAWorkflowRunningAJobBuiltInTheAppIsRefused(t *testing.T) {
	files := workflowTemplate()
	files["workflows/mixed.yaml"] = "apiVersion: cronomicon.io/v1\nkind: Workflow\nmetadata:\n  name: mixed\nspec:\n  steps:\n" +
		"    - {type: job, name: build}\n    - {type: job, name: by-hand, jobSource: cronomicon}\n"
	f := newAssignFixture(t, files, nil)
	if _, err := f.assignWorkflow("mixed", f.finScope, nil); refusalCode(err) != "not_in_git" {
		t.Fatalf("want not_in_git, got %v", err)
	}
}

// Each copied job's scope is picked by the scope it runs on in the origin
// ("" for none); with no pick and no default, the job is named.
func TestAWorkflowsJobsTakeTheirScopeByTheOriginsScope(t *testing.T) {
	f := newAssignFixture(t, workflowTemplate(), nil)
	f.finHasCalendar()
	if _, err := f.assignWorkflow("release", "", nil); refusalCode(err) != "scope_required" {
		t.Fatalf("no scope at all: want scope_required, got %v", err)
	}
	res, err := f.assignWorkflow("release", "", map[string]string{"": f.finScope})
	if err != nil {
		t.Fatalf("assign with Global's no-scope jobs mapped: %v", err)
	}
	if got := sysGit(t, f.finBare, "show", res.CommitSHA+":jobs/build.yaml"); !strings.Contains(got, "scope: "+f.finScope) {
		t.Errorf("build's copy:\n%s", got)
	}
}

// A sub-workflow and a job named by two steps are planned once: one file each
// in the commit and one record row each. It is the same early return that ends
// a cycle.
func TestAWorkflowNamingADependencyTwicePlansItOnce(t *testing.T) {
	files := workflowTemplate()
	files["workflows/twice.yaml"] = "apiVersion: cronomicon.io/v1\nkind: Workflow\nmetadata:\n  name: twice\nspec:\n  steps:\n" +
		"    - {type: workflow, name: first, workflow: notify}\n    - {type: job, name: ping}\n" +
		"    - type: sequence\n      steps:\n        - {type: workflow, name: again, workflow: notify}\n        - {type: job, name: ping}\n"
	f := newAssignFixture(t, files, nil)
	res, err := f.assignWorkflow("twice", f.finScope, nil)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	changed := strings.Fields(sysGit(t, f.finBare, "diff-tree", "--no-commit-id", "--name-only", "-r", res.CommitSHA))
	if want := []string{"jobs/ping.yaml", "workflows/notify.yaml", "workflows/twice.yaml"}; strings.Join(changed, " ") != strings.Join(want, " ") {
		t.Errorf("the commit carries %v, want %v", changed, want)
	}
	if len(res.Files) != 3 {
		t.Errorf("files answered: %+v, want three", res.Files)
	}
	if n := grString(t, f.global.db, `SELECT COUNT(*) FROM assignment_files WHERE assignment_id = ?`, res.AssignmentID); n != "3" {
		t.Errorf("record rows: %s, want 3", n)
	}
}

func TestWalkStepRefsVisitsEveryContainer(t *testing.T) {
	steps := []any{
		map[string]any{"type": "job", "name": "a"},
		map[string]any{"type": "sequence", "name": "seq", "steps": []any{map[string]any{"type": "job", "name": "b", "jobSource": "git"}}},
		map[string]any{"type": "parallel", "jobs": []any{map[string]any{"name": "c"}, map[string]any{"type": "workflow", "name": "w", "workflow": "sub"}}},
		map[string]any{"type": "branch", "pass": map[string]any{"steps": []any{map[string]any{"type": "job", "name": "d"}}},
			"fail": map[string]any{"steps": []any{map[string]any{"type": "workflow", "workflow": "sub2"}}}},
	}
	var jobs, wfs []string
	walkStepRefs(steps, func(s stepRef) { jobs = append(jobs, s.name+"/"+s.source) }, func(w string) { wfs = append(wfs, w) })
	if strings.Join(jobs, " ") != "a/ b/git c/ d/" || strings.Join(wfs, " ") != "sub sub2" {
		t.Errorf("jobs %v, workflows %v", jobs, wfs)
	}
}

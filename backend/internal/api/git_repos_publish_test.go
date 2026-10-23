package api_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/gitlab"
)

// A publish names a repository (2.4.0 Phase R6; GR-24).
//
// A repository other than Global's takes publishes from those who hold
// publishSchedule on ITS agency: jobs in scopes they hold it on, and the
// agency's own schedule and workflow files. Another agency's repository, and
// one that is not there, are refused in one sentence. Global's repository,
// which is what a publish that names none means, is a global publisher's
// alone (R6's question 19, decided by the owner on 2026-10-13; GC-9 had let an
// agency's approver publish there a job in their own scope).
//
// These are tests of the GATE. Nothing here has a remote to push to: a publish
// that passes the gate goes on to fail at its fetch, and "not 403" is the
// gate's answer.
func TestAPublishNamesARepositoryAndIsGatedByItsAgency(t *testing.T) {
	h, pool := repoGateServer(t)
	exec := mustExec(t, pool)
	connect := func(who, agency string) string {
		t.Helper()
		rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/"+agency+"/git-repo", who, `{"repoUrl":"http://127.0.0.1:1/`+agency+`.git"}`)
		var made struct{ ID string }
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &made) != nil || made.ID == "" {
			t.Fatalf("connect %s: %d %s", agency, rec.Code, rec.Body)
		}
		return made.ID
	}
	fin, tax := connect(gFinAdmin, "ag:FIN"), connect(gRoot, "ag:TAX")
	publish := func(who, repo, path, content string) (int, string) {
		t.Helper()
		body := map[string]any{"filePath": path, "content": content, "commitMessage": "m", "new": true}
		if repo != "" {
			body["repoId"] = repo
		}
		b, _ := json.Marshal(body)
		rec := gateReqWithHeader(t, h, http.MethodPost, "/api/v1/schedules/publish", who, string(b), "If-Match", "deadbeef")
		return rec.Code, rec.Body.String()
	}
	const schedule = "apiVersion: cronomicon.io/v1\nkind: Schedule\nmetadata:\n  name: nightly\nspec:\n  cron: '0 2 * * *'\n"
	const workflow = "apiVersion: cronomicon.io/v1\nkind: Workflow\nmetadata:\n  name: flow\nspec:\n  steps: []\n"
	passes := func(what, who, repo, path, content string) {
		t.Helper()
		if code, body := publish(who, repo, path, content); code == http.StatusForbidden || code == http.StatusNotFound {
			t.Errorf("%s = %d %s, want the gate to pass", what, code, body)
		}
	}
	refused := func(what, who, repo, path, content string) string {
		t.Helper()
		code, body := publish(who, repo, path, content)
		if code != http.StatusForbidden {
			t.Errorf("%s = %d %s, want 403", what, code, body)
		}
		return body
	}

	// The agency's own repository.
	passes("FIN's approver publishing FIN's job to FIN's repository", gFinApprover, fin, "jobs/fin-new.yaml", jobYAML("fin-new", "fin-hosts"))
	passes("FIN's approver publishing a schedule file to FIN's repository", gFinApprover, fin, "schedules/nightly.yaml", schedule)
	passes("FIN's approver publishing a workflow file to FIN's repository", gFinApprover, fin, "workflows/flow.yaml", workflow)
	passes("FIN's administrator, the same", gFinAdmin, fin, "jobs/fin-new.yaml", jobYAML("fin-new", "fin-hosts"))
	refused("FIN's approver publishing a job in TAX's scope to FIN's repository", gFinApprover, fin, "jobs/x.yaml", jobYAML("x", "tax-hosts"))
	// The scope is the repository's agency's (2.5.0 C11, defect 73): a grant on
	// TAX covered TAX's scope by its name, and the gate let it into FIN's
	// repository (the sync then refused the file).
	refused("FIN's approver who administers TAX publishing a job in TAX's scope to FIN's repository", gFinApprover+","+gTaxAdmin, fin, "jobs/x.yaml", jobYAML("x", "tax-hosts"))
	passes("the same person publishing FIN's job to FIN's repository", gFinApprover+","+gTaxAdmin, fin, "jobs/fin-new.yaml", jobYAML("fin-new", "fin-hosts"))
	refused("FIN's approver publishing a job with no scope to FIN's repository", gFinApprover, fin, "jobs/x.yaml", jobYAML("x", ""))
	refused("FIN's approver publishing content whose scope cannot be read to FIN's repository", gFinApprover, fin, "jobs/x.yaml", "::: not yaml :::")
	refused("FIN's approver publishing a job with no name to FIN's repository", gFinApprover, fin, "jobs/x.yaml",
		"apiVersion: cronomicon.io/v1\nkind: Job\nspec:\n  run_type: bash\n  command: echo hi\n  scope: fin-hosts\n")
	refused("FIN's operator, who may not publish at all", gFinOperator, fin, "jobs/fin-new.yaml", jobYAML("fin-new", "fin-hosts"))

	// Another agency's repository, and one that is not there: one answer.
	other := refused("FIN's approver publishing to TAX's repository", gFinApprover, tax, "jobs/fin-new.yaml", jobYAML("fin-new", "fin-hosts"))
	gone := refused("FIN's approver publishing to a repository that is not there", gFinApprover, "repo-nope", "jobs/fin-new.yaml", jobYAML("fin-new", "fin-hosts"))
	if other != gone {
		t.Errorf("the two refusals differ, which says whether the repository is there:\n%s\n%s", other, gone)
	}
	refused("FIN's approver publishing a schedule file to TAX's repository", gFinApprover, tax, "schedules/nightly.yaml", schedule)

	// Global's repository, by naming none and by its name: a global
	// publisher's alone, whatever the file and whoever of an agency asks.
	for _, repo := range []string{"", "global"} {
		said := refused("FIN's approver publishing FIN's job to Global's repository", gFinApprover, repo, "jobs/fin-new.yaml", jobYAML("fin-new", "fin-hosts"))
		if !strings.Contains(said, "Global's repository") || !strings.Contains(said, "your own agency's repository") {
			t.Errorf("the refusal does not say where to publish instead: %s", said)
		}
		refused("FIN's administrator publishing FIN's job to Global's repository", gFinAdmin, repo, "jobs/fin-new.yaml", jobYAML("fin-new", "fin-hosts"))
		refused("FIN's approver publishing a schedule file to Global's repository", gFinApprover, repo, "schedules/nightly.yaml", schedule)
		refused("FIN's approver publishing TAX's job to Global's repository", gFinApprover, repo, "jobs/x.yaml", jobYAML("x", "tax-hosts"))
		passes("a global publisher publishing a schedule file to Global's repository", gRoot, repo, "schedules/nightly.yaml", schedule)
		passes("a global publisher publishing a job to Global's repository", gRoot, repo, "jobs/fin-new.yaml", jobYAML("fin-new", "fin-hosts"))
	}

	// A global publisher: any repository, and is told when one is not there.
	passes("a global publisher publishing to TAX's repository", gRoot, tax, "schedules/nightly.yaml", schedule)
	if code, body := publish(gRoot, "repo-nope", "jobs/x.yaml", jobYAML("x", "fin-hosts")); code != http.StatusNotFound {
		t.Errorf("a global publisher publishing to a repository that is not there = %d %s, want 404", code, body)
	}

	// The name a publish would take is asked of THAT repository. A job of the
	// name in another agency's repository is nothing to it (and must not be
	// said); one in the same repository, outside the caller's access, is.
	exec(`INSERT INTO jobs (uid, name, source, repo_id, run_type, scope, source_path, synced_at) VALUES
	        ('j-tax-shared', 'shared-name', 'git', ?, 'bash', 'tax-hosts', 'jobs/shared.yaml', 't'),
	        ('j-fin-taken',  'fin-taken',   'git', ?, 'bash', NULL,        'jobs/taken.yaml',  't'),
	        ('j-glob-taken', 'glob-taken',  'git', 'global', 'bash', 'tax-hosts', 'jobs/glob.yaml', 't')`, tax, fin)
	exec(`INSERT INTO jobs (uid, name, source, repo_id, run_type, scope, source_path, synced_at) VALUES
	        ('j-fin-taxs', 'fin-taxs', 'git', ?, 'bash', 'tax-hosts', 'jobs/taxs.yaml', 't')`, fin)
	passes("a name another agency's repository has, published to FIN's", gFinApprover, fin, "jobs/mine.yaml", jobYAML("shared-name", "fin-hosts"))
	taken := refused("a name FIN's repository has with no scope, published to FIN's", gFinApprover, fin, "jobs/mine.yaml", jobYAML("fin-taken", "fin-hosts"))
	refused("a name FIN's repository has in a scope outside the caller's access, published to FIN's", gFinApprover, fin, "jobs/mine.yaml", jobYAML("fin-taxs", "fin-hosts"))
	passes("a name Global's repository has, published to FIN's, which does not have it", gFinApprover, fin, "jobs/mine.yaml", jobYAML("glob-taken", "fin-hosts"))

	// The FILE a publish would replace is asked too: one that holds a job
	// outside the caller's access is not theirs to replace, and the refusal is
	// the sentence for a taken name (which of the two it was is not said).
	syncsSettled(t, pool)
	held := filepath.Join(gitlab.CloneDirFor(fin), "jobs", "tax-existing.yaml")
	if err := os.MkdirAll(filepath.Dir(held), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(held, []byte(jobYAML("tax-existing", "tax-hosts")), 0o644); err != nil {
		t.Fatal(err)
	}
	if said := refused("FIN's approver replacing a file of FIN's repository that holds a job in TAX's scope", gFinApprover, fin, "jobs/tax-existing.yaml", jobYAML("tax-existing", "fin-hosts")); said != taken {
		t.Errorf("the refusal for a file that is taken differs from the one for a name that is:\n%s\n%s", said, taken)
	}

	// The list of past publishes says which repository each was to, and that
	// repository's branch.
	syncsSettled(t, pool)
	exec(`UPDATE git_repos SET branch = 'release' WHERE id = ?`, fin)
	exec(`DELETE FROM schedule_pushes`)
	exec(`INSERT INTO schedule_pushes (repo_id, at, actor, schedule_file, status, created_at) VALUES
	        ('global', '2026-01-01T00:00:00Z', 'a@example.com', 'jobs/g.yaml', 'success', 't'),
	        (?,        '2026-01-02T00:00:00Z', 'b@example.com', 'jobs/f.yaml', 'success', 't')`, fin)
	list := gateReq(t, h, http.MethodGet, "/api/v1/schedule-pushes", gRoot, "").Body.String()
	if !strings.Contains(list, `"repoId":"global"`) || !strings.Contains(list, `"repoId":"`+fin+`"`) || !strings.Contains(list, `"branch":"release"`) {
		t.Errorf("the list of past publishes does not carry each one's repository and its branch: %s", list)
	}
	only := gateReq(t, h, http.MethodGet, "/api/v1/schedule-pushes?repo="+fin, gRoot, "").Body.String()
	if strings.Contains(only, "jobs/g.yaml") || !strings.Contains(only, "jobs/f.yaml") {
		t.Errorf("the list of one repository's publishes is not that repository's alone: %s", only)
	}
}

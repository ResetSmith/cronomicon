package notices_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// A workflow built in the app whose jobs are several agencies' is reported to
// Global, naming the agencies, and the notice resolves when its jobs are all
// one agency's. What is not reported: a workflow of one agency's jobs, one
// that runs only Global's, a Git workflow, and one in the recycle bin.
func TestWorkflowSpansAgenciesIsAWorkflowOfSeveralAgenciesJobs(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	for _, s := range [][3]string{{"sc-fin", "fin-hosts", "ag-fin"}, {"sc-tax", "tax-hosts", "ag-tax"}} {
		mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at) VALUES (?, ?, 'cronomicon', 't')`, s[0], s[1])
		mustExec(t, pool, `INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, s[0], s[2])
	}
	job := func(uid, name string, scope any) {
		mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, synced_at) VALUES (?, ?, 'cronomicon', 'bash', ?, 't')`, uid, name, scope)
	}
	job("j-fin", "fin-job", "fin-hosts")
	job("j-tax", "tax-job", "tax-hosts")
	job("j-free", "free-job", nil)
	wf := func(uid, source, steps string, deleted any) {
		mustExec(t, pool, `INSERT INTO workflows (uid, name, source, steps, synced_at, deleted_at) VALUES (?, ?, ?, ?, 't', ?)`,
			uid, "wf-"+uid, source, steps, deleted)
	}
	const fin, tax, free = `{"type":"job","name":"fin-job"}`, `{"type":"job","name":"tax-job"}`, `{"type":"job","name":"free-job"}`
	wf("spans", "cronomicon", "["+fin+","+tax+"]", nil)                                                   // the condition
	wf("with-global", "cronomicon", "["+fin+","+free+"]", nil)                                            // an agency's and Global's: two
	wf("child", "cronomicon", "["+tax+"]", nil)                                                           // one agency's
	wf("via-child", "cronomicon", `[`+fin+`,{"type":"workflow","name":"go","workflow":"wf-child"}]`, nil) // spans through a sub-workflow
	wf("one", "cronomicon", "["+fin+"]", nil)
	wf("only-global", "cronomicon", "["+free+"]", nil)
	wf("empty", "cronomicon", "[]", nil)
	wf("from-git", "git", "["+fin+","+tax+"]", nil) // a Git workflow is its repository's
	wf("binned", "cronomicon", "["+fin+","+tax+"]", "2026-01-01T00:00:00Z")

	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindWorkflowSpansAgencies)
	}
	got := check()
	if len(got) != 3 {
		t.Fatalf("open notices = %d (%v), want the three workflows that span", len(got), got)
	}
	n, ok := got["spans"]
	if !ok || n.AgencyID != "global" {
		t.Fatalf("the spanning workflow's notice: found %v, agency %q; want it under Global", ok, n.AgencyID)
	}
	for _, want := range []string{"The workflow wf-spans", "Finance", "Tax", "global administrator"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the detail does not say %q: %s", want, n.Detail)
		}
	}
	if d := got["with-global"].Detail; !strings.Contains(d, "Finance") || !strings.Contains(d, "Global") {
		t.Errorf("the workflow of an agency's job and Global's: %q", d)
	}
	if _, ok := got["via-child"]; !ok {
		t.Errorf("no notice for the workflow that spans through a sub-workflow: %v", got)
	}

	// Its jobs become one agency's: the notice clears. A scope given to another
	// agency makes a workflow span that did not.
	mustExec(t, pool, `UPDATE workflows SET steps = ? WHERE uid = 'spans'`, "["+fin+"]")
	mustExec(t, pool, `UPDATE workflows SET steps = ? WHERE uid = 'one'`, "["+fin+","+tax+"]")
	got = check()
	if _, still := got["spans"]; still {
		t.Errorf("the notice did not clear when the workflow's jobs became one agency's")
	}
	if _, now := got["one"]; !now || len(got) != 3 {
		t.Errorf("open notices after the changes = %v, want with-global, via-child and one", got)
	}
}

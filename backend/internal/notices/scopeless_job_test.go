package notices_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// 2.5.3 CA7: every job names a scope. An agency's job built in the app with no
// scope, made before, is listed under that agency with the way out; Central's
// are central_holds_work's, a Git job and a binned one are not listed, and the
// notice clears when the agency has none left.
func TestAnAgencysJobsWithNoScopeAreToldToThatAgency(t *testing.T) {
	pool := open(t)
	ctx := context.Background()
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	job := func(name, source string, scope, owner any, deleted bool) {
		t.Helper()
		var del any
		if deleted {
			del = "t"
		}
		mustExec(t, pool, `INSERT INTO jobs (name, source, run_type, scope, synced_at, owner_agency, deleted_at) VALUES (?, ?, 'bash', ?, 't', ?, ?)`,
			name, source, scope, owner, del)
	}
	job("fin-none", "cronomicon", nil, "ag-fin", false)
	job("fin-blank", "cronomicon", "", "ag-fin", false)
	job("fin-scoped", "cronomicon", "fin-hosts", "ag-fin", false)
	job("fin-binned", "cronomicon", nil, "ag-fin", true)
	job("fin-git", "git", nil, "ag-fin", false)
	job("tax-scoped", "cronomicon", "tax-hosts", "ag-tax", false)
	job("central-none", "cronomicon", nil, "global", false)

	if err := notices.RunChecks(ctx, pool); err != nil {
		t.Fatal(err)
	}
	got := openOf(t, pool, notices.KindJobWithoutScope)
	if len(got) != 1 {
		t.Fatalf("notices: %v; want one, Finance's", got)
	}
	n, ok := got["ag-fin"]
	if !ok || n.AgencyID != "ag-fin" {
		t.Fatalf("the notice is %+v; want it Finance's, subject ag-fin", got)
	}
	for _, want := range []string{"2 of Finance's jobs", "fin-blank, fin-none", "global administrator", "ansible_connection=local", "lists no hosts", "Run dialog"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the detail does not say %q: %s", want, n.Detail)
		}
	}
	for _, not := range []string{"fin-scoped", "fin-binned", "fin-git", "central-none"} {
		if strings.Contains(n.Detail, not) {
			t.Errorf("the detail names %s: %s", not, n.Detail)
		}
	}

	// Each given a scope: the notice clears.
	mustExec(t, pool, `UPDATE jobs SET scope = 'fin-hosts' WHERE name IN ('fin-none', 'fin-blank')`)
	if err := notices.RunChecks(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if got := openOf(t, pool, notices.KindJobWithoutScope); len(got) != 0 {
		t.Errorf("after each was given a scope: %v; want none", got)
	}
}

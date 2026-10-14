package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// GR-7 holds after a save (2.4.0 Phase R6; the owner's decision on R4's
// question 9, built when question 21 was answered).
//
// A workflow whose jobs are several agencies' is Global's, and saving one is a
// global administrator's. R4's review reached the same graph two other ways,
// as somebody who holds compose in two agencies and is not one: by saving a
// CHILD workflow that a workflow already saved runs, and by saving a JOB it
// runs into another agency's scope. The parent came to span, a notice said
// so, and nothing was refused. Each is refused now, and nothing is written.
func TestASaveMayNotMakeAnotherWorkflowSpanAgencies(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's, tax-hosts is TAX's
	exec := mustExec(t, pool)
	exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	const both = gFinAdmin + "," + gTaxAdmin
	job := func(name, scope string) string {
		return `{"name":"` + name + `","scriptRef":"backup-db","scope":"` + scope + `"}`
	}
	must := func(what, method, path, who, body string) {
		t.Helper()
		if rec := gateReq(t, h, method, path, who, body); rec.Code/100 != 2 {
			t.Fatalf("%s: %s %s = %d (%s)", what, method, path, rec.Code, rec.Body)
		}
	}
	for name, scope := range map[string]string{"fin-a": "fin-hosts", "fin-b": "fin-hosts", "fin-c": "fin-hosts", "fin-d": "fin-hosts", "tax-a": "tax-hosts"} {
		must("create "+name, http.MethodPost, "/api/v1/jobs", gRoot, job(name, scope))
	}
	const stepA, stepB, stepD, stepTax = `{"type":"job","name":"fin-a"}`, `{"type":"job","name":"fin-b"}`, `{"type":"job","name":"fin-d"}`, `{"type":"job","name":"tax-a"}`
	must("FIN's child", http.MethodPost, "/api/v1/workflows", gFinAdmin, `{"name":"child","steps":[`+stepD+`]}`)
	must("FIN's parent, which runs the child", http.MethodPost, "/api/v1/workflows", gFinAdmin,
		`{"name":"parent","steps":[`+stepA+`,{"type":"workflow","name":"go","workflow":"child"}]}`)
	must("FIN's workflow of two of its jobs", http.MethodPost, "/api/v1/workflows", gFinAdmin, `{"name":"direct","steps":[`+stepA+`,`+stepB+`]}`)
	id := func(table, name string) string {
		return rowID(t, pool, `SELECT rowid FROM `+table+` WHERE name = ? AND source = 'cronomicon'`, name)
	}
	state := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(`
			SELECT (SELECT group_concat(name || '=' || COALESCE(scope,''), ',') FROM (SELECT name, scope FROM jobs ORDER BY name))
			    || ' | ' || (SELECT group_concat(name || '=' || steps, ',') FROM (SELECT name, steps FROM workflows ORDER BY name))
			    || ' | ' || (SELECT COUNT(*) FROM definition_revisions)`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	denied := 0
	refused := func(what, method, path, who, body string) string {
		t.Helper()
		before := state()
		rec := gateReq(t, h, method, path, who, body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403 (%s)", what, rec.Code, rec.Body)
		}
		if after := state(); after != before {
			t.Errorf("%s was refused and still wrote:\n before %s\n after  %s", what, before, after)
		}
		// The refusal is on record (it is written after the save is undone;
		// written while the save's transaction is open, it waits and is lost).
		if n := count(t, pool, `SELECT COUNT(*) FROM auth_events WHERE kind = 'denied'`); n != denied+1 {
			t.Errorf("%s left %d refusals on record, want one more than the %d before", what, n, denied)
		}
		denied++
		return rec.Body.String()
	}

	// ── The child. Its own jobs become TAX's alone, which is not a workflow
	// that spans; the PARENT, which runs FIN's job beside it, would.
	said := refused("an administrator of both agencies saving the child onto TAX's job", http.MethodPut, "/api/v1/workflows/"+id("workflows", "child"), both, `{"name":"child","steps":[`+stepTax+`]}`)
	if !strings.Contains(said, "the workflow parent") || !strings.Contains(said, "more than one agency") {
		t.Errorf("the refusal does not name the workflow it would make span: %s", said)
	}
	// A save of the child that leaves the parent one agency's is theirs.
	must("the child saved onto another of FIN's jobs", http.MethodPut, "/api/v1/workflows/"+id("workflows", "child"), both, `{"name":"child","steps":[`+stepB+`]}`)

	// ── The job. `direct` runs fin-a and fin-b; fin-b going to TAX's scope
	// makes it span (and the parent, through the child).
	said = refused("an administrator of both agencies saving a job into TAX's scope", http.MethodPut, "/api/v1/jobs/"+id("jobs", "fin-b"), both, job("fin-b", "tax-hosts"))
	if !strings.Contains(said, "direct") || !strings.Contains(said, "parent") {
		t.Errorf("the refusal does not name the two workflows it would make span: %s", said)
	}
	// A job that no workflow runs goes where its two administrators send it.
	must("a job no workflow runs, saved into TAX's scope", http.MethodPut, "/api/v1/jobs/"+id("jobs", "fin-c"), both, job("fin-c", "tax-hosts"))

	// ── The words name a workflow only to somebody who holds compose on the
	// agency it belongs to. (Nobody else gets here; this makes it so by hand.)
	exec(`UPDATE workflows SET owner_agency = 'global' WHERE name = 'direct'`)
	said = refused("the same save, with one of the workflows not theirs to read", http.MethodPut, "/api/v1/jobs/"+id("jobs", "fin-b"), both, job("fin-b", "tax-hosts"))
	if strings.Contains(said, "direct") || strings.Contains(said, "parent") || strings.Contains(said, "workflow") {
		t.Errorf("the refusal says which workflow, or that there is one, to somebody who may not read every one of them: %s", said)
	}
	exec(`UPDATE workflows SET owner_agency = 'ag:FIN' WHERE name = 'direct'`)

	// ── A workflow that spans ALREADY is not laid at the door of a later save.
	must("a global administrator's workflow of both agencies' jobs", http.MethodPost, "/api/v1/workflows", gRoot, `{"name":"spans","steps":[`+stepA+`,`+stepTax+`]}`)
	must("FIN's administrator saving a job the spanning workflow runs, as it is", http.MethodPut, "/api/v1/jobs/"+id("jobs", "fin-a"), gFinAdmin, job("fin-a", "fin-hosts"))

	// ── A global administrator makes either change, and the notice says so.
	must("a global administrator saving the job into TAX's scope", http.MethodPut, "/api/v1/jobs/"+id("jobs", "fin-b"), gRoot, job("fin-b", "tax-hosts"))
	if n := count(t, pool, `SELECT COUNT(*) FROM jobs WHERE name = 'fin-b' AND scope = 'tax-hosts'`); n != 1 {
		t.Errorf("the global administrator's save did not take")
	}
}

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
//
// Since §0a (IN-8) nobody saves such a workflow, a global administrator
// included, and the refusal is a 409 for everyone.
func TestASaveMayNotMakeAnotherWorkflowSpanAgencies(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's, tax-hosts is TAX's
	exec := mustExec(t, pool)
	exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	agenciesGetTheirOwnRepository(t, pool)
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
	// §0a (IN-8): the refusal is a rule, not a permission, and is the same for
	// everyone: 409 workflow_spans_agencies. Until then it was a 403, recorded
	// as a denial, and a global administrator's save was not asked.
	refused := func(what, method, path, who, body string) string {
		t.Helper()
		before := state()
		denied := count(t, pool, `SELECT COUNT(*) FROM auth_events WHERE kind = 'denied'`)
		rec := gateReq(t, h, method, path, who, body)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"crosses_agencies"`) {
			t.Errorf("%s = %d, want 409 crosses_agencies (%s)", what, rec.Code, rec.Body)
		}
		if after := state(); after != before {
			t.Errorf("%s was refused and still wrote:\n before %s\n after  %s", what, before, after)
		}
		if n := count(t, pool, `SELECT COUNT(*) FROM auth_events WHERE kind = 'denied'`); n != denied {
			t.Errorf("%s was recorded as a permission denial", what)
		}
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

	// ── The job. `direct` runs fin-a and fin-b. Since I2 a job cannot be saved
	// into another agency's scope at all: it is its owner's (1400), and finds
	// its scope in its own agency, so TAX's scope is a name FIN has no scope
	// of (422 scope_not_in_agency, for everyone, nothing written). Until I2 an
	// administrator of both agencies could, and the span it made was what was
	// refused here (409 crosses_agencies, naming the workflows).
	notTheirs := func(what, who, jobName string) {
		t.Helper()
		before := state()
		rec := gateReq(t, h, http.MethodPut, "/api/v1/jobs/"+id("jobs", jobName), who, job(jobName, "tax-hosts"))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"scope_not_in_agency"`) {
			t.Errorf("%s = %d, want 422 scope_not_in_agency (%s)", what, rec.Code, rec.Body)
		}
		if after := state(); after != before {
			t.Errorf("%s was refused and still wrote:\n before %s\n after  %s", what, before, after)
		}
	}
	notTheirs("an administrator of both agencies saving a job into TAX's scope", both, "fin-b")
	notTheirs("a job no workflow runs, saved into TAX's scope", both, "fin-c")

	// ── A workflow that spans ALREADY (as an upgrade leaves one: nobody can
	// save one since §0a) is not laid at the door of a later save.
	refused("a global administrator's workflow of both agencies' jobs", http.MethodPost, "/api/v1/workflows", gRoot, `{"name":"spans","steps":[`+stepA+`,`+stepTax+`]}`)
	exec(`INSERT INTO workflows (uid, name, source, steps, enabled, created_at, owner_agency) VALUES ('wf-spans', 'spans', 'cronomicon', ?, 1, 't', 'global')`,
		`[`+stepA+`,`+stepTax+`]`)
	must("FIN's administrator saving a job the spanning workflow runs, as it is", http.MethodPut, "/api/v1/jobs/"+id("jobs", "fin-a"), gFinAdmin, job("fin-a", "fin-hosts"))

	// ── A global administrator is refused either change too (§0a; until then
	// theirs to make).
	notTheirs("a global administrator saving the job into TAX's scope", gRoot, "fin-b")
}

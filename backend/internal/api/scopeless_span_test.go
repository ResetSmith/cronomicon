package api_test

import (
	"net/http"
	"testing"
)

// 2.5.3 CA6: a job with no scope is its own agency's (CA-5), so a workflow of
// one agency that runs one does not span agencies. Until CA6 the span reading
// (notices.SpanningWorkflows, which refuseNewSpan asks) took a scopeless job
// for Global's: making a job a workflow runs scopeless, or saving a workflow
// that runs one, was refused with 409 crosses_agencies.
func TestAScopelessJobOfTheAgencyDoesNotMakeItsWorkflowSpan(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's
	exec := mustExec(t, pool)
	exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	agenciesGetTheirOwnRepository(t, pool)
	job := func(name, scope string) string {
		return `{"name":"` + name + `","scriptRef":"backup-db","scope":"` + scope + `"}`
	}
	must := func(what string, code int, body string) {
		t.Helper()
		if code/100 != 2 {
			t.Fatalf("%s = %d (%s)", what, code, body)
		}
	}
	for _, name := range []string{"fin-a", "fin-b"} {
		rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs", gRoot, job(name, "fin-hosts"))
		must("create "+name, rec.Code, rec.Body.String())
	}
	const stepA, stepB = `{"type":"job","name":"fin-a"}`, `{"type":"job","name":"fin-b"}`
	rec := gateReq(t, h, http.MethodPost, "/api/v1/workflows", gFinAdmin, `{"name":"direct","steps":[`+stepA+`,`+stepB+`]}`)
	must("FIN's workflow of two of its jobs", rec.Code, rec.Body.String())

	// A job that FIN's workflow runs, made scopeless (a grant that reaches every
	// agency authors one), working in FIN: it stays FIN's, and the save is taken.
	finB := rowID(t, pool, `SELECT rowid FROM jobs WHERE name = 'fin-b' AND source = 'cronomicon'`)
	rec = gateReqWithHeader(t, h, http.MethodPut, "/api/v1/jobs/"+finB, gRoot, job("fin-b", ""), "X-Cronomicon-Agency", "ag:FIN")
	must("making fin-b, which FIN's workflow runs, scopeless", rec.Code, rec.Body.String())
	var owner, scope string
	if err := pool.QueryRow(`SELECT COALESCE(owner_agency, ''), COALESCE(scope, '') FROM jobs WHERE name = 'fin-b'`).Scan(&owner, &scope); err != nil {
		t.Fatal(err)
	}
	if owner != "ag:FIN" || scope != "" {
		t.Fatalf("fin-b after the save: owner %q, scope %q; want FIN's with no scope", owner, scope)
	}

	// A new workflow of FIN's running it beside a scoped job of FIN's. A workflow
	// that runs a scopeless job takes compose on every scope, as the job did; the
	// span rule is asked of everybody's save (§0a), a global administrator's too.
	rec = gateReqWithHeader(t, h, http.MethodPost, "/api/v1/workflows", gRoot, `{"name":"with-scopeless","steps":[`+stepA+`,`+stepB+`]}`, "X-Cronomicon-Agency", "ag:FIN")
	must("FIN's workflow of its scoped job and its scopeless one", rec.Code, rec.Body.String())
	if err := pool.QueryRow(`SELECT COALESCE(owner_agency, '') FROM workflows WHERE name = 'with-scopeless'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "ag:FIN" {
		t.Fatalf("the workflow is %q's; want FIN's", owner)
	}
}

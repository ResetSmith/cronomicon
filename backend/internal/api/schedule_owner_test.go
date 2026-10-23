package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// A schedule built in the app is its owner's (§0a, IN-8; the owner's answer
// to question 32). Writing it takes compose on that agency; a name is held
// once within an agency (I2; see TestI2_TwoAgenciesEachHaveTheirOwnScheduleOfOneName); the composer
// binds a definition to its own agency's only; and an edit reaches its own
// agency's definitions only: another agency's definition bound before §0a
// keeps the timing it copied, and the notice global_schedule_bound says so.
//
// Until §0a every such schedule was Global's, a compose administrator over
// every agency's alone to write, any definition bound to it, and an edit
// retimed every definition bound to it.
func TestI1_AScheduleIsItsOwnersToWrite(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's, tax-hosts is TAX's
	exec := mustExec(t, pool)
	exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	agenciesGetTheirOwnRepository(t, pool)
	code := func(who, method, path, body string) int {
		t.Helper()
		return gateReq(t, h, method, path, who, body).Code
	}

	// Writing.
	if c := code(gFinAdmin, http.MethodPost, "/api/v1/schedule-defs", `{"name":"fin-nightly","cron":"0 2 * * *","ownerAgency":"ag:FIN"}`); c != http.StatusCreated {
		t.Fatalf("FIN creating its own schedule = %d, want 201", c)
	}
	if got := count(t, pool, `SELECT COUNT(*) FROM schedules WHERE name = 'fin-nightly' AND owner_agency = 'ag:FIN'`); got != 1 {
		t.Errorf("FIN's schedule is not FIN's")
	}
	if c := code(gFinAdmin, http.MethodPost, "/api/v1/schedule-defs", `{"name":"for-tax","cron":"0 2 * * *","ownerAgency":"ag:TAX"}`); c != http.StatusForbidden {
		t.Errorf("FIN creating TAX's schedule = %d, want 403", c)
	}
	// 2.5.3 (CA-4): no schedule is Global's, whoever asks; the refusal is the
	// same for FIN's administrator as for a global administrator (below).
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/schedule-defs", gFinAdmin, `{"name":"for-global","cron":"0 2 * * *"}`); rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "central_runs_nothing" {
		t.Errorf("FIN creating Global's schedule = %d %s, want 422 central_runs_nothing", rec.Code, rec.Body)
	}
	// The name is FIN's within FIN only (I2): TAX has its own of the name,
	// and a route by the name alone must then say whose.
	if c := code(gTaxAdmin, http.MethodPost, "/api/v1/schedule-defs", `{"name":"fin-nightly","cron":"0 2 * * *","ownerAgency":"ag:TAX"}`); c != http.StatusCreated {
		t.Errorf("TAX creating its own schedule of the name FIN's holds = %d, want 201", c)
	}
	if c := code(gTaxAdmin, http.MethodPut, "/api/v1/schedule-defs/fin-nightly?agencyId=ag:FIN", `{"name":"fin-nightly","cron":"0 3 * * *"}`); c != http.StatusNotFound {
		t.Errorf("TAX editing FIN's schedule = %d, want 404 (question 72: not TAX's to read)", c)
	}
	if c := code(gTaxAdmin, http.MethodDelete, "/api/v1/schedule-defs/fin-nightly?agencyId=ag:FIN", ""); c != http.StatusNotFound {
		t.Errorf("TAX deleting FIN's schedule = %d, want 404 (question 72: not TAX's to read)", c)
	}
	if c := code(gTaxAdmin, http.MethodDelete, "/api/v1/schedule-defs/fin-nightly?agencyId=ag:TAX", ""); c != http.StatusNoContent {
		t.Errorf("TAX deleting its own schedule = %d, want 204", c)
	}
	if c := code(gFinAdmin, http.MethodPut, "/api/v1/schedule-defs/fin-nightly", `{"name":"fin-nightly","cron":"0 3 * * *"}`); c/100 != 2 {
		t.Errorf("FIN editing its own schedule = %d, want 2xx", c)
	}

	// Binding.
	job := func(name, scope, ref string) string {
		return `{"name":"` + name + `","scriptRef":"backup-db","scope":"` + scope + `","scheduleRefs":["` + ref + `"]}`
	}
	if c := code(gFinAdmin, http.MethodPost, "/api/v1/jobs", job("fin-job", "fin-hosts", "fin-nightly")); c/100 != 2 {
		t.Errorf("FIN's job bound to FIN's schedule = %d, want 2xx", c)
	}
	if c := code(gTaxAdmin, http.MethodPost, "/api/v1/jobs", job("tax-job", "tax-hosts", "fin-nightly")); c != http.StatusUnprocessableEntity {
		t.Errorf("TAX's job bound to FIN's schedule = %d, want 422", c)
	}

	// Editing Global's schedule reaches Global's definitions only. FIN's job
	// was bound to it before §0a (written here as that left it).
	//
	// Since 2.5.3 (CA-4) a global administrator cannot create Global's
	// schedule or job; what is Global's is an upgrade's leftover, written here
	// as an upgrade leaves it, and may still be edited.
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/schedule-defs", gRoot, `{"name":"g-nightly","cron":"0 1 * * *"}`); rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "central_runs_nothing" {
		t.Errorf("a global administrator creating Global's schedule = %d %s, want 422 central_runs_nothing", rec.Code, rec.Body)
	}
	exec(`INSERT INTO schedules (uid, name, source, cron, content_hash, owner_agency, created_at)
	      VALUES ('s-g-nightly', 'g-nightly', 'cronomicon', '0 1 * * *', 'h', 'global', 't')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, command, scope, owner_agency, synced_at)
	      VALUES ('j-g-job', 'g-job', 'cronomicon', 'bash', 'pg_dump mydb', 'shared', 'global', 't')`)
	exec(`INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, owner_uid, schedule_uid, source_ref)
	      VALUES ('cronomicon', 'job', 'g-job', 'g-nightly', '0 1 * * *', 0, 'j-g-job', 's-g-nightly', 'g-nightly')`)
	exec(`INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, owner_uid, schedule_uid, source_ref)
	      SELECT 'cronomicon', 'job', 'fin-job', 'g-nightly', '0 1 * * *', 1, j.uid, s.uid, 'g-nightly'
	        FROM jobs j, schedules s WHERE j.name = 'fin-job' AND j.source = 'cronomicon' AND s.name = 'g-nightly' AND s.source = 'cronomicon'`)
	if c := code(gRoot, http.MethodPut, "/api/v1/schedule-defs/g-nightly", `{"name":"g-nightly","cron":"0 5 * * *"}`); c/100 != 2 {
		t.Fatalf("editing Global's schedule = %d, want 2xx", c)
	}
	cronOf := func(owner string) string {
		var c string
		if err := pool.QueryRow(`SELECT cron FROM definition_schedules WHERE owner_name = ? AND name = 'g-nightly'`, owner).Scan(&c); err != nil {
			t.Fatalf("the entry of %s: %v", owner, err)
		}
		return c
	}
	if got := cronOf("g-job"); got != "0 5 * * *" {
		t.Errorf("Global's job fires at %q after Global's edit, want the new timing", got)
	}
	if got := cronOf("fin-job"); got != "0 1 * * *" {
		t.Errorf("FIN's job, bound to Global's schedule before §0a, fires at %q after Global's edit; want the timing it copied", got)
	}
	if err := notices.RunChecks(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM notices WHERE kind = ? AND agency_id = 'ag:FIN' AND resolved_at IS NULL`, notices.KindGlobalScheduleBound); n != 1 {
		t.Errorf("open global_schedule_bound notices for FIN = %d, want 1", n)
	}
}

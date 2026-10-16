package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// An in-app schedule's name is unique within its agency (§0a, IN-5; Phase
// I2): two agencies each have `nightly`, each writes its own, a route by the
// name alone asks whose when both exist, a job binds its own agency's, and an
// edit of one reaches that agency's definitions only.
func TestI2_TwoAgenciesEachHaveTheirOwnScheduleOfOneName(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's, tax-hosts is TAX's
	exec := mustExec(t, pool)
	exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	agenciesGetTheirOwnRepository(t, pool)
	code := func(who, method, path, body string) (int, string) {
		t.Helper()
		rec := gateReq(t, h, method, path, who, body)
		return rec.Code, rec.Body.String()
	}
	if c, b := code(gFinAdmin, http.MethodPost, "/api/v1/schedule-defs", `{"name":"nightly","cron":"0 2 * * *","ownerAgency":"ag:FIN"}`); c != http.StatusCreated {
		t.Fatalf("FIN creating its nightly = %d %s", c, b)
	}
	if c, b := code(gTaxAdmin, http.MethodPost, "/api/v1/schedule-defs", `{"name":"nightly","cron":"0 4 * * *","ownerAgency":"ag:TAX"}`); c != http.StatusCreated {
		t.Fatalf("TAX creating its nightly beside FIN's = %d %s, want 201", c, b)
	}
	if c, _ := code(gTaxAdmin, http.MethodPost, "/api/v1/schedule-defs", `{"name":"nightly","cron":"0 5 * * *","ownerAgency":"ag:TAX"}`); c != http.StatusConflict {
		t.Errorf("TAX creating a second nightly = %d, want 409", c)
	}

	// By the name alone the routes cannot tell them apart; with the agency they can.
	if c, b := code(gRoot, http.MethodGet, "/api/v1/schedule-defs/nightly?source=cronomicon", ""); c != http.StatusConflict || !strings.Contains(b, "ambiguous_schedule") {
		t.Errorf("reading nightly with two agencies holding it = %d %s, want 409 ambiguous_schedule", c, b)
	}
	if c, b := code(gRoot, http.MethodGet, "/api/v1/schedule-defs/nightly?source=cronomicon&agencyId=ag:TAX", ""); c != http.StatusOK || !strings.Contains(b, `"cron":"0 4 * * *"`) {
		t.Errorf("reading TAX's nightly = %d %s, want TAX's timing", c, b)
	}
	if c, _ := code(gRoot, http.MethodGet, "/api/v1/schedule-defs/nightly?source=cronomicon&agencyId=ag:NONE", ""); c != http.StatusNotFound {
		t.Errorf("reading nightly in an agency that has none = %d, want 404", c)
	}
	if c, b := code(gFinAdmin, http.MethodPut, "/api/v1/schedule-defs/nightly", `{"name":"nightly","cron":"0 3 * * *"}`); c != http.StatusConflict || !strings.Contains(b, "ambiguous_schedule") {
		t.Errorf("editing nightly by the name alone = %d %s, want 409 ambiguous_schedule", c, b)
	}

	// A job binds its own agency's; an edit of FIN's reaches FIN's job only.
	job := func(name, scope string) string {
		return `{"name":"` + name + `","scriptRef":"backup-db","scope":"` + scope + `","scheduleRefs":["nightly"]}`
	}
	if c, b := code(gFinAdmin, http.MethodPost, "/api/v1/jobs", job("fin-job", "fin-hosts")); c/100 != 2 {
		t.Fatalf("FIN's job bound to nightly = %d %s, want 2xx", c, b)
	}
	if c, b := code(gTaxAdmin, http.MethodPost, "/api/v1/jobs", job("tax-job", "tax-hosts")); c/100 != 2 {
		t.Fatalf("TAX's job bound to nightly = %d %s, want 2xx", c, b)
	}
	boundTo := func(jobName string) string {
		var owner string
		if err := pool.QueryRow(`SELECT s.owner_agency FROM definition_schedules ds JOIN schedules s ON s.uid = ds.schedule_uid
		                          WHERE ds.owner_name = ? AND ds.name = 'nightly'`, jobName).Scan(&owner); err != nil {
			t.Fatalf("the schedule %s is bound to: %v", jobName, err)
		}
		return owner
	}
	if got := boundTo("fin-job"); got != "ag:FIN" {
		t.Errorf("FIN's job is bound to %s's nightly, want FIN's", got)
	}
	if got := boundTo("tax-job"); got != "ag:TAX" {
		t.Errorf("TAX's job is bound to %s's nightly, want TAX's", got)
	}
	if c, b := code(gFinAdmin, http.MethodPut, "/api/v1/schedule-defs/nightly?agencyId=ag:FIN", `{"name":"nightly","cron":"0 3 * * *"}`); c/100 != 2 {
		t.Fatalf("FIN editing its nightly = %d %s, want 2xx", c, b)
	}
	cronOf := func(jobName string) string {
		var c string
		if err := pool.QueryRow(`SELECT cron FROM definition_schedules WHERE owner_name = ? AND name = 'nightly'`, jobName).Scan(&c); err != nil {
			t.Fatalf("the entry of %s: %v", jobName, err)
		}
		return c
	}
	if got := cronOf("fin-job"); got != "0 3 * * *" {
		t.Errorf("FIN's job fires at %q after FIN's edit, want the new timing", got)
	}
	if got := cronOf("tax-job"); got != "0 4 * * *" {
		t.Errorf("TAX's job fires at %q after FIN's edit, want its own timing kept", got)
	}
	if c, b := code(gRoot, http.MethodGet, "/api/v1/schedule-defs/nightly?source=cronomicon&agencyId=ag:FIN", ""); c != http.StatusOK || !strings.Contains(b, `"fin-job"`) || strings.Contains(b, `"tax-job"`) {
		t.Errorf("FIN's nightly is used by: %d %s, want FIN's job and not TAX's", c, b)
	}

	// Deleting TAX's (detaching its job) leaves FIN's, and the name is unambiguous again.
	if c, b := code(gTaxAdmin, http.MethodDelete, "/api/v1/schedule-defs/nightly?agencyId=ag:TAX&force=true", ""); c != http.StatusNoContent {
		t.Errorf("TAX deleting its nightly = %d %s, want 204", c, b)
	}
	if c, b := code(gFinAdmin, http.MethodGet, "/api/v1/schedule-defs/nightly?source=cronomicon", ""); c != http.StatusOK || !strings.Contains(b, `"cron":"0 3 * * *"`) {
		t.Errorf("reading nightly with one agency holding it = %d %s, want FIN's", c, b)
	}
}

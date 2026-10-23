package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// From the review of I2: a scope's name is unique within its agency, through
// the routes that create and move one. Creating a scope rewrites its
// membership, and between the delete and the insert the scope was Global's
// for one statement (the 1410 triggers), which a name Global holds refused:
// an agency's administrator could not create `web` while Global had `web`
// (a 500, and the scope taken back). Migration 1440 and the writers that set
// the owner first.
func TestI2_AnAgencyCreatesAScopeOfANameGlobalHolds(t *testing.T) {
	h, pool := gateServer(t)
	code := func(who, method, path, body string) (int, string) {
		t.Helper()
		rec := gateReq(t, h, method, path, who, body)
		return rec.Code, rec.Body.String()
	}
	if c, b := code(gRoot, http.MethodPost, "/api/v1/scopes", `{"scope":"web","hosts":["h0"]}`); c != http.StatusCreated {
		t.Fatalf("Global's web = %d %s", c, b)
	}
	if c, b := code(gFinAdmin, http.MethodPost, "/api/v1/scopes", `{"scope":"web","hosts":["h1"]}`); c != http.StatusCreated {
		t.Fatalf("FIN creating its web while Global has web = %d %s, want 201", c, b)
	}
	if c, b := code(gTaxAdmin, http.MethodPost, "/api/v1/scopes", `{"scope":"web","hosts":["h2"]}`); c != http.StatusCreated {
		t.Fatalf("TAX creating its web beside Global's and FIN's = %d %s, want 201", c, b)
	}
	if c, _ := code(gTaxAdmin, http.MethodPost, "/api/v1/scopes", `{"scope":"web","hosts":["h3"]}`); c != http.StatusConflict {
		t.Errorf("TAX creating a second web = %d, want 409", c)
	}
	for _, owner := range []string{"global", "ag:FIN", "ag:TAX"} {
		if n := count(t, pool, `SELECT COUNT(*) FROM scopes s WHERE s.name = 'web' AND s.owner_agency = ?
		                         AND (SELECT COUNT(*) FROM scope_agencies sa WHERE sa.scope_id = s.id AND sa.agency_id = ?) = 1`, owner, owner); n != 1 {
			t.Errorf("%s's web: %d row(s) owned and placed there, want 1", owner, n)
		}
	}
	// Moving TAX's web onto FIN, which has one, is refused as that and not as
	// a constraint failure half-way through; TAX's stays TAX's.
	var taxWeb string
	if err := pool.QueryRow(`SELECT id FROM scopes WHERE name = 'web' AND owner_agency = 'ag:TAX'`).Scan(&taxWeb); err != nil {
		t.Fatal(err)
	}
	if c, b := code(gRoot, http.MethodPut, "/api/v1/scopes/"+taxWeb+"/agency", asRepair(`{"agencyId":"ag:FIN"}`)); c != http.StatusConflict || !strings.Contains(b, "already exists") {
		t.Errorf("moving TAX's web onto FIN's = %d %s, want 409 scope name already exists", c, b)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scopes s JOIN scope_agencies sa ON sa.scope_id = s.id WHERE s.id = ? AND s.owner_agency = 'ag:TAX' AND sa.agency_id = 'ag:TAX'`, taxWeb); n != 1 {
		t.Error("TAX's web is not TAX's after the refused move")
	}
}

// From the review of I2: a list shows an agency's rows on a scope to the
// grants of that agency, not to another agency's grant on a scope of the
// same name. The detail routes refused them (ScopeReadableIn); the lists
// filtered by the scope's name alone.
func TestI2_AListShowsAScopesRowsToItsOwnAgencysGrants(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	agenciesGetTheirOwnRepository(t, pool)
	code := func(who, method, path, body string) (int, string) {
		t.Helper()
		rec := gateReq(t, h, method, path, who, body)
		return rec.Code, rec.Body.String()
	}
	for _, who := range []string{gFinAdmin, gTaxAdmin} {
		if c, b := code(who, http.MethodPost, "/api/v1/scopes", `{"scope":"web","hosts":["h1"]}`); c != http.StatusCreated {
			t.Fatalf("%s creating web = %d %s", who, c, b)
		}
	}
	job := func(name string) string {
		return `{"name":"` + name + `","scriptRef":"backup-db","scope":"web"}`
	}
	if c, b := code(gFinAdmin, http.MethodPost, "/api/v1/jobs", job("fin-web-job")); c/100 != 2 {
		t.Fatalf("FIN's job on its web = %d %s", c, b)
	}
	if c, b := code(gTaxAdmin, http.MethodPost, "/api/v1/jobs", job("tax-web-job")); c/100 != 2 {
		t.Fatalf("TAX's job on its web = %d %s", c, b)
	}
	exec(`INSERT INTO runs(id,job_name,run_type,scope,status,triggered_by,trigger_kind,executor,agencies_json,created_at)
	      VALUES('r-fin','fin-web-job','bash','web','success','t','manual','runner','["FIN"]','2026-01-01T00:00:00Z'),
	             ('r-tax','tax-web-job','bash','web','success','t','manual','runner','["TAX"]','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO run_agencies(run_id, agency) VALUES('r-fin','FIN'), ('r-tax','TAX')`)

	// A FIN viewer, granted FIN's web: FIN's job and run, not TAX's.
	if c, b := code(gFinViewer, http.MethodGet, "/api/v1/jobs?pageSize=100", ""); c != http.StatusOK || !strings.Contains(b, `"fin-web-job"`) || strings.Contains(b, `"tax-web-job"`) {
		t.Errorf("the jobs a FIN viewer lists = %d %s, want FIN's web job and not TAX's", c, b)
	}
	if c, b := code(gFinViewer, http.MethodGet, "/api/v1/runs?pageSize=100", ""); c != http.StatusOK || !strings.Contains(b, `"r-fin"`) || strings.Contains(b, `"r-tax"`) {
		t.Errorf("the runs a FIN viewer lists = %d %s, want FIN's run on web and not TAX's", c, b)
	}
	// A viewer of every agency sees both.
	if c, b := code(gAllViewer, http.MethodGet, "/api/v1/runs?pageSize=100", ""); c != http.StatusOK || !strings.Contains(b, `"r-fin"`) || !strings.Contains(b, `"r-tax"`) {
		t.Errorf("the runs a viewer of every agency lists = %d %s, want both", c, b)
	}
}

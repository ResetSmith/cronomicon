package api_test

import (
	"net/http"
	"testing"
)

// A save that changes a workflow's agency is a global administrator's repair
// (§0a, IN-7; the owner's answer to question 46). A workflow's agency follows
// its jobs (GR-6), so until then an editor who could compose both agencies'
// jobs made FIN's workflow TAX's by giving it TAX's jobs, with no word. Not a
// repair: a workflow edited within its agency, and one that had no jobs
// taking the agency of the first jobs it is given.
func TestI4_AWorkflowChangesAgencyOnlyByARepair(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	agenciesGetTheirOwnRepository(t, pool)
	both := gFinAdmin + "," + gTaxAdmin
	req := func(who, method, path, body string) int {
		t.Helper()
		return gateReq(t, h, method, path, who, body).Code
	}
	for _, j := range []struct{ who, name, scope string }{
		{gFinAdmin, "fin-a", "fin-hosts"}, {gFinAdmin, "fin-b", "fin-hosts"}, {gTaxAdmin, "tax-a", "tax-hosts"},
	} {
		if c := req(j.who, http.MethodPost, "/api/v1/jobs", `{"name":"`+j.name+`","scriptRef":"backup-db","scope":"`+j.scope+`"}`); c/100 != 2 {
			t.Fatalf("creating %s = %d", j.name, c)
		}
	}
	owner := func(name string) string {
		t.Helper()
		var o string
		if err := pool.QueryRow(`SELECT owner_agency FROM workflows WHERE source = 'cronomicon' AND name = ?`, name).Scan(&o); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return o
	}
	path := func(name string) string {
		return "/api/v1/workflows/" + rowID(t, pool, `SELECT rowid FROM workflows WHERE source = 'cronomicon' AND name = '`+name+`'`)
	}
	steps := func(name, job string) string {
		return `{"name":"` + name + `","steps":[{"type":"job","name":"` + job + `"}]}`
	}

	if c := req(gFinAdmin, http.MethodPost, "/api/v1/workflows", steps("nightly-flow", "fin-a")); c/100 != 2 || owner("nightly-flow") != "ag:FIN" {
		t.Fatalf("FIN's workflow: %d, %s's", c, owner("nightly-flow"))
	}
	// Within its agency: FIN's to edit, no repair.
	if c := req(gFinAdmin, http.MethodPut, path("nightly-flow"), steps("nightly-flow", "fin-b")); c != http.StatusOK || owner("nightly-flow") != "ag:FIN" {
		t.Errorf("FIN editing its workflow onto another of its jobs = %d, %s's; want 200, FIN's", c, owner("nightly-flow"))
	}
	// Onto TAX's jobs: refused to an administrator of both, with or without
	// asking for a repair; 409 to a global administrator who does not.
	for _, body := range []string{steps("nightly-flow", "tax-a"), asRepair(steps("nightly-flow", "tax-a"))} {
		if c := req(both, http.MethodPut, path("nightly-flow"), body); c != http.StatusForbidden {
			t.Errorf("an administrator of both agencies making FIN's workflow TAX's = %d, want 403", c)
		}
	}
	if c := req(gRoot, http.MethodPut, path("nightly-flow"), steps("nightly-flow", "tax-a")); c != http.StatusConflict {
		t.Errorf("a global administrator making it TAX's without a repair = %d, want 409", c)
	}
	if got := owner("nightly-flow"); got != "ag:FIN" {
		t.Fatalf("a refused save made the workflow %s's", got)
	}
	// The repair, audited with its reason.
	if c := req(gRoot, http.MethodPut, path("nightly-flow"), `{"repair":true,"reason":"built in FIN for TAX by mistake","name":"nightly-flow","steps":[{"type":"job","name":"tax-a"}]}`); c != http.StatusOK || owner("nightly-flow") != "ag:TAX" {
		t.Errorf("the repair = %d, %s's; want 200, TAX's", c, owner("nightly-flow"))
	}
	var uid string
	if err := pool.QueryRow(`SELECT uid FROM workflows WHERE name = 'nightly-flow'`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM change_log WHERE action = 'Repaired' AND target = ? AND details LIKE 'owner set to ag:TAX; reason: built in FIN for TAX by mistake'`, "workflow "+uid); n != 1 {
		t.Errorf("the workflow's repair audit row: %d, want 1", n)
	}

	// A workflow with no jobs yet is its creator's (question 52), and takes
	// the agency of the first jobs it is given without a repair.
	if c := req(gFinAdmin, http.MethodPost, "/api/v1/workflows", `{"name":"draft-flow","steps":[]}`); c/100 != 2 || owner("draft-flow") != "ag:FIN" {
		t.Fatalf("an empty workflow: %d, %s's", c, owner("draft-flow"))
	}
	if c := req(gFinAdmin, http.MethodPut, path("draft-flow"), steps("draft-flow", "fin-a")); c != http.StatusOK || owner("draft-flow") != "ag:FIN" {
		t.Errorf("FIN filling its empty workflow with its jobs = %d, %s's; want 200, FIN's", c, owner("draft-flow"))
	}
}

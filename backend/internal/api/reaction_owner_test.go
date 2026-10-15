package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// A reaction's upstream is looked for in the agency of the definition that
// holds the reaction, found by that definition's uid (§0a, IN-8; Phase I2):
// two agencies each have a job `reactor`, and only one of them has an
// `upstream` for it to react to. By the name, "the" reactor was whichever
// came first, and the other agency's reaction was accepted against an
// upstream its agency does not have — an edge that would never fire, shown
// as healthy.
func TestI2_AReactionsUpstreamIsLookedForInItsOwnersAgency(t *testing.T) {
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
	job := func(name, scope string) string {
		return `{"name":"` + name + `","scriptRef":"backup-db","scope":"` + scope + `"}`
	}
	for _, j := range []struct{ who, name, scope string }{
		{gFinAdmin, "upstream", "fin-hosts"}, {gFinAdmin, "reactor", "fin-hosts"}, {gTaxAdmin, "reactor", "tax-hosts"},
	} {
		if c, b := code(j.who, http.MethodPost, "/api/v1/jobs", job(j.name, j.scope)); c/100 != 2 {
			t.Fatalf("creating %s's %s = %d %s", j.who, j.name, c, b)
		}
	}
	uidOf := func(owner string) string {
		var uid string
		if err := pool.QueryRow(`SELECT uid FROM jobs WHERE source = 'cronomicon' AND name = 'reactor' AND owner_agency = ?`, owner).Scan(&uid); err != nil {
			t.Fatalf("%s's reactor: %v", owner, err)
		}
		return uid
	}
	reaction := `{"reactions":[{"name":"after","onKind":"job","onName":"upstream","onSource":"cronomicon","onOutcome":"success"}]}`
	if c, b := code(gRoot, http.MethodPut, "/api/v1/reactions/job/reactor?uid="+uidOf("ag:FIN"), reaction); c/100 != 2 {
		t.Errorf("FIN's reactor reacting to FIN's upstream = %d %s, want 2xx", c, b)
	}
	if c, b := code(gRoot, http.MethodPut, "/api/v1/reactions/job/reactor?uid="+uidOf("ag:TAX"), reaction); c != http.StatusUnprocessableEntity || !strings.Contains(b, "no such job upstream") {
		t.Errorf("TAX's reactor reacting to an upstream TAX does not have = %d %s, want 422 no such job", c, b)
	}
	if c, b := code(gFinAdmin, http.MethodGet, "/api/v1/reactions/job/reactor?uid="+uidOf("ag:FIN"), ""); c != http.StatusOK || !strings.Contains(b, `"missing":false`) {
		t.Errorf("FIN's reaction as listed = %d %s, want it present and not missing", c, b)
	}
}

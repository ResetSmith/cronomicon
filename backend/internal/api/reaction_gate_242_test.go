package api_test

import (
	"net/http"
	"testing"
)

// Question 61 (2.4.2): a definition's reactions are written by compose on the
// definition's agency, as calendars and schedules are. Until then only an
// administrator holding compose and configureApp on every agency could, for a
// reason §0a removed (a reaction's upstream is its own agency's).
func TestRV_ADefinitionsReactionsAreItsAgencysToWrite(t *testing.T) {
	h, pool := gateServer(t)
	mustExec(t, pool)(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	agenciesGetTheirOwnRepository(t, pool)
	for _, j := range []string{"fin-after", "fin-before"} {
		if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs", gFinAdmin, `{"name":"`+j+`","scriptRef":"backup-db","scope":"fin-hosts"}`); rec.Code/100 != 2 {
			t.Fatalf("FIN creates %s: %d %s", j, rec.Code, rec.Body)
		}
	}
	body := `{"reactions":[{"name":"when-before","onKind":"job","onSource":"cronomicon","onName":"fin-before","onOutcome":"success"}]}`
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/reactions/job/fin-after", gTaxAdmin, body); rec.Code != http.StatusForbidden {
		t.Errorf("TAX writing FIN's job's reactions = %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/reactions/job/fin-after", gFinAdmin, body); rec.Code/100 != 2 {
		t.Errorf("FIN writing its own job's reactions = %d %s, want it saved", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/reactions/job/fin-after/when-before", gTaxAdmin, ""); rec.Code != http.StatusForbidden {
		t.Errorf("TAX removing FIN's job's reaction = %d, want 403", rec.Code)
	}
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/reactions/job/fin-after/when-before", gFinAdmin, ""); rec.Code/100 != 2 {
		t.Errorf("FIN removing its own job's reaction = %d %s", rec.Code, rec.Body)
	}
}

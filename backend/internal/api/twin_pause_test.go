package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Pausing one agency's definition does not show another agency's definition
// of the same name as paused (2.4.0 Phase R6). The pause is written with the
// identity of the definition it pauses, and the lists and the detail read it
// by the name: pausing TAX's `nightly` showed FIN's `nightly` as disabled too
// (and the scheduler, which read it the same way, did not fire it:
// scheduler.IsPausedUID).
func TestPausingOneAgencysDefinitionIsNotShownOnAnothersOfTheSameName(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's, tax-hosts is TAX's
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, enabled, created_at) VALUES
	      ('j-fin-step', 'fin-job', 'cronomicon', 'bash', 'fin-hosts', 1, '2026-01-01T00:00:00Z'),
	      ('j-tax-step', 'tax-job', 'cronomicon', 'bash', 'tax-hosts', 1, '2026-01-01T00:00:00Z'),
	      ('j-fin-load', 'load',    'cronomicon', 'bash', 'fin-hosts', 1, '2026-01-01T00:00:00Z'),
	      ('j-tax-load', 'load',    'cronomicon', 'bash', 'tax-hosts', 1, '2026-01-01T00:00:00Z')`)
	for who, steps := range map[string]string{gFinAdmin: `[{"type":"job","name":"fin-job"}]`, gTaxAdmin: `[{"type":"job","name":"tax-job"}]`} {
		if rec := gateReq(t, h, http.MethodPost, "/api/v1/workflows", who, `{"name":"nightly","steps":`+steps+`}`); rec.Code/100 != 2 {
			t.Fatalf("%s creating its nightly = %d (%s)", who, rec.Code, rec.Body)
		}
	}
	fin := rowID(t, pool, `SELECT rowid FROM workflows WHERE name = 'nightly' AND owner_agency = 'ag:FIN'`)
	tax := rowID(t, pool, `SELECT rowid FROM workflows WHERE name = 'nightly' AND owner_agency = 'ag:TAX'`)

	// TAX's administrator pauses TAX's nightly.
	if rec := gateReq(t, h, http.MethodPatch, "/api/v1/workflows/"+tax, gTaxAdmin, `{"disabled":true}`); rec.Code/100 != 2 {
		t.Fatalf("pausing TAX's nightly = %d (%s)", rec.Code, rec.Body)
	}
	disabled := func(what, path string) map[string]bool {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, path, gRoot, "")
		var page struct {
			Items []struct {
				OwnerAgency string
				Disabled    bool
				Name        string
			}
			OwnerAgency string
			Disabled    bool
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("%s: %v (%s)", what, err, rec.Body)
		}
		out := map[string]bool{}
		for _, it := range page.Items {
			if it.Name == "nightly" {
				out[it.OwnerAgency] = it.Disabled
			}
		}
		if page.OwnerAgency != "" {
			out[page.OwnerAgency] = page.Disabled
		}
		return out
	}
	if got := disabled("the workflow list", "/api/v1/workflows"); !got["ag:TAX"] || got["ag:FIN"] {
		t.Errorf("the list shows TAX's nightly disabled = %v and FIN's = %v, want true and false", got["ag:TAX"], got["ag:FIN"])
	}
	if got := disabled("TAX's detail", "/api/v1/workflows/"+tax); !got["ag:TAX"] {
		t.Errorf("TAX's nightly, which is paused, reads as not disabled")
	}
	if got := disabled("FIN's detail", "/api/v1/workflows/"+fin); got["ag:FIN"] {
		t.Errorf("FIN's nightly reads as disabled: TAX's of the same name is paused")
	}

	// The same for two agencies' jobs of one name: TAX's is paused.
	exec(`INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at, owner_uid) VALUES ('cronomicon', 'job', 'load', 'u', 't', 'j-tax-load')`)
	rec := gateReq(t, h, http.MethodGet, "/api/v1/jobs?q=load", gRoot, "")
	var jobs struct {
		Items []struct{ UID, Name, Status string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("the job list: %v (%s)", err, rec.Body)
	}
	status := map[string]string{}
	for _, j := range jobs.Items {
		status[j.UID] = j.Status
	}
	if status["j-tax-load"] != "paused" || status["j-fin-load"] == "paused" || status["j-fin-load"] == "" {
		t.Errorf("the job list shows TAX's load as %q and FIN's as %q, want paused and not paused", status["j-tax-load"], status["j-fin-load"])
	}
	// And each job read on its own.
	for uid, want := range map[string]bool{"j-tax-load": true, "j-fin-load": false} {
		rec := gateReq(t, h, http.MethodGet, "/api/v1/jobs/"+rowID(t, pool, `SELECT rowid FROM jobs WHERE uid = ?`, uid), gRoot, "")
		var one struct{ Status string }
		if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("reading the job %s = %d (%v) %s", uid, rec.Code, err, rec.Body)
		}
		if (one.Status == "paused") != want {
			t.Errorf("the job %s reads as %q, want paused = %v", uid, one.Status, want)
		}
	}
}

package api_test

import (
	"net/http"
	"testing"
)

// PINNED, present defect 35 (found on 2026-10-10 while grounding 2.4.0's Phase
// R4; in the released code).
//
// A workflow built in the app shares its name only with workflows of OTHER
// agencies: creating one whose name its agency already has is refused (409,
// "name already in use"). Editing one is not checked at all. A workflow whose
// steps are changed to another agency's jobs moves into that agency's names,
// where the name may be taken: the agency then has two workflows of one name,
// which creating either would have refused. What starts a workflow by its name
// (present defect 34) then cannot tell them apart.
//
// When this is fixed (the edit asks the same question the create asks, leaving
// the workflow's own row out), this test is inverted: the edit answers 409.
func TestGR0_AWorkflowEditCanTakeANameItsNewAgencyAlreadyHas(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's, tax-hosts is TAX's
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled, created_at) VALUES
	      ('fin-job', 'cronomicon', 'bash', 'fin-hosts', 1, '2026-01-01T00:00:00Z'),
	      ('tax-job', 'cronomicon', 'bash', 'tax-hosts', 1, '2026-01-01T00:00:00Z')`)
	const finSteps, taxSteps = `[{"type":"job","name":"fin-job"}]`, `[{"type":"job","name":"tax-job"}]`

	// Each agency creates a workflow called nightly: two agencies, one name, allowed.
	for who, steps := range map[string]string{gFinAdmin: finSteps, gTaxAdmin: taxSteps} {
		if rec := gateReq(t, h, http.MethodPost, "/api/v1/workflows", who, `{"name":"nightly","steps":`+steps+`}`); rec.Code/100 != 2 {
			t.Fatalf("%s creating its nightly = %d (%s)", who, rec.Code, rec.Body)
		}
	}
	// A second one in the SAME agency is refused at creation.
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/workflows", gFinAdmin, `{"name":"nightly","steps":`+finSteps+`}`); rec.Code != http.StatusConflict {
		t.Fatalf("FIN creating a second nightly = %d, want 409 (%s)", rec.Code, rec.Body)
	}

	// TAX's nightly is edited to run FIN's job. It is FIN's now, by its steps,
	// and FIN already has a nightly.
	tax := rowID(t, pool, `SELECT rowid FROM workflows WHERE name = 'nightly' AND owner_agency = 'ag:TAX'`)
	rec := gateReq(t, h, http.MethodPut, "/api/v1/workflows/"+tax, gRoot, `{"steps":`+finSteps+`}`)
	if rec.Code == http.StatusConflict {
		t.Fatalf("PIN BROKEN: the edit was refused (%s). Present defect 35 is fixed: invert this test (want 409 and one FIN nightly)", rec.Body)
	}
	if rec.Code/100 != 2 {
		t.Fatalf("the edit = %d (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM workflows WHERE name = 'nightly' AND source = 'cronomicon' AND owner_agency = 'ag:FIN' AND deleted_at IS NULL`); n != 2 {
		t.Fatalf("FIN's workflows called nightly after the edit = %d; the pin expects the two", n)
	}
}

// PINNED, present defect 36 (found on 2026-10-10 while grounding 2.4.0's Phase
// R4; in the released code).
//
// The reactions of a definition built in the app are written through a route
// that names the definition: PUT /reactions/{kind}/{name}. Two agencies may
// each hold a job of one name (R2-5). The route finds "the" definition by the
// name, replaces every reaction row of that source, kind and NAME, and stamps
// the new rows' owner only when one definition holds the name. So saving one
// twin's reactions deletes the other twin's, and the rows it writes belong to
// neither by identity.
//
// It belongs with the uid on those routes (Phase R6). When it is fixed this
// test is inverted: the other twin's reaction is still there.
func TestGR0_SavingOneTwinsReactionsDeletesTheOthers(t *testing.T) {
	api, pool := newRxAPI(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (name, uid, source, run_type, enabled, synced_at) VALUES
	      ('extract', 'uid-extract', 'cronomicon', 'bash', 1, 't'),
	      ('load', 'uid-load-fin', 'cronomicon', 'bash', 1, 't'),
	      ('load', 'uid-load-tax', 'cronomicon', 'bash', 1, 't')`)
	// The other twin's reaction, as its own save would have left it had it been
	// the only `load` at the time: tied to it by identity.
	exec(`INSERT INTO reactions (owner_source, owner_kind, owner_name, name, on_source, on_kind, on_name, on_outcome,
	                             delay_seconds, min_interval_seconds, include_workflow_children, enabled, position, owner_uid, on_uid)
	      VALUES ('cronomicon', 'job', 'load', 'tax-after-extract', 'cronomicon', 'job', 'extract', 'success', 0, 0, 0, 1, 0, 'uid-load-tax', 'uid-extract')`)

	code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/load", map[string]any{
		"reactions": []map[string]any{{"name": "fin-after-extract", "onKind": "job", "onName": "extract", "onSource": "cronomicon", "onOutcome": "failure"}},
	})
	if code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", code, body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM reactions WHERE name = 'tax-after-extract'`); n != 0 {
		t.Fatalf("PIN BROKEN: the other twin's reaction survived the save. Present defect 36 is fixed: invert this test (want it kept)")
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM reactions WHERE name = 'fin-after-extract' AND owner_uid IS NULL`); n != 1 {
		t.Fatalf("the saved reaction with no owner identity: %d rows; the pin expects the one", n)
	}
}

package api_test

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// An edit of a workflow asks the name pool, as its creation does (2.4.0 Phase
// R6; present defect 35, found on 2026-10-10 while grounding Phase R4, in the
// released code).
//
// Two agencies may each hold a workflow of one name (R2-5), and one agency may
// not hold two: creating the second is refused. A workflow's agency follows the
// jobs it runs, so EDITING its steps can move it to an agency that already has
// a workflow of the name. The edit did not ask, and that agency then had two:
// what finds a workflow by its name could not tell them apart. It is refused
// now, in the words the create uses, and nothing is written. This was the pin
// TestGR0_AWorkflowEditCanTakeANameItsNewAgencyAlreadyHas.
func TestAWorkflowEditIsRefusedANameItsNewAgencyAlreadyHas(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's, tax-hosts is TAX's
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled, created_at) VALUES
	      ('fin-job',  'cronomicon', 'bash', 'fin-hosts', 1, '2026-01-01T00:00:00Z'),
	      ('fin-job2', 'cronomicon', 'bash', 'fin-hosts', 1, '2026-01-01T00:00:00Z'),
	      ('tax-job',  'cronomicon', 'bash', 'tax-hosts', 1, '2026-01-01T00:00:00Z'),
	      ('tax-job2', 'cronomicon', 'bash', 'tax-hosts', 1, '2026-01-01T00:00:00Z')`)
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
	created := gateReq(t, h, http.MethodPost, "/api/v1/workflows", gFinAdmin, `{"name":"nightly","steps":`+finSteps+`}`).Body.String()
	nightlies := func(agency string) int {
		return count(t, pool, `SELECT COUNT(*) FROM workflows WHERE name = 'nightly' AND source = 'cronomicon' AND owner_agency = ? AND deleted_at IS NULL`, agency)
	}
	tax := rowID(t, pool, `SELECT rowid FROM workflows WHERE name = 'nightly' AND owner_agency = 'ag:TAX'`)
	stepsOf := func() string {
		var s string
		if err := pool.QueryRow(`SELECT steps FROM workflows WHERE rowid = ?`, tax).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := stepsOf()

	// TAX's nightly is edited to run FIN's job. It would be FIN's, by its
	// steps, and FIN already has a nightly: refused, as the create is, and
	// nothing is written.
	rec := gateReq(t, h, http.MethodPut, "/api/v1/workflows/"+tax, gRoot, `{"steps":`+finSteps+`}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("the edit that would give FIN a second nightly = %d (%s), want 409", rec.Code, rec.Body)
	}
	if rec.Body.String() != created {
		t.Errorf("the edit's refusal is not the create's:\n%s\n%s", rec.Body, created)
	}
	if nightlies("ag:FIN") != 1 || nightlies("ag:TAX") != 1 || stepsOf() != before {
		t.Fatalf("after the refused edit: FIN has %d nightly, TAX has %d, and TAX's steps are %s", nightlies("ag:FIN"), nightlies("ag:TAX"), stepsOf())
	}

	// An edit that keeps the workflow in its agency is not refused by its own
	// name: its own row is left out of the question.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/workflows/"+tax, gRoot, `{"steps":[{"type":"job","name":"tax-job2"}]}`); rec.Code/100 != 2 {
		t.Fatalf("an edit that keeps TAX's nightly in TAX = %d (%s), want it saved", rec.Code, rec.Body)
	}
	fin := rowID(t, pool, `SELECT rowid FROM workflows WHERE name = 'nightly' AND owner_agency = 'ag:FIN'`)
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/workflows/"+fin, gFinAdmin, `{"steps":[{"type":"job","name":"fin-job2"}]}`); rec.Code/100 != 2 {
		t.Fatalf("FIN editing its own nightly = %d (%s), want it saved", rec.Code, rec.Body)
	}
	if nightlies("ag:FIN") != 1 || nightlies("ag:TAX") != 1 {
		t.Fatalf("after the two edits: FIN has %d nightly and TAX has %d, want one each", nightlies("ag:FIN"), nightlies("ag:TAX"))
	}
}

// The reactions of a definition are written by its identity (2.4.0 Phase R6;
// present defect 36, found on 2026-10-10 while grounding Phase R4, in the
// released code).
//
// The reactions of a definition built in the app are written through a route
// that names the definition: PUT /reactions/{kind}/{name}. Two agencies may
// each hold a job of one name (R2-5). The route found "the" definition by the
// name, replaced every reaction row of that source, kind and NAME, and stamped
// the new rows' owner only when one definition held the name: saving one
// twin's reactions deleted the other twin's, and the rows it wrote belonged to
// neither by identity. This was the pin
// TestGR0_SavingOneTwinsReactionsDeletesTheOthers.
//
// ?uid= says which definition. A write that does not say, under a name more
// than one definition holds, is refused and changes nothing. A write touches
// its own definition's rows and stamps them with its identity.
func TestADefinitionsReactionsAreWrittenByItsIdentity(t *testing.T) {
	api, pool := newRxAPI(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (name, uid, source, run_type, enabled, synced_at) VALUES
	      ('extract', 'uid-extract',  'cronomicon', 'bash', 1, 't'),
	      ('load',    'uid-load-fin', 'cronomicon', 'bash', 1, 't'),
	      ('load',    'uid-load-tax', 'cronomicon', 'bash', 1, 't'),
	      ('solo',    'uid-solo',     'cronomicon', 'bash', 1, 't')`)
	// The other twin's reaction, tied to it by identity; and under the name one
	// job holds, a row from before a reaction recorded its owner.
	exec(`INSERT INTO reactions (owner_source, owner_kind, owner_name, name, on_source, on_kind, on_name, on_outcome,
	                             delay_seconds, min_interval_seconds, include_workflow_children, enabled, position, owner_uid, on_uid)
	      VALUES ('cronomicon', 'job', 'load', 'tax-after-extract', 'cronomicon', 'job', 'extract', 'success', 0, 0, 0, 1, 0, 'uid-load-tax', 'uid-extract'),
	             ('cronomicon', 'job', 'solo', 'old-row',           'cronomicon', 'job', 'extract', 'success', 0, 0, 0, 1, 0, NULL,           'uid-extract')`)
	one := func(name, outcome string) map[string]any {
		return map[string]any{"reactions": []map[string]any{{"name": name, "onKind": "job", "onName": "extract", "onSource": "cronomicon", "onOutcome": outcome}}}
	}
	ownerOf := func(reaction string) string {
		t.Helper()
		var uid sql.NullString
		err := pool.QueryRow(`SELECT owner_uid FROM reactions WHERE name = ?`, reaction).Scan(&uid)
		if errors.Is(err, sql.ErrNoRows) {
			return "(gone)"
		}
		if err != nil {
			t.Fatal(err)
		}
		if !uid.Valid {
			return "(no owner)"
		}
		return uid.String
	}

	// A name two jobs hold, and no uid: refused, and nothing is changed.
	code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/load", one("fin-after-extract", "failure"))
	if code != http.StatusConflict || !strings.Contains(string(body), "ambiguous_definition") {
		t.Fatalf("a write that does not say which load = %d (%s), want 409 ambiguous_definition", code, body)
	}
	if ownerOf("tax-after-extract") != "uid-load-tax" || ownerOf("fin-after-extract") != "(gone)" {
		t.Fatalf("the refused write changed the reactions: tax's is %s, fin's is %s", ownerOf("tax-after-extract"), ownerOf("fin-after-extract"))
	}
	// A uid that is not a job of that name.
	if code, _ := api.doRaw(http.MethodPut, "/api/v1/reactions/job/load?uid=uid-solo", one("x", "failure")); code != http.StatusNotFound {
		t.Errorf("a uid that is another job's = %d, want 404", code)
	}

	// FIN's load, by its uid: its reaction is written as its own, and TAX's
	// load's reaction is where it was.
	code, body = api.doRaw(http.MethodPut, "/api/v1/reactions/job/load?uid=uid-load-fin", one("fin-after-extract", "failure"))
	if code != http.StatusOK {
		t.Fatalf("FIN's load's reactions = %d (%s)", code, body)
	}
	if strings.Contains(string(body), "tax-after-extract") || !strings.Contains(string(body), "fin-after-extract") {
		t.Errorf("the answer is not FIN's load's reactions alone: %s", body)
	}
	if got := ownerOf("tax-after-extract"); got != "uid-load-tax" {
		t.Fatalf("the other twin's reaction after the save is %s, want it kept, its own", got)
	}
	if got := ownerOf("fin-after-extract"); got != "uid-load-fin" {
		t.Fatalf("the saved reaction's owner is %s, want uid-load-fin", got)
	}
	// Each twin's list is its own.
	for uid, want := range map[string]string{"uid-load-fin": "fin-after-extract", "uid-load-tax": "tax-after-extract"} {
		other := map[string]string{"uid-load-fin": "tax-after-extract", "uid-load-tax": "fin-after-extract"}[uid]
		_, body := api.doRaw(http.MethodGet, "/api/v1/reactions/job/load?uid="+uid, nil)
		if !strings.Contains(string(body), want) || strings.Contains(string(body), other) {
			t.Errorf("the reactions of %s: %s", uid, body)
		}
	}
	// Saving FIN's again replaces FIN's, and still not TAX's.
	if code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/load?uid=uid-load-fin", one("fin-second", "success")); code != http.StatusOK {
		t.Fatalf("FIN's load's reactions, again = %d (%s)", code, body)
	}
	if ownerOf("fin-after-extract") != "(gone)" || ownerOf("fin-second") != "uid-load-fin" || ownerOf("tax-after-extract") != "uid-load-tax" {
		t.Fatalf("after FIN's second save: fin-after-extract %s, fin-second %s, tax-after-extract %s",
			ownerOf("fin-after-extract"), ownerOf("fin-second"), ownerOf("tax-after-extract"))
	}

	// Removing one: the same rule.
	if code, _ := api.doRaw(http.MethodDelete, "/api/v1/reactions/job/load/fin-second", nil); code != http.StatusConflict {
		t.Errorf("removing a reaction without saying which load = %d, want 409", code)
	}
	if code, _ := api.doRaw(http.MethodDelete, "/api/v1/reactions/job/load/fin-second?uid=uid-load-tax", nil); code != http.StatusNotFound {
		t.Errorf("removing FIN's reaction as TAX's load's = %d, want 404", code)
	}
	if ownerOf("fin-second") != "uid-load-fin" {
		t.Fatalf("a refused removal removed the reaction")
	}
	if code, _ := api.doRaw(http.MethodDelete, "/api/v1/reactions/job/load/fin-second?uid=uid-load-fin", nil); code != http.StatusNoContent {
		t.Errorf("removing FIN's reaction as FIN's load's = %d, want 204", code)
	}
	if ownerOf("fin-second") != "(gone)" || ownerOf("tax-after-extract") != "uid-load-tax" {
		t.Fatalf("after the removal: fin-second %s, tax-after-extract %s", ownerOf("fin-second"), ownerOf("tax-after-extract"))
	}

	// A name ONE job holds needs no uid, as before. Its rows are stamped with
	// its identity, and a row of the name that records no owner is its and is
	// replaced with the rest.
	if code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/solo", one("solo-after-extract", "success")); code != http.StatusOK {
		t.Fatalf("the reactions of a job whose name is its own = %d (%s)", code, body)
	}
	if ownerOf("old-row") != "(gone)" || ownerOf("solo-after-extract") != "uid-solo" {
		t.Fatalf("after solo's save: old-row %s, solo-after-extract %s", ownerOf("old-row"), ownerOf("solo-after-extract"))
	}
	if ownerOf("tax-after-extract") != "uid-load-tax" {
		t.Fatalf("solo's save touched another job's reaction")
	}
}

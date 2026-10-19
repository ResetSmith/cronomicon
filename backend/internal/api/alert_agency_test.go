package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// AL-1 and AL-3 (2.5.0 C4): an alert rule is one agency's. It is created in
// the agency the caller administers, read by that agency's people and by
// everywhere-readers (CF-1), changed with configureApp on its agency, and
// names only its own agency's job. Until C4 every rule was every session's to
// read and a global administrator's alone to write (GC-2).
func TestAlertRulesBelongToAnAgency(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at, owner_agency) VALUES
	        ('fin-load', 'cronomicon', 'uid-fin', 'bash', 't', 'ag:FIN'),
	        ('tax-load', 'cronomicon', 'uid-tax', 'bash', 't', 'ag:TAX')`)
	// What an upgraded installation holds: a rule with no owner named is Global's.
	exec(`INSERT INTO alert_config (id, target_mode, trigger, channels, recipients, enabled, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES ('a-old', 'all', 'failure', '["email"]', 'ops@example', 1, 'seed', '2026-01-01T00:00:00Z', 'seed', '2026-01-01T00:00:00Z')`)
	if n := count(t, pool, `SELECT COUNT(*) FROM alert_config WHERE id='a-old' AND owner_agency='global'`); n != 1 {
		t.Fatal("fixture: a rule with no owner named is not Global's")
	}
	rule := func(job string) string {
		if job == "" {
			return `{"targetMode":"all","trigger":"failure","channels":["email"],"recipients":"fin@example","enabled":true}`
		}
		return `{"targetMode":"job","jobName":"` + job + `","trigger":"failure","channels":["email"],"recipients":"fin@example","enabled":true}`
	}
	type made struct {
		ID          string `json:"id"`
		OwnerAgency string `json:"ownerAgency"`
	}

	// FIN's administrator: born FIN's, with no owner named.
	rec := gateReq(t, h, http.MethodPost, "/api/v1/alerts", gFinAdmin, rule("fin-load"))
	var fin made
	_ = json.Unmarshal(rec.Body.Bytes(), &fin)
	if rec.Code != http.StatusCreated || fin.OwnerAgency != "ag:FIN" {
		t.Fatalf("fin admin creating a rule = %d owned by %q, want 201 and ag:FIN (%s)", rec.Code, fin.OwnerAgency, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM alert_config WHERE id=? AND job_uid='uid-fin'`, fin.ID); n != 1 {
		t.Error("FIN's rule did not resolve its job in FIN")
	}
	// Not in Global or TAX, and not naming TAX's job.
	for _, c := range []struct {
		body string
		want int
	}{
		{strings.Replace(rule(""), `{`, `{"ownerAgency":"global",`, 1), http.StatusForbidden},
		{strings.Replace(rule(""), `{`, `{"ownerAgency":"ag:TAX",`, 1), http.StatusForbidden},
		{rule("tax-load"), http.StatusUnprocessableEntity},
	} {
		if rec := gateReq(t, h, http.MethodPost, "/api/v1/alerts", gFinAdmin, c.body); rec.Code != c.want {
			t.Errorf("fin admin POST %s = %d, want %d (%s)", c.body, rec.Code, c.want, rec.Body)
		}
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/alerts", gFinAdmin, rule("tax-load")); errCode(rec.Body.Bytes()) != "unknown_job" {
		t.Errorf("naming another agency's job: code %q, want unknown_job", errCode(rec.Body.Bytes()))
	}
	// Nor change Global's.
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		if rec := gateReq(t, h, m, "/api/v1/alerts/a-old", gFinAdmin, rule("")); rec.Code != http.StatusNotFound {
			t.Errorf("fin admin %s Global's rule = %d, want 404: it does not read it (%s)", m, rec.Code, rec.Body)
		}
	}

	// TAX's administrator does not read FIN's rule: 404, as for any object it cannot read (question 72).
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		if rec := gateReq(t, h, m, "/api/v1/alerts/"+fin.ID, gTaxAdmin, rule("")); rec.Code != http.StatusNotFound {
			t.Errorf("tax admin %s FIN's rule = %d, want 404 (%s)", m, rec.Code, rec.Body)
		}
	}
	// FIN's viewer reads it and may not change it.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/alerts/"+fin.ID, gFinViewer, rule("")); rec.Code != http.StatusForbidden {
		t.Errorf("fin viewer editing FIN's rule = %d, want 403 (%s)", rec.Code, rec.Body)
	}

	// Who reads which: FIN's people FIN's; TAX's people none; everywhere-readers all.
	ids := func(groups string) string {
		rec := gateReq(t, h, http.MethodGet, "/api/v1/alerts", groups, "")
		var list []made
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("GET /alerts as %s: %d %s", groups, rec.Code, rec.Body)
		}
		var out []string
		for _, a := range list {
			out = append(out, a.OwnerAgency)
		}
		return strings.Join(out, ",")
	}
	for groups, want := range map[string]string{
		gFinViewer: "ag:FIN", gTaxAdmin: "", gAllViewer: "global,ag:FIN", gRoot: "global,ag:FIN",
	} {
		if got := ids(groups); got != want {
			t.Errorf("GET /alerts as %s lists %q, want %q", groups, got, want)
		}
	}

	// FIN's administrator edits its own: the agency stays, and an edit naming
	// another agency's job is refused.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/alerts/"+fin.ID, gFinAdmin, rule("tax-load")); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("fin admin pointing its rule at TAX's job = %d, want 422 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/alerts/"+fin.ID, gFinAdmin, rule("")); rec.Code != http.StatusOK {
		t.Errorf("fin admin editing its own rule = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	// A global administrator writes Global's with no owner named, as before.
	rec = gateReq(t, h, http.MethodPost, "/api/v1/alerts", gRoot, rule("tax-load"))
	var glob made
	_ = json.Unmarshal(rec.Body.Bytes(), &glob)
	if rec.Code != http.StatusCreated || glob.OwnerAgency != "global" {
		t.Errorf("root creating a rule = %d owned by %q, want 201 and global (%s)", rec.Code, glob.OwnerAgency, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/alerts/"+fin.ID, gRoot, rule("")); rec.Code != http.StatusOK {
		t.Errorf("root editing FIN's rule = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	// The audit row is FIN's (Q74).
	if n := count(t, pool, `SELECT COUNT(*) FROM change_log WHERE category='Alerts' AND agency_id='ag:FIN'`); n < 2 {
		t.Errorf("FIN's rule's audit rows carry FIN: %d, want its create and edits", n)
	}

	// An agency that owns a rule is not deleted from under it.
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag:OPS','OPS','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO alert_config (id, target_mode, trigger, channels, enabled, created_by, created_at, last_modified_by, last_modified_at, owner_agency)
	      VALUES ('a-ops', 'all', 'failure', '["email"]', 1, 'seed', '2026-01-01T00:00:00Z', 'seed', '2026-01-01T00:00:00Z', 'ag:OPS')`)
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/agencies/ag:OPS", gRoot, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "alert rule") {
		t.Errorf("deleting an agency that owns an alert rule = %d (%s), want 409 naming it", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM agencies WHERE id='ag:OPS'`); n != 1 {
		t.Error("the agency was deleted from under its alert rule")
	}
	exec(`DELETE FROM alert_config WHERE id = 'a-ops'`)
	exec(`INSERT INTO alert_targets (id, owner_agency, url, created_at) VALUES ('t-ops', 'ag:OPS', 'json://ops', 't')`)
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/agencies/ag:OPS", gRoot, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "push target") {
		t.Errorf("deleting an agency that owns a push target = %d (%s), want 409 naming it", rec.Code, rec.Body)
	}
	exec(`INSERT INTO alert_config (id, target_mode, trigger, channels, enabled, created_by, created_at, last_modified_by, last_modified_at, owner_agency)
	      VALUES ('a-ops', 'all', 'failure', '["email"]', 1, 'seed', '2026-01-01T00:00:00Z', 'seed', '2026-01-01T00:00:00Z', 'ag:OPS')`)
	exec(`DELETE FROM alert_targets WHERE id = 't-ops'`)
	// The guard itself (1530), for a delete between the count and the transaction.
	if _, err := pool.Exec(`DELETE FROM agencies WHERE id='ag:OPS'`); err == nil || !strings.Contains(err.Error(), "agency_in_use") {
		t.Errorf("the database deleted an agency that owns an alert rule: %v", err)
	}

	// FIN's administrator deletes its own; TAX's could not.
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/alerts/"+fin.ID, gFinAdmin, ""); rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Errorf("fin admin deleting its own rule = %d (%s)", rec.Code, rec.Body)
	}
}

// AL-2 and AL-3 (2.5.0 C4): a push target is one agency's, changed with
// configureApp on it; its URL is read only by those who may change it
// (question 81); a rule names only its own agency's targets (question 79), and
// an edit that does not send targetIds keeps the rule's.
func TestPushTargetsBelongToAnAgency(t *testing.T) {
	h, pool := gateServer(t)
	type target struct {
		ID          string `json:"id"`
		OwnerAgency string `json:"ownerAgency"`
		URL         string `json:"url"`
		URLSet      bool   `json:"urlSet"`
	}
	create := func(groups, body string) (int, target) {
		t.Helper()
		rec := gateReq(t, h, http.MethodPost, "/api/v1/alert-targets", groups, body)
		var tg target
		_ = json.Unmarshal(rec.Body.Bytes(), &tg)
		return rec.Code, tg
	}
	code, fin := create(gFinAdmin, `{"label":"FIN hook","service":"json","url":"json://FINTOKEN"}`)
	if code != http.StatusCreated || fin.OwnerAgency != "ag:FIN" || fin.URL != "json://FINTOKEN" {
		t.Fatalf("fin admin creating a target = %d %+v, want 201, ag:FIN and its URL", code, fin)
	}
	code, glob := create(gRoot, `{"label":"ops","url":"json://GLOBTOKEN"}`)
	if code != http.StatusCreated || glob.OwnerAgency != "global" {
		t.Fatalf("root creating a target = %d %+v, want 201 and global", code, glob)
	}
	_, tax := create(gTaxAdmin, `{"label":"TAX hook","url":"json://TAXTOKEN"}`)
	if c, _ := create(gFinAdmin, `{"label":"x","url":"json://x","ownerAgency":"global"}`); c != http.StatusForbidden {
		t.Errorf("fin admin creating Global's target = %d, want 403", c)
	}
	if c, _ := create(gFinAdmin, `{"label":"x"}`); c != http.StatusUnprocessableEntity {
		t.Errorf("a target with no URL = %d, want 422", c)
	}

	// Who reads which, and who reads the URL.
	list := func(groups string) map[string]target {
		rec := gateReq(t, h, http.MethodGet, "/api/v1/alert-targets", groups, "")
		var l []target
		if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
			t.Fatalf("GET /alert-targets as %s: %d %s", groups, rec.Code, rec.Body)
		}
		out := map[string]target{}
		for _, tg := range l {
			out[tg.ID] = tg
		}
		return out
	}
	if l := list(gFinViewer); len(l) != 1 || l[fin.ID].URL != "" || !l[fin.ID].URLSet {
		t.Errorf("fin viewer reads %+v; want FIN's target alone, its URL withheld and urlSet", l)
	}
	if l := list(gFinAdmin); len(l) != 1 || l[fin.ID].URL != "json://FINTOKEN" {
		t.Errorf("fin admin reads %+v; want FIN's target with its URL", l)
	}
	if l := list(gAllViewer); len(l) != 3 || l[glob.ID].URL != "" || l[fin.ID].URL != "" {
		t.Errorf("an everywhere-viewer reads %+v; want all three, no URL", l)
	}
	if l := list(gRoot); len(l) != 3 || l[tax.ID].URL != "json://TAXTOKEN" {
		t.Errorf("root reads %+v; want all three with URLs", l)
	}

	// Changing one: 404 unread, 403 without configureApp.
	upd := `{"label":"renamed","url":"json://FINTOKEN2","enabled":false}`
	for _, c := range []struct {
		groups string
		want   int
	}{{gTaxAdmin, http.StatusNotFound}, {gFinViewer, http.StatusForbidden}, {gFinAdmin, http.StatusOK}} {
		if rec := gateReq(t, h, http.MethodPut, "/api/v1/alert-targets/"+fin.ID, c.groups, upd); rec.Code != c.want {
			t.Errorf("%s PUT FIN's target = %d, want %d (%s)", c.groups, rec.Code, c.want, rec.Body)
		}
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM alert_targets WHERE id=? AND owner_agency='ag:FIN' AND label='renamed' AND enabled=0`, fin.ID); n != 1 {
		t.Error("FIN's edit did not land, or moved the target")
	}

	// A rule names its own agency's targets only.
	rule := func(targets string) string {
		return `{"targetMode":"all","trigger":"failure","channels":["apprise"],"enabled":true,"targetIds":` + targets + `}`
	}
	for _, other := range []string{glob.ID, tax.ID, "no-such-target"} {
		rec := gateReq(t, h, http.MethodPost, "/api/v1/alerts", gFinAdmin, rule(`["`+fin.ID+`","`+other+`"]`))
		if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "unknown_target" {
			t.Errorf("FIN's rule naming %s = %d %s, want 422 unknown_target", other, rec.Code, rec.Body)
		}
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/alerts", gRoot, rule(`["`+fin.ID+`"]`)); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a Global rule naming FIN's target = %d, want 422 (%s)", rec.Code, rec.Body)
	}
	rec := gateReq(t, h, http.MethodPost, "/api/v1/alerts", gFinAdmin, rule(`["`+fin.ID+`"]`))
	var made struct {
		ID        string   `json:"id"`
		TargetIDs []string `json:"targetIds"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &made)
	if rec.Code != http.StatusCreated || len(made.TargetIDs) != 1 || made.TargetIDs[0] != fin.ID {
		t.Fatalf("FIN's rule naming its target = %d %s, want 201 naming it", rec.Code, rec.Body)
	}
	// An edit without targetIds keeps them; one with [] names none.
	noTargets := `{"targetMode":"all","trigger":"failure","channels":["apprise"],"enabled":true}`
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/alerts/"+made.ID, gFinAdmin, noTargets); rec.Code != http.StatusOK {
		t.Fatalf("edit = %d (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM alert_rule_targets WHERE rule_id=? AND target_id=?`, made.ID, fin.ID); n != 1 {
		t.Error("an edit that sent no targetIds dropped the rule's target")
	}
	// Deleting a target: TAX cannot; FIN can, and the rule stops naming it.
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/alert-targets/"+fin.ID, gTaxAdmin, ""); rec.Code != http.StatusNotFound {
		t.Errorf("tax admin deleting FIN's target = %d, want 404", rec.Code)
	}
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/alert-targets/"+fin.ID, gFinAdmin, ""); rec.Code != http.StatusNoContent {
		t.Errorf("fin admin deleting its target = %d (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM alert_rule_targets WHERE rule_id=?`, made.ID); n != 0 {
		t.Error("a deleted target is still named by the rule")
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM change_log WHERE category='Alerts' AND agency_id='ag:FIN' AND target LIKE 'push target%'`); n != 3 {
		t.Errorf("FIN's target's audit rows carrying FIN: %d, want create, edit and delete", n)
	}
}

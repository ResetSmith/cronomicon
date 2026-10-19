package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyctx"
)

// 2.5.0 C1 — an audit row is its OBJECT's agency's, never the agency the
// request was viewing (the owner's answer to question 74, decision D9): a
// global administrator who edits FIN's variable while viewing TAX writes FIN's
// rows, in the change log and in Activity, and the change log says "acting in"
// TAX.
func TestC1_AnEditMadeWhileViewingAnotherAgencyIsTheObjectsAgencys(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	if r := gateReqWithHeader(t, h, http.MethodPut, "/api/v1/env-vars/ev-fin", gRoot,
		`{"key":"S0_FIN_VAR","value":"2"}`, agencyctx.Header, "ag:TAX"); r.Code != http.StatusOK {
		t.Fatalf("edit FIN's variable while viewing TAX: %d %s", r.Code, r.Body)
	}
	var logAgency, acting, actAgency *string
	if err := pool.QueryRow(`SELECT agency_id, acting_agency FROM change_log WHERE target = 'S0_FIN_VAR' ORDER BY rowid DESC LIMIT 1`).
		Scan(&logAgency, &acting); err != nil {
		t.Fatalf("its change log row: %v", err)
	}
	if err := pool.QueryRow(`SELECT agency_id FROM activity WHERE kind = 'config' AND summary LIKE '%S0_FIN_VAR%' ORDER BY id DESC LIMIT 1`).
		Scan(&actAgency); err != nil {
		t.Fatalf("its Activity row: %v", err)
	}
	if logAgency == nil || *logAgency != "ag:FIN" || actAgency == nil || *actAgency != "ag:FIN" {
		t.Errorf("the edit's rows are %v's (change log) and %v's (Activity); want ag:FIN's", logAgency, actAgency)
	}
	if acting == nil || *acting != "ag:TAX" {
		t.Errorf("its change log row is acting in %v; want ag:TAX, the agency the administrator was viewing", acting)
	}
}

// The owner's answer to question 75 (D8): connecting an agency's repository
// writes a "connected" row in that agency's log, naming the repository, and
// the settings saved with it are that agency's too, not "Settings / GitLab
// Connection" with no repository (present defect 72).
func TestC1_ConnectingAnAgencysRepositoryIsInItsLog(t *testing.T) {
	h, pool := repoGateServer(t)
	rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/git-repo", gFinAdmin, `{"repoUrl":"http://127.0.0.1:1/fin.git"}`)
	var made struct{ ID string }
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &made) != nil || made.ID == "" {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	syncsSettled(t, pool)
	rows, err := pool.Query(`SELECT action, target, details, COALESCE(agency_id, '') FROM change_log WHERE category = 'Settings' ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var connected, saved bool
	for rows.Next() {
		var action, target, details, agency string
		if err := rows.Scan(&action, &target, &details, &agency); err != nil {
			t.Fatal(err)
		}
		want := "repository " + made.ID
		switch {
		case action == "connected" && target == "Repository connection":
			connected = true
			if agency != "ag:FIN" || details != want {
				t.Errorf("the connection's row is %q's, details %q; want ag:FIN's, %q", agency, details, want)
			}
		case target == "GitLab Connection":
			saved = true
			if agency != "ag:FIN" || details != want {
				t.Errorf("the connection settings' row is %q's, details %q; want ag:FIN's, %q", agency, details, want)
			}
		}
	}
	if !connected || !saved {
		t.Errorf("rows written: connected %v, settings %v; want both", connected, saved)
	}
}

// seedAuditRows writes one change log and one Activity row of FIN's, TAX's and
// Global's (an installation change), and an authentication event.
func seedAuditRows(t *testing.T, exec func(string, ...any)) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, a := range []string{"ag:FIN", "ag:TAX", "global"} {
		tag := map[string]string{"ag:FIN": "c1-fin", "ag:TAX": "c1-tax", "global": "c1-glob"}[a]
		exec(`INSERT INTO change_log (at, actor, category, action, target, details, created_at, agency_id)
		      VALUES (?, ?, 'Settings', 'updated', ?, '', ?, ?)`, now, tag+"@example", tag+"-change", now, a)
		exec(`INSERT INTO activity (kind, actor, outcome, category, summary, at, created_at, agency_id)
		      VALUES ('config', ?, 'success', 'Settings', ?, ?, ?, ?)`, tag+"@example", tag+"-act", now, now, a)
	}
	exec(`INSERT INTO auth_events (at, kind, outcome, actor, reason, target, created_at)
	      VALUES (?, 'login', 'failure', 'c1-auth@example', 'bad_nonce', 'session', ?)`, now, now)
}

// CF-1 (2.5.0 C1) and the owner's answer to question 76: an agency's people
// read its change log and Activity rows and no other's, nor Global's; a global
// administrator and a viewer of every agency read them all. Until C1 the
// change log was every session's in full.
func TestC1_AnAgencyReadsItsOwnAuditRowsOnly(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	seedAuditRows(t, mustExec(t, pool))
	sees := func(path, groups string) string {
		t.Helper()
		r := gateReq(t, h, http.MethodGet, path, groups, "")
		if r.Code != http.StatusOK {
			t.Fatalf("%s as %s: %d %s", path, groups, r.Code, r.Body)
		}
		out := ""
		for _, m := range []string{"c1-fin", "c1-tax", "c1-glob"} {
			if strings.Contains(r.Body.String(), m) {
				out += "Y"
			} else {
				out += "."
			}
		}
		return out
	}
	for _, c := range []struct{ groups, want string }{
		{gRoot, "YYY"}, {gAllViewer, "YYY"}, {gFinAdmin, "Y.."}, {gFinViewer, "Y.."}, {gTaxAdmin, ".Y."},
	} {
		for _, path := range []string{"/api/v1/change-log?pageSize=200", "/api/v1/activity?pageSize=200", "/api/v1/activity/actors"} {
			if got := sees(path, c.groups); got != c.want {
				t.Errorf("%s as %s: FIN/TAX/Global %s, want %s", path, c.groups, got, c.want)
			}
		}
	}
}

// CF-5 (2.5.0 C1): the audit export is an agency administrator's too, filtered
// to the agencies they administer, without authentication events; a global
// administrator's has everything; someone who administers nothing has none.
func TestC1_AnAgencyAdministratorExportsTheirAgencysRows(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	seedAuditRows(t, mustExec(t, pool))
	export := func(groups string) (int, string) {
		t.Helper()
		r := gateReq(t, h, http.MethodGet, "/api/v1/audit/export?format=csv", groups, "")
		return r.Code, r.Body.String()
	}
	code, body := export(gFinAdmin)
	if code != http.StatusOK {
		t.Fatalf("FIN's administrator's export: %d %s", code, body)
	}
	if !strings.Contains(body, "c1-fin-change") || !strings.Contains(body, "c1-fin-act") {
		t.Errorf("FIN's administrator's export lacks FIN's rows:\n%s", body)
	}
	for _, other := range []string{"c1-tax", "c1-glob", "c1-auth"} {
		if strings.Contains(body, other) {
			t.Errorf("FIN's administrator's export holds %s rows", other)
		}
	}
	if !strings.HasPrefix(body, "source,at,actor,category,action,target,details,status,traceId,actingAgency,agency\n") {
		t.Errorf("the export's header is not the 2.5.0 one: %q", strings.SplitN(body, "\n", 2)[0])
	}
	code, body = export(gRoot)
	if code != http.StatusOK {
		t.Fatalf("a global administrator's export: %d", code)
	}
	for _, m := range []string{"c1-fin-change", "c1-tax-change", "c1-glob-change", "c1-auth"} {
		if !strings.Contains(body, m) {
			t.Errorf("a global administrator's export lacks %s", m)
		}
	}
	if code, _ := export(gFinViewer); code != http.StatusForbidden {
		t.Errorf("a viewer's export = %d, want 403 (configureApp)", code)
	}
}

// Present defect 74 (C0): a role template's edit is an installation change,
// but its gate is inside the handler, not globalOnly, so it kept "acting in"
// the agency viewed. It is Global's, acting in nothing.
func TestC1_ARoleTemplateChangeActsInNoAgency(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	if r := gateReqWithHeader(t, h, http.MethodPost, "/api/v1/roles", gRoot,
		`{"name":"c1-role","description":"x","permissions":{"viewRuns":true}}`, agencyctx.Header, "ag:FIN"); r.Code != http.StatusCreated {
		t.Fatalf("create a role template while viewing FIN: %d %s", r.Code, r.Body)
	}
	var agency, acting *string
	if err := pool.QueryRow(`SELECT agency_id, acting_agency FROM change_log WHERE category = 'Roles' AND target = 'c1-role'`).
		Scan(&agency, &acting); err != nil {
		t.Fatalf("its change log row: %v", err)
	}
	if agency == nil || *agency != "global" || acting != nil {
		t.Errorf("the role template's row is %v's, acting in %v; want global's, acting in nothing", agency, acting)
	}
}

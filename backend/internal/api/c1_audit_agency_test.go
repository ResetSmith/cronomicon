package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

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

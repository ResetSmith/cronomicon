package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyctx"
)

// An agency's own syncs, pushes and changes are in its Activity (2.4.4; the
// owner's wish after F3): a row records the agency it is of (1510), and the
// feed narrows by it when the row is not a run's.
func TestSwitcherAnAgencysOwnSyncsAndChangesAreInItsActivity(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	now := time.Now().UTC().Format(time.RFC3339)
	// TAX's repository's sync, as the sync service writes it; and one from
	// before 2.4.4, which names no agency and so is Global's.
	exec(`INSERT INTO activity (kind, actor, outcome, category, action, summary, at, created_at, agency_id) VALUES
	        ('gitsync', 'poll', 'failure', 'Git', 'sync', 's0-tax-sync', ?, ?, 'ag:TAX'),
	        ('gitsync', 'poll', 'success', 'Git', 'sync', 's0-old-sync', ?, ?, NULL)`, now, now, now, now)
	feed := func(agency string) string {
		t.Helper()
		r := gateReqWithHeader(t, h, http.MethodGet, "/api/v1/activity?pageSize=200", gRoot, "", agencyctx.Header, agency)
		if r.Code != http.StatusOK {
			t.Fatalf("activity in %s: %d %s", agency, r.Code, r.Body)
		}
		return r.Body.String()
	}
	if b := feed("ag:TAX"); !strings.Contains(b, "s0-tax-sync") || strings.Contains(b, "s0-old-sync") {
		t.Errorf("TAX's Activity: its sync %v, the old one %v; want its own only", strings.Contains(b, "s0-tax-sync"), strings.Contains(b, "s0-old-sync"))
	}
	if b := feed("global"); strings.Contains(b, "s0-tax-sync") || !strings.Contains(b, "s0-old-sync") {
		t.Errorf("Global's Activity: TAX's sync %v, the old one %v; want the old one only", strings.Contains(b, "s0-tax-sync"), strings.Contains(b, "s0-old-sync"))
	}

	// A change FIN's administrator makes while working in FIN is FIN's.
	if r := gateReqWithHeader(t, h, http.MethodPost, "/api/v1/env-vars", gFinAdmin, `{"key":"S0_ACT_FIN","value":"v","scope":"fin-hosts"}`, agencyctx.Header, "ag:FIN"); r.Code != http.StatusCreated {
		t.Fatalf("create in FIN: %d %s", r.Code, r.Body)
	}
	var agency *string
	if err := pool.QueryRow(`SELECT agency_id FROM activity WHERE kind = 'config' AND summary LIKE '%S0_ACT_FIN%' ORDER BY id DESC LIMIT 1`).Scan(&agency); err != nil {
		t.Fatalf("the change's Activity row: %v", err)
	}
	if agency == nil || *agency != "ag:FIN" {
		t.Errorf("the change made in FIN is recorded as %v's", agency)
	}
	if b := feed("ag:FIN"); !strings.Contains(b, "S0_ACT_FIN") {
		t.Errorf("FIN's Activity does not show the change made in it")
	}
	// Made with no agency named (a script), it is still its object's: FIN's
	// variable is FIN's (2.5.0 C1, question 74; 2.4.4 recorded the request's
	// agency, and so Global's). Its change log row says so too.
	if r := gateReq(t, h, http.MethodPost, "/api/v1/env-vars", gFinAdmin, `{"key":"S0_ACT_NONE","value":"v","scope":"fin-hosts"}`); r.Code != http.StatusCreated {
		t.Fatalf("create with no agency named: %d %s", r.Code, r.Body)
	}
	if err := pool.QueryRow(`SELECT agency_id FROM activity WHERE kind = 'config' AND summary LIKE '%S0_ACT_NONE%' ORDER BY id DESC LIMIT 1`).Scan(&agency); err != nil {
		t.Fatalf("its Activity row: %v", err)
	}
	if agency == nil || *agency != "ag:FIN" {
		t.Errorf("a change to FIN's variable made with no agency named is recorded as %v's, want ag:FIN", agency)
	}
	if err := pool.QueryRow(`SELECT agency_id FROM change_log WHERE target = 'S0_ACT_NONE' ORDER BY rowid DESC LIMIT 1`).Scan(&agency); err != nil {
		t.Fatalf("its change log row: %v", err)
	}
	if agency == nil || *agency != "ag:FIN" {
		t.Errorf("its change log row is recorded as %v's, want ag:FIN", agency)
	}
}

// A change to the installation (Settings → Installation, requireGlobal) is
// Global's, whichever agency the administrator is viewing: its rows are
// Global's (2.5.0 C1: named, where 2.4.4 left them empty), and its change log
// entry names no "acting in".
func TestSwitcherAnInstallationChangeIsGlobalsWhereverItIsMade(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	cur := gateReq(t, h, http.MethodGet, "/api/v1/settings/general", gRoot, "")
	if cur.Code != http.StatusOK {
		t.Fatalf("read the general settings: %d %s", cur.Code, cur.Body)
	}
	var lastLog, lastAct int
	_ = pool.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM change_log`).Scan(&lastLog)
	_ = pool.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM activity`).Scan(&lastAct)
	if r := gateReqWithHeader(t, h, http.MethodPut, "/api/v1/settings/general", gRoot, cur.Body.String(), agencyctx.Header, "ag:FIN"); r.Code != http.StatusOK {
		t.Fatalf("save the general settings while viewing FIN: %d %s", r.Code, r.Body)
	}
	var logs, logsActing, logsGlobal, acts, actsGlobal int
	_ = pool.QueryRow(`SELECT COUNT(*), COUNT(acting_agency), COUNT(CASE WHEN agency_id = 'global' THEN 1 END)
	                     FROM change_log WHERE rowid > ?`, lastLog).Scan(&logs, &logsActing, &logsGlobal)
	_ = pool.QueryRow(`SELECT COUNT(*), COUNT(CASE WHEN agency_id = 'global' THEN 1 END) FROM activity WHERE id > ?`, lastAct).Scan(&acts, &actsGlobal)
	if logs == 0 || acts == 0 {
		t.Fatalf("saving the general settings recorded %d change log and %d Activity rows; want one of each", logs, acts)
	}
	if logsActing != 0 {
		t.Errorf("an installation change made while viewing FIN says it was acting in it: %d of %d change log rows", logsActing, logs)
	}
	if logsGlobal != logs || actsGlobal != acts {
		t.Errorf("an installation change made while viewing FIN is not Global's: %d of %d change log rows, %d of %d Activity rows",
			logsGlobal, logs, actsGlobal, acts)
	}
}

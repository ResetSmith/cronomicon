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
	// Made with no agency named (a script), it names none: Global's, as before.
	if r := gateReq(t, h, http.MethodPost, "/api/v1/env-vars", gFinAdmin, `{"key":"S0_ACT_NONE","value":"v","scope":"fin-hosts"}`); r.Code != http.StatusCreated {
		t.Fatalf("create with no agency named: %d %s", r.Code, r.Body)
	}
	if err := pool.QueryRow(`SELECT agency_id FROM activity WHERE kind = 'config' AND summary LIKE '%S0_ACT_NONE%' ORDER BY id DESC LIMIT 1`).Scan(&agency); err != nil {
		t.Fatalf("its Activity row: %v", err)
	}
	if agency != nil {
		t.Errorf("a change made with no agency named is recorded as %q's", *agency)
	}
}

// A change to the installation (Settings → Installation, requireGlobal) is
// Global's, whichever agency the administrator is viewing: neither its
// Activity row nor its change log entry names the agency.
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
	var logs, logsNamed, acts, actsNamed int
	_ = pool.QueryRow(`SELECT COUNT(*), COUNT(acting_agency) FROM change_log WHERE rowid > ?`, lastLog).Scan(&logs, &logsNamed)
	_ = pool.QueryRow(`SELECT COUNT(*), COUNT(agency_id) FROM activity WHERE id > ?`, lastAct).Scan(&acts, &actsNamed)
	if logs+acts == 0 {
		t.Fatalf("saving the general settings recorded nothing to check")
	}
	if logsNamed != 0 || actsNamed != 0 {
		t.Errorf("an installation change made while viewing FIN names an agency: %d of %d change log rows, %d of %d Activity rows",
			logsNamed, logs, actsNamed, acts)
	}
}

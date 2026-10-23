package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyctx"
)

// The Operations overview and the badges (2.4.4 S5).

type overviewBody struct {
	Agencies []struct {
		ID          string                    `json:"id"`
		Jobs        int                       `json:"jobs"`
		Workflows   int                       `json:"workflows"`
		Runs24h     map[string]int            `json:"runs24h"`
		OpenNotices int                       `json:"openNotices"`
		Repository  *struct{ Status *string } `json:"repository"`
	} `json:"agencies"`
	Attention []struct {
		Kind     string `json:"kind"`
		AgencyID string `json:"agencyId"`
		Name     string `json:"name"`
	} `json:"attention"`
	AttentionTotal int `json:"attentionTotal"`
}

// seedOverview adds what needs attention: FIN's run lost an hour ago, TAX's
// job's last run failed, and TAX's repository's last sync failed.
func seedOverview(t *testing.T, h http.Handler, exec func(string, ...any)) {
	t.Helper()
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	exec(`INSERT INTO runs (id, job_name, job_uid, run_type, status, queued_reason, triggered_by, trigger_kind, created_at, agencies_json) VALUES
	        ('r-fin-lost', 's0-fin-job', 'j-fin', 'bash', 'failure', 'runner_lost', 'x', 'manual', ?, '["FIN"]'),
	        ('r-tax-fail', 's0-tax-job', 'j-tax', 'bash', 'failure', NULL,          'x', 'manual', ?, '["TAX"]')`, recent, recent)
	exec(`UPDATE git_repos SET last_status = 'failed', last_synced_at = ? WHERE id = 'repo-tax'`, recent)
}

func TestSwitcherTheOverviewIsForThoseWhoReachEveryAgency(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	for _, c := range []struct {
		who, groups string
		want        int
	}{
		{"fin-admin", gFinAdmin, http.StatusForbidden},
		{"fin+tax admin", gFinAdmin + "," + gTaxAdmin, http.StatusForbidden},
		{"no grant", "nobody-group", http.StatusForbidden},
		{"all-viewer", gAllViewer, http.StatusOK},
		{"root", gRoot, http.StatusOK},
	} {
		for _, path := range []string{"/api/v1/overview", "/api/v1/overview/search?q=s0"} {
			if rec := gateReq(t, h, http.MethodGet, path, c.groups, ""); rec.Code != c.want {
				t.Errorf("%s as %s = %d, want %d", path, c.who, rec.Code, c.want)
			}
		}
	}
}

func TestSwitcherTheOverviewCountsWhatEachAgencysViewShows(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	seedOverview(t, h, mustExec(t, pool))
	rec := gateReq(t, h, http.MethodGet, "/api/v1/overview", gAllViewer, "")
	var ov overviewBody
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &ov) != nil {
		t.Fatalf("overview: %d %s", rec.Code, rec.Body)
	}
	total := func(path, agency string) int {
		t.Helper()
		r := gateReqWithHeader(t, h, http.MethodGet, path, gAllViewer, "", agencyctx.Header, agency)
		var page struct {
			TotalItems int `json:"totalItems"`
		}
		if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &page) != nil {
			t.Fatalf("%s in %s: %d %s", path, agency, r.Code, r.Body)
		}
		return page.TotalItems
	}
	seen := map[string]bool{}
	for _, a := range ov.Agencies {
		seen[a.ID] = true
		if want := total("/api/v1/jobs?pageSize=200", a.ID); a.Jobs != want {
			t.Errorf("%s: %d jobs on the overview, %d in its own list", a.ID, a.Jobs, want)
		}
		if want := total("/api/v1/workflows?pageSize=200", a.ID); a.Workflows != want {
			t.Errorf("%s: %d workflows on the overview, %d in its own list", a.ID, a.Workflows, want)
		}
	}
	for _, id := range []string{"global", "ag:FIN", "ag:TAX"} {
		if !seen[id] {
			t.Errorf("no row for %s", id)
		}
	}
	for _, a := range ov.Agencies {
		switch a.ID {
		case "ag:FIN":
			if a.Runs24h["lost"] != 1 || a.OpenNotices != 1 {
				t.Errorf("FIN: runs %v, notices %d; want one lost, one notice", a.Runs24h, a.OpenNotices)
			}
		case "ag:TAX":
			if a.Runs24h["failure"] != 1 || a.Repository == nil || a.Repository.Status == nil || *a.Repository.Status != "failed" {
				t.Errorf("TAX: runs %v, repository %+v; want one failure, a failed sync", a.Runs24h, a.Repository)
			}
		}
	}
	got := map[string]string{}
	for _, it := range ov.Attention {
		got[it.Kind+" "+it.Name] = it.AgencyID
	}
	for item, agency := range map[string]string{
		"run_lost s0-fin-job":   "ag:FIN",
		"job_failed s0-tax-job": "ag:TAX",
	} {
		if got[item] != agency {
			t.Errorf("attention %q: agency %q, want %q (all: %v)", item, got[item], agency, got)
		}
	}
	var syncs int
	for _, it := range ov.Attention {
		if it.Kind == "sync_failed" {
			syncs++
			if it.AgencyID != "ag:TAX" {
				t.Errorf("a failed sync attributed to %s", it.AgencyID)
			}
		}
	}
	if syncs != 1 || ov.AttentionTotal != len(ov.Attention) {
		t.Errorf("failed syncs %d (want 1); total %d for %d listed", syncs, ov.AttentionTotal, len(ov.Attention))
	}
}

func TestSwitcherTheOverviewSearchSaysWhoseEachMatchIs(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	rec := gateReq(t, h, http.MethodGet, "/api/v1/overview/search?q=s0-", gRoot, "")
	var hits []struct{ Kind, Name, AgencyID string }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &hits) != nil {
		t.Fatalf("search: %d %s", rec.Code, rec.Body)
	}
	got := map[string]string{}
	for _, h := range hits {
		got[h.Kind+" "+h.Name] = h.AgencyID
	}
	for k, want := range map[string]string{
		"job s0-fin-job": "ag:FIN", "job s0-tax-job": "ag:TAX", "job s0-glob-job": "global",
		"workflow s0-fin-flow": "ag:FIN", "workflow s0-tax-flow": "ag:TAX",
	} {
		if got[k] != want {
			t.Errorf("%s: %q, want %q (all: %v)", k, got[k], want, got)
		}
	}
	if rec := gateReq(t, h, http.MethodGet, "/api/v1/overview/search?q=", gRoot, ""); rec.Code != http.StatusOK || rec.Body.String() != "[]\n" && rec.Body.String() != "[]" {
		t.Errorf("an empty search = %d %q, want []", rec.Code, rec.Body)
	}
}

func TestSwitcherBadgesCountWhatEachAgencyHasWaiting(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	seedOverview(t, h, mustExec(t, pool))
	badges := func(groups string) map[string][2]int {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, "/api/v1/me/agency-badges", groups, "")
		var list []struct {
			AgencyID      string `json:"agencyId"`
			OpenNotices   int    `json:"openNotices"`
			FailedRuns24h int    `json:"failedRuns24h"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
			t.Fatalf("badges as %s: %d %s", groups, rec.Code, rec.Body)
		}
		out := map[string][2]int{}
		for _, b := range list {
			out[b.AgencyID] = [2]int{b.OpenNotices, b.FailedRuns24h}
		}
		return out
	}
	// Two agencies' administrator: each agency's notice and failure; Global is
	// not one of their choices (2.5.0 C2, question 77).
	if got := badges(gFinAdmin + "," + gTaxAdmin); len(got) != 2 || got["ag:FIN"] != [2]int{1, 1} || got["ag:TAX"] != [2]int{1, 1} {
		t.Errorf("fin+tax admin's badges: %v", got)
	}
	// One agency's: its own.
	if got := badges(gFinAdmin); len(got) != 1 || got["ag:FIN"] != [2]int{1, 1} {
		t.Errorf("fin-admin's badges: %v", got)
	}
	// A viewer of everything: every agency's failures, no notices to act on.
	if got := badges(gAllViewer); len(got) != 3 || got["ag:FIN"] != [2]int{0, 1} || got["ag:TAX"] != [2]int{0, 1} {
		t.Errorf("all-viewer's badges: %v", got)
	}
}

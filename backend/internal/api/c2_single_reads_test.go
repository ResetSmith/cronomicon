package api_test

import (
	"net/http"
	"testing"
)

// 2.5.0 C2 (CF-1; question 72): reading ONE object of an agency the caller does
// not read answers 404, as for an object that is not there. Each passed before
// C2 (C0's §6): FIN's job with no scope, script body and env and all; its run,
// its log and its references; a workflow run; a scope, hosts and all; an SSH
// key, Vault path and all; a calendar, every day; a schedule, env and all.
// Those that checked a scope answered 403, which confirmed the object.
func TestC2_OneObjectOfAnotherAgencyReadsAsAbsent(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	finJob := rowID(t, pool, `SELECT rowid FROM jobs WHERE uid = 'j-fin0'`)
	reads := []struct {
		path string
		fin  bool // FIN's own administrator reads it (200)
	}{
		{"/api/v1/jobs/" + finJob, true},
		{"/api/v1/job-reference-bindings/" + finJob, true},
		{"/api/v1/runs/r-fin0", true},
		{"/api/v1/runs/r-fin0/references", true},
		{"/api/v1/runs/r-fin0/log", false}, // the fixture writes no log
		{"/api/v1/workflow-runs/wr-fin", true},
		{"/api/v1/scopes/sc:fin", true},
		{"/api/v1/ssh/credentials/k-fin", true},
		{"/api/v1/ssh/credentials/k-fin/usage", true},
		{"/api/v1/calendars/s0-fin-cal?agencyId=ag:FIN", true},
		{"/api/v1/schedule-defs/s0-fin-sched?source=cronomicon&agencyId=ag:FIN", true},
		{"/api/v1/agencies/ag:FIN", true},
	}
	for _, rd := range reads {
		if code := gateReq(t, h, http.MethodGet, rd.path, gTaxAdmin, "").Code; code != http.StatusNotFound {
			t.Errorf("TAX's administrator reading %s = %d, want 404", rd.path, code)
		}
		if rd.fin {
			if code := gateReq(t, h, http.MethodGet, rd.path, gFinAdmin, "").Code; code != http.StatusOK {
				t.Errorf("FIN's administrator reading %s = %d, want 200", rd.path, code)
			}
		}
	}
	// Global's: those who reach every agency, and as absent to an agency's people
	// (question 77).
	globJob := rowID(t, pool, `SELECT rowid FROM jobs WHERE uid = 'j-glob'`)
	for _, path := range []string{"/api/v1/jobs/" + globJob, "/api/v1/runs/r-glob", "/api/v1/workflow-runs/wr-glob"} {
		if code := gateReq(t, h, http.MethodGet, path, gFinAdmin, "").Code; code != http.StatusNotFound {
			t.Errorf("FIN's administrator reading Global's %s = %d, want 404", path, code)
		}
		if code := gateReq(t, h, http.MethodGet, path, gAllViewer, "").Code; code != http.StatusOK {
			t.Errorf("a viewer of every agency reading Global's %s = %d, want 200", path, code)
		}
	}
}

package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// LR Phase 0 — today's behaviour at the route level, pinned before the phase
// named in each test changes it. The package-level pins are
// sshexec/lr_phase0_test.go (the SSH pool's claim, host records) and
// runner/lr_phase0_test.go (agency rename). The session pins were inverted by
// Phase S and live on as grants_live_test.go and auth/snapshot_test.go.

// lr0SharedRunner seeds one runner that belongs to both FIN and TAX.
func lr0SharedRunner(t *testing.T, exec func(string, ...any)) {
	t.Helper()
	exec(`INSERT INTO runners (id,name,status,registered_at,created_at)
	      VALUES ('r-both','runner-both','online','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('r-both','ag:FIN'), ('r-both','ag:TAX')`)
}

// LR-34 (i), the any-member rule — INVERTED by Phase G3 (LR-58, LR-59). Until
// 2.3.0 a runner that belonged to two agencies was administered by an
// administrator of EITHER: FIN's could change a machine TAX depended on, here
// by turning on secret injection for it. A runner has one owner now; one that
// serves two agencies is Global's, and only a global administrator changes it.
func TestLR0_AnAdminOfOneMemberAgencyNoLongerAdministersASharedRunner(t *testing.T) {
	h, pool := gateServer(t)
	lr0SharedRunner(t, mustExec(t, pool))

	rec := gateReq(t, h, http.MethodPut, "/api/v1/runners/r-both/secret-injection", gFinAdmin, `{"allow":true}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("FIN's admin changing a runner shared with TAX = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM runners WHERE id='r-both' AND allow_secret_injection`); n != 0 {
		t.Error("a refused change was applied: secret injection is on")
	}
	rec = gateReq(t, h, http.MethodPut, "/api/v1/runners/r-both/secret-injection", gRoot, `{"allow":true}`)
	if rec.Code/100 != 2 {
		t.Fatalf("a global administrator changing a runner Global owns = %d, want 2xx (%s)", rec.Code, rec.Body)
	}
}

// LR-34 (ii), the unchecked removals — INVERTED by Phase G3 (MA-11). Until
// 2.3.0 PUT /runner-agencies checked the caller's authority over every agency
// being ADDED and none being REMOVED: FIN's administrator could take a shared
// runner away from TAX, and could empty the list altogether. The serve list is
// the runner's owner's to write now, so every one of those writes is refused
// for an agency administrator, and the list is as it was.
func TestLR0_TheRunnerMembershipSetterIsTheOwners(t *testing.T) {
	h, pool := gateServer(t)
	lr0SharedRunner(t, mustExec(t, pool))

	for name, body := range map[string]string{
		"re-posting the list":  `[{"runnerId":"r-both","agencyIds":["ag:FIN","ag:TAX"]}]`,
		"removing it from TAX": `[{"runnerId":"r-both","agencyIds":["ag:FIN"]}]`,
		"emptying the list":    `[{"runnerId":"r-both","agencyIds":[]}]`,
		"moving it to Global":  `[{"runnerId":"r-both","agencyIds":["global"]}]`,
	} {
		rec := gateReq(t, h, http.MethodPut, "/api/v1/runner-agencies", gFinAdmin, body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("FIN's admin %s on a runner Global owns = %d, want 403 (%s)", name, rec.Code, rec.Body)
		}
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM runner_agencies WHERE runner_id='r-both' AND agency_id IN ('ag:FIN','ag:TAX')`); n != 2 {
		t.Errorf("after the refused writes the runner serves %d of its two agencies, want both", n)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM runner_agencies WHERE runner_id='r-both' AND agency_id='global'`); n != 0 {
		t.Errorf("a refused write put the runner in Global")
	}
	// An empty list is refused for the owner too (LR-26): a runner always
	// serves an agency.
	rec := gateReq(t, h, http.MethodPut, "/api/v1/runner-agencies", gRoot, `[{"runnerId":"r-both","agencyIds":[]}]`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "agency_required") {
		t.Errorf("a global administrator emptying a runner's agencies = %d, want 422 agency_required (%s)", rec.Code, rec.Body)
	}
}

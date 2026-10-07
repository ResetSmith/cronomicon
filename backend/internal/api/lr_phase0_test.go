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

// LR-34 (i), the any-member rule. A runner that belongs to two agencies is
// administered by an administrator of EITHER: the gate asks for configureApp on
// any one agency the runner is in. FIN's administrator can therefore change a
// machine TAX depends on, here by turning on secret injection for it.
//
// Phase G3 inverts it (LR-58, LR-59): a runner has one owner, a runner serving
// two agencies is owned by Global, and only a global administrator changes it.
func TestLR0_AnAdminOfOneMemberAgencyAdministersASharedRunner(t *testing.T) {
	h, pool := gateServer(t)
	lr0SharedRunner(t, mustExec(t, pool))

	rec := gateReq(t, h, http.MethodPut, "/api/v1/runners/r-both/secret-injection", gFinAdmin, `{"allow":true}`)
	if rec.Code/100 != 2 {
		t.Fatalf("PIN: FIN's admin changing a runner shared with TAX = %d, want 2xx (%s). "+
			"If runners now have an owner (LR-59), replace this pin with a 403.", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM runners WHERE id='r-both' AND allow_secret_injection`); n != 1 {
		t.Error("the change answered 2xx and was not applied: secret injection is still off")
	}
}

// LR-34 (ii), the unchecked removals. PUT /runner-agencies checks the caller's
// authority over every agency being ADDED and none being REMOVED: the writer
// deletes the runner's rows and inserts the posted set. FIN's administrator can
// take a shared runner away from TAX, and can empty the list altogether, which
// drops the runner into the general pool where it serves unowned work and FIN's
// administrator can no longer reach it.
//
// Phase G1 refused the empty set (LR-26) and an agency administrator placing a
// runner in Global; those two are asserted below as they now stand. Phase G3
// makes the rest of the serve list a global administrator's (MA-11).
func TestLR0_TheRunnerMembershipSetterDoesNotCheckRemovals(t *testing.T) {
	h, pool := gateServer(t)
	lr0SharedRunner(t, mustExec(t, pool))

	// The checked half, for contrast: adding an agency the caller does not hold.
	rec := gateReq(t, h, http.MethodPut, "/api/v1/runner-agencies", gFinAdmin,
		`[{"runnerId":"r-both","agencyIds":["ag:FIN","ag:TAX"]}]`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("naming TAX in the posted set = %d, want 403 (%s)", rec.Code, rec.Body)
	}

	rec = gateReq(t, h, http.MethodPut, "/api/v1/runner-agencies", gFinAdmin,
		`[{"runnerId":"r-both","agencyIds":["ag:FIN"]}]`)
	if rec.Code/100 != 2 {
		t.Fatalf("PIN: FIN's admin removing a runner from TAX = %d, want 2xx (%s). "+
			"If the serve list is now a global administrator's (LR-60), replace this pin with a 403.", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM runner_agencies WHERE runner_id='r-both' AND agency_id='ag:TAX'`); n != 0 {
		t.Errorf("TAX membership rows = %d, want 0 (removed without authority over TAX)", n)
	}

	// Emptying the list was how a runner dropped into the general pool, where it
	// served unowned work and FIN's administrator could no longer reach it. An
	// empty list is refused since G1 (LR-26), and so is the same move said
	// outright: an agency administrator cannot place a runner in Global.
	rec = gateReq(t, h, http.MethodPut, "/api/v1/runner-agencies", gFinAdmin,
		`[{"runnerId":"r-both","agencyIds":[]}]`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "agency_required") {
		t.Errorf("FIN's admin emptying a runner's agencies = %d, want 422 agency_required (%s)", rec.Code, rec.Body)
	}
	rec = gateReq(t, h, http.MethodPut, "/api/v1/runner-agencies", gFinAdmin,
		`[{"runnerId":"r-both","agencyIds":["global"]}]`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("FIN's admin moving a runner to Global = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM runner_agencies WHERE runner_id='r-both' AND agency_id='ag:FIN'`); n != 1 {
		t.Errorf("after two refused writes the runner's FIN row count = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM runner_agencies WHERE runner_id='r-both' AND agency_id='global'`); n != 0 {
		t.Errorf("a refused write put the runner in Global")
	}
}

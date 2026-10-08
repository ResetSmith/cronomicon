package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// The claim rule is written three times (execspec/claimrule.go says why): the
// claim query, execspec.EligibleOnlineRunnerForRun and
// execspec.UnclaimableReason. With exactly one runner online, the three must
// give one answer — the run is claimed if and only if the first mirror says an
// eligible runner exists and the second has no reason to give.
//
// This walks every combination of the things the rule reads, so a rule added
// to one copy and not the others fails here rather than in the field, as "this
// run is waiting" with a reason that is not the reason. It was written when
// the copies had already drifted four ways: the mirrors ignored the capability
// mask, keyed the injection gate on a job's name where the claim uses its uid,
// ignored a run's own SSH credential, and did not know the injection kill
// switch existed.
func TestClaimRuleMirrorsAgreeWithTheClaim(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	t.Cleanup(func() { execspec.SetInjectionGateArmed(true) })

	insertAgencyRow(t, svc, "a1", "alpha")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// Three scopes: unrestricted, bound to the runner under test, bound to
	// somebody else. And two same-named jobs, only one of which has bindings.
	for _, s := range []string{"open", "mine", "theirs"} {
		exec(`INSERT INTO scopes (id, name, source, created_at) VALUES (?, ?, 'cronomicon', ?)`, "sc-"+s, s, now())
	}
	exec(`INSERT INTO scope_runners(scope_id, runner_id, runner_name, bound_by, bound_at) VALUES('sc-mine', 'r1', 'r1', 'test', ?)`, now())
	exec(`INSERT INTO scope_runners(scope_id, runner_id, runner_name, bound_by, bound_at) VALUES('sc-theirs', 'r-else', 'r-else', 'test', ?)`, now())
	exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, owner_uid, ref_kind, ref_name, created_at)
	      VALUES('job', 'cronomicon', 'twin', 'uid-bound', 'secret', 'TOKEN', ?)`, now())
	exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at)
	      VALUES('script', 'git', 'scripts/bound.sh', 'secret', 'TOKEN', ?)`, now())
	// An SSH key binding: an injection like any other for an agent, and the one
	// thing the local runner does not take (LR-47).
	exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, owner_uid, ref_kind, ref_name, created_at)
	      VALUES('job', 'cronomicon', 'twin', 'uid-key', 'key', 'deploy_key', ?)`, now())

	type runnerShape struct {
		caps, mask []string
	}
	runners := []runnerShape{
		{[]string{"bash"}, nil},
		{[]string{"bash", "ansible", "vault"}, nil},
		{[]string{"bash", "ansible", "vault"}, []string{"bash"}},
		{[]string{"bash", "ansible", "vault"}, []string{"ansible"}},
	}
	type injection struct {
		name                    string
		jobUID, script, sshCred string
	}
	injections := []injection{
		{name: "nothing injected", jobUID: "uid-plain"},
		{name: "the job declares a binding", jobUID: "uid-bound"},
		// Same name and source as the bound job, another identity (R2F-1).
		{name: "a same-named sibling declares one", jobUID: "uid-sibling"},
		{name: "the script declares one", jobUID: "uid-plain", script: "scripts/bound.sh"},
		{name: "a per-run SSH credential", jobUID: "uid-plain", sshCred: "deploy-key"},
		{name: "the job binds an SSH key", jobUID: "uid-key"},
	}
	// The runner under test is an agent, or the local runner (kind 'server'),
	// which claims with one more clause and is always allowed injection.
	kinds := []string{"agent", "server"}

	checked, claimedN := 0, 0
	for _, armed := range []bool{true, false} {
		svc.cfg.SecretsInjectionEnabled = armed
		execspec.SetInjectionGateArmed(armed)
		for _, rs := range runners {
			for _, runnerAgency := range []string{"global", "a1"} {
				for _, allowInjection := range []bool{false, true} {
					for _, kind := range kinds {
						if kind == "server" && !allowInjection {
							continue // LR-46: secret injection is fixed on for it
						}
						// One runner, rebuilt per shape: nothing else is ever online.
						exec(`DELETE FROM runners`)
						insertRunner(t, svc, "r1", "r1", "online", rs.caps)
						exec(`UPDATE runners SET kind = ? WHERE id = 'r1'`, kind)
						if runnerAgency != "global" {
							addRunnerAgency(t, svc, "r1", runnerAgency)
						}
						if rs.mask != nil {
							m, _ := json.Marshal(map[string]any{"capabilityMask": rs.mask})
							setManagedSettings(t, svc, "r1", string(m))
						}
						if allowInjection {
							exec(`UPDATE runners SET allow_secret_injection = 1 WHERE id = 'r1'`)
						}
						for _, runType := range []string{"bash", "ansible"} {
							for _, requires := range []string{`[]`, `["vault"]`} {
								for _, runAgency := range []string{"Global", "alpha"} {
									for _, scope := range []string{"open", "mine", "theirs"} {
										for _, inj := range injections {
											exec(`DELETE FROM runs`)
											exec(`INSERT INTO runs(id, job_name, job_source, job_uid, script_ref, ssh_credential, run_type, scope,
										                       requires_json, agencies_json, status, triggered_by, trigger_kind, executor, created_at)
										      VALUES ('run', 'twin', 'cronomicon', ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, 'queued', 'test', 'manual', 'runner', ?)`,
												inj.jobUID, inj.script, inj.sshCred, runType, scope, requires, `["`+runAgency+`"]`, now())
											exec(`INSERT OR IGNORE INTO run_agencies(run_id, agency) VALUES('run', ?)`, runAgency)

											// The mirrors first: the claim moves the run to running.
											eligible, _, err := execspec.EligibleOnlineRunnerForRun(ctx, svc.db, "run")
											if err != nil {
												t.Fatalf("EligibleOnlineRunnerForRun: %v", err)
											}
											reason, err := execspec.UnclaimableReason(ctx, svc.db, "run")
											if err != nil {
												t.Fatalf("UnclaimableReason: %v", err)
											}
											// The claim, as HandlePoll makes it: the mask is
											// subtracted before the query sees the capabilities.
											got, err := Claim(ctx, svc.db, svc.log, ClaimRequest{
												RunnerID:    "r1",
												Caps:        execspec.EffectiveCaps(rs.caps, rs.mask),
												InjectionOK: !armed || allowInjection,
												Local:       kind == "server",
												Actor:       "test",
											})
											if err != nil {
												t.Fatalf("Claim: %v", err)
											}
											claimed := got != nil
											checked++
											if claimed {
												claimedN++
											}
											if eligible != claimed || (reason == "") != claimed {
												t.Errorf("%s\n  claimed=%v  EligibleOnlineRunnerForRun=%v  UnclaimableReason=%q",
													fmt.Sprintf("switch=%v runner{kind=%s caps=%v mask=%v agency=%s injection=%v} run{type=%s requires=%s agency=%s scope=%s, %s}",
														armed, kind, rs.caps, rs.mask, runnerAgency, allowInjection, runType, requires, runAgency, scope, inj.name),
													claimed, eligible, reason)
												if t.Failed() && checked > 4000 {
													t.FailNow()
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	// Both answers must be well represented, or agreement proves nothing.
	if claimedN < checked/20 || claimedN > checked*19/20 {
		t.Fatalf("%d of %d combinations were claimed: the matrix does not exercise both answers", claimedN, checked)
	}
	t.Logf("%d combinations, %d claimed, three copies agree", checked, claimedN)
}

// EffectiveCaps is the mask rule in Go, for the claim; the mirrors apply the
// same rule in SQL. One answer for both, including a settings blob that is
// missing, malformed or carries no mask.
func TestTheMaskSubtractsTheSameInGoAndInSQL(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	declared := []string{"bash", "python", "ansible", "vault"}
	for _, c := range []struct {
		name, managed string
		mask          []string
		online        bool
	}{
		{"no settings at all", "", nil, true},
		{"settings without a mask", `{"maxConcurrent":3}`, nil, true},
		{"an empty mask", `{"capabilityMask":[]}`, []string{}, true},
		{"a mask that lists another type", `{"capabilityMask":["python"]}`, []string{"python"}, true},
		{"a mask that lists the type", `{"capabilityMask":["bash","python"]}`, []string{"bash", "python"}, false},
		{"a blob that is not JSON", `not json`, nil, true},
		{"a mask that is not a list", `{"capabilityMask":"bash"}`, nil, true},
	} {
		if _, err := svc.db.Exec(`DELETE FROM runners`); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.db.Exec(`DELETE FROM runs`); err != nil {
			t.Fatal(err)
		}
		insertRunner(t, svc, "r1", "r1", "online", declared)
		if c.managed != "" {
			setManagedSettings(t, svc, "r1", c.managed)
		}
		insertQueuedRun(t, svc, "run", "j", "bash", "")
		wantGo := false
		for _, tok := range execspec.EffectiveCaps(declared, c.mask) {
			if tok == "bash" {
				wantGo = true
			}
		}
		gotSQL, _, err := execspec.EligibleOnlineRunnerForRun(ctx, svc.db, "run")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if wantGo != c.online || gotSQL != c.online {
			t.Errorf("%s: Go says bash is offered = %v, SQL says = %v, want both %v", c.name, wantGo, gotSQL, c.online)
		}
	}
}

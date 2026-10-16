package api_test

import (
	"testing"
)

// The run-detail path builds its masking dictionary from the bindings of the
// run's job AND of its script, and since migration 1290 it finds the script by
// the uid the run froze (runs.script_uid), not by the name. The fail-closed
// behaviour is the probe: a binding that cannot be resolved masks every output,
// so the outputs are masked exactly when the path found the script's binding.
// Three runs name the same script: one froze the script that declares the
// binding, one froze another script of that name, one froze none.
func TestRunDetailMaskingFollowsTheRunsOwnScript(t *testing.T) {
	ts, pool := injectionServer(t)
	client := devLoginClient(t, ts)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	const at = "2026-01-01T00:00:00Z"
	exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, synced_at, script_ref)
	      VALUES('j1','cronomicon','bash','echo hi','Allow',?, 'lib.sh')`, at)
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, command, content_hash, synced_at) VALUES('uid-lib','global','lib.sh','bash','x','h',?)`, at)
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, command, content_hash, synced_at) VALUES('uid-lib-other','repo-b','lib.sh','bash','x','h',?)`, at)
	// The SCRIPT binds a secret that does not exist: unresolvable, so fail closed.
	exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, owner_uid, ref_kind, ref_name, created_at)
	      VALUES('script','','lib.sh','uid-lib','secret','GONE',?)`, at)
	run := func(id string, scriptUID any) {
		t.Helper()
		exec(`INSERT INTO runs(id, job_name, job_source, run_type, status, scope, triggered_by, trigger_kind, created_at, outputs_json, script_ref, script_uid)
		      VALUES(?, 'j1', 'cronomicon', 'bash', 'success', 'prod', 't@x', 'manual', ?, '{"CLEAN":"pg-prod-01"}', 'lib.sh', ?)`, id, at, scriptUID)
	}
	run("run-script-own", "uid-lib")
	run("run-script-other", "uid-lib-other")
	run("run-script-none", nil)

	if out := fetchRunOutputs(t, client, ts, "run-script-own"); out["CLEAN"] != "[REDACTED]" {
		t.Errorf("a run whose own script declares an unresolvable binding: CLEAN=%q, want every output masked", out["CLEAN"])
	}
	for _, id := range []string{"run-script-other", "run-script-none"} {
		if out := fetchRunOutputs(t, client, ts, id); out["CLEAN"] != "pg-prod-01" {
			t.Errorf("%s: CLEAN=%q, want it shown: the binding belongs to another script of the same name", id, out["CLEAN"])
		}
	}
}

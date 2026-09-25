package api_test

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/auth"
)

// RF-6 — the workflow verb corrections (the RBAC-fixes plan).
//
// v0.56.4 wired RB-2's verbs into the execution surface but got two things wrong
// on the workflow side: patchWorkflow (pause/resume) checked triggerJobs where its
// job-level siblings check killJobs, and the workflow-run CANCEL route — run
// suppression, the same act as killJob — got no verb at all.
//
// Both are invisible with the built-in roles, which hold both verbs or neither.
// They surface the moment a custom role holds one and not the other, which is
// exactly what roles-as-data (RB-6..11) made possible — so these tests use custom
// single-verb roles deliberately.

// seedVerbRole creates a custom role holding exactly one execution verb, mapped to
// its own AD group with an unrestricted grant. The grant is "*" because this test
// is about the VERB axis; the scope axis has its own coverage.
func seedVerbRole(t *testing.T, pool *sql.DB, exec func(string, ...any), role, group string, trigger, kill int) {
	t.Helper()
	exec(`INSERT INTO roles (name, description, builtin, rank, trigger_jobs, kill_jobs,
	                         manage_env_vars, publish_schedule, configure_app, manage_roles)
	      VALUES (?,?,0,2,?,?,0,0,0,0)`, role, "RF-6 fixture", trigger, kill)
	exec(`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
	      VALUES (?,?,?,NULL,1,'2026-01-01T00:00:00Z')`, "g-"+role, group, role)
	// The role registry is a process cache bound at api.New (RB-7), so a role
	// inserted after the server exists is invisible until it is reloaded — the same
	// refresh the roles CRUD performs on every write.
	if err := auth.RefreshRoles(context.Background(), pool); err != nil {
		t.Fatalf("RefreshRoles: %v", err)
	}
}

// workflowPath addresses a workflow by rowid, which is how the routes identify
// them (fetchWorkflowByID selects WHERE rowid = ?).
func workflowPath(t *testing.T, pool *sql.DB, name string) string {
	t.Helper()
	var rowid int64
	if err := pool.QueryRow(`SELECT rowid FROM workflows WHERE name = ?`, name).Scan(&rowid); err != nil {
		t.Fatalf("workflow rowid for %q: %v", name, err)
	}
	return "/api/v1/workflows/" + strconv.FormatInt(rowid, 10)
}

// TestWorkflowPauseTakesKillJobs — RF-Q1. Pausing is run suppression, so it takes
// killJobs like pauseJob/resumeJob. Before RF-6 this route asked for triggerJobs,
// which let a trigger-only role pause a workflow it could not pause job-by-job.
func TestWorkflowPauseTakesKillJobs(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seedVerbRole(t, pool, exec, "trigger-only", "wf-triggerers", 1, 0)
	seedVerbRole(t, pool, exec, "kill-only", "wf-killers", 0, 1)
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled)
	      VALUES ('wf-step','cronomicon','bash','prod',1)`)
	exec(`INSERT INTO workflows (name, source, steps, enabled, synced_at)
	      VALUES ('wf-pause','cronomicon','[{"type":"job","name":"wf-step"}]',1,'2026-01-01T00:00:00Z')`)

	path := workflowPath(t, pool, "wf-pause")
	body := `{"disabled":true}`
	if rec := reqAs(t, h, http.MethodPatch, path, "wf-triggerers", body); rec.Code != http.StatusForbidden {
		t.Errorf("trigger-only role pausing a workflow = %d, want 403 — pausing is run "+
			"suppression and takes killJobs (RF-Q1) (%s)", rec.Code, rec.Body.String())
	}
	if rec := reqAs(t, h, http.MethodPatch, path, "wf-killers", body); rec.Code == http.StatusForbidden {
		t.Errorf("kill-only role pausing a workflow = 403, want the verb check to pass (%s)", rec.Body.String())
	}
}

// TestUnscopedWorkflowIsNotAFreePass — RF-6, and the sharpest thing this sweep
// found: a workflow made only of UNSCOPED jobs was authorized by nothing at all.
//
// JobScopes filtered empty scopes out of its result, so the guard's "an unscoped
// constituent is unrestricted-only" branch (the RB-26 rule) was unreachable, and
// a workflow whose jobs were all unscoped produced an EMPTY list — the per-scope
// loop never ran and the route returned true. Any session could trigger it.
func TestUnscopedWorkflowIsNotAFreePass(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"operator": "prod"})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// Every constituent job is unscoped — the shape that produced an empty list.
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled)
	      VALUES ('free-step','cronomicon','bash',NULL,1)`)
	exec(`INSERT INTO workflows (name, source, steps, enabled, synced_at)
	      VALUES ('wf-free','cronomicon','[{"type":"job","name":"free-step"}]',1,'2026-01-01T00:00:00Z')`)

	path := workflowPath(t, pool, "wf-free")
	// A viewer holds no execution verb anywhere: denied by the baseline.
	if rec := reqAs(t, h, http.MethodPost, path+"/trigger", "sec-viewers", ""); rec.Code != http.StatusForbidden {
		t.Errorf("viewer triggering an all-unscoped workflow = %d, want 403", rec.Code)
	}
	// A RESTRICTED operator holds triggerJobs, but only on their agency — and an
	// unscoped constituent job carries no authority of its own (RB-26), so the
	// workflow is unrestricted-only. This is the case that used to sail through.
	if rec := reqAs(t, h, http.MethodPost, path+"/trigger", "sec-operators", ""); rec.Code != http.StatusForbidden {
		t.Errorf("restricted operator triggering an all-unscoped workflow = %d, want 403 — "+
			"a workflow must not be a way to run an unscoped job without binding one (RB-26)", rec.Code)
	}
	// An unrestricted admin still may, which is what keeps this a boundary.
	if rec := reqAs(t, h, http.MethodPost, path+"/trigger", "sec-admins", ""); rec.Code == http.StatusForbidden {
		t.Errorf("unrestricted admin triggering an all-unscoped workflow = 403, want it allowed (%s)",
			rec.Body.String())
	}
}

// TestWorkflowRunCancelTakesKillJobs — RF-6. The cancel route carried only a scope
// guard; RB-2 wired every other execution route and missed this one, so a viewer
// with scope access could still suppress a running workflow.
func TestWorkflowRunCancelTakesKillJobs(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seedVerbRole(t, pool, exec, "trigger-only", "wf-triggerers", 1, 0)
	seedVerbRole(t, pool, exec, "kill-only", "wf-killers", 0, 1)
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled)
	      VALUES ('wf-step','cronomicon','bash','prod',1)`)
	exec(`INSERT INTO workflow_runs (id, workflow_id, workflow_name, status, triggered_by,
	                                 trigger_kind, created_at, steps_snapshot)
	      VALUES ('wfr-1',1,'wf-cancel','running','t','manual','2026-01-01T00:00:00Z',
	              '[{"type":"job","name":"wf-step"}]')`)

	const path = "/api/v1/workflows/runs/wfr-1/cancel"
	if rec := reqAs(t, h, http.MethodPost, path, "sec-viewers", ""); rec.Code != http.StatusForbidden {
		t.Errorf("viewer cancelling a workflow run = %d, want 403 (RF-6)", rec.Code)
	}
	if rec := reqAs(t, h, http.MethodPost, path, "wf-triggerers", ""); rec.Code != http.StatusForbidden {
		t.Errorf("trigger-only role cancelling a workflow run = %d, want 403 — cancel is "+
			"suppression, not triggering", rec.Code)
	}
	// A killJobs holder gets past the verb gate; whatever the engine then answers
	// (202 accepted, or 409 if the run is not actually cancellable in this fixture)
	// is not this test's business.
	if rec := reqAs(t, h, http.MethodPost, path, "wf-killers", ""); rec.Code == http.StatusForbidden {
		t.Errorf("kill-only role cancelling a workflow run = 403, want the verb check to pass (%s)",
			rec.Body.String())
	}
}

// TestWorkflowRunCancelChecksEachChildScope — the per-CHILD half of RF-6. The
// snapshot path and the child-run path are separate loops, and a run whose steps
// snapshot is empty (or whose jobs have since been renamed) is authorized purely
// from its child runs' frozen scopes. A departmental killJobs holder must be
// stopped by a child in someone else's scope.
func TestWorkflowRunCancelChecksEachChildScope(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"operator": "prod"})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT OR IGNORE INTO scopes (id,name,source,created_at)
	      VALUES ('s-fin','finance','cronomicon','2026-01-01T00:00:00Z')`)
	// No steps snapshot: authorization rests entirely on the child runs.
	exec(`INSERT INTO workflow_runs (id, workflow_id, workflow_name, status, triggered_by,
	                                 trigger_kind, created_at, steps_snapshot)
	      VALUES ('wfr-c',1,'wf-children','running','t','manual','2026-01-01T00:00:00Z','')`)
	exec(`INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind,
	                        executor, scope, workflow_run_id, created_at)
	      VALUES ('c-prod','j1','bash','running','t','workflow','ssh','prod','wfr-c','2026-01-01T00:00:00Z')`)

	const path = "/api/v1/workflows/runs/wfr-c/cancel"
	// All children are in the operator's own department: the guard passes.
	if rec := reqAs(t, h, http.MethodPost, path, "sec-operators", ""); rec.Code == http.StatusForbidden {
		t.Fatalf("operator cancelling a run whose children are all theirs = 403 (%s)", rec.Body.String())
	}

	// Add a child in ANOTHER department. Same run, same caller, now refused — the
	// check is per child, not per run.
	exec(`INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind,
	                        executor, scope, workflow_run_id, created_at)
	      VALUES ('c-fin','j2','bash','running','t','workflow','ssh','finance','wfr-c','2026-01-01T00:00:00Z')`)
	if rec := reqAs(t, h, http.MethodPost, path, "sec-operators", ""); rec.Code != http.StatusForbidden {
		t.Errorf("operator cancelling a run with a child in another department = %d, want 403", rec.Code)
	}

	// An UNBOUND child (system-global, or a scheduled fire of an unscoped job) is
	// unrestricted-only, mirroring killJob's unbound branch.
	exec(`DELETE FROM runs WHERE id = 'c-fin'`)
	exec(`INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind,
	                        executor, scope, workflow_run_id, created_at)
	      VALUES ('c-unbound','j3','bash','running','t','workflow','ssh','','wfr-c','2026-01-01T00:00:00Z')`)
	if rec := reqAs(t, h, http.MethodPost, path, "sec-operators", ""); rec.Code != http.StatusForbidden {
		t.Errorf("restricted operator cancelling a run with an UNBOUND child = %d, want 403", rec.Code)
	}
	if rec := reqAs(t, h, http.MethodPost, path, "sec-admins", ""); rec.Code == http.StatusForbidden {
		t.Errorf("unrestricted admin cancelling a run with an unbound child = 403, want it allowed (%s)",
			rec.Body.String())
	}
}

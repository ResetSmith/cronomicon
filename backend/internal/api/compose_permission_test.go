package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/api"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// AF-2 — Compose is a grantable, AGENCY-BOUND permission
// (the af2-compose-permission plan).
//
// The route gate only establishes "may you compose somewhere". Authority over a
// PARTICULAR definition is decided in the handler against that definition's scope,
// and these tests are that rule: a FIN composer authors FIN jobs, is refused Tax
// jobs, and — the load-bearing one — is refused ALL-scoped jobs entirely, which is
// what keeps RB-30's unscoped-scheduling bypass unreachable now that compose has
// widened beyond admin.
//
// The fixture builds three actors against two agencies:
//
//	fin-composers  → custom role "composer" granted on agency FIN   (departmental)
//	tax-composers  → the same role granted on agency TAX            (departmental)
//	unrestricted   → the same role granted with all_scopes=1        (reaches All)
//	plain-viewers  → viewer, no compose at all
func composeRBACServer(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "compose_rbac.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// A custom role holding compose (and nothing else) — the whole point of AF-2
	// is that this is expressible without being admin.
	exec(`INSERT INTO roles (name, description, builtin, rank,
	                         trigger_jobs, kill_jobs, manage_env_vars,
	                         publish_schedule, configure_app, manage_roles, compose)
	      VALUES ('composer','Authors jobs within its agencies.',0,2, 0,0,0, 0,0,0, 1)`)

	for _, a := range []struct{ agency, scope string }{{"FIN", "fin-prod"}, {"TAX", "tax-prod"}} {
		exec(`INSERT INTO agencies (id,name,created_at) VALUES (?,?,'2026-01-01T00:00:00Z')`, "ag:"+a.agency, a.agency)
		exec(`INSERT INTO scopes (id,name,source,created_at) VALUES (?,?,'cronomicon','2026-01-01T00:00:00Z')`, "sc:"+a.scope, a.scope)
		exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES (?,?)`, "sc:"+a.scope, "ag:"+a.agency)
	}
	grant := func(group, role, agency string) {
		if agency == "" {
			exec(`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
			      VALUES (?,?,?,NULL,1,'2026-01-01T00:00:00Z')`, "g:"+group, group, role)
			return
		}
		exec(`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
		      VALUES (?,?,?,?,0,'2026-01-01T00:00:00Z')`, "g:"+group, group, role, "ag:"+agency)
	}
	grant("fin-composers", "composer", "FIN")
	grant("tax-composers", "composer", "TAX")
	grant("unrestricted-composers", "composer", "")
	grant("plain-viewers", "viewer", "")

	// One script for every job to bind, and three jobs: one per agency plus a
	// deliberately global one.
	exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	seedJob := func(name string, scope any) {
		exec(`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, created_at)
		      VALUES(?,'cronomicon','bash','pg_dump mydb',?,'sha256:bbb','2026-01-01T00:00:00Z')`, name, scope)
	}
	seedJob("fin-job", "fin-prod")
	seedJob("tax-job", "tax-prod")
	seedJob("all-job", nil) // the All pool — RB-30's shape

	cfg := &config.Config{
		AuthMode:       config.AuthModeTrustedHeader,
		TrustedProxies: []string{"192.0.2.0/24"},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := auth.NewService(context.Background(), cfg, pool, log)
	if err := auth.RefreshRoles(context.Background(), pool); err != nil {
		t.Fatalf("refresh roles: %v", err)
	}
	h := api.New(api.Options{Config: cfg, Logger: log, Auth: authSvc, DB: pool}).Handler()
	return h, pool
}

func composeReq(t *testing.T, h http.Handler, method, path, group, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Remote-User", group+"@example.com")
	req.Header.Set("Remote-Groups", group)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", "tok")
		req.AddCookie(&http.Cookie{Name: "amadeus_csrf", Value: "tok"})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func composeJobID(t *testing.T, pool *sql.DB, name string) string {
	t.Helper()
	var id int64
	if err := pool.QueryRow(`SELECT rowid FROM jobs WHERE name=? AND source='cronomicon'`, name).Scan(&id); err != nil {
		t.Fatalf("job id for %s: %v", name, err)
	}
	return strconv.FormatInt(id, 10)
}

// TestComposeIsGrantableWithoutAdmin — the headline of AF-2. A non-admin holding
// the compose permission may author, which RequireRole("admin") made impossible.
func TestComposeIsGrantableWithoutAdmin(t *testing.T) {
	h, _ := composeRBACServer(t)

	body := `{"name":"new-fin-job","scriptRef":"backup-db","scope":"fin-prod"}`
	if rec := composeReq(t, h, http.MethodPost, "/api/v1/jobs", "fin-composers", body); rec.Code != http.StatusCreated {
		t.Fatalf("compose-granted non-admin POST /jobs = %d, want 201. Body: %s", rec.Code, rec.Body.String())
	}
	// And the permission is genuinely required: a viewer is still refused.
	if rec := composeReq(t, h, http.MethodPost, "/api/v1/jobs", "plain-viewers",
		`{"name":"viewer-job","scriptRef":"backup-db","scope":"fin-prod"}`); rec.Code != http.StatusForbidden {
		t.Errorf("viewer POST /jobs = %d, want 403", rec.Code)
	}
}

// TestComposeIsBoundToTheActorsAgencies — holding compose SOMEWHERE is not holding
// it everywhere. This is the Q-C enforcement the old requireCompose note promised
// and never had: without it, widening the gate would have handed every composer
// every department's jobs.
func TestComposeIsBoundToTheActorsAgencies(t *testing.T) {
	h, pool := composeRBACServer(t)
	taxID := composeJobID(t, pool, "tax-job")

	t.Run("create into another agency's scope", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodPost, "/api/v1/jobs", "fin-composers",
			`{"name":"sneaky","scriptRef":"backup-db","scope":"tax-prod"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("FIN composer creating into tax-prod = %d, want 403", rec.Code)
		}
	})

	t.Run("edit another agency's job", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodPut, "/api/v1/jobs/"+taxID, "fin-composers",
			`{"name":"tax-job","scriptRef":"backup-db","scope":"tax-prod"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("FIN composer editing a tax job = %d, want 403", rec.Code)
		}
	})

	t.Run("capture another agency's job by rewriting its scope", func(t *testing.T) {
		// The both-sides rule. Checking only the INCOMING scope would let this
		// through — the payload names a scope the actor legitimately holds.
		rec := composeReq(t, h, http.MethodPut, "/api/v1/jobs/"+taxID, "fin-composers",
			`{"name":"tax-job","scriptRef":"backup-db","scope":"fin-prod"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("FIN composer capturing a tax job into fin-prod = %d, want 403 "+
				"(the scope being LEFT must be checked too)", rec.Code)
		}
	})

	t.Run("push own job out into another agency", func(t *testing.T) {
		finID := composeJobID(t, pool, "fin-job")
		rec := composeReq(t, h, http.MethodPut, "/api/v1/jobs/"+finID, "fin-composers",
			`{"name":"fin-job","scriptRef":"backup-db","scope":"tax-prod"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("FIN composer pushing its job into tax-prod = %d, want 403", rec.Code)
		}
	})

	t.Run("delete another agency's job", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodDelete, "/api/v1/jobs/"+taxID, "fin-composers", "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("FIN composer deleting a tax job = %d, want 403", rec.Code)
		}
	})

	t.Run("its own agency still works", func(t *testing.T) {
		finID := composeJobID(t, pool, "fin-job")
		rec := composeReq(t, h, http.MethodPut, "/api/v1/jobs/"+finID, "fin-composers",
			`{"name":"fin-job","scriptRef":"backup-db","scope":"fin-prod"}`)
		if rec.Code != http.StatusOK {
			t.Errorf("FIN composer editing its OWN job = %d, want 200. Body: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestComposeGrantCannotAuthorAllScopedJobs — 🔴 THE RB-30 INVARIANT, in its new
// form. Read requireCompose's note before changing this.
//
// A scheduled fire of an UNSCOPED job runs unbound and scope-unchecked (RB-Q11(c)):
// a cron fire has no actor, so there are no grants to evaluate. That was acceptable
// only while authoring a schedule required admin. AF-2 widened authoring — so the
// safety now rests HERE: a departmental composer cannot create, edit, delete, or
// move a job into the All pool, and therefore can never attach a schedule to an
// unbound job.
//
// If this test fails, you have not broken composing. You have removed the
// assumption that makes RB-Q11(c) acceptable, and an unattended, unscoped,
// unchecked run is now reachable by anyone holding compose.
func TestComposeGrantCannotAuthorAllScopedJobs(t *testing.T) {
	h, pool := composeRBACServer(t)
	allID := composeJobID(t, pool, "all-job")

	t.Run("create an All job", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodPost, "/api/v1/jobs", "fin-composers",
			`{"name":"new-all-job","scriptRef":"backup-db","scope":""}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("departmental composer creating an All job = %d, want 403", rec.Code)
		}
	})

	t.Run("create an All job WITH a schedule", func(t *testing.T) {
		// The exact escalation shape RB-30 describes, spelled out so the test
		// documents the attack and not merely the rule.
		rec := composeReq(t, h, http.MethodPost, "/api/v1/jobs", "fin-composers",
			`{"name":"unattended","scriptRef":"backup-db","scope":"",`+
				`"schedules":[{"name":"nightly","cron":"0 0 2 * * *"}]}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("departmental composer scheduling an unbound job = %d, want 403 — "+
				"this is the RB-26 bypass RB-30 exists to prevent", rec.Code)
		}
	})

	t.Run("edit an existing All job", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodPut, "/api/v1/jobs/"+allID, "fin-composers",
			`{"name":"all-job","scriptRef":"backup-db","scope":""}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("departmental composer editing an All job = %d, want 403", rec.Code)
		}
	})

	t.Run("adopt an All job into its own agency", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodPut, "/api/v1/jobs/"+allID, "fin-composers",
			`{"name":"all-job","scriptRef":"backup-db","scope":"fin-prod"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("departmental composer adopting an All job = %d, want 403", rec.Code)
		}
	})

	t.Run("move its own job into All", func(t *testing.T) {
		finID := composeJobID(t, pool, "fin-job")
		rec := composeReq(t, h, http.MethodPut, "/api/v1/jobs/"+finID, "fin-composers",
			`{"name":"fin-job","scriptRef":"backup-db","scope":""}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("departmental composer moving a job into All = %d, want 403", rec.Code)
		}
	})

	t.Run("delete an All job", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodDelete, "/api/v1/jobs/"+allID, "fin-composers", "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("departmental composer deleting an All job = %d, want 403", rec.Code)
		}
	})

	t.Run("an UNRESTRICTED compose grant may", func(t *testing.T) {
		// The counterpart that proves the rule is about REACH, not about the
		// permission: All is reserved for a grant that already covers everything.
		rec := composeReq(t, h, http.MethodPost, "/api/v1/jobs", "unrestricted-composers",
			`{"name":"legit-all-job","scriptRef":"backup-db","scope":""}`)
		if rec.Code != http.StatusCreated {
			t.Errorf("unrestricted composer creating an All job = %d, want 201. Body: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestSharedAuthoringSurfacesStayAdminOnly — AF2-D1. The old admin gate covered six
// surfaces; AF-2 widened only the two whose objects carry a scope. These four act on
// objects with NO scope — a reusable schedule template, a global calendar, a
// reaction that attaches to a definition by name, a bin spanning every definition —
// so a departmental composer reaching them would be a cross-agency escalation, not
// a delegation: retiming another department's schedule, attaching a reaction that
// TRIGGERS their job, or purging their recycle bin.
func TestSharedAuthoringSurfacesStayAdminOnly(t *testing.T) {
	h, _ := composeRBACServer(t)

	for _, tc := range []struct{ what, method, path, body string }{
		{"schedule-def create", http.MethodPost, "/api/v1/schedule-defs", `{"name":"nightly","cron":"0 2 * * *"}`},
		{"schedule-def update", http.MethodPut, "/api/v1/schedule-defs/nightly", `{"name":"nightly","cron":"0 3 * * *"}`},
		{"schedule-def delete", http.MethodDelete, "/api/v1/schedule-defs/nightly", ""},
		{"calendar create", http.MethodPost, "/api/v1/calendars", `{"name":"holidays"}`},
		{"calendar delete", http.MethodDelete, "/api/v1/calendars/holidays", ""},
		{"reactions replace", http.MethodPut, "/api/v1/reactions/job/fin-job", `{"reactions":[]}`},
		{"recycle bin list", http.MethodGet, "/api/v1/recycle-bin", ""},
	} {
		// Even the UNRESTRICTED composer is refused: this is about the surface, not
		// about reach. Only admin authors shared objects.
		if rec := composeReq(t, h, tc.method, tc.path, "unrestricted-composers", tc.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s as a non-admin composer = %d, want 403 — AF-2 did not widen this surface", tc.what, rec.Code)
		}
	}
}

// TestComposeCapabilityFlagsReflectTheGrant — the SPA gates its authoring
// affordances on these, so they must track the permission rather than the old role
// check. composeUnbound is separate on purpose: it is what lets the composer
// withhold the "All agencies (global)" option instead of teaching the rule by 403.
func TestComposeCapabilityFlagsReflectTheGrant(t *testing.T) {
	h, _ := composeRBACServer(t)

	read := func(group string) map[string]bool {
		t.Helper()
		rec := composeReq(t, h, http.MethodGet, "/api/v1/capabilities", group, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /capabilities as %s = %d", group, rec.Code)
		}
		var caps map[string]bool
		if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
			t.Fatalf("decode capabilities: %v", err)
		}
		return caps
	}

	dept := read("fin-composers")
	if !dept["compose"] {
		t.Error("a compose-granted non-admin reports compose=false; the SPA would hide every authoring affordance")
	}
	if dept["composeUnbound"] {
		t.Error("a DEPARTMENTAL composer reports composeUnbound=true; the composer would offer an All option the server refuses")
	}

	unrestricted := read("unrestricted-composers")
	if !unrestricted["compose"] || !unrestricted["composeUnbound"] {
		t.Errorf("unrestricted composer flags = compose:%v composeUnbound:%v, want both true",
			unrestricted["compose"], unrestricted["composeUnbound"])
	}

	viewer := read("plain-viewers")
	if viewer["compose"] || viewer["composeUnbound"] {
		t.Error("a viewer reports a compose capability")
	}
}

// TestWorkflowComposeIsBoundToItsJobsAgencies — a workflow has no scope of its own,
// so its authority is the union of the jobs it drives: triggering the workflow
// triggers them. Without this a departmental composer could wrap another
// department's job in a workflow and run it on a schedule — laundering exactly the
// authority the job-level check refuses.
func TestWorkflowComposeIsBoundToItsJobsAgencies(t *testing.T) {
	h, pool := composeRBACServer(t)
	wf := func(job string) string {
		return `{"name":"wf-x","steps":[{"type":"job","name":"` + job + `"}]}`
	}

	t.Run("over its own agency's job", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodPost, "/api/v1/workflows", "fin-composers", wf("fin-job"))
		if rec.Code != http.StatusCreated {
			t.Fatalf("FIN composer creating a workflow over its own job = %d, want 201. Body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("over another agency's job", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodPost, "/api/v1/workflows", "fin-composers",
			`{"name":"wf-tax","steps":[{"type":"job","name":"tax-job"}]}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("FIN composer wrapping a tax job = %d, want 403", rec.Code)
		}
	})

	t.Run("over an All-scoped job", func(t *testing.T) {
		// The RB-30 shape again, one level of indirection out: a workflow over an
		// unbound job, on a schedule, is the same unattended unchecked run.
		rec := composeReq(t, h, http.MethodPost, "/api/v1/workflows", "fin-composers",
			`{"name":"wf-all","steps":[{"type":"job","name":"all-job"}]}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("FIN composer wrapping an All job = %d, want 403", rec.Code)
		}
	})

	t.Run("editing a workflow that drives out-of-reach jobs", func(t *testing.T) {
		// Seed a workflow over the tax job directly, then try to take it over by
		// sending a graph of only FIN jobs — the both-sides rule for workflows.
		if _, err := pool.Exec(`INSERT INTO workflows(name, source, steps, enabled, created_at)
		                        VALUES('wf-taxonly','cronomicon','[{"type":"job","name":"tax-job"}]',1,'2026-01-01T00:00:00Z')`); err != nil {
			t.Fatalf("seed workflow: %v", err)
		}
		var id int64
		if err := pool.QueryRow(`SELECT rowid FROM workflows WHERE name='wf-taxonly'`).Scan(&id); err != nil {
			t.Fatalf("workflow id: %v", err)
		}
		rec := composeReq(t, h, http.MethodPut, "/api/v1/workflows/"+strconv.FormatInt(id, 10), "fin-composers",
			`{"name":"wf-taxonly","steps":[{"type":"job","name":"fin-job"}]}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("FIN composer capturing a tax workflow = %d, want 403 (the graph being REPLACED must be checked)", rec.Code)
		}
	})
}

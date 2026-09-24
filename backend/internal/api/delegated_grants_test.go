package api_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/api"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// AF-3 — delegated grant management
// (the af3-delegated-governance plan).
//
// A departmental holder of manageRoles administers access for THEIR agencies.
// The route gate is CanAnywhere (kept, so a scope-restricted admin can still
// reach Users & Access), so everything that matters is the per-object rule —
// and most of these tests are escalation attempts, because delegation is only
// worth having if it cannot be turned into promotion.
//
// Fixture:
//
//	fin-govs   → role "fin-admin" (everything except configureApp) granted on FIN
//	tax-govs   → the same role granted on TAX
//	root-govs  → builtin admin, all_scopes (the unrestricted control)
func delegatedGrantServer(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "af3.db"))
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
	// The delegate's own role, shaped like the product owner's sketch row ("FIN Admins —
	// restricted to agency, manage roles, create jobs/workflows, manage vars &
	// secrets, trigger, kill, publish"). It deliberately does NOT hold
	// configureApp, which is what it must therefore be unable to hand out.
	//
	// Note the configuration rule this encodes: a delegate can only grant what it
	// holds, so a departmental administrator's role must be a SUPERSET of the
	// roles it is expected to hand out. A manageRoles-only role could grant
	// nothing — which is the rule working, not a bug.
	exec(`INSERT INTO roles (name, description, builtin, rank,
	                         trigger_jobs, kill_jobs, manage_env_vars,
	                         publish_schedule, configure_app, manage_roles, compose)
	      VALUES ('fin-admin','Departmental administrator.',0,2, 1,1,1, 1,0,1, 1)`)
	// A role carrying a permission the delegate lacks — the amplification bait.
	exec(`INSERT INTO roles (name, description, builtin, rank,
	                         trigger_jobs, kill_jobs, manage_env_vars,
	                         publish_schedule, configure_app, manage_roles, compose)
	      VALUES ('power','Holds configure application.',0,2, 1,1,1, 0,1,0, 1)`)

	for _, a := range []string{"FIN", "TAX"} {
		exec(`INSERT INTO agencies (id,name,created_at) VALUES (?,?,'2026-01-01T00:00:00Z')`, "ag:"+a, a)
	}
	grantRow := func(gid, group, role, agency string, all int) {
		var ag any
		if agency != "" {
			ag = "ag:" + agency
		}
		exec(`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
		      VALUES (?,?,?,?,?,'2026-01-01T00:00:00Z')`, gid, group, role, ag, all)
	}
	grantRow("g:fin", "fin-govs", "fin-admin", "FIN", 0)
	grantRow("g:tax", "tax-govs", "fin-admin", "TAX", 0)
	grantRow("g:root", "root-govs", "admin", "", 1)
	// Two existing grants for the both-sides tests: one in TAX (out of the FIN
	// delegate's reach) and one unrestricted.
	grantRow("g:target-tax", "some-tax-team", "viewer", "TAX", 0)
	grantRow("g:target-global", "some-global-team", "admin", "", 1)

	cfg := &config.Config{AuthMode: config.AuthModeTrustedHeader, TrustedProxies: []string{"192.0.2.0/24"}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := auth.NewService(context.Background(), cfg, pool, log)
	if err := auth.RefreshRoles(context.Background(), pool); err != nil {
		t.Fatalf("refresh roles: %v", err)
	}
	h := api.New(api.Options{Config: cfg, Logger: log, Auth: authSvc, DB: pool}).Handler()
	return h, pool
}

// TestDelegateAdministersItsOwnAgency — the feature itself: a departmental
// administrator grants and revokes within their agency without being admin.
func TestDelegateAdministersItsOwnAgency(t *testing.T) {
	h, pool := delegatedGrantServer(t)

	rec := composeReq(t, h, http.MethodPost, "/api/v1/access-grants", "fin-govs",
		`{"adGroup":"fin-operators","role":"operator","agencyId":"ag:FIN","allScopes":false}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("delegate granting operator in its own agency = %d, want 201. Body: %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM access_grants WHERE ad_group='fin-operators'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("grant not persisted (count=%d, err=%v)", n, err)
	}
}

// TestDelegateCannotEscalate — every escalation route out of a departmental
// grant. If any of these starts passing, delegation has become promotion.
func TestDelegateCannotEscalate(t *testing.T) {
	h, _ := delegatedGrantServer(t)

	t.Run("grant into another agency", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodPost, "/api/v1/access-grants", "fin-govs",
			`{"adGroup":"mine","role":"operator","agencyId":"ag:TAX","allScopes":false}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("delegate granting into TAX = %d, want 403", rec.Code)
		}
	})

	t.Run("grant everywhere", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodPost, "/api/v1/access-grants", "fin-govs",
			`{"adGroup":"mine","role":"operator","agencyId":"","allScopes":true}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("delegate minting an all-scopes grant = %d, want 403", rec.Code)
		}
	})

	t.Run("grant a permission it does not hold", func(t *testing.T) {
		// The amplification attempt: 'power' carries configureApp and compose,
		// which fin-admin does not. Granting it to a group the delegate belongs to
		// would promote them in one step.
		rec := composeReq(t, h, http.MethodPost, "/api/v1/access-grants", "fin-govs",
			`{"adGroup":"fin-govs","role":"power","agencyId":"ag:FIN","allScopes":false}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("delegate granting a role carrying configureApp = %d, want 403 — this is self-promotion", rec.Code)
		}
	})

	t.Run("grant admin to itself", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodPost, "/api/v1/access-grants", "fin-govs",
			`{"adGroup":"fin-govs","role":"admin","agencyId":"ag:FIN","allScopes":false}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("delegate granting itself admin = %d, want 403", rec.Code)
		}
	})

	t.Run("capture another agency's grant by re-pointing it", func(t *testing.T) {
		// The both-sides rule: the payload names the delegate's OWN agency, so
		// only checking the incoming row would let this through.
		rec := composeReq(t, h, http.MethodPut, "/api/v1/access-grants/g:target-tax", "fin-govs",
			`{"adGroup":"some-tax-team","role":"viewer","agencyId":"ag:FIN","allScopes":false}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("delegate re-pointing a TAX grant into FIN = %d, want 403", rec.Code)
		}
	})

	t.Run("re-point a global admin grant into its agency", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodPut, "/api/v1/access-grants/g:target-global", "fin-govs",
			`{"adGroup":"some-global-team","role":"viewer","agencyId":"ag:FIN","allScopes":false}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("delegate re-pointing a global grant = %d, want 403", rec.Code)
		}
	})

	t.Run("revoke another agency's grant", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodDelete, "/api/v1/access-grants/g:target-tax", "fin-govs", "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("delegate deleting a TAX grant = %d, want 403", rec.Code)
		}
	})

	t.Run("revoke the global admin grant", func(t *testing.T) {
		// The lockout floor: if this passed, a departmental admin could strip the
		// instance of its administrators.
		rec := composeReq(t, h, http.MethodDelete, "/api/v1/access-grants/g:target-global", "fin-govs", "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("delegate deleting the global admin grant = %d, want 403", rec.Code)
		}
	})
}

// TestRoleTemplatesAreUnrestrictedOnly — AF3-D1a. A role is shared by every
// agency, and editing one is the circular route around the amplification rule:
// add a permission to a role, then grant that role to yourself and pass the
// check, because by then you hold what you just granted yourself.
func TestRoleTemplatesAreUnrestrictedOnly(t *testing.T) {
	h, _ := delegatedGrantServer(t)

	body := `{"name":"fin-admin","description":"x","rank":2,"permissions":{"triggerJobs":true,"killJobs":false,` +
		`"manageEnvVars":false,"publishSchedule":false,"configureApp":true,"manageRoles":true,"compose":false}}`

	t.Run("delegate may not edit a role", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodPut, "/api/v1/roles/fin-admin", "fin-govs", body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("delegate adding configureApp to its own role = %d, want 403 — this is the circular escalation", rec.Code)
		}
	})

	t.Run("delegate may not create a role", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodPost, "/api/v1/roles", "fin-govs",
			`{"name":"invented","rank":2,"permissions":{"triggerJobs":true,"killJobs":false,"manageEnvVars":false,`+
				`"publishSchedule":false,"configureApp":true,"manageRoles":false,"compose":false}}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("delegate creating a role = %d, want 403", rec.Code)
		}
	})

	t.Run("delegate may not delete a role", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodDelete, "/api/v1/roles/power", "fin-govs", "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("delegate deleting a role = %d, want 403", rec.Code)
		}
	})

	t.Run("but the screen stays readable", func(t *testing.T) {
		// The identity.go lockout note: a restricted administrator must still be
		// able to SEE roles, or the screen that fixes their grants is unreachable.
		if rec := composeReq(t, h, http.MethodGet, "/api/v1/roles", "fin-govs", ""); rec.Code != http.StatusOK {
			t.Errorf("delegate reading roles = %d, want 200 — the screen must stay reachable", rec.Code)
		}
		if rec := composeReq(t, h, http.MethodGet, "/api/v1/access-grants", "fin-govs", ""); rec.Code != http.StatusOK {
			t.Errorf("delegate reading grants = %d, want 200", rec.Code)
		}
	})
}

// TestUnrestrictedAdminIsUnaffected — the control. Every rule above is scoped to
// delegates; a global administrator must administer exactly as before, or AF-3
// has broken the instance's own governance instead of extending it.
func TestUnrestrictedAdminIsUnaffected(t *testing.T) {
	h, _ := delegatedGrantServer(t)

	for _, tc := range []struct{ what, method, path, body string }{
		{"grant into any agency", http.MethodPost, "/api/v1/access-grants",
			`{"adGroup":"anyone","role":"operator","agencyId":"ag:TAX","allScopes":false}`},
		{"grant everywhere", http.MethodPost, "/api/v1/access-grants",
			`{"adGroup":"anyone-global","role":"operator","agencyId":"","allScopes":true}`},
		{"grant a powerful role", http.MethodPost, "/api/v1/access-grants",
			`{"adGroup":"power-team","role":"power","agencyId":"ag:FIN","allScopes":false}`},
	} {
		rec := composeReq(t, h, tc.method, tc.path, "root-govs", tc.body)
		if rec.Code != http.StatusCreated {
			t.Errorf("unrestricted admin, %s = %d, want 201. Body: %s", tc.what, rec.Code, rec.Body.String())
		}
	}
	// And role templates remain theirs to edit.
	rec := composeReq(t, h, http.MethodPut, "/api/v1/roles/power", "root-govs",
		`{"name":"power","description":"still theirs","rank":2,"permissions":{"triggerJobs":true,"killJobs":true,`+
			`"manageEnvVars":true,"publishSchedule":false,"configureApp":true,"manageRoles":false,"compose":true}}`)
	if rec.Code != http.StatusOK {
		t.Errorf("unrestricted admin editing a role = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
}

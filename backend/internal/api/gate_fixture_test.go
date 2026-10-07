package api_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/api"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// The two-agency fixture (GC-18).
//
// Every cross-agency hole closed in v2.2.2 was invisible to a suite whose
// default actor is one unrestricted admin: with a single principal there is no
// "other agency" for a gate to keep out. This fixture is the standing cast for
// any test that has to tell "holds the permission somewhere" from "holds it
// here" from "holds it everywhere", so a new gate is tested against the actors
// that distinguish those three rather than against the one that passes all of
// them.
//
// Agencies and scopes:
//
//	FIN  ag:FIN  → scope fin-hosts (sc:fin)
//	TAX  ag:TAX  → scope tax-hosts (sc:tax)
//	—            → scope shared    (sc:shared, no agency)
//
// Principals, by the AD group a request presents (gateReq):
//
//	gRoot         admin    on every scope   — the global administrator
//	gFinAdmin     admin    on FIN           — an administrator of ONE agency
//	gTaxAdmin     admin    on TAX
//	gFinApprover  approver on FIN           — may run and publish, not configure
//	gFinOperator  operator on FIN
//	gFinViewer    viewer   on FIN
//	gAllViewer    viewer   on every scope   — reaches everything, holds no verb
//	gAllComposer  composer on every scope   — custom role: compose and nothing else
//	gAllFull      full     on every scope   — custom role: every permission
//	gMixed        gAllViewer + gFinAdmin    — the permission-blind case: reads as
//	                                          "unrestricted" and is an admin of one
//	                                          agency, and is neither a global admin
const (
	gRoot        = "root-admins"
	gFinAdmin    = "fin-admins"
	gTaxAdmin    = "tax-admins"
	gFinApprover = "fin-approvers"
	gFinOperator = "fin-operators"
	gFinViewer   = "fin-viewers"
	gAllViewer   = "all-viewers"
	gAllComposer = "all-composers"
	gAllFull     = "all-full"
	gMixed       = gAllViewer + "," + gFinAdmin
)

// gateServer boots a trusted-header server over a migrated database seeded with
// the cast above. The KEK is set so secret and credential routes work.
func gateServer(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()
	return gateServerWith(t, nil)
}

// gateServerWith is gateServer with the configuration adjusted before the
// server is built — DevAuth, for a test that needs a cookie session beside the
// trusted-header cast.
func gateServerWith(t *testing.T, adjust func(*config.Config)) (http.Handler, *sql.DB) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := mustExec(t, pool)

	exec(`INSERT INTO roles (name, description, builtin, rank,
	                         trigger_jobs, kill_jobs, manage_env_vars,
	                         publish_schedule, configure_app, manage_roles, compose)
	      VALUES ('composer','Authors jobs and workflows; nothing else.',0,2, 0,0,0, 0,0,0, 1),
	             ('full','Every permission, under a name that is not admin.',0,3, 1,1,1, 1,1,1, 1)`)

	for _, a := range []string{"FIN", "TAX"} {
		exec(`INSERT INTO agencies (id,name,created_at) VALUES (?,?,'2026-01-01T00:00:00Z')`, "ag:"+a, a)
	}
	for _, s := range [][3]string{{"sc:fin", "fin-hosts", "ag:FIN"}, {"sc:tax", "tax-hosts", "ag:TAX"}, {"sc:shared", "shared", ""}} {
		exec(`INSERT INTO scopes (id,name,source,created_at) VALUES (?,?,'cronomicon','2026-01-01T00:00:00Z')`, s[0], s[1])
		if s[2] != "" {
			exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES (?,?)`, s[0], s[2])
		}
	}
	grant := func(group, role, agency string) {
		var ag any
		all := 1
		if agency != "" {
			ag, all = "ag:"+agency, 0
		}
		exec(`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
		      VALUES (?,?,?,?,?,'2026-01-01T00:00:00Z')`, "g:"+group, group, role, ag, all)
	}
	grant(gRoot, "admin", "")
	grant(gFinAdmin, "admin", "FIN")
	grant(gTaxAdmin, "admin", "TAX")
	grant(gFinApprover, "approver", "FIN")
	grant(gFinOperator, "operator", "FIN")
	grant(gFinViewer, "viewer", "FIN")
	grant(gAllViewer, "viewer", "")
	grant(gAllComposer, "composer", "")
	grant(gAllFull, "full", "")

	cfg := &config.Config{
		AuthMode:       config.AuthModeTrustedHeader,
		TrustedProxies: []string{"192.0.2.0/24"},
		SecretKEKEnv:   "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
	}
	if adjust != nil {
		adjust(cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := auth.NewService(context.Background(), cfg, pool, log)
	if err := auth.RefreshRoles(context.Background(), pool); err != nil {
		t.Fatalf("refresh roles: %v", err)
	}
	// The roles cache is process-wide; put the built-in set back for the next test.
	t.Cleanup(func() { _ = auth.RefreshRoles(context.Background(), pool) })
	h := api.New(api.Options{Config: cfg, Logger: log, Auth: authSvc, DB: pool}).Handler()
	return h, pool
}

// gateReq issues one request as a member of the given AD group(s) — a
// comma-separated list presents several, as gMixed does.
func gateReq(t *testing.T, h http.Handler, method, path, groups, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = "192.0.2.10:40000"
	req.Header.Set("Remote-User", strings.ReplaceAll(groups, ",", "+")+"@example.com")
	req.Header.Set("Remote-Groups", groups)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", "tok")
		req.AddCookie(&http.Cookie{Name: "cronomicon_csrf", Value: "tok"})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// gateReqWithHeader is gateReq with one extra request header (If-Match, …).
func gateReqWithHeader(t *testing.T, h http.Handler, method, path, groups, body, header, value string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.10:40000"
	req.Header.Set("Remote-User", strings.ReplaceAll(groups, ",", "+")+"@example.com")
	req.Header.Set("Remote-Groups", groups)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", "tok")
	req.Header.Set(header, value)
	req.AddCookie(&http.Cookie{Name: "cronomicon_csrf", Value: "tok"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/auth"
)

// requireCan is the RB-5 seam: it exists in v0.56.0 with no call sites so that
// RB-2 (v0.56.4) is a one-line substitution per route rather than a new mechanism
// landing in the same release as a behavioral shock. That justification only holds
// if the seam is actually verified, so these tests exercise it directly — the
// compiler cannot catch a regression in code nothing calls.
//
// This is an internal (package api) test because requireCan is unexported.
//
// RB-15: Can reads the RESOLVED grants, so these identities carry them explicitly.
// `granted` builds the wide shape (one grant per role over the given scopes), which
// is what a login resolves to for an actor whose grants all cover the same reach —
// these cases are about requireCan's response and audit behavior, not about grant
// resolution.
func granted(roles []string, scopes []string) auth.Identity {
	id := auth.Identity{Roles: roles, AllowedScopes: scopes}
	for _, r := range roles {
		id.Grants = append(id.Grants, auth.RoleGrant{Role: r, Scopes: scopes})
	}
	return id
}

func withEmail(id auth.Identity, email string) auth.Identity {
	id.Email = email
	return id
}

func TestRequireCanAllowsAndDenies(t *testing.T) {
	srv := &Server{}

	cases := []struct {
		name  string
		id    auth.Identity
		perm  string
		scope string
		allow bool
	}{
		{
			name: "operator with the verb on the scope",
			id:   withEmail(granted([]string{"operator"}, []string{"tax"}), "op@example.com"),
			perm: auth.PermTriggerJobs, scope: "tax", allow: true,
		},
		{
			name: "operator on a scope they do not hold",
			id:   withEmail(granted([]string{"operator"}, []string{"tax"}), "op@example.com"),
			perm: auth.PermTriggerJobs, scope: "finance", allow: false,
		},
		{
			name: "viewer lacks the verb even on a scope they hold",
			id:   withEmail(granted([]string{"viewer"}, []string{"tax"}), "v@example.com"),
			perm: auth.PermTriggerJobs, scope: "tax", allow: false,
		},
		{
			name: "unrestricted admin reaches any scope",
			id:   withEmail(granted([]string{"admin"}, []string{auth.AllScopes}), "a@example.com"),
			perm: auth.PermKillJobs, scope: "anything", allow: true,
		},
		{
			// Q-F7 preserved in v0.56.0: the empty effective scope is allowed for
			// everyone. RB-26 changes this at the call site via CanUnbound, not here.
			name: "empty scope is allowed for a restricted actor (Q-F7)",
			id:   withEmail(granted([]string{"operator"}, []string{"tax"}), "op@example.com"),
			perm: auth.PermTriggerJobs, scope: "", allow: true,
		},
		{
			name: "no roles denies",
			id:   auth.Identity{Email: "nobody@example.com"},
			perm: auth.PermTriggerJobs, scope: "tax", allow: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/x/run", nil)
			r = r.WithContext(auth.WithIdentity(r.Context(), tc.id))

			got := srv.requireCan(rec, r, tc.id, tc.perm, tc.scope)
			if got != tc.allow {
				t.Fatalf("requireCan = %v, want %v", got, tc.allow)
			}
			if tc.allow {
				// On success it must write NOTHING — the handler continues and owns
				// the response. A helper that writes a 200 here would corrupt every
				// call site it is added to.
				if rec.Body.Len() != 0 {
					t.Errorf("requireCan wrote a body on the allow path: %q", rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("denial status = %d, want 403", rec.Code)
			}
			var body struct {
				Error   string `json:"error"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("denial body is not JSON: %v (%s)", err, rec.Body.String())
			}
			// RB-5/RB-25: the message must name the permission AND the scope, or
			// "why was I denied?" is unanswerable under departmental RBAC.
			if !strings.Contains(body.Message, tc.perm) {
				t.Errorf("denial message %q does not name the permission %q", body.Message, tc.perm)
			}
			wantScope := tc.scope
			if wantScope == "" {
				wantScope = auth.AllScopes
			}
			if !strings.Contains(body.Message, wantScope) {
				t.Errorf("denial message %q does not name the scope %q", body.Message, wantScope)
			}
		})
	}
}

// TestRequireCanSurvivesNilAuthService pins that the helper degrades to a plain
// 403 when no auth service is wired, rather than panicking. Every sibling helper
// in authz.go guards s.auth the same way, and a nil-pointer panic inside an authz
// denial path would turn a 403 into a 500 — failing OPEN from the caller's point
// of view if any middleware treats 5xx as retryable.
func TestRequireCanSurvivesNilAuthService(t *testing.T) {
	srv := &Server{} // s.auth is nil
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/x/run", nil)
	id := withEmail(granted([]string{"viewer"}, []string{"tax"}), "v@example.com")

	if srv.requireCan(rec, r, id, auth.PermTriggerJobs, "tax") {
		t.Fatal("requireCan allowed a viewer to trigger")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

package auth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// Service-account authentication (ET-A, the prod-features plan §1).
//
// This is the machine half of the identity story, and its whole design is that
// it produces an ORDINARY Identity. Everything downstream of authentication —
// requireCan, Can / CanAnywhere / CanUnbound, the scope-where fragments, the
// audit actor, InsertChangeLog — reads only auth.IdentityFrom(ctx) and needed
// no change to accommodate a token. A service account is a principal, not a
// bypass.
//
// It is modeled on RequireRunner (T9): bearer header, no cookies, no CSRF. CSRF
// is a browser-session defense and is meaningless for a caller that never has a
// cookie to be ridden — csrf.go says so explicitly for runner routes and the
// same reasoning applies here.
//
// ⚠️ requirePerm CANNOT be used on a token route. It chains
// RequireSession → RequireCSRF → predicate, so a token-authenticated request
// would be rejected by the first link. Token routes authorize IN THE HANDLER
// via requireCan / Can, which is the scoped check anyway.

// ServicePrincipalPrefix marks a service account's audit actor. A run triggered
// by the "nagios" account records `svc:nagios` in runs.triggered_by, the change
// log and the activity stream, alongside the existing `runner:<id>`,
// `scheduler` and `operator` sentinels.
const ServicePrincipalPrefix = "svc:"

// ServiceActor renders a service account's name as its audit actor.
func ServiceActor(name string) string { return ServicePrincipalPrefix + name }

// RequireServiceToken authenticates a service account via
// `Authorization: Bearer <token>` and attaches the resolved Identity, so the
// handler behind it is written exactly as a session handler would be.
//
// A token is valid when its hash matches a non-revoked, unexpired
// service_accounts row. The row's (role, agency|*) grant is expanded to scopes
// through the same AgencyScopes the login path uses, so adding a scope to an
// agency reaches every token bound to it on the NEXT request — tokens have no
// session cookie to go stale, which makes them strictly fresher than humans.
func (s *Service) RequireServiceToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		id, name, valid, err := ServiceTokenIdentity(r.Context(), s.db, token)
		if err != nil {
			s.log.Error("service token check failed", "error", err)
			httpx.Fail(w, http.StatusServiceUnavailable, "unavailable", "auth backend error")
			return
		}
		if !valid {
			// The response is deliberately indistinguishable across revoked /
			// expired / never-existed: a caller must not be able to probe which
			// applies, which would confirm an account name for them. The audit row
			// is where the distinction would be drawn, and it cannot be drawn here
			// either — we hold only a hash that matched nothing.
			s.auditAuth(r.Context(), r, auditlog.AuthEventParams{
				Kind:    auditlog.AuthLoginFailed,
				Outcome: auditlog.OutcomeFailure,
				Reason:  "invalid_service_token",
				Target:  r.URL.Path,
			})
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "invalid or expired service token")
			return
		}
		// Best-effort liveness stamp. Deliberately not in the request's critical
		// path: a failure here must never turn a valid trigger into a 500, and the
		// column exists only to answer "can I revoke this yet?".
		if err := TouchServiceAccount(r.Context(), s.db, name); err != nil {
			s.log.Warn("service token: stamp last_used_at", "account", name, "err", err)
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
	})
}

// ServiceTokenIdentity resolves a plaintext token to the Identity it authorizes
// as. Exported so the trigger handlers' tests (and any future non-HTTP caller)
// can mint the same principal the middleware does.
//
// Returns the account NAME alongside the identity because the caller needs it
// for the liveness stamp; the identity's Email carries it in `svc:` form.
func ServiceTokenIdentity(ctx context.Context, db *sql.DB, token string) (Identity, string, bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var (
		name, role string
		agencyID   sql.NullString
		allScopes  int
	)
	err := db.QueryRowContext(ctx, `
		SELECT name, role, agency_id, all_scopes
		  FROM service_accounts
		 WHERE token_hash = ?
		   AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > ?)
		 LIMIT 1`, HashToken(token), now).Scan(&name, &role, &agencyID, &allScopes)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, "", false, nil
	}
	if err != nil {
		return Identity{}, "", false, err
	}

	grant := RoleGrant{Role: strings.ToLower(strings.TrimSpace(role))}
	if allScopes == 1 {
		// The "*" sentinel — unrestricted, exactly as an all_scopes access_grant.
		grant.Scopes = []string{AllScopes}
	} else {
		byAgency, err := AgencyScopes(ctx, db, []string{agencyID.String})
		if err != nil {
			return Identity{}, "", false, err
		}
		// An agency holding no scopes yields an EMPTY grant, which is zero access
		// — never "all". That is A5's tri-state and the reason this is not
		// `len(scopes) > 0 ? scopes : all`.
		grant.Agency = agencyID.String
		grant.Scopes = byAgency[agencyID.String]
	}

	id := Identity{
		Email:         ServiceActor(name),
		DisplayName:   name,
		Roles:         []string{grant.Role},
		AllowedScopes: append([]string{}, grant.Scopes...),
		IssuedAt:      time.Now().UTC(),
		Grants:        []RoleGrant{grant},
	}
	return id, name, true, nil
}

// TouchServiceAccount stamps last_used_at for a successful authentication.
func TouchServiceAccount(ctx context.Context, db *sql.DB, name string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE service_accounts SET last_used_at = ? WHERE name = ?`,
		time.Now().UTC().Format(time.RFC3339), name)
	return err
}

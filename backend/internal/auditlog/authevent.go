package auditlog

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Auth event kinds (LU-9). Free-form in the schema by design — migration 720
// omits a CHECK because widening one means rebuilding the table — so these
// constants are the contract instead.
const (
	AuthLogin          = "login"           // a session was established
	AuthLoginFailed    = "login-failed"    // an attempt did not establish one
	AuthLogout         = "logout"          // the operator ended their session
	AuthDenied         = "denied"          // authenticated, but not permitted
	AuthCSRFFailed     = "csrf-failed"     // a state-changing request failed the double-submit check
	AuthDevAuth        = "dev-auth"        // the local-preview bypass minted a session
	AuthBootstrapAdmin = "bootstrap-admin" // admin granted via the bootstrap group
	AuthSessionRevoked = "session-revoked" // an epoch bump forced a session out
)

// Auth outcomes.
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
)

// AuthEventParams is one row of the auth audit trail.
//
// Actor is deliberately optional: the events that matter most — a failed login,
// a CSRF rejection — happen before an identity is known, and inventing a
// sentinel would corrupt the column an auditor filters on. That is precisely why
// this lives in its own table rather than change_log, whose actor is NOT NULL.
type AuthEventParams struct {
	Kind    string
	Outcome string
	Actor   string
	// Reason is a stable machine token (exchange_failed, bad_nonce,
	// insufficient_scope, …), not a human sentence — it is what a detection rule
	// keys on. Human context belongs in Details.
	Reason     string
	Target     string // the denied scope / required role / resource
	RemoteAddr string
	ClientIP   string
	UserAgent  string
	Details    string
}

// WriteAuthEvent records an authentication or authorization event.
//
// Callers must treat a failure as non-fatal and swallow it: this is the login
// path, and a full disk or a locked database must not be able to stop people
// signing in. That matches the precedent already set by recordLogin, whose write
// error is logged and dropped. The returned error exists so a caller that wants
// to log it can; nobody should branch on it.
func WriteAuthEvent(ctx context.Context, db *sql.DB, p AuthEventParams) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if p.Outcome == "" {
		p.Outcome = OutcomeFailure // fail-safe: an unspecified outcome is not a success
	}
	// Mask before trim (AM-1). reason is a machine token by contract but is
	// still caller-supplied text, and target is the denied resource — both are
	// masked alongside details (AM-2) so a masker installed in main covers every
	// free-text column of the row.
	p.Reason = TrimForAudit(mask(p.Reason))
	p.Target = TrimForAudit(mask(p.Target))
	details := TrimForAudit(mask(p.Details))
	_, err := db.ExecContext(ctx, `
		INSERT INTO auth_events
			(at, kind, outcome, actor, reason, target, remote_addr, client_ip, user_agent, details, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		now, p.Kind, p.Outcome, nullStr(p.Actor), nullStr(p.Reason), nullStr(p.Target),
		nullStr(p.RemoteAddr), nullStr(p.ClientIP), nullStr(p.UserAgent), nullStr(details), now)
	if err != nil {
		return fmt.Errorf("write auth_event: %w", err)
	}
	emit(Event{
		At: now, Source: "auth", Kind: p.Kind, Outcome: p.Outcome,
		Actor: p.Actor, Reason: p.Reason, Target: p.Target,
		RemoteAddr: p.RemoteAddr, ClientIP: p.ClientIP, UserAgent: p.UserAgent,
		Details: details,
	})
	return nil
}

package runner

import (
	"strings"
	"testing"
)

// TestManifestAuditsInjection (P1.6): a manifest fetch that injects references
// writes exactly ONE change_log "Secrets"/"injected" row — target = trace id,
// details carry the reference names + source (never values) — and a re-fetch does
// NOT duplicate it (guarded by runs.injection_audited).
func TestManifestAuditsInjection(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-audit", "amt_run_audit"
	insertRunner(t, svc, runnerID, "audit", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	traceID, secretVal, _ := seedInjectionRun(t, svc, runnerID, 6, true)

	// First fetch resolves + audits.
	m := getManifest(t, svc, as, traceID, tok)
	if m.Secrets["CRONOMICON_SECRET_DB_PASS"] != secretVal {
		t.Fatalf("precondition: secret not injected: %q", m.Secrets["CRONOMICON_SECRET_DB_PASS"])
	}

	var count int
	var actor, details string
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE category='Secrets' AND action='injected' AND target=?`, traceID).Scan(&count); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 injection audit row, got %d", count)
	}
	_ = svc.db.QueryRow(`SELECT actor, details FROM change_log WHERE category='Secrets' AND action='injected' AND target=?`, traceID).Scan(&actor, &details)
	if actor != "ops@x" { // seedInjectionRun sets triggered_by='ops@x'
		t.Errorf("audit actor = %q, want ops@x (the run's triggered_by)", actor)
	}
	// Names + source recorded; the VALUE must never appear.
	if !strings.Contains(details, "DB_PASS") || !strings.Contains(details, "REGION") {
		t.Errorf("audit details missing reference names: %s", details)
	}
	if strings.Contains(details, secretVal) {
		t.Errorf("audit details leaked the secret value: %s", details)
	}

	// A re-fetch (agent retry) must not write a second row.
	_ = getManifest(t, svc, as, traceID, tok)
	_ = svc.db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE category='Secrets' AND action='injected' AND target=?`, traceID).Scan(&count)
	if count != 1 {
		t.Errorf("re-fetch duplicated the injection audit: got %d rows, want 1", count)
	}

	// The run is flagged audited.
	var audited bool
	_ = svc.db.QueryRow(`SELECT injection_audited FROM runs WHERE id=?`, traceID).Scan(&audited)
	if !audited {
		t.Errorf("injection_audited not set after audit")
	}
}

// TestManifestFailsClosedOnAuditError (P1.6): if the injection audit cannot be
// written, the manifest is withheld (500 audit_failed) rather than shipping secret
// material with no audit trail — mirrors the reveal handler.
func TestManifestFailsClosedOnAuditError(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-audit-fc", "amt_run_audit_fc"
	insertRunner(t, svc, runnerID, "auditfc", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	traceID, _, _ := seedInjectionRun(t, svc, runnerID, 6, true)

	// Break the audit sink so WriteChangeLog errors.
	if _, err := svc.db.Exec(`DROP TABLE change_log`); err != nil {
		t.Fatalf("drop change_log: %v", err)
	}

	rec := callManifest(t, svc, as, traceID, tok)
	if rec.Code != 500 {
		t.Fatalf("got %d, want 500; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "audit_failed") {
		t.Errorf("expected audit_failed, got: %s", rec.Body.String())
	}
	// Not flagged audited (so a retry re-attempts once the sink recovers).
	var audited bool
	_ = svc.db.QueryRow(`SELECT injection_audited FROM runs WHERE id=?`, traceID).Scan(&audited)
	if audited {
		t.Errorf("injection_audited must not be set when the audit write failed")
	}
}

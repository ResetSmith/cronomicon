package notify

import (
	"sort"
	"strings"
	"testing"
)

// AL-1 (2.5.0 C4): an agency's rule matches only runs of its agency; a Global
// rule matches every agency's runs, as every rule did before. A missed fire
// (no run) is placed by its job's uid.
func TestAnAgencysRuleMatchesOnlyItsAgencysRuns(t *testing.T) {
	d, pool := newDispatcherDB(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at, owner_agency) VALUES
	        ('load', 'cronomicon', 'uid-fin', 'bash', 't', 'ag:FIN'),
	        ('load', 'cronomicon', 'uid-tax', 'bash', 't', 'ag:TAX')`)
	exec(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, status, triggered_by, trigger_kind, created_at) VALUES
	        ('r-fin', 'load', 'cronomicon', 'uid-fin', 'bash', 'failure', 't', 'manual', 't'),
	        ('r-tax', 'load', 'cronomicon', 'uid-tax', 'bash', 'failure', 't', 'manual', 't')`)
	exec(`INSERT INTO alert_config (id, target_mode, trigger, channels, recipients, enabled, created_at, owner_agency) VALUES
	        ('a-fin',  'all', 'failure',    '["email"]', 'fin@example',  1, 'now', 'ag:FIN'),
	        ('a-glob', 'all', 'failure',    '["email"]', 'glob@example', 1, 'now', 'global'),
	        ('a-finm', 'all', 'missed-run', '["email"]', 'fin@example',  1, 'now', 'ag:FIN')`)
	exec(`INSERT INTO notification_config
		(id, smtp_host, smtp_port, smtp_from, smtp_encryption, smtp_recipients, apprise_enabled, apprise_targets, last_modified_at)
		VALUES (1,'mail.example.com',587,'cronomicon@example.com','starttls','[]',0,'[]','now')`)

	var to []string
	d.sendEmail = func(_ SMTP, rcpt []string, _, _ string) error { to = append([]string(nil), rcpt...); return nil }
	got := func() string {
		sort.Strings(to)
		s := strings.Join(to, ",")
		to = nil
		return s
	}

	if err := d.dispatch(t.Context(), runDispatchEvent(RunEvent{TraceID: "r-fin", JobName: "load", Status: "failure"})); err != nil {
		t.Fatal(err)
	}
	if g := got(); g != "fin@example,glob@example" {
		t.Errorf("FIN's failed run mailed %q; want FIN's rule and Global's", g)
	}
	if err := d.dispatch(t.Context(), runDispatchEvent(RunEvent{TraceID: "r-tax", JobName: "load", Status: "failure"})); err != nil {
		t.Fatal(err)
	}
	if g := got(); g != "glob@example" {
		t.Errorf("TAX's failed run mailed %q; want Global's rule only (FIN's never matches TAX's run)", g)
	}
	// A missed fire of TAX's load: no run, placed by the job's uid.
	if err := d.dispatch(t.Context(), dispatchEvent{jobName: "load", jobUID: "uid-tax", matchKey: TriggerMissedRun,
		subject: "s", body: "b"}); err != nil {
		t.Fatal(err)
	}
	if g := got(); g != "" {
		t.Errorf("TAX's missed fire mailed %q; want nothing (FIN's missed-run rule is FIN's)", g)
	}
	if err := d.dispatch(t.Context(), dispatchEvent{jobName: "load", jobUID: "uid-fin", matchKey: TriggerMissedRun,
		subject: "s", body: "b"}); err != nil {
		t.Fatal(err)
	}
	if g := got(); g != "fin@example" {
		t.Errorf("FIN's missed fire mailed %q; want FIN's missed-run rule", g)
	}
}

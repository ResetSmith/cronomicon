package notify

import (
	"context"
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

// AL-2 (2.5.0 C4): a push goes to the targets the matched rules name, each
// once; and (question 80) the installation's default recipients are on a
// Global rule's mail only, not on an agency's.
func TestAPushGoesToTheMatchedRulesTargets(t *testing.T) {
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
	        ('a-fin',  'all', 'failure', '["email","apprise"]', 'fin@example', 1, 'now', 'ag:FIN'),
	        ('a-glob', 'all', 'failure', '["apprise"]',         '',            1, 'now', 'global')`)
	exec(`INSERT INTO notification_config
		(id, smtp_host, smtp_port, smtp_from, smtp_encryption, smtp_recipients, apprise_enabled, apprise_url, apprise_targets, last_modified_at)
		VALUES (1,'mail.example.com',587,'cronomicon@example.com','starttls','["installation@example"]',1,'http://apprise:8000','[]','now')`)
	seedTarget(t, pool, "t-fin", "ag:FIN", "json://fin", true, "a-fin")
	seedTarget(t, pool, "t-off", "ag:FIN", "json://fin-off", false, "a-fin")
	seedTarget(t, pool, "t-ops", "global", "json://ops", true, "a-glob", "a-fin") // named twice: sent once
	seedTarget(t, pool, "t-idle", "global", "json://idle", true)                  // named by no rule

	var mailed, pushed []string
	d.sendEmail = func(_ SMTP, rcpt []string, _, _ string) error { mailed = rcpt; return nil }
	d.sendPush = func(_ context.Context, _ string, tg []string, _, _ string) error { pushed = tg; return nil }
	run := func(trace string) (string, string) {
		t.Helper()
		mailed, pushed = nil, nil
		if err := d.dispatch(t.Context(), runDispatchEvent(RunEvent{TraceID: trace, JobName: "load", Status: "failure"})); err != nil {
			t.Fatal(err)
		}
		sort.Strings(mailed)
		sort.Strings(pushed)
		return strings.Join(mailed, ","), strings.Join(pushed, ",")
	}

	m, p := run("r-fin")
	if p != "json://fin,json://ops" {
		t.Errorf("FIN's run pushed to %q; want FIN's enabled target and the one Global's rule names, once each", p)
	}
	if m != "fin@example" {
		t.Errorf("FIN's run mailed %q; want FIN's rule's recipients only: the defaults are on Global rules' mail", m)
	}
	m, p = run("r-tax")
	if p != "json://ops" || m != "" {
		t.Errorf("TAX's run pushed %q and mailed %q; want Global's rule's target and no mail", p, m)
	}
	// A Global rule that mails brings the defaults.
	exec(`UPDATE alert_config SET channels = '["email"]', recipients = 'glob@example' WHERE id = 'a-glob'`)
	if m, _ = run("r-tax"); m != "glob@example,installation@example" {
		t.Errorf("a Global rule's mail went to %q; want its recipients and the installation's", m)
	}
}

package notify

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// 2.5.1 SG-17: an event about FIN's secret reaches FIN's rules and Global's,
// not TAX's; a rule aimed at a job does not hear it; backup-key-changed is
// Global's alone; and the body names no job.
func TestSecretAlertsReachTheirAgency(t *testing.T) {
	d, pool := newDispatcherDB(t)
	for _, q := range []string{
		`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin','Finance','now'), ('ag-tax','Tax','now')`,
		`INSERT INTO notification_config (id, smtp_host, smtp_port, smtp_from, smtp_encryption, smtp_recipients, apprise_enabled, apprise_url, apprise_targets, last_modified_at)
		 VALUES (1,'mail.example.com',587,'cronomicon@example.com','starttls','[]',0,'','[]','now')`,
		`INSERT INTO alert_config (id, target_mode, job_name, trigger, channels, recipients, enabled, created_at, owner_agency) VALUES
		 ('fin-all','all',NULL,'secret-revealed','["email"]','fin@x',1,'now','ag-fin'),
		 ('fin-job','job','deploy','secret-revealed','["email"]','fin-job@x',1,'now','ag-fin'),
		 ('tax-all','all',NULL,'secret-revealed','["email"]','tax@x',1,'now','ag-tax'),
		 ('glob-all','all',NULL,'secret-revealed','["email"]','ops@x',1,'now','global'),
		 ('glob-keys','all',NULL,'backup-key-changed','["email"]','keys@x',1,'now','global'),
		 ('fin-keys','all',NULL,'backup-key-changed','["email"]','fin-keys@x',1,'now','ag-fin')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	var to []string
	var body string
	d.sendEmail = func(_ SMTP, rcpt []string, _ string, b string) error { to = append(to, rcpt...); body = b; return nil }
	got := func() string {
		sort.Strings(to)
		s := strings.Join(to, ",")
		to = nil
		return s
	}

	ev := AlertEvent{Trigger: TriggerSecretRevealed, OwnerKind: "secret", Agency: "ag-fin",
		Subject: "[Cronomicon] Secret FIN_TOKEN revealed in Finance by root@x", Detail: "Secret FIN_TOKEN of Finance was revealed."}
	if err := d.dispatch(context.Background(), alertDispatchEvent(ev)); err != nil {
		t.Fatal(err)
	}
	if g := got(); g != "fin@x,ops@x" {
		t.Errorf("FIN's reveal reached %q, want FIN's all-rule and Global's", g)
	}
	if strings.Contains(body, "Job:") {
		t.Errorf("the body names a job:\n%s", body)
	}

	ev = AlertEvent{Trigger: TriggerBackupKeyChanged, OwnerKind: "installation", Agency: "global", Subject: "keys"}
	if err := d.dispatch(context.Background(), alertDispatchEvent(ev)); err != nil {
		t.Fatal(err)
	}
	if g := got(); g != "keys@x" {
		t.Errorf("backup-key-changed reached %q, want Global's rule only", g)
	}
	for _, tr := range []string{TriggerSecretRevealed, TriggerSecretChanged, TriggerBackupKeyChanged} {
		if !TriggerHonoured(tr) {
			t.Errorf("TriggerHonoured(%q) = false", tr)
		}
	}
}

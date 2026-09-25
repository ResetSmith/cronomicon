package notify

import "testing"

// R2-4 — alert rules target a job by identity when they have one.
//
// alert_config carries no source column, so a rule naming "backup" has always
// been ambiguous in principle; the kind (job vs workflow) was separated for
// this reason once already. These pin the identity half, including the two
// cases where matching by name must SURVIVE.
func TestTargetMatchesByUID(t *testing.T) {
	for _, tc := range []struct {
		name                                       string
		mode, ruleJob, evJob, kind, ruleUID, evUID string
		want                                       bool
	}{
		{"uid rule matches its own job", "job", "backup", "backup", "job", "uid-a", "uid-a", true},
		// The case the whole stage exists for: same name, different job.
		{"uid rule ignores a same-named other job", "job", "backup", "backup", "job", "uid-a", "uid-b", false},
		// A rule that says "this exact job" stays quiet rather than guessing when
		// the event cannot say which job it was (history predating R2-1).
		{"uid rule does not fall back to the name", "job", "backup", "backup", "job", "uid-a", "", false},
		// Every pre-1040 rule, and every rule whose name was ambiguous at backfill.
		{"name rule still matches", "job", "backup", "backup", "job", "", "uid-a", true},
		{"name rule still rejects other names", "job", "backup", "other", "job", "", "uid-a", false},
		// Unchanged rules from earlier bands.
		{"job rule ignores a workflow", "job", "release", "release", "workflow", "", "", false},
		{"all mode matches anything", "all", "", "anything", "job", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := targetMatches(tc.mode, tc.ruleJob, tc.evJob, tc.kind, tc.ruleUID, tc.evUID); got != tc.want {
				t.Errorf("targetMatches = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUIDRuleFiresForTheRightJobEndToEnd drives the real dispatch, because the
// unit test above proves the predicate and not the plumbing — the event's uid
// is resolved inside dispatch, from the run row, and that resolution is the
// part that could silently return nothing.
func TestUIDRuleFiresForTheRightJobEndToEnd(t *testing.T) {
	d, pool := newDispatcherDB(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// Two jobs called 'backup', one per catalog. The rule targets the git one.
	exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('backup','git','uid-git','bash','t')`)
	exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('backup','amadeus','uid-ama','bash','t')`)
	exec(`INSERT INTO alert_config
		(id, target_mode, job_name, job_uid, trigger, channels, recipients, enabled, created_at)
		VALUES ('r-uid','job','backup','uid-git','failure','["email"]','ops@example.com',1,'now')`)
	exec(`INSERT INTO notification_config
		(id, smtp_host, smtp_port, smtp_from, smtp_encryption, smtp_recipients, apprise_enabled, apprise_targets, last_modified_at)
		VALUES (1,'mail.example.com',587,'cronomicon@example.com','starttls','[]',0,'[]','now')`)
	// A failing run of the AMADEUS job — same name, different identity.
	exec(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES('trace-ama','backup','amadeus','uid-ama','bash','failure','t','manual','t')`)
	// …and one of the git job, which the rule DOES target.
	exec(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES('trace-git','backup','git','uid-git','bash','failure','t','manual','t')`)

	sent := 0
	d.sendEmail = func(SMTP, []string, string, string) error { sent++; return nil }

	if err := d.dispatch(t.Context(), runDispatchEvent(RunEvent{
		TraceID: "trace-ama", JobName: "backup", Status: "failure",
	})); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if sent != 0 {
		t.Errorf("the rule fired for the OTHER job of the same name (%d sends)", sent)
	}

	if err := d.dispatch(t.Context(), runDispatchEvent(RunEvent{
		TraceID: "trace-git", JobName: "backup", Status: "failure",
	})); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if sent != 1 {
		t.Errorf("sends for the targeted job = %d, want 1", sent)
	}
}

// A missed-run alert has NO trace id — that is the point of a missed run — so
// the event's uid can only come from the name. Without that arm every
// uid-targeted rule would go silent for exactly the alerts that report that
// something did not happen.
func TestUIDRuleStillFiresForAnAlertWithNoRun(t *testing.T) {
	d, pool := newDispatcherDB(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('nightly','git','uid-nightly','bash','t')`)
	exec(`INSERT INTO alert_config
		(id, target_mode, job_name, job_uid, trigger, channels, recipients, enabled, created_at)
		VALUES ('r-missed','job','nightly','uid-nightly','missed-run','["email"]','ops@example.com',1,'now')`)
	exec(`INSERT INTO notification_config
		(id, smtp_host, smtp_port, smtp_from, smtp_encryption, smtp_recipients, apprise_enabled, apprise_targets, last_modified_at)
		VALUES (1,'mail.example.com',587,'cronomicon@example.com','starttls','[]',0,'[]','now')`)

	sent := 0
	d.sendEmail = func(SMTP, []string, string, string) error { sent++; return nil }
	if err := d.dispatch(t.Context(), dispatchEvent{
		jobName: "nightly", matchKey: TriggerMissedRun, isRunOutcome: false,
		subject: "missed", body: "no run appeared",
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if sent != 1 {
		t.Errorf("sends = %d, want 1 — a uid-targeted rule went silent for a run-less alert", sent)
	}
}

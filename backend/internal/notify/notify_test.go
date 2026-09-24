package notify

import (
	"bufio"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// fakeSMTP is a minimal in-process SMTP sink that captures one delivered message.
// It speaks just enough of the protocol for net/smtp with encryption="none".
func fakeSMTP(t *testing.T) (host string, port int, got *string, done *sync.WaitGroup) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	captured := new(string)
	var wg sync.WaitGroup
	wg.Go(func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		defer ln.Close()
		br := bufio.NewReader(conn)
		w := func(s string) { _, _ = conn.Write([]byte(s)) }
		w("220 mock ESMTP\r\n")
		inData := false
		var body strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					*captured = body.String()
					w("250 OK\r\n")
					continue
				}
				body.WriteString(line)
				continue
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				w("250 mock\r\n")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				w("250 OK\r\n")
			case strings.HasPrefix(cmd, "DATA"):
				inData = true
				w("354 end with .\r\n")
			case strings.HasPrefix(cmd, "QUIT"):
				w("221 bye\r\n")
				return
			default:
				w("250 OK\r\n")
			}
		}
	})
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	var portNum int
	_, _ = fmtSscan(p, &portNum)
	return h, portNum, captured, &wg
}

func fmtSscan(s string, p *int) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	*p = n
	return 1, nil
}

func TestSMTPSend(t *testing.T) {
	host, port, got, wg := fakeSMTP(t)
	s := SMTP{Host: host, Port: port, Encryption: "none", From: "amadeus@example.com", FromName: "Cronomicon"}
	if err := s.Send([]string{"ops@example.com"}, "[Cronomicon] job FAILED", "Job: nightly\nStatus: failure\n"); err != nil {
		t.Fatalf("send: %v", err)
	}
	wg.Wait()
	if !strings.Contains(*got, "Subject: [Cronomicon] job FAILED") {
		t.Errorf("captured message missing subject:\n%s", *got)
	}
	if !strings.Contains(*got, "To: ops@example.com") {
		t.Errorf("captured message missing recipient:\n%s", *got)
	}
	if !strings.Contains(*got, "Status: failure") {
		t.Errorf("captured message missing body:\n%s", *got)
	}
}

func TestAppriseSend(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/notify" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := Apprise{Gateway: srv.URL}
	if err := a.Send(context.Background(), []string{"slack://tok/chan", "mailto://x"}, "title", "body"); err != nil {
		t.Fatalf("apprise send: %v", err)
	}
	if !strings.Contains(gotBody, "slack://tok/chan,mailto://x") {
		t.Errorf("apprise payload missing joined targets: %s", gotBody)
	}

	// Non-2xx surfaces as an error (failures are not swallowed).
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()
	if err := (Apprise{Gateway: bad.URL}).Send(context.Background(), []string{"x://y"}, "t", "b"); err == nil {
		t.Error("expected error on non-2xx apprise response")
	}
}

func newDispatcherDB(t *testing.T) (*Dispatcher, *sql.DB) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	d := New(pool, &config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return d, pool
}

func TestDispatcherMatchAndSend(t *testing.T) {
	d, pool := newDispatcherDB(t)

	// Seed an enabled failure-trigger rule (all jobs, email + push channels).
	_, err := pool.Exec(`INSERT INTO alert_config
		(id, target_mode, job_name, trigger, channels, recipients, enabled, created_at)
		VALUES ('r1','all',NULL,'failure','["email","push"]','ops@example.com',1,'now')`)
	if err != nil {
		t.Fatalf("seed alert: %v", err)
	}
	// Seed notification_config: SMTP host + apprise targets.
	_, err = pool.Exec(`INSERT INTO notification_config
		(id, smtp_host, smtp_port, smtp_from, smtp_encryption, smtp_recipients, apprise_enabled, apprise_url, apprise_targets, last_modified_at)
		VALUES (1,'mail.example.com',587,'amadeus@example.com','starttls','[]',1,'http://apprise:8000','[{"label":"On-call","service":"slack","url":"slack://x","enabled":true}]','now')`)
	if err != nil {
		t.Fatalf("seed notif config: %v", err)
	}

	var gotEmailTo []string
	var gotPushTargets []string
	d.sendEmail = func(s SMTP, to []string, subject, body string) error {
		gotEmailTo = to
		if s.Host != "mail.example.com" {
			t.Errorf("smtp host = %q", s.Host)
		}
		return nil
	}
	d.sendPush = func(ctx context.Context, gateway string, targets []string, title, body string) error {
		gotPushTargets = targets
		if gateway != "http://apprise:8000" {
			t.Errorf("gateway = %q", gateway)
		}
		return nil
	}

	// Failure event → both transports fire.
	if err := d.dispatch(context.Background(), runDispatchEvent(RunEvent{TraceID: "t1", JobName: "nightly", Status: "failure"})); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(gotEmailTo) != 1 || gotEmailTo[0] != "ops@example.com" {
		t.Errorf("email recipients = %v", gotEmailTo)
	}
	if len(gotPushTargets) != 1 || gotPushTargets[0] != "slack://x" {
		t.Errorf("push targets = %v", gotPushTargets)
	}

	// Success event → the failure-only rule does NOT match, no sends.
	gotEmailTo, gotPushTargets = nil, nil
	if err := d.dispatch(context.Background(), runDispatchEvent(RunEvent{TraceID: "t2", JobName: "nightly", Status: "success"})); err != nil {
		t.Fatalf("dispatch success: %v", err)
	}
	if gotEmailTo != nil || gotPushTargets != nil {
		t.Errorf("success should not have dispatched: email=%v push=%v", gotEmailTo, gotPushTargets)
	}
}

func TestAppriseTargetURLs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"invalid", "{not json", nil},
		// The pre-rich bare-string form is NOT tolerated any more. It was
		// dropped from the settings reader in v1.5.41 and from this one in
		// v1.5.43; migration 1150 rewrites any stored row that still carries
		// it, so by the time the dispatcher reads the column there is nothing
		// in this shape left. Tolerating it HERE and not there is precisely the
		// split that let the UI show zero targets while alerts kept being
		// delivered — see DM-1.
		{"legacy strings are no longer delivered to", `["slack://x","mailto://y"]`, nil},
		{"objects honor enabled", `[{"url":"slack://x","enabled":true},{"url":"mailto://y","enabled":false}]`, []string{"slack://x"}},
		{"object without url skipped", `[{"label":"empty","enabled":true}]`, nil},
		{"mixed row keeps only the object", `["slack://legacy",{"url":"mailto://y","enabled":true}]`, []string{"mailto://y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appriseTargetURLs(tc.in)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("appriseTargetURLs(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestAppriseReadersAgree is the regression test for DM-1. The Settings card and
// the notification dispatcher read the SAME column through different code paths
// (settings.GetNotificationConfig vs the dispatcher's own SELECT), and for two
// releases they disagreed about the legacy bare-string form: the settings reader
// had dropped it while this package's private copy of the parser kept it, so a
// row in the old form was invisible in the UI, still delivered to, and wiped by
// the first Save.
//
// The guarantee is now structural — appriseTargetURLs calls
// settings.ParseAppriseTargets — and this test is what keeps it that way: it
// fails the moment anyone re-introduces a second parser with its own opinion.
func TestAppriseReadersAgree(t *testing.T) {
	fixtures := []string{
		"",
		"{not json",
		`[]`,
		`["slack://legacy"]`,
		`[{"label":"On-call","service":"email","url":"mailto://a@b.c","enabled":true}]`,
		`[{"url":"slack://on","enabled":true},{"url":"slack://off","enabled":false}]`,
		`["slack://legacy",{"url":"mailto://y","enabled":true}]`,
		`[{"label":"no url","enabled":true}]`,
	}
	for _, in := range fixtures {
		// What the Settings card would show, reduced to the same projection the
		// dispatcher applies: enabled targets that actually carry a URL.
		var viaSettings []string
		for _, tgt := range settings.ParseAppriseTargets(in) {
			if tgt.Enabled && tgt.URL != "" {
				viaSettings = append(viaSettings, tgt.URL)
			}
		}
		viaDispatcher := appriseTargetURLs(in)
		if strings.Join(viaSettings, ",") != strings.Join(viaDispatcher, ",") {
			t.Errorf("readers disagree on %s:\n  settings card  -> %v\n  dispatcher     -> %v\nOne column, one parser: route both through settings.ParseAppriseTargets.",
				in, viaSettings, viaDispatcher)
		}
	}
}

// TestChannelTransportHonoursAppriseAndEmailOnly pins the channel vocabulary the
// dispatcher acts on (VF-15 / J-3). The inline switch this replaced recognised
// email/smtp and push/apprise while the UI offered slack, webhook and in-app —
// so a rule created with only those persisted, displayed, and sent nothing.
func TestChannelTransportHonoursAppriseAndEmailOnly(t *testing.T) {
	cases := []struct {
		ch                string
		email, push, want bool
	}{
		{"email", true, false, true},
		{"smtp", true, false, true}, // config layer's older name
		{"apprise", false, true, true},
		{"push", false, true, true},      // config layer's older name
		{"EMAIL", true, false, true},     // matched case-insensitively
		{"slack", false, false, false},   // removed in v0.52.23
		{"webhook", false, false, false}, // removed in v0.52.23
		{"in-app", false, false, false},  // removed in v0.52.23 — never implemented
		{"", false, false, false},
	}
	for _, tc := range cases {
		email, push, ok := channelTransport(tc.ch)
		if ok != tc.want || email != tc.email || push != tc.push {
			t.Errorf("channelTransport(%q) = (email=%v, push=%v, ok=%v), want (%v, %v, %v)",
				tc.ch, email, push, ok, tc.email, tc.push, tc.want)
		}
		if ChannelHonoured(tc.ch) != tc.want {
			t.Errorf("ChannelHonoured(%q) = %v, want %v", tc.ch, !tc.want, tc.want)
		}
	}
}

// TestInertChannelSendsNothing is the behaviour the type-level test cannot show:
// a rule whose only channel has no transport matches the event and produces no
// delivery. It is the shape of the VF-15 defect, pinned so a future refactor
// cannot quietly make it a delivery instead.
func TestInertChannelSendsNothing(t *testing.T) {
	d, pool := newDispatcherDB(t)
	if _, err := pool.Exec(`INSERT INTO alert_config
		(id, target_mode, job_name, trigger, channels, recipients, enabled, created_at)
		VALUES ('legacy','all',NULL,'failure','["slack"]','ops@example.com',1,'now')`); err != nil {
		t.Fatalf("seed alert: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO notification_config
		(id, smtp_host, smtp_port, smtp_from, smtp_encryption, smtp_recipients, apprise_enabled, apprise_url, apprise_targets, last_modified_at)
		VALUES (1,'mail.example.com',587,'amadeus@example.com','starttls','[]',1,'http://apprise:8000','[{"label":"On-call","service":"slack","url":"slack://x","enabled":true}]','now')`); err != nil {
		t.Fatalf("seed notif config: %v", err)
	}
	sent := 0
	d.sendEmail = func(SMTP, []string, string, string) error { sent++; return nil }
	d.sendPush = func(context.Context, string, []string, string, string) error { sent++; return nil }

	if err := d.dispatch(context.Background(), runDispatchEvent(RunEvent{TraceID: "t1", JobName: "nightly", Status: "failure"})); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if sent != 0 {
		t.Errorf("a slack-only rule dispatched %d times; it has no transport and must send nothing", sent)
	}
}

// TestTriggerAndTargetHonoured pins the two predicates the API validates against
// and the J-6 guard compares to the spec.
func TestTriggerAndTargetHonoured(t *testing.T) {
	for _, tr := range []string{"success", "failure", "warning", "killed", "skipped", "any", "ALWAYS", "completion", "completed"} {
		if !TriggerHonoured(tr) {
			t.Errorf("TriggerHonoured(%q) = false, want true", tr)
		}
	}
	// `n-failures-window` was offered by the UI and accepted by the API for
	// months; triggerMatches compares the trigger to a run STATUS, so it never
	// matched anything (VF-15).
	for _, tr := range []string{"n-failures-window", "running", "queued", "", "job_failed"} {
		if TriggerHonoured(tr) {
			t.Errorf("TriggerHonoured(%q) = true, want false", tr)
		}
	}
	for _, m := range []string{"job", "all", "", "ALL"} {
		if !TargetModeHonoured(m) {
			t.Errorf("TargetModeHonoured(%q) = false, want true", m)
		}
	}
	// `tag` needs job tags on RunEvent, which it never had; `scope` is seeded by
	// the demo data and equally unmatched.
	for _, m := range []string{"tag", "scope", "agency"} {
		if TargetModeHonoured(m) {
			t.Errorf("TargetModeHonoured(%q) = true, want false", m)
		}
	}
}

// TestSendTestReportsPerTransport covers K-5.
//
// The button exists because Phase K's per-transport "last sent" line only filled
// in after a real run failed, so an operator setting up notifications had no way
// to ask "can this reach anyone?". What matters is that the report is honest
// about each transport separately — a half-configured install is the normal case,
// and "skipped: no recipients" tells the operator what to do where a bare
// failure does not.
func TestSendTestReportsPerTransport(t *testing.T) {
	byTransport := func(r TestReport, name string) TransportResult {
		t.Helper()
		for _, res := range r.Results {
			if res.Transport == name {
				return res
			}
		}
		t.Fatalf("report has no %s result: %+v", name, r.Results)
		return TransportResult{}
	}

	t.Run("both transports configured and working", func(t *testing.T) {
		d, pool := newDispatcherDB(t)
		if _, err := pool.Exec(`INSERT INTO notification_config
			(id, smtp_host, smtp_port, smtp_from, smtp_encryption, smtp_recipients, apprise_enabled, apprise_url, apprise_targets, last_modified_at)
			VALUES (1,'mail.example.com',587,'amadeus@example.com','starttls','["ops@example.com"]',1,'http://apprise:8000','[{"label":"On-call","service":"slack","url":"slack://x","enabled":true}]','now')`); err != nil {
			t.Fatalf("seed config: %v", err)
		}
		var mailedTo []string
		var pushed []string
		d.sendEmail = func(_ SMTP, to []string, subject, body string) error {
			mailedTo = to
			if subject == "" || body == "" {
				t.Error("test email has an empty subject or body")
			}
			return nil
		}
		d.sendPush = func(_ context.Context, _ string, targets []string, _, _ string) error {
			pushed = targets
			return nil
		}

		report, err := d.SendTest(context.Background())
		if err != nil {
			t.Fatalf("SendTest: %v", err)
		}
		if !report.AnySent() {
			t.Error("AnySent() = false with both transports working")
		}
		if email := byTransport(report, "email"); !email.Sent || email.Skipped {
			t.Errorf("email result = %+v, want sent", email)
		}
		if push := byTransport(report, "apprise"); !push.Sent || push.Skipped {
			t.Errorf("apprise result = %+v, want sent", push)
		}
		if len(mailedTo) != 1 || mailedTo[0] != "ops@example.com" {
			t.Errorf("mailed to %v, want the stored default recipients", mailedTo)
		}
		if len(pushed) != 1 || pushed[0] != "slack://x" {
			t.Errorf("pushed to %v, want the stored Apprise targets", pushed)
		}

		// A successful test stamps last-sent, because that field answers exactly
		// the question the button asks.
		var emailAt, emailStatus, appriseStatus sql.NullString
		if err := pool.QueryRow(`SELECT last_email_at, last_email_status, last_apprise_status FROM notification_config WHERE id=1`).
			Scan(&emailAt, &emailStatus, &appriseStatus); err != nil {
			t.Fatalf("read stamps: %v", err)
		}
		if !emailAt.Valid || emailAt.String == "" {
			t.Error("a successful test did not stamp last_email_at")
		}
		if emailStatus.String != "ok" || appriseStatus.String != "ok" {
			t.Errorf("stamps = email:%q apprise:%q, want ok/ok", emailStatus.String, appriseStatus.String)
		}
	})

	t.Run("nothing configured skips both with a reason", func(t *testing.T) {
		d, _ := newDispatcherDB(t)
		d.sendEmail = func(SMTP, []string, string, string) error { t.Error("sent email with no SMTP host"); return nil }
		d.sendPush = func(context.Context, string, []string, string, string) error {
			t.Error("pushed with no gateway")
			return nil
		}

		report, err := d.SendTest(context.Background())
		if err != nil {
			t.Fatalf("SendTest: %v", err)
		}
		if report.AnySent() {
			t.Error("AnySent() = true with nothing configured")
		}
		for _, name := range []string{"email", "apprise"} {
			res := byTransport(report, name)
			if !res.Skipped || res.Sent {
				t.Errorf("%s result = %+v, want skipped", name, res)
			}
			if res.Detail == "" {
				t.Errorf("%s was skipped with no reason — the reason is the useful part", name)
			}
		}
	})

	t.Run("an SMTP host with no recipients is skipped, not failed", func(t *testing.T) {
		// A rule's own recipients are deliberately not borrowed: a test must not
		// mail someone because an unrelated alert rule happens to name them.
		d, pool := newDispatcherDB(t)
		if _, err := pool.Exec(`INSERT INTO notification_config
			(id, smtp_host, smtp_port, smtp_from, smtp_encryption, smtp_recipients, apprise_targets, last_modified_at)
			VALUES (1,'mail.example.com',587,'amadeus@example.com','starttls','[]','[]','now')`); err != nil {
			t.Fatalf("seed config: %v", err)
		}
		if _, err := pool.Exec(`INSERT INTO alert_config
			(id, target_mode, job_name, trigger, channels, recipients, enabled, created_at)
			VALUES ('r1','all',NULL,'failure','["email"]','someone-elses@example.com',1,'now')`); err != nil {
			t.Fatalf("seed rule: %v", err)
		}
		d.sendEmail = func(_ SMTP, to []string, _, _ string) error {
			t.Errorf("sent to %v with no DEFAULT recipients configured", to)
			return nil
		}
		report, err := d.SendTest(context.Background())
		if err != nil {
			t.Fatalf("SendTest: %v", err)
		}
		if res := byTransport(report, "email"); !res.Skipped {
			t.Errorf("email result = %+v, want skipped", res)
		}
	})

	t.Run("a refused transport is reported, and stamped as an error", func(t *testing.T) {
		d, pool := newDispatcherDB(t)
		if _, err := pool.Exec(`INSERT INTO notification_config
			(id, smtp_host, smtp_port, smtp_from, smtp_encryption, smtp_recipients, apprise_targets, last_modified_at)
			VALUES (1,'mail.example.com',587,'amadeus@example.com','starttls','["ops@example.com"]','[]','now')`); err != nil {
			t.Fatalf("seed config: %v", err)
		}
		d.sendEmail = func(SMTP, []string, string, string) error { return errRefused }

		report, err := d.SendTest(context.Background())
		if err != nil {
			// A refused connection is an answer, not a server error.
			t.Fatalf("SendTest returned an error for a refused send: %v", err)
		}
		res := byTransport(report, "email")
		if res.Sent || res.Skipped {
			t.Errorf("email result = %+v, want a failure", res)
		}
		if res.Detail == "" {
			t.Error("a failed send reported no detail")
		}
		var status sql.NullString
		if err := pool.QueryRow(`SELECT last_email_status FROM notification_config WHERE id=1`).Scan(&status); err != nil {
			t.Fatalf("read stamp: %v", err)
		}
		if status.String != "error" {
			t.Errorf("last_email_status = %q, want error", status.String)
		}
	})
}

type refusedErr struct{}

func (refusedErr) Error() string { return "dial tcp: connection refused" }

var errRefused = refusedErr{}

// FX-D4 — a rule on `skipped` can finally fire, and an `any` rule still cannot
// be dragged into firing for one.
//
// `skipped` has been offered by the UI and accepted by the API since the feature
// shipped, and nothing could ever emit it: the only producers of a run event
// were the two executor paths, which by definition run only when something ran.
// An operator could author the rule, see it listed, and never hear from it —
// which looks exactly like a system that never suppressed anything.
//
// The `any` half is the reason this is not simply "emit it as a run outcome". A
// weekdays-only job suppresses ten times every weekend, so letting the `any`
// aliases cover suppressions would have turned every existing "notify me on any
// run" rule in the fleet into a weekend pager on the day it shipped.
func TestSuppressionNotifiesOnlyAnExplicitSkippedRule(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger string
		want    bool
	}{
		{"an explicit skipped rule fires", "skipped", true},
		{"an any rule does NOT", "any", false},
		{"a failure rule does not", "failure", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, pool := newDispatcherDB(t)
			if _, err := pool.Exec(`INSERT INTO alert_config
				(id, target_mode, job_name, trigger, channels, recipients, enabled, created_at)
				VALUES ('r1','all',NULL,?,'["email"]','ops@example.com',1,'now')`,
				tc.trigger); err != nil {
				t.Fatalf("seed alert: %v", err)
			}
			if _, err := pool.Exec(`INSERT INTO notification_config
				(id, smtp_host, smtp_port, smtp_from, smtp_encryption, smtp_recipients, apprise_enabled, apprise_url, apprise_targets, last_modified_at)
				VALUES (1,'mail.example.com',587,'a@example.com','starttls','[]',0,'','[]','now')`); err != nil {
				t.Fatalf("seed notif config: %v", err)
			}
			var sent bool
			var gotSubject, gotBody string
			d.sendEmail = func(_ SMTP, _ []string, subject, body string) error {
				sent, gotSubject, gotBody = true, subject, body
				return nil
			}

			if err := d.dispatch(context.Background(), runDispatchEvent(RunEvent{
				JobName: "nightly", Status: "skipped",
				Reason: `Skipped: suppressed by calendar "holidays" (Independence Day)`,
			})); err != nil {
				t.Fatalf("dispatch: %v", err)
			}

			if sent != tc.want {
				t.Fatalf("notification sent = %v, want %v for a %q rule", sent, tc.want, tc.trigger)
			}
			if !tc.want {
				return
			}
			// It must not claim the job completed, and it has no run to point at.
			if !strings.Contains(gotSubject, "did not run") {
				t.Errorf("subject = %q, want it to say the job did not run", gotSubject)
			}
			if !strings.Contains(gotBody, "Independence Day") {
				t.Errorf("body = %q, want it to carry the reason — a suppression has no exit "+
					"code to explain itself", gotBody)
			}
			if strings.Contains(gotBody, "Run:") {
				t.Errorf("body = %q names a run id, but nothing ran", gotBody)
			}
		})
	}
}

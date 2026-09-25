// Package notify implements notification dispatch (C.1): given a terminal run
// event and the operator-configured alert rules + notification settings, it
// sends via the right transport (SMTP email and/or Apprise push). Dispatch runs
// off the request/run path (async) so a slow mail server never blocks a run.
//
// The DB is the source of truth: alert_config (which runs alert, on which
// trigger, via which channels) and notification_config (the SMTP server +
// Apprise gateway). The SMTP password is stored envelope-encrypted and decrypted
// here via secrets.DecryptString — it never sits in plaintext config.
package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// RunEvent is the terminal-run signal the runner hands the dispatcher.
type RunEvent struct {
	TraceID string
	JobName string
	// JobUID is the job's permanent identity (R2-4), for rules that target one
	// exact job. Optional: a producer that does not hold it leaves it empty and
	// the rule falls back to matching by name.
	JobUID string
	Scope  string
	// OwnerKind distinguishes a workflow-owned event from a job-owned one.
	// Empty means job, so every existing caller keeps its behaviour. See
	// AlertEvent.OwnerKind for why name alone is not enough.
	OwnerKind string
	Status    string // "success" | "failure" | "skipped"
	// Reason carries WHY, for a status that has no exit code to explain itself.
	// Set for suppressions ("Skipped: the job is paused"); empty for executions,
	// which are explained by their status and exit code.
	Reason   string
	ExitCode *int
}

// Alert triggers that are NOT run outcomes (SL).
//
// Until these, every notification in Cronomicon described a run that had finished:
// RunEvent.Status was matched against runs.status, and `running` was explicitly
// not an honoured trigger. Both of these fire while nothing has terminated —
// one mid-run, one for a run that never started at all — so they are matched by
// NAME rather than by outcome, and the `any` aliases deliberately do NOT cover
// them. An operator with an "any" rule asked to hear about run outcomes; they
// did not ask to be paged because a nightly job is running long.
const (
	// TriggerSLABreach — a run is still going past its warn_after_seconds or its
	// must_finish_by wall-clock deadline. The run is NOT touched; timeout_seconds
	// remains the only thing that kills anything.
	TriggerSLABreach = "sla-breach"
	// TriggerMissedRun — a schedule expected a fire and no run appeared, with no
	// suppression on record to explain it.
	TriggerMissedRun = "missed-run"
)

// alertTriggers is the closed set, and the single source the guard and the
// dispatcher both read.
var alertTriggers = []string{TriggerSLABreach, TriggerMissedRun}

// AlertEvent is a non-run-terminal alert.
//
// TraceID is empty for a missed run — there is no run, which is the entire
// point — so consumers must not assume it is set.
type AlertEvent struct {
	Trigger string // one of the Trigger* constants above
	JobName string
	// JobUID — see RunEvent.JobUID.
	JobUID  string
	Scope   string
	TraceID string
	// OwnerKind distinguishes a workflow-owned alert from a job-owned one.
	// Empty means job, so every existing caller keeps its behaviour.
	//
	// It exists because target_mode='job' rules match by NAME alone: without a
	// kind, a rule watching JOB "release" also fired for WORKFLOW "release", and
	// a workflow alert could not be targeted at all except through mode 'all'
	// (FX-B3).
	OwnerKind string
	// Subject/Detail carry the already-composed human sentence. The dispatcher
	// does not re-derive it: the scanner that raised the alert knows why, and
	// re-computing the reason here would mean a second copy of that logic.
	Subject string
	Detail  string
}

// Notifier is the seam the runner and the scheduler depend on (nil ⇒ no
// dispatch).
type Notifier interface {
	RunEnded(ev RunEvent)
	AlertRaised(ev AlertEvent)
}

// Dispatcher is the concrete Notifier. The send* fields are transport seams with
// real defaults; tests override them to capture without real network I/O.
type Dispatcher struct {
	db  *sql.DB
	cfg *config.Config
	log *slog.Logger

	sendEmail func(s SMTP, to []string, subject, body string) error
	sendPush  func(ctx context.Context, gateway string, targets []string, title, body string) error

	// wg, when set, tracks the async RunEnded dispatch goroutines so graceful
	// shutdown can drain them before the DB pool closes (PP-L15) — the dispatch
	// reads the shared pool, so an in-flight terminal notification must not race
	// pool.Close().
	wg *sync.WaitGroup
}

// WithShutdownWG registers a WaitGroup the async RunEnded dispatch joins, so the
// process (or a test) can wait for in-flight notifications before closing the
// DB pool (PP-L15). Chainable.
func (d *Dispatcher) WithShutdownWG(wg *sync.WaitGroup) *Dispatcher {
	d.wg = wg
	return d
}

// New builds a Dispatcher with the real SMTP/Apprise transports.
func New(db *sql.DB, cfg *config.Config, log *slog.Logger) *Dispatcher {
	// SU-7: the Apprise push egresses through the SSRF-guarded dialer so an
	// operator-configured gateway URL can't be pointed at internal/metadata
	// hosts. Policy comes from config (private allowed by default; loopback not).
	pushClient := httpx.SafeClient(15*time.Second, httpx.EgressPolicy{
		AllowPrivate:  cfg.OutboundAllowPrivate,
		AllowLoopback: cfg.OutboundAllowLoopback,
	})
	return &Dispatcher{
		db:  db,
		cfg: cfg,
		log: log,
		sendEmail: func(s SMTP, to []string, subject, body string) error {
			return s.Send(to, subject, body)
		},
		sendPush: func(ctx context.Context, gateway string, targets []string, title, body string) error {
			return Apprise{Gateway: gateway, Client: pushClient}.Send(ctx, targets, title, body)
		},
	}
}

// RunEnded dispatches asynchronously so the run-finalize path is never blocked.
// When a shutdown WaitGroup is registered, the dispatch is tracked so the pool
// isn't closed out from under an in-flight terminal notification (PP-L15).
func (d *Dispatcher) RunEnded(ev RunEvent) { d.send(runDispatchEvent(ev)) }

// runDispatchEvent converts a terminal run into the dispatcher's internal shape.
// Extracted so tests that drive dispatch directly exercise the same conversion
// production does, rather than hand-building a dispatchEvent that could drift.
func runDispatchEvent(ev RunEvent) dispatchEvent {
	subject, body := renderMessage(ev)
	// FX-D4 — a SUPPRESSION is matched by name only, never by the `any` aliases.
	//
	// isRunOutcome=false is what enforces that. An operator whose rule says
	// "notify me on any run" asked about runs that RAN; a calendar veto is the
	// absence of a run, and a weekdays-only job suppresses ten times every
	// weekend. Letting `any` cover suppressions would have converted every
	// existing rule in the fleet into a weekend pager the day this shipped —
	// the same trap the comment on triggerMatches describes for SLA alerts.
	return dispatchEvent{
		traceID: ev.TraceID, jobName: ev.JobName, jobUID: ev.JobUID, scope: ev.Scope, ownerKind: ev.OwnerKind,
		matchKey: ev.Status, isRunOutcome: ev.Status != "skipped", subject: subject, body: body,
	}
}

// AlertRaised dispatches a non-run-terminal alert (SL) through the same rules,
// transports and per-transport delivery stamps as a run notification. Only the
// matching differs — see dispatchEvent.isRunOutcome.
func (d *Dispatcher) AlertRaised(ev AlertEvent) {
	subject, body := renderAlert(ev)
	d.send(dispatchEvent{
		traceID: ev.TraceID, jobName: ev.JobName, jobUID: ev.JobUID, scope: ev.Scope, ownerKind: ev.OwnerKind,
		matchKey: ev.Trigger, isRunOutcome: false, subject: subject, body: body,
	})
}

// dispatchEvent is what the dispatcher actually works on: a subject, a body, and
// enough identity to decide which rules match.
//
// isRunOutcome is the load-bearing field. matchKey is a runs.status for a run
// outcome and a trigger NAME for an alert, and the `any` aliases apply only to
// the former — so an "any" rule keeps meaning "every run outcome" rather than
// silently widening to include SLA breaches the day this shipped.
type dispatchEvent struct {
	traceID string
	jobName string
	// jobUid is the fired job's permanent identity (R2-4). Empty for a workflow
	// event, for history predating R2-1, and for a missed-run alert whose job
	// has since gone — all cases where a uid-targeted rule correctly stays quiet.
	jobUID       string
	ownerKind    string // "" or "job" ⇒ job-owned; "workflow" ⇒ workflow-owned
	scope        string
	matchKey     string
	isRunOutcome bool
	subject      string
	body         string
}

// send runs the dispatch asynchronously so neither the run-finalize path nor the
// scheduler's scan loop is ever blocked. When a shutdown WaitGroup is
// registered, the dispatch is tracked so the pool isn't closed out from under an
// in-flight notification (PP-L15).
func (d *Dispatcher) send(ev dispatchEvent) {
	if d.wg != nil {
		d.wg.Add(1) // synchronous Add so it can't race a concurrent Wait()
	}
	go func() {
		if d.wg != nil {
			defer d.wg.Done()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := d.dispatch(ctx, ev); err != nil {
			d.log.Error("notification dispatch failed", "trace_id", ev.traceID, "error", err)
		}
	}()
}

// notifConfig is the resolved notification_config row.
type notifConfig struct {
	smtp           SMTP
	smtpRecipients []string
	appriseEnabled bool
	appriseGateway string
	appriseTargets []string
}

func (d *Dispatcher) dispatch(ctx context.Context, ev dispatchEvent) error {
	ev.jobUID = d.resolveEventJobUID(ctx, ev)
	rules, err := d.matchingRules(ctx, ev)
	if err != nil {
		return fmt.Errorf("load alert rules: %w", err)
	}
	if len(rules) == 0 {
		return nil // nothing configured to alert on this event
	}

	nc, err := d.loadConfig(ctx)
	if err != nil {
		return fmt.Errorf("load notification config: %w", err)
	}

	wantEmail, wantPush := false, false
	recipients := map[string]struct{}{}
	for _, r := range rules {
		for _, ch := range r.channels {
			if email, push, ok := channelTransport(ch); ok {
				wantEmail = wantEmail || email
				wantPush = wantPush || push
			} else {
				// Only reachable for a rule stored before v0.52.23 — the API
				// refuses these now. Say so once, because the alternative is a
				// rule that silently sends nothing (VF-15).
				d.log.Warn("alert rule names a channel with no transport; ignoring",
					"channel", ch, "trace_id", ev.traceID)
			}
		}
		for _, rcpt := range splitRecipients(r.recipients) {
			recipients[rcpt] = struct{}{}
		}
	}
	for _, rcpt := range nc.smtpRecipients {
		recipients[rcpt] = struct{}{}
	}

	// AN-4 — "it failed, and here is who to call". Appended HERE rather than in
	// the renderers: renderMessage/renderAlert are pure and hold no db, and both
	// have already run by the time an event reaches this function. One site
	// therefore covers run outcomes and alerts alike.
	//
	// Below the rules check on purpose: an event nobody subscribed to returns
	// above, so an un-alerted job never pays for this lookup.
	subject, body := ev.subject, ev.body+d.annotationLines(ctx, ev)

	if wantEmail {
		to := keys(recipients)
		if len(to) == 0 {
			d.log.Warn("alert matched email channel but no recipients configured", "trace_id", ev.traceID)
		} else if err := d.sendEmail(nc.smtp, to, subject, body); err != nil {
			d.log.Error("email send failed", "trace_id", ev.traceID, "error", err)
			d.markTransport(ctx, "email", "error")
		} else {
			d.log.Info("alert email sent", "trace_id", ev.traceID, "recipients", len(to))
			d.markTransport(ctx, "email", "ok")
		}
	}

	if wantPush {
		gateway := nc.appriseGateway
		if gateway == "" {
			gateway = d.cfg.AppriseURL
		}
		switch {
		case !nc.appriseEnabled && d.cfg.AppriseURL == "":
			d.log.Warn("alert matched push channel but Apprise is not configured", "trace_id", ev.traceID)
		case len(nc.appriseTargets) == 0:
			d.log.Warn("alert matched push channel but no Apprise targets configured", "trace_id", ev.traceID)
		default:
			if err := d.sendPush(ctx, gateway, nc.appriseTargets, subject, body); err != nil {
				d.log.Error("apprise push failed", "trace_id", ev.traceID, "error", err)
				d.markTransport(ctx, "apprise", "error")
			} else {
				d.log.Info("alert push sent", "trace_id", ev.traceID, "targets", len(nc.appriseTargets))
				d.markTransport(ctx, "apprise", "ok")
			}
		}
	}
	return nil
}

// matchedRule is the subset of an alert rule the dispatcher needs.
type matchedRule struct {
	channels   []string
	recipients string
}

// matchingRules returns enabled rules whose trigger + target match the event.
func (d *Dispatcher) matchingRules(ctx context.Context, ev dispatchEvent) ([]matchedRule, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT target_mode, job_name, trigger, channels, recipients, job_uid
		FROM alert_config WHERE enabled = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []matchedRule
	for rows.Next() {
		var targetMode, trigger, channelsJSON string
		var jobName, recipients, jobUID sql.NullString
		if err := rows.Scan(&targetMode, &jobName, &trigger, &channelsJSON, &recipients, &jobUID); err != nil {
			return nil, err
		}
		if !triggerMatches(trigger, ev.matchKey, ev.isRunOutcome) {
			continue
		}
		if !targetMatches(targetMode, jobName.String, ev.jobName, ev.ownerKind, jobUID.String, ev.jobUID) {
			continue
		}
		var channels []string
		_ = json.Unmarshal([]byte(channelsJSON), &channels)
		out = append(out, matchedRule{channels: channels, recipients: recipients.String})
	}
	return out, rows.Err()
}

func (d *Dispatcher) loadConfig(ctx context.Context) (notifConfig, error) {
	var nc notifConfig
	var host, from, fromName, username, pwEnc, encryption, appriseURL sql.NullString
	var recipientsJSON, appriseTargetsJSON sql.NullString
	var port sql.NullInt64
	var appriseEnabled sql.NullInt64
	err := d.db.QueryRowContext(ctx, `
		SELECT smtp_host, smtp_port, smtp_from, smtp_from_name, smtp_username, smtp_password_enc,
		       smtp_encryption, smtp_recipients, apprise_enabled, apprise_url, apprise_targets
		FROM notification_config WHERE id = 1`).
		Scan(&host, &port, &from, &fromName, &username, &pwEnc, &encryption,
			&recipientsJSON, &appriseEnabled, &appriseURL, &appriseTargetsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nc, nil
	}
	if err != nil {
		return nc, err
	}
	nc.smtp = SMTP{
		Host:       host.String,
		Port:       int(port.Int64),
		Encryption: encryption.String,
		Username:   username.String,
		From:       from.String,
		FromName:   fromName.String,
	}
	if pwEnc.Valid && pwEnc.String != "" {
		pw, err := secrets.DecryptString(d.cfg, pwEnc.String)
		if err != nil {
			d.log.Error("decrypt SMTP password failed; sending unauthenticated", "error", err)
		} else {
			nc.smtp.Password = pw
		}
	}
	nc.smtpRecipients = parseJSONList(recipientsJSON.String)
	nc.appriseEnabled = appriseEnabled.Int64 == 1
	nc.appriseGateway = appriseURL.String
	nc.appriseTargets = appriseTargetURLs(appriseTargetsJSON.String)
	return nc, nil
}

// markTransport best-effort records when a transport last sent, and whether it
// worked, on the notification_config singleton (K-2 / migration 740).
//
// This replaced markDestinations, which stamped last_fired_at on every enabled
// row of alert_destinations whose type matched. That was misleading in a way
// worth remembering: no destination was ever a delivery target — routing came
// from notification_config plus each rule's channels — so "last fired" on a
// destination only ever meant "something sent over this transport". Now it says
// exactly that, about the thing that actually sends.
//
// Upsert rather than update: a install that has never opened Settings has no
// notification_config row, and the transport can still have sent (an Apprise
// gateway configured by CRONOMICON_APPRISE_URL needs no row at all).
func (d *Dispatcher) markTransport(ctx context.Context, transport, status string) {
	var atCol, statusCol string
	switch transport {
	case "email":
		atCol, statusCol = "last_email_at", "last_email_status"
	case "apprise":
		atCol, statusCol = "last_apprise_at", "last_apprise_status"
	default:
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO notification_config (id, `+atCol+`, `+statusCol+`) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET `+atCol+` = excluded.`+atCol+`, `+statusCol+` = excluded.`+statusCol,
		now, status)
	if err != nil {
		d.log.Warn("record transport send failed", "transport", transport, "error", err)
	}
}

// ── Test send (K-5) ───────────────────────────────────────────────────────────
//
// Phase K left a gap it could not close from the outside: the per-transport
// "last sent" line only fills in after a real run fails, so an operator setting
// up notifications had no way to answer "can this reach anyone?" — and neither
// did the verification of K-2 itself, which needed a throwaway helper to stamp
// the columns. This closes it.
//
// Deliberately parameterless. Recipients, gateway and targets all come from
// stored config, never from the request, so the button cannot be turned into a
// relay for arbitrary addresses by anyone who reaches the endpoint. The Apprise
// leg reuses the same SSRF-guarded client as a real dispatch (SU-7), so this
// adds no egress an operator did not already have.

// TransportResult is one transport's outcome from a test send. Skipped carries
// the reason a transport was not attempted, which is the answer most of the time
// on a half-configured install and is more useful than a bare failure.
type TransportResult struct {
	Transport string `json:"transport"` // "email" | "apprise"
	Sent      bool   `json:"sent"`
	Skipped   bool   `json:"skipped"`
	Detail    string `json:"detail"`
}

// TestReport is the result of a test send across every configured transport.
type TestReport struct {
	Results []TransportResult `json:"results"`
}

// AnySent reports whether at least one transport delivered.
func (r TestReport) AnySent() bool {
	for _, res := range r.Results {
		if res.Sent {
			return true
		}
	}
	return false
}

// SendTest delivers a fixed test message over every configured transport and
// reports per-transport outcomes. A successful leg stamps the transport's
// last-sent columns exactly as a real alert does — the field answers "when did
// this transport last reach anyone", and a test send is a real answer to that
// question. The alternative (a successful test that leaves the line empty) is
// the confusing one.
func (d *Dispatcher) SendTest(ctx context.Context) (TestReport, error) {
	nc, err := d.loadConfig(ctx)
	if err != nil {
		return TestReport{}, fmt.Errorf("load notification config: %w", err)
	}
	const subject = "[Cronomicon] Test notification"
	body := "This is a test notification from Cronomicon.\n\n" +
		"It was sent from Settings → Notifications and is not tied to a run. If you " +
		"received it, this transport can reach you.\n"

	report := TestReport{}

	// ── email ────────────────────────────────────────────────────────────────
	to := nc.smtpRecipients
	switch {
	case nc.smtp.Host == "":
		report.Results = append(report.Results, TransportResult{
			Transport: "email", Skipped: true,
			Detail: "No SMTP host is configured.",
		})
	case len(to) == 0:
		// A rule's own recipients are deliberately NOT borrowed here: a test must
		// not mail someone because an unrelated alert rule names them.
		report.Results = append(report.Results, TransportResult{
			Transport: "email", Skipped: true,
			Detail: "No default recipients are configured — a test send has no rule to borrow addresses from.",
		})
	default:
		if err := d.sendEmail(nc.smtp, to, subject, body); err != nil {
			d.log.Error("test email failed", "error", err)
			d.markTransport(ctx, "email", "error")
			report.Results = append(report.Results, TransportResult{
				Transport: "email", Detail: err.Error(),
			})
		} else {
			d.markTransport(ctx, "email", "ok")
			report.Results = append(report.Results, TransportResult{
				Transport: "email", Sent: true,
				Detail: fmt.Sprintf("Sent to %d recipient(s).", len(to)),
			})
		}
	}

	// ── apprise ──────────────────────────────────────────────────────────────
	gateway := nc.appriseGateway
	if gateway == "" {
		gateway = d.cfg.AppriseURL
	}
	switch {
	case gateway == "":
		report.Results = append(report.Results, TransportResult{
			Transport: "apprise", Skipped: true,
			Detail: "No Apprise gateway is configured.",
		})
	case !nc.appriseEnabled && d.cfg.AppriseURL == "":
		report.Results = append(report.Results, TransportResult{
			Transport: "apprise", Skipped: true,
			Detail: "Apprise is switched off.",
		})
	case len(nc.appriseTargets) == 0:
		report.Results = append(report.Results, TransportResult{
			Transport: "apprise", Skipped: true,
			Detail: "No Apprise targets are configured.",
		})
	default:
		if err := d.sendPush(ctx, gateway, nc.appriseTargets, subject, body); err != nil {
			d.log.Error("test push failed", "error", err)
			d.markTransport(ctx, "apprise", "error")
			report.Results = append(report.Results, TransportResult{
				Transport: "apprise", Detail: err.Error(),
			})
		} else {
			d.markTransport(ctx, "apprise", "ok")
			report.Results = append(report.Results, TransportResult{
				Transport: "apprise", Sent: true,
				Detail: fmt.Sprintf("Sent to %d target(s).", len(nc.appriseTargets)),
			})
		}
	}
	return report, nil
}

// ── What this dispatcher honours (VF-15 / J-3) ────────────────────────────────
//
// These three predicates are the single source of truth for which stored rule
// values can ever produce a notification, and the API validates new rules
// against them so a caller cannot create a rule that is inert by construction.
// TestAlertEnumsAreHonoured (internal/api) pins them against openapi.yaml's
// AlertRuleInput enums in both directions — a spec value with no branch here,
// or a branch here that the spec forbids, fails the build.
//
// Why this exists: `tag` targeting, the `n-failures-window` trigger and the
// slack/webhook/in-app channels were all offered by the UI and accepted by the
// API for months while matching nothing and sending nothing. Silence is the one
// failure mode a notification system cannot afford, because a rule that never
// fires looks exactly like a system that never failed.

// terminalRunStatuses is the set a run can end in — the CHECK constraint on
// runs.status (db/migrations/001_execution.up.sql). A trigger is matched against
// this value, so anything outside the set (plus the aliases below) is unmatchable.
var terminalRunStatuses = []string{"success", "failure", "warning", "killed", "skipped"}

// anyRunAliases all mean "notify on every run, whatever the outcome".
var anyRunAliases = []string{"any", "always", "completion", "completed"}

// TriggerHonoured reports whether a stored trigger can ever match a run.
func TriggerHonoured(trigger string) bool {
	t := strings.ToLower(trigger)
	if slices.Contains(anyRunAliases, t) {
		return true
	}
	if slices.Contains(terminalRunStatuses, t) {
		return true
	}
	return slices.Contains(alertTriggers, t)
}

// TargetModeHonoured reports whether a stored target mode can ever match a run.
// `tag` is absent deliberately: RunEvent carries no tags, so it never matched.
func TargetModeHonoured(mode string) bool {
	switch strings.ToLower(mode) {
	case "all", "", "job":
		return true
	default:
		return false
	}
}

// ChannelHonoured reports whether a stored channel reaches a transport. The
// pairs are aliases: the UI writes `email`/`apprise`, older rows and the config
// layer use `smtp`/`push`.
func ChannelHonoured(ch string) bool {
	_, _, ok := channelTransport(ch)
	return ok
}

// channelTransport maps a channel onto the transports it selects. Extracted from
// the inline switch in dispatch so validation, the guard and the dispatcher
// cannot disagree about what a channel means.
func channelTransport(ch string) (email, push, ok bool) {
	switch strings.ToLower(ch) {
	case "email", "smtp":
		return true, false, true
	case "push", "apprise":
		return false, true, true
	default:
		return false, false, false
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// triggerMatches decides whether one rule's trigger covers this event.
//
// The `any` aliases mean "any RUN OUTCOME" and are deliberately confined to run
// events. Letting them cover alerts too would have silently converted every
// existing "notify me on any run" rule into an SLA pager the moment SL shipped
// — a behavior change nobody asked for, delivered at 3am.
func triggerMatches(trigger, matchKey string, isRunOutcome bool) bool {
	if !isRunOutcome {
		return strings.EqualFold(trigger, matchKey)
	}
	switch strings.ToLower(trigger) {
	case "any", "always", "completion", "completed":
		return true
	default:
		return strings.EqualFold(trigger, matchKey)
	}
}

func targetMatches(mode, ruleJob, eventJob, ownerKind, ruleUID, eventUID string) bool {
	switch strings.ToLower(mode) {
	case "all", "":
		return true
	case "job":
		// A job-targeted rule matches JOBS only. Jobs and workflows are separate
		// namespaces that may share a name, so matching on the name alone made a
		// rule about one fire for the other.
		if ownerKind != "" && !strings.EqualFold(ownerKind, "job") {
			return false
		}
		// R2-4 — when the RULE names an identity, that is the whole test: it is
		// the narrower statement of intent and the only one that keeps meaning
		// one job once a name may belong to several. An event with no uid
		// (history predating R2-1, or a job since deleted) does NOT match such a
		// rule, which is deliberate — a rule that says "this exact job" should
		// stay silent rather than guess from a name.
		if ruleUID != "" {
			return eventUID != "" && ruleUID == eventUID
		}
		// No identity on the rule: match by name, exactly as before. This is the
		// state every pre-1040 rule is in, and rules whose name was ambiguous at
		// backfill time stay here on purpose.
		return ruleJob != "" && strings.EqualFold(ruleJob, eventJob)
	default:
		// tag/other modes need data not present in the event; don't match.
		return false
	}
}

func renderMessage(ev RunEvent) (subject, body string) {
	verb := "completed"
	switch {
	case strings.EqualFold(ev.Status, "failure"):
		verb = "FAILED"
	case strings.EqualFold(ev.Status, "skipped"):
		// FX-D4 — a suppression did not complete anything, and saying so is the
		// whole content of the message. It also has no run to point at: nothing
		// executed, which is the point.
		verb = "did not run"
	}
	label := "Job"
	if strings.EqualFold(ev.OwnerKind, "workflow") {
		label = "Workflow"
	}
	subject = fmt.Sprintf("[Cronomicon] %s %s", ev.JobName, verb)
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", label, ev.JobName)
	fmt.Fprintf(&b, "Status: %s\n", ev.Status)
	if ev.Reason != "" {
		fmt.Fprintf(&b, "Reason: %s\n", ev.Reason)
	}
	if ev.Scope != "" {
		fmt.Fprintf(&b, "Scope:  %s\n", ev.Scope)
	}
	if ev.ExitCode != nil {
		fmt.Fprintf(&b, "Exit:   %d\n", *ev.ExitCode)
	}
	if ev.TraceID != "" {
		fmt.Fprintf(&b, "Run:    %s\n", ev.TraceID)
	}
	return subject, b.String()
}

func splitRecipients(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseJSONList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// appriseTargetURLs reads the apprise_targets column and returns the URLs of the
// enabled targets only. A disabled target is skipped so the per-target toggle in
// the UI actually suppresses delivery.
//
// The PARSE is settings.ParseAppriseTargets — the same function the Settings
// card reads the column with. This package had its own copy until v1.5.43, and
// the two drifted the moment one was edited: v1.5.41 dropped the pre-rich
// bare-string form from the settings reader only, so a row still holding it
// showed zero targets in the UI while every alert kept being delivered here,
// and the first Save silently wiped it. One parser is the fix; this function is
// now only the enabled-filter and the URL projection.
func appriseTargetURLs(s string) []string {
	var out []string
	for _, t := range settings.ParseAppriseTargets(s) {
		if t.Enabled && t.URL != "" {
			out = append(out, t.URL)
		}
	}
	return out
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// renderAlert composes the message for a non-run-terminal alert. The scanner
// that raised it supplies the sentence; this only dresses it.
func renderAlert(ev AlertEvent) (subject, body string) {
	subject = ev.Subject
	if subject == "" {
		subject = fmt.Sprintf("[Cronomicon] %s — %s", ev.JobName, ev.Trigger)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Alert:  %s\n", ev.Trigger)
	fmt.Fprintf(&b, "Job:    %s\n", ev.JobName)
	if ev.Scope != "" {
		fmt.Fprintf(&b, "Scope:  %s\n", ev.Scope)
	}
	if ev.Detail != "" {
		fmt.Fprintf(&b, "\n%s\n", ev.Detail)
	}
	if ev.TraceID != "" {
		fmt.Fprintf(&b, "\nRun:    %s\n", ev.TraceID)
	}
	return subject, b.String()
}

// annotationLines returns the operator-annotation tail appended to a
// notification body (AN-4), or "" when the definition carries no annotation.
//
//	Critical: yes
//	Contact:  dba-oncall@corp.example
//
// Each line is conditional: a job that is merely annotated with notes adds
// nothing here, because a notification is not the place to read them — the two
// facts worth pushing are "this one matters" and "here is who to call".
//
// SUBJECT UNTOUCHED (AN-Q4). Operators keep mail filters keyed on the existing
// `[Cronomicon] <name> FAILED` shape, and a silent subject change breaks those at
// 3am — the same lesson the FX-D4 comment records for suppression wording. A
// `[CRITICAL]` prefix is a one-line follow-up if it is ever asked for, as an
// opt-in.
//
// FAILURE-OPEN throughout: every error path returns "" and the un-enriched
// message still goes out. An annotation must never cost a notification.
func (d *Dispatcher) annotationLines(ctx context.Context, ev dispatchEvent) string {
	kind, uid := d.resolveAnnotationOwner(ctx, ev)
	if uid == "" {
		return ""
	}
	var (
		critical int
		contact  string
	)
	if err := d.db.QueryRowContext(ctx,
		`SELECT critical, contact FROM annotations WHERE owner_kind = ? AND owner_uid = ?`,
		kind, uid).Scan(&critical, &contact); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			d.log.Warn("annotation lookup failed; sending un-enriched notification",
				"trace_id", ev.traceID, "kind", kind, "error", err)
		}
		return ""
	}
	var b strings.Builder
	if critical == 1 {
		b.WriteString("\nCritical: yes")
	}
	if contact != "" {
		fmt.Fprintf(&b, "\nContact:  %s", contact)
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String() + "\n"
}

// resolveAnnotationOwner names the definition whose annotation belongs on this
// event: ("job"|"workflow", uid), or an empty uid when it cannot be established.
//
// TWO THINGS THIS DELIBERATELY DOES NOT DO.
//
// It does not re-derive the job uid. dispatch has already assigned
// ev.jobUID = resolveEventJobUID(...) by the time anything calls this, and that
// resolver's NAME arm is load-bearing: an SLA breach and a missed run have no
// trace id — there is no run, that is the point — so without it the contact line
// would be absent from exactly the alerts that report something did not happen.
// The arm is gated on the name being unique across the catalog, so it declines
// when ambiguous rather than guessing; that is the R2F-1 twin-safety property,
// reached by uniqueness rather than by refusing to look.
//
// And it does not write back into ev.jobUID. That field feeds targetMatches,
// where a workflow event holding a uid is precisely the confusion notify.go's
// "job rule matches JOBS only" arm exists to prevent. The workflow identity
// resolved here is for this lookup and goes no further — which is why the
// function returns it rather than assigning it.
func (d *Dispatcher) resolveAnnotationOwner(ctx context.Context, ev dispatchEvent) (string, string) {
	if !strings.EqualFold(ev.ownerKind, "workflow") {
		return "job", ev.jobUID
	}
	// Workflow events carry no uid at all: resolveEventJobUID returns "" for
	// them (correctly — a workflow must not borrow a same-named job's identity),
	// and dispatchEvent has no workflow uid field. Without this arm an annotated
	// critical WORKFLOW would fail with a body byte-identical to an un-annotated
	// one, while its row in the catalog wears a Critical chip — the twin
	// asymmetry that keeps recurring in this area.
	//
	// Same uniqueness gate as the job arm, for the same reason.
	if ev.jobName == "" {
		return "workflow", ""
	}
	var uid sql.NullString
	if err := d.db.QueryRowContext(ctx,
		`SELECT w.uid FROM workflows w WHERE w.name = ?
		   AND (SELECT COUNT(*) FROM workflows w2 WHERE w2.name = w.name) = 1`,
		ev.jobName).Scan(&uid); err != nil {
		return "workflow", ""
	}
	return "workflow", uid.String
}

// resolveEventJobUID fills in the fired job's identity for an event that did
// not carry one (R2-4).
//
// Done HERE rather than at each of the nine producers for the same reason the
// activity writer resolves its own uid: the producers all hold either a trace
// id or a job name already, and threading a new field through every one of them
// invites exactly the drift this band keeps finding. Two sources, in order:
//
//   - the RUN, which has carried job_uid since R2-1 and knows its own source.
//     This is the exact answer.
//   - the NAME, but only when it is unambiguous across both catalogs. This arm
//     is not optional: an SLA breach and a missed run have NO trace id (there
//     is no run — that is the point of a missed run), so without it every
//     uid-targeted rule would go silent for precisely the alerts that say
//     something did not happen.
//
// Unresolvable stays empty, and an empty event uid simply never matches a
// uid-targeted rule.
func (d *Dispatcher) resolveEventJobUID(ctx context.Context, ev dispatchEvent) string {
	if ev.jobUID != "" {
		return ev.jobUID
	}
	// A workflow event has no job identity to resolve, and must not borrow one
	// from a same-named job.
	if ev.ownerKind != "" && !strings.EqualFold(ev.ownerKind, "job") {
		return ""
	}
	if ev.traceID != "" {
		var uid sql.NullString
		if err := d.db.QueryRowContext(ctx,
			`SELECT job_uid FROM runs WHERE id = ?`, ev.traceID).Scan(&uid); err == nil && uid.String != "" {
			return uid.String
		}
	}
	if ev.jobName != "" {
		var uid sql.NullString
		if err := d.db.QueryRowContext(ctx,
			`SELECT j.uid FROM jobs j WHERE j.name = ?
			   AND (SELECT COUNT(*) FROM jobs j2 WHERE j2.name = j.name) = 1`,
			ev.jobName).Scan(&uid); err == nil {
			return uid.String
		}
	}
	return ""
}

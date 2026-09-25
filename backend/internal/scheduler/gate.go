package scheduler

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/calendar"
)

// The run-admission gates, and the one place that says which producer applies
// which (FX-C, the review-fixes plan §3).
//
// Cronomicon has five things that can start a run — a clock, a click, a reaction, a
// pending-run promotion and a file arrival — and until this file there was no
// shared seam where the gates were applied. Every producer re-applied pause, the
// fleet-wide cap, calendars and concurrency by hand, so each new one had to
// rediscover the full list from the others' source. Predictably they did not:
// the v1.0.0 review found the newest producer missing pause and the cap, and the
// FX audit that followed found five more misses spread across three producers.
//
// The recurring failure is not that someone forgot a check. It is that "which
// gates apply to this producer" was never written down anywhere, so it could
// only be reconstructed — and a reconstruction that comes out one gate short
// looks exactly like a correct one. originPolicy below is that missing
// statement. A new producer declares its origin and gets the right answer; if
// the right answer is genuinely different, the difference is a line in a table
// with a reason next to it rather than an omission nobody can see.
//
// WHAT THIS FILE DOES NOT DO YET. It evaluates the gates and returns a decision;
// it does not perform the enqueue, and the cron and reaction producers still
// call their own inlined checks rather than routing through it. Those two are
// the producers whose gates were already complete, so migrating them buys no
// bug fix and risks the most safety-critical path in the app. The conformance
// matrix in gate_test.go pins all five producers' BEHAVIOUR regardless of which
// code path each one takes, which is the property that actually matters; moving
// the remaining two onto this seam is a refactor that can happen behind it.

// RunOrigin names the thing that is trying to start a run.
type RunOrigin string

const (
	// OriginCron — the scheduler's own tick, firing a schedule entry.
	OriginCron RunOrigin = "cron"
	// OriginManual — a person clicking Run in the UI.
	OriginManual RunOrigin = "manual"
	// OriginToken — a service account calling the machine trigger endpoint.
	OriginToken RunOrigin = "token"
	// OriginReaction — another run finishing.
	OriginReaction RunOrigin = "reaction"
	// OriginPromotion — a parked pending_runs row coming due.
	OriginPromotion RunOrigin = "promotion"
	// OriginPromotionAdHoc — a parked row coming due whose PROVENANCE is a
	// person (or token) deferring a run from the Run dialog, rather than a
	// queue park or a reaction (FX2-B).
	OriginPromotionAdHoc RunOrigin = "promotion-adhoc"
	// OriginFileArrival — a watched file landing on a runner.
	OriginFileArrival RunOrigin = "file"
)

// originPolicy declares which gates bind a producer, and why any exemption is
// an exemption rather than an oversight.
type originPolicy struct {
	// pause — does an operator's pause stop this producer?
	pause bool
	// globalFreeze — does a fleet-wide calendar freeze stop it? (FX-Q2)
	globalFreeze bool
	// entryCalendars — do the schedule entry's own skip/only calendars apply?
	//
	// DOCUMENTS THE MATRIX; Evaluate does not implement it. The only producer for
	// which it is true is cron, which resolves its entry's bindings itself in
	// calendarVerdict (they hang off the definition_schedules row, which Evaluate
	// is not given). It stays in the table because the matrix is the deliverable:
	// a reader asking "does a reaction inherit its target's calendars?" must find
	// the answer here. If a future producer sets this true AND routes through
	// Evaluate, TestGateMatrix_EntryCalendarsAreCronOnly fails, which is the
	// intended way to discover that this needs implementing.
	entryCalendars bool
	// fleetCap — is it subject to settings.maxConcurrent?
	fleetCap bool
	// enabledCheck — must the definition still be enabled in the catalog?
	//
	// Separate from pause because they answer different questions: pause is an
	// operator saying "not now", enabled=0 is the catalog saying "not at all".
	// Promotion was missing this one — it re-checked deleted_at and not enabled,
	// so a job disabled after a run was parked still ran.
	enabledCheck bool
	// why documents the exemptions. Read it before adding a producer.
	why string
}

// originPolicies is the table. FX-Q1 and FX-Q2 are the decisions it encodes.
var originPolicies = map[RunOrigin]originPolicy{
	OriginCron: {
		pause: true, enabledCheck: true, globalFreeze: true, entryCalendars: true, fleetCap: true,
		why: "The reference producer: every gate applies. A cron fire is the case all the others are compared against.",
	},
	OriginManual: {
		pause: false, enabledCheck: false, globalFreeze: true, entryCalendars: false, fleetCap: true,
		why: "FX-Q1: a person clicking Run OVERRIDES a pause — pause exists to stop " +
			"automation, not the operator standing in front of it — but the UI names the " +
			"pause in a confirmation first, so it is a choice rather than a surprise. A " +
			"fleet-wide freeze still binds (FX-Q2): a change freeze that a click can " +
			"walk through is not a freeze. Entry calendars need a schedule entry, and a " +
			"manual run has none. enabledCheck is off for BOTH HTTP producers, " +
			"preserving behaviour that predates this table: a disabled job has always " +
			"been runnable by hand (the UI simply hides it), and quietly closing that " +
			"here would be a product change smuggled in as a refactor. It is written " +
			"down now, which is the point, and can be decided on its own.",
	},
	OriginToken: {
		pause: true, enabledCheck: false, globalFreeze: true, entryCalendars: false, fleetCap: true,
		why: "FX-Q1: the machine half of the manual path, and the one that must NOT " +
			"inherit the click's pause override — a service account cannot see the " +
			"confirmation dialog that makes the override deliberate, so for it a pause " +
			"is simply a pause. This was the gap: the token route delegates to the same " +
			"handler as the UI and silently inherited its exemption.",
	},
	OriginReaction: {
		pause: true, enabledCheck: true, globalFreeze: true, entryCalendars: false, fleetCap: true,
		why: "Entry calendars deliberately do not apply (RX): a reaction has no schedule " +
			"entry of its own, and inheriting the upstream job's would bind the " +
			"downstream to a calendar nobody wrote for it. The fleet cap is applied at " +
			"promotion rather than delivery, since a reaction parks first.",
	},
	OriginPromotion: {
		pause: true, enabledCheck: true, globalFreeze: true, entryCalendars: false, fleetCap: true,
		why: "Judged at the PROMOTION instant, not when the row was parked. 'The system " +
			"is busy now' says nothing about tomorrow at 5pm, and neither does 'nobody " +
			"had declared a freeze yet'. A pause or a freeze applied AFTER a row was " +
			"parked must still stop it — the gap that let a paused job's deferred run " +
			"fire, since delay can be arbitrarily long and rows survive restarts.",
	},
	OriginPromotionAdHoc: {
		pause: true, enabledCheck: false, globalFreeze: true, entryCalendars: false, fleetCap: true,
		why: "FX2-B: the promotion of a HAND deferral inherits the hand's enabled " +
			"exemption. The manual and token origins accept a deferred run of a " +
			"disabled job at park time (enabledCheck=false, 'runnable by hand'); " +
			"promoting that row under the strict promotion policy meant the system " +
			"accepted a run it then guaranteed would miss — held by the disabled gate " +
			"for the full 24h grace, then paged on-call about it. Provenance decides: " +
			"a queue-parked or reaction-parked row keeps strict OriginPromotion, so " +
			"disabling a job still stops its queued backlog. PAUSE stays strict for " +
			"both (FX2-Q2, deliberate asymmetry): pause is 'stop everything now' and " +
			"the hold is visible and clears on resume; disable is a catalog state the " +
			"hand already chose to override when it parked the row.",
	},
	OriginFileArrival: {
		pause: true, enabledCheck: true, globalFreeze: true, entryCalendars: false, fleetCap: true,
		why: "An external event is still subject to the operator's controls. A watch has " +
			"no schedule entry, so entry calendars cannot apply; everything else does, " +
			"including the freeze this producer was missing.",
	},
}

// PolicyFor returns a producer's gate policy. Unknown origins get the strictest
// answer rather than a permissive default — a producer nobody has classified
// should refuse loudly, not run through every gate unchecked.
func PolicyFor(o RunOrigin) originPolicy {
	if p, ok := originPolicies[o]; ok {
		return p
	}
	return originPolicy{pause: true, globalFreeze: true, entryCalendars: true, fleetCap: true, enabledCheck: true,
		why: "unknown origin — every gate applied, deliberately"}
}

// GateRequest is what the gates are asked about.
type GateRequest struct {
	Origin    RunOrigin
	Source    string // git | cronomicon
	OwnerKind string // job | workflow
	Name      string
}

// GateOutcome is what the gates decided.
type GateOutcome struct {
	// Allowed is the only field a caller must check.
	Allowed bool
	// Gate names which gate refused ("pause", "calendar", "cap"); empty when allowed.
	Gate string
	// Reason is the human sentence, suitable for a log line, an HTTP body or a
	// skipped run's queued_reason.
	Reason string
	// Calendar is the suppressing calendar's label, when Gate == "calendar".
	Calendar string
	// Day is the application-zone day the calendar verdict was taken on.
	Day string
}

func allowed() GateOutcome { return GateOutcome{Allowed: true} }

// Evaluate applies every gate the request's origin is subject to, in the same
// order the cron path applies them: pause, then calendars, then the fleet cap.
//
// The order is load-bearing and is the cron path's, deliberately. A fire
// suppressed by policy must be RECORDED as suppressed by policy rather than
// swallowed by a cap that happened to be full at the same instant — the cap is
// transient and re-fires, a policy suppression is a decision someone made, and
// reversing the two makes the audit trail lie about why a run did not happen.
//
// Per-job concurrency (Forbid/Queue) is deliberately NOT here. It is not an
// admission gate but a routing decision — Queue parks rather than refuses — and
// its two callers need different outcomes from the same verdict (a 409 for the
// API, a parked row for the scheduler). Keeping it out leaves this function
// total: every gate here either allows or refuses.
func Evaluate(ctx context.Context, database *sql.DB, log logger, req GateRequest, appLoc *time.Location) GateOutcome {
	pol := PolicyFor(req.Origin)
	kind := req.OwnerKind
	if kind == "" {
		kind = "job"
	}

	// Catalog state first: "this definition must not run at all" outranks "not
	// right now", and reporting the weaker reason for the stronger condition
	// sends an operator to resume a job that would still not have run.
	if pol.enabledCheck && definitionDisabled(ctx, database, kind, req.Source, req.Name) {
		return GateOutcome{Gate: "disabled", Reason: reasonDefinitionDisabled}
	}

	if pol.pause && IsPaused(ctx, database, req.Source, kind, req.Name) {
		return GateOutcome{Gate: "pause", Reason: reasonJobPaused}
	}

	if pol.globalFreeze {
		if v, day, ok := globalFreezeVerdict(ctx, database, log, appLoc); ok && v.Suppressed {
			return GateOutcome{
				Gate:     "calendar",
				Reason:   calendarReason(v),
				Calendar: v.Calendar,
				Day:      day,
			}
		}
	}

	if pol.fleetCap && AtCapacity(ctx, database) {
		return GateOutcome{Gate: "cap", Reason: reasonConcurrencyCap}
	}

	return allowed()
}

// logger is the narrow slice of *slog.Logger this file needs, so a caller
// without one can pass nil.
type logger interface {
	Error(msg string, args ...any)
}

// logErr logs only when there is something to log on.
//
// The nil check cannot be `log != nil`: a typed nil — a (*slog.Logger)(nil)
// stored in this interface — is non-nil as an interface value and panics on
// call. api.Server.log is nil-able and guarded for exactly that elsewhere, so a
// calendar-load failure would otherwise take down a request.
func logErr(log logger, msg string, args ...any) {
	if l, ok := log.(*slog.Logger); ok && l == nil {
		return
	}
	if log == nil {
		return
	}
	log.Error(msg, args...)
}

// reasonDefinitionDisabled is the refusal when the catalog says a definition is
// off. It shares the "Skipped: " prefix the rest of this package's reasons use.
const reasonDefinitionDisabled = "Skipped: the definition is disabled"

// definitionDisabled reports whether the definition EXISTS and is switched off.
//
// Absence deliberately reads as NOT disabled, which looks backwards and is not.
// This gate answers "may this run?" for conditions that can CLEAR — a disabled
// job can be re-enabled, so its callers hold the run and retry. A definition
// that is absent (renamed, purged) is terminal, and every producer already has
// its own handling for that with the right shape: promotion marks the parked run
// missed and says so, file arrival refuses the sighting, cron and the reactor
// filter it out of their candidate sets before they ever get here. Answering
// "disabled" for an absent definition would preempt all of that and hold a row
// forever, waiting for something that is never coming back.
//
// deleted_at is likewise not consulted: the recycle bin is its own state with
// its own handling (FX-A3 holds the row and names the bin on expiry), and
// collapsing it into "disabled" would report the wrong remedy — restore, not
// re-enable.
func definitionDisabled(ctx context.Context, database *sql.DB, kind, source, name string) bool {
	table := "jobs"
	if kind == "workflow" {
		table = "workflows"
	}
	var enabled int
	//nolint:gosec // table is a package-local constant chosen by the kind switch
	err := database.QueryRowContext(ctx,
		"SELECT enabled FROM "+table+" WHERE source = ? AND name = ?", source, name).Scan(&enabled)
	if err != nil {
		return false // absent, or unreadable — not this gate's question
	}
	return enabled == 0
}

// appLocationOrDB resolves the zone the freeze day is computed in.
//
// A caller that already holds the engine's location (the scheduler) passes it;
// one that does not (the file-watch ingest, which lives in the runner package
// and has no Scheduler) passes nil and gets it read from the same settings key
// the engine was built from. Nobody gets to skip it and land on UTC by default:
// computing a calendar DAY in the wrong zone is the FX-B #11 defect class, and
// on a UTC deployment the mistake is invisible.
func appLocationOrDB(ctx context.Context, database *sql.DB, loc *time.Location) *time.Location {
	if loc != nil {
		return loc
	}
	var tz sql.NullString
	_ = database.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'timezone'`).Scan(&tz)
	if tz.Valid {
		if l, err := time.LoadLocation(strings.TrimSpace(tz.String)); err == nil {
			return l
		}
	}
	// UTC, matching what the APP resolves to when the key is absent:
	// assembleGlobalSettings defaults `timezone` to "UTC", and the row is written
	// only when an operator saves General Settings (or the demo seeder runs), so a
	// production install that never opens that page has no row at all. Falling back
	// to time.Local here instead would put this gate in the machine's zone while
	// every other schedule surface was in UTC — the FX-B #11 defect class, and
	// invisible on a UTC box, which is where it would be developed.
	return time.UTC
}

// globalFreezeVerdict evaluates ONLY the global calendar tier — the fleet-wide
// freeze — with no entry bindings. It is the reactor's rule (RX), generalised:
// a freeze declared for the whole installation must stop every producer, while
// a schedule entry's own skip/only calendars belong to that entry and cannot be
// inherited by a producer that has no entry.
//
// A load failure returns ok=false rather than a suppression: a calendar the
// process cannot read must not become an outage that stops every run.
func globalFreezeVerdict(ctx context.Context, database *sql.DB, log logger, loc *time.Location) (calendar.Verdict, string, bool) {
	day := calendar.DayOf(time.Now(), appLocationOrDB(ctx, database, loc))
	sets, err := calendar.Load(ctx, database, nil)
	if err != nil {
		logErr(log, "gate: load calendars — admitting the run without calendar suppression", "err", err)
		return calendar.Verdict{}, day, false
	}
	return calendar.Evaluate(sets, nil, nil, day), day, true
}

// calendarReason renders a calendar verdict in the SAME vocabulary the cron path
// records, so an operator comparing a refused trigger to a suppressed fire is not
// left working out whether two sentences mean the same thing.
//
// Verdict.Calendar is the calendar's NAME and Verdict.Label is the DAY's label
// inside it ("Independence Day"); naming the label alone reads as if the day were
// the calendar. Both are carried, and the "Skipped: " prefix every other refusal
// in this package uses is kept.
func calendarReason(v calendar.Verdict) string {
	name := strings.TrimSpace(v.Calendar)
	label := strings.TrimSpace(v.Label)
	switch {
	case name == "" && label == "":
		return "Skipped: suppressed by a working calendar"
	case name == "":
		return "Skipped: suppressed by a working calendar (" + label + ")"
	case label == "":
		return `Skipped: suppressed by calendar "` + name + `"`
	default:
		return `Skipped: suppressed by calendar "` + name + `" (` + label + `)`
	}
}

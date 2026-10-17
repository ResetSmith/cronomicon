// Package runcap is the concurrency cap: the installation's, and an agency's
// own (2.5.0 C6, QT-1; the owner's answer to question 17, present defect 47).
//
// The installation has one cap, settings.maxConcurrent (five by default). Until
// 2.5.0 it counted every queued run as well as the running ones, and was asked
// only where a run is produced. Nothing ends a run that stays queued, so one
// agency's switched-off agent left a waiting run at every fire, and at the cap
// every agency's fires were skipped: one department's mistake reaching every
// other's.
//
// The rule now, asked in two places:
//
//   - Where a run of agency A is produced (a fire, a click, a token, a
//     promotion, a file arrival, a workflow's trigger): the runs running in
//     every agency plus the runs WAITING in A against the installation's cap; a
//     waiting run holds up its own agency and nobody else. And, when A has a
//     cap, A's runs waiting and running against it.
//   - Where a run is claimed (Claim, for an agent and the local runner alike):
//     the runs running in every agency against the installation's cap, and A's
//     running runs against A's. Without this, an agent that comes back starts
//     every run that waited for it at once, past the cap.
//
// A workflow's steps are produced by its engine without the first question
// (the workflow was admitted when it was triggered); the second still holds
// them, so a fan-out wider than the cap waits for slots.
package runcap

import (
	"context"
	"database/sql"
	"fmt"
)

// DefaultMax is the installation's cap when settings.maxConcurrent is unset.
const DefaultMax = 5

// Max is the installation's cap: settings.maxConcurrent, or DefaultMax when it
// is unset or not a positive number.
func Max(ctx context.Context, database *sql.DB) int {
	var val sql.NullString
	_ = database.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'maxConcurrent'`).Scan(&val)
	if val.Valid && val.String != "" {
		var n int
		if _, err := fmt.Sscanf(val.String, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return DefaultMax
}

// Verdict is the cap's answer where a run is produced.
type Verdict struct {
	// Reached: the run is not to be produced.
	Reached bool
	// Agency: it is the agency's own cap that is reached, not the
	// installation's. AgencyName names it.
	Agency     bool
	AgencyName string
	// Cap is the cap that was reached.
	Cap int
}

// AtProduction answers whether a run of agency (an id) may be produced now.
// A read error answers "not reached": the claim asks again.
func AtProduction(ctx context.Context, database *sql.DB, agency string) Verdict {
	limit := Max(ctx, database)
	var running, waitingHere, activeHere int
	var agencyCap sql.NullInt64
	var name sql.NullString
	if err := database.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM runs WHERE status = 'running'),
		       (SELECT COUNT(*) FROM runs WHERE status = 'queued' AND owner_agency = ?1),
		       (SELECT COUNT(*) FROM runs WHERE status IN ('queued','running') AND owner_agency = ?1),
		       (SELECT max_concurrent FROM agencies WHERE id = ?1),
		       (SELECT name FROM agencies WHERE id = ?1)`, agency).
		Scan(&running, &waitingHere, &activeHere, &agencyCap, &name); err != nil {
		return Verdict{}
	}
	if running+waitingHere >= limit {
		return Verdict{Reached: true, Cap: limit}
	}
	if agencyCap.Valid && int64(activeHere) >= agencyCap.Int64 {
		return Verdict{Reached: true, Agency: true, AgencyName: nameOr(name, agency), Cap: int(agencyCap.Int64)}
	}
	return Verdict{}
}

// HeldAtClaim is why a run of agency that a runner could take is not taken:
// the sentence for a waiting run, or "" when no cap holds it.
func HeldAtClaim(ctx context.Context, database *sql.DB, agency string) string {
	limit := Max(ctx, database)
	var running, runningHere int
	var agencyCap sql.NullInt64
	var name sql.NullString
	if err := database.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM runs WHERE status = 'running'),
		       (SELECT COUNT(*) FROM runs WHERE status = 'running' AND owner_agency = ?1),
		       (SELECT max_concurrent FROM agencies WHERE id = ?1),
		       (SELECT name FROM agencies WHERE id = ?1)`, agency).
		Scan(&running, &runningHere, &agencyCap, &name); err != nil {
		return ""
	}
	if running >= limit {
		return fmt.Sprintf("Waiting for a slot: the installation's concurrency cap (%d) is reached by running runs", limit)
	}
	if agencyCap.Valid && int64(runningHere) >= agencyCap.Int64 {
		return fmt.Sprintf("Waiting for a slot: %s's concurrency cap (%d) is reached by its running runs", nameOr(name, agency), agencyCap.Int64)
	}
	return ""
}

// ClaimSQL is the claim's two clauses, ANDed into the WHERE of the candidate
// run (alias `runs`). It takes one placeholder, the installation's cap (Max,
// read before the statement). The claim is one UPDATE, so the counts are the
// state the claim itself changes, with no window between them.
//
// The installation's count is uncorrelated and worked out once per statement.
// The agency's runs only for a run whose agency has a cap (the keyed lookup of
// the agency comes first), and reads the partial index of running runs
// (idx_runs_running_agency, 1560): without it the count walked every run the
// agency ever had. An uncorrelated form (the agencies at their cap as a list)
// measured no better. cap_claim_bench_test.go has the numbers.
const ClaimSQL = `
	  AND (SELECT COUNT(*) FROM runs rc WHERE rc.status = 'running') < ?
	  AND NOT EXISTS (SELECT 1 FROM agencies cap_ag
	                   WHERE cap_ag.id = runs.owner_agency AND cap_ag.max_concurrent IS NOT NULL
	                     AND cap_ag.max_concurrent <= (SELECT COUNT(*) FROM runs rca
	                                                    WHERE rca.status = 'running' AND rca.owner_agency = cap_ag.id))`

func nameOr(name sql.NullString, id string) string {
	if name.Valid && name.String != "" {
		return name.String
	}
	return id
}

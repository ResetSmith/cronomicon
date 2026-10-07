package execspec

import (
	"context"
	"database/sql"
	"sync/atomic"
)

// The rules a runner's claim applies are written three times: the claim query
// itself (runner/poll.go, which sees them from one runner's side, for every
// queued run), EligibleOnlineRunnerForRun (one run's side, for every online
// runner) and UnclaimableReason (the same, one gate at a time, to name the first
// that fails). They are three statements because they answer three questions,
// and three statements drift: by 2.3.0 the two mirrors matched a runner's
// DECLARED capabilities where the claim subtracts the mask, keyed the injection
// gate on a job's name where the claim uses its uid, ignored a run's own SSH
// credential, and did not know the injection kill switch existed — each one a
// way for "why can't this run be claimed" to name a reason that is not the
// reason, or to stay silent about the one that is.
//
// This file holds every piece the three can share, so a rule has one spelling
// even where it has three call sites. A new claim rule goes here first.

// EffectiveCaps subtracts a runner's server-managed capability mask from the
// capability tokens it declared. The mask is subtract-only (it lists run types
// the operator has switched off for this runner). Order-preserving; an empty
// mask returns caps unchanged. The claim calls this; effectiveCapsSQL is the
// same rule for the two mirrors, and a test holds them to one answer.
func EffectiveCaps(caps, mask []string) []string {
	if len(mask) == 0 {
		return caps
	}
	masked := make(map[string]bool, len(mask))
	for _, m := range mask {
		masked[m] = true
	}
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		if !masked[c] {
			out = append(out, c)
		}
	}
	return out
}

// effectiveCapsSQL is EffectiveCaps as a one-column subquery over the runners
// row aliased `alias`: the declared tokens that the mask does not list. A
// managed_settings blob that is absent, not JSON, or carries no mask subtracts
// nothing — as LoadManagedSettings reads it.
func effectiveCapsSQL(alias string) string {
	return `(SELECT ec.value FROM json_each(COALESCE(` + alias + `.capabilities, '[]')) ec
	          WHERE ec.value NOT IN (
	                SELECT em.value FROM json_each(
	                       CASE WHEN json_valid(` + alias + `.managed_settings)
	                             AND json_type(` + alias + `.managed_settings, '$.capabilityMask') = 'array'
	                            THEN json_extract(` + alias + `.managed_settings, '$.capabilityMask')
	                            ELSE '[]' END) em))`
}

// injectionGateArmed mirrors config.SecretsInjectionEnabled for the two
// mirrors, which are called from places that hold no configuration (the run
// writer, the workflow engine). True — the configuration's own default — until
// the server says otherwise at boot.
var injectionGateArmed atomic.Bool

func init() { injectionGateArmed.Store(true) }

// SetInjectionGateArmed tells the claim mirrors whether the secret-injection
// fence is in force. With the kill switch off the claim query lets any runner
// take a run that declares bindings (nothing is injected, so nothing needs
// fencing), and a mirror that still applied the gate would report such a run
// as waiting for an injection-flagged runner while an ordinary one claimed it.
func SetInjectionGateArmed(armed bool) { injectionGateArmed.Store(armed) }

// runNeedsInjectionRunner reports whether the claim query's injection gate
// applies to this run: the kill switch is on, and the run carries a per-run SSH
// credential (an implicit key binding, CA-3b) or its job or script declares
// reference bindings. The job is identified by the run's frozen uid when it has
// one (R2F-1: a same-named sibling's bindings must not decide this run's gate)
// and by source and name for a run that predates the uid.
func runNeedsInjectionRunner(ctx context.Context, database *sql.DB, runID string) (bool, error) {
	if !injectionGateArmed.Load() {
		return false, nil
	}
	var needs bool
	err := database.QueryRowContext(ctx, `
		SELECT COALESCE(r.ssh_credential, '') <> ''
		    OR EXISTS (
		       SELECT 1 FROM reference_bindings rb
		        WHERE (rb.owner_kind = 'job'
		                AND CASE WHEN COALESCE(r.job_uid, '') <> ''
		                         THEN rb.owner_uid = r.job_uid
		                         ELSE rb.owner_source = COALESCE(NULLIF(r.job_source, ''), 'git')
		                              AND rb.owner_name = r.job_name END)
		           OR (rb.owner_kind = 'script' AND rb.owner_name = r.script_ref))
		  FROM runs r WHERE r.id = ?`, runID).Scan(&needs)
	return needs, err
}

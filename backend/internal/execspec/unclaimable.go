package execspec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// RA-20(b) — name the missing runner property at ENQUEUE.
//
// Every claim gate that saves correctness presents to an operator as the same
// symptom: the run sits queued and nothing happens. With a fleet of one that was
// survivable, because there was only one runner to inspect. As the fleet grows —
// and it is growing — "queued" stops being a diagnosis and becomes a mystery, and
// under the ops model (§13) the person who must diagnose it is the one furthest
// from the fleet: the agency user only knows their job did not run.
//
// Computed at enqueue and STORED rather than derived on read, because the question
// is usually asked in the past tense. "Why didn't it run last night?" cannot be
// answered by a probe of today's fleet — the runner that was missing may be online
// now. A stored reason survives into run history with the run it explains.
//
// The reason is advisory: it never blocks the enqueue and never affects claiming.
// A run whose runner appears a minute later is claimed normally, and the stale
// reason is cleared by the claim.

// UnclaimableReason returns a human sentence naming WHY no online runner can claim
// the run, or "" when one can. It narrows through claimRun's gates in the order an
// operator would check them, so the sentence names the FIRST thing that is wrong
// rather than the last — "no runner is online" is more useful than "no runner
// advertises become-file" when both are true.
//
// Fleet-capability information is deliberately included. The audience holds
// triggerJobs on the run's scope, the facts are about infrastructure rather than
// credentials, and a reason vague enough to leak nothing is a reason nobody can act
// on — which is the entire failure being fixed.
func UnclaimableReason(ctx context.Context, database *sql.DB, runID string) (string, error) {
	ok, _, err := EligibleOnlineRunnerForRun(ctx, database, runID)
	if err != nil || ok {
		return "", err
	}

	var agenciesJSON, runType, requiresJSON, jobName, jobSource, jobUID, scriptRef, runnerTag sql.NullString
	err = database.QueryRowContext(ctx, `
		SELECT COALESCE(agencies_json,'[]'), run_type, requires_json, job_name, job_source, job_uid, script_ref, runner_tag
		FROM runs WHERE id = ?`, runID).
		Scan(&agenciesJSON, &runType, &requiresJSON, &jobName, &jobSource, &jobUID, &scriptRef, &runnerTag)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	ag := agenciesJSON.String
	if ag == "" || ag == "null" {
		ag = "[]"
	}

	count := func(where string, args ...any) (int, error) {
		var n int
		e := database.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM runners rn WHERE rn.status = 'online' AND `+where, args...).Scan(&n)
		return n, e
	}

	// 1. Is anything online at all?
	n, err := count(`1 = 1`)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "no runner is online", nil
	}

	// 2. Can anything online run this TYPE?
	typeClause := `? IN (SELECT value FROM json_each(rn.capabilities))`
	n, err = count(typeClause, runType.String)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "no online runner can run " + runType.String + " jobs", nil
	}

	// 3. Agency eligibility — claimRun's disjoint split (AG-Q3a), reproduced exactly.
	// A tagged run needs a MEMBER of one of its agencies; an untagged run needs a
	// runner with NO agencies. The second half is the one that surprises people:
	// an unbound run is not claimable by "anything", it is claimable ONLY by the
	// general pool, so a fully departmentalised fleet cannot take it at all.
	agencyClause := typeClause + ` AND (
		(? <> '[]' AND EXISTS (SELECT 1 FROM runner_agencies ra JOIN agencies a ON a.id = ra.agency_id
		                       WHERE ra.runner_id = rn.id AND a.name IN (SELECT value FROM json_each(?))))
		OR (? = '[]' AND NOT EXISTS (SELECT 1 FROM runner_agencies ra WHERE ra.runner_id = rn.id)))`
	n, err = count(agencyClause, runType.String, ag, ag, ag)
	if err != nil {
		return "", err
	}
	if n == 0 {
		if ag == "[]" {
			return "this run has no scope, so only a runner with no agencies can claim it, " +
				"and every online runner belongs to one — bind a scope to run it", nil
		}
		var names []string
		_ = json.Unmarshal([]byte(ag), &names)
		return "no online runner belongs to " + strings.Join(names, ", "), nil
	}

	// 3.5. The RT-1 runner pin (mig. 1070). Checked AFTER agency and BEFORE
	// injection because that is the order an operator checks them in: "is it even
	// the right network" precedes "is it allowed to hold secrets". An unpinned run
	// short-circuits on the '' arm and this gate costs it one cheap comparison.
	pinClause := agencyClause + ` AND (? = '' OR EXISTS (SELECT 1 FROM runner_tags rt
	                                                     WHERE rt.runner_id = rn.id AND rt.tag = ?))`
	n, err = count(pinClause, runType.String, ag, ag, ag, runnerTag.String, runnerTag.String)
	if err != nil {
		return "", err
	}
	if n == 0 {
		// RT-G5 — two very different situations produce the same symptom, and
		// conflating them sends the operator to the wrong place. "Nothing carries
		// this tag" means a typo or an un-tagged runner: go edit tags. "Tagged
		// runners exist but none is eligible" means they are offline, draining, in
		// another agency, or cannot run this type: go look at those runners. The
		// fleet-wide count deliberately ignores status, which is the whole point.
		var tagged int
		_ = database.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM runner_tags WHERE tag = ?`, runnerTag.String).Scan(&tagged)
		if tagged == 0 {
			return "this run is pinned to runners tagged " + runnerTag.String +
				", and no runner in the fleet carries that tag", nil
		}
		return "this run is pinned to runners tagged " + runnerTag.String +
			", and none of them is online and otherwise eligible", nil
	}

	// 4. Secret injection (P1.4): a run whose job/script declares bindings needs an
	// injection-flagged runner. This is the gate most likely to be hit by a NEW
	// runner — enrolling it in the agency is the obvious step, ticking the
	// injection flag is the one people forget.
	var bindsSecrets int
	_ = database.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM reference_bindings rb
			-- R2F-1: keyed on the run's frozen job identity, matching poll.go's gate
			-- exactly. An explainer reading a DIFFERENT job's bindings than the claim
			-- query would name a reason that is not the one holding the run.
			WHERE (rb.owner_kind = 'job'
			        AND CASE WHEN ? != '' THEN rb.owner_uid = ?
			                 ELSE rb.owner_source = COALESCE(NULLIF(?, ''), 'git') AND rb.owner_name = ? END)
			   OR (rb.owner_kind = 'script' AND rb.owner_name = ?))`,
		jobUID.String, jobUID.String, jobSource.String, jobName.String, scriptRef.String).Scan(&bindsSecrets)
	injectionClause := pinClause + ` AND (? = 0 OR rn.allow_secret_injection = 1)`
	n, err = count(injectionClause, runType.String, ag, ag, ag, runnerTag.String, runnerTag.String, bindsSecrets)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "this run carries credentials, and no eligible runner is flagged for secret injection", nil
	}

	// 5. Requirement tokens — whatever is left. Named individually, because
	// "requirements unmet" sends an operator to read JSON.
	var requires []string
	if requiresJSON.Valid && requiresJSON.String != "" {
		_ = json.Unmarshal([]byte(requiresJSON.String), &requires)
	}
	var missing []string
	for _, tok := range requires {
		n, err = count(injectionClause+` AND ? IN (SELECT value FROM json_each(rn.capabilities))`,
			runType.String, ag, ag, ag, runnerTag.String, runnerTag.String, bindsSecrets, tok)
		if err != nil {
			return "", err
		}
		if n == 0 {
			missing = append(missing, tok)
		}
	}
	if len(missing) > 0 {
		return "no eligible runner advertises " + strings.Join(missing, ", "), nil
	}

	// Every gate above passed individually but the combined predicate did not — the
	// tokens are satisfiable only by DIFFERENT runners. Rare, and worth saying
	// plainly rather than guessing.
	return "no single online runner satisfies all of this run's requirements", nil
}

// StampUnclaimableReason writes UnclaimableReason onto a queued run, if any. Never
// fails the caller: an advisory hint that errors must not take an enqueue down with
// it, so the error is returned for logging and the run stands either way.
//
// Only ever writes to a run still in 'queued' — by the time this runs, a fast
// runner may already have claimed it, and overwriting a live run's reason column
// with a stale "nobody can claim this" would be worse than saying nothing.
func StampUnclaimableReason(ctx context.Context, database *sql.DB, runID string) (string, error) {
	reason, err := UnclaimableReason(ctx, database, runID)
	if err != nil || reason == "" {
		return "", err
	}
	_, err = database.ExecContext(ctx,
		`UPDATE runs SET queued_reason = ? WHERE id = ? AND status = 'queued'`, reason, runID)
	return reason, err
}

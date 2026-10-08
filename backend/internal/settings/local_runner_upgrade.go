package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/notices"
)

// The 2.3.0 upgrade pass for the local runner (LR-16, LR-42).
//
// Until 2.3.0 a run was written for one of two executors, chosen by the job,
// by a default, or by its run type, and the server's SSH pool took every
// `executor='ssh'` row whatever its agency. From 2.3.0 every run is written for
// the runner executor and the server takes its share as the local runner, by
// the same agency rule as an agent. Left alone, that would stop the shell jobs
// of every agency the local runner does not serve — which at first is every
// agency but Global.
//
// So, ONCE, on the first start of 2.3.0, and only if the SSH executor was
// turned on:
//
//   - for every UNBOUND scope whose shell jobs the server was running, the
//     local runner is put on the serve list of the scope's agency;
//   - how each scope's shell jobs were being served is recorded
//     (notices.LocalRunnerUpgrade), so the inbox can say where a job may now
//     run somewhere it did not — for as long as that is true. Nothing is bound
//     and nothing is moved (LR-16: a warning only);
//   - rows still queued for the SSH executor are re-written for the runner
//     executor, where the local runner will claim them.
//
// If the SSH executor was turned OFF, those queued rows were never going to
// run. They are closed as skipped, with the reason, and nothing is placed.
//
// "Was being served by the server" is decided by a frozen copy of the 2.2
// precedence, kept only here: the job's own executor, else the global default,
// else the run type's (a shell type ran over SSH). The scope binding rung is
// absent because bound scopes are not looked at: their jobs already ran on
// their runners, and still do.

// LocalRunnerUpgradeResult is what the pass did, for the boot log.
type LocalRunnerUpgradeResult struct {
	Ran            bool // false: it had run before
	WasOn          bool
	PlacedAgencies []string // names
	Requeued       int64    // queued ssh rows re-written for the runner executor
	Skipped        int64    // queued ssh rows closed, because nothing would run them
	Scopes         int      // scopes recorded
}

const reasonSSHExecutorWasOff = "Skipped: this run was queued for the SSH executor, which was turned off; " +
	"2.3.0 replaced it with the local runner and nothing would have claimed this run — run the job again"

// RunLocalRunnerUpgradePass runs the pass if it has not run. wasOn is whether
// the SSH executor was turned on when 2.3.0 first started (SSHExecutorWasOn) —
// what the server WAS doing, whatever the switch says now and whether or not
// the host has since forbidden the local runner. A forbidden local runner is
// still placed where the server was serving: it claims nothing while
// forbidden, and the day that is lifted it serves what it served.
func RunLocalRunnerUpgradePass(ctx context.Context, database *sql.DB, log *slog.Logger, wasOn bool) (*LocalRunnerUpgradeResult, error) {
	if done, err := notices.ReadLocalRunnerUpgrade(ctx, database); err != nil {
		return nil, err
	} else if done != nil {
		return &LocalRunnerUpgradeResult{}, nil
	}
	localID, err := LocalRunnerID(ctx, database)
	if err != nil {
		return nil, err
	}
	if localID == "" {
		return nil, ErrNoLocalRunner
	}
	res := &LocalRunnerUpgradeResult{Ran: true, WasOn: wasOn}
	rec := notices.LocalRunnerUpgrade{WasOn: wasOn, Scopes: map[string]string{}, Placed: []string{}}
	ts := time.Now().UTC().Format(time.RFC3339)

	// Read everything first: no query runs inside an open cursor here.
	served, agenciesOf, err := shellServing22(ctx, database)
	if err != nil {
		return nil, fmt.Errorf("local runner upgrade: %w", err)
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	if wasOn {
		rec.Scopes = served
		res.Scopes = len(served)
		// Place the local runner where the server was serving.
		want := map[string]bool{}
		for scopeID, how := range served {
			if how == notices.ServedByAgents {
				continue
			}
			for _, a := range agenciesOf[scopeID] {
				want[a] = true
			}
		}
		add := make([]string, 0, len(want))
		for a := range want {
			add = append(add, a)
		}
		sort.Strings(add)
		for _, a := range add {
			// Written directly: the local runner's list may be any non-empty
			// set (CheckRunnerPlacement, local), and this only ever adds.
			r, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO runner_agencies (runner_id, agency_id) VALUES (?, ?)`, localID, a)
			if err != nil {
				return nil, fmt.Errorf("local runner upgrade: place in %s: %w", a, err)
			}
			if n, _ := r.RowsAffected(); n == 0 {
				continue // it served this agency already
			}
			var name string
			_ = tx.QueryRowContext(ctx, `SELECT name FROM agencies WHERE id = ?`, a).Scan(&name)
			rec.Placed = append(rec.Placed, a)
			res.PlacedAgencies = append(res.PlacedAgencies, name)
			if err := auditlog.WriteActivity(ctx, tx, auditlog.ActivityParams{
				At: ts, Kind: "config", Actor: "upgrade",
				RunnerName: LocalRunnerName,
				Summary: "the local runner now serves " + name + " — the server was running that agency's shell jobs, " +
					"and from 2.3.0 it takes only the runs of the agencies on its list",
			}); err != nil {
				return nil, fmt.Errorf("local runner upgrade: record placement: %w", err)
			}
		}
		r, err := tx.ExecContext(ctx,
			`UPDATE runs SET executor = 'runner' WHERE status = 'queued' AND executor = 'ssh'`)
		if err != nil {
			return nil, fmt.Errorf("local runner upgrade: requeue: %w", err)
		}
		res.Requeued, _ = r.RowsAffected()
	} else {
		r, err := tx.ExecContext(ctx, `
			UPDATE runs SET status = 'skipped', completed_at = ?, queued_reason = ?
			 WHERE status = 'queued' AND executor = 'ssh'`, ts, reasonSSHExecutorWasOff)
		if err != nil {
			return nil, fmt.Errorf("local runner upgrade: close queued runs: %w", err)
		}
		res.Skipped, _ = r.RowsAffected()
	}

	blob, _ := json.Marshal(rec)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, last_modified_by, last_modified_at) VALUES (?, ?, 'upgrade', ?)`,
		notices.LocalRunnerUpgradeKey, string(blob), ts); err != nil {
		return nil, fmt.Errorf("local runner upgrade: record: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if log != nil {
		// A warning when it changed or recorded anything: where shell jobs run
		// can differ from this start on, and the inbox is the only other place
		// that says so.
		logf := log.Info
		if len(res.PlacedAgencies) > 0 || res.Scopes > 0 || res.Skipped > 0 {
			logf = log.Warn
		}
		scopes := make([]string, 0, len(rec.Scopes))
		for id, how := range rec.Scopes {
			scopes = append(scopes, id+"="+how)
		}
		sort.Strings(scopes)
		logf("local runner: the 2.3.0 upgrade pass ran — every run is now taken by whichever eligible runner asks first, "+
			"an agent or this server; see Notices in the app for the scopes where that can differ from before",
			"ssh_executor_was_on", wasOn, "agencies_the_local_runner_now_serves", res.PlacedAgencies,
			"queued_runs_moved_to_the_runner_executor", res.Requeued, "queued_runs_closed", res.Skipped,
			"shell_jobs_were_served_by", scopes)
	}
	return res, nil
}

// shellServing22 classifies every unbound scope that has shell jobs — and the
// jobs with no scope, under notices.NoScopeSubject — by the executor those
// jobs resolved to under the 2.2 precedence. It also returns each one's
// agencies (Global's id for the jobs with no scope).
func shellServing22(ctx context.Context, database *sql.DB) (served map[string]string, agenciesOf map[string][]string, err error) {
	var def sql.NullString
	if err := database.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = 'defaultExecutor'`).Scan(&def); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	fallback := execspec.ExecutorSSH // a shell type's own default, under 2.2
	if def.String == execspec.ExecutorSSH || def.String == execspec.ExecutorRunner {
		fallback = def.String
	}

	rows, err := database.QueryContext(ctx, `
		SELECT COALESCE(sc.id, ''), COALESCE(j.scope, ''), COALESCE(j.executor, '')
		  FROM jobs j LEFT JOIN scopes sc ON sc.name = j.scope
		 WHERE j.deleted_at IS NULL
		   AND j.run_type IN ('bash','perl','powershell','python')
		   AND NOT EXISTS (SELECT 1 FROM scope_runners sr WHERE sr.scope_id = sc.id)`)
	if err != nil {
		return nil, nil, err
	}
	type tally struct{ ssh, runner bool }
	seen := map[string]*tally{}
	for rows.Next() {
		var scopeID, scopeName, executor string
		if err := rows.Scan(&scopeID, &scopeName, &executor); err != nil {
			rows.Close()
			return nil, nil, err
		}
		key := scopeID
		switch {
		case scopeName == "":
			key = notices.NoScopeSubject
		case scopeID == "":
			continue // a job naming a scope that does not exist runs nowhere
		}
		if executor != execspec.ExecutorSSH && executor != execspec.ExecutorRunner {
			executor = fallback
		}
		t := seen[key]
		if t == nil {
			t = &tally{}
			seen[key] = t
		}
		if executor == execspec.ExecutorSSH {
			t.ssh = true
		} else {
			t.runner = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	served, agenciesOf = map[string]string{}, map[string][]string{}
	for key, t := range seen {
		switch {
		case t.ssh && t.runner:
			served[key] = notices.ServedByBoth
		case t.ssh:
			served[key] = notices.ServedByServer
		default:
			served[key] = notices.ServedByAgents
		}
		if key == notices.NoScopeSubject {
			agenciesOf[key] = []string{agencyid.Global}
			continue
		}
		arows, err := database.QueryContext(ctx,
			`SELECT agency_id FROM scope_agencies WHERE scope_id = ? ORDER BY agency_id`, key)
		if err != nil {
			return nil, nil, err
		}
		for arows.Next() {
			var a string
			if err := arows.Scan(&a); err != nil {
				arows.Close()
				return nil, nil, err
			}
			agenciesOf[key] = append(agenciesOf[key], a)
		}
		arows.Close()
		if err := arows.Err(); err != nil {
			return nil, nil, err
		}
	}
	return served, agenciesOf, nil
}

package api_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// RX Phase A — kill dispositions (the reactions-update plan §3).
//
// Stopping a run and saying what the stop MEANT are two different acts. Before
// this, they were one: every kill wrote status='killed' and a run-end activity
// hardcoded to outcome 'failure', so "I stopped this because it was already
// done" and "I stopped this because it was going wrong" were indistinguishable
// afterwards — and a wedged workflow step could not be stopped without failing
// the whole workflow.
//
// The invariant these tests exist to protect: `status` is what happened,
// `killed_by` is the proof a human ended it. They are independent, and every
// consumer that used to infer the first from the second must now read both.

// killPathFor is the kill route for a job seeded by name.
func killPathFor(t *testing.T, pool *sql.DB, name string) string {
	t.Helper()
	return "/api/v1/jobs/" + jobRowIDStr(t, pool, name) + "/kill"
}

// dispositionFixture seeds an unscoped job with one running, unbound run and
// returns the handler, pool and kill path. Unrestricted admin (nil restrict) so
// the RBAC surface — covered by kill_scope_rf_test.go — stays out of the way.
func dispositionFixture(t *testing.T, jobName, runID string) (http.Handler, *sql.DB, string) {
	t.Helper()
	h, pool := secretRBACServer(t, nil)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled)
	      VALUES (?, 'cronomicon', 'bash', NULL, 1)`, jobName)
	seedRunFor(t, exec, runID, jobName, "")
	return h, pool, killPathFor(t, pool, jobName)
}

func runStatusAndKiller(t *testing.T, pool *sql.DB, runID string) (string, sql.NullString) {
	t.Helper()
	var status string
	var killedBy sql.NullString
	if err := pool.QueryRow(`SELECT status, killed_by FROM runs WHERE id = ?`, runID).
		Scan(&status, &killedBy); err != nil {
		t.Fatalf("read run %s: %v", runID, err)
	}
	return status, killedBy
}

// TestKillWithoutABodyStillRecordsKilled — RX-4's compatibility guarantee.
//
// The disposition is optional and defaults to 'killed'. Every existing caller
// (the "Stop all queued" loop, any script, an older frontend bundle) sends no
// body, and must keep getting exactly the pre-disposition behaviour. If this
// test fails, the feature is not additive.
func TestKillWithoutABodyStillRecordsKilled(t *testing.T) {
	h, pool, path := dispositionFixture(t, "legacy-caller", "r-legacy")

	if rec := reqAs(t, h, http.MethodPost, path, "sec-admins", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("body-less kill = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	status, killedBy := runStatusAndKiller(t, pool, "r-legacy")
	if status != "killed" {
		t.Errorf("status after a body-less kill = %q, want %q — the default must not move", status, "killed")
	}
	if !killedBy.Valid || killedBy.String == "" {
		t.Error("killed_by empty after a kill; it is the provenance half of every disposition (RX-5)")
	}
}

// TestEachDispositionRoundTripsToTheRunStatus — RX-4's core claim, and the one
// that needs no migration: all four values are already legal runs.status
// entries, so a disposition is a choice among existing vocabulary rather than a
// new column.
//
// RX-5 rides along: killed_by is set on EVERY path, including the ones whose
// status says nothing about a human being involved. A run recorded 'success'
// with killed_by set is the genuinely new state Phase A creates.
func TestEachDispositionRoundTripsToTheRunStatus(t *testing.T) {
	for _, tc := range []struct{ outcome, wantStatus string }{
		{"killed", "killed"},
		{"failure", "failure"},
		{"success", "success"},
		{"warning", "warning"},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			h, pool, path := dispositionFixture(t, "disposed-"+tc.outcome, "r-"+tc.outcome)

			rec := reqAs(t, h, http.MethodPost, path, "sec-admins", `{"outcome":"`+tc.outcome+`"}`)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("kill with outcome %q = %d, want 202 (%s)", tc.outcome, rec.Code, rec.Body.String())
			}

			status, killedBy := runStatusAndKiller(t, pool, "r-"+tc.outcome)
			if status != tc.wantStatus {
				t.Errorf("status after outcome %q = %q, want %q", tc.outcome, status, tc.wantStatus)
			}
			if !killedBy.Valid || killedBy.String == "" {
				t.Errorf("killed_by empty for outcome %q — status is the outcome, killed_by is the "+
					"proof a human ended it, and the second must not depend on the first (RX-5)", tc.outcome)
			}
		})
	}
}

// TestKillLeavesExitCodeAlone — the deliberate omission, stated in the plan and
// in the OpenAPI description.
//
// The kill is only QUEUED for the runner (via action_queue), so the process may
// still be alive when the status is written. A disposition asserts intent; it
// does not observe an exit. Synthesising exit 0 for a Success disposition would
// be a lie that a later reader could not detect.
func TestKillLeavesExitCodeAlone(t *testing.T) {
	h, pool, path := dispositionFixture(t, "no-exit-invention", "r-noexit")

	if rec := reqAs(t, h, http.MethodPost, path, "sec-admins", `{"outcome":"success"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("kill = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	var exitCode sql.NullInt64
	if err := pool.QueryRow(`SELECT exit_code FROM runs WHERE id='r-noexit'`).Scan(&exitCode); err != nil {
		t.Fatal(err)
	}
	if exitCode.Valid {
		t.Errorf("exit_code = %d after a Success disposition, want NULL — the process may still be "+
			"alive, so an exit status here would assert something unobserved", exitCode.Int64)
	}
}

// TestKillRejectsAnUnknownDisposition — the enum is closed. 'stopped' is the
// obvious near-miss: it is reaction-side vocabulary (§2.3) and NEVER a
// runs.status value, so accepting it here would write a status that violates
// the CHECK constraint.
func TestKillRejectsAnUnknownDisposition(t *testing.T) {
	for _, bad := range []string{`{"outcome":"stopped"}`, `{"outcome":"cancelled"}`, `{"outcome":"danger"}`, `{"outcome":"KILLED"}`} {
		h, pool, path := dispositionFixture(t, "strict-enum", "r-strict")
		rec := reqAs(t, h, http.MethodPost, path, "sec-admins", bad)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("kill with %s = %d, want 422", bad, rec.Code)
		}
		if status, _ := runStatusAndKiller(t, pool, "r-strict"); status != "running" {
			t.Errorf("run status after a REJECTED disposition = %q, want running — a 422 must not "+
				"have written anything", status)
		}
	}
}

// TestKillRejectsAMalformedBody — F1's rule: an optional body tolerates empty,
// never garbage. A malformed body must not silently fall through to the default
// disposition, or a typo'd client would stop runs it only meant to inspect.
func TestKillRejectsAMalformedBody(t *testing.T) {
	h, pool, path := dispositionFixture(t, "malformed-body", "r-malformed")

	rec := reqAs(t, h, http.MethodPost, path, "sec-admins", `{"outcome":`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("kill with malformed JSON = %d, want 422", rec.Code)
	}
	if status, _ := runStatusAndKiller(t, pool, "r-malformed"); status != "running" {
		t.Errorf("run status after a malformed body = %q, want running", status)
	}
}

// TestKillOfAnAlreadyTerminalRunIs409 — RX-6, through the front door.
//
// The route's SELECT filters to queued/running, so a run that is already
// terminal produces no_active_run rather than silently "succeeding" and
// rewriting a real outcome as an asserted one. This is the reachable half of
// the guard; TestGuardedKillWriteIgnoresATerminalRun pins the write clause that
// closes the race the SELECT cannot.
func TestKillOfAnAlreadyTerminalRunIs409(t *testing.T) {
	h, pool, path := dispositionFixture(t, "already-done", "r-done")
	if _, err := pool.Exec(`UPDATE runs SET status='success', completed_at='2026-01-01T00:01:00Z' WHERE id='r-done'`); err != nil {
		t.Fatal(err)
	}

	rec := reqAs(t, h, http.MethodPost, path, "sec-admins", `{"outcome":"failure"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("kill of a finished run = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if status, killedBy := runStatusAndKiller(t, pool, "r-done"); status != "success" || killedBy.Valid {
		t.Errorf("finished run after a refused kill = (%q, killed_by valid=%v), want (success, false) — "+
			"a completed run must not be rewritten as failed by a kill that lost the race",
			status, killedBy.Valid)
	}
}

// TestGuardedKillWriteIgnoresATerminalRun — RX-6's write clause, pinned
// directly.
//
// The race it closes is not reachable through the HTTP layer in a test: it needs
// the run to reach a terminal state BETWEEN the route's SELECT and its UPDATE.
// So this asserts the property the guard provides — the statement is a no-op
// against a row that is no longer active — rather than simulating the interleave.
// Without `AND status IN ('queued','running')` this write would clobber a run
// that completed a millisecond before the operator's click, which is exactly the
// live bug §6 records.
func TestGuardedKillWriteIgnoresATerminalRun(t *testing.T) {
	_, pool, _ := dispositionFixture(t, "guard-clause", "r-guard")
	if _, err := pool.Exec(`UPDATE runs SET status='success' WHERE id='r-guard'`); err != nil {
		t.Fatal(err)
	}

	res, err := pool.Exec(`
		UPDATE runs SET status = ?, killed_by = ?, completed_at = ?
		WHERE id = ? AND status IN ('queued','running')`,
		"failure", "operator@example.com", "2026-01-01T00:02:00Z", "r-guard")
	if err != nil {
		t.Fatal(err)
	}
	n, _ := res.RowsAffected()
	if n != 0 {
		t.Errorf("guarded kill write touched %d rows of a terminal run, want 0", n)
	}
	if status, killedBy := runStatusAndKiller(t, pool, "r-guard"); status != "success" || killedBy.Valid {
		t.Errorf("terminal run after the guarded write = (%q, killed_by valid=%v), want (success, false)",
			status, killedBy.Valid)
	}
}

// TestRunEndActivityOutcomeFollowsTheDisposition — RX-7.
//
// The activity outcome was hardcoded 'failure'. History and the activity trail
// have to agree with the status that was written, or the audit record contradicts
// the run it describes.
//
// The asymmetry is deliberate and is the point of dispositionOutcome: an
// UNCLASSIFIED kill still records 'failure', because `killed` means exactly what
// it always meant and stays in the danger family the filters and health rollup
// already put it in (RX-Q7). Only a disposition the operator actively chose
// moves the outcome.
func TestRunEndActivityOutcomeFollowsTheDisposition(t *testing.T) {
	for _, tc := range []struct{ outcome, wantActivityOutcome string }{
		{"killed", "failure"},
		{"failure", "failure"},
		{"success", "success"},
		{"warning", "warning"},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			h, pool, path := dispositionFixture(t, "activity-"+tc.outcome, "r-act-"+tc.outcome)
			if rec := reqAs(t, h, http.MethodPost, path, "sec-admins", `{"outcome":"`+tc.outcome+`"}`); rec.Code != http.StatusAccepted {
				t.Fatalf("kill = %d (%s)", rec.Code, rec.Body.String())
			}

			var gotOutcome, gotKilledBy sql.NullString
			if err := pool.QueryRow(`
				SELECT outcome, killed_by FROM activity
				WHERE kind = 'run-end' AND trace_id = ?`, "r-act-"+tc.outcome).Scan(&gotOutcome, &gotKilledBy); err != nil {
				t.Fatalf("read run-end activity: %v", err)
			}
			if gotOutcome.String != tc.wantActivityOutcome {
				t.Errorf("activity outcome for disposition %q = %q, want %q",
					tc.outcome, gotOutcome.String, tc.wantActivityOutcome)
			}
			if gotKilledBy.String == "" {
				t.Errorf("activity killed_by empty for disposition %q — the human is recorded on "+
					"every path, including the ones whose outcome reads success", tc.outcome)
			}
		})
	}
}

// TestStopRecordsADuration — the observability half of RX-6.
//
// Before the guard, a stopped run sometimes acquired a duration by ACCIDENT: the
// runner's late final log overwrote the row and supplied one, along with the
// status it was clobbering. Closing that race correctly also closed the accident,
// so the duration is now written deliberately — otherwise every stopped run shows
// a blank Duration in History, which reads as a bug in the run rather than a
// consequence of how it ended.
func TestStopRecordsADuration(t *testing.T) {
	h, pool, path := dispositionFixture(t, "timed-stop", "r-timed")
	// A run that actually started, an hour ago.
	if _, err := pool.Exec(`UPDATE runs SET started_at = ? WHERE id='r-timed'`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	if rec := reqAs(t, h, http.MethodPost, path, "sec-admins", `{"outcome":"success"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("kill = %d (%s)", rec.Code, rec.Body.String())
	}
	var duration sql.NullInt64
	if err := pool.QueryRow(`SELECT duration_ms FROM runs WHERE id='r-timed'`).Scan(&duration); err != nil {
		t.Fatal(err)
	}
	if !duration.Valid || duration.Int64 <= 0 {
		t.Errorf("duration_ms after stopping a started run = %v, want a positive elapsed time", duration)
	}
}

// TestStopOfANeverStartedRunLeavesDurationNull — the other half: a queued run
// that never started has no elapsed time, and inventing one would be the same
// class of lie as synthesising an exit code.
func TestStopOfANeverStartedRunLeavesDurationNull(t *testing.T) {
	h, pool, path := dispositionFixture(t, "never-started", "r-queued")
	if _, err := pool.Exec(`UPDATE runs SET status='queued', started_at=NULL WHERE id='r-queued'`); err != nil {
		t.Fatal(err)
	}

	if rec := reqAs(t, h, http.MethodPost, path, "sec-admins", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("kill = %d (%s)", rec.Code, rec.Body.String())
	}
	var duration sql.NullInt64
	if err := pool.QueryRow(`SELECT duration_ms FROM runs WHERE id='r-queued'`).Scan(&duration); err != nil {
		t.Fatal(err)
	}
	if duration.Valid {
		t.Errorf("duration_ms = %d for a run that never started, want NULL", duration.Int64)
	}
}

// TestDispositionFlowsIntoTheJobHealthRollup — RX-8's rollup half.
//
// The job list derives a display status from the latest run, folding
// failure/killed/danger/warning into 'danger'. That fold reads `status`, so a
// disposition moves the rollup for free — a job whose last run was stopped and
// recorded as a success reads healthy, and one stopped without a disposition
// still reads danger.
//
// Pinned because this is the most visible consequence of dispositions outside
// History: an operator who stops a wedged run and marks it done expects the
// job's tile to stop showing red, and an operator who just stops something
// expects it to keep showing red.
func TestDispositionFlowsIntoTheJobHealthRollup(t *testing.T) {
	for _, tc := range []struct{ disposition, wantRollup string }{
		{"success", "success"},
		{"warning", "danger"},
		{"failure", "danger"},
		{"killed", "danger"},
	} {
		t.Run(tc.disposition, func(t *testing.T) {
			h, _, path := dispositionFixture(t, "rollup-"+tc.disposition, "r-roll-"+tc.disposition)
			if rec := reqAs(t, h, http.MethodPost, path, "sec-admins", `{"outcome":"`+tc.disposition+`"}`); rec.Code != http.StatusAccepted {
				t.Fatalf("kill = %d (%s)", rec.Code, rec.Body.String())
			}

			rec := reqAs(t, h, http.MethodGet, "/api/v1/jobs", "sec-admins", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /jobs = %d", rec.Code)
			}
			var env struct {
				Items []struct {
					Name   string `json:"name"`
					Status string `json:"status"`
				} `json:"items"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode jobs: %v", err)
			}
			var got string
			for _, j := range env.Items {
				if j.Name == "rollup-"+tc.disposition {
					got = j.Status
				}
			}
			if got != tc.wantRollup {
				t.Errorf("job rollup after a stop recorded as %q = %q, want %q",
					tc.disposition, got, tc.wantRollup)
			}
		})
	}
}

// TestStoppedFilterIsOrthogonalToStatus — RX-22, and the reason it is a separate
// filter rather than a Result option.
//
// A dispositioned stop can carry ANY status, so "was this stopped by a human?"
// is not a value the status filter could hold. The two must compose:
// ?status=success&stopped=true is "runs we stopped and then called fine", which
// is precisely the state Phase A creates and the one an auditor will ask for.
func TestStoppedFilterIsOrthogonalToStatus(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled) VALUES ('mixed','cronomicon','bash',NULL,1)`)
	// Three terminal runs: an ordinary success, a stop recorded as success, and
	// an unclassified stop. Only the middle one is invisible to status alone.
	exec(`INSERT INTO runs (id, job_name, job_source, run_type, status, killed_by, triggered_by,
	                        trigger_kind, executor, created_at)
	      VALUES ('r-ok','mixed','cronomicon','bash','success',NULL,'t','manual','ssh','2026-01-01T00:00:00Z'),
	             ('r-stopped-ok','mixed','cronomicon','bash','success','ops@example.com','t','manual','ssh','2026-01-01T00:00:01Z'),
	             ('r-stopped','mixed','cronomicon','bash','killed','ops@example.com','t','manual','ssh','2026-01-01T00:00:02Z')`)

	ids := func(query string) map[string]bool {
		t.Helper()
		rec := reqAs(t, h, http.MethodGet, query, "sec-admins", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d (%s)", query, rec.Code, rec.Body.String())
		}
		var env struct {
			Items []struct {
				TraceID string `json:"traceId"`
				ID      string `json:"id"`
			} `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode %s: %v", query, err)
		}
		got := map[string]bool{}
		for _, it := range env.Items {
			id := it.TraceID
			if id == "" {
				id = it.ID
			}
			got[id] = true
		}
		return got
	}

	stopped := ids("/api/v1/runs?job=mixed&stopped=true")
	if !stopped["r-stopped-ok"] || !stopped["r-stopped"] {
		t.Errorf("stopped=true missed a human-stopped run: %v", stopped)
	}
	if stopped["r-ok"] {
		t.Error("stopped=true returned a run nobody stopped")
	}

	notStopped := ids("/api/v1/runs?job=mixed&stopped=false")
	if !notStopped["r-ok"] || notStopped["r-stopped-ok"] || notStopped["r-stopped"] {
		t.Errorf("stopped=false should return exactly the un-stopped run, got %v", notStopped)
	}

	// The composition that motivates the whole filter.
	composed := ids("/api/v1/runs?job=mixed&status=success&stopped=true")
	if !composed["r-stopped-ok"] || composed["r-ok"] || composed["r-stopped"] {
		t.Errorf("status=success&stopped=true should return exactly the stop recorded as success, got %v", composed)
	}

	// Absent the param, nothing is filtered — the default must stay unfiltered.
	all := ids("/api/v1/runs?job=mixed")
	if len(all) != 3 {
		t.Errorf("unfiltered listing returned %d runs, want 3 — the new param must not filter by default", len(all))
	}
}

// ── RX-17: reaction provenance over the wire ───────────────────────────────

// The because-of link must be SERVED, in both directions, for jobs and for
// workflows. The forward link is a field on the run row; the reverse is a
// filter, because a run has no list of its children and building one would
// duplicate a fact the child already owns.
func TestReactionProvenanceIsServedBothDirections(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO jobs (name, source, run_type, enabled) VALUES ('up','cronomicon','bash',1),('down','cronomicon','bash',1)`)
	exec(`INSERT INTO runs (id, job_name, job_source, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES ('r-cause','up','cronomicon','bash','success','scheduler','scheduled','2026-01-01T00:00:00Z')`)
	// Two effects, one a job run and one a workflow run — the graph crosses
	// tables, so answering only for jobs answers half of it.
	exec(`INSERT INTO runs (id, job_name, job_source, run_type, status, triggered_by, trigger_kind,
	                        reacted_to_run_id, reaction_depth, created_at)
	      VALUES ('r-effect','down','cronomicon','bash','success','reactor','reaction','r-cause',1,'2026-01-01T00:01:00Z')`)
	exec(`INSERT INTO workflow_runs (id, workflow_name, workflow_source, status, triggered_by, trigger_kind,
	                                 reacted_to_run_id, reaction_depth, created_at)
	      VALUES ('wfr-effect','cleanup','cronomicon','success','reactor','reaction','r-cause',1,'2026-01-01T00:01:00Z')`)

	get := func(path string) []map[string]any {
		t.Helper()
		rec := reqAs(t, h, http.MethodGet, path, "sec-admins", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d (%s)", path, rec.Code, rec.Body.String())
		}
		var env struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return env.Items
	}

	// Forward: the effect names its cause.
	runs := get("/api/v1/runs?job=down")
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if runs[0]["reactedToRunId"] != "r-cause" {
		t.Errorf("reactedToRunId = %v, want r-cause — without it History cannot answer "+
			"\"why did this run?\"", runs[0]["reactedToRunId"])
	}
	if runs[0]["reactionDepth"] != float64(1) {
		t.Errorf("reactionDepth = %v, want 1", runs[0]["reactionDepth"])
	}

	// Reverse: what did the cause trigger? Across BOTH tables.
	if got := get("/api/v1/runs?reactedTo=r-cause"); len(got) != 1 || got[0]["traceId"] != "r-effect" {
		t.Errorf("?reactedTo returned %+v, want just r-effect", got)
	}
	if got := get("/api/v1/workflow-runs?reactedTo=r-cause"); len(got) != 1 || got[0]["traceId"] != "wfr-effect" {
		t.Errorf("workflow ?reactedTo returned %+v, want just wfr-effect", got)
	}
	// A run that caused nothing returns nothing, rather than everything.
	if got := get("/api/v1/runs?reactedTo=r-effect"); len(got) != 0 {
		t.Errorf("?reactedTo for a childless run returned %d rows, want 0", len(got))
	}

	// The trigger-kind filter, which is what the History tab's Reaction filter uses.
	if got := get("/api/v1/runs?triggerKind=reaction"); len(got) != 1 || got[0]["traceId"] != "r-effect" {
		t.Errorf("?triggerKind=reaction returned %+v, want just r-effect", got)
	}
	if got := get("/api/v1/runs?triggerKind=scheduled"); len(got) != 1 || got[0]["traceId"] != "r-cause" {
		t.Errorf("?triggerKind=scheduled returned %+v, want just r-cause", got)
	}
	// Absent the params, nothing is filtered.
	if got := get("/api/v1/runs"); len(got) != 2 {
		t.Errorf("unfiltered runs = %d, want 2 — the new params must not filter by default", len(got))
	}
}

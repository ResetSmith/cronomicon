package api

import (
	"context"
	"fmt"
	"os"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/entitycode"
	"github.com/ResetSmith/cronomicon/internal/runner"
	"github.com/ResetSmith/cronomicon/internal/scheduler"
	"github.com/ResetSmith/cronomicon/internal/sshexec"
)

// recordSSHTestRun mirrors an SSH "Test connection" probe into History/Executions
// as a terminal runs row (kind='ssh-test') with the probe output stored as the
// run's Log Output (V1.1-2). It is best-effort: any failure to record is logged
// and swallowed so it never fails the Test request, which has already returned
// its own result via the status pill.
//
// The synthetic run sets a CHECK-valid run_type ('bash') even though no bash ran
// — the kind discriminator is what lets History badge it as a Test and lets the
// Dashboard/job-failure metrics exclude diagnostics (excluded in the frontend).
func (s *Server) recordSSHTestRun(ctx context.Context, targetName string, probe sshexec.ProbeResult, actor string) {
	traceID := db.NewTraceID()

	// verified | reachable ⇒ success; cred_error | conn_error ⇒ failure.
	// reachable is the keyless tier's expected good outcome (TT) — the endpoint
	// answered and its host key pinned; there was no credential to exercise.
	status := "failure"
	outcome := "failure"
	if probe.Status == sshexec.StatusVerified || probe.Status == sshexec.StatusReachable {
		status = "success"
		outcome = "success"
	}

	jobName := "SSH Test — " + targetName
	at := probe.CheckedAt // RFC3339 UTC, set by the probe

	// A terminal run: started_at == completed_at == created_at == CheckedAt;
	// duration is the probe's measured dial+auth latency. RR-2: written through
	// the one runs writer like every other producer — Kind and EntityCode are
	// what make it a diagnostic rather than a job run, and Terminal is why it
	// carries no dispatch policy.
	latency := probe.LatencyMs
	if _, err := scheduler.InsertRun(ctx, s.db, scheduler.RunRow{
		EnqueueParams: scheduler.EnqueueParams{
			JobName:     jobName,
			RunType:     "bash",
			Executor:    "ssh",
			TriggerKind: "manual",
			TriggeredBy: actor,
			TargetHost:  targetName,
		},
		TraceID:    traceID,
		Kind:       "ssh-test",
		EntityCode: entitycode.SystemCode,
		Status:     status,
		Terminal:   true,
		CreatedAt:  at,
		DurationMs: &latency,
	}); err != nil {
		s.log.Error("record ssh-test run", "trace_id", traceID, "target", targetName, "error", err)
		return
	}

	// Write the probe output as the run's log so GET /runs/{traceId}/log serves it.
	// Reads the same in-process value the runner/sshexec services hold rather than
	// re-querying the DB, so all three writers agree the instant a path change is
	// applied (LU-5) — they previously used two different resolution schedules.
	logDir := s.logDirValue(ctx)
	if err := writeSSHTestLog(logDir, traceID, targetName, probe); err != nil {
		s.log.Error("write ssh-test log", "trace_id", traceID, "target", targetName, "error", err)
		// Non-fatal: the run row still stands; the log just won't have content.
	}

	// Mirror the executor's run-end activity row so the Activity feed shows it.
	// At is the probe's CheckedAt, the same instant stamped on the run above —
	// the pair must not drift apart in the History timeline.
	if err := auditlog.WriteActivity(ctx, s.db, auditlog.ActivityParams{
		At:      at,
		Kind:    "run-end",
		Outcome: outcome,
		Actor:   "ssh-test",
		JobName: jobName,
		Target:  targetName,
		TraceID: traceID,
	}); err != nil {
		s.log.Error("record ssh-test activity", "trace_id", traceID, "target", targetName, "error", err)
	}
}

// writeSSHTestLog writes the probe output into the same place
// GET /runs/{traceId}/log reads from. Content is a few operator-safe lines: the
// target, the probe status, the message, and the latency.
//
// LU-7: an SSH connection test records a run but belongs to no job, so there is
// no entity to allocate a code for — it lands in the _system bucket. The run row
// is stamped with the same value, so the read path resolves the identical path
// without special-casing kind='ssh-test'.
func writeSSHTestLog(logDir, traceID, targetName string, probe sshexec.ProbeResult) error {
	// LU-12: this traceID is server-minted, so the check is belt-and-braces —
	// but it is the third site that turns an id into a filename, and leaving one
	// unguarded is how the guarantee quietly stops being true.
	path, err := runner.LogPath(logDir, entitycode.SystemCode, traceID)
	if err != nil {
		return fmt.Errorf("write ssh-test log: %w", err)
	}
	if err := runner.EnsureLogDir(logDir, entitycode.SystemCode); err != nil {
		return err
	}
	body := fmt.Sprintf(
		"SSH connection test — %s\nStatus:  %s\nMessage: %s\nLatency: %dms\n",
		targetName, probe.Status, probe.Message, probe.LatencyMs)
	return os.WriteFile(path, []byte(body), 0o640)
}

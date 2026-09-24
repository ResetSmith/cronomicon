package runner

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/logarchive"
	"github.com/ResetSmith/cronomicon/internal/metrics"
	"github.com/ResetSmith/cronomicon/internal/notify"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"github.com/ResetSmith/cronomicon/internal/runref"
)

// HandleIngestLog handles chunked POST log ingest from a runner (T6).
// POST /api/v1/runs/{traceId}/log  (runner bearer auth)
//
// Flow:
//  1. Validate the run exists and is in 'running' state.
//  2. Honor X-Resume-Offset: if the log file already has N bytes, the runner
//     must resume from exactly N bytes; mismatch → 409.
//  3. Stream the body, redact sensitive values on the fly, append to the
//     per-run log file.
//  4. On stream close parse the last line as the trailing JSON envelope.
//  5. Transition the run to its terminal status and emit run-end activity.
func (s *Service) HandleIngestLog(w http.ResponseWriter, r *http.Request) {
	traceID := r.PathValue("traceId")
	if !ValidTraceID(traceID) {
		httpx.Fail(w, http.StatusBadRequest, "invalid_trace_id", "malformed trace id")
		return
	}

	// Verify the run exists and is running.
	var jobName, scope, runnerID, jobSource, scriptRef string
	var status string
	// entity_code names this run's log folder (LU-7). It was stamped at enqueue,
	// so it is a property of the run rather than something re-derived here — a
	// job deleted mid-run must not move its own in-flight log.
	var entityCode string
	err := s.db.QueryRowContext(r.Context(), `
		SELECT job_name, COALESCE(scope,''), COALESCE(runner_id,''), status,
		       COALESCE(job_source,''), COALESCE(script_ref,''), COALESCE(entity_code,'')
		FROM runs WHERE id = ?`, traceID).
		Scan(&jobName, &scope, &runnerID, &status, &jobSource, &scriptRef, &entityCode)
	if err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if status != "running" {
		httpx.Fail(w, http.StatusConflict, "conflict",
			fmt.Sprintf("run is in status %q, not running", status))
		return
	}

	// Ownership (R1.4): only the runner that claimed this run may stream logs to
	// it — otherwise any valid runner token could spoof logs onto any run. 404
	// (not 403) so a non-owning runner cannot probe other runs' existence.
	callerRunnerID, ok := auth.RunnerIDFrom(r.Context())
	if !ok || callerRunnerID != runnerID {
		httpx.Fail(w, http.StatusNotFound, "not_found", "run not found")
		return
	}

	// M5: the fail-closed decision is authoritative to the DISPATCH-time flag
	// (runs.injects_secret, set when the manifest shipped a secret VALUE), NOT to a
	// live re-enumeration of bindings. A binding deleted/edited or its job pruned
	// mid-run yields a clean-empty enumeration that must NOT flip a secret-bearing
	// run to the lenient path — the previously-injected (vault-source) value would
	// then persist un-redacted in remaining chunks. The flag also governs over the
	// live kill-switch (L2): a run dispatched while ON stays fail-closed even after
	// the switch is flipped OFF and the process restarts.
	var injectedSecretAtDispatch bool
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT injects_secret FROM runs WHERE id = ?`, traceID).Scan(&injectedSecretAtDispatch)

	// Build redactor for this run's scope (T7/S7): stored-secret values plus
	// multi-line env_var key material. Variable values and per-run prompt answers
	// are log-visible by design (D7) and are not in the dictionary. P1.5: also
	// seed the per-run resolver output so injected values the global dictionary
	// does not carry (vault-source secrets, key material) are masked here too —
	// symmetric with the SSH executor's openLog. This is best-effort masking of
	// the CURRENT values; the fail-closed guarantee comes from the dispatch flag
	// above, not this enumeration.
	injectRedact, _, injectErr := s.runInjectionRedaction(r.Context(), traceID, jobName, jobSource, scriptRef, scope)
	if injectErr != nil {
		s.log.Warn("ingest redaction: resolve failed; global dictionary still applies",
			"trace_id", traceID, "error", injectErr)
	}
	redactor, err := NewRedactor(r.Context(), s.db, s.cfg, scope, injectRedact...)
	redactorBroken := err != nil
	if redactorBroken {
		s.log.Error("build redactor", "trace_id", traceID, "error", err)
		redactor = noopRedactor
	}

	// Fail-closed (P1.5/M5, mirrors sshexec.openLog): a run KNOWN to have injected a
	// secret at dispatch must NOT persist raw bytes while its redaction dictionary is
	// incomplete. Incomplete means: the per-run resolve failed (injectErr — a vault
	// outage), the base redactor could not be built (redactorBroken), OR the current
	// resolve yields NO injected secret value (injectRedact empty) though dispatch
	// injected one — the binding-deleted / job-pruned / kill-switch-off cases the old
	// live-enumeration check silently passed. Refuse the chunk (persist nothing,
	// advance no offset) so the agent re-sends once the fault clears. Runs that never
	// injected a secret keep the lenient (best-effort) path. Availability tradeoff §8.
	if injectedSecretAtDispatch && (injectErr != nil || redactorBroken || len(injectRedact) == 0) {
		s.log.Error("ingest: incomplete redaction for a secret-bearing run; failing closed",
			"trace_id", traceID, "resolve_error", injectErr, "redactor_error", err,
			"resolved_values", len(injectRedact))
		httpx.Fail(w, http.StatusServiceUnavailable, "redactor_unavailable",
			"cannot fully redact a secret-bearing run's output right now; refusing to persist it un-redacted")
		return
	}

	// Ensure the run's log directory exists.
	if err := s.ensureLogDir(entityCode); err != nil {
		s.log.Error("ensure log dir", "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "log dir unavailable")
		return
	}
	// LU-7: explain the folder on disk, once, at creation. Best-effort.
	WriteEntityMeta(r.Context(), s.db, s.LogDir(), entityCode)

	logPath, err := s.logPath(entityCode, traceID)
	if err != nil {
		// LU-12: the trace ID is an unvalidated path parameter concatenated into
		// a filename. Reject a malformed one here rather than trusting the
		// router's single-segment wildcard to be the only guard.
		httpx.Fail(w, http.StatusBadRequest, "invalid_trace_id", "malformed trace id")
		return
	}

	// PP-M8: validate X-Resume-Offset against the stored raw (pre-redaction) byte
	// count rather than fi.Size() (the redacted file size). The two diverge whenever
	// a secret appears in log output: the agent tracks raw bytes sent while the old
	// code compared against redacted bytes on disk — causing spurious 409s after any
	// redaction event. Using log_raw_offset keeps both sides in the same unit.
	var rawOffset int64
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(log_raw_offset, 0) FROM runs WHERE id = ?`, traceID).Scan(&rawOffset)

	// SU-9: cap the cumulative per-run log size. This endpoint is intentionally
	// exempt from the global 2 MiB body cap (a run streams many chunks), which
	// removed the only ceiling on per-run log growth — a rogue/compromised runner
	// with a valid token could otherwise stream unbounded output and fill the disk.
	// This is the cross-request fast stop: once a run trips the cap its offset is
	// pinned at the ceiling (below), so every later chunk short-circuits here with a
	// 413 before opening the file. The strict per-file bound is enforced in the scan
	// loop against the on-disk size. A cap of 0 disables the check.
	maxRunLog := int64(s.cfg.MaxRunLogBytes)
	if maxRunLog > 0 && rawOffset >= maxRunLog {
		httpx.Fail(w, http.StatusRequestEntityTooLarge, "log_limit_exceeded",
			"run log has reached the maximum size; further output is discarded")
		return
	}

	if resumeHdr := r.Header.Get("X-Resume-Offset"); resumeHdr != "" {
		resumeOffset, err := strconv.ParseInt(resumeHdr, 10, 64)
		if err != nil || resumeOffset < 0 {
			httpx.Fail(w, http.StatusBadRequest, "bad_request",
				"X-Resume-Offset must be a non-negative integer")
			return
		}
		if resumeOffset != rawOffset {
			httpx.JSON(w, http.StatusConflict, map[string]any{
				"code":            "resume_offset_mismatch",
				"message":         "X-Resume-Offset does not match persisted raw byte count",
				"persistedOffset": rawOffset,
			})
			return
		}
	}

	// Open the log file for append (rawOffset > 0) or fresh write (rawOffset == 0).
	// When rawOffset == 0 we truncate any stale partial file from a previous crashed
	// handler so retransmitted bytes don't produce duplicate content.
	openFlag := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if rawOffset == 0 {
		openFlag = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	f, err := os.OpenFile(logPath, openFlag, 0640)
	if err != nil && os.IsNotExist(err) {
		// The folder vanished between MkdirAll above and this open. The nightly
		// reaper prunes directories it finds empty, and a run that has created its
		// folder but not yet its first file is momentarily exactly that. The window
		// is microseconds against a once-a-day sweep, but the cost of losing the
		// race is a failed ingest, so recreate and retry once rather than 500.
		if mkErr := s.ensureLogDir(entityCode); mkErr == nil {
			f, err = os.OpenFile(logPath, openFlag, 0640)
		}
	}
	if err != nil {
		s.log.Error("open log file", "path", logPath, "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "log file open error")
		return
	}
	defer f.Close()

	// SU-9: the cap bounds the PERSISTED (redacted) file, so measure growth from the
	// file's current on-disk size — NOT log_raw_offset, which counts raw pre-redaction
	// bytes (redaction shrinks long secrets but expands very short ones, so raw bytes
	// are not the on-disk size). Fresh writes (O_TRUNC) start at 0.
	var persistedStart int64
	if fi, statErr := f.Stat(); statErr == nil {
		persistedStart = fi.Size()
	}

	// Stream body, redact, and write to file.
	// We buffer line-by-line so we can parse the trailing envelope and never
	// log raw bytes pre-redaction (S7). The counting reader tracks raw bytes
	// consumed for log_raw_offset bookkeeping (PP-M8).
	var lastLine string
	var ingestBytes int
	var truncated bool
	outputs := map[string]string{} // A12 captured inter-job outputs (raw values)
	counter := &rawByteCounter{r: r.Body}
	scanner := bufio.NewScanner(counter)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // 1 MiB max line

	for scanner.Scan() {
		raw := scanner.Bytes()
		// Copy raw so redactor doesn't mutate the scanner buffer.
		line := make([]byte, len(raw))
		copy(line, raw)

		// A12: parse output markers from the RAW line (before redaction) so the
		// captured value is exact; the line is still persisted redacted below.
		if k, v, ok := execspec.ParseOutputMarker(string(line)); ok {
			outputs[k] = v
		}

		// Redact before persisting (S7: never log raw pre-redaction bytes).
		redacted := redactor.Redact(line)

		// SU-9: stop appending once this line would push the persisted log past the
		// ceiling. Measured on the on-disk (redacted) size, so the file is strictly
		// bounded (plus the one truncation-notice line). Write the notice and bail to 413.
		if maxRunLog > 0 && persistedStart+int64(ingestBytes)+int64(len(redacted))+1 > maxRunLog {
			_, _ = f.WriteString("amadeus: run log size limit reached; further output discarded\n")
			truncated = true
			break
		}

		n, err := f.Write(append(redacted, '\n'))
		if err != nil {
			s.log.Error("write log line", "trace_id", traceID, "error", err)
			break
		}
		ingestBytes += n
		lastLine = string(redacted)
	}

	// SU-9: the run tripped its log-size ceiling. Pin the raw offset AT the ceiling
	// so every subsequent chunk short-circuits at the pre-check above (no duplicate
	// appends on retry), account the bytes written this request, and refuse with 413.
	// We do NOT finalize the run here — a later chunk may still carry a normal
	// terminal envelope; if the runner gives up, the orphan reaper reconciles it.
	if truncated {
		if _, dbErr := s.db.ExecContext(r.Context(),
			`UPDATE runs SET log_raw_offset = ? WHERE id = ?`, maxRunLog, traceID); dbErr != nil {
			s.log.Error("cap log_raw_offset", "trace_id", traceID, "error", dbErr)
		}
		metrics.AddLogIngest(ingestBytes, 1)
		httpx.Fail(w, http.StatusRequestEntityTooLarge, "log_limit_exceeded",
			"run log has reached the maximum size; further output is discarded")
		return
	}
	// H2/DEC-2 — refuse: an ::amadeus-output:: value that carries an injected
	// secret would propagate that secret VERBATIM into outputs_json, into a child
	// step's plaintext env_json (workflow engine), and into the run-detail API —
	// around allow_secret_injection and the §8 "never persist an injected value to
	// env_json" invariant, via the natural idiom
	// `echo "::amadeus-output name=TOKEN::$CRONOMICON_SECRET_API"`. Fail the run closed
	// at this earliest choke point (before outputs_json is written): drop the
	// captured outputs and mark the run failed so nothing propagates. The offending
	// value is already masked in the persisted log (the redactor is seeded with the
	// injected dictionary); we surface only the output NAME, never the value.
	leakedOutput := execspec.FirstOutputLeakingSecret(outputs, injectRedact)
	if leakedOutput != "" {
		s.log.Error("ingest: captured output would leak an injected secret; failing run closed",
			"trace_id", traceID, "output", leakedOutput)
		if _, werr := f.WriteString(fmt.Sprintf(
			"amadeus: output %q would leak an injected secret value; refusing to capture it and failing the run\n",
			leakedOutput)); werr != nil {
			s.log.Error("write leak-refusal line", "trace_id", traceID, "error", werr)
		}
		outputs = nil // do NOT persist — nothing carries the secret forward
	}

	// A12: persist captured outputs before flipping the run terminal (no-op if empty).
	if len(outputs) > 0 {
		if b, mErr := json.Marshal(outputs); mErr == nil {
			if _, uErr := s.db.ExecContext(r.Context(), `UPDATE runs SET outputs_json = ? WHERE id = ?`, string(b), traceID); uErr != nil {
				s.log.Error("write run outputs", "trace_id", traceID, "error", uErr)
			}
		}
	}
	metrics.AddLogIngest(ingestBytes, 1) // one POST = one chunk (T6/C.2)
	scanErr := scanner.Err()
	if scanErr != nil && scanErr != io.ErrUnexpectedEOF {
		s.log.Error("read log stream", "trace_id", traceID, "error", scanErr)
	}

	// PP-M8: update the raw-byte offset ONLY on a clean EOF (all bytes received
	// without a connection error). A mid-stream drop leaves log_raw_offset at its
	// prior value so the agent re-sends from the last confirmed raw position rather
	// than resuming at an inconsistent mid-stream point.
	if scanErr == nil {
		if _, dbErr := s.db.ExecContext(r.Context(),
			`UPDATE runs SET log_raw_offset = log_raw_offset + ? WHERE id = ?`,
			counter.n, traceID); dbErr != nil {
			s.log.Error("update log_raw_offset", "trace_id", traceID, "error", dbErr)
		}
	}

	// H2/DEC-2: a refused output-leak overrides the envelope's own exit status —
	// the run is failed closed regardless of how the body exited, with a distinct
	// reason the run detail can surface.
	if leakedOutput != "" {
		s.finalizeRun(r.Context(), traceID, jobName, scope, runnerID,
			"failure", nil, 0, "output_secret_leak")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// v12 live tailing: a partial chunk (X-Log-Partial: 1) is a mid-run flush,
	// not the whole stream — persist + advance the offset (done above) and stop
	// here. No envelope parse, no finalize: the run is still executing and the
	// agent will keep streaming. Only a v12 assignment that carried LiveLog
	// invites these, so an unmarked chunk keeps the pre-v12 contract below.
	// (The output-leak fail-closed path above deliberately still finalizes: a
	// leaked secret must stop the run at the earliest chunk that reveals it.)
	if r.Header.Get("X-Log-Partial") == "1" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Try to parse the trailing JSON envelope from the last line (§6.3).
	env := parseTrailingEnvelope(lastLine)
	if env == nil {
		// No valid envelope — mark failure with reason log_stream_lost.
		s.finalizeRun(r.Context(), traceID, jobName, scope, runnerID,
			"failure", nil, 0, "log_stream_lost")
	} else {
		// Determine terminal status from exit code. A host_key_unverified reason
		// (Phase 5) rides the envelope and becomes the run's status reason, so the
		// run detail can offer "Scan & approve host keys".
		termStatus := exitCodeToStatus(env.ExitCode)
		s.finalizeRun(r.Context(), traceID, jobName, scope, runnerID,
			termStatus, &env.ExitCode, env.DurationMs, env.Reason)
	}

	w.WriteHeader(http.StatusNoContent)
}

// runInjectionRedaction re-resolves the run's declared secret/variable bindings so
// the ingest redactor masks whatever dispatch actually injected (P1.5). It returns
// the extra sensitive values to add to the redaction dictionary and whether the run
// injects ANY secret. Post-M5 the caller drives its fail-closed decision from the
// authoritative dispatch flag (runs.injects_secret) rather than this live
// enumeration; the returned values here are used only to (best-effort) seed the
// redactor with the CURRENT values, and injectsSecret remains for unit coverage.
//
// For a STORED secret this is belt-and-suspenders — secrets.RedactionValues already
// puts every stored secret in the global dictionary. It EARNS its keep for values
// the global dictionary does NOT contain: vault-source secret values (Phase 2, by
// construction) and, once delivered, key material. Variable values are log-safe (D7)
// and are deliberately excluded (they are not in resolved.Redact).
//
// Fail-closed reliability (v0.49.3 review): the returned injectsSecret must be a
// sound basis for the caller's fail-closed decision, so it is NOT derived solely
// from a query that can silently degrade. Two error shapes are distinguished:
//   - Binding ENUMERATION failed (kinds unknown): we cannot prove the run is
//     secret-free, so injectsSecret=true is returned WITH the error — the caller
//     fails closed. A transient reference_bindings error thus refuses the chunk
//     (the agent re-sends) rather than risk persisting an un-maskable injected value.
//   - Enumeration succeeded but RESOLVE failed: injectsSecret reflects the real
//     binding kinds, so a var-only run stays lenient while a secret-bearing run
//     fails closed (e.g. a Vault outage or a secret deleted mid-run).
//
// Injection off ⇒ (nil,false,nil). D8: key MATERIAL is resolved and its bytes enter
// resolved.Redact so the ingest redactor masks a delivered key echoed in output.
func (s *Service) runInjectionRedaction(ctx context.Context, runID, jobName, jobSource, scriptRef, scope string) (redact []string, injectsSecret bool, err error) {
	if !s.cfg.SecretsInjectionEnabled {
		return nil, false, nil
	}
	bindings, err := s.collectReferenceBindings(ctx, runID, jobName, jobSource, scriptRef)
	if err != nil {
		// Kinds unknown → assume the worst so the caller fails closed.
		return nil, true, err
	}
	// D8: keys are NO LONGER skipped here — delivered key MATERIAL is resolved into
	// resolved.Redact so the ingest redactor masks it, in lockstep with the manifest
	// delivering it. Skipping keys (as before D8) would let a job that echoes/cats a
	// delivered key file leak its bytes un-masked. A key-bearing run also counts as
	// secret-bearing for the fail-closed decision.
	for _, b := range bindings {
		if b.Kind == runref.KindSecret || b.Kind == runref.KindKey {
			injectsSecret = true
		}
	}
	// AG-Q8 — the run's frozen snapshot, same source the manifest path reads, so
	// the redaction dictionary covers exactly what was injected.
	runAgencies, aerr := runref.RunAgencies(ctx, s.db, runID)
	if aerr != nil {
		return nil, true, aerr
	}
	resolved, err := s.resolver.Resolve(ctx, nil, scope, runAgencies, bindings)
	if err != nil {
		return nil, injectsSecret, err
	}
	return resolved.Redact, injectsSecret, nil
}

// parseTrailingEnvelope attempts to parse the last log line as the §6.3 JSON
// envelope. Returns nil if the line is not valid envelope JSON.
func parseTrailingEnvelope(line string) *runnerproto.TrailingEnvelope {
	line = strings.TrimSpace(line)
	if len(line) == 0 || !strings.HasPrefix(line, "{") {
		return nil
	}
	var env runnerproto.TrailingEnvelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		return nil
	}
	// Validate envelope has at least an endedAt field.
	if env.EndedAt == "" {
		return nil
	}
	return &env
}

// exitCodeToStatus maps an exit code to a terminal run status.
func exitCodeToStatus(code int) string {
	if code == 0 {
		return "success"
	}
	return "failure"
}

// finalizeRun transitions a run to a terminal status and emits run-end activity.
// This is B4's ownership of the running→terminal transition seam.
func (s *Service) finalizeRun(ctx context.Context, traceID, jobName, scope, runnerID,
	status string, exitCode *int, durationMs int64, reason string) {

	ts := now()

	// 'danger' is an API-layer concept; DB status is always 'failure' for bad exits.
	outcome := status

	var exitCodeVal any
	if exitCode != nil {
		exitCodeVal = *exitCode
	}

	// RX-6 — status-guarded, mirroring sshexec's finalize and the workflow
	// finalize guard. The route-level check at the top of the log handler
	// ("run is in status %q, not running") is a check-then-act with a real
	// window: an operator stopping a run writes a terminal status from another
	// goroutine, and the runner's in-flight final log would land afterwards and
	// overwrite it. Before kill dispositions that silently rewrote 'killed' as
	// 'failure'; with them it would rewrite the operator's chosen outcome —
	// destroying the very assertion Phase A exists to record, on exactly the
	// runs people stop. RowsAffected == 0 means somebody else already owns this
	// run's terminal state, so we leave it alone and skip the run-end emission.
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs
		SET status = ?, completed_at = ?, duration_ms = ?, exit_code = ?, queued_reason = ?
		WHERE id = ? AND status = 'running'`,
		status, ts, durationMs, exitCodeVal, nullIf(reason), traceID)
	if err != nil {
		s.log.Error("finalize run", "trace_id", traceID, "error", err)
	}
	// "Somebody else owns this run's terminal state" and "the write failed" are
	// different facts and must not collapse into one flag. A failed write (a busy
	// DB, say) leaves the run 'running' with nobody having emitted its run-end —
	// so it keeps the pre-guard behaviour of emitting anyway, or a transient
	// SQLITE_BUSY would silently cost the operator their failure notification and
	// the RunFinished metric, and only self-heal when the reaper eventually calls
	// it runner_lost. Only a clean write that matched no row means we genuinely
	// lost the race and must stay quiet.
	lostTheRace := false
	if err == nil {
		if n, _ := res.RowsAffected(); n == 0 {
			lostTheRace = true
		}
	}

	// Decrement runner load.
	if runnerID != "" {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE runners SET load = MAX(0, load - 1) WHERE id = ?`, runnerID); err != nil {
			s.log.Error("decrement runner load", "runner_id", runnerID, "error", err)
		}
	}

	// Check if draining runner now has zero load → transition to offline (A6.4).
	if runnerID != "" {
		s.checkDrainComplete(ctx, runnerID)
	}

	// The load decrement and drain check above are deliberately NOT gated on the
	// race result: they account for the runner's slot, not the run's status. The
	// runner really did finish executing and release its slot even when someone
	// else won the terminal write, and skipping the decrement would leak load
	// until the runner restarted. This is the common case for a stopped run — the
	// operator's write lands first, then this streaming flush arrives.
	if lostTheRace {
		s.log.Info("finalize run: run already terminal (stopped or finalized elsewhere); "+
			"leaving its status and run-end emission alone",
			"trace_id", traceID, "attempted_status", status)
		return
	}

	// Emit run-end activity (B4 seam: B4 owns the run-end activity row). At is the
	// same ts written to runs.completed_at above — History pairs the two.
	_ = auditlog.WriteActivity(ctx, s.db, auditlog.ActivityParams{
		At:         ts,
		Kind:       "run-end",
		Outcome:    outcome,
		Actor:      "runner:" + runnerID,
		RunnerName: s.runnerNameOf(ctx, runnerID),
		JobName:    jobName,
		Scope:      scope,
		TraceID:    traceID,
	})

	s.emitRunTerminal(notify.RunEvent{
		TraceID: traceID, JobName: jobName, Scope: scope, Status: status, ExitCode: exitCode,
	})
}

// emitRunTerminal fires the cross-cutting signals for a run reaching a terminal
// status: the RunFinished metric (C.2) and async notification dispatch (C.1).
// Both finalizeRun and the drain-timeout path call it, so neither terminal path
// can silently skip metrics/alerts. nil notifier is a no-op.
func (s *Service) emitRunTerminal(ev notify.RunEvent) {
	metrics.RunFinished(ev.Status)
	if s.notifier != nil {
		s.notifier.RunEnded(ev)
	}
}

// nullIf returns nil if s is empty, else s (for sql nullable strings).
func nullIf(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// HandleGetLog serves the redacted log file (operator-facing).
// GET /api/v1/runs/{traceId}/log
func (s *Service) HandleGetLog(w http.ResponseWriter, r *http.Request) {
	traceID := r.PathValue("traceId")
	// LU-12: validate before the run lookup, not after. A malformed id is a bad
	// request regardless of whether a row happens to exist, and checking here is
	// what makes the guard reachable at all — the lookup below 404s first, so a
	// check placed only at the path build would be dead code. The logPath guard
	// stays as well, covering callers that don't come through this handler.
	if !ValidTraceID(traceID) {
		httpx.Fail(w, http.StatusBadRequest, "invalid_trace_id", "malformed trace id")
		return
	}

	// Verify the run exists and get its scope + log folder.
	var scope, archivedAt sql.NullString
	var entityCode string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT scope, COALESCE(entity_code,''), log_archived_at FROM runs WHERE id = ?`, traceID).Scan(&scope, &entityCode, &archivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	// Scope access check (FR-H2 IDOR mitigation, A5 fix): an unrestricted actor reads
	// any run's log; everyone else may read only global runs plus their granted scopes.
	id, hasID := auth.IdentityFrom(r.Context())
	if hasID && !id.Unrestricted() {
		scStr := ""
		if scope.Valid {
			scStr = scope.String
		}
		if !auth.ScopeReadable(id, scStr) {
			// LU-9: audited like every scope denial in internal/api. This one is
			// worth more than most — it guards run OUTPUT, which carries whatever
			// the job printed — so a walk across trace ids must be visible.
			if s.authSvc != nil {
				s.authSvc.AuditDenied(r, id.Email, "insufficient_scope", scStr,
					r.Method+" "+r.URL.Path+": scope access denied")
			}
			httpx.Fail(w, http.StatusForbidden, "forbidden", "scope access denied")
			return
		}
	}

	logPath, perr := s.logPath(entityCode, traceID)
	if perr != nil {
		httpx.Fail(w, http.StatusBadRequest, "invalid_trace_id", "malformed trace id")
		return
	}
	// EP-8a (the expanded-panels plan) — ?offset=<bytes> serves the
	// tail from that byte on, for the UI's live log follow. Correct because the
	// file is APPEND-ONLY with stable offsets: redaction happens at ingest
	// (T7/S7), before the write, so a byte once written is never rewritten and
	// an offset the client holds cannot go stale in place.
	//
	// A query parameter rather than HTTP Range: Range drags in 206/416
	// semantics, multi-range parsing and If-Range correctness for a single
	// internal caller. The need is "give me what is new and tell me where I am
	// now", which is one parameter and one response header.
	offset, oerr := parseLogOffset(r.URL.Query().Get("offset"))
	if oerr != nil {
		httpx.Fail(w, http.StatusBadRequest, "invalid_offset", oerr.Error())
		return
	}

	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			// SL-3: the local file is gone but the run row says the log was
			// archived — serve it from the bucket, same contract, same headers.
			// Only ever a SEALED log (the sweep archives terminal runs after a
			// grace period), so the tail-follow case never lands here.
			if archivedAt.Valid && archivedAt.String != "" {
				s.serveArchivedLog(w, r, entityCode, traceID, offset)
				return
			}
			// Log not yet written (run may be queued or just started). The
			// offset header still goes out, so a tailing client learns "still
			// zero bytes" rather than having to guess.
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Log-Offset", "0")
			w.WriteHeader(http.StatusOK)
			return
		}
		s.log.Error("open log file for read", "path", logPath, "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "log file unavailable")
		return
	}
	defer f.Close()

	size := int64(0)
	if st, serr := f.Stat(); serr == nil {
		size = st.Size()
	}

	// An offset PAST the end is not an error. A log can be reaped, rotated or
	// replaced under a live tail; answering 4xx would wedge the viewer, whereas
	// answering "no new bytes, and here is where the file actually ends" lets
	// the client notice its offset is now beyond EOF and restart from 0. The
	// same branch covers offset == size, which is the ordinary "nothing new
	// since your last poll" tick.
	if offset >= size {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Log-Offset", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	if offset > 0 {
		if _, serr := f.Seek(offset, io.SeekStart); serr != nil {
			s.log.Error("seek log file", "trace_id", traceID, "offset", offset, "error", serr)
			httpx.Fail(w, http.StatusInternalServerError, "internal", "log file unavailable")
			return
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// Stamped from the size measured BEFORE the copy. Taking it after would
	// hand back an offset covering bytes appended mid-response that the client
	// never received, and it would silently skip them on the next poll.
	w.Header().Set("X-Log-Offset", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.CopyN(w, f, size-offset); err != nil && err != io.EOF {
		s.log.Error("stream log file", "trace_id", traceID, "error", err)
	}
}

// serveArchivedLog streams a run's log from the S3 archive tier (SL-3, SL-Q2
// option (a): proxy, never a redirect). The bucket hostname stays off the
// browser, the X-Log-Offset contract is unchanged, and it works wherever the
// SERVER can reach the bucket even when the browser cannot (on-prem MinIO).
//
// The object is immutable, so `offset >= size` is 200-empty exactly as for a
// local file, and a mid-object offset is a ranged GET. The store's absence
// (settings blanked after archiving — SL-Q14) is 503 with the reason, not 404:
// the log exists and the server knows where.
func (s *Service) serveArchivedLog(w http.ResponseWriter, r *http.Request, entityCode, traceID string, offset int64) {
	var store *logarchive.Store
	if s.logArchive != nil {
		store = s.logArchive()
	}
	if store == nil {
		httpx.Fail(w, http.StatusServiceUnavailable, "log_archive_unavailable",
			"this log was archived to S3 but the archive backend is not configured; restore the S3 settings to read it")
		return
	}
	key := store.Key(entityCode, traceID)
	rc, size, err := store.Get(r.Context(), key, offset)
	if err != nil {
		if logarchive.IsNotFound(err) {
			// The marker says archived, the bucket says no. Expired by a lifecycle
			// rule the operator set outside Cronomicon, or deleted by hand. Answer
			// as for a reaped local log — 200-empty, offset 0 — and say so in the
			// process log; a reconcile sync will not re-stamp it either.
			s.log.Warn("archived log object is missing from the bucket", "trace_id", traceID, "key", key)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Log-Offset", "0")
			w.WriteHeader(http.StatusOK)
			return
		}
		s.log.Error("read archived log", "trace_id", traceID, "key", key, "error", err)
		httpx.Fail(w, http.StatusServiceUnavailable, "log_archive_unavailable", "the log archive could not be read: "+err.Error())
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Log-Offset", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		s.log.Error("stream archived log", "trace_id", traceID, "error", err)
	}
}

// parseLogOffset validates the ?offset= parameter. Absent or empty means 0 (a
// full read — the pre-EP-8a behaviour, byte for byte). Anything that is not a
// non-negative integer is rejected rather than coerced: silently treating
// garbage as 0 would hand a tailing client the whole file again and again with
// no signal that its bookmark was never understood.
func parseLogOffset(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("offset must be a non-negative integer")
	}
	return n, nil
}

// checkDrainComplete checks if a draining runner now has zero load and if so
// transitions it to offline (A6.4).
func (s *Service) checkDrainComplete(ctx context.Context, runnerID string) {
	var load int
	var status string
	var drainDeadline *string
	err := s.db.QueryRowContext(ctx, `
		SELECT load, status, drain_deadline_at FROM runners WHERE id = ?`,
		runnerID).Scan(&load, &status, &drainDeadline)
	if err != nil {
		return
	}
	if status != "draining" {
		return
	}
	if load == 0 {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE runners SET status = 'offline', drain_deadline_at = NULL WHERE id = ?`,
			runnerID); err != nil {
			s.log.Error("drain complete → offline", "runner_id", runnerID, "error", err)
		}
		return
	}
	// Check if drain deadline has passed → force offline (A6.4).
	if drainDeadline != nil {
		dl, err := time.Parse(time.RFC3339, *drainDeadline)
		if err == nil && time.Now().UTC().After(dl) {
			s.forceOfflineOnDrainTimeout(ctx, runnerID)
		}
	}
}

// forceOfflineOnDrainTimeout marks a runner offline and marks its active runs
// as danger with reason drain_timeout (A6.4).
func (s *Service) forceOfflineOnDrainTimeout(ctx context.Context, runnerID string) {
	ts := now()
	// Mark still-running runs as danger.
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_name, COALESCE(scope,'') FROM runs
		WHERE runner_id = ? AND status = 'running'`, runnerID)
	if err != nil {
		s.log.Error("query running runs on drain timeout", "error", err)
		return
	}
	defer rows.Close()
	// One lookup per drain timeout, not per stuck run (AA-1).
	runnerName := s.runnerNameOf(ctx, runnerID)

	var stuckRuns [][3]string
	for rows.Next() {
		var tid, jn, sc string
		if err := rows.Scan(&tid, &jn, &sc); err == nil {
			stuckRuns = append(stuckRuns, [3]string{tid, jn, sc})
		}
	}
	_ = rows.Err()

	for _, run := range stuckRuns {
		tid, jn, sc := run[0], run[1], run[2]
		// RX-6 — status-guarded like every other terminal writer. The stuck-run
		// SELECT above and this write are separated by the drain deadline, so a
		// run stopped by an operator (or finalized by a late log) in between must
		// not be rewritten as a drain failure.
		res, err := s.db.ExecContext(ctx, `
			UPDATE runs SET status = 'failure', completed_at = ?, queued_reason = 'drain_timeout'
			WHERE id = ? AND status = 'running'`, ts, tid)
		if err != nil {
			s.log.Error("drain timeout: finalize run", "trace_id", tid, "error", err)
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // already terminal — don't overwrite or double-emit
		}
		_ = auditlog.WriteActivity(ctx, s.db, auditlog.ActivityParams{
			At:         ts,
			Kind:       "run-end",
			Outcome:    "failure",
			Actor:      "system",
			RunnerName: runnerName,
			JobName:    jn,
			Scope:      sc,
			TraceID:    tid,
		})
		// Same terminal seam as finalizeRun: a drain-timeout failure must also
		// emit the RunFinished metric and fire notifications (C.1/C.2).
		s.emitRunTerminal(notify.RunEvent{TraceID: tid, JobName: jn, Scope: sc, Status: "failure"})
	}

	// Transition runner to offline.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE runners SET status = 'offline', load = 0, drain_deadline_at = NULL WHERE id = ?`,
		runnerID); err != nil {
		s.log.Error("force runner offline on drain timeout", "error", err)
	}
}

// rawByteCounter wraps an io.Reader and counts the total bytes read (PP-M8).
type rawByteCounter struct {
	r io.Reader
	n int64
}

func (c *rawByteCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

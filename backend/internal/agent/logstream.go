package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// logBuffer accumulates the raw (un-redacted — redaction is the server's job)
// newline-delimited log bytes a run produces, tracks how many of those bytes
// the server has durably persisted, and drives the resumable POST.
//
// Design (R2.3): execution writes lines into the buffer as they are produced;
// flush() streams everything past the committed offset to the server in one
// POST, advancing the committed offset on success. On a dropped connection it
// retries with X-Resume-Offset = committed; on a 409 it adopts the server's
// persistedOffset (the source of truth) and replays from there. Buffering makes
// resume lossless: nothing is dropped between the produce side and the server.
type logBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer // all bytes ever produced (lines + trailing envelope)
	committed int64        // bytes the server has acknowledged persisting
	sealed    bool         // true once the trailing envelope has been appended
}

// writeLine appends one log line (a trailing newline is added). The agent
// streams RAW bytes — the server redacts on ingest (S7), so the agent must not.
func (b *logBuffer) writeLine(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.WriteString(line)
	b.buf.WriteByte('\n')
}

// seal appends the trailing JSON envelope line (§6.3) that closes the stream
// with the real exit code, duration, and end time. After seal, no more lines
// may be written.
func (b *logBuffer) seal(env runnerproto.TrailingEnvelope) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sealed {
		return
	}
	data, _ := json.Marshal(env)
	b.buf.Write(data)
	b.buf.WriteByte('\n')
	b.sealed = true
}

// pending returns the bytes past the committed offset (a copy, safe to send).
func (b *logBuffer) pending() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	all := b.buf.Bytes()
	if b.committed >= int64(len(all)) {
		return nil
	}
	out := make([]byte, int64(len(all))-b.committed)
	copy(out, all[b.committed:])
	return out
}

// setCommitted advances (or resets, on a 409) the committed offset.
func (b *logBuffer) setCommitted(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 {
		n = 0
	}
	if n > int64(b.buf.Len()) {
		n = int64(b.buf.Len())
	}
	b.committed = n
}

// committedOffset reports the current committed offset.
func (b *logBuffer) committedOffset() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.committed
}

// makeEnvelope builds the §6.3 trailing envelope from an execution outcome.
func makeEnvelope(exitCode int, start time.Time, endedAt time.Time, reason string) runnerproto.TrailingEnvelope {
	return runnerproto.TrailingEnvelope{
		ExitCode:   exitCode,
		DurationMs: endedAt.Sub(start).Milliseconds(),
		EndedAt:    endedAt.UTC().Format(time.RFC3339),
		Reason:     reason,
	}
}

// streamLogs flushes the buffer to the server, retrying on connection loss
// with X-Resume-Offset and honoring 409 persistedOffset, up to budget attempts.
// Exhausting the budget returns an error and lets the run land log_stream_lost
// server-side (R2.3).
//
// partial=false is the terminal flush: the buffer must be sealed before calling
// (the whole remaining stream incl. the trailing envelope goes in one logical
// body). partial=true is a mid-run live-tail flush (v12): whatever whole lines
// are pending are sent with X-Log-Partial so the server appends without
// treating the chunk as the end of the stream. The executor may keep writing
// lines while a partial flush is in flight, which is why success commits
// exactly the bytes this attempt sent — never b.total(), which could by then
// include lines the server has not seen.
func streamLogs(ctx context.Context, c *Client, id Identity, traceID string,
	b *logBuffer, budget int, partial bool) error {

	if budget < 1 {
		budget = 1
	}
	var lastErr error
	for attempt := 0; attempt < budget; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		off := b.committedOffset()
		pending := b.pending()
		if len(pending) == 0 {
			return nil // everything committed
		}
		res, err := c.postLog(ctx, id, traceID, off, bytes.NewReader(pending), partial)
		if err != nil {
			lastErr = err
			// Back off briefly before resuming (connection drop).
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
			}
			continue
		}
		if res.resumeMismatch {
			// Server's persisted offset is the truth; replay from exactly there.
			b.setCommitted(res.persistedOffset)
			continue
		}
		// Success: the pending slice this attempt sent was persisted.
		b.setCommitted(off + int64(len(pending)))
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("log stream retry budget (%d) exhausted", budget)
	}
	return fmt.Errorf("log stream gave up after %d attempts: %w", budget, lastErr)
}

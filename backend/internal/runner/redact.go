package runner

import (
	"context"
	"database/sql"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/redactdict"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// Redactor holds the ordered list of plaintext values that should be masked
// in log output. It is the PER-RUN dictionary; the process-wide one that masks
// the audit stream is redactdict (AM-4), and the rules for what enters either —
// minimum length, common literals, per-line PEM handling — live there. Values are loaded at run-claim time from B6's stored secrets
// (decrypted via the KEK) plus multi-line env_vars values (key material — see
// below). Redaction happens at ingest (T7/S7) so runners stream raw bytes and
// Cronomicon masks before persisting.
//
// Plain (single-line) Variables and per-run prompt answers are deliberately NOT
// in the dictionary: variables are plaintext, log-safe values (D7), and masking
// them everywhere they happened to appear only confused users. The one carve-out
// is a MULTI-LINE env_vars value: hosts may reference an SSH private key stored
// in env_vars by name (sshexec.signerFromEnvVar, `@variable:` references), so a
// multi-line value is treated as key material and stays masked.
//
// Redaction is UNCONDITIONAL: there is no per-job or global toggle. The former
// `sensitive_logging` flag (job-level) and `sensitiveLogging` settings toggle were
// inert — this redactor never consulted them and the agent never read the manifest
// field — so they were removed in PP-M9. Every run's known secret/env values are
// always masked.
type Redactor struct {
	dict *redactdict.Dictionary
}

// redactMask is the replacement text — one constant, owned by redactdict.
const redactMask = redactdict.Mask

// redactSkip and addRedactionValue are the shared rules (V1.1-9, PP-B4a); see
// redactdict.Skip / redactdict.AddValue. Kept as package aliases so the
// per-run builder below and its tests read the same as before AM-4a.
func redactSkip(v string) bool                           { return redactdict.Skip(v) }
func addRedactionValue(vals []string, v string) []string { return redactdict.AddValue(vals, v) }

// NewRedactor builds a Redactor for a run: all stored-secret plaintext values
// (B6's secrets.RedactionValues, which decrypts via the KEK) plus any MULTI-LINE
// env_var value for the run's scope — key material referenced by name, per the
// type comment. Single-line Variables stay visible in logs (D7). cfg may be nil
// in tests, in which case stored secrets contribute nothing.
func NewRedactor(ctx context.Context, db *sql.DB, cfg *config.Config, scope string, extra ...string) (*Redactor, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT value FROM env_vars WHERE scope = ? OR scope = '*'`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vals []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		// Single-line Variables are log-safe (D7) and stay visible; only a
		// multi-line value (an SSH private key a host references by name) is
		// treated as secret material.
		if strings.ContainsAny(v, "\r\n") {
			vals = addRedactionValue(vals, v)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Append stored-secret plaintext values (S14 envelope-decrypted by B6) so
	// they're masked too (S7). Best-effort: a nil cfg or missing KEK yields none.
	if cfg != nil {
		secretVals, err := secrets.RedactionValues(ctx, db, cfg)
		if err != nil {
			return nil, err
		}
		for _, v := range secretVals {
			vals = addRedactionValue(vals, v)
		}
	}

	// extra carries the dispatch-time injected values the global dictionary does
	// not know: vault-source secret values and delivered SSH key material (P1.3/
	// P1.5). Per-run prompt answers, schedule env (§3.7) and job env (JC12) are
	// all deliberately NOT fed in — run inputs and variables are plaintext and
	// log-visible.
	for _, v := range extra {
		vals = addRedactionValue(vals, v)
	}

	// Longest-first ordering and the greedy loop live in redactdict.
	return &Redactor{dict: redactdict.FromValues(vals)}, nil
}

// Redact replaces all known sensitive values in p with [REDACTED].
// Returns the redacted bytes. Never logs the original content (S7).
func (r *Redactor) Redact(p []byte) []byte {
	if r == nil {
		return p
	}
	return r.dict.Redact(p)
}

// noopRedactor is used when scope is empty (bootstrap/test).
var noopRedactor = &Redactor{}

package runref

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/ResetSmith/cronomicon/internal/settings"
)

// AuditInjectionOnce records a run's dispatch-time reference injection to change_log
// exactly once (P1.6), shared by both executors. It atomically CLAIMS the one-time
// slot (`runs.injection_audited`) so concurrent runner-manifest re-fetches cannot
// double-write, then writes the audit; on a write failure it RELEASES the slot so a
// retry re-attempts — favoring audit presence over strict uniqueness (a run must
// never be flagged audited with no row). Records reference names + source only,
// NEVER values. Returns the WriteChangeLog error unmasked so the caller can fail
// closed. actor "" ⇒ "system". log may be nil.
//
// M6 — two hardenings against the "audited with no row" wedge (secrets ship
// untraced forever):
//   - The release-on-failure runs on a CANCELLATION-PROOF context
//     (context.WithoutCancel + timeout). Previously it reused the caller's request
//     context, so an agent that disconnected during WriteChangeLog failed BOTH the
//     write and the release on the same canceled context, leaving injection_audited
//     stuck at 1 with no row — permanent.
//   - Compare-and-repair: an already-claimed slot (RowsAffected==0) is only trusted
//     when a matching change_log row actually EXISTS; otherwise the row is
//     (re)written here, self-healing a previously-wedged slot on the next fetch.
func AuditInjectionOnce(ctx context.Context, database *sql.DB, log *slog.Logger, traceID, actor, scope string, resolved *Resolved) error {
	res, err := database.ExecContext(ctx,
		`UPDATE runs SET injection_audited = 1 WHERE id = ? AND injection_audited = 0`, traceID)
	if err != nil {
		return fmt.Errorf("claim injection audit slot: %w", err)
	}
	claimed, _ := res.RowsAffected()
	if claimed == 0 {
		// Slot already claimed. Trust it ONLY if the audit row is actually present;
		// otherwise a prior wedge left it flagged-but-unwritten — fall through and
		// (re)write the row (a benign duplicate at worst; presence over uniqueness).
		if injectionAuditRowExists(ctx, database, traceID) {
			return nil
		}
	}
	if actor == "" {
		actor = "system"
	}
	if werr := settings.WriteChangeLog(ctx, database, actor, "Secrets", "injected", traceID, AuditDetails(resolved.Refs, scope)); werr != nil {
		// Only release the slot WE just claimed (claimed>0); in the repair path the
		// slot was already 1 and owned by a prior call. Release on a cancellation-proof
		// context so a disconnected request can't leave the slot wedged (M6).
		if claimed > 0 {
			relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, rerr := database.ExecContext(relCtx, `UPDATE runs SET injection_audited = 0 WHERE id = ?`, traceID); rerr != nil {
				if log != nil {
					log.Error("injection audit slot release failed; next fetch will compare-and-repair",
						"trace_id", traceID, "write_error", werr, "release_error", rerr)
				}
			}
		}
		return werr
	}
	return nil
}

// injectionAuditRowExists reports whether a dispatch-injection audit row is present
// for the run. On a query error it returns false (favor a repair write over
// silently trusting a possibly-missing row — presence over uniqueness, M6).
func injectionAuditRowExists(ctx context.Context, database *sql.DB, traceID string) bool {
	var n int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM change_log WHERE category = 'Secrets' AND action = 'injected' AND target = ?`,
		traceID).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// auditRef is the wire shape of one reference in the dispatch-audit details.
// `as` is omitted for an un-aliased binding, so every pre-Phase-A audit row parses
// back identically and a new row for an un-aliased reference is byte-compatible
// with the old shape.
type auditRef struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	As     string `json:"as,omitempty"`
	Source string `json:"source"`
}

// ParseAuditReferences parses a dispatch-audit `details` payload (as produced by
// AuditDetails) back into bindings with derived references — the authoritative
// record of WHICH references a run actually injected at dispatch (P1.8 run detail),
// as opposed to re-deriving from live bindings (which drift after edits, and would
// list SSH-skipped keys / refused runs the run never received). Values are never in
// the payload. Returns nil on malformed input.
func ParseAuditReferences(details string) []Binding {
	var parsed struct {
		References []auditRef `json:"references"`
	}
	if err := json.Unmarshal([]byte(details), &parsed); err != nil {
		return nil
	}
	out := make([]Binding, 0, len(parsed.References))
	for _, r := range parsed.References {
		k := Kind(r.Kind)
		out = append(out, Binding{Kind: k, Name: r.Name, As: r.As, Reference: k.Reference(r.Name)})
	}
	return out
}

// AuditDetails renders the change_log `details` payload for a dispatch-time
// reference-injection event (P1.6): the run scope plus the sorted set of references
// injected — kind, bare name, alias (RA-7), and backing source only, NEVER values.
// Deterministic ordering (kind, then name, then alias — the alias tiebreak matters
// once one row can be injected twice under two destinations) keeps the audit row
// stable and diffable across re-resolves. Returns "{}" for an empty set (callers
// gate on len(refs) > 0, so this is a defensive default).
func AuditDetails(refs []ResolvedRef, scope string) string {
	out := struct {
		Scope      string     `json:"scope"`
		References []auditRef `json:"references"`
	}{Scope: scope, References: make([]auditRef, 0, len(refs))}
	for _, r := range refs {
		out.References = append(out.References, auditRef{Kind: string(r.Kind), Name: r.Name, As: r.As, Source: r.Source})
	}
	sort.Slice(out.References, func(i, j int) bool {
		if out.References[i].Kind != out.References[j].Kind {
			return out.References[i].Kind < out.References[j].Kind
		}
		if out.References[i].Name != out.References[j].Name {
			return out.References[i].Name < out.References[j].Name
		}
		return out.References[i].As < out.References[j].As
	})
	b, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	return string(b)
}

package redactdict

import (
	"context"
	"database/sql"
	"log/slog"
	"sync/atomic"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/config"
)

// Install is the Phase C wiring (AM-5): configure the process-wide store and
// hand auditlog a masker over it. Call once at boot, after the database is
// migrated and before anything writes an audit row.
//
// The masker is fail-open in exactly one shape (AM-Q3): the dictionary that is
// installed — complete, partial, or the last good one — is always applied; the
// only thing that ever writes unmasked is a process in which no build has yet
// produced anything. What Install adds over MaskString is the REPORTING: the
// first mask that finds a standing outage (a partial build, or a failed rebuild
// serving stale data) logs a WARN and writes one self-describing change_log
// row, `Audit / redactor-unavailable`, so the gap is itself in the stream; the
// first mask after recovery writes `redactor-restored`. One row per transition,
// not per write — a masker that audited every call would flood the table it
// exists to protect.
//
// Redaction is unconditional. There is no Audit & Compliance knob for it, for
// the same reason the run-log redactor has none (PP-M9).
func Install(database *sql.DB, cfg *config.Config, log *slog.Logger) *Store {
	st := Configure(database, cfg)
	r := &reporter{db: database, log: log}
	auditlog.SetRedactor(func(s string) string {
		d, err := st.Get(context.Background())
		r.observe(err)
		return d.RedactString(s)
	})
	return st
}

type reporter struct {
	db  *sql.DB
	log *slog.Logger
	// outage is true while the last observed build state was unhealthy and has
	// been reported. It is swapped BEFORE the self-audit row is written, and
	// that ordering is the recursion guard: the row goes through WriteChangeLog
	// → mask → observe on this same goroutine, sees outage already set, and
	// returns without writing a second row.
	outage atomic.Bool
}

func (r *reporter) observe(err error) {
	if err != nil {
		if r.outage.Swap(true) {
			return // already reported this outage
		}
		r.log.Warn("audit-stream redaction is degraded; rows are masked with what could be built",
			"error", err)
		_ = auditlog.WriteChangeLog(context.Background(), r.db, "system", "Audit", "redactor-unavailable", "",
			"audit masking degraded: "+err.Error())
		return
	}
	if !r.outage.Swap(false) {
		return // healthy, and was healthy
	}
	r.log.Info("audit-stream redaction restored")
	_ = auditlog.WriteChangeLog(context.Background(), r.db, "system", "Audit", "redactor-restored", "",
		"audit masking rebuilt with every encrypted value decrypted")
}

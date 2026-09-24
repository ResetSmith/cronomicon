package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AuditCompliance maps the openapi AuditComplianceSettings schema: the A4
// retention knobs. Stored as a single JSON blob under the settings KV key below.
//
// Note: log redaction (S7) is unconditional — there is no toggle. The former
// sensitiveLogging field was inert (runner/redact.go always masks) and was
// removed in PP-M9. The former DB-stored Backup config (FU-3) was likewise inert
// — the uploader is driven solely by the AMADEUS_BACKUP_S3_* env (see
// internal/config + internal/backup), which never read this settings blob — so
// it was removed to kill the "configured it but it did nothing" trap.
type AuditCompliance struct {
	RetentionDays RetentionDays `json:"retentionDays"`
}

type RetentionDays struct {
	Runs           int `json:"runs"`
	Activity       int `json:"activity"`
	WorkflowRuns   int `json:"workflowRuns"`
	ChangeLog      int `json:"changeLog"`
	SchedulePushes int `json:"schedulePushes"`
	LogFiles       int `json:"logFiles"`
	// AuditLogFiles bounds the on-disk audit stream (audit.log and its dated
	// generations), independently of and longer than the DB tables it exports
	// (LU-Q4(c)). Keeping the file window longer is the whole point: it is what
	// preserves a tamper-evident tail after the rows themselves have been pruned.
	AuditLogFiles int `json:"auditLogFiles"`
	// RecycleBin bounds how long a soft-deleted amadeus-source definition stays
	// restorable before the reaper hard-deletes it (RH). It is the one knob here
	// whose expiry destroys a DEFINITION rather than a log of one, which is why
	// its default is the shortest window that still spans a long weekend plus a
	// holiday — long enough that "I deleted the wrong workflow on Friday" is
	// recoverable on Tuesday, short enough that the bin does not become storage.
	RecycleBin int `json:"recycleBin"`
	// DefinitionRevisions bounds the append-only snapshot history. Kept a year by
	// default, matching changeLog: a revision and the change-log row describing it
	// are two halves of the same answer, and pruning one first leaves the other
	// pointing at nothing.
	DefinitionRevisions int `json:"definitionRevisions"`
	// RunnerPlacementHistory bounds how long a deregistered runner's placement
	// snapshot stays available to be offered back (DR-7 / DR-Q6). 30 days, and
	// deliberately NOT aligned with the 90-day runs/activity family: it covers a
	// multi-week outage while leaving less standing exposure — do not "fix" it
	// to match its neighbours. The window ADDS to AMADEUS_RUNNER_DEREGISTER_AFTER
	// (default 14d): a runner offline from day 0 is reaped at day 14 and its
	// snapshot written THEN, so useful coverage is ~44 days of continuous outage.
	RunnerPlacementHistory int `json:"runnerPlacementHistory"`
	// ArchivedLogFiles bounds the S3 archive tier's objects (SL-4 / SL-Q4). The
	// eleventh knob, and the only one whose default is 0 = keep forever: the
	// bucket is the disaster-recovery copy, and Cronomicon deletes from it only
	// when an operator says so. When set, the archive sweep issues the deletes
	// itself (the bucket policy must then grant s3:DeleteObject); when 0, the
	// bucket policy may withhold delete and an S3 lifecycle rule may do the
	// expiry instead. Separate from `logFiles`, which governs LOCAL files and
	// which, while the archive backend is on, never reaps a file the sweep has
	// not yet copied.
	ArchivedLogFiles int `json:"archivedLogFiles"`
}

const auditComplianceKey = "auditCompliance"

func defaultAuditCompliance() AuditCompliance {
	return AuditCompliance{
		RetentionDays: RetentionDays{
			Runs: 90, Activity: 90, WorkflowRuns: 90,
			ChangeLog: 365, SchedulePushes: 365, LogFiles: 90,
			// Two years — deliberately longer than changeLog's one, so the exported
			// stream outlives the rows.
			AuditLogFiles: 730,
			RecycleBin:    30,
			// Matches ChangeLog deliberately — see the field comment.
			DefinitionRevisions: 365,
			// Deliberately not 90 — see the field comment.
			RunnerPlacementHistory: 30,
			// Keep forever — see the field comment.
			ArchivedLogFiles: 0,
		},
	}
}

// maxRetentionDays caps a single knob at ~27 years. It exists to catch a
// fat-fingered entry, not to express a policy — anyone wanting "forever" has 0.
const maxRetentionDays = 10000

// validate rejects day counts the sweep cannot act on sensibly (LU-2).
//
// It is not load-bearing for safety today: both runSweep's prune and
// reapLogFiles gate on `days <= 0`, so a negative value is absorbed as "keep
// forever" rather than producing a future cutoff that would match every row.
// It exists for two other reasons. First, that absorption is a silent footgun
// in the other direction — an operator who types -1 expecting aggressive
// pruning gets *no* pruning, and nothing tells them. Second, the `days <= 0`
// guards are the only thing standing between a negative value and a
// delete-everything cutoff, and this keeps a bad value out of the store rather
// than relying on those guards staying put.
//
// Note this validates on write only. GetAuditCompliance does not re-validate,
// so a hand-edited or legacy blob still reaches the sweeper — which is exactly
// why the `days <= 0` guards must stay.
func (a AuditCompliance) validate() error {
	for _, f := range []struct {
		field string
		days  int
	}{
		{"retentionDays.runs", a.RetentionDays.Runs},
		{"retentionDays.activity", a.RetentionDays.Activity},
		{"retentionDays.workflowRuns", a.RetentionDays.WorkflowRuns},
		{"retentionDays.changeLog", a.RetentionDays.ChangeLog},
		{"retentionDays.schedulePushes", a.RetentionDays.SchedulePushes},
		{"retentionDays.logFiles", a.RetentionDays.LogFiles},
		{"retentionDays.auditLogFiles", a.RetentionDays.AuditLogFiles},
		{"retentionDays.recycleBin", a.RetentionDays.RecycleBin},
		{"retentionDays.definitionRevisions", a.RetentionDays.DefinitionRevisions},
		{"retentionDays.runnerPlacementHistory", a.RetentionDays.RunnerPlacementHistory},
		{"retentionDays.archivedLogFiles", a.RetentionDays.ArchivedLogFiles},
	} {
		if f.days < 0 {
			return &ValidationError{Field: f.field, Message: f.field + " must be 0 (keep forever) or a positive number of days"}
		}
		if f.days > maxRetentionDays {
			return &ValidationError{Field: f.field, Message: fmt.Sprintf("%s must be at most %d days (use 0 to keep forever)", f.field, maxRetentionDays)}
		}
	}
	return nil
}

// SeedAuditComplianceFromEnv writes the retention blob from the legacy
// AMADEUS_RETENTION_* env values, exactly once, iff nothing has ever been
// stored. It reports whether it wrote.
//
// This is what makes LU-Q3(a) — "the DB is authoritative, env is the bootstrap
// default" — safe to upgrade into. Until LU-2 the policy came from env alone and
// the blob sat at its untouched defaults, so flipping the source of truth
// without seeding would silently reset a deployment that had set
// AMADEUS_RETENTION_RUNS_DAYS=30 back to 90 — a retention regression nobody
// notices until the disk fills or an auditor asks.
//
// The env vars are coarser than the blob (one knob for runs/activity/
// workflow_runs, one for change_log/schedule_pushes), which is precisely the
// grouping runSweep used before LU-2, so fanning each out to its members
// reproduces the pre-upgrade behaviour exactly.
//
// ON CONFLICT DO NOTHING makes this a single atomic statement: concurrent boots
// cannot double-seed, and an operator who has already saved the panel is never
// overwritten.
func SeedAuditComplianceFromEnv(ctx context.Context, db *sql.DB, runsDays, changeLogDays, logFilesDays int) (bool, error) {
	// START FROM THE DEFAULTS and override only what the env actually carries.
	//
	// This used to hand-list the fields instead, and the list fell behind the
	// struct twice: RecycleBin and DefinitionRevisions were added by PF-3 and
	// never added here, so every install seeded after that shipped stored zeros
	// for them — and 0 means KEEP FOREVER to both sweeps (db/retention.go's
	// `days <= 0` guards), so the recycle bin and the revision history were
	// never reaped until someone happened to press Save on the panel.
	//
	// The bug was invisible for two compounding reasons. A knob absent from the
	// stored JSON is harmless, because GetAuditCompliance unmarshals OVER a
	// defaults struct and leaves absent keys alone — which is why deployments
	// seeded BEFORE those knobs existed were unaffected and the failure looked
	// like it did not exist. And an explicit zero is indistinguishable from a
	// deliberate "keep forever" once stored, so nothing downstream could flag it.
	//
	// Deriving from defaults makes the next knob correct by construction: a new
	// field gets its default here unless someone deliberately gives it an env
	// source. TestSeedAuditComplianceMatchesDefaultsExceptEnvKnobs enforces that.
	ac := defaultAuditCompliance()
	ac.RetentionDays.Runs = runsDays
	ac.RetentionDays.Activity = runsDays
	ac.RetentionDays.WorkflowRuns = runsDays
	ac.RetentionDays.ChangeLog = changeLogDays
	ac.RetentionDays.SchedulePushes = changeLogDays
	ac.RetentionDays.LogFiles = logFilesDays
	if err := ac.validate(); err != nil {
		return false, err
	}
	b, err := json.Marshal(ac)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := db.ExecContext(ctx, `
		INSERT INTO settings (key, value, last_modified_by, last_modified_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO NOTHING`,
		auditComplianceKey, string(b), "system (env bootstrap)", now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// GetAuditCompliance returns the stored settings, or A4 defaults if unset.
func GetAuditCompliance(ctx context.Context, db *sql.DB) (*AuditCompliance, error) {
	ac := defaultAuditCompliance()
	var raw string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, auditComplianceKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return &ac, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(raw), &ac); err != nil {
		return nil, err
	}
	return &ac, nil
}

// UpdateAuditCompliance persists the settings and writes an audit row (S6).
func UpdateAuditCompliance(ctx context.Context, db *sql.DB, inp AuditCompliance, actor string) (*AuditCompliance, error) {
	if err := inp.validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(inp)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO settings (key, value, last_modified_by, last_modified_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			last_modified_by = excluded.last_modified_by,
			last_modified_at = excluded.last_modified_at`,
		auditComplianceKey, string(b), actor, now); err != nil {
		return nil, err
	}
	_ = WriteChangeLog(ctx, db, actor, "Settings", "updated", "Audit & Compliance", "")
	out := inp
	return &out, nil
}

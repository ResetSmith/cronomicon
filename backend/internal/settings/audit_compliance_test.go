package settings

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// TestAuditComplianceBackupRemoved guards FU-3 Phase B: the DB-stored backup
// config was inert (the uploader is driven solely by CRONOMICON_BACKUP_S3_* env)
// and was removed. A legacy settings blob that still carries a "backup" object
// must load without error and without resurrecting it — env stays the sole
// backup-config surface.
func TestAuditComplianceBackupRemoved(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	// Simulate a pre-FU-3 stored blob that included the old backup object.
	legacy := `{"retentionDays":{"runs":30},"backup":{"enabled":true,"s3Bucket":"stale","secretKey":"leaked"}}`
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO settings (key, value, last_modified_by, last_modified_at) VALUES (?, ?, ?, ?)`,
		auditComplianceKey, legacy, "test", "2026-07-23T00:00:00Z"); err != nil {
		t.Fatalf("seed legacy blob: %v", err)
	}

	ac, err := GetAuditCompliance(ctx, pool)
	if err != nil {
		t.Fatalf("GetAuditCompliance on legacy blob: %v", err)
	}
	if ac.RetentionDays.Runs != 30 {
		t.Errorf("retentionDays.runs = %d, want 30 (legacy value should survive)", ac.RetentionDays.Runs)
	}

	// The AuditCompliance type no longer has any backup surface — round-trip a
	// re-save and confirm the stale key is dropped, not silently carried forward.
	if _, err := UpdateAuditCompliance(ctx, pool, *ac, "test"); err != nil {
		t.Fatalf("UpdateAuditCompliance: %v", err)
	}
	var stored string
	if err := pool.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, auditComplianceKey).Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if want := "backup"; contains(stored, want) {
		t.Errorf("re-saved audit-compliance blob still contains %q: %s", want, stored)
	}
}

// TestBackupConfigComesFromEnv asserts the wired backup surface is the
// CRONOMICON_BACKUP_S3_* env, not the settings table (FU-3 Phase B).
func TestBackupConfigComesFromEnv(t *testing.T) {
	// DEV_AUTH sidesteps the trusted-header boot guard (see config loadWith).
	t.Setenv("CRONOMICON_DEV_AUTH", "true")
	t.Setenv("CRONOMICON_BACKUP_S3_BUCKET", "env-bucket")
	t.Setenv("CRONOMICON_BACKUP_S3_REGION", "eu-west-1")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.BackupS3Bucket != "env-bucket" {
		t.Errorf("BackupS3Bucket = %q, want env-bucket (env is the wired surface)", cfg.BackupS3Bucket)
	}
	if cfg.BackupS3Region != "eu-west-1" {
		t.Errorf("BackupS3Region = %q, want eu-west-1", cfg.BackupS3Region)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ── LU-2: env bootstrap seeding + retention validation ───────────────────────
//
// These cover the two properties that make "the DB is authoritative, env is only
// the bootstrap default" (LU-Q3(a)) safe to upgrade into, plus the validation
// that had to arrive with it: the blob is no longer decorative, it now drives
// db.RetentionPolicy's DELETE cutoffs.

// TestSeedAuditComplianceFromEnvFansOutCoarseKnobs verifies the seed writes on a
// virgin DB and that the coarse env vars are expanded to the exact grouping
// runSweep used before LU-2 — runsDays covering runs/activity/workflowRuns and
// changeLogDays covering changeLog/schedulePushes. Getting the fan-out wrong
// would silently change retention for a deployment that never touched the panel.
func TestSeedAuditComplianceFromEnvFansOutCoarseKnobs(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	wrote, err := SeedAuditComplianceFromEnv(ctx, pool, 30, 400, 14)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !wrote {
		t.Fatal("seed reported no write on a fresh DB")
	}

	ac, err := GetAuditCompliance(ctx, pool)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Every knob with no env source keeps its DEFAULT — spelled by deriving the
	// expectation from defaultAuditCompliance() and overriding the six the env
	// actually feeds, rather than by listing them. A literal list here is what
	// let the bug in: the production seed had its own list, it fell two knobs
	// behind the struct, and this test's list was wrong in exactly the same way,
	// so the suite stayed green on a real retention failure.
	want := defaultAuditCompliance().RetentionDays
	want.Runs, want.Activity, want.WorkflowRuns = 30, 30, 30
	want.ChangeLog, want.SchedulePushes = 400, 400
	want.LogFiles = 14
	if ac.RetentionDays != want {
		t.Errorf("retentionDays = %+v, want %+v (coarse env must fan out: runs→runs/activity/workflowRuns, changeLog→changeLog/schedulePushes)", ac.RetentionDays, want)
	}
}

// TestSeedAuditComplianceMatchesDefaultsExceptEnvKnobs is the structural guard
// that makes the NEXT knob correct without anyone remembering this bug.
//
// It seeds with the env values equal to their own defaults, so the whole blob
// must come back byte-equal to defaultAuditCompliance(). Any field the seed
// forgets marshals as a zero, and a zero means "keep forever" to every sweep —
// the sweeps read `days <= 0` and return early — so a forgotten knob is not a
// cosmetic omission but retention silently switched off for that table.
//
// The failure this catches is invisible in production: nothing errors, nothing
// logs, and the panel shows 0 as though someone had chosen it.
func TestSeedAuditComplianceMatchesDefaultsExceptEnvKnobs(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	def := defaultAuditCompliance().RetentionDays
	if _, err := SeedAuditComplianceFromEnv(ctx, pool, def.Runs, def.ChangeLog, def.LogFiles); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ac, err := GetAuditCompliance(ctx, pool)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ac.RetentionDays != def {
		t.Errorf("seeding with the default env values produced %+v, want %+v.\nA knob that differs is one the seed does not set: it stored 0, and 0 = keep forever to the sweeps.\nFix SeedAuditComplianceFromEnv (it derives from defaultAuditCompliance — do not re-introduce a hand-written field list).",
			ac.RetentionDays, def)
	}

	// And the stored JSON must actually carry the knobs, not rely on
	// GetAuditCompliance's unmarshal-over-defaults to paper over an omission —
	// that leniency is exactly why the original bug was invisible for deployments
	// seeded before the knobs existed.
	var raw string
	if err := pool.QueryRow(`SELECT value FROM settings WHERE key = ?`, auditComplianceKey).Scan(&raw); err != nil {
		t.Fatalf("read stored blob: %v", err)
	}
	for _, key := range []string{"recycleBin", "definitionRevisions", "auditLogFiles", "runnerPlacementHistory", "archivedLogFiles"} {
		if !strings.Contains(raw, `"`+key+`"`) {
			t.Errorf("stored blob does not carry %q: %s", key, raw)
		}
	}
}

// TestSeedAuditComplianceIsOnceOnly is the upgrade-safety property. Seeding runs
// on every boot, so a second call must be a no-op: an operator who has saved the
// Audit & Compliance panel must never have their values reverted to whatever the
// (possibly stale, possibly absent) CRONOMICON_RETENTION_* env still says on the
// next restart.
func TestSeedAuditComplianceIsOnceOnly(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	if _, err := SeedAuditComplianceFromEnv(ctx, pool, 30, 400, 14); err != nil {
		t.Fatalf("first seed: %v", err)
	}

	// The operator then saves their own, distinct values in the panel.
	operator := AuditCompliance{RetentionDays: RetentionDays{
		Runs: 7, Activity: 8, WorkflowRuns: 9, ChangeLog: 10, SchedulePushes: 11, LogFiles: 12,
	}}
	if _, err := UpdateAuditCompliance(ctx, pool, operator, "operator"); err != nil {
		t.Fatalf("operator update: %v", err)
	}

	// A later boot seeds again with the original (now stale) env values.
	wrote, err := SeedAuditComplianceFromEnv(ctx, pool, 30, 400, 14)
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if wrote {
		t.Error("second seed reported a write: seeding must be once-only")
	}

	ac, err := GetAuditCompliance(ctx, pool)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ac.RetentionDays != operator.RetentionDays {
		t.Errorf("retentionDays = %+v, want %+v: re-seeding clobbered the operator's saved values", ac.RetentionDays, operator.RetentionDays)
	}
}

// TestSeedAuditCompliancePreservesZero pins that a seeded 0 stays 0. Zero is the
// "keep forever" sentinel shared with db.RetentionPolicy, so coercing it to a
// default would turn a deliberate CRONOMICON_RETENTION_RUNS_DAYS=0 into a 90-day
// window and start deleting history the operator asked to keep.
func TestSeedAuditCompliancePreservesZero(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	if _, err := SeedAuditComplianceFromEnv(ctx, pool, 0, 0, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ac, err := GetAuditCompliance(ctx, pool)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Every ENV-DERIVED knob must hold the seeded 0; every knob with NO env source
	// keeps its default, because seeding 0 there would mean "keep this forever"
	// and the ABSENCE of an env var must never decide that.
	//
	// Spelled as defaults-minus-the-env-six rather than as a literal. The literal
	// this replaces listed auditLogFiles and runnerPlacementHistory as the only
	// non-env knobs and so expected recycleBin and definitionRevisions to be 0 —
	// the same two-knob omission the production seed had, which is why this test
	// passed while retention for the recycle bin and the revision history was
	// silently off.
	want := defaultAuditCompliance().RetentionDays
	want.Runs, want.Activity, want.WorkflowRuns = 0, 0, 0
	want.ChangeLog, want.SchedulePushes = 0, 0
	want.LogFiles = 0
	if ac.RetentionDays != want {
		t.Errorf("retentionDays = %+v, want %+v: a seeded 0 means keep forever and must not fall back to defaults", ac.RetentionDays, want)
	}
}

// TestUpdateAuditComplianceRejectsNegativeDays is the destructive-input guard.
// A negative day count means a cutoff in the *future*, and `DELETE FROM runs
// WHERE created_at < cutoff` would then match every row and empty the table on
// the next nightly sweep. Before LU-2 the blob was inert and a bad value was
// harmless; now that it feeds db.RetentionPolicy it must be refused at the door,
// and nothing may be persisted. (db.runSweep's `days <= 0` check currently
// absorbs a negative as "keep forever" too — that is defence in depth, not a
// reason to let a value the operator can never have meant reach the DB.)
func TestUpdateAuditComplianceRejectsNegativeDays(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	baseline := AuditCompliance{RetentionDays: RetentionDays{
		Runs: 30, Activity: 30, WorkflowRuns: 30, ChangeLog: 365, SchedulePushes: 365, LogFiles: 30,
	}}
	if _, err := UpdateAuditCompliance(ctx, pool, baseline, "operator"); err != nil {
		t.Fatalf("baseline update: %v", err)
	}

	for _, tc := range []struct {
		name string
		bad  AuditCompliance
	}{
		{"runs", AuditCompliance{RetentionDays: RetentionDays{Runs: -1}}},
		{"activity", AuditCompliance{RetentionDays: RetentionDays{Activity: -1}}},
		{"workflowRuns", AuditCompliance{RetentionDays: RetentionDays{WorkflowRuns: -1}}},
		{"changeLog", AuditCompliance{RetentionDays: RetentionDays{ChangeLog: -1}}},
		{"schedulePushes", AuditCompliance{RetentionDays: RetentionDays{SchedulePushes: -1}}},
		{"logFiles", AuditCompliance{RetentionDays: RetentionDays{LogFiles: -1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := UpdateAuditCompliance(ctx, pool, tc.bad, "operator")
			if err == nil {
				t.Fatalf("negative %s accepted (returned %+v)", tc.name, out)
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error = %T (%v), want *settings.ValidationError so the API can map it to 400", err, err)
			}
			if !strings.Contains(ve.Field, tc.name) {
				t.Errorf("ValidationError.Field = %q, want it to name retentionDays.%s", ve.Field, tc.name)
			}
		})
	}

	// Nothing above may have reached the table.
	ac, err := GetAuditCompliance(ctx, pool)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ac.RetentionDays != baseline.RetentionDays {
		t.Errorf("retentionDays = %+v, want %+v: a rejected update must not persist", ac.RetentionDays, baseline.RetentionDays)
	}
}

// TestUpdateAuditComplianceRejectsAboveMax covers the fat-finger cap. The bound
// exists to catch a stray keystroke (e.g. an extra digit), not to express policy
// — "forever" already has a spelling, and it is 0.
func TestUpdateAuditComplianceRejectsAboveMax(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	// The cap itself is valid; one past it is not.
	if _, err := UpdateAuditCompliance(ctx, pool,
		AuditCompliance{RetentionDays: RetentionDays{Runs: maxRetentionDays}}, "operator"); err != nil {
		t.Fatalf("exactly maxRetentionDays should be accepted: %v", err)
	}

	_, err := UpdateAuditCompliance(ctx, pool,
		AuditCompliance{RetentionDays: RetentionDays{Runs: maxRetentionDays + 1}}, "operator")
	if err == nil {
		t.Fatal("a day count above maxRetentionDays was accepted")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %T (%v), want *settings.ValidationError", err, err)
	}
	if ve.Field != "retentionDays.runs" {
		t.Errorf("ValidationError.Field = %q, want retentionDays.runs", ve.Field)
	}

	// The rejected value must not have replaced the accepted one.
	ac, err := GetAuditCompliance(ctx, pool)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ac.RetentionDays.Runs != maxRetentionDays {
		t.Errorf("retentionDays.runs = %d, want %d: the over-cap update must not persist", ac.RetentionDays.Runs, maxRetentionDays)
	}
}

// TestUpdateAuditComplianceRoundTripsAllKnobs proves all six knobs survive a
// save/load unchanged. Each one now maps to a distinct field of
// db.RetentionPolicy (LU-2), so a marshalling slip here would silently apply the
// wrong window to a real table rather than just render oddly in the panel.
func TestUpdateAuditComplianceRoundTripsAllKnobs(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	in := AuditCompliance{RetentionDays: RetentionDays{
		Runs: 1, Activity: 2, WorkflowRuns: 3, ChangeLog: 4, SchedulePushes: 5, LogFiles: 6,
		RunnerPlacementHistory: 7, // DRF-7: the tenth knob round-trips like the rest
		ArchivedLogFiles:       8, // SL-4: the eleventh
	}}
	out, err := UpdateAuditCompliance(ctx, pool, in, "operator")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if out.RetentionDays != in.RetentionDays {
		t.Errorf("returned retentionDays = %+v, want %+v", out.RetentionDays, in.RetentionDays)
	}
	ac, err := GetAuditCompliance(ctx, pool)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ac.RetentionDays != in.RetentionDays {
		t.Errorf("stored retentionDays = %+v, want %+v", ac.RetentionDays, in.RetentionDays)
	}
}

// DRF-7: a blob stored before the tenth knob existed has no
// runnerPlacementHistory key. Unmarshalling over the defaults must leave it at
// 30 — not 0, which the sweeper reads as "keep forever" and would silently stop
// pruning the table the moment an operator upgraded.
func TestAuditComplianceLegacyBlobKeepsNewKnobDefault(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	legacy := `{"retentionDays":{"runs":90,"activity":90,"workflowRuns":90,"changeLog":365,"schedulePushes":365,"logFiles":90,"auditLogFiles":730,"recycleBin":30,"definitionRevisions":365}}`
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO settings (key, value, last_modified_by, last_modified_at) VALUES (?, ?, 'test', '2026-01-01T00:00:00Z')`,
		auditComplianceKey, legacy); err != nil {
		t.Fatal(err)
	}
	ac, err := GetAuditCompliance(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if ac.RetentionDays.RunnerPlacementHistory != 30 {
		t.Errorf("runnerPlacementHistory = %d on a legacy blob, want the default 30", ac.RetentionDays.RunnerPlacementHistory)
	}
}

// SL-4: the eleventh knob defaults to 0 (keep forever) on a legacy blob that
// predates it, validates like the rest, and is the only knob whose default is
// zero — pinned so a future "align the defaults" sweep cannot silently turn on
// bucket deletes.
func TestArchivedLogFilesKnobDefaultsToForever(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?)`, auditComplianceKey,
		`{"retentionDays":{"runs":1,"activity":1,"workflowRuns":1,"changeLog":1,"schedulePushes":1,"logFiles":1}}`); err != nil {
		t.Fatal(err)
	}
	ac, err := GetAuditCompliance(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if ac.RetentionDays.ArchivedLogFiles != 0 {
		t.Fatalf("archivedLogFiles on a legacy blob = %d, want 0", ac.RetentionDays.ArchivedLogFiles)
	}
	if defaultAuditCompliance().RetentionDays.ArchivedLogFiles != 0 {
		t.Fatal("the default must be keep-forever")
	}
	ac.RetentionDays.ArchivedLogFiles = -1
	if _, err := UpdateAuditCompliance(ctx, pool, *ac, "t"); err == nil {
		t.Fatal("negative window must be refused")
	}
}

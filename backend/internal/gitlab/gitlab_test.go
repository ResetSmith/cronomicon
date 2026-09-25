package gitlab

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// ──────────────────────────────────────────────────────────────────────────────
// Test helpers
// ──────────────────────────────────────────────────────────────────────────────

// mustOpenDB opens a SQLite DB with the minimal schema needed by B3 tests.
// It does NOT call db.Migrate (which includes all slices' migrations and fails
// if other slices have conflicting ALTER TABLE statements). Instead it applies
// only the tables B3 tests actually query.
func mustOpenDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	pool, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	// Apply only the tables B3 exercises (subset of full schema).
	schema := `
CREATE TABLE IF NOT EXISTS jobs (
    uid                TEXT, -- AF-4a surrogate identity (migration 1000)
    name               TEXT NOT NULL,
    source             TEXT NOT NULL DEFAULT 'git',
    run_type           TEXT NOT NULL,
    description        TEXT,
    scope              TEXT,
    target_host        TEXT,
    schedule           TEXT,
    tags               TEXT NOT NULL DEFAULT '[]',
    enabled            INTEGER NOT NULL DEFAULT 1,
    timeout_seconds    INTEGER,
    retries            INTEGER NOT NULL DEFAULT 0,
    requestable        INTEGER NOT NULL DEFAULT 0,
    -- SL (migration 930). This fixture is a hand-maintained SUBSET of the real
    -- schema, so every column the sync upsert writes has to be added here too
    -- or the upsert fails with "no such column" long after the migration landed.
    warn_after_seconds INTEGER,
    must_finish_by     TEXT,
    -- ET-D (migration 960). Same fixture-drift caveat as the two above.
    watch_json         TEXT,
    concurrency_policy TEXT NOT NULL DEFAULT 'Allow',
    concurrency_key    TEXT,
    command            TEXT,
    script             TEXT,
    script_path        TEXT,
    executor           TEXT,
    script_ref         TEXT,
    content_hash       TEXT,
    source_path        TEXT,
    synced_at          TEXT,
    prompts_json       TEXT NOT NULL DEFAULT '[]',
    env_passthrough    TEXT NOT NULL DEFAULT '[]',
    project_root       TEXT,
    requires_json      TEXT NOT NULL DEFAULT '[]',
    prompt_enforcement TEXT NOT NULL DEFAULT 'warn' CHECK (prompt_enforcement IN ('warn','block')),
    ssh_user           TEXT,
    ssh_credential     TEXT,
    become_password_secret TEXT,
    -- RT-2 (migration 1070). Same fixture-drift caveat as the columns above.
    -- The operator-override sibling that lived here until v1.3.5 is gone with
    -- migration 1090; the declared pin is the whole of the job-level layer now.
    runner_tag          TEXT,
    PRIMARY KEY (source, name)
);

-- LU-6: the sync path now allocates a log-folder code per definition, so this
-- minimal schema needs the registry too. Mirrors migration 710.
CREATE TABLE IF NOT EXISTS entity_codes (
    code       INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL,
    source     TEXT NOT NULL,
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    deleted_at TEXT,
    -- R2-2 (migration 1020). Same fixture-drift caveat as everything else here.
    uid        TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_entity_codes_live
    ON entity_codes(kind, source, name) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS scripts (
    name         TEXT PRIMARY KEY,
    description  TEXT,
    run_type     TEXT NOT NULL,
    command      TEXT,
    script       TEXT,
    script_path  TEXT,
    executor     TEXT,
    content_hash TEXT NOT NULL,
    source_path  TEXT,
    synced_at    TEXT NOT NULL,
    warnings     TEXT NOT NULL DEFAULT '[]',
    variables    TEXT NOT NULL DEFAULT '[]',
    prompts_json TEXT NOT NULL DEFAULT '[]',
    tags         TEXT NOT NULL DEFAULT '[]',
    project_root TEXT
);

CREATE TABLE IF NOT EXISTS workflows (
    uid                TEXT, -- AF-4a surrogate identity (migration 1000)
    name        TEXT NOT NULL,
    source      TEXT NOT NULL DEFAULT 'git',
    description TEXT,
    steps       TEXT NOT NULL DEFAULT '[]',
    schedule    TEXT,
    enabled     INTEGER NOT NULL DEFAULT 1,
    source_path TEXT,
    synced_at   TEXT,
    tags        TEXT NOT NULL DEFAULT '[]',
    PRIMARY KEY (source, name)
);

CREATE TABLE IF NOT EXISTS definition_schedules (
    owner_source TEXT NOT NULL DEFAULT 'git' CHECK (owner_source IN ('git','amadeus')),
    owner_kind  TEXT NOT NULL CHECK (owner_kind IN ('job','workflow')),
    owner_name  TEXT NOT NULL,
    name        TEXT NOT NULL,
    cron        TEXT NOT NULL,
    env         TEXT,
    position    INTEGER NOT NULL DEFAULT 0,
    source_ref  TEXT,
    start_at    TEXT,
    end_at      TEXT,
    interval    TEXT,
    skip_calendars TEXT,
    only_calendars TEXT,
    -- R2-2 (migration 1020).
    owner_uid    TEXT,
    schedule_uid TEXT,
    PRIMARY KEY (owner_source, owner_kind, owner_name, name)
);

-- RX-13. This subset schema is hand-maintained and does NOT run migrations, so
-- a table the sync path writes must be added here or every test in this package
-- fails on "no such table". The cascade trigger comes with it: without the
-- trigger the prune tests would silently prove the wrong thing about what a
-- deleted job takes with it.
CREATE TABLE IF NOT EXISTS reactions (
    owner_source  TEXT NOT NULL DEFAULT 'git' CHECK (owner_source IN ('git','amadeus')),
    owner_kind    TEXT NOT NULL CHECK (owner_kind IN ('job','workflow')),
    owner_name    TEXT NOT NULL,
    name          TEXT NOT NULL,
    on_source     TEXT NOT NULL DEFAULT 'git' CHECK (on_source IN ('git','amadeus')),
    on_kind       TEXT NOT NULL CHECK (on_kind IN ('job','workflow')),
    on_name       TEXT NOT NULL,
    on_outcome    TEXT NOT NULL CHECK (on_outcome IN ('success','failure','stopped','any')),
    delay_seconds INTEGER NOT NULL DEFAULT 0 CHECK (delay_seconds >= 0),
    min_interval_seconds INTEGER NOT NULL DEFAULT 0 CHECK (min_interval_seconds >= 0),
    include_workflow_children INTEGER NOT NULL DEFAULT 0 CHECK (include_workflow_children IN (0,1)),
    enabled       INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    position      INTEGER NOT NULL DEFAULT 0,
    -- R2-2 (migration 1020).
    owner_uid     TEXT,
    on_uid        TEXT,
    PRIMARY KEY (owner_source, owner_kind, owner_name, name)
);

CREATE TRIGGER IF NOT EXISTS trg_reactions_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM reactions
     WHERE owner_kind = 'job' AND owner_source = OLD.source AND owner_name = OLD.name;
END;

CREATE TRIGGER IF NOT EXISTS trg_reactions_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM reactions
     WHERE owner_kind = 'workflow' AND owner_source = OLD.source AND owner_name = OLD.name;
END;

CREATE TABLE IF NOT EXISTS calendars (
    source            TEXT NOT NULL DEFAULT 'amadeus',
    name              TEXT NOT NULL,
    description       TEXT,
    global            INTEGER NOT NULL DEFAULT 0,
    record_suppressed INTEGER NOT NULL DEFAULT 0,
    created_by        TEXT,
    created_at        TEXT NOT NULL DEFAULT '2026-01-01T00:00:00Z',
    last_modified_by  TEXT,
    last_modified_at  TEXT,
    PRIMARY KEY (source, name)
);

CREATE TABLE IF NOT EXISTS calendar_days (
    calendar_source TEXT NOT NULL,
    calendar_name   TEXT NOT NULL,
    day             TEXT NOT NULL,
    label           TEXT,
    rule            TEXT,
    PRIMARY KEY (calendar_source, calendar_name, day)
);

CREATE TABLE IF NOT EXISTS schedules (
    uid                TEXT, -- AF-4a surrogate identity (migration 1000)
    name             TEXT NOT NULL,
    source           TEXT NOT NULL DEFAULT 'git' CHECK (source IN ('git','amadeus')),
    description      TEXT,
    cron             TEXT NOT NULL,
    env              TEXT,
    content_hash     TEXT NOT NULL,
    source_path      TEXT,
    synced_at        TEXT,
    created_by       TEXT,
    created_at       TEXT,
    last_modified_by TEXT,
    last_modified_at TEXT,
    tags             TEXT NOT NULL DEFAULT '[]',
    start_at         TEXT,
    end_at           TEXT,
    interval         TEXT,
    skip_calendars   TEXT,
    only_calendars   TEXT,
    PRIMARY KEY (source, name)
);

CREATE TABLE IF NOT EXISTS git_sync_state (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    last_sha    TEXT,
    last_synced_at TEXT,
    last_status TEXT
);

CREATE TABLE IF NOT EXISTS git_sync_events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    triggered_by  TEXT NOT NULL,
    sha           TEXT,
    status        TEXT NOT NULL,
    jobs_synced      INTEGER NOT NULL DEFAULT 0,
    scripts_synced   INTEGER NOT NULL DEFAULT 0,
    schedules_synced INTEGER NOT NULL DEFAULT 0,
    wfs_synced       INTEGER NOT NULL DEFAULT 0,
    scopes_synced    INTEGER NOT NULL DEFAULT 0,
    error_message  TEXT,
    started_at     TEXT NOT NULL,
    finished_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS schedule_pushes (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    at            TEXT NOT NULL,
    actor         TEXT NOT NULL,
    schedule_file TEXT NOT NULL,
    base_sha      TEXT,
    new_sha       TEXT,
    status        TEXT NOT NULL,
    trace_id      TEXT,
    details       TEXT,
    created_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS activity (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    kind          TEXT NOT NULL,
    outcome       TEXT,
    actor         TEXT,
    job_name      TEXT,
    workflow_name TEXT,
    target        TEXT,
    scope         TEXT,
    schedule_file TEXT,
    category      TEXT,
    summary       TEXT,
    details       TEXT,
    trace_id      TEXT,
    at            TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    -- Added by migration 030. The fixture must carry them because the shared
    -- auditlog writer inserts the full 22-column row; a subset table makes every
    -- activity write fail (and the writes are best-effort, so it fails silently).
    job_id        INTEGER,
    workflow_id   INTEGER,
    action        TEXT,
    commit_sha    TEXT,
    repository    TEXT,
    branch        TEXT,
    duration_ms   INTEGER,
    killed_by     TEXT,
    -- R2-1 (migration 1010). Same fixture-drift caveat as job_id above.
    job_uid       TEXT,
    workflow_uid  TEXT,
    -- AA-1 (migration 1100). Same caveat again -- and this one cost a red
    -- TestRecordPush to rediscover, so: any column added to the activity table
    -- must be added HERE too, or every activity write in this package starts
    -- failing silently and the symptom surfaces somewhere unrelated.
    runner_name   TEXT
);

-- R2-1: the activity writer resolves a missing uid through the run the event
-- describes, so its INSERT names these two tables. SQLite compiles the whole
-- statement up front, which means their ABSENCE breaks every activity write in
-- this package — including pushes, which reference no run at all. They are here
-- to be nameable, hence the two columns each path actually reads.
CREATE TABLE IF NOT EXISTS runs (
    id       TEXT PRIMARY KEY,
    job_uid  TEXT
);
CREATE TABLE IF NOT EXISTS workflow_runs (
    id           TEXT PRIMARY KEY,
    workflow_uid TEXT
);

-- AN-1 (migration 1060). Same hand-maintained-subset caveat as reactions above,
-- and the same reason for bringing the triggers along: without them the prune
-- test would silently prove the wrong thing about what a deleted git job takes
-- with it. Keyed on OLD.uid with no name arm, exactly as 1060 defines them.
CREATE TABLE IF NOT EXISTS annotations (
    owner_kind TEXT NOT NULL CHECK (owner_kind IN ('job','workflow')),
    owner_uid  TEXT NOT NULL,
    critical   INTEGER NOT NULL DEFAULT 0 CHECK (critical IN (0,1)),
    contact    TEXT NOT NULL DEFAULT '',
    notes      TEXT NOT NULL DEFAULT '',
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (owner_kind, owner_uid)
);

CREATE TRIGGER IF NOT EXISTS annotations_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM annotations WHERE owner_kind = 'job' AND owner_uid = OLD.uid;
END;

CREATE TRIGGER IF NOT EXISTS annotations_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM annotations WHERE owner_kind = 'workflow' AND owner_uid = OLD.uid;
END;
`
	if _, err := pool.Exec(schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return pool
}

// ──────────────────────────────────────────────────────────────────────────────
// T1: pragma parsing — valid directives
// ──────────────────────────────────────────────────────────────────────────────

func TestParseCronomiconPragma_Valid(t *testing.T) {
	content := `# cronomicon:v1 types=bash,ansible,terraform
# cronomicon:v1 owner=infra-platform
# cronomicon:v1 description=Production inventory

[webservers]
web-01.prod.internal
`
	dir, errs := parseCronomiconPragma(content)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(dir.Types) != 3 {
		t.Errorf("want 3 types, got %v", dir.Types)
	}
	if dir.Owner != "infra-platform" {
		t.Errorf("want owner infra-platform, got %q", dir.Owner)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// T2: pragma parsing — unknown run types produce line-numbered errors
// ──────────────────────────────────────────────────────────────────────────────

func TestParseCronomiconPragma_UnknownRunType(t *testing.T) {
	content := "# cronomicon:v1 types=bahs,ansibl\n[qa_hosts]\nqa-01\n"
	_, errs := parseCronomiconPragma(content)
	if len(errs) == 0 {
		t.Fatal("expected errors for unknown run types, got none")
	}
	if errs[0].Line != 1 {
		t.Errorf("want error on line 1, got line %d", errs[0].Line)
	}
	if !strings.Contains(errs[0].Message, "bahs") {
		t.Errorf("expected 'bahs' in error message, got: %s", errs[0].Message)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// T3: pragma parsing — unsupported version
// ──────────────────────────────────────────────────────────────────────────────

func TestParseCronomiconPragma_UnsupportedVersion(t *testing.T) {
	content := "# cronomicon:v2 types=bash\n[hosts]\nhost-01\n"
	_, errs := parseCronomiconPragma(content)
	if len(errs) == 0 {
		t.Fatal("expected error for unsupported pragma version")
	}
	if !strings.Contains(errs[0].Message, "v2") {
		t.Errorf("expected v2 in error: %s", errs[0].Message)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// T4: pragma parsing — unknown directive key
// ──────────────────────────────────────────────────────────────────────────────

func TestParseCronomiconPragma_UnknownDirective(t *testing.T) {
	content := "# cronomicon:v1 ownr=qa-team\n[hosts]\nhost-01\n"
	_, errs := parseCronomiconPragma(content)
	if len(errs) == 0 {
		t.Fatal("expected error for unknown directive")
	}
	if !strings.Contains(errs[0].Message, "ownr") {
		t.Errorf("expected 'ownr' in error: %s", errs[0].Message)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// T5: pragma parsing — stops at first non-comment line
// ──────────────────────────────────────────────────────────────────────────────

func TestParseCronomiconPragma_StopsAtNonComment(t *testing.T) {
	content := "[hosts]\n# cronomicon:v1 types=bash\nhost-01\n"
	dir, _ := parseCronomiconPragma(content)
	if len(dir.Types) != 0 {
		t.Errorf("expected no types (pragma after section), got %v", dir.Types)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// T6: apiVersion validation — valid YAML
// ──────────────────────────────────────────────────────────────────────────────

func TestValidateYAMLBytes_Valid(t *testing.T) {
	content := "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: test-job\nspec:\n  run_type: bash\n  command: echo hi\n"
	errs, err := validateYAMLBytes("test.yaml", []byte(content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("expected no errors, got: %v", errs)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// T7: apiVersion validation — wrong apiVersion (T10)
// ──────────────────────────────────────────────────────────────────────────────

func TestValidateYAMLBytes_WrongAPIVersion(t *testing.T) {
	content := "apiVersion: cronomicon.io/v2\nkind: Job\nmetadata:\n  name: test-job\n"
	errs, err := validateYAMLBytes("test.yaml", []byte(content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(errs) == 0 {
		t.Fatal("expected error for wrong apiVersion")
	}
	if errs[0].Field != "apiVersion" {
		t.Errorf("expected field=apiVersion, got %q", errs[0].Field)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// T8: apiVersion validation — unknown kind (T10)
// ──────────────────────────────────────────────────────────────────────────────

func TestValidateYAMLBytes_UnknownKind(t *testing.T) {
	content := "apiVersion: cronomicon.io/v1\nkind: CronJob\nmetadata:\n  name: test-job\n"
	errs, err := validateYAMLBytes("test.yaml", []byte(content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(errs) == 0 {
		t.Fatal("expected error for unknown kind")
	}
	if errs[0].Field != "kind" {
		t.Errorf("expected field=kind, got %q", errs[0].Field)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// T9: apiVersion validation — missing both fields
// ──────────────────────────────────────────────────────────────────────────────

func TestValidateYAMLBytes_MissingFields(t *testing.T) {
	content := "metadata:\n  name: test-job\n"
	errs, err := validateYAMLBytes("test.yaml", []byte(content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(errs) < 2 {
		t.Errorf("expected at least 2 errors (apiVersion + kind), got %d: %v", len(errs), errs)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// T10: capability resolution
// ──────────────────────────────────────────────────────────────────────────────

func TestResolveInventoryCapability_PragmaPrecedence(t *testing.T) {
	content := "# cronomicon:v1 types=bash,terraform\n[hosts]\nhost-01\n"
	cap := resolveInventoryCapability(content, nil, "")
	if cap.Origin != OriginPragma {
		t.Errorf("want origin=pragma, got %q", cap.Origin)
	}
	if len(cap.Types) != 2 {
		t.Errorf("want 2 types, got %v", cap.Types)
	}
}

func TestResolveInventoryCapability_SidecarWins(t *testing.T) {
	content := "# cronomicon:v1 types=bash\n[hosts]\nhost-01\n"
	sc := &sidecarYAML{}
	sc.Spec.Types = []string{"bash", "ansible", "terraform"}
	cap := resolveInventoryCapability(content, sc, "inventory/foo.cronomicon.yaml")
	if cap.Origin != OriginSidecar {
		t.Errorf("want origin=sidecar, got %q", cap.Origin)
	}
	if len(cap.Types) != 3 {
		t.Errorf("want 3 types from sidecar, got %v", cap.Types)
	}
}

func TestResolveInventoryCapability_Inferred(t *testing.T) {
	content := "[hosts]\nhost-01\n"
	cap := resolveInventoryCapability(content, nil, "")
	if cap.Origin != OriginInferred {
		t.Errorf("want origin=inferred, got %q", cap.Origin)
	}
	if len(cap.Types) == 0 {
		t.Error("inferred types should not be empty")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// T11: publish precondition — error when clone missing
// ──────────────────────────────────────────────────────────────────────────────

func TestPublish_NoClone(t *testing.T) {
	dir := t.TempDir()
	svc := &Service{
		cloneDir:      filepath.Join(dir, "nonexistent"),
		webhookSecret: "sec",
	}
	_, err := svc.Publish(context.Background(), PublishRequest{
		FilePath: "jobs/test.yaml",
		Content:  "apiVersion: cronomicon.io/v1\nkind: Job\n",
	}, "abc123", "user@example.com")
	if err == nil {
		t.Fatal("expected error when clone doesn't exist")
	}
	// Should not be a PreconditionError — it's a clone open error.
	if _, ok := err.(*PreconditionError); ok {
		t.Error("should not be a PreconditionError when clone doesn't exist")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// T12: DB upsert — jobs are stored correctly
// ──────────────────────────────────────────────────────────────────────────────

func TestUpsertJobs(t *testing.T) {
	pool := mustOpenDB(t)

	svc := &Service{db: pool, cloneDir: t.TempDir()}

	enabled := true
	j := JobYAML{}
	j.APIVersion = requiredAPIVersion
	j.Kind = "Job"
	j.Metadata.Name = "test-upsert-job"
	j.Spec.RunType = "bash"
	j.Spec.Schedule = "*/15 * * * *"
	j.Spec.Scope = "Prod"
	j.Spec.TargetHost = "host-01"
	j.Spec.Tags = []string{"infra"}
	j.Spec.Enabled = &enabled
	j.Spec.ConcurrencyPolicy = "Allow"

	tx, err := pool.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	nowStr := time.Now().UTC().Format(time.RFC3339)
	if err := svc.upsertJobs(context.Background(), tx, []JobYAML{j}, nil, nil, nowStr, "sha123"); err != nil {
		t.Fatalf("upsertJobs: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var name, runType string
	err = pool.QueryRow(`SELECT name, run_type FROM jobs WHERE name='test-upsert-job'`).Scan(&name, &runType)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if name != "test-upsert-job" || runType != "bash" {
		t.Errorf("unexpected row: name=%q run_type=%q", name, runType)
	}
}

// UDV1 — a job's declared spec.prompts are serialized to jobs.prompts_json on sync,
// round-trip through the column, and a job with no prompts defaults to '[]'.
func TestUpsertJobs_Prompts(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{db: pool}

	def := "staging"
	withPrompts := JobYAML{}
	withPrompts.APIVersion = requiredAPIVersion
	withPrompts.Kind = "Job"
	withPrompts.Metadata.Name = "prompted-job"
	withPrompts.Spec.RunType = "bash"
	withPrompts.Spec.ConcurrencyPolicy = "Allow"
	withPrompts.Spec.Prompts = []PromptSpec{
		{Name: "TARGET_ENV", Label: "Deployment environment", Required: true, Options: []string{"dev", "staging", "prod"}},
		{Name: "REPLICAS", Default: &def},
		{Name: "", Label: "dropped — empty name"}, // MarshalPrompts must drop nameless rows
	}

	plain := JobYAML{}
	plain.APIVersion = requiredAPIVersion
	plain.Kind = "Job"
	plain.Metadata.Name = "plain-job"
	plain.Spec.RunType = "bash"
	plain.Spec.ConcurrencyPolicy = "Allow"

	tx, err := pool.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	nowStr := time.Now().UTC().Format(time.RFC3339)
	if err := svc.upsertJobs(context.Background(), tx, []JobYAML{withPrompts, plain}, nil, nil, nowStr, "sha123"); err != nil {
		t.Fatalf("upsertJobs: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var raw string
	if err := pool.QueryRow(`SELECT prompts_json FROM jobs WHERE name='prompted-job'`).Scan(&raw); err != nil {
		t.Fatalf("query prompted-job: %v", err)
	}
	var got []PromptSpec
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal prompts_json %q: %v", raw, err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 persisted prompts (empty-name dropped), got %d: %q", len(got), raw)
	}
	if got[0].Name != "TARGET_ENV" || !got[0].Required || len(got[0].Options) != 3 {
		t.Errorf("TARGET_ENV not round-tripped: %+v", got[0])
	}
	if got[1].Name != "REPLICAS" || got[1].Default == nil || *got[1].Default != "staging" {
		t.Errorf("REPLICAS default not round-tripped: %+v", got[1])
	}

	var plainRaw string
	if err := pool.QueryRow(`SELECT prompts_json FROM jobs WHERE name='plain-job'`).Scan(&plainRaw); err != nil {
		t.Fatalf("query plain-job: %v", err)
	}
	if plainRaw != "[]" {
		t.Errorf("a job with no prompts should default to '[]', got %q", plainRaw)
	}
}

// RX.9 (Phase 1) — a job's declared spec.env_passthrough NAMES are serialized to
// jobs.env_passthrough on sync (blanks dropped), and a job without the field
// defaults to '[]'.
func TestUpsertJobs_EnvPassthrough(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{db: pool}

	withNames := JobYAML{}
	withNames.APIVersion = requiredAPIVersion
	withNames.Kind = "Job"
	withNames.Metadata.Name = "passthrough-job"
	withNames.Spec.RunType = "ansible"
	withNames.Spec.ConcurrencyPolicy = "Allow"
	withNames.Spec.EnvPassthrough = []string{"VCENTER_TOKEN", " ", "WEB_PASS"} // blank dropped

	plain := JobYAML{}
	plain.APIVersion = requiredAPIVersion
	plain.Kind = "Job"
	plain.Metadata.Name = "plain-env-job"
	plain.Spec.RunType = "ansible"
	plain.Spec.ConcurrencyPolicy = "Allow"

	tx, err := pool.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	nowStr := time.Now().UTC().Format(time.RFC3339)
	if err := svc.upsertJobs(context.Background(), tx, []JobYAML{withNames, plain}, nil, nil, nowStr, "sha123"); err != nil {
		t.Fatalf("upsertJobs: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var raw string
	if err := pool.QueryRow(`SELECT env_passthrough FROM jobs WHERE name='passthrough-job'`).Scan(&raw); err != nil {
		t.Fatalf("query passthrough-job: %v", err)
	}
	var got []string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal env_passthrough %q: %v", raw, err)
	}
	if len(got) != 2 || got[0] != "VCENTER_TOKEN" || got[1] != "WEB_PASS" {
		t.Errorf("env_passthrough not round-tripped (blank dropped): %q", raw)
	}

	var plainRaw string
	if err := pool.QueryRow(`SELECT env_passthrough FROM jobs WHERE name='plain-env-job'`).Scan(&plainRaw); err != nil {
		t.Fatalf("query plain-env-job: %v", err)
	}
	if plainRaw != "[]" {
		t.Errorf("a job with no env_passthrough should default to '[]', got %q", plainRaw)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// T13: webhook handler — valid and invalid token
// ──────────────────────────────────────────────────────────────────────────────

func TestWebhookHandler_TokenValidation(t *testing.T) {
	pool := mustOpenDB(t)

	// TriggerSync starts a goroutine — give it a clone dir that doesn't exist so
	// it fails fast (no real GitLab needed). The goroutine is allowed to fail;
	// we only test the HTTP response from the handler itself.
	svc := &Service{
		db:            pool,
		cloneDir:      filepath.Join(t.TempDir(), "clone"),
		repoURL:       "https://example.com/does-not-exist.git",
		webhookSecret: "correct-secret",
	}
	h := NewHandlers(svc)

	// Valid token → 202.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", strings.NewReader(`{}`))
	req.Header.Set("X-Gitlab-Token", "correct-secret")
	rec := httptest.NewRecorder()
	h.WebhookGitLab(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Errorf("valid token: want 202, got %d", rec.Code)
	}

	// Invalid token → 401.
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", strings.NewReader(`{}`))
	req2.Header.Set("X-Gitlab-Token", "wrong-secret")
	rec2 := httptest.NewRecorder()
	h.WebhookGitLab(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("invalid token: want 401, got %d", rec2.Code)
	}

	// Missing token → 401.
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", strings.NewReader(`{}`))
	rec3 := httptest.NewRecorder()
	h.WebhookGitLab(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Errorf("missing token: want 401, got %d", rec3.Code)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// T14: recordSyncEvent round-trip
// ──────────────────────────────────────────────────────────────────────────────

func TestRecordSyncEvent(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{db: pool}
	now := time.Now().UTC()
	res := SyncResult{
		SHA:        "deadbeef",
		Status:     "success",
		JobsSynced: 5,
		StartedAt:  now,
		FinishedAt: now.Add(2 * time.Second),
	}
	if err := svc.recordSyncEvent(context.Background(), "manual", res); err != nil {
		t.Fatalf("recordSyncEvent: %v", err)
	}

	var sha, status string
	var jobs int
	err := pool.QueryRow(`SELECT sha, status, jobs_synced FROM git_sync_events LIMIT 1`).Scan(&sha, &status, &jobs)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if sha != "deadbeef" || status != "success" || jobs != 5 {
		t.Errorf("unexpected row: sha=%q status=%q jobs=%d", sha, status, jobs)
	}
}

// TestListSyncEventsSpecShape locks the GET /git/history item shape to the
// spec's GitSyncEvent schema (timestamp/action/commit/author/message/changes/
// details; status ∈ success|warning|failure) — the DB row uses different
// column names and statuses, and the translation lives in ListSyncEvents.
func TestListSyncEventsSpecShape(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{db: pool}
	now := time.Now().UTC()

	ok := SyncResult{
		SHA: "deadbeef", Status: "success",
		JobsSynced: 3, WfsSynced: 2, ScopesSynced: 1,
		StartedAt: now, FinishedAt: now.Add(time.Second),
	}
	if err := svc.recordSyncEvent(context.Background(), "manual", ok); err != nil {
		t.Fatalf("recordSyncEvent(success): %v", err)
	}
	bad := SyncResult{
		Status: "failed", ErrorMessage: "git fetch: auth failed",
		StartedAt: now.Add(time.Minute), FinishedAt: now.Add(time.Minute + time.Second),
	}
	if err := svc.recordSyncEvent(context.Background(), "webhook", bad); err != nil {
		t.Fatalf("recordSyncEvent(failed): %v", err)
	}

	events, total, err := svc.ListSyncEvents(context.Background(), 1, 50, "")
	if err != nil {
		t.Fatalf("ListSyncEvents: %v", err)
	}
	if total != 2 || len(events) != 2 {
		t.Fatalf("want 2 events, got total=%d len=%d", total, len(events))
	}

	// Newest first: the failed event.
	failed, success := events[0], events[1]
	if failed["status"] != "failure" {
		t.Errorf("failed status: want %q, got %q", "failure", failed["status"])
	}
	if failed["details"] != "git fetch: auth failed" {
		t.Errorf("failed details: got %q", failed["details"])
	}
	if failed["commit"] != nil {
		t.Errorf("failed commit: want nil, got %v", failed["commit"])
	}
	if failed["author"] != "webhook" {
		t.Errorf("failed author: got %q", failed["author"])
	}

	if success["status"] != "success" {
		t.Errorf("success status: got %q", success["status"])
	}
	if success["commit"] != "deadbeef" {
		t.Errorf("success commit: got %v", success["commit"])
	}
	if success["timestamp"] != now.Format(time.RFC3339) {
		t.Errorf("success timestamp: want %q, got %q", now.Format(time.RFC3339), success["timestamp"])
	}
	if success["action"] != "Pulled" {
		t.Errorf("success action: got %q", success["action"])
	}
	if success["message"] != "Synced 3 jobs, 0 scripts, 0 schedules, 2 workflows, 1 scopes" {
		t.Errorf("success message: got %q", success["message"])
	}

	// Legacy internal keys must not leak onto the wire.
	for _, k := range []string{"sha", "triggeredBy", "jobsSynced", "wfsSynced", "scopesSynced", "errorMessage", "startedAt", "finishedAt"} {
		if _, leak := success[k]; leak {
			t.Errorf("legacy key %q leaked into the API payload", k)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// T15: ValidateFile on real temp files (T11 entry point)
// ──────────────────────────────────────────────────────────────────────────────

func TestValidateFile_YAML(t *testing.T) {
	dir := t.TempDir()

	// Valid.
	valid := filepath.Join(dir, "valid.yaml")
	_ = os.WriteFile(valid, []byte("apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: x\nspec:\n  run_type: bash\n  command: echo hi\n"), 0o644)
	errs, err := ValidateFile(valid)
	if err != nil {
		t.Fatalf("ValidateFile error: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("valid YAML should have no errors, got: %v", errs)
	}

	// Invalid.
	invalid := filepath.Join(dir, "invalid.yaml")
	_ = os.WriteFile(invalid, []byte("apiVersion: cronomicon.io/v9\nkind: Garbage\n"), 0o644)
	errs2, err2 := ValidateFile(invalid)
	if err2 != nil {
		t.Fatalf("ValidateFile error: %v", err2)
	}
	if len(errs2) == 0 {
		t.Error("invalid YAML should produce errors")
	}
}

func TestValidateFile_INI(t *testing.T) {
	dir := t.TempDir()
	ini := filepath.Join(dir, "prod.ini")
	_ = os.WriteFile(ini, []byte("# cronomicon:v1 types=bahs\n[hosts]\nhost-01\n"), 0o644)
	errs, err := ValidateFile(ini)
	if err != nil {
		t.Fatalf("ValidateFile error: %v", err)
	}
	if len(errs) == 0 {
		t.Error("invalid type in INI pragma should produce errors")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// T16: RecordPush inserts schedule_pushes and activity rows
// ──────────────────────────────────────────────────────────────────────────────

func TestRecordPush(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{db: pool}

	if err := svc.RecordPush(context.Background(),
		"alice@example.com", "jobs/test.yaml", "sha1", "sha2", "success", ""); err != nil {
		t.Fatalf("RecordPush: %v", err)
	}

	var actor, file, status string
	err := pool.QueryRow(`SELECT actor, schedule_file, status FROM schedule_pushes LIMIT 1`).Scan(&actor, &file, &status)
	if err != nil {
		t.Fatalf("query schedule_pushes: %v", err)
	}
	if actor != "alice@example.com" || file != "jobs/test.yaml" || status != "success" {
		t.Errorf("unexpected row: actor=%q file=%q status=%q", actor, file, status)
	}

	// Activity row should also exist.
	var actKind string
	err = pool.QueryRow(`SELECT kind FROM activity WHERE kind='push' LIMIT 1`).Scan(&actKind)
	if err != nil {
		t.Fatalf("query activity: %v", err)
	}
	if actKind != "push" {
		t.Errorf("expected activity kind=push, got %q", actKind)
	}
}

// Phase 3 (§5/RX.13) — a job's spec.requires tokens serialize to
// jobs.requires_json on sync; a job without them defaults to '[]'.
func TestUpsertJobs_Requires(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{db: pool}

	withReq := JobYAML{}
	withReq.APIVersion = requiredAPIVersion
	withReq.Kind = "Job"
	withReq.Metadata.Name = "vault-job"
	withReq.Spec.RunType = "ansible"
	withReq.Spec.ConcurrencyPolicy = "Allow"
	withReq.Spec.Requires = []string{"vault", " ", "checkout"} // blank dropped

	plain := JobYAML{}
	plain.APIVersion = requiredAPIVersion
	plain.Kind = "Job"
	plain.Metadata.Name = "plain-req-job"
	plain.Spec.RunType = "ansible"
	plain.Spec.ConcurrencyPolicy = "Allow"

	tx, err := pool.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	nowStr := time.Now().UTC().Format(time.RFC3339)
	if err := svc.upsertJobs(context.Background(), tx, []JobYAML{withReq, plain}, nil, nil, nowStr, "sha123"); err != nil {
		t.Fatalf("upsertJobs: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var raw string
	if err := pool.QueryRow(`SELECT requires_json FROM jobs WHERE name='vault-job'`).Scan(&raw); err != nil {
		t.Fatalf("query vault-job: %v", err)
	}
	var got []string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal requires_json %q: %v", raw, err)
	}
	if len(got) != 2 || got[0] != "vault" || got[1] != "checkout" {
		t.Errorf("requires_json not round-tripped (blank dropped): %q", raw)
	}

	var plainRaw string
	if err := pool.QueryRow(`SELECT requires_json FROM jobs WHERE name='plain-req-job'`).Scan(&plainRaw); err != nil {
		t.Fatalf("query plain-req-job: %v", err)
	}
	if plainRaw != "[]" {
		t.Errorf("a job with no requires should default to '[]', got %q", plainRaw)
	}
}

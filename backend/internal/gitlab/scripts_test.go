package gitlab

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────────
// Script-kind YAML validation (B-Git)
// ──────────────────────────────────────────────────────────────────────────────

func TestValidateYAMLBytes_Script(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr bool
		field   string // expected first error field when wantErr
	}{
		{
			name:    "valid inline command",
			content: "apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: backup-db\nspec:\n  run_type: bash\n  command: echo hi\n",
			wantErr: false,
		},
		{
			name:    "missing body",
			content: "apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: s\nspec:\n  run_type: bash\n",
			wantErr: true,
			field:   "spec",
		},
		{
			name:    "two bodies",
			content: "apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: s\nspec:\n  run_type: bash\n  command: a\n  script: b\n",
			wantErr: true,
			field:   "spec",
		},
		{
			name:    "missing run_type",
			content: "apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: s\nspec:\n  command: echo hi\n",
			wantErr: true,
			field:   "spec.run_type",
		},
		{
			name:    "unknown run_type",
			content: "apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: s\nspec:\n  run_type: cobol\n  command: echo hi\n",
			wantErr: true,
			field:   "spec.run_type",
		},
		{
			name:    "scriptPath escape",
			content: "apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: s\nspec:\n  run_type: bash\n  scriptPath: ../etc/passwd\n",
			wantErr: true,
			field:   "spec.scriptPath",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs, err := validateYAMLBytes("scripts/s.yaml", []byte(c.content))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.wantErr && len(errs) == 0 {
				t.Fatalf("expected validation error, got none")
			}
			if !c.wantErr && len(errs) != 0 {
				t.Fatalf("expected no errors, got %v", errs)
			}
			if c.wantErr && c.field != "" {
				found := false
				for _, e := range errs {
					if e.Field == c.field {
						found = true
					}
				}
				if !found {
					t.Errorf("expected an error on field %q, got %v", c.field, errs)
				}
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Job source: script_ref XOR inline body
// ──────────────────────────────────────────────────────────────────────────────

func TestValidateYAMLBytes_JobScriptRef(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr bool
	}{
		{
			name:    "script_ref only",
			content: "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  script_ref: backup-db\n  scope: Prod\n",
			wantErr: false,
		},
		{
			name:    "script_ref and inline body → error",
			content: "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  script_ref: backup-db\n  command: echo hi\n",
			wantErr: true,
		},
		{
			name:    "neither → error",
			content: "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  scope: Prod\n",
			wantErr: true,
		},
		{
			name:    "inline body only (legacy) still valid",
			content: "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  run_type: bash\n  command: echo hi\n",
			wantErr: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs, err := validateYAMLBytes("jobs/j.yaml", []byte(c.content))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.wantErr && len(errs) == 0 {
				t.Fatalf("expected validation error, got none")
			}
			if !c.wantErr && len(errs) != 0 {
				t.Fatalf("expected no errors, got %v", errs)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// ContentHash format (Decision 8)
// ──────────────────────────────────────────────────────────────────────────────

func TestContentHash(t *testing.T) {
	h := ContentHash([]byte("echo hi"))
	// sha256("echo hi") is a known value; assert prefix + length + determinism.
	if len(h) != len("sha256:")+64 {
		t.Errorf("unexpected hash length: %q", h)
	}
	if h[:7] != "sha256:" {
		t.Errorf("missing sha256: prefix: %q", h)
	}
	if ContentHash([]byte("echo hi")) != h {
		t.Error("hash is not deterministic")
	}
	if ContentHash([]byte("echo bye")) == h {
		t.Error("different bodies should hash differently")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// ValidateRepo: cross-file script_ref resolution + orphan warnings
// ──────────────────────────────────────────────────────────────────────────────

func TestValidateRepo_CrossRef(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "scripts", "backup-db.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: backup-db\nspec:\n  run_type: bash\n  command: pg_dump\n")
	mustWrite(t, filepath.Join(dir, "scripts", "orphan.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: orphan\nspec:\n  run_type: bash\n  command: echo unused\n")
	// good job references backup-db; bad job references a missing script.
	mustWrite(t, filepath.Join(dir, "jobs", "nightly.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: nightly\nspec:\n  script_ref: backup-db\n  scope: Prod\n")
	mustWrite(t, filepath.Join(dir, "jobs", "broken.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: broken\nspec:\n  script_ref: does-not-exist\n  scope: Prod\n")

	errs, warnings, err := ValidateRepo(dir)
	if err != nil {
		t.Fatalf("ValidateRepo: %v", err)
	}
	// Exactly one hard error: the dangling script_ref.
	dangling := 0
	for _, e := range errs {
		if e.Field == "spec.script_ref" {
			dangling++
		}
	}
	if dangling != 1 {
		t.Errorf("want 1 dangling script_ref error, got %d (errs=%v)", dangling, errs)
	}
	// Exactly one orphan warning: the unreferenced script.
	if len(warnings) != 1 {
		t.Errorf("want 1 orphan warning, got %d (%v)", len(warnings), warnings)
	}
}

// TestUpsertScripts_DeclaredPrompts (JR-Q6): a script's spec.prompts serializes to
// scripts.prompts_json on sync via the SAME MarshalPrompts the jobs path uses (so the
// two can't drift), nameless rows are dropped, and a script declaring none defaults to
// '[]' rather than NULL.
func TestUpsertScripts_DeclaredPrompts(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{db: pool, cloneDir: t.TempDir()}
	ctx := context.Background()

	withPrompts := ScriptYAML{}
	withPrompts.APIVersion = requiredAPIVersion
	withPrompts.Kind = "Script"
	withPrompts.Metadata.Name = "deploy-app"
	withPrompts.Spec.RunType = "bash"
	withPrompts.Spec.Command = "echo deploying"
	withPrompts.Spec.Prompts = []PromptSpec{
		{Name: "TARGET_ENV", Label: "Deployment environment", Required: true, Options: []string{"dev", "prod"}},
		{Name: "", Label: "dropped — empty name"}, // shape guard, mirrors the jobs path
	}

	plain := ScriptYAML{}
	plain.APIVersion = requiredAPIVersion
	plain.Kind = "Script"
	plain.Metadata.Name = "plain-script"
	plain.Spec.RunType = "bash"
	plain.Spec.Command = "echo hi"

	resolved, rErrs := svc.resolveScripts([]ScriptYAML{withPrompts, plain})
	if len(rErrs) != 0 {
		t.Fatalf("resolveScripts errs: %v", rErrs)
	}
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	nowStr := time.Now().UTC().Format(time.RFC3339)
	if err := svc.upsertScripts(ctx, tx, resolved, nowStr, "sha1"); err != nil {
		t.Fatalf("upsertScripts: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var raw string
	if err := pool.QueryRow(`SELECT prompts_json FROM scripts WHERE name='deploy-app'`).Scan(&raw); err != nil {
		t.Fatalf("query deploy-app: %v", err)
	}
	var got []PromptSpec
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal prompts_json %q: %v", raw, err)
	}
	if len(got) != 1 {
		t.Fatalf("prompts_json = %q, want exactly 1 entry (nameless dropped)", raw)
	}
	if got[0].Name != "TARGET_ENV" || !got[0].Required || len(got[0].Options) != 2 {
		t.Errorf("TARGET_ENV not round-tripped: %+v", got[0])
	}

	var plainRaw string
	if err := pool.QueryRow(`SELECT prompts_json FROM scripts WHERE name='plain-script'`).Scan(&plainRaw); err != nil {
		t.Fatalf("query plain-script: %v", err)
	}
	if plainRaw != "[]" {
		t.Errorf("script declaring no prompts = %q, want %q", plainRaw, "[]")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Denormalization: a job's script_ref pulls the script's fields onto the cache row
// ──────────────────────────────────────────────────────────────────────────────

func TestSync_ScriptDenormalization(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{db: pool, cloneDir: t.TempDir()}
	ctx := context.Background()

	// One script (inline) + one job referencing it.
	scripts := []ScriptYAML{{}}
	scripts[0].APIVersion = requiredAPIVersion
	scripts[0].Kind = "Script"
	scripts[0].Metadata.Name = "backup-db"
	scripts[0].Spec.RunType = "ansible"
	scripts[0].Spec.Script = "- hosts: all\n"
	scripts[0].Spec.Executor = "runner"

	resolved, rErrs := svc.resolveScripts(scripts)
	if len(rErrs) != 0 {
		t.Fatalf("resolveScripts errs: %v", rErrs)
	}
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	nowStr := time.Now().UTC().Format(time.RFC3339)

	if err := svc.upsertScripts(ctx, tx, resolved, nowStr, "sha1"); err != nil {
		t.Fatalf("upsertScripts: %v", err)
	}

	j := JobYAML{}
	j.APIVersion = requiredAPIVersion
	j.Kind = "Job"
	j.Metadata.Name = "nightly-backup"
	j.Spec.ScriptRef = "backup-db"
	j.Spec.Scope = "Prod"
	if err := svc.upsertJobs(ctx, tx, []JobYAML{j}, resolved, nil, nowStr, "sha1"); err != nil {
		t.Fatalf("upsertJobs: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The job row must carry the script's run_type/body/executor + script_ref +
	// the matching content_hash (denormalization, Decision 7).
	var runType, script, executor, scriptRef, jobHash, scriptHash string
	if err := pool.QueryRow(`SELECT run_type, script, executor, script_ref, content_hash FROM jobs WHERE name='nightly-backup'`).
		Scan(&runType, &script, &executor, &scriptRef, &jobHash); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if err := pool.QueryRow(`SELECT content_hash FROM scripts WHERE name='backup-db'`).Scan(&scriptHash); err != nil {
		t.Fatalf("query script: %v", err)
	}
	if runType != "ansible" {
		t.Errorf("run_type not denormalized: got %q", runType)
	}
	if script != "- hosts: all\n" {
		t.Errorf("script body not denormalized: got %q", script)
	}
	if executor != "runner" {
		t.Errorf("executor not denormalized: got %q", executor)
	}
	if scriptRef != "backup-db" {
		t.Errorf("script_ref not set: got %q", scriptRef)
	}
	if jobHash == "" || jobHash != scriptHash {
		t.Errorf("content_hash mismatch: job=%q script=%q", jobHash, scriptHash)
	}
}

// TestSync_InlineJobHashed confirms a legacy inline job still gets a content_hash
// denormalized onto its row (so the enqueue snapshot is uniform).
func TestSync_InlineJobHashed(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{db: pool, cloneDir: t.TempDir()}

	j := JobYAML{}
	j.APIVersion = requiredAPIVersion
	j.Kind = "Job"
	j.Metadata.Name = "legacy-inline"
	j.Spec.RunType = "bash"
	j.Spec.Command = "echo hi"
	tx2, err := pool.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx2.Rollback()
	nowStr2 := time.Now().UTC().Format(time.RFC3339)
	if err := svc.upsertJobs(context.Background(), tx2, []JobYAML{j}, nil, nil, nowStr2, "sha1"); err != nil {
		t.Fatalf("upsertJobs: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var hash string
	var ref *string
	if err := pool.QueryRow(`SELECT content_hash, script_ref FROM jobs WHERE name='legacy-inline'`).Scan(&hash, &ref); err != nil {
		t.Fatalf("query: %v", err)
	}
	if hash != ContentHash([]byte("echo hi")) {
		t.Errorf("inline content_hash = %q, want hash of body", hash)
	}
	if ref != nil {
		t.Errorf("inline job should have NULL script_ref, got %v", *ref)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestSync_GitOpsPruning(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{db: pool, cloneDir: t.TempDir()}
	ctx := context.Background()

	// 1. Setup initial state in DB: a job, script, workflow, and scope.
	// We'll set their synced_at to an older timestamp.
	oldTime := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	// Create scopes and scope_hosts tables for this test since mustOpenDB doesn't define them.
	_, err := pool.Exec(`
		CREATE TABLE IF NOT EXISTS scopes (
			id TEXT PRIMARY KEY,
			name TEXT UNIQUE,
			source TEXT,
			source_path TEXT,
			capability_types TEXT,
			capability_json TEXT,
			synced_at TEXT,
			description TEXT,
			created_by TEXT,
			created_at TEXT,
			supported_types TEXT,
			raw_inventory TEXT,
			inventory_format TEXT
		);
		CREATE TABLE IF NOT EXISTS scope_hosts (
			scope_id TEXT,
			host TEXT,
			PRIMARY KEY (scope_id, host)
		);
	`)
	if err != nil {
		t.Fatalf("create scopes schema: %v", err)
	}

	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	// Insert old script
	_, err = tx.ExecContext(ctx, `
		INSERT INTO scripts(name, description, run_type, command, source_path, content_hash, synced_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		"old-script", "desc", "bash", "echo 1", "scripts/old-script.yaml", "hash1", oldTime)
	if err != nil {
		t.Fatalf("insert script: %v", err)
	}

	// Insert old job
	_, err = tx.ExecContext(ctx, `
		INSERT INTO jobs(name, run_type, description, scope, target_host, command, source_path, synced_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		"old-job", "bash", "desc", "Prod", "localhost", "echo 1", "jobs/old-job.yaml", oldTime)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO definition_schedules(owner_kind, owner_name, name, cron, env, position)
		VALUES(?, ?, ?, ?, ?, ?)`,
		"job", "old-job", "default", "*/5 * * * *", nil, 0)
	if err != nil {
		t.Fatalf("insert definition schedule: %v", err)
	}

	// Insert old workflow
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflows(name, description, steps, source_path, synced_at)
		VALUES(?, ?, ?, ?, ?)`,
		"old-workflow", "desc", "[]", "workflows/old-workflow.yaml", oldTime)
	if err != nil {
		t.Fatalf("insert workflow: %v", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO definition_schedules(owner_kind, owner_name, name, cron, env, position)
		VALUES(?, ?, ?, ?, ?, ?)`,
		"workflow", "old-workflow", "default", "*/10 * * * *", nil, 0)
	if err != nil {
		t.Fatalf("insert definition schedule: %v", err)
	}

	// Insert old scope
	_, err = tx.ExecContext(ctx, `
		INSERT INTO scopes(id, name, source, source_path, capability_types, capability_json, synced_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		"old-scope-id", "old-scope", "git", "scopes/old-scope.yaml", "[]", "{}", oldTime)
	if err != nil {
		t.Fatalf("insert scope: %v", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO scope_hosts(scope_id, host)
		VALUES(?, ?)`,
		"old-scope-id", "localhost")
	if err != nil {
		t.Fatalf("insert scope host: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit initial: %v", err)
	}

	// 2. Perform a sync containing only NEW elements.
	// This will write the new elements with a current timestamp,
	// and prune the old elements (whose synced_at < newTime).
	newTimeStr := time.Now().UTC().Format(time.RFC3339)
	tx2, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer tx2.Rollback()

	// New resolved script
	resolved := map[string]resolvedScript{
		"new-script": {
			runType:     "bash",
			command:     "echo new",
			sourcePath:  "scripts/new-script.yaml",
			contentHash: "hash2",
		},
	}
	if err := svc.upsertScripts(ctx, tx2, resolved, newTimeStr, "sha2"); err != nil {
		t.Fatalf("upsertScripts: %v", err)
	}

	// New job
	newJob := JobYAML{}
	newJob.APIVersion = requiredAPIVersion
	newJob.Kind = "Job"
	newJob.Metadata.Name = "new-job"
	newJob.Spec.RunType = "bash"
	newJob.Spec.Command = "echo new"
	newJob.Spec.Scope = "Prod"
	if err := svc.upsertJobs(ctx, tx2, []JobYAML{newJob}, resolved, nil, newTimeStr, "sha2"); err != nil {
		t.Fatalf("upsertJobs: %v", err)
	}

	// New workflow
	newWf := WorkflowYAML{}
	newWf.APIVersion = requiredAPIVersion
	newWf.Kind = "Workflow"
	newWf.Metadata.Name = "new-workflow"
	newWf.Spec.Steps = []any{}
	if err := svc.upsertWorkflows(ctx, tx2, []WorkflowYAML{newWf}, nil, newTimeStr, "sha2"); err != nil {
		t.Fatalf("upsertWorkflows: %v", err)
	}

	// New scope
	newScope := inventoryScope{
		Name:       "new-scope",
		SourcePath: "scopes/new-scope.yaml",
		Hosts:      []string{"host1"},
	}
	if err := svc.upsertScopes(ctx, tx2, []inventoryScope{newScope}, newTimeStr, "sha2"); err != nil {
		t.Fatalf("upsertScopes: %v", err)
	}

	// Run pruning logic inside tx2
	// Prune jobs and definition schedules
	if _, err := tx2.ExecContext(ctx, `
		DELETE FROM definition_schedules 
		WHERE owner_kind = 'job' AND owner_name IN (
			SELECT name FROM jobs WHERE synced_at < ? AND source_path LIKE 'jobs/%'
		)`, newTimeStr); err != nil {
		t.Fatalf("prune job schedules: %v", err)
	}
	if _, err := tx2.ExecContext(ctx, `
		DELETE FROM jobs 
		WHERE synced_at < ? AND source_path LIKE 'jobs/%'`, newTimeStr); err != nil {
		t.Fatalf("prune jobs: %v", err)
	}

	// Prune workflows and definition schedules
	if _, err := tx2.ExecContext(ctx, `
		DELETE FROM definition_schedules 
		WHERE owner_kind = 'workflow' AND owner_name IN (
			SELECT name FROM workflows WHERE synced_at < ? AND source_path LIKE 'workflows/%'
		)`, newTimeStr); err != nil {
		t.Fatalf("prune workflow schedules: %v", err)
	}
	if _, err := tx2.ExecContext(ctx, `
		DELETE FROM workflows 
		WHERE synced_at < ? AND source_path LIKE 'workflows/%'`, newTimeStr); err != nil {
		t.Fatalf("prune workflows: %v", err)
	}

	// Prune scripts
	if _, err := tx2.ExecContext(ctx, `
		DELETE FROM scripts 
		WHERE synced_at < ? AND source_path LIKE 'scripts/%'`, newTimeStr); err != nil {
		t.Fatalf("prune scripts: %v", err)
	}

	// Prune scopes and scope hosts
	if _, err := tx2.ExecContext(ctx, `
		DELETE FROM scope_hosts 
		WHERE scope_id IN (
			SELECT id FROM scopes WHERE synced_at < ? AND source = 'git'
		)`, newTimeStr); err != nil {
		t.Fatalf("prune scope hosts: %v", err)
	}
	if _, err := tx2.ExecContext(ctx, `
		DELETE FROM scopes 
		WHERE synced_at < ? AND source = 'git'`, newTimeStr); err != nil {
		t.Fatalf("prune scopes: %v", err)
	}

	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit tx2: %v", err)
	}

	// 3. Verify that old items are deleted and new ones exist.
	var count int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM scripts WHERE name = 'old-script'`).Scan(&count)
	if count != 0 {
		t.Errorf("old-script not pruned")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM scripts WHERE name = 'new-script'`).Scan(&count)
	if count != 1 {
		t.Errorf("new-script missing")
	}

	_ = pool.QueryRow(`SELECT COUNT(*) FROM jobs WHERE name = 'old-job'`).Scan(&count)
	if count != 0 {
		t.Errorf("old-job not pruned")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM definition_schedules WHERE owner_kind = 'job' AND owner_name = 'old-job'`).Scan(&count)
	if count != 0 {
		t.Errorf("old-job schedule not pruned")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM jobs WHERE name = 'new-job'`).Scan(&count)
	if count != 1 {
		t.Errorf("new-job missing")
	}

	_ = pool.QueryRow(`SELECT COUNT(*) FROM workflows WHERE name = 'old-workflow'`).Scan(&count)
	if count != 0 {
		t.Errorf("old-workflow not pruned")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM definition_schedules WHERE owner_kind = 'workflow' AND owner_name = 'old-workflow'`).Scan(&count)
	if count != 0 {
		t.Errorf("old-workflow schedule not pruned")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM workflows WHERE name = 'new-workflow'`).Scan(&count)
	if count != 1 {
		t.Errorf("new-workflow missing")
	}

	_ = pool.QueryRow(`SELECT COUNT(*) FROM scopes WHERE name = 'old-scope'`).Scan(&count)
	if count != 0 {
		t.Errorf("old-scope not pruned")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM scope_hosts WHERE scope_id = 'old-scope-id'`).Scan(&count)
	if count != 0 {
		t.Errorf("old-scope-id host not pruned")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM scopes WHERE name = 'new-scope'`).Scan(&count)
	if count != 1 {
		t.Errorf("new-scope missing")
	}
}

func TestDiscoverScripts(t *testing.T) {
	// Create a temp directory for scripts
	dir, err := os.MkdirTemp("", "amadeus-scripts-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	// Create a standard wrapper
	wrapperYAML := `apiVersion: cronomicon.io/v1
kind: Script
metadata:
  name: standard-wrapper
spec:
  description: "A standard wrapper script"
  run_type: bash
  scriptPath: scripts/some-other-file.sh
`
	if err := os.WriteFile(filepath.Join(dir, "wrapper.yaml"), []byte(wrapperYAML), 0644); err != nil {
		t.Fatalf("failed to write wrapper.yaml: %v", err)
	}

	// Create some raw script files
	if err := os.WriteFile(filepath.Join(dir, "backup.sh"), []byte("#!/bin/bash\necho hi"), 0755); err != nil {
		t.Fatalf("failed to write backup.sh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy.tf"), []byte("resource \"null_resource\" \"x\" {}"), 0644); err != nil {
		t.Fatalf("failed to write deploy.tf: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metrics.py"), []byte("#!/usr/bin/env python3\nimport os\nprint(os.environ['HOME'])\n"), 0755); err != nil {
		t.Fatalf("failed to write metrics.py: %v", err)
	}
	// Create an Ansible playbook (not an Cronomicon Script wrapper)
	playbookYAML := `- name: A playbook
  hosts: all
  tasks:
    - ping:
`
	if err := os.WriteFile(filepath.Join(dir, "playbook.yml"), []byte(playbookYAML), 0644); err != nil {
		t.Fatalf("failed to write playbook.yml: %v", err)
	}
	// Create a subfolder with a nested script
	subDir := filepath.Join(dir, "cleanup")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatalf("failed to create subDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "log-rotate.sh"), []byte("#!/bin/bash\necho cleaning"), 0755); err != nil {
		t.Fatalf("failed to write log-rotate.sh: %v", err)
	}

	// Create an unsupported format file (e.g. readme)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("Documentation"), 0644); err != nil {
		t.Fatalf("failed to write README.md: %v", err)
	}

	// Run discoverScripts
	scripts, errs := discoverScripts(dir)
	if len(errs) != 0 {
		t.Fatalf("unexpected discoverScripts errors: %v", errs)
	}

	// We expect exactly 6 scripts: wrapper.yaml, backup.sh, deploy.tf, metrics.py,
	// playbook.yml, and cleanup/log-rotate.sh.
	// README.md is unsupported and should be skipped.
	if len(scripts) != 6 {
		t.Errorf("expected 6 scripts, got %d: %+v", len(scripts), scripts)
	}

	expected := map[string]struct {
		runType    string
		scriptPath string
		sourcePath string
	}{
		"standard-wrapper":      {runType: "bash", scriptPath: "scripts/some-other-file.sh", sourcePath: "scripts/wrapper.yaml"},
		"backup.sh":             {runType: "bash", scriptPath: "scripts/backup.sh", sourcePath: "scripts/backup.sh"},
		"deploy.tf":             {runType: "terraform", scriptPath: "scripts/deploy.tf", sourcePath: "scripts/deploy.tf"},
		"metrics.py":            {runType: "python", scriptPath: "scripts/metrics.py", sourcePath: "scripts/metrics.py"},
		"playbook.yml":          {runType: "ansible", scriptPath: "scripts/playbook.yml", sourcePath: "scripts/playbook.yml"},
		"cleanup/log-rotate.sh": {runType: "bash", scriptPath: "scripts/cleanup/log-rotate.sh", sourcePath: "scripts/cleanup/log-rotate.sh"},
	}

	for _, sc := range scripts {
		name := sc.Metadata.Name
		exp, ok := expected[name]
		if !ok {
			t.Errorf("unexpected script discovered: %q", name)
			continue
		}
		if sc.Spec.RunType != exp.runType {
			t.Errorf("script %q: expected runType %q, got %q", name, exp.runType, sc.Spec.RunType)
		}
		cleanScriptPath := filepath.ToSlash(sc.Spec.ScriptPath)
		cleanSourcePath := filepath.ToSlash(sc.SourcePath)
		if cleanScriptPath != exp.scriptPath {
			t.Errorf("script %q: expected scriptPath %q, got %q", name, exp.scriptPath, cleanScriptPath)
		}
		if cleanSourcePath != exp.sourcePath {
			t.Errorf("script %q: expected sourcePath %q, got %q", name, exp.sourcePath, cleanSourcePath)
		}
	}
}

// TestDiscoverScriptsProjectGrouping covers the §7 two-pass grouping (review R1).
// The critical case: a project's member files are lexically BEFORE the wrapper
// that claims them (dir "a-project/" vs wrapper "z-wrapper.yaml"), so a naive
// single-pass WalkDir would auto-synthesize the members before ever parsing the
// claim. Two-pass discovery must still collapse the tree into exactly one script.
func TestDiscoverScriptsProjectGrouping(t *testing.T) {
	dir := t.TempDir()

	// Project member tree under a-project/ (lexically first).
	mustWrite := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a-project/site.yml", "- hosts: all\n  roles: [web]\n")
	mustWrite("a-project/roles/web/tasks/main.yml", "- ping:\n")
	mustWrite("a-project/templates/nginx.conf.j2", "server { listen {{ port }}; }\n")

	// The wrapper claiming the project — lexically AFTER its member files.
	mustWrite("z-wrapper.yaml", `apiVersion: cronomicon.io/v1
kind: Script
metadata:
  name: vmware-patch
spec:
  description: "A checkout project"
  run_type: ansible
  project_root: scripts/a-project
  entry: scripts/a-project/site.yml
`)
	// A loose playbook OUTSIDE any project still auto-synthesizes (regression).
	mustWrite("loose.yml", "- hosts: all\n  tasks: []\n")

	scripts, errs := discoverScripts(dir)
	if len(errs) != 0 {
		t.Fatalf("unexpected discoverScripts errors: %v", errs)
	}

	byName := map[string]ScriptYAML{}
	for _, sc := range scripts {
		byName[sc.Metadata.Name] = sc
	}
	// Exactly two catalog entries: the ONE project + the loose playbook.
	if len(scripts) != 2 {
		t.Fatalf("expected 2 scripts (one project + loose), got %d: %v", len(scripts), byName)
	}
	proj, ok := byName["vmware-patch"]
	if !ok {
		t.Fatalf("project wrapper did not produce a script; got %v", byName)
	}
	if got := filepath.ToSlash(proj.Spec.ScriptPath); got != "scripts/a-project/site.yml" {
		t.Errorf("project script_path = %q, want the entry scripts/a-project/site.yml", got)
	}
	if proj.Spec.ProjectRoot != "scripts/a-project" {
		t.Errorf("project_root = %q, want scripts/a-project", proj.Spec.ProjectRoot)
	}
	// Member files must NOT have been synthesized into standalone scripts.
	for name := range byName {
		if strings.Contains(name, "a-project/") {
			t.Errorf("member file leaked as a standalone script: %q", name)
		}
	}
	if _, ok := byName["loose.yml"]; !ok {
		t.Errorf("loose playbook outside any project should still synthesize; got %v", byName)
	}
}

// TestDiscoverScriptsProjectErrors covers §7 validation surfaced at discovery:
// an escaping project_root, a missing entry, and an entry outside the root.
func TestDiscoverScriptsProjectErrors(t *testing.T) {
	cases := []struct {
		name    string
		wrapper string
		wantMsg string
	}{
		{"root escapes repo", `apiVersion: cronomicon.io/v1
kind: Script
metadata: {name: bad}
spec: {run_type: ansible, project_root: ../evil, entry: scripts/x/site.yml}
`, "escape"},
		{"missing entry", `apiVersion: cronomicon.io/v1
kind: Script
metadata: {name: bad}
spec: {run_type: ansible, project_root: scripts/proj}
`, "must declare spec.entry"},
		{"entry outside root", `apiVersion: cronomicon.io/v1
kind: Script
metadata: {name: bad}
spec: {run_type: ansible, project_root: scripts/proj, entry: scripts/other/site.yml}
`, "must live under project_root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "w.yaml"), []byte(tc.wrapper), 0o644); err != nil {
				t.Fatal(err)
			}
			_, errs := discoverScripts(dir)
			joined := ""
			for _, e := range errs {
				joined += e.Error() + "\n"
			}
			if !strings.Contains(joined, tc.wantMsg) {
				t.Errorf("expected an error containing %q, got: %s", tc.wantMsg, joined)
			}
		})
	}
}

// TestLintProject covers Phase 3 (RX.5/§6.2): requirements.yml pinning lint and
// the tree secret-scan, both surfaced by `cronomicon validate`.
func TestLintProject(t *testing.T) {
	dir := t.TempDir()
	mk := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// An unpinned collection + a plaintext-secret vars file + a vault-encrypted
	// sibling (must be exempt).
	mk("scripts/proj/requirements.yml", "collections:\n  - name: community.vmware\n    version: \">=3.0\"\n")
	mk("scripts/proj/vars/main.yml", "ansible_become_pass: hunter2\n")
	mk("scripts/proj/vars/vault.yml", "$ANSIBLE_VAULT;1.1;AES256\n66386439...\n")
	mk("scripts/proj/vars/ok.yml", "ansible_become_pass: \"{{ lookup('env','BECOME') }}\"\n")

	findings := LintProject(dir, "scripts/proj")
	var reqHit, secretHit bool
	for _, f := range findings {
		if strings.Contains(f.Message, "not an exact pin") {
			reqHit = true
		}
		if strings.Contains(f.File, "vars/main.yml") && strings.Contains(f.Message, "ansible_become_pass") {
			secretHit = true
		}
		if strings.Contains(f.File, "vault.yml") {
			t.Errorf("vault-encrypted file must be exempt from the secret scan: %v", f)
		}
		if strings.Contains(f.File, "ok.yml") {
			t.Errorf("env-lookup indirection must not be flagged: %v", f)
		}
	}
	if !reqHit {
		t.Errorf("expected an unpinned-collection finding; got %v", findings)
	}
	if !secretHit {
		t.Errorf("expected a plaintext-secret finding for vars/main.yml; got %v", findings)
	}
}

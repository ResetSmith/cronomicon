package gitlab

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestUpsertJobsAnsibleMetacharTargetHostWarnsNotBlocks is the TG-4 advisory
// check inside upsertJobs (sync.go, search "TG-4"): an ansible job's
// target_host is passed verbatim to `ansible --limit`, which silently DROPS any
// name carrying a pattern metacharacter — a dropped pin means no --limit at
// all, widening the run to the full inventory. Runs of such a job are refused
// at the manifest boundary (409, HandleGetManifest), which is what makes this
// sync-time check advisory rather than blocking: a git sync must never wedge a
// whole repo over one bad field, so the row is still written and the operator
// is warned, not stopped.
//
// The job here reaches its effective run_type through a script_ref (Decision
// 7) rather than an inline run_type, mirroring how upsertJobs actually
// resolves runType for the metachar check — the check must fire off the
// RESOLVED type, not the (absent) inline one.
func TestUpsertJobsAnsibleMetacharTargetHostWarnsNotBlocks(t *testing.T) {
	pool := mustOpenDB(t)
	ctx := context.Background()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := &Service{db: pool, log: logger, cloneDir: t.TempDir()}

	resolved := map[string]resolvedScript{
		"playbook": {
			runType:     "ansible",
			command:     "- hosts: web\n",
			contentHash: "sha256:aaa",
			sourcePath:  "scripts/playbook.yaml",
		},
	}
	j := JobYAML{}
	j.APIVersion = requiredAPIVersion
	j.Kind = "Job"
	j.Metadata.Name = "pinjob"
	j.Spec.ScriptRef = "playbook"
	j.Spec.Scope = "Prod"
	j.Spec.TargetHost = "web[01:50]"

	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := svc.upsertJobs(ctx, tx, []JobYAML{j}, resolved, nil, "t1", "sha1"); err != nil {
		t.Fatalf("upsertJobs must not fail sync over a metachar target_host: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Non-blocking: the job row is written with its target_host (and the
	// script_ref-resolved run_type) intact — sync never wedges over this.
	var runType, targetHost string
	if err := pool.QueryRowContext(ctx,
		`SELECT run_type, target_host FROM jobs WHERE source='git' AND name='pinjob'`,
	).Scan(&runType, &targetHost); err != nil {
		t.Fatalf("fetch job: %v", err)
	}
	if runType != "ansible" {
		t.Errorf("run_type = %q, want ansible (resolved from script_ref)", runType)
	}
	if targetHost != "web[01:50]" {
		t.Errorf("target_host = %q, want the metachar pin preserved verbatim", targetHost)
	}

	// Advisory: a warn-level log names the offending job and host.
	logs := buf.String()
	if !strings.Contains(logs, "level=WARN") {
		t.Fatalf("expected a warn-level log for the metachar target_host; logs:\n%s", logs)
	}
	if !strings.Contains(logs, "pinjob") || !strings.Contains(logs, "web[01:50]") {
		t.Errorf("expected the warning to name the job and target_host; logs:\n%s", logs)
	}
}

// TestUpsertJobsPlainTargetHostNoWarning is the negative case: a plain (non-
// metachar) ansible target_host must not trip the TG-4 advisory warning.
func TestUpsertJobsPlainTargetHostNoWarning(t *testing.T) {
	pool := mustOpenDB(t)
	ctx := context.Background()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := &Service{db: pool, log: logger, cloneDir: t.TempDir()}

	resolved := map[string]resolvedScript{
		"playbook": {
			runType:     "ansible",
			command:     "- hosts: web\n",
			contentHash: "sha256:aaa",
			sourcePath:  "scripts/playbook.yaml",
		},
	}
	j := JobYAML{}
	j.APIVersion = requiredAPIVersion
	j.Kind = "Job"
	j.Metadata.Name = "cleanpin"
	j.Spec.ScriptRef = "playbook"
	j.Spec.Scope = "Prod"
	j.Spec.TargetHost = "web1"

	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := svc.upsertJobs(ctx, tx, []JobYAML{j}, resolved, nil, "t1", "sha1"); err != nil {
		t.Fatalf("upsertJobs: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var targetHost string
	if err := pool.QueryRowContext(ctx,
		`SELECT target_host FROM jobs WHERE source='git' AND name='cleanpin'`,
	).Scan(&targetHost); err != nil {
		t.Fatalf("fetch job: %v", err)
	}
	if targetHost != "web1" {
		t.Errorf("target_host = %q, want web1", targetHost)
	}
	if logs := buf.String(); logs != "" {
		t.Errorf("expected no warning for a plain target_host, got logs:\n%s", logs)
	}
}

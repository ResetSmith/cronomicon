package gitlab

import (
	"context"
	"database/sql"
	"testing"
)

// RT-2 — jobs.runner_tag is the DECLARED pin, and Git owns it: sync must WRITE
// it and must OVERWRITE it. Omitting it from the upsert would leave
// spec.runner_tag parsing fine and doing nothing — a YAML key that silently
// no-ops, which is a quiet enough failure to be worth a guard.
//
// This file used to guard a second, opposite rule as well: jobs.runner_tag_override
// was operator-owned and sync had to never touch it. That column was retired in
// v1.3.5 (migration 1090) and the pin now has exactly one owner per layer.

func syncOneJob(t *testing.T, svc *Service, pool *sql.DB, declared, desc, now string) {
	t.Helper()
	ctx := context.Background()
	j := JobYAML{}
	j.Metadata.Name = "backup"
	j.Spec.RunType = "bash"
	j.Spec.Scope = "Prod"
	j.Spec.Description = desc
	j.Spec.RunnerTag = declared
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := svc.upsertJobs(ctx, tx, []JobYAML{j}, nil, nil, now, "sha"); err != nil {
		t.Fatalf("upsertJobs: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func readPin(t *testing.T, pool *sql.DB) (declared sql.NullString) {
	t.Helper()
	if err := pool.QueryRow(
		`SELECT runner_tag FROM jobs WHERE source='git' AND name='backup'`).
		Scan(&declared); err != nil {
		t.Fatalf("read pin: %v", err)
	}
	return declared
}

// TestRunnerTagSyncsFromYAML — the git-owned direction. The declared pin lands
// on first sync and is overwritten on re-sync, exactly like description.
func TestRunnerTagSyncsFromYAML(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{cloneDir: t.TempDir()}

	syncOneJob(t, svc, pool, "vlan-dmz", "v1", "t1")
	declared := readPin(t, pool)
	if declared.String != "vlan-dmz" {
		t.Fatalf("declared pin after first sync = %q, want vlan-dmz — spec.runner_tag is not reaching the column", declared.String)
	}

	// Git changes its mind: the new value wins. This is the property that
	// distinguishes the declared pin from tags/annotations.
	syncOneJob(t, svc, pool, "vlan-core", "v2", "t2")
	declared = readPin(t, pool)
	if declared.String != "vlan-core" {
		t.Fatalf("declared pin after re-sync = %q, want vlan-core", declared.String)
	}

	// Removing the key from YAML unpins the job. A stale pin surviving its own
	// deletion from the repository would be the worst of both models.
	syncOneJob(t, svc, pool, "", "v3", "t3")
	declared = readPin(t, pool)
	if declared.Valid && declared.String != "" {
		t.Fatalf("declared pin after removal from YAML = %q, want empty", declared.String)
	}
}

// TestSyncRunnerTagDegradesOnBadValue — an unusable tag must leave the job
// unpinned rather than fail the sync for the whole repository, matching the
// must_finish_by / become_password_secret precedent.
func TestSyncRunnerTagDegradesOnBadValue(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{cloneDir: t.TempDir()}

	syncOneJob(t, svc, pool, "   ", "v1", "t1")
	declared := readPin(t, pool)
	if declared.Valid && declared.String != "" {
		t.Fatalf("whitespace-only runner_tag became %q; want unpinned", declared.String)
	}

	syncOneJob(t, svc, pool, "  vlan-dmz  ", "v2", "t2")
	declared = readPin(t, pool)
	if declared.String != "vlan-dmz" {
		t.Fatalf("runner_tag = %q, want it trimmed to vlan-dmz", declared.String)
	}
}

// TestNormalizeRunnerTagMatchesTagutilCaps guards the duplicated length cap named
// in NormalizeRunnerTag's comment: gitlab cannot import tagutil (leaf-package
// rule), so the 64 here and tagutil.MaxLen there can drift silently. A pin one
// character over the cap the tag EDITOR enforces would be unenterable in the UI
// and therefore unmatchable forever.
func TestNormalizeRunnerTagMatchesTagutilCaps(t *testing.T) {
	at := make([]byte, runnerTagMaxLen)
	over := make([]byte, runnerTagMaxLen+1)
	for i := range at {
		at[i] = 'a'
	}
	for i := range over {
		over[i] = 'a'
	}
	if got := NormalizeRunnerTag(string(at)); got == "" {
		t.Errorf("a tag exactly at the cap (%d) was rejected", runnerTagMaxLen)
	}
	if got := NormalizeRunnerTag(string(over)); got != "" {
		t.Errorf("a tag one over the cap was accepted as %q", got)
	}
	if got := NormalizeRunnerTag("bad\x01tag"); got != "" {
		t.Errorf("a control character was accepted as %q", got)
	}
	// Case is preserved: matching is case-insensitive at the storage layer
	// (runner_tags.tag is COLLATE NOCASE, RT-G9), so folding here would only
	// misrepresent what the operator typed.
	if got := NormalizeRunnerTag("VLAN-DMZ"); got != "VLAN-DMZ" {
		t.Errorf("case was folded: got %q, want VLAN-DMZ", got)
	}
}

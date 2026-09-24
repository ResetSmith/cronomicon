package workflow

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestMarkSkipped_CarriesTargetHost verifies TG-2's display-only pin on the
// branch-skip path: markSkipped's INSERT writes jd.targetHost onto the skipped
// child run row, and a job with no pin leaves it NULL. The row never executes,
// so this only affects what the skipped run displays.
func TestMarkSkipped_CarriesTargetHost(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "markskip.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	e := New(pool, discardLog())

	// runs.workflow_run_id FKs to workflow_runs(id); seed the parent row so the
	// INSERT below doesn't fail the foreign-key check.
	if _, err := pool.Exec(`
		INSERT INTO workflow_runs (id, workflow_name, status, triggered_by, trigger_kind, created_at)
		VALUES ('wf-trace', 'wf', 'running', 'tester@example.com', 'manual', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed workflow_runs: %v", err)
	}

	// Keyed as collectStepRefs keys them (R2F-2): name-only steps file under
	// "name:<name>".
	jobDefs := map[string]jobDef{
		"name:pinned-skip": {runType: "bash", targetHost: "skip-host"},
		"name:plain-skip":  {runType: "bash"},
	}

	e.markSkipped(context.Background(), "wf-trace", "wf", Step{Name: "pinned-skip", NodeID: "node-1"}, "tester@example.com", jobDefs)
	e.markSkipped(context.Background(), "wf-trace", "wf", Step{Name: "plain-skip", NodeID: "node-2"}, "tester@example.com", jobDefs)

	var targetHost sql.NullString
	if err := pool.QueryRow(`SELECT target_host FROM runs WHERE id = 'node-1'`).Scan(&targetHost); err != nil {
		t.Fatalf("fetch pinned skip row: %v", err)
	}
	if targetHost.String != "skip-host" {
		t.Errorf("target_host = %q, want skip-host", targetHost.String)
	}

	if err := pool.QueryRow(`SELECT target_host FROM runs WHERE id = 'node-2'`).Scan(&targetHost); err != nil {
		t.Fatalf("fetch plain skip row: %v", err)
	}
	if targetHost.Valid {
		t.Errorf("target_host = %v, want NULL for an unpinned job", targetHost)
	}
}

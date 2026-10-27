package runner

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// 2.5.2 GR — the built-in agency may be renamed. Runs carry agencies by NAME,
// so every writer of its name reads it by id: a run waiting across the rename
// is carried to the new name, a run enqueued after it is stamped with the new
// name (by the producer, and by the birth trigger for a run with no snapshot),
// and the built-in agency's runner claims both. A finished run keeps the name
// it ran under.
func TestGR_ARenamedBuiltInAgencyStillClaimsItsRuns(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	insertRunner(t, svc, "r-central", "central", "online", []string{"bash"}) // born serving the built-in agency

	insertQueuedRunAgency(t, svc, "run-across", "bash", "Global") // waiting across the rename
	insertQueuedRunAgency(t, svc, "run-done", "bash", "Global")
	if _, err := svc.db.Exec(`UPDATE runs SET status = 'success' WHERE id = 'run-done'`); err != nil {
		t.Fatal(err)
	}

	if _, err := settings.UpdateAgency(ctx, svc.db, "global", settings.AgencyInput{Name: "Central"}, "root@example.com"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	snapshot := func(run string) (aj, idx string) {
		t.Helper()
		_ = svc.db.QueryRow(`SELECT agencies_json FROM runs WHERE id = ?`, run).Scan(&aj)
		_ = svc.db.QueryRow(`SELECT COALESCE(group_concat(agency), '') FROM run_agencies WHERE run_id = ?`, run).Scan(&idx)
		return aj, idx
	}
	if aj, idx := snapshot("run-across"); aj != `["Central"]` || idx != "Central" {
		t.Errorf("the run waiting across the rename: agencies_json=%s run_agencies=%q, want Central", aj, idx)
	}
	if aj, _ := snapshot("run-done"); aj != `["Global"]` {
		t.Errorf("a finished run was rewritten to %s; history keeps the name it ran under", aj)
	}

	// A run enqueued after the rename with no snapshot: the birth trigger
	// stamps the name now.
	if _, err := svc.db.Exec(`INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, agencies_json, created_at)
		VALUES ('run-born', 'j', 'bash', '', 'queued', 'test', 'manual', 'runner', '[]', ?)`, now()); err != nil {
		t.Fatal(err)
	}
	if aj, idx := snapshot("run-born"); aj != `["Central"]` || idx != "Central" {
		t.Errorf("the birth trigger stamped agencies_json=%s run_agencies=%q, want Central", aj, idx)
	}
	// And the producers' own stamp for a run with no scope.
	got, err := execspec.ScopeAgencies(ctx, svc.db, "global", "")
	if err != nil || len(got) != 1 || got[0] != "Central" {
		t.Errorf("a run with no scope is stamped %v (%v), want [Central]", got, err)
	}
	insertQueuedRunAgency(t, svc, "run-after", "bash", got[0])

	// The built-in agency's runner claims every one of them.
	claimed := map[string]bool{}
	for i := 0; i < 4; i++ {
		a, err := svc.claimRun(ctx, "r-central", []string{"bash"}, true)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if a == nil {
			break
		}
		claimed[a.TraceID] = true
	}
	for _, run := range []string{"run-across", "run-born", "run-after"} {
		if !claimed[run] {
			t.Errorf("%s was not claimed by the renamed agency's runner (claimed %v)", run, claimed)
		}
	}
}

// 2.5.2 GR — a new installation names the built-in agency (at its first
// start; settings.NameNewInstallationAgency), audited as the system; an
// existing one keeps "Global" (nothing renames it on an upgrade). The name is
// "Administration" since 2.5.3 (2.5.2's was "Central").
func TestGR_ANewInstallationCallsItAdministration(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	var name string
	_ = svc.db.QueryRow(`SELECT name FROM agencies WHERE id = 'global'`).Scan(&name)
	if name != "Global" {
		t.Fatalf("after the migrations alone the built-in agency is %q, want Global (migration 1220 is history)", name)
	}
	if err := settings.NameNewInstallationAgency(ctx, svc.db); err != nil {
		t.Fatal(err)
	}
	if got := settings.GlobalAgencyName(ctx, svc.db); got != "Administration" {
		t.Errorf("a new installation's built-in agency is %q, want Administration", got)
	}
	var by string
	_ = svc.db.QueryRow(`SELECT actor FROM change_log WHERE category = 'Agencies' AND target = 'Administration' ORDER BY id DESC LIMIT 1`).Scan(&by)
	if by != "system" {
		t.Errorf("the naming was recorded as %q, want system", by)
	}
}

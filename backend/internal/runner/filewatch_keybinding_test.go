package runner

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// LR-47 — the file-arrival half of the producer conformance: a sighting for a
// shell job that binds an SSH key fires when an agent serves the job's scope
// (the agent delivers the key), and is refused, with the reason recorded on
// the sighting, when none does — the local runner cannot deliver a key, and a
// run queued for a runner that does not exist would hold the fleet cap.
func TestSightingForKeyBoundJobNeedsAnAgent(t *testing.T) {
	for _, tc := range []struct {
		name         string
		agentServes  bool
		wantFire     bool
		wantExecutor string
	}{
		{"no agent serves the scope", false, false, ""},
		{"an agent serves the scope", true, true, "runner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			ctx := context.Background()
			// The scope is the tax agency's. r1 is Global's agent, and serves
			// tax only in the second case (seeded as tax's own agent).
			insertAgencyRow(t, svc, "ag-tax", "tax-agency")
			insertScopeRow(t, svc, "s-tax", "tax")
			if _, err := svc.db.Exec(`DELETE FROM scope_agencies WHERE scope_id = 's-tax'`); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.db.Exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('s-tax', 'ag-tax')`); err != nil {
				t.Fatal(err)
			}
			insertRunner(t, svc, "r1", "one", "online", []string{"bash"})
			if tc.agentServes {
				if _, err := svc.db.Exec(`DELETE FROM runner_agencies WHERE runner_id = 'r1'`); err != nil {
					t.Fatal(err)
				}
				addRunnerAgency(t, svc, "r1", "ag-tax")
			}
			// Scoped, so the AF unbound probe (unscoped runs only) is not what refuses it.
			seedWatchJob(t, svc, "git", "ingest", "tax", `[{"path":"/srv/incoming/*.csv"}]`)
			if err := runref.ReplaceBindings(ctx, svc.db,
				runref.Owner{Kind: "job", Source: "git", Name: "ingest"},
				[]runref.Binding{{Kind: runref.KindKey, Name: "deploy_key"}}, "t"); err != nil {
				t.Fatal(err)
			}
			in := sightingIn{JobSource: "git", JobName: "ingest", Path: "/srv/incoming/a.csv", SizeBytes: 42, MTime: "2026-08-11T00:00:00Z"}
			ok, err := svc.recordAndFireSighting(ctx, "r1", in, specFor(in))
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.wantFire {
				t.Fatalf("fired = %v, want %v", ok, tc.wantFire)
			}
			var reason sql.NullString
			if err := svc.db.QueryRow(`SELECT refused_reason FROM file_watch_sightings WHERE job_name='ingest'`).Scan(&reason); err != nil {
				t.Fatalf("no sighting row: %v", err)
			}
			if tc.wantFire && reason.Valid && reason.String != "" {
				t.Errorf("a sighting an agent can serve was refused: %q", reason.String)
			}
			if !tc.wantFire && reason.String != runref.ReasonKeyBindingNeedsAgent {
				t.Errorf("refused_reason = %q, want %q", reason.String, runref.ReasonKeyBindingNeedsAgent)
			}
			if tc.wantFire {
				var executor string
				if err := svc.db.QueryRow(`SELECT executor FROM runs WHERE job_name='ingest'`).Scan(&executor); err != nil || executor != tc.wantExecutor {
					t.Errorf("the fired run's executor = %q (%v), want %q", executor, err, tc.wantExecutor)
				}
			}
		})
	}
}

package runner

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// KB — the file-arrival half of the producer conformance: a sighting for a
// key-bound job that resolves to the ssh executor is refused with the reason
// recorded on the sighting; on the runner executor it fires.
func TestSightingForKeyBoundJobOnSSHIsRefused(t *testing.T) {
	for _, tc := range []struct {
		executor string
		wantFire bool
	}{{"ssh", false}, {"runner", true}} {
		t.Run(tc.executor, func(t *testing.T) {
			svc := newTestService(t)
			ctx := context.Background()
			insertRunner(t, svc, "r1", "one", "online", []string{"bash"})
			// Scoped, so the AF unbound probe (unscoped runs only) is not what refuses it.
			seedWatchJob(t, svc, "git", "ingest", "tax", `[{"path":"/srv/incoming/*.csv"}]`)
			if _, err := svc.db.Exec(`UPDATE jobs SET executor=? WHERE name='ingest'`, tc.executor); err != nil {
				t.Fatal(err)
			}
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
				t.Errorf("runner-executor sighting was refused: %q", reason.String)
			}
			if !tc.wantFire && reason.String != runref.ReasonKeyBindingOnSSH {
				t.Errorf("refused_reason = %q, want %q", reason.String, runref.ReasonKeyBindingOnSSH)
			}
		})
	}
}

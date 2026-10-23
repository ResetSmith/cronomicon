package runner

import (
	"context"
	"database/sql"
	"testing"
)

// LR-42 — the file-arrival half of the producer conformance for the one
// executor: a sighting fires onto the runner executor whatever the job's own
// `executor` says. That column is no longer read. (Until 2.3.0 a job that asked
// for ssh on a scope bound to runners was refused, `scope_requires_runner`;
// there is nothing left to ask for.)
func TestSightingIgnoresTheJobsExecutor(t *testing.T) {
	for _, tc := range []struct {
		name, jobExecutor string
	}{
		{"the job's executor says ssh", "ssh"},
		{"the job's executor says runner", "runner"},
		{"the job expresses no executor", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			ctx := context.Background()
			insertRunner(t, svc, "r1", "one", "online", []string{"bash"})
			insertScopeRow(t, svc, "s-dmz", "dmz-web")
			bindScope(t, svc, "s-dmz", "r1", "one")
			seedWatchJob(t, svc, "git", "ingest", "dmz-web", `[{"path":"/srv/incoming/*.csv"}]`)
			if tc.jobExecutor != "" {
				if _, err := svc.db.Exec(`UPDATE jobs SET executor=? WHERE name='ingest'`, tc.jobExecutor); err != nil {
					t.Fatal(err)
				}
			}
			in := sightingIn{JobSource: "git", JobName: "ingest", Path: "/srv/incoming/a.csv", SizeBytes: 42, MTime: "2026-08-11T00:00:00Z"}
			ok, err := svc.recordAndFireSighting(ctx, "r1", in, specFor(in))
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("the sighting did not fire")
			}
			var reason sql.NullString
			if err := svc.db.QueryRow(`SELECT refused_reason FROM file_watch_sightings WHERE job_name='ingest'`).Scan(&reason); err != nil {
				t.Fatalf("no sighting row: %v", err)
			}
			if reason.Valid && reason.String != "" {
				t.Errorf("the sighting was refused: %q", reason.String)
			}
			var executor string
			if err := svc.db.QueryRow(`SELECT executor FROM runs WHERE job_name='ingest'`).Scan(&executor); err != nil {
				t.Fatalf("no run row for the fired sighting: %v", err)
			}
			if executor != "runner" {
				t.Errorf("executor = %q, want runner", executor)
			}
		})
	}
}

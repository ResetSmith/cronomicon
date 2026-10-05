package runner

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// SB — the file-arrival half of the producer conformance for a scope bound to
// runners: a sighting for a job that asks for ssh is refused with the reason
// recorded on the sighting, and one for a job with no executor of its own fires
// onto the runner executor.
func TestSightingOnABoundScope(t *testing.T) {
	for _, tc := range []struct {
		name, jobExecutor string
		wantFire          bool
	}{
		{"job asks for ssh", "ssh", false},
		{"job expresses no executor", "", true},
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
			if ok != tc.wantFire {
				t.Fatalf("fired = %v, want %v", ok, tc.wantFire)
			}
			var reason sql.NullString
			if err := svc.db.QueryRow(`SELECT refused_reason FROM file_watch_sightings WHERE job_name='ingest'`).Scan(&reason); err != nil {
				t.Fatalf("no sighting row: %v", err)
			}
			if !tc.wantFire {
				if reason.String != execspec.ReasonScopeRequiresRunner {
					t.Errorf("refused_reason = %q, want %q", reason.String, execspec.ReasonScopeRequiresRunner)
				}
				return
			}
			if reason.Valid && reason.String != "" {
				t.Errorf("the sighting was refused: %q", reason.String)
			}
			var executor string
			if err := svc.db.QueryRow(`SELECT executor FROM runs WHERE job_name='ingest'`).Scan(&executor); err != nil {
				t.Fatalf("no run row for the fired sighting: %v", err)
			}
			if executor != "runner" {
				t.Errorf("executor = %q, want runner — a shell job on a bound scope must not default to ssh", executor)
			}
		})
	}
}

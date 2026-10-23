package runner

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// A file arrival reads the bindings of the job's SCRIPT by the uid on the job
// (jobs.script_uid, migration 1290). A script that binds a department's secret
// refuses the unbound arrival; another script of the same name does not.
func TestArrivalFollowsTheJobsOwnScript(t *testing.T) {
	svc := newTestService(t)
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	insertRunner(t, svc, "r1", "one", "online", []string{"bash"})
	seedWatchJob(t, svc, "git", "ingest", "", `[{"path":"/srv/incoming/*.csv"}]`)
	insertAgencyRow(t, svc, "ag-tax", "Tax")
	exec(`INSERT INTO secrets (id, key, source, owner_agency, created_at) VALUES ('s1', 'DEPT_PASSWORD', 'stored', 'ag-tax', 't')`)
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES ('s1', 'ag-tax')`)
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, command, content_hash, synced_at) VALUES('uid-lib','global','lib.sh','bash','x','h','t')`)
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, command, content_hash, synced_at) VALUES('uid-lib-other','repo-b','lib.sh','bash','x','h','t')`)
	if err := runref.ReplaceBindings(context.Background(), svc.db,
		runref.Owner{Kind: "script", Name: "lib.sh", UID: "uid-lib"},
		[]runref.Binding{{Kind: runref.KindSecret, Name: "DEPT_PASSWORD"}}, "t"); err != nil {
		t.Fatalf("bind secret to the script: %v", err)
	}
	arrival := func(path string) sightingIn {
		return sightingIn{JobSource: "git", JobName: "ingest", Path: path, SizeBytes: 42, MTime: "2026-08-11T00:00:00Z"}
	}

	// The job uses the script that binds the department's secret.
	exec(`UPDATE jobs SET script_ref='lib.sh', script_uid='uid-lib' WHERE name='ingest'`)
	sg := arrival("/srv/incoming/a.csv")
	ok, err := svc.recordAndFireSighting(context.Background(), "r1", sg, specFor(sg))
	if ok || err != nil {
		t.Fatalf("an arrival whose script's secret could not resolve fired (%v, %v)", ok, err)
	}
	if got := sightingReason(t, svc, sg.Path); got != runref.QueuedReasonUnboundReferences {
		t.Errorf("refused_reason = %q, want %q", got, runref.QueuedReasonUnboundReferences)
	}
	if n := runCountFor(t, svc, "ingest"); n != 0 {
		t.Errorf("%d runs were enqueued for a refused arrival", n)
	}

	// The same name, another script: not this job's bindings, so it fires.
	exec(`UPDATE jobs SET script_uid='uid-lib-other' WHERE name='ingest'`)
	sg = arrival("/srv/incoming/b.csv")
	ok, err = svc.recordAndFireSighting(context.Background(), "r1", sg, specFor(sg))
	if !ok || err != nil {
		t.Errorf("an arrival was refused (%v, %v; reason %q) for a binding of ANOTHER script that shares its script's name",
			ok, err, sightingReason(t, svc, sg.Path))
	}
}

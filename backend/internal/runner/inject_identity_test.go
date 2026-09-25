package runner

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// R2F-1 end-to-end on the dispatch read path: a run of one twin collects only
// THAT job's bindings. collectReferenceBindings is the shared pass behind both
// manifest injection and ingest redaction, so a union here would ship one
// department's credentials to the other's run and seed the wrong redaction
// dictionary at the same time.
//
// Note the uids: a seed that omits them (jobs.uid takes NULL even as PRIMARY KEY)
// silently exercises only the legacy name arm and would pass either way.
func TestCollectReferenceBindingsFollowsRunJobIdentity(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	ctx := context.Background()

	for _, j := range []struct{ uid, secret string }{
		{"uid-twin-a", "A_PASS"},
		{"uid-twin-b", "B_PASS"},
	} {
		if _, err := svc.db.Exec(`INSERT INTO jobs(uid, name, source, run_type, command, concurrency_policy, synced_at)
			VALUES(?,'twin','cronomicon','bash','echo hi','Allow',?)`, j.uid, now()); err != nil {
			t.Fatalf("seed job %s: %v", j.uid, err)
		}
		if _, err := svc.db.Exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at, owner_uid)
			VALUES('job','cronomicon','twin','secret',?,?,?)`, j.secret, now(), j.uid); err != nil {
			t.Fatalf("seed binding %s: %v", j.secret, err)
		}
	}

	// The run belongs to twin A — job_uid is frozen at enqueue (R2-1).
	traceID := db.NewTraceID()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, job_source, job_uid, run_type, scope, status, executor, triggered_by, trigger_kind, started_at, created_at)
		VALUES(?, 'twin', 'cronomicon', 'uid-twin-a', 'bash', 'prod', 'running', 'runner', 'ops@x', 'manual', ?, ?)`,
		traceID, now(), now()); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	bindings, err := svc.collectReferenceBindings(ctx, traceID, "twin", "cronomicon", "")
	if err != nil {
		t.Fatalf("collectReferenceBindings: %v", err)
	}
	if len(bindings) != 1 || bindings[0].Name != "A_PASS" {
		names := make([]string, 0, len(bindings))
		for _, b := range bindings {
			names = append(names, b.Name)
		}
		t.Fatalf("run of twin A collected %v, want [A_PASS] — a sibling department's credential must not reach this run", names)
	}
}

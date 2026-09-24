package scheduler_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/scheduler"
)

// RR-0a — RA-20a's become-file requirement must be injected by EVERY queued-run
// producer, not only the cron path. From v0.57.3 to v1.5.16 only EnqueueRun
// (cron) carried the arm; EnqueueRunWithID (manual, token, file-watch,
// promoted) did not, so a become-password job triggered by anything except a
// schedule could be claimed by a pre-v9 runner and 409 at manifest time.
// Both producers are asserted side by side because they share a column list
// that is maintained by hand (the run-row-unification plan).

func seedBecomeJob(t *testing.T, pool *sql.DB, name, uid, becomeSecret, requiresJSON string) {
	t.Helper()
	var secret any
	if becomeSecret != "" {
		secret = becomeSecret
	}
	_, err := pool.ExecContext(context.Background(), `
		INSERT INTO jobs (name, source, uid, run_type, concurrency_policy, synced_at,
		                  become_password_secret, requires_json)
		VALUES (?, 'git', ?, 'ansible', 'Allow', '2026-01-01T00:00:00Z', ?, ?)
	`, name, uid, secret, requiresJSON)
	if err != nil {
		t.Fatalf("seed job %q: %v", name, err)
	}
}

func runRequires(t *testing.T, pool *sql.DB, where string, arg any) sql.NullString {
	t.Helper()
	var r sql.NullString
	if err := pool.QueryRowContext(context.Background(),
		`SELECT requires_json FROM runs WHERE `+where, arg).Scan(&r); err != nil {
		t.Fatalf("read requires_json: %v", err)
	}
	return r
}

func TestEnqueueRunWithID_InjectsBecomeFile(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name     string
		secret   string
		requires string
		want     string // substring that must be present, or "" for NULL
		wantNull bool
	}{
		{"no requires, become password → injected", "vault:become", "[]", "become-file", false},
		{"existing requires, become password → appended", "vault:become", `["vault"]`, "become-file", false},
		{"already declared → not duplicated", "vault:become", `["become-file"]`, "become-file", false},
		{"no become password, no requires → NULL", "", "[]", "", true},
		{"no become password, requires kept verbatim", "", `["vault"]`, `["vault"]`, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := openPool(t)
			job := "bf-job"
			uid := "uid-bf"
			seedBecomeJob(t, pool, job, uid, tc.secret, tc.requires)

			traceID, err := scheduler.EnqueueRunWithID(ctx, pool, scheduler.EnqueueParams{
				JobName: job, JobSource: "git", RunType: "ansible", TriggerKind: "manual", TriggeredBy: "alice",
			})
			if err != nil {
				t.Fatalf("EnqueueRunWithID: %v", err)
			}
			got := runRequires(t, pool, "id = ?", traceID)
			if tc.wantNull {
				if got.Valid {
					t.Fatalf("requires_json = %q, want NULL", got.String)
				}
				return
			}
			if !got.Valid || !strings.Contains(got.String, tc.want) {
				t.Fatalf("requires_json = %v, want containing %q", got, tc.want)
			}
			if strings.Count(got.String, "become-file") > 1 {
				t.Fatalf("become-file duplicated: %s", got.String)
			}
			_ = i
		})
	}
}

// TestEnqueueRun_BecomeFileParity pins the two producers to the same answer.
func TestEnqueueRun_BecomeFileParity(t *testing.T) {
	ctx := context.Background()
	pool := openPool(t)
	seedBecomeJob(t, pool, "parity", "uid-parity", "vault:become", `["vault"]`)

	withID, err := scheduler.EnqueueRunWithID(ctx, pool, scheduler.EnqueueParams{
		JobName: "parity", JobSource: "git", RunType: "ansible", TriggerKind: "manual", TriggeredBy: "alice",
	})
	if err != nil {
		t.Fatalf("EnqueueRunWithID: %v", err)
	}
	if err := scheduler.EnqueueRun(ctx, pool, scheduler.EnqueueParams{
		JobName: "parity", JobSource: "git", RunType: "ansible", TriggerKind: "scheduled", TriggeredBy: "cron",
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}

	a := runRequires(t, pool, "id = ?", withID)
	b := runRequires(t, pool, "trigger_kind = ?", "scheduled")
	if a.String != b.String {
		t.Fatalf("producers drifted: EnqueueRunWithID=%q EnqueueRun=%q", a.String, b.String)
	}
	if !strings.Contains(a.String, "become-file") || !strings.Contains(a.String, "vault") {
		t.Fatalf("requires_json = %q, want vault AND become-file", a.String)
	}
}

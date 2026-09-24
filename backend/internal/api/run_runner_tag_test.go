package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// RT-2 end-to-end — the pin as it actually reaches a run
// (the runner-targeting plan).
//
// The unit tests cover the resolver's tri-state in isolation; these cover the
// wiring around it, where the states are easiest to lose: the JSON decode
// (absent vs null vs ""), the freeze onto runs.runner_tag, and RT-Q5's rejection.

// seedPinJob inserts a git-source job with an explicit executor so the RT-Q5
// gate is exercised deliberately rather than by whatever the default chain
// happens to resolve to.
func seedPinJob(t *testing.T, pool *sql.DB, name, executor, declared string) int64 {
	t.Helper()
	ctx := context.Background()
	var pin any
	if declared != "" {
		pin = declared
	}
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO jobs(name, source, run_type, command, scope, executor, runner_tag, content_hash, source_path, synced_at)
		 VALUES(?,'git','bash','echo hi','Prod',?,?,'sha256:e',?,'t')`,
		name, executor, pin, "jobs/"+name+".yaml"); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	var rowid int64
	if err := pool.QueryRowContext(ctx, `SELECT rowid FROM jobs WHERE name=?`, name).Scan(&rowid); err != nil {
		t.Fatalf("rowid: %v", err)
	}
	return rowid
}

func TestRunFreezesResolvedRunnerTag(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	client, csrf := devLoginWithCSRF(t, ts)

	run := func(rowid int64, body map[string]any) int {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/api/v1/jobs/%d/run", ts.URL, rowid), bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST run: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	frozen := func(job string) sql.NullString {
		t.Helper()
		var got sql.NullString
		if err := pool.QueryRowContext(ctx,
			`SELECT runner_tag FROM runs WHERE job_name=? ORDER BY created_at DESC LIMIT 1`, job).Scan(&got); err != nil {
			t.Fatalf("read frozen pin: %v", err)
		}
		return got
	}

	t.Run("declared pin is inherited when the body says nothing", func(t *testing.T) {
		id := seedPinJob(t, pool, "pin-inherit", "runner", "vlan-dmz")
		if code := run(id, map[string]any{}); code != http.StatusAccepted {
			t.Fatalf("run = %d, want 202", code)
		}
		if got := frozen("pin-inherit"); got.String != "vlan-dmz" {
			t.Errorf("frozen pin = %q, want vlan-dmz", got.String)
		}
	})

	t.Run("per-run override wins", func(t *testing.T) {
		id := seedPinJob(t, pool, "pin-override", "runner", "vlan-dmz")
		if code := run(id, map[string]any{"runnerTag": "vlan-core"}); code != http.StatusAccepted {
			t.Fatalf("run = %d, want 202", code)
		}
		if got := frozen("pin-override"); got.String != "vlan-core" {
			t.Errorf("frozen pin = %q, want vlan-core", got.String)
		}
	})

	// The break-glass case, and the reason the body field is a *string: "" must
	// reach the resolver as a decision, not as an absent field.
	t.Run("empty string unpins this run only", func(t *testing.T) {
		id := seedPinJob(t, pool, "pin-breakglass", "runner", "vlan-dmz")
		if code := run(id, map[string]any{"runnerTag": ""}); code != http.StatusAccepted {
			t.Fatalf("run = %d, want 202", code)
		}
		if got := frozen("pin-breakglass"); got.Valid && got.String != "" {
			t.Errorf("frozen pin = %q, want unpinned — the per-run unpin was lost", got.String)
		}
		// The job itself is untouched: this was one run's decision.
		var declared sql.NullString
		_ = pool.QueryRowContext(ctx, `SELECT runner_tag FROM jobs WHERE name='pin-breakglass'`).Scan(&declared)
		if declared.String != "vlan-dmz" {
			t.Errorf("job's declared pin = %q; a per-run unpin must not edit the job", declared.String)
		}
	})

	t.Run("unpinned job freezes NULL", func(t *testing.T) {
		id := seedPinJob(t, pool, "pin-none", "runner", "")
		if code := run(id, map[string]any{}); code != http.StatusAccepted {
			t.Fatalf("run = %d, want 202", code)
		}
		if got := frozen("pin-none"); got.Valid {
			t.Errorf("frozen pin = %q, want NULL for an unpinned job", got.String)
		}
	})
}

// TestRunRejectsPinOnSSHExecutor is RT-Q5. A pin on a run that resolves to the
// in-process ssh pool would be silently meaningless — that pool has no runner and
// its claim query deliberately carries no pin predicate — so the run would
// execute from the control plane while the operator believed it was confined to a
// network segment. That is the failure the band exists to prevent, so it is the
// one hard rejection in it.
func TestRunRejectsPinOnSSHExecutor(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	post := func(rowid int64, body map[string]any) (int, string) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/api/v1/jobs/%d/run", ts.URL, rowid), bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST run: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out.Error + " " + out.Message
	}

	// Declared pin + ssh executor on the job.
	id := seedPinJob(t, pool, "pin-ssh", "ssh", "vlan-dmz")
	code, msg := post(id, map[string]any{})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("pinned + ssh = %d, want 422 (message %q)", code, msg)
	}

	// The escape hatch still works: unpin this run and it is accepted.
	if code, msg := post(id, map[string]any{"runnerTag": ""}); code != http.StatusAccepted {
		t.Fatalf("unpinned run on an ssh job = %d, want 202 (message %q)", code, msg)
	}

	// And a per-run pin on an ssh job is rejected just the same, even though the
	// job itself declares nothing.
	id2 := seedPinJob(t, pool, "pin-ssh2", "ssh", "")
	if code, _ := post(id2, map[string]any{"runnerTag": "vlan-dmz"}); code != http.StatusUnprocessableEntity {
		t.Fatalf("per-run pin on an ssh job = %d, want 422", code)
	}
}

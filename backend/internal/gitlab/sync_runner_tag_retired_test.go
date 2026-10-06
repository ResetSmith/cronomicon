package gitlab

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSyncWarnsAboutARetiredRunnerTag drives the real sync over a job that
// still carries `runner_tag:` — the key the SB band retired — through the whole
// life of that line: unbound scope, dismissed notice, bound scope, line removed.
//
// The owner's rule is that such a job ALWAYS syncs; it is never refused. What
// the product owes its author instead is to be told, because the decode is not
// strict and silence would leave them believing the job is still confined:
//
//   - on a scope with no runner binding, nothing confines the job any more. The
//     warning says so plainly and the job goes on the notice list, once;
//   - a dismissed notice is not re-raised by the next sync;
//   - on a bound scope the line is merely stale and the warning says that;
//   - removing the line clears the notice sync wrote — the fix needs no dismiss;
//   - a notice the MIGRATION wrote for a git job is never sync's to delete.
func TestSyncWarnsAboutARetiredRunnerTag(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	ctx := context.Background()
	pool := svc.db
	var logs bytes.Buffer
	svc.log = slog.New(slog.NewTextHandler(&logs, nil))

	job := func(name string, pinned bool) string {
		y := "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: " + name +
			"\nspec:\n  run_type: bash\n  command: echo hi\n  scope: dmz\n"
		if pinned {
			y += "  runner_tag: vlan-dmz\n"
		}
		return y
	}
	gitCommitFile(t, repo, remote, "inventory/dmz.ini", "[web]\nweb1\n", "add dmz")
	gitCommitFile(t, repo, remote, "jobs/pinned.yaml", job("pinned", true), "add pinned")
	gitCommitFile(t, repo, remote, "jobs/plain.yaml", job("plain", false), "add plain")

	const (
		unconfined = "NOTHING CONFINES THIS JOB NOW"
		stale      = "its scope is bound to runners and it runs on those"
		retired    = "still declares runner_tag"
	)
	sync := func() string {
		t.Helper()
		logs.Reset()
		if r := svc.SyncBlocking(ctx, "t"); r.Status != "success" {
			t.Fatalf("sync = %q, want success — a retired key is a warning, never an error (%s)", r.Status, r.ErrorMessage)
		}
		return logs.String()
	}
	notices := func(job string) (n int, reason string, dismissed bool) {
		t.Helper()
		rows, err := pool.Query(`SELECT reason, dismissed_at IS NOT NULL FROM retired_runner_pins WHERE job_source='git' AND job_name=?`, job)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			if err := rows.Scan(&reason, &dismissed); err != nil {
				t.Fatal(err)
			}
			n++
		}
		return n, reason, dismissed
	}

	// 1. Unbound scope: loud, and on the notice list.
	out := sync()
	if !strings.Contains(out, unconfined) || !strings.Contains(out, "job=pinned") {
		t.Errorf("first sync did not say the pinned job is unconfined:\n%s", out)
	}
	if strings.Contains(out, "job=plain") && strings.Contains(out, retired) && strings.Count(out, retired) > 1 {
		t.Errorf("a job without the key was warned about:\n%s", out)
	}
	if n, reason, _ := notices("pinned"); n != 1 || reason != "leftover_git_key" {
		t.Fatalf("notices for pinned = %d %q, want one leftover_git_key", n, reason)
	}
	var scope, tag, uid string
	if err := pool.QueryRow(`SELECT scope, runner_tag, COALESCE(job_uid,'') FROM retired_runner_pins WHERE job_name='pinned'`).
		Scan(&scope, &tag, &uid); err != nil || scope != "dmz" || tag != "vlan-dmz" || uid == "" {
		t.Errorf("notice = scope %q tag %q uid %q (err %v), want dmz, vlan-dmz and the job's uid", scope, tag, uid, err)
	}
	// The job itself synced, and its pin is stored nowhere.
	if n := jobCount(t, pool, "pinned"); n != 1 {
		t.Fatalf("the pinned job did not sync (%d rows) — it must never be refused", n)
	}

	// 2. A second sync does not stack a second notice.
	sync()
	if n, _, _ := notices("pinned"); n != 1 {
		t.Errorf("notices after a second sync = %d, want 1", n)
	}

	// 3. Dismissed stays dismissed. A migration-written notice for a git job is
	//    left alone throughout.
	if _, err := pool.Exec(`UPDATE retired_runner_pins SET dismissed_at='t', dismissed_by='ops@example' WHERE job_name='pinned'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO retired_runner_pins (job_uid,job_name,job_source,scope,runner_tag,reason,recorded_at)
	                        VALUES ('u','plain','git','dmz','edge','mixed_pins','t')`); err != nil {
		t.Fatal(err)
	}
	sync()
	if n, _, dismissed := notices("pinned"); n != 1 || !dismissed {
		t.Errorf("after a sync following a dismiss: %d notices, dismissed=%v; want the one row, still dismissed", n, dismissed)
	}

	// 4. Bound scope: the line is stale, not dangerous.
	if _, err := pool.Exec(`
		INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_by, bound_at)
		SELECT id, 'r-dmz', 'runner-dmz-01', 'ops@example', 'now' FROM scopes WHERE name='dmz'`); err != nil {
		t.Fatalf("bind: %v", err)
	}
	out = sync()
	if !strings.Contains(out, stale) || strings.Contains(out, unconfined) {
		t.Errorf("on a bound scope the warning should call the line stale, not the job unconfined:\n%s", out)
	}

	// 5. The line goes; so does the warning, and the notice sync wrote.
	gitCommitFile(t, repo, remote, "jobs/pinned.yaml", job("pinned", false), "drop runner_tag")
	out = sync()
	if strings.Contains(out, retired) {
		t.Errorf("still warning after the line was removed:\n%s", out)
	}
	if n, _, _ := notices("pinned"); n != 0 {
		t.Errorf("notices for pinned after the line was removed = %d, want 0", n)
	}
	if n, reason, _ := notices("plain"); n != 1 || reason != "mixed_pins" {
		t.Errorf("the migration's notice for plain = %d %q, want it untouched", n, reason)
	}
}

// TestValidateWarnsAboutARetiredRunnerTag — `cronomicon validate` reports the
// retired key as a WARNING with its line, on a single file and across a
// checkout, and never as an error: a job-definitions repository written before
// the change must keep passing its CI check.
func TestValidateWarnsAboutARetiredRunnerTag(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	pinned := filepath.Join(dir, "jobs", "pinned.yaml")
	plain := filepath.Join(dir, "jobs", "plain.yaml")
	body := "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: %s\nspec:\n  run_type: bash\n  command: echo hi\n"
	if err := os.WriteFile(pinned, []byte(strings.Replace(body, "%s", "pinned", 1)+"  runner_tag: vlan-dmz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, []byte(strings.Replace(body, "%s", "plain", 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	errs, warnings, err := ValidateFile(pinned)
	if err != nil || len(errs) != 0 {
		t.Fatalf("ValidateFile = errs %v, err %v; want none — the key is retired, not invalid", errs, err)
	}
	if len(warnings) != 1 || warnings[0].Line != 8 || !strings.Contains(warnings[0].Message, "runner_tag") {
		t.Errorf("warnings = %+v, want one, on line 8, naming runner_tag", warnings)
	}
	if _, warnings, _ := ValidateFile(plain); len(warnings) != 0 {
		t.Errorf("a job without the key drew warnings: %+v", warnings)
	}

	errs, warnings, err = ValidateRepo(dir)
	if err != nil || len(errs) != 0 {
		t.Fatalf("ValidateRepo = errs %v, err %v; want none", errs, err)
	}
	hits := 0
	for _, w := range warnings {
		if w.Field == "spec.runner_tag" {
			hits++
			if filepath.Base(w.File) != "pinned.yaml" {
				t.Errorf("warning attributed to %s, want pinned.yaml", w.File)
			}
		}
	}
	if hits != 1 {
		t.Errorf("ValidateRepo runner_tag warnings = %d, want 1 (%+v)", hits, warnings)
	}
}

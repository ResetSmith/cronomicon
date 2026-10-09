package runner

import (
	"context"
	"database/sql"
	"sort"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// Migration 1290 (2.4.0, GR-5) on the dispatch read path: a run collects the
// bindings of the SCRIPT IT FROZE (runs.script_uid), and of no other script of
// that name. collectReferenceBindings is the pass behind both manifest
// injection and ingest redaction, so a lookup by name here would ship one
// repository's credentials to a run of another's script and seed the wrong
// redaction dictionary at the same time.
//
// Three runs name `deploy.sh`: one froze Global's script, one the other
// repository's, and one froze none (its script had gone before it was queued).
func TestCollectReferenceBindingsFollowsRunScriptIdentity(t *testing.T) {
	svc := newTestService(t)
	enableInjection(svc)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}

	// Two scripts of one name, in two repositories, each with its own secret.
	for _, sc := range []struct{ uid, repo, secret string }{
		{"uid-s-global", "global", "GLOBAL_PASS"},
		{"uid-s-other", "repo-b", "OTHER_PASS"},
	} {
		exec(`INSERT INTO scripts(uid, repo_id, name, run_type, command, content_hash, synced_at)
		      VALUES(?, ?, 'deploy.sh', 'bash', 'echo hi', 'sha256:x', ?)`, sc.uid, sc.repo, now())
		exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at, owner_uid)
		      VALUES('script', '', 'deploy.sh', 'secret', ?, ?, ?)`, sc.secret, now(), sc.uid)
	}
	exec(`INSERT INTO jobs(uid, name, source, run_type, command, concurrency_policy, synced_at, script_ref)
	      VALUES('uid-roll', 'roll', 'git', 'bash', 'echo hi', 'Allow', ?, 'deploy.sh')`, now())

	run := func(scriptUID any) string {
		t.Helper()
		id := db.NewTraceID()
		exec(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, scope, status, executor, triggered_by, trigger_kind,
		                       started_at, created_at, script_ref, script_uid)
		      VALUES(?, 'roll', 'git', 'uid-roll', 'bash', 'prod', 'running', 'runner', 'ops@x', 'manual', ?, ?, 'deploy.sh', ?)`,
			id, now(), now(), scriptUID)
		return id
	}
	collected := func(runID string) []string {
		t.Helper()
		bs, err := svc.collectReferenceBindings(ctx, runID, "roll", "git", "deploy.sh")
		if err != nil {
			t.Fatalf("collectReferenceBindings: %v", err)
		}
		names := make([]string, 0, len(bs))
		for _, b := range bs {
			names = append(names, b.Name)
		}
		sort.Strings(names)
		return names
	}
	same := func(got []string, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range want {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	ofGlobal, ofOther, ofNone := run("uid-s-global"), run("uid-s-other"), run(nil)

	if got := collected(ofGlobal); !same(got, "GLOBAL_PASS") {
		t.Errorf("a run of Global's deploy.sh collected %v, want [GLOBAL_PASS]: another repository's credential must not reach it", got)
	}
	if got := collected(ofOther); !same(got, "OTHER_PASS") {
		t.Errorf("a run of the other repository's deploy.sh collected %v, want [OTHER_PASS]", got)
	}
	// The name alone finds nothing. This is the reader that fails closed: before
	// 1290 it would have matched by name, and with two repositories that is both.
	if got := collected(ofNone); !same(got) {
		t.Errorf("a run with a script name and no script uid collected %v, want nothing", got)
	}

	// The claim and its mirror ask the same question in SQL. They are asked here
	// through the production code, not through a copy of their text: with the
	// injection fence up, a runner that may NOT be handed injected values cannot
	// take a run whose own script binds a secret, and CAN take a run that only
	// shares that script's name.
	execspec.SetInjectionGateArmed(true)
	t.Cleanup(func() { execspec.SetInjectionGateArmed(true) })
	insertRunner(t, svc, "r1", "r1", "online", []string{"bash"})
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-open', 'open', 'cronomicon', ?)`, now())
	// A third script of the name, which binds nothing.
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, command, content_hash, synced_at)
	      VALUES('uid-s-plain', 'repo-c', 'deploy.sh', 'bash', 'echo hi', 'sha256:x', ?)`, now())
	for _, tc := range []struct {
		what      string
		scriptUID any
		gated     bool
	}{
		{"its own script binds a secret", "uid-s-global", true},
		{"another script of that name binds one, its own binds nothing", "uid-s-plain", false},
		{"a script of that name binds one, and it froze no script", nil, false},
	} {
		exec(`DELETE FROM runs WHERE id = 'queued-run'`)
		exec(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, scope, requires_json, agencies_json, status,
		                       triggered_by, trigger_kind, executor, created_at, script_ref, script_uid)
		      VALUES('queued-run', 'roll', 'git', 'uid-roll', 'bash', 'open', '[]', '["Global"]', 'queued',
		             'test', 'manual', 'runner', ?, 'deploy.sh', ?)`, now(), tc.scriptUID)
		exec(`INSERT OR IGNORE INTO run_agencies(run_id, agency) VALUES('queued-run', 'Global')`)

		reason, err := execspec.UnclaimableReason(ctx, svc.db, "queued-run")
		if err != nil {
			t.Fatalf("UnclaimableReason: %v", err)
		}
		// The one online runner is not allowed injection, so a gated run has
		// nobody to take it, and the mirror says why.
		if tc.gated && reason == "" {
			t.Errorf("a run whose %s: the mirror sees a runner for it, although the only runner may not receive injected values", tc.what)
		}
		got, err := Claim(ctx, svc.db, svc.log, ClaimRequest{RunnerID: "r1", Caps: []string{"bash"}, InjectionOK: false, Actor: "test"})
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if claimed := got != nil; claimed == tc.gated {
			t.Errorf("a run whose %s: claimed by a runner without injection = %v, want %v", tc.what, claimed, !tc.gated)
		}
		if !tc.gated && reason != "" {
			t.Errorf("a run whose %s: the mirror says it is waiting (%q), although nothing of its own is injected", tc.what, reason)
		}
	}
	exec(`DELETE FROM runs WHERE id = 'queued-run'`)

	// And a key bound to ONE repository's script makes only that script's runs
	// "bind a key" (the rule that keeps such a run off the local runner).
	exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at, owner_uid)
	      VALUES('script', '', 'deploy.sh', 'key', 'DEPLOY_KEY', ?, 'uid-s-other')`, now())
	bindsKey := func(runID string) bool {
		t.Helper()
		var b sql.NullBool
		if err := svc.db.QueryRow(`SELECT `+execspec.RunBindsKeySQL("r")+` FROM runs r WHERE r.id = ?`, runID).Scan(&b); err != nil {
			t.Fatalf("RunBindsKeySQL: %v", err)
		}
		return b.Bool
	}
	if bindsKey(ofGlobal) {
		t.Errorf("a run of Global's deploy.sh reads as binding the OTHER repository's key")
	}
	if !bindsKey(ofOther) {
		t.Errorf("a run of the script that binds the key does not read as binding it")
	}
	if bindsKey(ofNone) {
		t.Errorf("a run with no script uid reads as binding a script's key")
	}
}

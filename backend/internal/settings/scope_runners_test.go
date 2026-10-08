package settings

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestSetScopeRunnersRefusesAnUnauthorizedAddition pins the seam that ties the
// API's per-runner authorization to the write. The handler authorizes the
// runners it believes are being ADDED and hands the writer that set; the writer
// re-derives the additions inside its own transaction. If the two disagree —
// another operator unbound a runner in between, so a runner the caller listed
// as "already bound" is now an addition nobody gated — the write must be
// refused, not carried out on the strength of a check that never happened.
func TestSetScopeRunnersRefusesAnUnauthorizedAddition(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "scoperunners.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO scopes(id, name, source, created_at) VALUES('s1','shared','cronomicon','t')`)
	for _, id := range []string{"r-a", "r-b"} {
		exec(`INSERT INTO runners(id, name, status, registered_at, created_at) VALUES(?, ?, 'online', 't', 't')`, id, id)
	}

	// The caller was authorized for r-a only. r-b is in the list because, when the
	// caller looked, it was already bound — and it no longer is.
	authorized := map[string]bool{"r-a": true}
	_, err = SetScopeRunners(ctx, pool, "s1", []string{"r-a", "r-b"}, "ops@example",
		func(id string) bool { return authorized[id] })
	if !errors.Is(err, ErrBindingsChanged) {
		t.Fatalf("SetScopeRunners = %v, want ErrBindingsChanged", err)
	}
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM scope_runners`).Scan(&n); err != nil || n != 0 {
		t.Errorf("binding rows after the refused write = %d (err %v), want 0 — nothing may be half-applied", n, err)
	}

	// A runner that IS still bound needs no fresh authorization to stay.
	exec(`INSERT INTO scope_runners(scope_id, runner_id, runner_name, bound_at) VALUES('s1','r-b','r-b','t')`)
	sc, err := SetScopeRunners(ctx, pool, "s1", []string{"r-a", "r-b"}, "ops@example",
		func(id string) bool { return authorized[id] })
	if err != nil {
		t.Fatalf("SetScopeRunners with r-b already bound: %v", err)
	}
	if len(sc.BoundRunners) != 2 {
		t.Errorf("bound runners = %+v, want both", sc.BoundRunners)
	}
}

// TestBoundScopeWithWaitingRunsCannotBeRenamedOrDeleted. A run carries its scope
// by NAME. Rename or delete a bound scope while runs wait under that name and no
// row answers to it any more: the runs read as unrestricted and any runner in
// the agency may claim them. Both edits are refused until the work drains — and
// only then: an unbound scope, a bound scope with nothing waiting, and an edit
// that does not change the name are all unaffected.
func TestBoundScopeWithWaitingRunsCannotBeRenamedOrDeleted(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "scopebusy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	mk := func(name string) string {
		t.Helper()
		sc, err := CreateScope(ctx, pool, LocalScopeInput{Scope: name, Hosts: []string{"h1"}}, "ops@example")
		if err != nil {
			t.Fatalf("create scope %s: %v", name, err)
		}
		return sc.ID
	}
	queue := func(id, scope, executor string) {
		exec(`INSERT INTO runs (id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, created_at)
		      VALUES (?, 'j', 'bash', ?, 'queued', 'seed', 'manual', ?, 't')`, id, scope, executor)
	}
	bind := func(scopeID string) {
		exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES (?, 'r1', 'runner-1', 't')`, scopeID)
	}
	rename := func(id, to string) error {
		_, _, err := UpdateScope(ctx, pool, id, LocalScopeInput{Scope: to, Hosts: []string{"h1"}}, "ops@example")
		return err
	}
	isBusy := func(err error) bool {
		var busy *ErrBoundScopeBusy
		return errors.As(err, &busy)
	}

	// Unbound, with a queued run: free to rename.
	open := mk("open")
	queue("run-open", "open", "runner")
	if err := rename(open, "open-2"); err != nil {
		t.Errorf("renaming an UNBOUND scope with a queued run = %v, want it allowed", err)
	}

	// Bound, nothing waiting: free to rename. An ssh run does not count — no
	// runner claims it, so nothing about it changes.
	idle := mk("idle")
	bind(idle)
	queue("run-ssh", "idle", "ssh")
	if err := rename(idle, "idle-2"); err != nil {
		t.Errorf("renaming a bound scope with only an ssh run queued = %v, want it allowed", err)
	}

	// Bound, with a queued runner run.
	busy := mk("dmz")
	bind(busy)
	queue("run-dmz", "dmz", "runner")
	if err := rename(busy, "dmz-web"); !isBusy(err) {
		t.Errorf("renaming a bound scope with a queued run = %v, want ErrBoundScopeBusy", err)
	}
	if _, err := DeleteScope(ctx, pool, busy, "ops@example"); !isBusy(err) {
		t.Errorf("deleting a bound scope with a queued run = %v, want ErrBoundScopeBusy", err)
	}
	var name string
	if err := pool.QueryRow(`SELECT name FROM scopes WHERE id = ?`, busy).Scan(&name); err != nil || name != "dmz" {
		t.Errorf("after the refusals the scope is %q (err %v), want it untouched as dmz", name, err)
	}
	// An edit that keeps the name is not a rename.
	if _, _, err := UpdateScope(ctx, pool, busy, LocalScopeInput{Scope: "dmz", Hosts: []string{"h1", "h2"}}, "ops@example"); err != nil {
		t.Errorf("editing the hosts of a busy bound scope = %v, want it allowed", err)
	}

	// A parked run counts too: it is enqueued later under the same name.
	exec(`UPDATE runs SET status = 'success' WHERE id = 'run-dmz'`)
	exec(`INSERT INTO pending_runs (id, kind, name, source, scope, run_at, scheduled_by, created_at, status)
	      VALUES ('p1', 'job', 'j', 'cronomicon', 'dmz', 't', 'ops@example', 't', 'pending')`)
	if err := rename(busy, "dmz-web"); !isBusy(err) {
		t.Errorf("renaming a bound scope with a parked run = %v, want ErrBoundScopeBusy", err)
	}

	// Drained: both edits go through.
	exec(`DELETE FROM pending_runs`)
	if err := rename(busy, "dmz-web"); err != nil {
		t.Errorf("renaming once nothing is waiting = %v, want it allowed", err)
	}
	if found, err := DeleteScope(ctx, pool, busy, "ops@example"); err != nil || !found {
		t.Errorf("deleting once nothing is waiting = %v, %v; want it allowed", found, err)
	}
}

// TestPreviewScopeRunners covers the three questions the bind form asks before
// it saves: which jobs change executor, which would be refused, and whether the
// runners being proposed can serve what runs on the scope. Each is asked in both
// directions — binding an open scope and clearing a bound one — because clearing
// is the direction that sends work back to the server.
func TestPreviewScopeRunners(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "preview.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-fin','Finance','t')`)
	exec(`INSERT INTO scopes(id, name, source, created_at) VALUES('s-dmz','dmz-web','cronomicon','t')`)
	exec(`INSERT INTO scope_agencies(scope_id, agency_id) VALUES('s-dmz','ag-fin')`)
	job := func(uid, name, runType string, executor any) {
		exec(`INSERT INTO jobs(uid, name, source, run_type, scope, executor, synced_at)
		      VALUES(?, ?, 'cronomicon', ?, 'dmz-web', ?, 't')`, uid, name, runType, executor)
	}
	job("u-shell", "restart", "bash", nil)        // defaults to ssh → moves
	job("u-ssh", "legacy", "bash", "ssh")         // asks for ssh → refused once bound
	job("u-runner", "pinned", "python", "runner") // already on a runner → unaffected
	job("u-play", "deploy", "ansible", nil)       // already runner by run type → unaffected
	// A binned job and a job on another scope play no part.
	exec(`INSERT INTO jobs(uid, name, source, run_type, scope, synced_at, deleted_at)
	      VALUES('u-binned','old','cronomicon','perl','dmz-web','t','t')`)
	exec(`INSERT INTO jobs(uid, name, source, run_type, scope, synced_at) VALUES('u-else','elsewhere','cronomicon','powershell','other','t')`)
	// restart binds a secret, so it needs an injection-flagged runner once it moves.
	exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, owner_uid, ref_kind, ref_name, created_at)
	      VALUES('job','cronomicon','restart','u-shell','secret','DB_PASSWORD','t')`)
	exec(`INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, created_at)
	      VALUES('run-ssh','restart','bash','dmz-web','queued','seed','manual','ssh','t')`)
	// Parked runs frozen onto ssh count too — they are promoted onto it later.
	// One parked for the runner, one workflow row with no params, and one with an
	// unreadable blob do not, and must not break the count.
	park := func(id, kind string, params any) {
		exec(`INSERT INTO pending_runs(id, kind, name, source, scope, run_at, scheduled_by, created_at, status, params_json)
		      VALUES(?, ?, 'restart', 'cronomicon', 'dmz-web', 't', 'ops@example', 't', 'pending', ?)`, id, kind, params)
	}
	park("p-ssh", "job", `{"Executor":"ssh"}`)
	park("p-runner", "job", `{"Executor":"runner"}`)
	park("p-wf", "workflow", nil)
	park("p-junk", "job", "not json")

	// r-full can run everything and may hold secrets; r-thin declares ansible but
	// the server-managed mask takes it away, and it may not hold secrets; r-out
	// is in no agency, so it is not eligible for this scope.
	exec(`INSERT INTO runners(id, name, status, capabilities, allow_secret_injection, registered_at, created_at)
	      VALUES('r-full','runner-full','online','["bash","python","ansible"]',1,'t','t')`)
	exec(`INSERT INTO runners(id, name, status, capabilities, managed_settings, registered_at, created_at)
	      VALUES('r-thin','runner-thin','online','["bash","ansible"]','{"capabilityMask":["ansible"]}','t','t')`)
	exec(`INSERT INTO runners(id, name, status, capabilities, registered_at, created_at)
	      VALUES('r-out','runner-out','online','["bash"]','t','t')`)
	exec(`INSERT INTO runner_agencies(runner_id, agency_id) VALUES('r-full','ag-fin'), ('r-thin','ag-fin')`)
	// A runner whose managed settings are not JSON (a hand-edited or restored
	// row): the mask cannot be read, so it is treated as no mask — not as a
	// reason to fail the preview for the whole scope.
	exec(`INSERT INTO runners(id, name, status, capabilities, managed_settings, registered_at, created_at)
	      VALUES('r-junk','runner-junk','online','["bash"]','not json','t','t')`)
	exec(`INSERT INTO runner_agencies(runner_id, agency_id) VALUES('r-junk','ag-fin')`)

	names := func(js []PreviewJob) string {
		out := ""
		for _, j := range js {
			out += j.Name + " "
		}
		return out
	}

	// The local runner serves Finance too, and legacy binds an SSH key.
	exec(`INSERT INTO runners(id, name, kind, status, capabilities, allow_secret_injection, registered_at, created_at)
	      VALUES('r-local','Local runner','server','online','["bash","perl","powershell","python"]',1,'t','t')`)
	exec(`INSERT INTO runner_agencies(runner_id, agency_id) VALUES('r-local','ag-fin')`)
	exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, owner_uid, ref_kind, ref_name, created_at)
	      VALUES('job','cronomicon','legacy','u-ssh','key','deploy_key','t')`)
	who := func(rs []PreviewRunnerRef) string {
		out := ""
		for _, r := range rs {
			out += r.Name + " "
		}
		return out
	}

	// Binding an open scope to two of the four runners that serve Finance: the
	// other two — the local runner among them — stop taking its runs.
	p, err := PreviewScopeRunners(ctx, pool, "s-dmz", []string{"r-full", "r-thin", "r-out", "r-nope"})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if p.CurrentlyBound || !p.WillBeBound {
		t.Errorf("bound state = %v → %v, want false → true", p.CurrentlyBound, p.WillBeBound)
	}
	if got := who(p.RunnersLosing); got != "Local runner runner-junk " {
		t.Errorf("runners losing the scope = %q, want the local runner and runner-junk", got)
	}
	if len(p.RunnersGaining) != 0 {
		t.Errorf("runners gaining the scope = %q, want none when binding an open scope", who(p.RunnersGaining))
	}
	for _, r := range p.RunnersLosing {
		if r.Local != (r.RunnerID == "r-local") {
			t.Errorf("%s: local = %v", r.Name, r.Local)
		}
	}
	// An agent is among the runners bound, so the key-bound job has someone to
	// deliver its key.
	if len(p.JobsNeedingAgent) != 0 {
		t.Errorf("jobs needing an agent = %q, want none with agents bound", names(p.JobsNeedingAgent))
	}
	// Every job of the scope sets a requirement: there is one executor, and the
	// job's own `executor` is not read.
	if got := strings.Join(p.RunTypes, ","); got != "ansible,bash,python" {
		t.Errorf("run types = %q, want ansible,bash,python", got)
	}
	if p.JobsNeedingInjection != 2 {
		t.Errorf("jobs needing injection = %d, want 2 (restart's secret, legacy's key)", p.JobsNeedingInjection)
	}
	byID := map[string]PreviewRunner{}
	for _, r := range p.Runners {
		byID[r.RunnerID] = r
	}
	if r := byID["r-full"]; !r.Registered || !r.Eligible || len(r.MissingRunTypes) != 0 || !r.AllowsSecretInjection {
		t.Errorf("r-full = %+v, want registered, eligible, nothing missing, injection allowed", r)
	}
	if r := byID["r-thin"]; strings.Join(r.MissingRunTypes, ",") != "ansible,python" || r.AllowsSecretInjection {
		t.Errorf("r-thin = %+v, want ansible (masked) and python missing, no injection", r)
	}
	if r := byID["r-out"]; !r.Registered || r.Eligible {
		t.Errorf("r-out = %+v, want registered but not eligible", r)
	}
	if r := byID["r-nope"]; r.Registered || r.Eligible {
		t.Errorf("r-nope = %+v, want unregistered", r)
	}
	var rows int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM scope_runners`).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("binding rows after a preview = %d (err %v), want 0 — a preview writes nothing", rows, err)
	}

	// A runner whose managed settings are not JSON is still previewed, with its
	// declared capabilities.
	p, err = PreviewScopeRunners(ctx, pool, "s-dmz", []string{"r-junk"})
	if err != nil {
		t.Fatalf("preview junk: %v", err)
	}
	if r := p.Runners[0]; !r.Registered || strings.Join(r.Capabilities, ",") != "bash" {
		t.Errorf("r-junk = %+v, want registered with its declared capabilities", r)
	}

	// Binding the scope to the local runner ALONE: no agent serves it any more,
	// and the job that binds an SSH key would be refused (LR-47).
	p, err = PreviewScopeRunners(ctx, pool, "s-dmz", []string{"r-local"})
	if err != nil {
		t.Fatalf("preview local: %v", err)
	}
	if got := names(p.JobsNeedingAgent); got != "legacy " {
		t.Errorf("jobs needing an agent with only the local runner bound = %q, want legacy", got)
	}
	if got := who(p.RunnersLosing); got != "runner-full runner-junk runner-thin " {
		t.Errorf("runners losing the scope = %q, want the three agents", got)
	}
	// It has no known_hosts of its own to be missing a key from (until its keys
	// move to the ledger, it verifies against the keys kept with the SSH targets).
	if r := p.Runners[0]; !r.Local || r.HostsWithoutKey != 0 {
		t.Errorf("the local runner in the preview = %+v, want local and no missing host keys", r)
	}

	// Clearing a bound scope: every runner that serves Finance takes its runs
	// again.
	exec(`INSERT INTO scope_runners(scope_id, runner_id, runner_name, bound_at) VALUES('s-dmz','r-full','runner-full','t')`)
	p, err = PreviewScopeRunners(ctx, pool, "s-dmz", []string{})
	if err != nil {
		t.Fatalf("preview clear: %v", err)
	}
	if !p.CurrentlyBound || p.WillBeBound {
		t.Errorf("bound state = %v → %v, want true → false", p.CurrentlyBound, p.WillBeBound)
	}
	if got := who(p.RunnersGaining); got != "Local runner runner-junk runner-thin " {
		t.Errorf("runners gaining the scope on clear = %q, want everyone in Finance but the one already bound", got)
	}
	if len(p.RunnersLosing) != 0 || len(p.JobsNeedingAgent) != 0 {
		t.Errorf("on clear: losing %q, needing an agent %q; want neither", who(p.RunnersLosing), names(p.JobsNeedingAgent))
	}

	// Swapping one runner for another on a bound scope: one out, one in.
	p, err = PreviewScopeRunners(ctx, pool, "s-dmz", []string{"r-thin"})
	if err != nil {
		t.Fatalf("preview swap: %v", err)
	}
	if who(p.RunnersLosing) != "runner-full " || who(p.RunnersGaining) != "runner-thin " {
		t.Errorf("on swap: losing %q, gaining %q; want runner-full out and runner-thin in",
			who(p.RunnersLosing), who(p.RunnersGaining))
	}

	if p, err := PreviewScopeRunners(ctx, pool, "no-such-scope", nil); err != nil || p != nil {
		t.Errorf("unknown scope = %+v, %v; want nil, nil", p, err)
	}
}

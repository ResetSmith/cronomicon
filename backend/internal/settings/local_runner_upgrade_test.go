package settings

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/notices"
)

// upgradeFixture is an installation as 2.2 left it: two agencies, scopes whose
// shell jobs ran from the server, on agents, or both, and two runs still queued
// for the SSH executor.
func upgradeFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-fin','Finance','t'), ('ag-tax','Tax','t')`)
	scope := func(id, name, agency string) {
		exec(`INSERT INTO scopes(id, name, source, created_at) VALUES(?, ?, 'cronomicon', 't')`, id, name)
		if agency != "global" {
			exec(`DELETE FROM scope_agencies WHERE scope_id = ?`, id)
			exec(`INSERT INTO scope_agencies(scope_id, agency_id) VALUES(?, ?)`, id, agency)
		}
	}
	job := func(name, runType string, scope, executor any) {
		exec(`INSERT INTO jobs(uid, name, source, run_type, scope, executor, synced_at)
		      VALUES('u-'||?, ?, 'cronomicon', ?, ?, ?, 't')`, name, name, runType, scope, executor)
	}
	scope("s-fin", "fin-web", "ag-fin")         // shell jobs ran from the server
	scope("s-tax", "tax-web", "ag-tax")         // one from the server, one on an agent
	scope("s-agents", "shared-tools", "global") // shell jobs ran on agents
	scope("s-bound", "fin-dmz", "ag-fin")       // bound: not looked at
	scope("s-play", "fin-plays", "ag-fin")      // no shell job at all
	job("restart", "bash", "fin-web", nil)
	job("rotate", "python", "fin-web", "ssh")
	job("ledger", "bash", "tax-web", nil)
	job("export", "bash", "tax-web", "runner")
	job("lint", "bash", "shared-tools", "runner")
	job("dmz", "bash", "fin-dmz", nil)
	job("play", "ansible", "fin-plays", nil)
	job("loose", "bash", nil, nil) // no scope: Global's, from the server
	exec(`INSERT INTO jobs(uid, name, source, run_type, scope, synced_at, deleted_at)
	      VALUES('u-binned','binned','cronomicon','bash','fin-plays','t','t')`)
	// Agents: Finance's, Tax's and Global's, each with a shell capability.
	for _, r := range [][2]string{{"r-fin", "ag-fin"}, {"r-tax", "ag-tax"}, {"r-glob", "global"}} {
		exec(`INSERT INTO runners(id, name, status, capabilities, registered_at, created_at, owner_agency)
		      VALUES(?, ?, 'online', '["bash","ansible"]', 't', 't', ?)`, r[0], r[0], r[1])
	}
	exec(`INSERT INTO scope_runners(scope_id, runner_id, runner_name, bound_at) VALUES('s-bound','r-fin','r-fin','t')`)
	for _, id := range []string{"q1", "q2"} {
		exec(`INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, agencies_json, created_at)
		      VALUES(?, 'restart', 'bash', 'fin-web', 'queued', 'seed', 'manual', 'ssh', '["Finance"]', 't')`, id)
	}
	exec(`INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, created_at)
	      VALUES('done', 'restart', 'bash', 'fin-web', 'success', 'seed', 'manual', 'ssh', 't')`)
	localID, _, err := EnsureLocalRunner(context.Background(), pool, true, 4)
	if err != nil {
		t.Fatal(err)
	}
	return pool, localID
}

func openNotices(t *testing.T, pool *sql.DB) map[string]notices.Notice {
	t.Helper()
	if err := notices.RunChecks(context.Background(), pool); err != nil {
		t.Fatalf("checks: %v", err)
	}
	all, err := notices.ListOpen(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]notices.Notice{}
	for _, n := range all {
		out[n.Kind+"/"+n.Subject] = n
	}
	return out
}

// The SSH executor was on: the local runner is put where the server was
// serving, how each scope was served is recorded, the queued rows move to the
// runner executor — and the inbox says where a job may now run somewhere it
// did not, for as long as that is true.
func TestLocalRunnerUpgradePassWhenTheSSHExecutorWasOn(t *testing.T) {
	pool, localID := upgradeFixture(t)
	ctx := context.Background()

	res, err := RunLocalRunnerUpgradePass(ctx, pool, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ran || !res.WasOn {
		t.Fatalf("result = %+v, want it to have run with the executor on", res)
	}
	// Placed in Finance and Tax (the server ran their shell jobs); Global it
	// served already, so the jobs with no scope need nothing.
	if !slices.Equal(res.PlacedAgencies, []string{"Finance", "Tax"}) {
		t.Errorf("placed in %v, want Finance and Tax", res.PlacedAgencies)
	}
	if got := serveList(t, pool, localID); !slices.Equal(got, []string{"ag-fin", "ag-tax", "global"}) {
		t.Errorf("the local runner serves %v, want Finance, Tax and Global", got)
	}
	// The two rows that were waiting are the runner executor's now; history is
	// left as it was.
	if res.Requeued != 2 || res.Skipped != 0 {
		t.Errorf("requeued %d, skipped %d; want 2 and 0", res.Requeued, res.Skipped)
	}
	var queuedSSH, doneSSH int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE status = 'queued' AND executor = 'ssh'`).Scan(&queuedSSH)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE status = 'success' AND executor = 'ssh'`).Scan(&doneSSH)
	if queuedSSH != 0 || doneSSH != 1 {
		t.Errorf("queued ssh rows = %d, finished ssh rows = %d; want 0 and the 1 that is history", queuedSSH, doneSSH)
	}
	rec, err := notices.ReadLocalRunnerUpgrade(ctx, pool)
	if err != nil || rec == nil {
		t.Fatalf("the record: %+v, %v", rec, err)
	}
	want := map[string]string{
		"s-fin": notices.ServedByServer, "s-tax": notices.ServedByBoth, "s-agents": notices.ServedByAgents,
		notices.NoScopeSubject: notices.ServedByServer,
	}
	if len(rec.Scopes) != len(want) {
		t.Errorf("recorded scopes = %v, want exactly %v (not the bound scope, not the one with no shell job)", rec.Scopes, want)
	}
	for id, how := range want {
		if rec.Scopes[id] != how {
			t.Errorf("scope %s recorded as %q, want %q", id, rec.Scopes[id], how)
		}
	}
	// Each placement is in the activity feed.
	var placements int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM activity WHERE actor = 'upgrade' AND runner_name = ?`, LocalRunnerName).Scan(&placements)
	if placements != 2 {
		t.Errorf("activity rows for the placements = %d, want 2", placements)
	}

	// It runs once.
	again, err := RunLocalRunnerUpgradePass(ctx, pool, nil, true)
	if err != nil || again.Ran {
		t.Errorf("a second pass = %+v, %v; want it to do nothing", again, err)
	}

	open := openNotices(t, pool)
	for key, agency := range map[string]string{
		"may_run_on_agent/s-fin":                     "ag-fin", // ran from the server; Finance's agent can take it now
		"mixed_scope/s-tax":                          "ag-tax",
		"may_run_on_server/s-agents":                 "global", // ran on agents; the local runner serves Global
		"may_run_on_agent/" + notices.NoScopeSubject: "global",
		"agency_placed/ag-fin":                       "global",
		"agency_placed/ag-tax":                       "global",
	} {
		n, ok := open[key]
		if !ok {
			t.Errorf("no open notice %s (open: %v)", key, keysOf(open))
			continue
		}
		if n.AgencyID != agency {
			t.Errorf("%s is filed under %q, want %q", key, n.AgencyID, agency)
		}
	}
	for key := range open {
		if strings.HasSuffix(key, "/s-bound") || strings.HasSuffix(key, "/s-play") {
			t.Errorf("a notice about a scope the pass does not look at: %s", key)
		}
	}
	if d := open["may_run_on_agent/s-fin"].Detail; !strings.Contains(d, "fin-web") || !strings.Contains(d, "r-fin") ||
		!strings.Contains(d, "restart") || !strings.Contains(d, "known_hosts") || !strings.Contains(d, "bind the scope") {
		t.Errorf("the notice must name the scope, the agent, the jobs, what differs and the remedy: %q", d)
	}

	// The remedy resolves it: bind the scope, and where its jobs run is decided.
	if _, err := pool.Exec(`INSERT INTO scope_runners(scope_id, runner_id, runner_name, bound_at) VALUES('s-fin','r-fin','r-fin','t')`); err != nil {
		t.Fatal(err)
	}
	// And so does the other runner going away: no agent serves Tax any more.
	if _, err := pool.Exec(`DELETE FROM runners WHERE id = 'r-tax'`); err != nil {
		t.Fatal(err)
	}
	// And taking Tax off the local runner's list settles its placement notice.
	if _, err := pool.Exec(`DELETE FROM runner_agencies WHERE runner_id = ? AND agency_id = 'ag-tax'`, localID); err != nil {
		t.Fatal(err)
	}
	open = openNotices(t, pool)
	for _, key := range []string{"may_run_on_agent/s-fin", "mixed_scope/s-tax", "agency_placed/ag-tax"} {
		if _, still := open[key]; still {
			t.Errorf("%s is still open after its cause went away", key)
		}
	}
	if _, ok := open["agency_placed/ag-fin"]; !ok {
		t.Error("agency_placed/ag-fin was resolved although the local runner still serves Finance")
	}
}

// The SSH executor was off: the rows queued for it were never going to run.
// They are closed, with the reason; nothing is placed and nothing is reported.
func TestLocalRunnerUpgradePassWhenTheSSHExecutorWasOff(t *testing.T) {
	pool, localID := upgradeFixture(t)
	ctx := context.Background()

	res, err := RunLocalRunnerUpgradePass(ctx, pool, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ran || res.WasOn || len(res.PlacedAgencies) != 0 || res.Requeued != 0 || res.Skipped != 2 {
		t.Errorf("result = %+v, want run, off, nothing placed, 2 rows closed", res)
	}
	if got := serveList(t, pool, localID); !slices.Equal(got, []string{"global"}) {
		t.Errorf("the local runner serves %v, want Global only", got)
	}
	var status, reason string
	if err := pool.QueryRow(`SELECT status, COALESCE(queued_reason, '') FROM runs WHERE id = 'q1'`).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "skipped" || !strings.HasPrefix(reason, "Skipped: ") || !strings.Contains(reason, "SSH executor") {
		t.Errorf("a row queued for an executor that was off = %q / %q, want skipped with the reason", status, reason)
	}
	for key := range openNotices(t, pool) {
		for _, kind := range []string{"may_run_on_agent", "mixed_scope", "may_run_on_server", "agency_placed"} {
			if strings.HasPrefix(key, kind+"/") {
				t.Errorf("a placement notice with the executor off: %s", key)
			}
		}
	}
	// It does not run again, even if the local runner is turned on later.
	if again, err := RunLocalRunnerUpgradePass(ctx, pool, nil, true); err != nil || again.Ran {
		t.Errorf("a second pass = %+v, %v; want it to do nothing", again, err)
	}
}

// The 2.2 precedence, frozen: with a global default of "runner", a shell job
// that set no executor ran on agents, not from the server.
func TestLocalRunnerUpgradePassReadsTheOldGlobalDefault(t *testing.T) {
	pool, localID := upgradeFixture(t)
	ctx := context.Background()
	if _, err := pool.Exec(`INSERT INTO settings(key, value) VALUES('defaultExecutor', 'runner')`); err != nil {
		t.Fatal(err)
	}
	res, err := RunLocalRunnerUpgradePass(ctx, pool, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	// Only "rotate" (fin-web) said ssh itself: Finance is placed, Tax is not.
	if !slices.Equal(res.PlacedAgencies, []string{"Finance"}) {
		t.Errorf("placed in %v, want Finance only", res.PlacedAgencies)
	}
	rec, _ := notices.ReadLocalRunnerUpgrade(ctx, pool)
	if rec.Scopes["s-fin"] != notices.ServedByBoth || rec.Scopes["s-tax"] != notices.ServedByAgents ||
		rec.Scopes[notices.NoScopeSubject] != notices.ServedByAgents {
		t.Errorf("recorded %v; want fin-web both, tax-web agents, no-scope agents", rec.Scopes)
	}
	if got := serveList(t, pool, localID); !slices.Equal(got, []string{"ag-fin", "global"}) {
		t.Errorf("the local runner serves %v, want Finance and Global", got)
	}
}

// A shell job that declares requirements ran until 2.3.0 (the SSH executor did
// not read them). It is reported while no registered agent that serves it
// advertises them, and not once one does.
func TestShellJobRequiresNotice(t *testing.T) {
	pool, _ := upgradeFixture(t)
	if _, err := pool.Exec(`UPDATE jobs SET requires_json = '["vault"]' WHERE name IN ('restart', 'play')`); err != nil {
		t.Fatal(err)
	}
	open := openNotices(t, pool)
	n, ok := open["shell_job_requires/u-restart"]
	if !ok {
		t.Fatalf("no notice for the shell job that requires vault (open: %v)", keysOf(open))
	}
	if n.AgencyID != "ag-fin" || !strings.Contains(n.Detail, "vault") || !strings.Contains(n.Detail, "fin-web") {
		t.Errorf("notice = %+v, want Finance's, naming the requirement and the scope", n)
	}
	if _, ok := open["shell_job_requires/u-play"]; ok {
		t.Error("an ansible job's requirements were reported: they were always read")
	}
	// Finance's agent gets vault: the job has someone to wait for.
	if _, err := pool.Exec(`UPDATE runners SET capabilities = '["bash","vault"]' WHERE id = 'r-fin'`); err != nil {
		t.Fatal(err)
	}
	if _, ok := openNotices(t, pool)["shell_job_requires/u-restart"]; ok {
		t.Error("still reported although an agent that serves it advertises vault")
	}
}

func keysOf(m map[string]notices.Notice) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// What the upgrade pass acts on is what the SSH executor WAS, recorded when
// the switch was seeded — not what the switch says by the time the pass runs.
func TestSSHExecutorWasOnIsTheSeedNotTheSwitch(t *testing.T) {
	for _, seed := range []bool{true, false} {
		pool, err := db.Open(filepath.Join(t.TempDir(), "seed.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(pool); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if _, _, err := EnsureLocalRunner(ctx, pool, seed, 4); err != nil {
			t.Fatal(err)
		}
		// An operator flips the switch before the pass has run.
		flipped := !seed
		if _, err := SetLocalRunner(ctx, pool, false, &flipped, nil, "root@example.com"); err != nil {
			t.Fatal(err)
		}
		// A later boot with the old variable changed does not re-seed either.
		if _, _, err := EnsureLocalRunner(ctx, pool, !seed, 4); err != nil {
			t.Fatal(err)
		}
		if got, err := SSHExecutorWasOn(ctx, pool); err != nil || got != seed {
			t.Errorf("seeded %v, switch flipped: SSHExecutorWasOn = %v, %v; want %v", seed, got, err, seed)
		}
		_ = pool.Close()
	}
}

// Until 2.3.0 the server ran every agency's shell jobs. An agency whose shell
// jobs nobody can run now — no agent of its own, not on the local runner's
// list — is told so, whenever that becomes true.
func TestNoRunnerForShellJobsNotice(t *testing.T) {
	pool, localID := upgradeFixture(t)
	const key = "no_runner_for_shell_jobs/ag-tax"
	// Every agency has an agent with bash: nothing to report.
	if _, ok := openNotices(t, pool)[key]; ok {
		t.Fatal("reported although Tax has an agent")
	}
	// Tax's agent goes, and the local runner serves only Global.
	if _, err := pool.Exec(`DELETE FROM runners WHERE id = 'r-tax'`); err != nil {
		t.Fatal(err)
	}
	open := openNotices(t, pool)
	n, ok := open[key]
	if !ok {
		t.Fatalf("no notice for Tax, whose shell jobs have no runner (open: %v)", keysOf(open))
	}
	if n.AgencyID != "ag-tax" || !strings.Contains(n.Detail, "export") || !strings.Contains(n.Detail, "ledger") ||
		!strings.Contains(n.Detail, "Settings → Local runner") {
		t.Errorf("notice = %+v, want Tax's, naming its shell jobs and both ways out", n)
	}
	if _, ok := open["no_runner_for_shell_jobs/ag-fin"]; ok {
		t.Error("Finance was reported although its agent is registered")
	}
	// A global administrator puts Tax on the local runner's list.
	if _, err := pool.Exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES (?, 'ag-tax')`, localID); err != nil {
		t.Fatal(err)
	}
	if _, ok := openNotices(t, pool)[key]; ok {
		t.Error("still reported although the local runner serves Tax now")
	}
}

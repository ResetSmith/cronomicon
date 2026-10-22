package db_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/notices"
	"github.com/ResetSmith/cronomicon/internal/preflight"
	"github.com/ResetSmith/cronomicon/internal/runcap"
	"github.com/ResetSmith/cronomicon/internal/scheduler"
)

// The upgrade from 2.2.3 (schema 1210) to 2.5.0 in one step, over a database
// that looks like a real 2.2.3 installation: two agencies, Git and in-app
// scopes with inventories, runners of one agency, of two and of none, secrets,
// variables, keys, host records and a bastion with captured keys, scripts and
// reference bindings, Git and in-app jobs, two schedules of one name, a global
// calendar, an in-app workflow, run history, queued runs for the SSH executor,
// activity, the GitLab row, push targets, an alert rule and grants (2.5.0 C11,
// from the pre-release review's harness).
//
// It pins what the changelog's "Upgrading from 2.2.3" and `cronomicon
// preflight` say: nothing is lost, the queued SSH-executor runs are closed
// (1590, present defect 80) so that they block nothing, and what stops or moves
// is listed in the notices.

const ts223 = "2026-09-01T00:00:00Z"

func seed223(t *testing.T, pool *sql.DB) {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-ops','Ops',?),('ag-web','Web',?)`, ts223, ts223)
	for _, s := range [][3]string{{"sc-ops", "ops", "git"}, {"sc-web", "web", "cronomicon"}, {"sc-shared", "shared", "git"}, {"sc-free", "free", "git"}} {
		exec(`INSERT INTO scopes (id, name, source, created_at, source_path) VALUES (?,?,?,?,?)`, s[0], s[1], s[2], ts223, "scopes/"+s[1]+".yaml")
		exec(`INSERT INTO scope_hosts VALUES (?, 'h1.'||?), (?, 'h2.'||?)`, s[0], s[1], s[0], s[1])
		exec(`INSERT INTO scope_groups VALUES (?, 'app')`, s[0])
		exec(`INSERT INTO scope_group_hosts VALUES (?, 'app', 'h1.'||?)`, s[0], s[1])
		exec(`INSERT INTO scope_group_vars VALUES (?, 'app', 'port', '80')`, s[0])
		exec(`INSERT INTO scope_host_vars VALUES (?, 'h1.'||?, 'x', '1')`, s[0], s[1])
	}
	exec(`INSERT INTO scope_agencies VALUES ('sc-ops','ag-ops'),('sc-web','ag-web'),('sc-shared','ag-ops'),('sc-shared','ag-web')`)
	for _, r := range []string{"r-ops", "r-web", "r-both", "r-pool"} {
		exec(`INSERT INTO runners (id, name, status, os, capabilities, load, max_concurrent, version, protocol_version, registered_at, created_at)
		      VALUES (?, ?, 'online', 'Linux', '["bash","ansible"]', 0, 5, '2.2.3', 14, ?, ?)`, r, r, ts223, ts223)
	}
	exec(`INSERT INTO runner_agencies VALUES ('r-ops','ag-ops'),('r-web','ag-web'),('r-both','ag-ops'),('r-both','ag-web')`)
	exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_by, bound_at) VALUES ('sc-ops','r-ops','r-ops','a',?)`, ts223)
	exec(`INSERT INTO secrets (id, key, scope, source, created_at) VALUES ('se-ops','DB_PW',NULL,'stored',?),('se-shared','API',NULL,'stored',?),('se-free','TOKEN',NULL,'stored',?)`, ts223, ts223, ts223)
	exec(`INSERT INTO secret_agencies VALUES ('se-ops','ag-ops'),('se-shared','ag-ops'),('se-shared','ag-web')`)
	exec(`INSERT INTO env_vars (id, key, scope, value, created_at) VALUES ('ev-web','REGION','web','us',?)`, ts223)
	exec(`INSERT INTO env_var_agencies VALUES ('ev-web','ag-web')`)
	exec(`INSERT INTO ssh_credentials (id, label, source, created_at) VALUES ('k-ops','opskey','stored',?)`, ts223)
	exec(`INSERT INTO ssh_credential_agencies VALUES ('k-ops','ag-ops')`)
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	exec(`INSERT INTO bastions (id, hostname, port, name, address, status, created_at, host_key) VALUES ('b1','jump',22,'jump','10.0.0.1','ok',?,?)`, ts223, key)
	exec(`INSERT INTO ssh_hosts (id, hostname, port, address, status, source, created_at, host_key, bastion_id) VALUES ('hh1','h1.ops',22,'10.0.1.1','ok','cronomicon',?,?,'b1')`, ts223, key)
	exec(`INSERT INTO ssh_hosts (id, hostname, port, address, status, source, scope_id, created_at) VALUES ('hh2','h2.ops',22,'10.0.1.2','ok','git','sc-ops',?)`, ts223)
	exec(`INSERT INTO scripts (name, run_type, script, content_hash, synced_at) VALUES ('deploy.sh','bash','echo hi','h1',?)`, ts223)
	exec(`INSERT INTO reference_bindings (owner_kind, owner_source, owner_name, ref_kind, ref_name, alias, created_by, created_at) VALUES ('script','git','deploy.sh','secret','DB_PW','DB_PW','a',?)`, ts223)
	for _, x := range [][6]string{
		{"j-ops", "backup", "git", "ops", "bash", ""},
		{"j-web", "rotate", "cronomicon", "web", "bash", ""},
		{"j-shared", "patch", "git", "shared", "ansible", ""},
		{"j-free", "cleanup", "git", "", "bash", ""},
		{"j-script", "deployer", "git", "ops", "bash", "deploy.sh"},
		{"j-webgit", "webcheck", "git", "web", "bash", ""},
	} {
		exec(`INSERT INTO jobs (uid, name, source, run_type, scope, command, script_ref, created_at) VALUES (?,?,?,?,NULLIF(?,''),'echo',NULLIF(?,''),?)`, x[0], x[1], x[2], x[4], x[3], x[5], ts223)
		exec(`INSERT INTO reference_bindings (owner_kind, owner_source, owner_name, ref_kind, ref_name, alias, created_by, created_at, owner_uid) VALUES ('job',?,?,'secret','DB_PW','DB_PW','a',?,?)`, x[2], x[1], ts223, x[0])
	}
	// A Forbid job, whose concurrency key a queued run holds.
	exec(`INSERT INTO jobs (uid, name, source, run_type, command, created_at, concurrency_policy, concurrency_key) VALUES ('j-nc','nightly-clean','git','bash','echo',?,'Forbid','nightly-clean')`, ts223)
	exec(`INSERT INTO schedules (uid, name, source, cron, content_hash, skip_calendars) VALUES ('s-git','nightly','git','0 2 * * *','x','["holidays"]'),('s-app','nightly','cronomicon','0 3 * * *','y',NULL)`)
	exec(`INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, schedule_uid) VALUES ('git','job','backup','nightly','0 2 * * *',0,'nightly','j-ops','s-git')`)
	exec(`INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, skip_calendars) VALUES ('cronomicon','job','rotate','nightly','0 3 * * *',0,'nightly','j-web','["holidays"]')`)
	exec(`INSERT INTO calendars (source, name, global, created_at) VALUES ('cronomicon','freeze',1,?),('cronomicon','holidays',0,?)`, ts223, ts223)
	exec(`INSERT INTO calendar_days (calendar_source, calendar_name, day) VALUES ('cronomicon','freeze','2026-12-24'),('cronomicon','holidays','2026-12-25')`)
	exec(`INSERT INTO workflows (uid, name, source, steps, created_at) VALUES ('w-web','webflow','cronomicon','[{"id":"a","type":"job","name":"rotate"}]',?)`, ts223)
	run := func(id, job, juid, src, scope, status, ag, executor, key string) {
		exec(`INSERT INTO runs (id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, created_at, agencies_json, job_uid, job_source, started_at, concurrency_key)
		      VALUES (?,?,'bash',NULLIF(?,''),?,'scheduler','scheduled',?,?,?,?,?,?,NULLIF(?,''))`, id, job, scope, status, executor, ts223, ag, juid, src, ts223, key)
	}
	run("ru-1", "backup", "j-ops", "git", "ops", "success", `["Ops"]`, "runner", "")
	run("ru-2", "rotate", "j-web", "cronomicon", "web", "failure", `["Web"]`, "ssh", "")
	run("ru-3", "patch", "j-shared", "git", "shared", "success", `["Ops","Web"]`, "runner", "")
	run("ru-4", "webcheck", "j-webgit", "git", "web", "success", `["Web"]`, "ssh", "")
	// Queued for the SSH executor when 2.2.3 stopped: the Forbid job's, and as
	// many of Global's as the default cap.
	run("q-forbid", "nightly-clean", "j-nc", "git", "", "queued", `[]`, "ssh", "nightly-clean")
	for i := 0; i < 6; i++ {
		run(fmt.Sprintf("q-%d", i), "cleanup", "j-free", "git", "", "queued", `[]`, "ssh", "")
	}
	exec(`INSERT INTO run_agencies VALUES ('ru-1','Ops'),('ru-2','Web'),('ru-4','Web')`)
	exec(`INSERT INTO activity (kind, outcome, job_name, trace_id, at, created_at, job_uid) VALUES ('run-end','success','webcheck','ru-4',?,?,'j-webgit')`, ts223, ts223)
	exec(`INSERT INTO change_log (at, actor, category, action, target, created_at) VALUES (?, 'a', 'Secrets', 'updated', 'DB_PW', ?)`, ts223, ts223)
	exec(`INSERT INTO gitlab_config (id, branch, write_branch, repo_url, pat_enc, webhook_enabled, webhook_events_push) VALUES (1,'main','ops-main','https://gitlab.example/x.git','enc',0,0)`)
	exec(`INSERT INTO git_sync_state (id, last_sha, last_synced_at, last_status) VALUES (1,'abc',?,'ok')`, ts223)
	exec(`INSERT OR REPLACE INTO notification_config (id, apprise_targets, provider, smtp_encryption, smtp_recipients, apprise_enabled) VALUES (1, '[{"label":"slack","service":"slack","url":"slack://x","enabled":true}]', 'smtp', 'starttls', '[]', 1)`)
	exec(`INSERT INTO alert_config (id, enabled, created_at, target_mode, trigger, channels, job_name) VALUES ('al-1',1,?,'job','failure','["apprise"]','webcheck')`, ts223)
	exec(`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at) VALUES ('g1','ops-admins','admin','ag-ops',0,?),('g0','root','admin',NULL,1,?)`, ts223, ts223)
	exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('defaultExecutor','ssh')`)
}

var tables223 = []string{"scopes", "scope_hosts", "scope_groups", "scope_group_hosts", "scope_group_vars", "scope_host_vars",
	"scope_runners", "runners", "secrets", "env_vars", "ssh_credentials", "ssh_hosts", "bastions", "scripts", "reference_bindings",
	"jobs", "schedules", "definition_schedules", "calendar_days", "workflows", "runs", "activity", "change_log", "alert_config", "access_grants"}

func counts223(t *testing.T, pool *sql.DB) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, tb := range tables223 {
		var n int
		if err := pool.QueryRow(`SELECT COUNT(*) FROM ` + tb).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tb, err)
		}
		out[tb] = n
	}
	return out
}

func open223(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "223.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.MigrateTo(pool, 1210); err != nil {
		t.Fatalf("to 1210: %v", err)
	}
	seed223(t, pool)
	return pool
}

func TestC11_TheUpgradeFrom223(t *testing.T) {
	pool := open223(t)
	ctx := context.Background()
	before := counts223(t, pool)
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v, dirty, err := db.Status(pool); err != nil || dirty || v != 1640 {
		t.Errorf("schema %d dirty %v (%v), want 1640 clean", v, dirty, err)
	}
	var fk int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&fk); err != nil || fk != 0 {
		t.Errorf("foreign_key_check: %d row(s) (%v)", fk, err)
	}
	after := counts223(t, pool)
	for _, tb := range tables223 {
		if after[tb] < before[tb] {
			t.Errorf("%s: %d row(s) before, %d after", tb, before[tb], after[tb])
		}
	}

	// 1590: the queued SSH-executor runs are closed, with the reason, and block
	// neither Global's cap nor the Forbid job's key.
	var queued, closed int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE status = 'queued'`).Scan(&queued)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE status = 'skipped' AND queued_reason LIKE 'Skipped at the upgrade to 2.5.0:%'`).Scan(&closed)
	if queued != 0 || closed != 7 {
		t.Errorf("after the upgrade: %d run(s) queued, %d closed by 1590; want 0 and 7", queued, closed)
	}
	if at := runcap.AtProduction(ctx, pool, "global"); at.Reached {
		t.Errorf("Global's cap is reached after the upgrade (%+v): the queued SSH-executor runs still count", at)
	}
	if blocked, err := scheduler.CheckForbid(ctx, pool, "nightly-clean"); err != nil || blocked {
		t.Errorf("the Forbid job is blocked after the upgrade (%v, %v): a queued SSH-executor run still holds its key", blocked, err)
	}

	// What the changelog and preflight say moves, as it moves.
	for what, q := range map[string]string{
		"Ops's Git scope becomes Ops's own, in the app (1470)": `SELECT COUNT(*) FROM scopes WHERE id = 'sc-ops' AND source = 'cronomicon' AND owner_agency = 'ag-ops' AND released_from = 'global'`,
		"its hosts are kept":                                           `SELECT COUNT(*) = 2 FROM scope_hosts WHERE scope_id = 'sc-ops'`,
		"the shared scope is Global's":                                 `SELECT COUNT(*) FROM scopes WHERE id = 'sc-shared' AND owner_agency = 'global'`,
		"the hand-written host record is Global's":                     `SELECT COUNT(*) FROM ssh_hosts WHERE id = 'hh1' AND owner_agency = 'global'`,
		"each agency has its copy of Global's global calendar (1430)":  `SELECT COUNT(*) = 2 FROM calendars WHERE name = 'freeze' AND owner_agency IN ('ag-ops','ag-web') AND global = 1`,
		"Ops's Git run history is Global's (documented, question 104)": `SELECT COUNT(*) FROM runs WHERE id = 'ru-1' AND owner_agency = 'global'`,
		"Web's in-app job's history stays Web's":                       `SELECT COUNT(*) FROM runs WHERE id = 'ru-2' AND owner_agency = 'ag-web'`,
		"the GitLab row is Global's repository, with its write branch": `SELECT COUNT(*) FROM git_repos WHERE id = 'global' AND branch = 'ops-main' AND url = 'https://gitlab.example/x.git'`,
		"the push target is Global's, and the rule names it (1540)":    `SELECT COUNT(*) FROM alert_rule_targets rt JOIN alert_targets t ON t.id = rt.target_id WHERE rt.rule_id = 'al-1' AND t.owner_agency = 'global'`,
		"the captured keys are carried for the local runner":           `SELECT COUNT(*) = 2 FROM carried_server_host_keys`,
		"the legacy runner is Global's and still lists both agencies":  `SELECT COUNT(*) = 2 FROM runner_agencies ra JOIN runners r ON r.id = ra.runner_id WHERE r.id = 'r-both' AND r.owner_agency = 'global'`,
	} {
		var ok bool
		if err := pool.QueryRow(q).Scan(&ok); err != nil || !ok {
			t.Errorf("%s: no (%v)", what, err)
		}
	}

	// And the notices list what needs a person.
	if err := notices.RunChecks(ctx, pool); err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	var open []string
	rows, err := pool.Query(`SELECT kind || ':' || subject FROM notices WHERE resolved_at IS NULL ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		open = append(open, s)
	}
	rows.Close()
	got := strings.Join(open, " ")
	for _, want := range []string{
		"agency_no_repository:ag-ops", "agency_no_repository:ag-web",
		"global_job_on_agency_scope:j-ops", "global_job_on_agency_scope:j-script", "global_job_on_agency_scope:j-webgit",
		"legacy_placement:r-both", "scope_several_agencies:sc-shared", "global_host_record:ag-ops:h1.ops",
		"grant_group_unlisted:g1", "shared_ownership:secret:se-shared",
	} {
		if !strings.Contains(" "+got+" ", " "+want+" ") {
			t.Errorf("no open notice %s after the upgrade; open: %s", want, got)
		}
	}
}

// `cronomicon preflight`, run against the same 2.2.3 database before the
// upgrade, says what the first start stops or moves (C11, present defect 81),
// and no longer that what it finds keeps working.
func TestC11_ThePreflightOf223SaysWhatStops(t *testing.T) {
	pool := open223(t)
	rep, err := preflight.Build230(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	var stopped []string
	for _, j := range rep.GitJobsOnAgencyScopes {
		stopped = append(stopped, j.Job+"/"+j.Scope+"/"+j.Detail)
	}
	if got := strings.Join(stopped, ","); got != "backup/ops/Ops,deployer/ops/Ops,webcheck/web/Web" {
		t.Errorf("Git jobs on an agency's scope = %s, want backup, deployer and webcheck (the notices the upgrade raises)", got)
	}
	if len(rep.AgencyGitScopes) != 1 || rep.AgencyGitScopes[0].Name != "ops" {
		t.Errorf("Git scopes of one agency = %+v, want ops (1470 makes it Ops's own)", rep.AgencyGitScopes)
	}
	if len(rep.GlobalHostRecords) != 1 || rep.GlobalHostRecords[0].Name != "h1.ops" || strings.Join(rep.GlobalHostRecords[0].Agencies, ",") != "Ops" {
		t.Errorf("hand-written records an agency uses = %+v, want h1.ops for Ops", rep.GlobalHostRecords)
	}
	if strings.Join(rep.GlobalCalendars, ",") != "freeze" || rep.QueuedSSHRuns != 7 || rep.Agencies != 2 || rep.AgencyGrants != 1 {
		t.Errorf("calendars %v, queued SSH runs %d, agencies %d, agency grants %d; want freeze, 7, 2, 1",
			rep.GlobalCalendars, rep.QueuedSSHRuns, rep.Agencies, rep.AgencyGrants)
	}
	runs := map[string]int{}
	for _, c := range rep.AgencyGitRuns {
		runs[c.Agency] = c.N
	}
	if runs["Ops"] != 1 || runs["Web"] != 1 || len(runs) != 2 {
		t.Errorf("Git run history per agency = %v, want Ops 1 and Web 1 (ru-1, ru-4)", runs)
	}
	var b strings.Builder
	on := true
	rep.Write(&b, &on)
	for _, want := range []string{"What stops or moves at the first start", "STOP RUNNING", "webcheck — scope web (Web)",
		"scope ops — Ops", "h1.ops — Ops", "Runs queued for the SSH executor (7)", "Ops — 1 run(s)"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, b.String())
		}
	}
}

// And back: the down migrations restore every row a 2.2.3 installation had.
func TestC11_TheUpgradeFrom223GoesBack(t *testing.T) {
	pool := open223(t)
	before := counts223(t, pool)
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.MigrateTo(pool, 1210); err != nil {
		t.Fatalf("down to 1210: %v", err)
	}
	after := counts223(t, pool)
	for _, tb := range tables223 {
		if after[tb] != before[tb] {
			t.Errorf("%s: %d row(s) before, %d after going back", tb, before[tb], after[tb])
		}
	}
	var hk, wb sql.NullString
	_ = pool.QueryRow(`SELECT host_key FROM ssh_hosts WHERE id = 'hh1'`).Scan(&hk)
	_ = pool.QueryRow(`SELECT write_branch FROM gitlab_config WHERE id = 1`).Scan(&wb)
	if hk.String == "" || wb.String != "ops-main" {
		t.Errorf("going back lost the host key (%q) or the write branch (%q)", hk.String, wb.String)
	}
}

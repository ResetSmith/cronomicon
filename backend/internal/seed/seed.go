// Package seed loads representative demo data into the database for local
// preview (CRONOMICON_DEV_SEED). It exists so the operator UI can be browsed with
// realistic content before GitLab/SSO/runners are wired up. It is NOT part
// of the production data path: GitLab remains the source of truth for job and
// workflow definitions (architecture §2.1); these rows merely populate the
// read-model cache and the operator-managed tables so every view renders.
package seed

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/inventory"
)

// allScopes aliases the A5 unrestricted sentinel so the seed cannot drift from
// the resolver's definition of it.
const allScopes = auth.AllScopes

// Seed inserts demo data when the database is empty. It is a no-op when the
// jobs cache already has rows, so it is safe to call on every boot and never
// clobbers a real (synced) database.
func Seed(ctx context.Context, database *sql.DB, log *slog.Logger) error {
	var jobCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&jobCount); err != nil {
		return fmt.Errorf("seed precheck: %w", err)
	}
	if jobCount > 0 {
		log.Info("demo seed skipped — database already has data", "jobs", jobCount)
		return nil
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("seed begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var execErr error
	exec := func(q string, args ...any) {
		if execErr != nil {
			return
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			snippet := q
			if len(snippet) > 70 {
				snippet = snippet[:70]
			}
			execErr = fmt.Errorf("seed exec: %w (near: %s)", err, snippet)
		}
	}
	// audit funnels the two audit tables through auditlog — the single writer —
	// while staying inside the seed's tx (auditlog takes an Execer, so *sql.Tx
	// works) and honouring exec's fail-fast discipline: once execErr is set no
	// further row is attempted, and a failure here fails the seed like any other.
	audit := func(what string, fn func() error) {
		if execErr != nil {
			return
		}
		if err := fn(); err != nil {
			execErr = fmt.Errorf("seed exec: %w (near: %s)", err, what)
		}
	}

	now := time.Now().UTC()
	iso := func(t time.Time) string { return t.Format(time.RFC3339) }
	ago := func(d time.Duration) string { return iso(now.Add(-d)) }
	const (
		dev    = "developer@amadeus.local"
		hour   = time.Hour
		minute = time.Minute
		day    = 24 * time.Hour
	)
	nowStr := iso(now)

	// ── Scopes (operator-managed / amadeus-source so they list in Settings) ────
	type scopeSpec struct {
		name, desc, types string
		hosts             []string
		raw               string // optional grouped inventory; empty ⇒ flat [all]
	}
	scopes := []scopeSpec{
		{"Production", "Production fleet — change-controlled", `["bash","ansible","terraform","perl"]`, []string{"db-01", "lb-01", "vault-01", "app-01", "app-02"},
			"[app]\napp-01 ansible_host=10.0.1.11\napp-02 ansible_host=10.0.1.12\n[db]\ndb-01 ansible_host=10.0.2.11 ansible_user=postgres\n[lb]\nlb-01\n[secrets]\nvault-01\n[prod:children]\napp\ndb\n[app:vars]\nansible_user=deploy\nansible_port=22\n"},
		{"Cluster-A", "Kubernetes cluster A worker pool", `["bash","ansible"]`, []string{"web-01", "web-02", "k8s-node-01", "k8s-node-02"}, ""},
		{"Windows-Fleet", "Windows domain controllers + members", `["powershell","ansible"]`, []string{"win-dc-01", "win-app-01"}, ""},
		{"Staging", "Pre-prod staging environment", `["bash","ansible","terraform"]`, []string{"stg-app-01", "stg-db-01"}, ""},
		{"Reporting", "Reporting + analytics hosts", `["bash","perl","python"]`, []string{"report-01"}, ""},
	}
	scopeIDs := map[string]string{}
	for _, sc := range scopes {
		id := db.NewID()
		scopeIDs[sc.name] = id
		// A managed Ansible inventory so the seeded ansible jobs have something to
		// ship for `-i` (M1). Production carries a real grouped inventory so the M2
		// advisory group tree / host_vars panel is demoable; others get a flat
		// [all] of their hosts. Without an inventory an ansible run hard-fails.
		rawInv := sc.raw
		if rawInv == "" {
			rawInv = "[all]\n" + strings.Join(sc.hosts, "\n") + "\n"
		}
		exec(`INSERT INTO scopes (id, name, source, description, supported_types, raw_inventory, inventory_format, created_by, created_at, last_modified_by, last_modified_at)
		      VALUES (?, ?, 'amadeus', ?, ?, ?, 'ini', ?, ?, ?, ?)`,
			id, sc.name, sc.desc, sc.types, rawInv, dev, ago(20*day), dev, ago(2*day))
		for _, h := range sc.hosts {
			exec(`INSERT INTO scope_hosts (scope_id, host) VALUES (?, ?)`, id, h)
		}
		seedScopeProjection(exec, id, rawInv)
	}

	// ── Agencies (network-isolation zones, agency-support.md M1) ───────────────
	// A small catalog with two scopes bound, so the Agencies tab + the scope→agency
	// binding are demoable. Runner membership + hard-isolation dispatch land in M2/M3.
	agencies := []struct{ id, name, desc string }{
		{db.NewID(), "agency-alpha", "Tenant A isolated network"},
		{db.NewID(), "agency-beta", "Tenant B isolated network"},
	}
	for _, a := range agencies {
		exec(`INSERT INTO agencies (id, name, description, created_by, created_at, last_modified_by, last_modified_at)
		      VALUES (?, ?, ?, ?, ?, ?, ?)`,
			a.id, a.name, a.desc, dev, ago(20*day), dev, ago(2*day))
	}
	// scopeAgency maps a seeded scope NAME to the agency it belongs to, and is the
	// single source every membership row below is written from.
	//
	// EVERY scope is bound, deliberately (RB-15). A grant's "where" is an agency, so
	// a scope belonging to no agency produces NO grant and the access silently
	// disappears — the "orphan scope" the pre-flight reports as a hard blocker.
	// Leaving some scopes unbound would model a database that is not ready for the
	// predicate switch, which is a poor default for a demo whose whole point is to
	// show the working end state.
	scopeAgency := map[string]string{
		"Production":    agencies[0].id,
		"Cluster-A":     agencies[0].id,
		"Staging":       agencies[1].id,
		"Reporting":     agencies[1].id,
		"Windows-Fleet": agencies[0].id,
	}
	for name, aid := range scopeAgency {
		sid := scopeIDs[name]
		if sid == "" {
			continue
		}
		// scope_agencies is the ONLY binding now — scopes.agency_id was dropped in
		// migration 700 (T3.9).
		exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, sid, aid)
	}

	// The AD-group→role mappings and the A5 scope matrix that used to be seeded
	// here went with their tables (RB-19, v0.57.8). The grants block below is now
	// the ONLY thing that decides what a seeded demo user can do — which was
	// already true at runtime since v0.56.5; the seed was just still writing to
	// two tables nobody read.

	// ── Access grants (RB-12/RB-15) ────────────────────────────────────────────
	//
	// The seed MUST write these. Migration 810's backfill converts
	// ad_group_mappings × scope_restrictions into grants, but it runs at Migrate
	// time — over empty tables, before a line of this seed exists. Without this
	// block a freshly seeded database has zero grants, and once grants became
	// authoritative that meant every demo user could do nothing at all.
	//
	// A grant's "where" is an AGENCY or the "*" sentinel — never a bare scope
	// (RB-Q1) — so the operator grants are expressed against the agencies their
	// scopes belong to, and expand back to those scopes at login.
	grants := []struct {
		group, role, agency string
		all                 bool
	}{
		{"amadeus-admins", "admin", "", true},
		// operator reaches Cluster-A (alpha) and Staging/Reporting (beta).
		{"amadeus-operators", "operator", agencies[0].id, false},
		{"amadeus-operators", "operator", agencies[1].id, false},
		{"infra-oncall", "operator", agencies[1].id, false},
		// viewer sees Reporting, which lives in beta.
		{"amadeus-viewers", "viewer", agencies[1].id, false},
	}
	for _, g := range grants {
		var agency any
		if g.agency != "" {
			agency = g.agency
		}
		all := 0
		if g.all {
			all = 1
		}
		exec(`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_by, created_at, last_modified_by, last_modified_at)
		      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			db.NewID(), g.group, g.role, agency, all, dev, ago(20*day), dev, ago(20*day))
	}

	// ── Recent logins (A3.2 Honest View) ───────────────────────────────────────
	logins := []struct {
		email, name, groups string
		first, last         time.Duration
	}{
		{"alice@corp.example", "Alice Chen", `["amadeus-admins"]`, 60 * day, 2 * hour},
		{"bob@corp.example", "Bob Diaz", `["amadeus-operators","infra-oncall"]`, 45 * day, 6 * hour},
		{"carol@corp.example", "Carol Singh", `["amadeus-viewers"]`, 30 * day, 28 * hour},
		{dev, "Developer (bypass)", `["amadeus-admins"]`, 10 * day, 0},
	}
	for _, l := range logins {
		exec(`INSERT INTO recent_logins (email, display_name, groups, first_seen_at, last_login_at)
		      VALUES (?, ?, ?, ?, ?)`, l.email, l.name, l.groups, ago(l.first), ago(l.last))
	}

	// ── Jobs (definition cache / read model) ───────────────────────────────────
	type jobSpec struct {
		name, runType, scope, host, schedule, tags string
		enabled                                    int
	}
	jobs := []jobSpec{
		{"nightly-db-backup", "bash", "Production", "db-01", "0 2 * * *", `["backup","db"]`, 1},
		{"ansible-patch-tuesday", "ansible", "Production", "", "0 3 * * 2", `["patching","security"]`, 1},
		{"terraform-plan-prod", "terraform", "Production", "", "", `["iac","prod"]`, 1},
		{"terraform-apply-prod", "terraform", "Production", "", "", `["iac","prod"]`, 1},
		{"log-rotate-web", "bash", "Cluster-A", "web-01", "0 0 * * *", `["maintenance"]`, 1},
		{"win-update-check", "powershell", "Windows-Fleet", "win-dc-01", "0 6 * * *", `["patching","windows"]`, 1},
		{"cert-renewal", "bash", "Production", "lb-01", "0 4 * * 1", `["security","tls"]`, 1},
		{"perl-report-gen", "perl", "Reporting", "report-01", "0 7 * * 1-5", `["reporting"]`, 1},
		{"python-metrics-export", "python", "Reporting", "report-01", "0 6 * * *", `["reporting","monitoring"]`, 1},
		{"k8s-node-drain", "bash", "Cluster-A", "", "", `["k8s","maintenance"]`, 1},
		{"ansible-deploy-app", "ansible", "Staging", "", "", `["deploy","staging"]`, 1},
		{"disk-usage-audit", "bash", "", "", "*/30 * * * *", `["monitoring"]`, 1},
		{"vault-token-rotate", "bash", "Production", "vault-01", "0 1 * * *", `["security","secrets"]`, 0},
	}
	for _, j := range jobs {
		var scope, host, schedule any
		if j.scope != "" {
			scope = j.scope
		}
		if j.host != "" {
			host = j.host
		}
		if j.schedule != "" {
			schedule = j.schedule
		}
		// R2-2: the seeder writes definitions directly, so it must mint the uid the
		// sync and compose writers would have — otherwise dev preview is the one
		// environment where every definition is uid-less and the uid-keyed paths
		// silently exercise only their name fallbacks.
		exec(`INSERT INTO jobs (name, run_type, description, scope, target_host, schedule, tags, enabled,
		        timeout_seconds, retries, requestable, concurrency_policy, source_path, synced_at, uid)
		      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, 'Allow', ?, ?, ?)`,
			j.name, j.runType, "Demo job: "+j.name, scope, host, schedule, j.tags, j.enabled,
			3600, "jobs/"+j.name+".yaml", ago(2*day), db.NewID())
		// LU-6: the seeder inserts straight into jobs, bypassing both the sync and
		// compose paths, so it needs its own allocation — otherwise every demo job
		// would write its logs to the flat fallback path and the folder layout
		// would be invisible in local preview, which is the one place it gets
		// looked at by eye.
		exec(`INSERT INTO entity_codes (kind, source, name, created_at, uid)
		      VALUES ('job','git',?,?, (SELECT uid FROM jobs WHERE name = ? AND source = 'git'))
		      ON CONFLICT DO NOTHING`,
			j.name, ago(2*day), j.name)
	}

	// ── Workflows ──────────────────────────────────────────────────────────────
	workflows := []struct {
		name, desc, steps, schedule string
		enabled                     int
	}{
		{"prod-release", "Plan → apply → smoke test the production release",
			`[{"type":"job","name":"terraform-plan-prod","label":"Plan"},{"type":"job","name":"terraform-apply-prod","label":"Apply"},{"type":"job","name":"disk-usage-audit","label":"Verify"}]`, "", 1},
		{"patch-and-report", "Patch the fleet then generate the compliance report",
			`[{"type":"job","name":"ansible-patch-tuesday","label":"Patch"},{"type":"job","name":"perl-report-gen","label":"Report"}]`, "0 3 * * 2", 1},
		{"nightly-maintenance", "Backups, log rotation and cert checks",
			`[{"type":"parallel","jobs":[{"type":"job","name":"nightly-db-backup","label":"Backup"},{"type":"job","name":"log-rotate-web","label":"Rotate"}]},{"type":"job","name":"cert-renewal","label":"Certs"}]`, "0 0 * * *", 1},
	}
	for _, wf := range workflows {
		var schedule any
		if wf.schedule != "" {
			schedule = wf.schedule
		}
		exec(`INSERT INTO workflows (name, description, steps, schedule, enabled, source_path, synced_at, uid)
		      VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			wf.name, wf.desc, wf.steps, schedule, wf.enabled, "workflows/"+wf.name+".yaml", ago(2*day), db.NewID())
		exec(`INSERT INTO entity_codes (kind, source, name, created_at, uid)
		      VALUES ('workflow','git',?,?, (SELECT uid FROM workflows WHERE name = ? AND source = 'git'))
		      ON CONFLICT DO NOTHING`,
			wf.name, ago(2*day), wf.name)
	}

	// ── Multi-schedule entries (definition_schedules) ──────────────────────────
	// Mirror each legacy single schedule as the 'default' entry, then add two
	// extra named entries — one env-bearing entry on a job, one on a workflow — so
	// the Schedule hub (inventory/upcoming), the +N badge, and env traceability
	// are all populated in dev preview.
	type schedEntry struct {
		kind, owner, name, cron, env string
		pos                          int
	}
	var schedEntries []schedEntry
	for _, j := range jobs {
		if j.schedule != "" {
			schedEntries = append(schedEntries, schedEntry{"job", j.name, "default", j.schedule, "", 0})
		}
	}
	for _, wf := range workflows {
		if wf.schedule != "" {
			schedEntries = append(schedEntries, schedEntry{"workflow", wf.name, "default", wf.schedule, "", 0})
		}
	}
	schedEntries = append(schedEntries,
		schedEntry{"job", "nightly-db-backup", "verify-4h", "0 */4 * * *", `{"VERIFY":"true","STAGE":"prod"}`, 1},
		schedEntry{"workflow", "nightly-maintenance", "weekend-deep", "0 1 * * 0", "", 1},
	)
	for _, e := range schedEntries {
		exec(`INSERT INTO definition_schedules (owner_kind, owner_name, name, cron, env, position, owner_uid)
		      VALUES (?, ?, ?, ?, ?, ?,
		              CASE ?
		                WHEN 'job'      THEN (SELECT uid FROM jobs      WHERE name = ? AND source = 'git')
		                WHEN 'workflow' THEN (SELECT uid FROM workflows WHERE name = ? AND source = 'git')
		              END)`,
			e.kind, e.owner, e.name, e.cron, nullStr(e.env), e.pos,
			e.kind, e.owner, e.owner)
	}

	// ── First-class Schedules catalog (operator-authored amadeus rows) ─────────
	// So the Schedules catalog + the Schedule Builder edit/delete affordances are
	// populated in dev preview. content_hash is a placeholder digest (the catalog
	// shows only a short prefix; the real digest is recomputed on any in-app edit).
	type schedCatalog struct{ name, desc, cron, env, hash string }
	for _, sd := range []schedCatalog{
		{"business-hours", "Weekday business-hours sweep", "0 9 * * 1-5", `{"WINDOW":"business"}`, "sha256:seedbiz"},
		{"nightly-window", "Nightly maintenance window", "0 0 2 * * *", "", "sha256:seednight"},
	} {
		exec(`INSERT INTO schedules (name, source, description, cron, env, content_hash, created_by, created_at, last_modified_by, last_modified_at, uid)
		      VALUES (?, 'amadeus', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sd.name, sd.desc, sd.cron, nullStr(sd.env), sd.hash, dev, ago(10*day), dev, ago(2*day), db.NewID())
	}

	// ── Runs (history) — spread over the last week across statuses ─────────────
	runStatuses := []string{"success", "success", "failure", "success", "warning", "success", "success", "killed", "success", "running"}
	runIdx := 0
	for pass := range 3 {
		for ji, j := range jobs {
			st := runStatuses[runIdx%len(runStatuses)]
			created := now.Add(-time.Duration(runIdx)*4*hour - time.Duration(pass)*7*minute)
			triggerKind := "scheduled"
			triggeredBy := "scheduler@amadeus"
			if ji%2 == 1 {
				triggerKind, triggeredBy = "manual", []string{"alice@corp.example", "bob@corp.example", dev}[runIdx%3]
			}
			if j.schedule == "" {
				triggerKind, triggeredBy = "manual", "alice@corp.example"
			}
			// Attribute scheduled runs to a schedule entry; one job's run carries
			// the second (env-bearing) entry so History/Inventory show env traceability.
			scheduleName, envJSON := "", ""
			if triggerKind == "scheduled" {
				scheduleName = "default"
				if j.name == "nightly-db-backup" && pass == 0 {
					scheduleName, envJSON = "verify-4h", `{"VERIFY":"true","STAGE":"prod"}`
				}
			}
			seedRun(exec, runRow{
				id: db.NewTraceID(), job: j.name, runType: j.runType, scope: j.scope,
				host: j.host, status: st, triggeredBy: triggeredBy, triggerKind: triggerKind,
				scheduleName: scheduleName, envJSON: envJSON,
				created: created, now: now,
			})
			runIdx++
		}
	}
	// A couple of queued runs waiting on a capable runner (A6.3 demo).
	for _, q := range []struct{ job, runType, scope, reason string }{
		{"terraform-apply-prod", "terraform", "Production", "waiting for a terraform-capable runner"},
		{"win-update-check", "powershell", "Windows-Fleet", "waiting for a windows runner"},
	} {
		// R2-2: seeded history carries the job's uid, exactly as a real enqueue
		// would. Without it the job-detail panel — which asks by uid — would show
		// an empty history for every demo job.
		exec(`INSERT INTO runs (id, job_name, run_type, scope, status, queued_reason, triggered_by, trigger_kind, created_at, job_uid)
		      VALUES (?, ?, ?, ?, 'queued', ?, 'alice@corp.example', 'manual', ?,
		              (SELECT uid FROM jobs WHERE name = ? AND source = 'git'))`,
			db.NewTraceID(), q.job, q.runType, q.scope, q.reason, ago(3*minute), q.job)
	}

	// ── Workflow runs with child job runs ──────────────────────────────────────
	wfRuns := []struct {
		name        string
		wfID        int64
		status      string
		scope       string
		schedule    string      // schedule entry name; "" ⇒ no schedule attribution
		children    [][2]string // {jobName, runType}
		childStatus []string
		age         time.Duration
	}{
		{"prod-release", 1, "success", "Production", "",
			[][2]string{{"terraform-plan-prod", "terraform"}, {"terraform-apply-prod", "terraform"}, {"disk-usage-audit", "bash"}},
			[]string{"success", "success", "success"}, 26 * hour},
		{"patch-and-report", 2, "warning", "Production", "default",
			[][2]string{{"ansible-patch-tuesday", "ansible"}, {"perl-report-gen", "perl"}},
			[]string{"success", "warning"}, 50 * hour},
		{"nightly-maintenance", 3, "failure", "Production", "default",
			[][2]string{{"nightly-db-backup", "bash"}, {"log-rotate-web", "bash"}, {"cert-renewal", "bash"}},
			[]string{"success", "success", "failure"}, 8 * hour},
	}
	for _, wr := range wfRuns {
		wfTrace := db.NewTraceID()
		start := now.Add(-wr.age)
		end := start.Add(12 * minute)
		exec(`INSERT INTO workflow_runs (id, workflow_name, workflow_id, status, triggered_by, trigger_kind, scope, schedule_name, started_at, completed_at, duration_ms, created_at, workflow_uid)
		      VALUES (?, ?, ?, ?, 'alice@corp.example', 'scheduled', ?, ?, ?, ?, ?, ?,
		              (SELECT uid FROM workflows WHERE name = ? AND source = 'git'))`,
			wfTrace, wr.name, wr.wfID, wr.status, wr.scope, nullStr(wr.schedule), iso(start), iso(end), 720000, iso(start), wr.name)
		for ci, ch := range wr.children {
			cStart := start.Add(time.Duration(ci) * 4 * minute)
			cEnd := cStart.Add(3 * minute)
			exec(`INSERT INTO runs (id, job_name, run_type, scope, status, triggered_by, trigger_kind, workflow_run_id, started_at, completed_at, duration_ms, exit_code, created_at, job_uid)
			      VALUES (?, ?, ?, ?, ?, 'alice@corp.example', 'workflow', ?, ?, ?, ?, ?, ?,
			              (SELECT uid FROM jobs WHERE name = ? AND source = 'git'))`,
				db.NewTraceID(), ch[0], ch[1], wr.scope, wr.childStatus[ci], wfTrace,
				iso(cStart), iso(cEnd), 180000, exitFor(wr.childStatus[ci]), iso(cStart), ch[0])
		}
	}

	// ── Activity feed (7-kind discriminated union) ─────────────────────────────
	type actRow struct {
		kind, outcome, actor, job, wf, scope, category, action, summary, repo, branch, sha string
		age                                                                                time.Duration
	}
	acts := []actRow{
		{"run-end", "success", "scheduler@amadeus", "nightly-db-backup", "", "Production", "", "", "Backup completed (2.3 GB)", "", "", "", 1 * hour},
		{"run-end", "failure", "scheduler@amadeus", "cert-renewal", "", "Production", "", "", "ACME challenge failed for lb-01", "", "", "", 8 * hour},
		{"run-start", "", "alice@corp.example", "terraform-plan-prod", "", "Production", "", "", "Plan started", "", "", "", 2 * hour},
		{"workflow-end", "warning", "alice@corp.example", "", "patch-and-report", "Production", "", "", "1 host reported a warning", "", "", "", 50 * hour},
		{"workflow-start", "", "scheduler@amadeus", "", "nightly-maintenance", "Production", "", "", "Nightly maintenance triggered", "", "", "", 8 * hour},
		{"config", "", "bob@corp.example", "vault-token-rotate", "", "Production", "Jobs", "Paused", "Paused scheduled runs", "", "", "", 5 * hour},
		{"config", "", "alice@corp.example", "", "", "", "Settings", "updated", "Updated max concurrency to 8", "", "", "", 30 * hour},
		{"config", "", "alice@corp.example", "", "", "", "Secrets", "revealed", "Revealed GITLAB_PAT", "", "", "", 4 * hour},
		{"gitsync", "success", "gitlab-webhook", "", "", "", "Git", "sync", "Synced 12 jobs, 3 workflows", "infra/job-defs", "main", "a1b2c3d", 90 * minute},
		{"gitsync", "failure", "poll", "", "", "", "Git", "sync", "Clone failed: auth error", "infra/job-defs", "main", "", 10 * hour},
		{"push", "success", "alice@corp.example", "", "", "Production", "Schedule", "publish", "Published nightly-db-backup schedule", "infra/job-defs", "main", "f00ba12", 3 * hour},
		{"run-end", "failure", "bob@corp.example", "k8s-node-drain", "", "Cluster-A", "", "", "Run killed by operator", "", "", "", 6 * hour},
		{"run-end", "success", "scheduler@amadeus", "disk-usage-audit", "", "", "", "", "All hosts under threshold", "", "", "", 30 * minute},
		{"run-end", "warning", "scheduler@amadeus", "win-update-check", "", "Windows-Fleet", "", "", "Reboot pending on win-app-01", "", "", "", 12 * hour},
	}
	for _, a := range acts {
		// Backdated via At: the spread of ages is the point — it is what the
		// Dashboard's "last 24h" panels and the History timeline render.
		audit("INSERT INTO activity", func() error {
			return auditlog.WriteActivity(ctx, tx, auditlog.ActivityParams{
				At:           ago(a.age),
				Kind:         a.kind,
				Outcome:      a.outcome,
				Actor:        a.actor,
				JobName:      a.job,
				WorkflowName: a.wf,
				Scope:        a.scope,
				Category:     a.category,
				Action:       a.action,
				Summary:      a.summary,
				CommitSha:    a.sha,
				Repository:   a.repo,
				Branch:       a.branch,
			})
		})
	}

	// ── Change log (audit trail) ───────────────────────────────────────────────
	changes := []struct {
		actor, category, action, target, details string
		age                                      time.Duration
	}{
		{"alice@corp.example", "Settings", "updated", "general", "maxConcurrent 5 → 8", 30 * hour},
		{"alice@corp.example", "Secrets", "created", "GITLAB_PAT", "vault-backed", 40 * hour},
		{"alice@corp.example", "Secrets", "revealed", "GITLAB_PAT", "", 4 * hour},
		{"bob@corp.example", "Jobs", "Paused", "vault-token-rotate", "", 5 * hour},
		{"bob@corp.example", "Jobs", "Triggered", "k8s-node-drain", "manual run", 6 * hour},
		{"alice@corp.example", "Scopes", "created", "Reporting", "1 host", 18 * day},
		{"alice@corp.example", "EnvVars", "updated", "LOG_LEVEL", "info → debug", 2 * day},
		{"carol@corp.example", "Alerts", "created", "prod-failures", "", 12 * day},
		{"alice@corp.example", "SSH", "created", "db-01", "", 16 * day},
		{"bob@corp.example", "Workflows", "Triggered", "prod-release", "", 26 * hour},
	}
	for _, c := range changes {
		audit("INSERT INTO change_log", func() error {
			return auditlog.WriteChangeLogAt(ctx, tx, ago(c.age),
				c.actor, c.category, c.action, c.target, c.details)
		})
	}

	// ── Schedule pushes (A2 audit) ─────────────────────────────────────────────
	pushes := []struct {
		actor, file, base, new, status string
		age                            time.Duration
	}{
		{"alice@corp.example", "schedules/nightly-db-backup.yaml", "a1b2c3d", "f00ba12", "success", 3 * hour},
		{"bob@corp.example", "schedules/cert-renewal.yaml", "f00ba12", "9c8d7e6", "success", 2 * day},
		{"alice@corp.example", "schedules/ansible-patch-tuesday.yaml", "old1234", "", "rejected", 5 * day},
		{"alice@corp.example", "schedules/log-rotate-web.yaml", "9c8d7e6", "", "failed", 6 * day},
	}
	for _, p := range pushes {
		exec(`INSERT INTO schedule_pushes (at, actor, schedule_file, base_sha, new_sha, status, details, created_at)
		      VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			ago(p.age), p.actor, p.file, nullStr(p.base), nullStr(p.new), p.status,
			nullStr(map[string]string{"rejected": "base SHA mismatch (concurrent edit)", "failed": "GitLab API 500"}[p.status]), ago(p.age))
	}

	// ── Git sync history + state ───────────────────────────────────────────────
	syncs := []struct {
		by, sha, status   string
		jobs, wfs, scopes int
		errMsg            string
		age               time.Duration
	}{
		{"webhook", "a1b2c3d", "success", 12, 3, 5, "", 90 * minute},
		{"poll", "a1b2c3d", "success", 12, 3, 5, "", 6 * hour},
		{"poll", "", "failed", 0, 0, 0, "clone failed: auth error", 10 * hour},
		{"manual", "9c8d7e6", "success", 11, 3, 5, "", 2 * day},
		{"webhook", "9c8d7e6", "partial", 11, 3, 4, "1 scope file failed to parse", 3 * day},
	}
	for _, s := range syncs {
		start := now.Add(-s.age)
		exec(`INSERT INTO git_sync_events (triggered_by, sha, status, jobs_synced, wfs_synced, scopes_synced, error_message, started_at, finished_at)
		      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.by, nullStr(s.sha), s.status, s.jobs, s.wfs, s.scopes, nullStr(s.errMsg), iso(start), iso(start.Add(8*time.Second)))
	}
	exec(`INSERT INTO git_sync_state (id, last_sha, last_synced_at, last_status) VALUES (1, ?, ?, 'success')`, "a1b2c3d", ago(90*minute))

	// ── Runners ────────────────────────────────────────────────────────────────
	runners := []struct {
		name, status, os, caps string
		load, maxConc          int
		lastSeen               time.Duration
		// toolchains is the display-only detected-toolchain blob (RX.7/R6); "" ⇒
		// NULL (a pre-Phase-4 agent, so the detail shows "—").
		toolchains string
	}{
		{"runner-linux-01", "online", "Linux", `["bash","ansible","terraform","perl","python"]`, 2, 5, 20 * time.Second,
			`{"ansibleCore":"2.16.3","collections":{"community.general":"8.5.0","ansible.posix":"1.5.4"},"checkout":true,"vault":true,"sandboxed":true,"keyNames":["ansible_rh8_key","bastion_key","prod_deploy"]}`},
		{"runner-linux-02", "online", "Linux", `["bash","ansible"]`, 0, 5, 15 * time.Second,
			`{"ansibleCore":"2.15.9","sandboxed":true,"keyNames":["ansible_rh8_key"]}`},
		{"runner-win-01", "online", "Windows", `["powershell"]`, 1, 3, 45 * time.Second, ""},
		{"runner-linux-03", "draining", "Linux", `["bash","terraform"]`, 1, 5, 2 * minute, ""},
		{"runner-linux-04", "offline", "Linux", `["bash"]`, 0, 5, 3 * hour, ""},
	}
	for _, r := range runners {
		var drain any
		if r.status == "draining" {
			drain = ago(-10 * minute) // deadline in the future
		}
		var toolchains any // NULL when empty (pre-Phase-4 agent)
		if r.toolchains != "" {
			toolchains = r.toolchains
		}
		exec(`INSERT INTO runners (id, name, status, capabilities, load, last_seen_at, drain_deadline_at, registered_at, created_at, os, version, max_concurrent, toolchains)
		      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '1.4.0', ?, ?)`,
			db.NewID(), r.name, r.status, r.caps, r.load, ago(r.lastSeen), drain, ago(15*day), ago(15*day), r.os, r.maxConc, toolchains)
	}

	// ── Env vars ───────────────────────────────────────────────────────────────
	envVars := []struct{ key, scope, value, desc string }{
		{"LOG_LEVEL", "", "debug", "Global log verbosity for all runners."},
		{"TZ", "", "UTC", "Timezone applied to scheduled runs."},
		{"BACKUP_RETENTION_DAYS", "Production", "30", "How long nightly DB backups are kept."},
		{"ANSIBLE_FORKS", "Production", "10", ""},
		{"TF_IN_AUTOMATION", "Production", "true", "Suppresses Terraform interactive prompts."},
		{"REPORT_FORMAT", "Reporting", "pdf", ""},
		{"K8S_NAMESPACE", "Cluster-A", "default", ""},
		{"MAX_PARALLEL", "", "4", "Per-job fan-out cap for scope runs."},
	}
	for _, e := range envVars {
		var scope, desc any
		if e.scope != "" {
			scope = e.scope
		}
		if e.desc != "" {
			desc = e.desc
		}
		vid := db.NewID()
		exec(`INSERT INTO env_vars (id, key, scope, value, description, created_by, created_at, last_modified_by, last_modified_at)
		      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, vid, e.key, scope, e.value, desc, dev, ago(18*day), dev, ago(2*day))
		if aid := scopeAgency[e.scope]; aid != "" {
			exec(`INSERT INTO env_var_agencies (env_var_id, agency_id) VALUES (?, ?)`, vid, aid)
		}
	}

	// ── Secrets (vault-backed refs only — no KEK needed for demo) ──────────────
	secs := []struct{ key, scope, ref, desc string }{
		{"GITLAB_PAT", "", "secret/data/amadeus/gitlab#pat", "Personal access token for cloning Git-source definitions"},
		{"VAULT_TOKEN", "Production", "secret/data/amadeus/vault#token", "Vault token used by Production jobs"},
		{"SMTP_PASSWORD", "", "secret/data/amadeus/smtp#password", "SMTP relay password for alert email delivery"},
		{"DB_BACKUP_KEY", "Production", "secret/data/amadeus/backup#key", "Encryption key for nightly database backups"},
		{"WIN_ADMIN_PASS", "Windows-Fleet", "secret/data/amadeus/windows#admin", "Local administrator password for the Windows fleet"},
	}
	for _, s := range secs {
		var scope any
		if s.scope != "" {
			scope = s.scope
		}
		sid := db.NewID()
		exec(`INSERT INTO secrets (id, key, scope, description, source, vault_ref, created_by, created_at, last_modified_by, last_modified_at)
		      VALUES (?, ?, ?, ?, 'vault', ?, ?, ?, ?, ?)`, sid, s.key, scope, s.desc, s.ref, dev, ago(18*day), dev, ago(5*day))
		// T2.10 — membership mirroring exactly what migration 670's backfill computes
		// for an existing database: a SCOPED secret inherits its scope's agency, an
		// UNSCOPED (global) one gets no rows at all. Seeding global secrets into every
		// agency would demo a narrower model than the one that actually ships.
		if aid := scopeAgency[s.scope]; aid != "" {
			exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES (?, ?)`, sid, aid)
		}
	}

	// ── SSH key credential (SK.13) ─────────────────────────────────────────────
	// A first-class SSH key the amadeus hosts + bastions below attach to via FK,
	// so the SK.10/SK.11 picker and the Env Vars → SSH Keys management tab are
	// browsable in dev preview. Display-only: dev preview has no KEK to seal real
	// key material, so this stored row carries the DERIVED metadata (type +
	// fingerprint + public key) for the UI but no ciphertext — it can't actually
	// authenticate, which is moot since the seeded hosts use placeholder addresses.
	// The git-imported host further down stays on the legacy auth_key_env_var NAME
	// path to demonstrate dual-read.
	sshCredID := db.NewID()
	exec(`INSERT INTO ssh_credentials (id, label, description, source, key_type, fingerprint, public_key, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES (?, ?, ?, 'stored', 'ssh-ed25519', ?, ?, ?, ?, ?, ?)`,
		sshCredID, "prod_deploy_ed25519", "Primary deploy key for the Production fleet (demo).",
		"SHA256:Jm6h0vQ2nC8x7yQk9rTfLwApZ3Bd1sEoUvHnMxRkY4w",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINrQ2vJk8hPzXmC5dLwoYbApZ3Bd1sEoUvHnMxRkY4w amadeus-deploy",
		dev, ago(18*day), dev, ago(5*day))

	// T2.10 — a SECOND key so the membership matrix has one per agency. Note that
	// migration 670 deliberately backfills NO key membership (ssh_credentials has no
	// scope to infer from, and AG-Q5's tightening is Phase 3 behind the T2.12
	// report), so these two rows are OPERATOR-ASSIGNED membership — exactly what an
	// admin would create by hand, and the only way Phase 4's matrix has key data to
	// render before the tightening lands.
	stagingCredID := db.NewID()
	exec(`INSERT INTO ssh_credentials (id, label, description, source, key_type, fingerprint, public_key, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES (?, ?, ?, 'stored', 'ssh-ed25519', ?, ?, ?, ?, ?, ?)`,
		stagingCredID, "staging_deploy_ed25519", "Deploy key for the Staging fleet (demo).",
		"SHA256:Qw3rTy7uIoP2aSdFgHjKlZxCvBnM4eRt6YuIoP8aSdF",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKp7QwErTyUiOpAsDfGhJkLzXcVbNm4eRt6YuIoP8aSd amadeus-staging",
		dev, ago(16*day), dev, ago(4*day))
	exec(`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES (?, ?)`, sshCredID, agencies[0].id)
	exec(`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES (?, ?)`, stagingCredID, agencies[1].id)

	// ── SSH bastions + hosts ───────────────────────────────────────────────────
	// Seeded as 'verified' with a recent last_checked_at so the SSH Targets UI is
	// browsable in dev preview; a real "Test connection" only downgrades these if
	// it fails (ssh-update.md TC.7). Both bastions reference the credential above.
	bastionID := db.NewID()
	exec(`INSERT INTO bastions (id, hostname, name, address, port, username, zone, auth_credential_id, status, last_checked_at, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES (?, ?, ?, ?, 22, 'jump', ?, ?, 'verified', ?, ?, ?, ?, ?)`,
		bastionID, "bastion-prod.corp.example", "prod-bastion", "bastion-prod.corp.example", "prod", sshCredID, ago(2*day), dev, ago(20*day), dev, ago(20*day))
	exec(`INSERT INTO bastions (id, hostname, name, address, port, username, zone, auth_credential_id, status, last_checked_at, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES (?, ?, ?, ?, 22, 'jump', ?, ?, 'verified', ?, ?, ?, ?, ?)`,
		db.NewID(), "bastion-dmz.corp.example", "dmz-bastion", "bastion-dmz.corp.example", "dmz", sshCredID, ago(2*day), dev, ago(20*day), dev, ago(20*day))

	sshHosts := []struct{ host, addr, os, via string }{
		{"db-01", "10.0.1.10", "Linux", "prod-bastion"},
		{"lb-01", "10.0.1.20", "Linux", "prod-bastion"},
		{"vault-01", "10.0.1.30", "Linux", "prod-bastion"},
		{"web-01", "10.0.2.10", "Linux", ""},
		{"win-dc-01", "10.0.3.10", "Windows", "dmz-bastion"},
		{"report-01", "10.0.4.10", "Linux", ""},
	}
	for _, h := range sshHosts {
		var via any
		if h.via != "" {
			via = h.via
		}
		exec(`INSERT INTO ssh_hosts (id, hostname, address, port, os, via, auth_credential_id, username, status, last_checked_at, created_by, created_at, last_modified_by, last_modified_at)
		      VALUES (?, ?, ?, 22, ?, ?, ?, 'amadeus', 'verified', ?, ?, ?, ?, ?)`,
			db.NewID(), h.host, h.addr, h.os, via, sshCredID, ago(2*day), dev, ago(16*day), dev, ago(3*day))
	}
	// One git-IMPORTED host (M4) so the demo shows the read-only "git" badge + the
	// dual-source precedence in Settings → SSH Targets. Tied to Production; starts
	// unverified (first dial would TOFU). created_by/last_modified_by set so the
	// strict scanSshHost read works.
	exec(`INSERT INTO ssh_hosts (id, source, scope_id, hostname, address, port, auth_key_env_var, username, status, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES (?, 'git', (SELECT id FROM scopes WHERE name='Production'), 'app-01', '10.0.1.11', 22, 'SSH_DEPLOY_KEY', 'deploy', 'unverified', 'gitlab', ?, 'gitlab', ?)`,
		db.NewID(), ago(2*day), ago(2*day))

	// ── Alert destinations + rules ─────────────────────────────────────────────
	//
	// The destinations below have no UI as of v0.52.24 (K-1 deleted the section:
	// nothing ever read this table to route a message). They are still seeded
	// because the table and `alert_config.destination_id`'s FK were deliberately
	// retained — if the rule-targets-a-destination design is ever built, the demo
	// data is already shaped for it. See VF-16 for why it was not built now.
	destSlack := db.NewID()
	destEmail := db.NewID()
	destWebhook := db.NewID()
	exec(`INSERT INTO alert_destinations (id, type, target, label, config, enabled, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES (?, 'slack', '#infra-alerts', 'Infra Slack', '{"webhookUrl":"https://hooks.slack.example/T000/B000"}', 1, ?, ?, ?, ?)`,
		destSlack, dev, ago(12*day), dev, ago(12*day))
	exec(`INSERT INTO alert_destinations (id, type, target, label, config, enabled, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES (?, 'email', 'oncall@corp.example', 'On-call Email', '{"to":"oncall@corp.example"}', 1, ?, ?, ?, ?)`,
		destEmail, dev, ago(12*day), dev, ago(12*day))
	exec(`INSERT INTO alert_destinations (id, type, target, label, config, enabled, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES (?, 'webhook', 'https://pager.example/hook', 'PagerDuty', '{"url":"https://pager.example/hook"}', 0, ?, ?, ?, ?)`,
		destWebhook, dev, ago(12*day), dev, ago(12*day))

	// J-5 (VF-15) — these rows used to seed `target_mode = "scope"` and the
	// slack/in-app/webhook channels, none of which the dispatcher has a branch
	// for: three of the four demo rules matched nothing or sent nothing. Demo
	// data has to show the product working, or it teaches the defect. Every row
	// below is now a rule that would genuinely fire.
	//
	// `backup-warnings` keeps its `warning` trigger deliberately: it is real,
	// it fires, and it is the fixture for J-4 — a stored trigger the form does
	// not offer must survive an open-and-save instead of being rewritten.
	// (The legacy name/condition columns these rows once carried were dropped
	// by migration 1140; the rule is its target + trigger.)
	alerts := []struct {
		dest, mode, jobName, trigger, channels, owner string
		enabled                                       int
	}{
		{destSlack, "all", "", "failure", `["apprise"]`, "alice@corp.example", 1},
		{destEmail, "all", "", "failure", `["email","apprise"]`, "bob@corp.example", 1},
		{destSlack, "job", "nightly-db-backup", "warning", `["apprise"]`, "alice@corp.example", 1},
		{destWebhook, "job", "cert-renewal", "failure", `["email"]`, "alice@corp.example", 0},
	}
	for _, a := range alerts {
		var jobName any
		if a.jobName != "" {
			jobName = a.jobName
		}
		exec(`INSERT INTO alert_config (id, destination_id, target_mode, job_name, trigger, channels, owner, enabled, created_by, created_at, last_modified_by, last_modified_at)
		      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			db.NewID(), a.dest, a.mode, jobName, a.trigger, a.channels, a.owner, a.enabled, dev, ago(12*day), dev, ago(4*day))
	}

	// ── Singleton configs + global settings ────────────────────────────────────
	exec(`INSERT INTO notification_config (id, smtp_host, smtp_port, smtp_from, apprise_targets, last_modified_by, last_modified_at)
	      VALUES (1, 'smtp.corp.example', 587, 'amadeus@corp.example', '[{"label":"On-call","service":"email","url":"mailto://oncall@corp.example","enabled":true}]', ?, ?)`, dev, ago(9*day))
	exec(`INSERT INTO gitlab_config (id, base_url, project_path, webhook_secret, branch, last_modified_by, last_modified_at)
	      VALUES (1, 'https://gitlab.corp.example', 'infra/job-defs', 'demo-webhook-secret', 'main', ?, ?)`, dev, ago(9*day))

	settings := map[string]string{
		"appName":           "Cronomicon",
		"timezone":          "UTC",
		"maxConcurrent":     "8",
		"jobTimeoutSeconds": "3600",
		"sessionPolicy":     `{"timeoutMinutes":720,"reauth":false}`,
	}
	for k, v := range settings {
		exec(`INSERT INTO settings (key, value, last_modified_by, last_modified_at) VALUES (?, ?, ?, ?)`, k, v, dev, nowStr)
	}

	if execErr != nil {
		return execErr
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("seed commit: %w", err)
	}
	log.Warn("demo data seeded (CRONOMICON_DEV_SEED=true) — local preview content; not a real sync")
	return nil
}

// runRow carries the fields for a single seeded historical run.
type runRow struct {
	id, job, runType, scope, host, status, triggeredBy, triggerKind string
	scheduleName, envJSON                                           string
	created, now                                                    time.Time
}

// seedScopeProjection parses a scope's raw inventory and writes the advisory
// projection tables + projection_status (M2), mirroring gitlab.writeScopeProjection
// so CRONOMICON_DEV_SEED can exercise the inventory group-tree panel.
func seedScopeProjection(exec func(string, ...any), id, raw string) {
	pr := inventory.ParseProjection(raw)
	if pr.PreviewUnavailable {
		exec(`UPDATE scopes SET projection_status='unavailable' WHERE id=?`, id)
		return
	}
	exec(`UPDATE scopes SET projection_status='ok' WHERE id=?`, id)
	for gname, g := range pr.Groups {
		exec(`INSERT OR IGNORE INTO scope_groups(scope_id,name) VALUES(?,?)`, id, gname)
		for _, h := range g.Hosts {
			exec(`INSERT OR IGNORE INTO scope_group_hosts(scope_id,group_name,host) VALUES(?,?,?)`, id, gname, h)
		}
		for _, ch := range g.Children {
			exec(`INSERT OR IGNORE INTO scope_group_children(scope_id,parent,child) VALUES(?,?,?)`, id, gname, ch)
		}
		for k, v := range g.Vars {
			exec(`INSERT OR IGNORE INTO scope_group_vars(scope_id,group_name,key,value) VALUES(?,?,?,?)`, id, gname, k, v)
		}
	}
	for h, vars := range pr.HostVars {
		for k, v := range vars {
			exec(`INSERT OR IGNORE INTO scope_host_vars(scope_id,host,key,value) VALUES(?,?,?,?)`, id, h, k, v)
		}
	}
}

func seedRun(exec func(string, ...any), r runRow) {
	var scope, host any
	if r.scope != "" {
		scope = r.scope
	}
	if r.host != "" {
		host = r.host
	}
	iso := func(t time.Time) string { return t.Format(time.RFC3339) }
	switch r.status {
	case "running":
		started := r.created
		exec(`INSERT INTO runs (id, job_name, run_type, scope, target_host, status, triggered_by, trigger_kind, schedule_name, env_json, started_at, created_at, job_uid)
		      VALUES (?, ?, ?, ?, ?, 'running', ?, ?, ?, ?, ?, ?,
		              (SELECT uid FROM jobs WHERE name = ? AND source = 'git'))`,
			r.id, r.job, r.runType, scope, host, r.triggeredBy, r.triggerKind,
			nullStr(r.scheduleName), nullStr(r.envJSON), iso(started), iso(r.created), r.job)
	default:
		started := r.created
		dur := 120000 + (len(r.job)*1000)%480000
		completed := started.Add(time.Duration(dur) * time.Millisecond)
		var killedBy any
		if r.status == "killed" {
			killedBy = "bob@corp.example"
		}
		exec(`INSERT INTO runs (id, job_name, run_type, scope, target_host, status, triggered_by, trigger_kind, killed_by, schedule_name, env_json, started_at, completed_at, duration_ms, exit_code, created_at, job_uid)
		      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		              (SELECT uid FROM jobs WHERE name = ? AND source = 'git'))`,
			r.id, r.job, r.runType, scope, host, r.status, r.triggeredBy, r.triggerKind, killedBy,
			nullStr(r.scheduleName), nullStr(r.envJSON),
			iso(started), iso(completed), dur, exitFor(r.status), iso(r.created), r.job)
	}
}

func exitFor(status string) int {
	switch status {
	case "success", "warning":
		return 0
	case "killed":
		return 137
	default:
		return 1
	}
}

// nullStr maps "" → SQL NULL so optional columns stay null rather than empty.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

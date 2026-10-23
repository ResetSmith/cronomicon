package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/preflight"
)

// runPreflight implements `cronomicon preflight` (GC-19): a read-only report of
// what this release changes for the installation the database belongs to. Run
// it with the NEW binary against the EXISTING database, before upgrading. It
// runs no migration and changes no row. It opens the database the way the
// server does (WAL journal mode), which is the mode a server-managed database
// is already in.
//
// Exit status: 0 when the report was produced, 3 when it found that no global
// administrator exists (the one finding that makes the upgrade unsafe to take
// unattended), 1 on an error, 2 on bad usage.
func runPreflight(args []string) int {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	dbPath := fs.String("db", "", "database path (default: CRONOMICON_DB_PATH from config)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: cronomicon preflight [-db path]

Reports what this release changes for this installation. For 2.2.2 and 2.2.3:
whether a global administrator exists, which agency-scoped grants lose
abilities, which service accounts, Vault-backed secrets and host records are
affected. For 2.3.0: rows and runners in several agencies, where shell jobs may
run once the SSH executor becomes the local runner, hosts with no stored host
key. Read-only; safe to run against a live database.`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	// The configuration is read for two things: the database path when -db is
	// not given, and what the SSH executor's switch says here — the 2.3.0
	// upgrade pass acts on it. A configuration that does not load is fatal only
	// for the first.
	cfg, cfgErr := config.Load()
	target := *dbPath
	if target == "" {
		if cfgErr != nil {
			fmt.Fprintln(os.Stderr, "preflight: load config:", cfgErr)
			return 1
		}
		target = cfg.DBPath
	}
	if _, err := os.Stat(target); err != nil {
		fmt.Fprintln(os.Stderr, "preflight: database not found at", target, "— pass -db")
		return 1
	}
	pool, err := db.Open(target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "preflight: open database:", err)
		return 1
	}
	defer func() { _ = pool.Close() }()
	ctx := context.Background()
	// Belt and braces: nothing below writes, and this makes sure of it. The
	// pragma is per connection, so the pool is held to one.
	pool.SetMaxOpenConns(1)
	if _, err := pool.ExecContext(ctx, `PRAGMA query_only = ON`); err != nil {
		fmt.Fprintln(os.Stderr, "preflight: set read-only:", err)
		return 1
	}
	rep, err := preflight.Build(ctx, pool)
	if err != nil {
		fmt.Fprintln(os.Stderr, "preflight:", err)
		fmt.Fprintln(os.Stderr, "preflight: this report reads the 2.2.x schema; upgrade to 2.2.1 first if the database is older.")
		return 1
	}
	rep.Write(os.Stdout)

	fmt.Println()
	var schema int
	if err := pool.QueryRowContext(ctx, `SELECT version FROM schema_migrations LIMIT 1`).Scan(&schema); err != nil {
		fmt.Fprintln(os.Stderr, "preflight: read schema version:", err)
		return 1
	}
	if schema < lastSchema22 {
		// The section reads tables 2.2 added (scope_runners, for one). An older
		// database is two upgrades away, and the first one changes what this
		// section would report.
		fmt.Printf("Cronomicon 2.3.0 — not reported: this database is on schema v%d, older than 2.2 (v%d).\n", schema, lastSchema22)
		fmt.Println("Upgrade to 2.2.3 first, then run `cronomicon preflight` again before 2.3.0.")
	} else if schema >= firstSchema230 {
		// The section reads the 2.2 tables. On a database already upgraded,
		// what it would say is in the app, and current.
		fmt.Println("Cronomicon 2.3.0 — this database is already on the 2.3.0 schema.")
		fmt.Println("What the upgrade found is in the app under Notices.")
	} else {
		rep230, err := preflight.Build230(ctx, pool)
		if err != nil {
			fmt.Fprintln(os.Stderr, "preflight:", err)
			return 1
		}
		var sshExecutor *bool
		if cfgErr == nil {
			sshExecutor = &cfg.SSHExecutorEnabled
		}
		rep230.Write(os.Stdout, sshExecutor)
	}
	if !rep.HasGlobalAdmin() {
		return 3
	}
	return 0
}

// firstSchema230 is the first migration of 2.3.0 (1220_global_agency). A
// database below it holds the 2.2 tables the 2.3.0 preflight section reads.
const firstSchema230 = 1220

// lastSchema22 is the schema of the 2.2 line (1210_runner_known_hosts), the
// oldest the 2.3.0 section can read.
const lastSchema22 = 1210

// upgradeReportMarker records that the 2.2.2 report has been logged, so it is
// said once and not on every boot.
const upgradeReportMarker = "upgradeReport.2.2.2"

// logUpgradeReport writes the 2.2.2 preflight findings to the process log the
// first time this release boots against a database (GC-19). A fresh
// installation — no access grants at all — has nothing to report: the existing
// first-boot lockout warning already covers it.
func logUpgradeReport(ctx context.Context, pool *sql.DB, logger *slog.Logger) {
	var seen string
	if err := pool.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, upgradeReportMarker).Scan(&seen); err == nil {
		return
	}
	defer func() {
		_, _ = pool.ExecContext(ctx, `
			INSERT INTO settings (key, value, last_modified_by, last_modified_at)
			VALUES (?, 'logged', 'upgrade', datetime('now'))
			ON CONFLICT(key) DO NOTHING`, upgradeReportMarker)
	}()
	var grants int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM access_grants`).Scan(&grants); err != nil || grants == 0 {
		return
	}
	rep, err := preflight.Build(ctx, pool)
	if err != nil {
		logger.Warn("upgrade report (2.2.2) could not be built — run `cronomicon preflight`", "error", err)
		return
	}
	if rep.Quiet() {
		return
	}
	logger.Warn("upgrade 2.2.2: install-wide changes now need a global administrator (a role on every agency "+
		"that carries the permission) — run `cronomicon preflight` for the full report",
		"global_admin_groups", len(rep.GlobalAdminGroups), "agency_grants_affected", len(rep.AgencyGrants),
		"service_accounts_to_review", len(rep.ServiceAccounts), "agency_vault_secrets", len(rep.VaultSecrets))
	for _, g := range rep.AgencyGrants {
		logger.Warn("upgrade 2.2.2: this agency-scoped grant no longer reaches install-wide surfaces",
			"ad_group", g.ADGroup, "role", g.Role, "agency", g.Agency, "abilities_lost", len(g.Loses))
	}
	for _, s := range rep.ServiceAccounts {
		logger.Warn("upgrade 2.2.2: review this service account — its creator could not mint it under the new rules",
			"name", s.Name, "role", s.Role, "where", s.Where, "created_by", s.CreatedBy, "why", s.Why)
	}
}

// hostKeyReportMarker records that the 2.2.3 host-key findings have been
// logged. It is its own marker: an installation that already ran 2.2.2 has the
// 2.2.2 marker set and would otherwise never hear about this.
const hostKeyReportMarker = "upgradeReport.2.2.3"

// logHostKeyReport says, once, which host records the in-app SSH executor will
// no longer connect for (v2.2.3): a record whose key belongs to an agency other
// than the one whose runs use the host. Silent when there are none, and when
// the SSH executor is off there is nothing to break — it still says so, since
// the switch may be turned on later.
func logHostKeyReport(ctx context.Context, pool *sql.DB, logger *slog.Logger, sshExecutorEnabled bool) {
	var seen string
	if err := pool.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, hostKeyReportMarker).Scan(&seen); err == nil {
		return
	}
	defer func() {
		_, _ = pool.ExecContext(ctx, `
			INSERT INTO settings (key, value, last_modified_by, last_modified_at)
			VALUES (?, 'logged', 'upgrade', datetime('now'))
			ON CONFLICT(key) DO NOTHING`, hostKeyReportMarker)
	}()
	rep, err := preflight.Build(ctx, pool)
	if err != nil {
		logger.Warn("upgrade report (2.2.3) could not be built — run `cronomicon preflight`", "error", err)
		return
	}
	for _, h := range rep.HostKeys {
		where := "scope " + h.Scope
		effect := "runs fail for this host"
		if h.Scope == "" {
			where = "every scope (a manually authored record)"
			effect = "only the key's own agency's runs connect"
		}
		logger.Warn("upgrade 2.2.3: this host record names an agency's SSH key, which the in-app SSH executor "+
			"now loads only for that agency's runs — give the host a key of its own agency, or make the key shared",
			"host", h.Host, "used_by", where, "key", h.Key, "key_agencies", strings.Join(h.KeyAgencies, ", "),
			"effect", effect, "ssh_executor_enabled", sshExecutorEnabled)
	}
}

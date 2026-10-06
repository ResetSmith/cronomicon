package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/preflight"
)

// runPreflight implements `cronomicon preflight` (GC-19): a read-only report of
// what this release changes for the installation the database belongs to. Run
// it with the NEW binary against the EXISTING database, before upgrading. It
// runs no migration and writes nothing.
//
// Exit status: 0 when the report was produced, 3 when it found that no global
// administrator exists (the one finding that makes the upgrade unsafe to take
// unattended), 1 on an error, 2 on bad usage.
func runPreflight(args []string) int {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	dbPath := fs.String("db", "", "database path (default: CRONOMICON_DB_PATH from config)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: cronomicon preflight [-db path]

Reports what this release changes for this installation: whether a global
administrator exists, which agency-scoped grants lose abilities, which service
accounts and Vault-backed secrets are affected. Read-only; safe to run against
a live database.`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	target := *dbPath
	if target == "" {
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, "preflight: load config:", err)
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
	defer pool.Close()
	ctx := context.Background()
	// Belt and braces: nothing below writes, and this makes sure of it.
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
	if !rep.HasGlobalAdmin() {
		return 3
	}
	return 0
}

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

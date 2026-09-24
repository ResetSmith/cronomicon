package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// `amadeus grant-admin` — the break-glass recovery path for an ADMIN LOCKOUT
// (RF-25 / RB-Q15, the RBAC-fixes plan; RF-Q4 resolved 2026-08-04).
//
// Why this exists: on an OIDC deployment, CRONOMICON_BOOTSTRAP_ADMIN_GROUP does
// NOTHING — the floor is applied on the trusted-header login path only, and the
// OIDC path never calls it. Before this subcommand, an OIDC instance whose
// grants were broken (group renamed in the IdP, last admin grant deleted, a bad
// bulk edit) had no recovery except hand-editing SQLite. The administrator
// manual used to document the env var as the lockout lever with no mode
// qualifier, which was worse than no documentation: the procedure silently does
// not work in the mode most production installs run.
//
// Why a CLI subcommand rather than extending the floor to OIDC: the floor would
// grant admin in a mode where it currently does not — a privilege change — and a
// forgotten env var becomes a standing skeleton key. A subcommand runs offline,
// against the database file, only for someone who already has shell access to
// the host (a boundary Cronomicon cannot enforce anyway), leaves an audit trail,
// and changes no login path.
//
// The grant is written for an AD GROUP, because grants are keyed by AD group —
// an email has no grant row. Passing an email is supported as a LOOKUP: it
// prints that user's recorded groups from recent_logins so the operator can
// re-run with the right one. It deliberately does not auto-pick: granting admin
// to a group grants it to EVERYONE in that group, and choosing which group gets
// that must be a human act performed with the group name in view.
func runGrantAdmin(args []string) int {
	fs := flag.NewFlagSet("grant-admin", flag.ContinueOnError)
	dbPath := fs.String("db", "", "database path (default: CRONOMICON_DB_PATH from config)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: amadeus grant-admin [-db path] <ad-group | email>

Break-glass admin recovery. STOP THE SERVER FIRST if it is running against the
same database file.

  <ad-group>  writes an unrestricted admin grant for this AD group. Everyone the
              identity provider places in the group holds full admin on their
              next login.
  <email>     looks the user up in recent logins and prints their recorded AD
              groups, so you can re-run with the one you mean. Nothing is written.`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	subject := strings.TrimSpace(fs.Arg(0))

	target := *dbPath
	if target == "" {
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, "grant-admin: load config:", err)
			return 1
		}
		target = cfg.DBPath
	}
	if _, err := os.Stat(target); err != nil {
		fmt.Fprintln(os.Stderr, "grant-admin: database not found at", target, "— pass -db")
		return 1
	}
	pool, err := db.Open(target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "grant-admin: open database:", err)
		return 1
	}
	defer pool.Close()
	ctx := context.Background()

	// The email form: a lookup, never a write (see the header comment).
	if strings.Contains(subject, "@") {
		var groupsJSON string
		err := pool.QueryRowContext(ctx,
			`SELECT groups FROM recent_logins WHERE email = ? COLLATE NOCASE`, subject).Scan(&groupsJSON)
		if err != nil {
			fmt.Fprintf(os.Stderr, "grant-admin: no recent login recorded for %s — Cronomicon only knows "+
				"users who have signed in (the Honest View). Pass the AD group name directly.\n", subject)
			return 1
		}
		var groups []string
		_ = json.Unmarshal([]byte(groupsJSON), &groups)
		if len(groups) == 0 {
			fmt.Fprintf(os.Stderr, "grant-admin: %s has no recorded AD groups. Pass a group name directly.\n", subject)
			return 1
		}
		fmt.Printf("%s last signed in with these AD groups:\n", subject)
		for _, g := range groups {
			fmt.Printf("  %s\n", g)
		}
		fmt.Println("\nRe-run with the group that should hold admin — the grant applies to EVERYONE in it:")
		fmt.Printf("  amadeus grant-admin %q\n", groups[0])
		return 0
	}

	// The write. An unrestricted admin grant, exactly what the Access Grants UI
	// would create — so the recovered admin sees a normal row they can later
	// delete, not an invisible backdoor.
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := pool.ExecContext(ctx, `
		INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_by, created_at)
		SELECT ?, ?, 'admin', NULL, 1, 'grant-admin (break-glass CLI)', ?
		WHERE NOT EXISTS (
			SELECT 1 FROM access_grants
			WHERE ad_group = ? AND role = 'admin' AND all_scopes = 1)`,
		db.NewID(), subject, now, subject)
	if err != nil {
		fmt.Fprintln(os.Stderr, "grant-admin: write grant:", err)
		return 1
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		fmt.Printf("%s already holds an unrestricted admin grant — nothing to do.\n", subject)
		fmt.Println("If logins still fail, the group name may not match the IdP claim exactly:")
		fmt.Println("AD-group matching is case-sensitive at login.")
		return 0
	}

	// The audit row is the point of doing this through a tool instead of sqlite3:
	// a break-glass grant that leaves no trace is indistinguishable from an
	// attacker's. Best-effort — recovery must not fail on an audit hiccup.
	// Through the shared writer, not a raw INSERT: auditlog is the single
	// writer for the audit tables (AM-3 pins that repo-wide), so the row gets
	// the same masking and bounding as every other auth event.
	_ = auditlog.WriteAuthEvent(ctx, pool, auditlog.AuthEventParams{
		Kind:    auditlog.AuthBootstrapAdmin,
		Outcome: auditlog.OutcomeSuccess,
		Actor:   "grant-admin (break-glass CLI)",
		Reason:  "unrestricted admin grant written offline",
		Target:  subject,
	})

	fmt.Printf("Wrote an unrestricted admin grant for AD group %q.\n\n", subject)
	fmt.Println("Members of that group hold FULL ADMIN on their next login (sessions resolve")
	fmt.Println("grants at login, so no restart is needed, but an already-open session must")
	fmt.Println("sign out and back in). Group matching is case-sensitive: the name above must")
	fmt.Println("match the IdP's groups claim exactly.")
	fmt.Println("\nWhen the lockout is resolved, review Settings → Users & Access and remove")
	fmt.Println("this grant if it should not be permanent.")
	return 0
}

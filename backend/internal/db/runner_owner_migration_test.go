package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// TestMigrate1250RunnerOwner pins what the runner-owner migration does to an
// installation that exists (LR-64): who owns each runner afterwards, that
// nothing about what a runner SERVES changes, and what its triggers hold.
func TestMigrate1250RunnerOwner(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = pool.Close() }()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(1240); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to 1240: %v", err)
	}

	const ts = "2026-10-01T00:00:00Z"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	str := func(q string, args ...any) string {
		t.Helper()
		var s sql.NullString
		if err := pool.QueryRow(q, args...).Scan(&s); err != nil {
			t.Fatalf("query: %v\n%s", err, q)
		}
		return s.String
	}
	serves := func(runner string) string {
		t.Helper()
		return str(`SELECT COALESCE(group_concat(agency_id, ','), '') FROM (
		              SELECT agency_id FROM runner_agencies WHERE runner_id = ? ORDER BY agency_id)`, runner)
	}

	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', ?), ('ag-tax', 'Tax', ?)`, ts, ts)
	// The four shapes a 2.2 installation can hold. Every runner is born with a
	// Global row at this schema; naming an agency takes it out of Global.
	for _, r := range []struct {
		id       string
		agencies []string
	}{
		{"r-one", []string{"ag-fin"}},
		{"r-global", nil},
		{"r-several", []string{"ag-fin", "ag-tax"}},
		{"r-none", nil}, // damage: its Global row is deleted below
	} {
		exec(`INSERT INTO runners (id, name, status, registered_at, created_at) VALUES (?, ?, 'online', ?, ?)`, r.id, r.id, ts, ts)
		for _, a := range r.agencies {
			exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES (?, ?)`, r.id, a)
		}
	}
	exec(`DELETE FROM runner_agencies WHERE runner_id = 'r-none'`)

	// Snapshots of runners already deregistered, and a token already minted.
	for id, agencies := range map[string]string{
		"h-one":     `["ag-fin"]`,
		"h-none":    `[]`,
		"h-several": `["ag-fin","ag-tax"]`,
		"h-junk":    `not json`,
		"h-blank":   `[""]`,
		"h-number":  `[7]`,
	} {
		exec(`INSERT INTO runner_placement_history (runner_id, name, agency_ids, deregistered_at, deregistered_by, deregistered_via)
		      VALUES (?, ?, ?, ?, 'system', 'reaper')`, id, id, agencies, ts)
	}
	exec(`INSERT INTO registration_tokens (token_hash, created_by, created_at, expires_at) VALUES ('h1', 'ops', ?, '2099-01-01T00:00:00Z')`, ts)

	if err := m.Migrate(1250); err != nil {
		t.Fatalf("migrate to 1250: %v", err)
	}

	// LR-64, case by case. What each runner serves is exactly what it served.
	for _, tc := range []struct{ runner, owner, serves, why string }{
		{"r-one", "ag-fin", "ag-fin", "a runner in one agency is owned by it"},
		{"r-global", "global", "global", "a runner in none is Global's and serves Global"},
		{"r-several", "global", "ag-fin,ag-tax", "a runner in several is Global's with its serve list unchanged: a legacy placement"},
		{"r-none", "global", "", "a runner with no serve row at all is Global's; the row it lacks is the orphan check's to report, not this migration's to invent"},
	} {
		if got := str(`SELECT owner_agency FROM runners WHERE id = ?`, tc.runner); got != tc.owner {
			t.Errorf("%s: owner = %q, want %q (%s)", tc.runner, got, tc.owner, tc.why)
		}
		if got := serves(tc.runner); got != tc.serves {
			t.Errorf("%s: serves %q after the migration, want %q unchanged", tc.runner, got, tc.serves)
		}
	}
	for id, want := range map[string]string{
		"h-one": "ag-fin", "h-none": "global", "h-several": "global",
		"h-junk": "global", "h-blank": "global", "h-number": "global",
	} {
		if got := str(`SELECT owner_agency FROM runner_placement_history WHERE runner_id = ?`, id); got != want {
			t.Errorf("snapshot %s: owner = %q, want %q", id, got, want)
		}
	}
	if got := str(`SELECT agency_id FROM registration_tokens WHERE token_hash = 'h1'`); got != "global" {
		t.Errorf("an existing token enrols for %q, want global: what it always would have", got)
	}

	// A runner is born serving its owner, and only its owner: an agency's agent
	// never holds a Global row, even for the length of the insert.
	exec(`INSERT INTO runners (id, name, status, registered_at, created_at, owner_agency) VALUES ('r-new', 'r-new', 'online', ?, ?, 'ag-tax')`, ts, ts)
	if got := serves("r-new"); got != "ag-tax" {
		t.Errorf("a runner born to Tax serves %q, want ag-tax alone", got)
	}
	exec(`INSERT INTO runners (id, name, status, registered_at, created_at) VALUES ('r-default', 'r-default', 'online', ?, ?)`, ts, ts)
	if got := str(`SELECT owner_agency FROM runners WHERE id = 'r-default'`) + "/" + serves("r-default"); got != "global/global" {
		t.Errorf("a runner born with no owner named is %q, want global/global", got)
	}
	// An owner that names no agency fails the insert (LR-33): the serve row has
	// nowhere to point, and there is no falling back into Global.
	if _, err := pool.Exec(`INSERT INTO runners (id, name, status, registered_at, created_at, owner_agency)
	                        VALUES ('r-ghost', 'r-ghost', 'online', ?, ?, 'ag-nowhere')`, ts, ts); err == nil {
		t.Error("a runner was born to an agency that does not exist")
	}
	for _, q := range []string{
		`INSERT INTO runners (id, name, status, registered_at, created_at, owner_agency) VALUES ('r-blank', 'r-blank', 'online', '` + ts + `', '` + ts + `', '')`,
		`UPDATE runners SET owner_agency = '' WHERE id = 'r-one'`,
	} {
		if _, err := pool.Exec(q); err == nil || !strings.Contains(err.Error(), "owner_required") {
			t.Errorf("a runner with no owner was accepted (err %v):\n%s", err, q)
		}
	}
	// An agency that owns a runner cannot be deleted, even one it does not serve.
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-own', 'Owner', ?)`, ts)
	exec(`INSERT INTO runners (id, name, status, registered_at, created_at, owner_agency) VALUES ('r-own', 'r-own', 'online', ?, ?, 'ag-own')`, ts, ts)
	exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('r-own', 'ag-fin')`)
	exec(`DELETE FROM runner_agencies WHERE runner_id = 'r-own' AND agency_id = 'ag-own'`)
	if _, err := pool.Exec(`DELETE FROM agencies WHERE id = 'ag-own'`); err == nil || !strings.Contains(err.Error(), "agency_in_use") {
		t.Errorf("an agency that owns a runner was deleted (err %v)", err)
	}

	// Down: the owner is forgotten, the serve lists stay, and a new runner is
	// born in Global again. An unused token minted for an agency is revoked: at
	// the previous schema it would enrol a general-pool runner, which an
	// agency's administrator could never grant. Global's, and a used one, stay.
	exec(`INSERT INTO registration_tokens (token_hash, created_by, created_at, expires_at, agency_id)
	      VALUES ('h-tax', 'tax-admin', ?, '2099-01-01T00:00:00Z', 'ag-tax')`, ts)
	exec(`INSERT INTO registration_tokens (token_hash, created_by, created_at, expires_at, agency_id, used_at, used_by_runner_id)
	      VALUES ('h-tax-used', 'tax-admin', ?, '2099-01-01T00:00:00Z', 'ag-tax', ?, 'r-new')`, ts, ts)
	if err := m.Migrate(1240); err != nil {
		t.Fatalf("migrate down to 1240: %v", err)
	}
	for hash, wantRevoked := range map[string]bool{"h-tax": true, "h-tax-used": false, "h1": false} {
		if got := str(`SELECT COALESCE(revoked_at, '') FROM registration_tokens WHERE token_hash = ?`, hash) != ""; got != wantRevoked {
			t.Errorf("down: token %s revoked = %v, want %v", hash, got, wantRevoked)
		}
	}
	if got := serves("r-several"); got != "ag-fin,ag-tax" {
		t.Errorf("down: r-several serves %q, want its list unchanged", got)
	}
	if got := serves("r-new"); got != "ag-tax" {
		t.Errorf("down: an agent enrolled by Tax serves %q, want ag-tax still", got)
	}
	exec(`INSERT INTO runners (id, name, status, registered_at, created_at) VALUES ('r-after', 'r-after', 'online', ?, ?)`, ts, ts)
	if got := serves("r-after"); got != "global" {
		t.Errorf("down: a new runner serves %q, want global", got)
	}
	var cols int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runners') WHERE name = 'owner_agency'`).Scan(&cols); err != nil || cols != 0 {
		t.Errorf("down: runners.owner_agency still present (%d, %v)", cols, err)
	}
}

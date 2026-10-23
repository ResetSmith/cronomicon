package db

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// TestMigrate1220GlobalAgency pins what the Global-agency migration does to an
// installation that exists (LR-21, LR-22, LR-27, LR-28) and what its triggers
// hold afterwards. What it does to reference RESOLUTION has a test of its own
// (TestMigrate1220ResolvesEveryReferenceAsBefore).
func TestMigrate1220GlobalAgency(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(1210); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 1210: %v", err)
	}

	const ts = "2026-09-01T00:00:00Z"
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
	refused := func(why, q string, args ...any) {
		t.Helper()
		_, err := pool.Exec(q, args...)
		if err == nil {
			t.Fatalf("accepted, want a refusal mentioning %q:\n%s", why, q)
		}
		if !strings.Contains(err.Error(), why) {
			t.Fatalf("refused with %q, want it to mention %q", err, why)
		}
	}

	// An installation that already has an agency called Global, in another
	// letter case, and a department beside it.
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-old', 'GLOBAL', ?), ('ag-fin', 'Finance', ?)`, ts, ts)

	runner := func(id string) {
		exec(`INSERT INTO runners (id, name, status, os, capabilities, load, max_concurrent, version, protocol_version, registered_at, created_at)
		      VALUES (?, ?, 'online', 'Linux', '["bash"]', 0, 5, '1.0', 14, ?, ?)`, id, id, ts, ts)
	}
	// One of each kind with no agency, and one of each in Finance.
	for _, s := range []string{"free", "fin"} {
		exec(`INSERT INTO scopes (id, name, source, created_at) VALUES (?, ?, 'cronomicon', ?)`, "sc-"+s, s, ts)
		runner("r-" + s)
		exec(`INSERT INTO secrets (id, key, scope, source, created_at) VALUES (?, ?, NULL, 'stored', ?)`, "se-"+s, "S_"+s, ts)
		exec(`INSERT INTO env_vars (id, key, scope, value, created_at) VALUES (?, ?, NULL, 'v', ?)`, "ev-"+s, "V_"+s, ts)
		exec(`INSERT INTO ssh_credentials (id, label, source, created_at) VALUES (?, ?, 'stored', ?)`, "k-"+s, "K_"+s, ts)
	}
	exec(`INSERT INTO scope_agencies VALUES ('sc-fin', 'ag-fin')`)
	exec(`INSERT INTO runner_agencies VALUES ('r-fin', 'ag-fin')`)
	exec(`INSERT INTO secret_agencies VALUES ('se-fin', 'ag-fin')`)
	exec(`INSERT INTO env_var_agencies VALUES ('ev-fin', 'ag-fin')`)
	exec(`INSERT INTO ssh_credential_agencies VALUES ('k-fin', 'ag-fin')`)

	run := func(id, status, agencies string, index ...string) {
		exec(`INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind, executor, created_at, agencies_json)
		      VALUES (?, 'j', 'bash', ?, 'u', 'manual', 'runner', ?, ?)`, id, status, ts, agencies)
		for _, a := range index {
			exec(`INSERT INTO run_agencies (run_id, agency) VALUES (?, ?)`, id, a)
		}
	}
	run("q-none", "queued", `[]`)
	run("r-none", "running", `[]`)
	run("done-none", "success", `[]`)
	run("q-old", "queued", `["GLOBAL","Finance"]`, "GLOBAL", "Finance")
	// Damaged snapshots on runs that are still waiting. None has an index row,
	// so each was general-pool work to the old claim, and json_each on any of
	// them would abort a migration that did not look first.
	run("q-blank", "queued", ``)
	run("q-garbage", "queued", `not json`)
	run("q-object", "running", `{"a":1}`)
	// The same key as a secret Finance owns nothing of, but as a VARIABLE Finance
	// already owns: the secret must not be promoted beside it.
	exec(`INSERT INTO secrets (id, key, scope, source, created_at) VALUES ('se-pair', 'PAIR', 'fin', 'stored', ?)`, ts)
	exec(`INSERT INTO secret_agencies VALUES ('se-pair', 'ag-fin')`)
	exec(`INSERT INTO env_vars (id, key, scope, value, created_at, owner_agency) VALUES ('ev-pair', 'PAIR', 'fin', 'v', ?, 'ag-fin')`, ts)
	exec(`INSERT INTO env_var_agencies VALUES ('ev-pair', 'ag-fin')`)
	run("done-old", "success", `["GLOBAL"]`, "GLOBAL")
	// A parked run: the snapshot is a STRING holding the array.
	exec(`INSERT INTO pending_runs (id, kind, name, source, scope, run_at, scheduled_by, created_at, status, params_json)
	      VALUES ('p1', 'job', 'j', 'cronomicon', '', ?, 'u', ?, 'pending', ?)`, ts, ts,
		`{"JobName":"j","AgenciesJSON":"[\"GLOBAL\"]"}`)

	if err := m.Migrate(1220); err != nil {
		t.Fatalf("migrate to 1220: %v", err)
	}

	t.Run("the built-in agency exists", func(t *testing.T) {
		if got := str(`SELECT name || '/' || builtin FROM agencies WHERE id = 'global'`); got != "Global/1" {
			t.Fatalf("global row = %q, want Global/1", got)
		}
		if got := str(`SELECT COUNT(*) FROM agencies WHERE builtin = 1`); got != "1" {
			t.Fatalf("%s built-in agencies, want 1", got)
		}
	})

	t.Run("an agency that held the name is renamed, with its waiting runs", func(t *testing.T) {
		if got := str(`SELECT name FROM agencies WHERE id = 'ag-old'`); got != "GLOBAL (renamed)" {
			t.Fatalf("name = %q", got)
		}
		if got := str(`SELECT agencies_json FROM runs WHERE id = 'q-old'`); got != `["GLOBAL (renamed)","Finance"]` {
			t.Fatalf("waiting run snapshot = %s", got)
		}
		if got := str(`SELECT group_concat(agency, ',') FROM (SELECT agency FROM run_agencies WHERE run_id = 'q-old' ORDER BY agency)`); got != "Finance,GLOBAL (renamed)" {
			t.Fatalf("waiting run index = %s", got)
		}
		// A finished run is history: it keeps the name it ran under.
		if got := str(`SELECT agencies_json FROM runs WHERE id = 'done-old'`); got != `["GLOBAL"]` {
			t.Fatalf("finished run snapshot = %s", got)
		}
		if got := str(`SELECT json_extract(params_json, '$.AgenciesJSON') FROM pending_runs WHERE id = 'p1'`); got != `["GLOBAL (renamed)"]` {
			t.Fatalf("parked snapshot = %s", got)
		}
		// json_extract renders a string holding an array and an array the same
		// way. The scheduler decodes a STRING; an array here is a parked run it
		// can no longer read, and marks missed.
		if got := str(`SELECT json_type(params_json, '$.AgenciesJSON') FROM pending_runs WHERE id = 'p1'`); got != "text" {
			t.Fatalf("parked snapshot is stored as JSON %s, want the string it was", got)
		}
		var parked struct{ JobName, AgenciesJSON string }
		if err := json.Unmarshal([]byte(str(`SELECT params_json FROM pending_runs WHERE id = 'p1'`)), &parked); err != nil {
			t.Fatalf("the parked run no longer decodes: %v", err)
		}
		var names []string
		if err := json.Unmarshal([]byte(parked.AgenciesJSON), &names); err != nil || len(names) != 1 || names[0] != "GLOBAL (renamed)" {
			t.Fatalf("parked agencies = %v (%v)", names, err)
		}
		if got := str(`SELECT json_extract(params_json, '$.JobName') FROM pending_runs WHERE id = 'p1'`); got != "j" {
			t.Fatalf("parked run lost its other fields: JobName = %q", got)
		}
		if got := str(`SELECT kind || '/' || agency_id || '/' || subject FROM notices`); got != "agency_renamed/global/ag-old" {
			t.Fatalf("notice = %q", got)
		}
		if got := str(`SELECT detail FROM notices`); !strings.Contains(got, `"GLOBAL (renamed)"`) {
			t.Fatalf("notice does not name the new name: %s", got)
		}
	})

	t.Run("what belonged to no agency belongs to Global", func(t *testing.T) {
		for _, c := range []struct{ table, col, free, fin string }{
			{"scope_agencies", "scope_id", "sc-free", "sc-fin"},
			{"runner_agencies", "runner_id", "r-free", "r-fin"},
			{"secret_agencies", "secret_id", "se-free", "se-fin"},
			{"env_var_agencies", "env_var_id", "ev-free", "ev-fin"},
			{"ssh_credential_agencies", "credential_id", "k-free", "k-fin"},
		} {
			q := `SELECT group_concat(agency_id, ',') FROM ` + c.table + ` WHERE ` + c.col + ` = ?`
			if got := str(q, c.free); got != "global" {
				t.Errorf("%s of %s = %q, want global", c.table, c.free, got)
			}
			if got := str(q, c.fin); got != "ag-fin" {
				t.Errorf("%s of %s = %q, want ag-fin alone", c.table, c.fin, got)
			}
		}
		for _, c := range []struct{ table, free, fin string }{
			{"secrets", "se-free", "se-fin"}, {"env_vars", "ev-free", "ev-fin"}, {"ssh_credentials", "k-free", "k-fin"},
		} {
			q := `SELECT owner_agency FROM ` + c.table + ` WHERE id = ?`
			if got := str(q, c.free); got != "global" {
				t.Errorf("%s owner of %s = %q, want global", c.table, c.free, got)
			}
			// Unowned, in exactly one agency: it is that agency's.
			if got := str(q, c.fin); got != "ag-fin" {
				t.Errorf("%s owner of %s = %q, want ag-fin", c.table, c.fin, got)
			}
		}
		// ...unless the agency already owns that key as the other kind: a secret
		// and a variable with one key, scope and owner cannot be saved again.
		if got := str(`SELECT owner_agency FROM secrets WHERE id = 'se-pair'`); got != "global" {
			t.Errorf("a secret was promoted beside its agency's same-named variable: owner = %q", got)
		}
		if got := str(`SELECT agency_id FROM secret_agencies WHERE secret_id = 'se-pair'`); got != "ag-fin" {
			t.Errorf("the held-back secret lost its member agency: %q", got)
		}
	})

	t.Run("a waiting run with no agency is Global's, a finished one is left", func(t *testing.T) {
		for _, id := range []string{"q-none", "r-none", "q-blank", "q-garbage", "q-object"} {
			if got := str(`SELECT agencies_json FROM runs WHERE id = ?`, id); got != `["Global"]` {
				t.Errorf("%s snapshot = %s", id, got)
			}
			if got := str(`SELECT group_concat(agency, ',') FROM run_agencies WHERE run_id = ?`, id); got != "Global" {
				t.Errorf("%s index = %q", id, got)
			}
		}
		if got := str(`SELECT agencies_json FROM runs WHERE id = 'done-none'`); got != `[]` {
			t.Errorf("finished snapshot = %s, want it untouched", got)
		}
		if got := str(`SELECT COUNT(*) FROM run_agencies WHERE run_id = 'done-none'`); got != "0" {
			t.Errorf("finished run gained %s index rows", got)
		}
	})

	t.Run("every new row is born in Global", func(t *testing.T) {
		exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-new', 'new', 'cronomicon', ?)`, ts)
		runner("r-new")
		exec(`INSERT INTO secrets (id, key, scope, source, created_at) VALUES ('se-new', 'S_new', NULL, 'stored', ?)`, ts)
		exec(`INSERT INTO env_vars (id, key, scope, value, created_at) VALUES ('ev-new', 'V_new', NULL, 'v', ?)`, ts)
		exec(`INSERT INTO ssh_credentials (id, label, source, created_at) VALUES ('k-new', 'K_new', 'stored', ?)`, ts)
		for q, id := range map[string]string{
			`SELECT agency_id FROM scope_agencies WHERE scope_id = ?`:               "sc-new",
			`SELECT agency_id FROM runner_agencies WHERE runner_id = ?`:             "r-new",
			`SELECT agency_id FROM secret_agencies WHERE secret_id = ?`:             "se-new",
			`SELECT agency_id FROM env_var_agencies WHERE env_var_id = ?`:           "ev-new",
			`SELECT agency_id FROM ssh_credential_agencies WHERE credential_id = ?`: "k-new",
			`SELECT owner_agency FROM secrets WHERE id = ?`:                         "se-new",
			`SELECT owner_agency FROM env_vars WHERE id = ?`:                        "ev-new",
			`SELECT owner_agency FROM ssh_credentials WHERE id = ?`:                 "k-new",
		} {
			if got := str(q, id); got != "global" {
				t.Errorf("%s (%s) = %q, want global", q, id, got)
			}
		}
		// And an UPDATE cannot take the owner away again.
		refused("owner_required", `UPDATE secrets SET owner_agency = '' WHERE id = 'se-new'`)
		refused("owner_required", `UPDATE env_vars SET owner_agency = '' WHERE id = 'ev-new'`)
		refused("owner_required", `UPDATE ssh_credentials SET owner_agency = '' WHERE id = 'k-new'`)
		// A row created WITH an owner keeps it; only its membership defaults.
		exec(`INSERT INTO secrets (id, key, scope, source, created_at, owner_agency) VALUES ('se-own', 'S_own', NULL, 'stored', ?, 'ag-fin')`, ts)
		if got := str(`SELECT owner_agency FROM secrets WHERE id = 'se-own'`); got != "ag-fin" {
			t.Errorf("an owned row's owner was overwritten: %q", got)
		}

		run("q-new", "queued", `[]`)
		if got := str(`SELECT agencies_json FROM runs WHERE id = 'q-new'`); got != `["Global"]` {
			t.Errorf("new waiting run snapshot = %s", got)
		}
		run("q-new-blank", "queued", ``)
		if got := str(`SELECT agencies_json FROM runs WHERE id = 'q-new-blank'`); got != `["Global"]` {
			t.Errorf("new waiting run with a blank snapshot = %s", got)
		}
		if got := str(`SELECT group_concat(agency, ',') FROM run_agencies WHERE run_id = 'q-new'`); got != "Global" {
			t.Errorf("new waiting run index = %q", got)
		}
		// A row written finished (a skip, a missed fire) is not a claim on anyone.
		run("skip-new", "skipped", `[]`)
		if got := str(`SELECT agencies_json FROM runs WHERE id = 'skip-new'`); got != `[]` {
			t.Errorf("terminal row snapshot = %s, want it untouched", got)
		}
		run("q-fin", "queued", `["Finance"]`, "Finance")
		if got := str(`SELECT agencies_json FROM runs WHERE id = 'q-fin'`); got != `["Finance"]` {
			t.Errorf("an agency's run was restamped: %s", got)
		}
	})

	t.Run("Global or an agency, never both and never neither by accident", func(t *testing.T) {
		// Naming an agency takes the row out of Global.
		exec(`INSERT INTO scope_agencies VALUES ('sc-new', 'ag-fin')`)
		exec(`INSERT INTO runner_agencies VALUES ('r-new', 'ag-fin')`)
		exec(`INSERT INTO secret_agencies VALUES ('se-new', 'ag-fin')`)
		exec(`INSERT INTO env_var_agencies VALUES ('ev-new', 'ag-fin')`)
		exec(`INSERT INTO ssh_credential_agencies VALUES ('k-new', 'ag-fin')`)
		for q, id := range map[string]string{
			`SELECT group_concat(agency_id, ',') FROM scope_agencies WHERE scope_id = ?`:               "sc-new",
			`SELECT group_concat(agency_id, ',') FROM runner_agencies WHERE runner_id = ?`:             "r-new",
			`SELECT group_concat(agency_id, ',') FROM secret_agencies WHERE secret_id = ?`:             "se-new",
			`SELECT group_concat(agency_id, ',') FROM env_var_agencies WHERE env_var_id = ?`:           "ev-new",
			`SELECT group_concat(agency_id, ',') FROM ssh_credential_agencies WHERE credential_id = ?`: "k-new",
		} {
			if got := str(q, id); got != "ag-fin" {
				t.Errorf("%s (%s) = %q, want ag-fin alone", q, id, got)
			}
		}
		// Global cannot be added beside it.
		refused("global_mixed", `INSERT INTO scope_agencies VALUES ('sc-new', 'global')`)
		refused("global_mixed", `INSERT INTO runner_agencies VALUES ('r-new', 'global')`)
		refused("global_mixed", `INSERT INTO secret_agencies VALUES ('se-new', 'global')`)
		refused("global_mixed", `INSERT INTO env_var_agencies VALUES ('ev-new', 'global')`)
		refused("global_mixed", `INSERT INTO ssh_credential_agencies VALUES ('k-new', 'global')`)
		// Removing the last agency does NOT put the row back in Global: making
		// something Global is a decision (a Global secret is every agency's to
		// use), never the side effect of a delete. The setters refuse to leave a
		// row here; the readers treat it as an error.
		exec(`DELETE FROM secret_agencies WHERE secret_id = 'se-new'`)
		if got := str(`SELECT COUNT(*) FROM secret_agencies WHERE secret_id = 'se-new'`); got != "0" {
			t.Errorf("a delete put the secret back in Global (%s rows)", got)
		}
	})

	t.Run("the built-in agency is permanent and its name is reserved", func(t *testing.T) {
		refused("builtin_agency", `DELETE FROM agencies WHERE id = 'global'`)
		refused("builtin_agency", `UPDATE agencies SET name = 'Everyone' WHERE id = 'global'`)
		refused("builtin_agency", `UPDATE agencies SET builtin = 0 WHERE id = 'global'`)
		refused("reserved", `INSERT INTO agencies (id, name, created_at) VALUES ('ag-x', 'gLoBaL', ?)`, ts)
		refused("reserved", `UPDATE agencies SET name = 'global' WHERE id = 'ag-fin'`)
		// Its description is an ordinary field.
		exec(`UPDATE agencies SET description = 'ours' WHERE id = 'global'`)
	})

	t.Run("an agency that holds anything cannot be deleted from under it", func(t *testing.T) {
		// Every membership table cascades from agencies: without this, deleting
		// Finance would leave each of its rows in no agency at all.
		refused("agency_in_use", `DELETE FROM agencies WHERE id = 'ag-fin'`)
		if got := str(`SELECT COUNT(*) FROM scope_agencies WHERE scope_id = 'sc-fin' AND agency_id = 'ag-fin'`); got != "1" {
			t.Fatalf("the refused delete removed a membership row")
		}
		// One reason at a time: a member, then only an owned row.
		exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-tmp', 'Temp', ?)`, ts)
		exec(`INSERT INTO runner_agencies VALUES ('r-free', 'ag-tmp')`)
		refused("agency_in_use", `DELETE FROM agencies WHERE id = 'ag-tmp'`)
		exec(`DELETE FROM runner_agencies WHERE runner_id = 'r-free'`)
		exec(`INSERT INTO runner_agencies VALUES ('r-free', 'global')`)
		exec(`UPDATE ssh_credentials SET owner_agency = 'ag-tmp' WHERE id = 'k-free'`)
		refused("agency_in_use", `DELETE FROM agencies WHERE id = 'ag-tmp'`)
		exec(`UPDATE ssh_credentials SET owner_agency = 'global' WHERE id = 'k-free'`)
		exec(`DELETE FROM agencies WHERE id = 'ag-tmp'`)
		// An agency with nothing in it goes, as before.
		exec(`DELETE FROM agencies WHERE id = 'ag-old'`)
	})

	t.Run("down removes what up added", func(t *testing.T) {
		if err := m.Migrate(1210); err != nil {
			t.Fatalf("migrate down to 1210: %v", err)
		}
		if got := str(`SELECT COUNT(*) FROM agencies WHERE id = 'global'`); got != "0" {
			t.Errorf("the Global row survived the down migration")
		}
		if got := str(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'notices' OR name LIKE '%born_global' OR name LIKE '%leave_global'`); got != "0" {
			t.Errorf("%s of the migration's objects survived the down migration", got)
		}
		if got := str(`SELECT agencies_json FROM runs WHERE id = 'q-none'`); got != `[]` {
			t.Errorf("waiting run snapshot after down = %s", got)
		}
		if got := str(`SELECT COUNT(*) FROM scope_agencies WHERE scope_id = 'sc-free'`); got != "0" {
			t.Errorf("sc-free still has %s agency rows after down", got)
		}
		if got := str(`SELECT owner_agency FROM secrets WHERE id = 'se-free'`); got != "" {
			t.Errorf("se-free owner after down = %q, want unowned", got)
		}
		// And up again over the result, as a re-upgrade would.
		if err := m.Migrate(1220); err != nil {
			t.Fatalf("migrate back up to 1220: %v", err)
		}
		if got := str(`SELECT agency_id FROM scope_agencies WHERE scope_id = 'sc-free'`); got != "global" {
			t.Errorf("sc-free after the second upgrade = %q", got)
		}
	})
}

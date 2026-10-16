package db

import "testing"

// TestMigrate1380LocalRunnerGlobalOnly: the local runner serves Global only
// (§0a, IN-3). The agencies it served are recorded, for the notice, and taken
// off its list; agents are untouched. Then the way back.
func TestMigrate1380LocalRunnerGlobalOnly(t *testing.T) {
	h := openAt(t, 1370)
	h.exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	runner := func(id, kind, owner string) {
		t.Helper()
		h.exec(`INSERT INTO runners (id, name, kind, status, os, capabilities, load, max_concurrent, version, registered_at, created_at, owner_agency)
		        VALUES (?, ?, ?, 'online', 'Linux', '["bash"]', 0, 1, '1', 't', 't', ?)`, id, id, kind, owner)
		h.exec(`DELETE FROM runner_agencies WHERE runner_id = ?`, id)
	}
	runner("local", "server", "global")
	runner("agent-fin", "agent", "ag-fin")
	h.exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('local', 'ag-fin'), ('local', 'ag-tax'), ('agent-fin', 'ag-fin')`)

	h.to(1380)
	if got := h.str(`SELECT group_concat(agency_id) FROM (SELECT agency_id FROM runner_agencies WHERE runner_id = 'local' ORDER BY agency_id)`); got != "global" {
		t.Errorf("the local runner serves %q after the migration, want global alone", got)
	}
	if got := h.str(`SELECT value FROM settings WHERE key = 'upgrade.2.4.0.localRunnerServed'`); got != `["ag-fin","ag-tax"]` {
		t.Errorf("the record of what it served is %q", got)
	}
	if got := h.str(`SELECT group_concat(agency_id) FROM runner_agencies WHERE runner_id = 'agent-fin'`); got != "ag-fin" {
		t.Errorf("an agent's serve list was changed: %q", got)
	}

	h.to(1370)
	if got := h.str(`SELECT group_concat(agency_id) FROM (SELECT agency_id FROM runner_agencies WHERE runner_id = 'local' ORDER BY agency_id)`); got != "ag-fin,ag-tax" {
		t.Errorf("the local runner serves %q after the way back, want ag-fin,ag-tax", got)
	}
	if n := h.count(`SELECT COUNT(*) FROM settings WHERE key = 'upgrade.2.4.0.localRunnerServed'`); n != 0 {
		t.Errorf("the record outlived the way back")
	}
}

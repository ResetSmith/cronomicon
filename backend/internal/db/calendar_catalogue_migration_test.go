package db

import "testing"

// TestMigrate1700CalendarCatalogue: every calendar gets a uid of its own, one
// a writer does not give is generated, and none ever changes; a calendar of
// the built-in agency's may not be a copy (2.5.3 CA4a, CA-25). Nothing is
// linked by the upgrade. Then the way back.
func TestMigrate1700CalendarCatalogue(t *testing.T) {
	h := openAt(t, 1670)
	h.exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't')`)
	h.exec(`INSERT INTO calendars (name, created_at, owner_agency) VALUES ('holidays', 't', 'global'), ('holidays', 't', 'ag-fin')`)

	h.to(1700)
	if n := h.count(`SELECT COUNT(DISTINCT uid) FROM calendars WHERE COALESCE(uid, '') <> ''`); n != 2 {
		t.Errorf("%d distinct uids for the two upgraded calendars, want 2", n)
	}
	if n := h.count(`SELECT COUNT(*) FROM calendars WHERE origin_uid IS NOT NULL OR update_mode IS NOT NULL OR origin_gone <> 0`); n != 0 {
		t.Errorf("the upgrade linked %d calendars, want none (1430's copies stay the agencies' own)", n)
	}
	h.exec(`INSERT INTO calendars (name, created_at, owner_agency) VALUES ('freeze', 't', 'ag-fin')`)
	if got := h.str(`SELECT COALESCE(uid, '') FROM calendars WHERE owner_agency = 'ag-fin' AND name = 'freeze'`); len(got) != 32 {
		t.Errorf("a calendar inserted with no uid has %q, want one generated", got)
	}
	catalogueUID := h.str(`SELECT uid FROM calendars WHERE owner_agency = 'global' AND name = 'holidays'`)
	if _, err := h.pool.Exec(`UPDATE calendars SET uid = 'other' WHERE owner_agency = 'global' AND name = 'holidays'`); err == nil {
		t.Errorf("a calendar's uid was changed")
	}
	if _, err := h.pool.Exec(`INSERT INTO calendars (name, created_at, owner_agency, origin_uid, origin_hash, update_mode) VALUES ('copy', 't', 'global', ?, 'h', 'ask')`, catalogueUID); err == nil {
		t.Errorf("a calendar of the built-in agency's was made a copy")
	}
	h.exec(`INSERT INTO calendars (name, created_at, owner_agency, origin_uid, origin_hash, update_mode) VALUES ('copy', 't', 'ag-fin', ?, 'h', 'ask')`, catalogueUID)
	if _, err := h.pool.Exec(`UPDATE calendars SET update_mode = 'sometimes' WHERE name = 'copy'`); err == nil {
		t.Errorf("an update mode other than ask or auto was stored")
	}

	h.to(1670)
	if n := h.count(`SELECT COUNT(*) FROM calendars`); n != 4 {
		t.Errorf("%d calendars after the way back, want the four, the copy an ordinary calendar", n)
	}
}

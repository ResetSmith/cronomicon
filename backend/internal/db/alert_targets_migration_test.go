package db

import "testing"

// 1540 (2.5.0 C4, AL-2): the stored push targets become Global's rows, in
// their order and as enabled as they were; an element that is not an object
// is skipped, as its reader skipped it. Every existing rule is linked to every
// target, so each pushes where it pushed. An agency that owns a target is not
// deleted. Down and up again with the rows present.
func TestMigrate1540PushTargetsBecomeGlobalsRows(t *testing.T) {
	pool, m := migrateTo(t, "alert-targets.db", 1530)
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	must(`INSERT INTO notification_config (id, apprise_targets, last_modified_by, last_modified_at) VALUES (1,
	      '[{"label":"Ops","service":"slack","url":"slack://a/b/c","enabled":true},
	        "not-an-object",
	        {"label":"Old","url":"mailto://old@example","enabled":false},
	        {"url":"json://hook"}]', 'admin', '2026-01-01T00:00:00Z')`)
	for _, id := range []string{"r1", "r2"} {
		must(`INSERT INTO alert_config (id, target_mode, trigger, channels, enabled, created_at) VALUES (?, 'all', 'failure', '["apprise"]', 1, 't')`, id)
	}
	if err := m.Migrate(1540); err != nil {
		t.Fatalf("migrate 1540: %v", err)
	}
	got := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(`SELECT COALESCE(group_concat(owner_agency||'|'||label||'|'||service||'|'||url||'|'||enabled, ';'), '')
		      FROM (SELECT * FROM alert_targets ORDER BY position)`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	const want = "global|Ops|slack|slack://a/b/c|1;global|Old||mailto://old@example|0;global|||json://hook|0"
	if g := got(); g != want {
		t.Errorf("targets after 1540:\n got %s\nwant %s", g, want)
	}
	var links int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM alert_rule_targets`).Scan(&links)
	if links != 6 {
		t.Errorf("links = %d, want 6: each of two rules to each of three targets", links)
	}
	// A rule deleted takes its links; so does a target.
	must(`PRAGMA foreign_keys = ON`)
	must(`DELETE FROM alert_config WHERE id = 'r2'`)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM alert_rule_targets`).Scan(&links)
	if links != 3 {
		t.Errorf("links after a rule's delete = %d, want 3", links)
	}

	must(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't')`)
	must(`INSERT INTO alert_targets (id, owner_agency, url, created_at) VALUES ('t-fin', 'ag-fin', 'json://fin', 't')`)
	if _, err := pool.Exec(`DELETE FROM agencies WHERE id = 'ag-fin'`); err == nil {
		t.Error("an agency that owns a push target was deleted")
	}
	must(`DELETE FROM alert_targets WHERE id = 't-fin'`)

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := m.Migrate(1540); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if g := got(); g != want {
		t.Errorf("targets after down and up:\n got %s\nwant %s", g, want)
	}
}

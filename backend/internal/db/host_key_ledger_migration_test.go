package db

import (
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// TestMigrate1200BackfillsResolvedHostKeys pins what 1200 carries over from
// pending_host_keys (SB): every decision still on record becomes a ledger row,
// keyed on the host as the known_hosts line names it, with the delivery state
// it had — so an approval that had not reached its runner before the upgrade
// is still delivered after it, and one that had is not delivered twice.
func TestMigrate1200BackfillsResolvedHostKeys(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "hk.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(1190); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 1190: %v", err)
	}

	const ts = "2026-09-01T00:00:00Z"
	if _, err := pool.Exec(`
		INSERT INTO runners (id, name, status, os, capabilities, load, max_concurrent, version, protocol_version, registered_at, created_at)
		VALUES ('r1', 'runner-dmz-01', 'online', 'Linux', '["bash"]', 0, 5, '1.0', 13, ?, ?)`, ts, ts); err != nil {
		t.Fatalf("seed runner: %v", err)
	}
	for _, p := range []struct {
		id, runner, host, line                 string
		approvedAt, approvedBy, rejAt, trusted any
	}{
		// approved and delivered
		{"p1", "r1", "web01", "web01 ssh-ed25519 AAAAkey1", ts, "alice", nil, ts},
		// approved, not yet delivered, on a non-default port: the typed form and
		// the known_hosts form differ
		{"p2", "r1", "db01:2222", "[db01]:2222 ssh-ed25519 AAAAkey2", ts, "alice", nil, nil},
		// rejected
		{"p3", "r1", "evil", "evil ssh-rsa AAAAkey3", nil, nil, ts, nil},
		// never resolved: not a decision, so not in the ledger
		{"p4", "r1", "web09", "web09 ssh-ed25519 AAAAkey4", nil, nil, nil, nil},
		// approved for a runner that no longer exists
		{"p5", "gone", "web01", "web01 ssh-ed25519 AAAAkey5", ts, "bob", nil, ts},
		// the same host as p1 under its other typed form, approved LATER: one
		// known_hosts host, so only one of the two may stay in force
		{"p6", "r1", "web01:22", "web01 ssh-ed25519 AAAAkey6", "2026-09-02T00:00:00Z", "dave", nil, nil},
	} {
		var rejBy any
		if p.rejAt != nil {
			rejBy = "carol"
		}
		if _, err := pool.Exec(`
			INSERT INTO pending_host_keys
			    (id, runner_id, host, key_type, fingerprint, known_hosts_line, scanned_at,
			     approved_at, approved_by, rejected_at, rejected_by, trusted_at)
			VALUES (?, ?, ?, 'ssh-ed25519', 'SHA256:' || ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.id, p.runner, p.host, p.id, p.line, ts, p.approvedAt, p.approvedBy, p.rejAt, rejBy, p.trusted); err != nil {
			t.Fatalf("seed %s: %v", p.id, err)
		}
	}

	if err := m.Migrate(1200); err != nil {
		t.Fatalf("migrate to 1200: %v", err)
	}

	type row struct {
		runnerName, host, decision, source, actor string
		delivered, superseded                     bool
	}
	got := map[string]row{}
	rows, err := pool.Query(`
		SELECT fingerprint, runner_name, host, decision, source, actor,
		       delivered_at IS NOT NULL, superseded_at IS NOT NULL
		  FROM host_key_ledger`)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	for rows.Next() {
		var fp string
		var r row
		if err := rows.Scan(&fp, &r.runnerName, &r.host, &r.decision, &r.source, &r.actor, &r.delivered, &r.superseded); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[fp] = r
	}
	rows.Close()

	want := map[string]row{
		// Superseded by p6, the later approval for the same known_hosts host.
		"SHA256:p1": {"runner-dmz-01", "web01", "approved", "scan", "alice", true, true},
		"SHA256:p6": {"runner-dmz-01", "web01", "approved", "scan", "dave", false, false},
		"SHA256:p2": {"runner-dmz-01", "[db01]:2222", "approved", "scan", "alice", false, false},
		"SHA256:p3": {"runner-dmz-01", "evil", "rejected", "scan", "carol", false, true},
		// The runner is gone; its id stands in for the name it no longer has.
		"SHA256:p5": {"gone", "web01", "approved", "scan", "bob", true, false},
	}
	if len(got) != len(want) {
		t.Fatalf("ledger holds %d rows, want %d: %+v", len(got), len(want), got)
	}
	for fp, w := range want {
		if got[fp] != w {
			t.Errorf("%s = %+v, want %+v", fp, got[fp], w)
		}
	}

	// The pending table gained its scope columns and kept its rows.
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM pending_host_keys WHERE scope_id IS NULL AND host_name IS NULL`).Scan(&n); err != nil || n != 6 {
		t.Fatalf("pending rows after 1200 = %d (%v), want 6 with empty scope columns", n, err)
	}
	// Never two keys in force for one (runner, host, key type).
	if err := pool.QueryRow(`
		SELECT COUNT(*) FROM (SELECT 1 FROM host_key_ledger
		                       WHERE decision = 'approved' AND superseded_at IS NULL
		                       GROUP BY runner_id, host, key_type HAVING COUNT(*) > 1)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d (runner, host, key type) group(s) with more than one key in force (%v)", n, err)
	}

	// 1210, then all the way back down to 1190: both reverse cleanly with data
	// present, and the pending rows — the pre-1200 record — are untouched.
	if err := m.Migrate(1210); err != nil {
		t.Fatalf("migrate to 1210: %v", err)
	}
	if _, err := pool.Exec(`
		INSERT INTO runner_known_hosts (runner_id, line_no, hosts, key_type, fingerprint)
		VALUES ('r1', 1, 'web01', 'ssh-ed25519', 'SHA256:p1')`); err != nil {
		t.Fatalf("seed report: %v", err)
	}
	if err := m.Migrate(1190); err != nil {
		t.Fatalf("migrate down to 1190: %v", err)
	}
	if err := pool.QueryRow(`SELECT COUNT(*) FROM pending_host_keys WHERE approved_at IS NOT NULL`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("approved pending rows after the down = %d (%v), want 4", n, err)
	}
	if err := pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('host_key_ledger', 'host_key_scan_targets', 'runner_known_hosts')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d host-key table(s) survived the down (%v)", n, err)
	}
}

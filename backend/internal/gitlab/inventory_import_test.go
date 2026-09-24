package gitlab

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestImportGitHosts (M4 / §9): sync imports a git inventory's connection vars into
// ssh_hosts (source='git'); a TOFU-captured host_key survives re-sync; and the
// prune-by-owner reaps a host dropped from inventory without touching operator rows.
func TestImportGitHosts(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "import.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	clone := t.TempDir()
	invDir := filepath.Join(clone, "inventory")
	if err := os.MkdirAll(invDir, 0o755); err != nil {
		t.Fatal(err)
	}
	svc := &Service{db: pool, cloneDir: clone}

	sync := func(t *testing.T, ts string) {
		t.Helper()
		scopes, errs := svc.parseInventories()
		if len(errs) != 0 {
			t.Fatalf("parse errs: %v", errs)
		}
		tx, err := pool.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.upsertScopes(ctx, tx, scopes, ts, "sha"); err != nil {
			t.Fatalf("upsertScopes: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	get := func(host string) (address, user, key, source, syncedAt sql.NullString, hostKey sql.NullString) {
		if err := pool.QueryRow(`SELECT address, username, auth_key_env_var, source, synced_at, host_key
		      FROM ssh_hosts WHERE hostname=?`, host).
			Scan(&address, &user, &key, &source, &syncedAt, &hostKey); err != nil {
			t.Fatalf("get %s: %v", host, err)
		}
		return
	}

	// First sync: web1 (host_var ansible_host/user), web2 (group_var user + group auth-key).
	writeInvFile(t, filepath.Join(invDir, "prod.ini"),
		"[web]\nweb1 ansible_host=10.0.0.1 ansible_user=deploy\nweb2\n[web:vars]\nansible_user=svc\namadeus_auth_key_env_var=WEB_KEY\n")
	sync(t, "2026-01-01T00:00:00Z")

	addr, user, key, source, _, _ := get("web1")
	if source.String != "git" || addr.String != "10.0.0.1" || user.String != "deploy" || key.String != "WEB_KEY" {
		t.Errorf("web1 import = addr=%q user=%q key=%q src=%q; want 10.0.0.1/deploy(host_var wins)/WEB_KEY/git", addr.String, user.String, key.String, source.String)
	}
	_, w2user, w2key, _, _, _ := get("web2")
	if w2user.String != "svc" || w2key.String != "WEB_KEY" { // both from [web:vars]
		t.Errorf("web2 group-var inherit = user=%q key=%q, want svc/WEB_KEY", w2user.String, w2key.String)
	}

	// An operator overlay (amadeus) row for web1 must survive the prune.
	if _, err := pool.Exec(`INSERT INTO ssh_hosts(id,source,hostname,username,created_at) VALUES('op1','amadeus','web1','operator','t')`); err != nil {
		t.Fatal(err)
	}

	// TOFU captures a host key on web1's git row.
	if _, err := pool.Exec(`UPDATE ssh_hosts SET host_key='SSHKEYDATA' WHERE hostname='web1' AND source='git'`); err != nil {
		t.Fatal(err)
	}

	// Second sync at a LATER timestamp, with web2 REMOVED from inventory.
	writeInvFile(t, filepath.Join(invDir, "prod.ini"),
		"[web]\nweb1 ansible_host=10.0.0.1 ansible_user=deploy\n[web:vars]\namadeus_auth_key_env_var=WEB_KEY\n")
	sync(t, "2026-01-02T00:00:00Z")

	// web1 git row: host_key preserved across re-sync, synced_at re-stamped.
	// (web1 now has 2 rows — git + amadeus overlay — so query the git row explicitly.)
	var gitKey, gitSynced string
	if err := pool.QueryRow(`SELECT COALESCE(host_key,''), COALESCE(synced_at,'') FROM ssh_hosts WHERE hostname='web1' AND source='git'`).Scan(&gitKey, &gitSynced); err != nil {
		t.Fatal(err)
	}
	if gitKey != "SSHKEYDATA" {
		t.Errorf("web1 git host_key = %q, want it PRESERVED across re-sync", gitKey)
	}
	if gitSynced != "2026-01-02T00:00:00Z" {
		t.Errorf("web1 synced_at = %q, want re-stamped to the second sync", gitSynced)
	}

	// Prune-by-owner (the Sync flow's scopesOK block) reaps stale git rows.
	if _, err := pool.Exec(`DELETE FROM ssh_hosts WHERE source='git' AND (synced_at IS NULL OR synced_at < ?)`, "2026-01-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	var web2git, web1git, web1amadeus int
	pool.QueryRow(`SELECT COUNT(*) FROM ssh_hosts WHERE hostname='web2' AND source='git'`).Scan(&web2git)
	pool.QueryRow(`SELECT COUNT(*) FROM ssh_hosts WHERE hostname='web1' AND source='git'`).Scan(&web1git)
	pool.QueryRow(`SELECT COUNT(*) FROM ssh_hosts WHERE hostname='web1' AND source='amadeus'`).Scan(&web1amadeus)
	if web2git != 0 {
		t.Errorf("web2 git row should be pruned (dropped from inventory), still present")
	}
	if web1git != 1 {
		t.Errorf("web1 git row should survive (re-stamped), got %d", web1git)
	}
	if web1amadeus != 1 {
		t.Errorf("operator (amadeus) overlay must NEVER be pruned, got %d", web1amadeus)
	}
}

// TestImportGitHosts_DegradePreserves: a TRANSIENT inventory degrade (e.g. a host
// range) must NOT lose the scope's imported git rows or their TOFU-captured
// host_key. The degrade re-stamps synced_at so the prune leaves them (OD-13/§9.4).
func TestImportGitHosts_DegradePreserves(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "degrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	clone := t.TempDir()
	invDir := filepath.Join(clone, "inventory")
	if err := os.MkdirAll(invDir, 0o755); err != nil {
		t.Fatal(err)
	}
	svc := &Service{db: pool, cloneDir: clone}
	sync := func(ts string) {
		t.Helper()
		scopes, errs := svc.parseInventories()
		if len(errs) != 0 {
			t.Fatalf("parse errs (a degrade is NOT a parse err): %v", errs)
		}
		tx, _ := pool.Begin()
		if err := svc.upsertScopes(ctx, tx, scopes, ts, "sha"); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		tx.Commit()
	}

	// First sync: clean. web1 imported as a git row; TOFU captures a key.
	writeInvFile(t, filepath.Join(invDir, "prod.ini"), "[web]\nweb1 ansible_host=10.0.0.1\n")
	sync("2026-01-01T00:00:00Z")
	if _, err := pool.Exec(`UPDATE ssh_hosts SET host_key='SSHKEY' WHERE hostname='web1' AND source='git'`); err != nil {
		t.Fatal(err)
	}

	// Second sync: the inventory now DEGRADES (a host-range line is out-of-subset).
	// Projection becomes unavailable, but the scope is NOT a parse error, so the
	// prune-by-owner still runs.
	writeInvFile(t, filepath.Join(invDir, "prod.ini"), "[web]\nweb1 ansible_host=10.0.0.1\nweb[01:99]\n")
	sync("2026-01-02T00:00:00Z")

	var status string
	_ = pool.QueryRow(`SELECT projection_status FROM scopes WHERE name='prod'`).Scan(&status)
	if status != "unavailable" {
		t.Fatalf("expected a degraded projection, got status=%q", status)
	}

	// Run the prune-by-owner (the scopesOK block's query).
	if _, err := pool.Exec(`DELETE FROM ssh_hosts WHERE source='git' AND (synced_at IS NULL OR synced_at < ?)`, "2026-01-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	// web1's git row + its TOFU key MUST survive the degrade + prune.
	var key, synced string
	if err := pool.QueryRow(`SELECT COALESCE(host_key,''), COALESCE(synced_at,'') FROM ssh_hosts WHERE hostname='web1' AND source='git'`).
		Scan(&key, &synced); err != nil {
		t.Fatalf("web1 git row was LOST on a transient degrade: %v", err)
	}
	if key != "SSHKEY" {
		t.Errorf("web1 host_key = %q, want it PRESERVED through the degrade (OD-13)", key)
	}
	if synced != "2026-01-02T00:00:00Z" {
		t.Errorf("web1 synced_at = %q, want re-stamped by the degrade path", synced)
	}
}

package settings

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

func TestPutScopeInventory(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	id := db.NewID()
	if _, err := pool.Exec(`INSERT INTO scopes(id,name,source,created_at,supported_types) VALUES(?,'edge','amadeus','t','["bash"]')`, id); err != nil {
		t.Fatal(err)
	}

	raw := "[all:vars]\nansible_user=deploy\n[web]\nweb1 ansible_host=10.0.0.1\n[web:vars]\ncronomicon_auth_key_env_var=EDGE_KEY\n"
	doc, lineErrs, err := PutScopeInventory(ctx, pool, id, raw, "ini", "op")
	if err != nil || len(lineErrs) != 0 {
		t.Fatalf("put: err=%v lineErrs=%v", err, lineErrs)
	}
	if doc == nil || !doc.HasInventory || doc.ParseStatus != "ok" {
		t.Fatalf("doc = %+v", doc)
	}
	if doc.Raw == nil || *doc.Raw != raw {
		t.Errorf("raw not stored/exposed for amadeus scope")
	}
	if doc.Projection == nil || doc.Projection.Groups == nil {
		t.Errorf("projection missing: %+v", doc.Projection)
	}
	// scope_hosts membership replaced with the inventory's hosts.
	var hostCount int
	pool.QueryRow(`SELECT COUNT(*) FROM scope_hosts WHERE scope_id=?`, id).Scan(&hostCount)
	if hostCount != 1 {
		t.Errorf("scope_hosts = %d, want 1 (web1)", hostCount)
	}
	// Capability unioned with the inferred ansible (from [all:vars] + ansible_user).
	var types string
	pool.QueryRow(`SELECT supported_types FROM scopes WHERE id=?`, id).Scan(&types)
	if types == "" || !strings.Contains(types, "ansible") || !strings.Contains(types, "bash") {
		t.Errorf("supported_types = %q, want bash + inferred ansible", types)
	}

	// Secret-bearing inventory → line errors, scope UNCHANGED.
	_, lineErrs2, err2 := PutScopeInventory(ctx, pool, id, "[web]\nweb1 ansible_become_pass=hunter2\n", "ini", "op")
	if err2 != nil {
		t.Fatal(err2)
	}
	if len(lineErrs2) == 0 {
		t.Error("expected secret-rejection line errors")
	}
	var rawNow string
	pool.QueryRow(`SELECT raw_inventory FROM scopes WHERE id=?`, id).Scan(&rawNow)
	if rawNow != raw {
		t.Errorf("scope raw changed on a REJECTED put (should be unchanged)")
	}

	// Git scope → read-only.
	gid := db.NewID()
	pool.Exec(`INSERT INTO scopes(id,name,source,created_at) VALUES(?,'gitscope','git','t')`, gid)
	if _, _, gerr := PutScopeInventory(ctx, pool, gid, raw, "ini", "op"); !errors.Is(gerr, ErrInventoryGitReadOnly) {
		t.Errorf("git put err = %v, want ErrInventoryGitReadOnly", gerr)
	}

	// Unsupported format → rejected.
	if _, _, ferr := PutScopeInventory(ctx, pool, id, raw, "yaml", "op"); !errors.Is(ferr, ErrInventoryUnsupportedFormat) {
		t.Errorf("yaml format err = %v, want ErrInventoryUnsupportedFormat", ferr)
	}
}

func TestImportScopeHosts(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	id := db.NewID()
	pool.Exec(`INSERT INTO scopes(id,name,source,created_at,supported_types) VALUES(?,'edge','amadeus','t','["bash"]')`, id)
	raw := "[all:vars]\nansible_user=deploy\n[web]\nweb1 ansible_host=10.0.0.1\nweb2 ansible_host=10.0.0.2\n[web:vars]\ncronomicon_auth_key_env_var=EDGE_KEY\n"
	if _, le, err := PutScopeInventory(ctx, pool, id, raw, "ini", "op"); err != nil || len(le) != 0 {
		t.Fatalf("put: %v %v", err, le)
	}

	res, err := ImportScopeHosts(ctx, pool, id, nil, false, "op")
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 2 || res.Updated != 0 {
		t.Errorf("import = %+v, want 2 created", res)
	}
	var addr, user, key string
	pool.QueryRow(`SELECT address, username, auth_key_env_var FROM ssh_hosts WHERE hostname='web1' AND scope_id=? AND source='amadeus'`, id).
		Scan(&addr, &user, &key)
	if addr != "10.0.0.1" || user != "deploy" || key != "EDGE_KEY" {
		t.Errorf("web1 imported conn = %s/%s/%s, want 10.0.0.1/deploy/EDGE_KEY", addr, user, key)
	}

	// Re-import without overwrite → both skipped.
	res2, _ := ImportScopeHosts(ctx, pool, id, nil, false, "op")
	if res2.Created != 0 || len(res2.Skipped) != 2 {
		t.Errorf("re-import (no overwrite) = %+v, want 2 skipped", res2)
	}
	// With overwrite → both updated.
	res3, _ := ImportScopeHosts(ctx, pool, id, nil, true, "op")
	if res3.Updated != 2 {
		t.Errorf("re-import (overwrite) = %+v, want 2 updated", res3)
	}

	// A requested host that is NOT in the inventory is reported as rejected.
	res4, _ := ImportScopeHosts(ctx, pool, id, []string{"web1", "ghost"}, true, "op")
	var sawGhost bool
	for _, r := range res4.Rejected {
		if r == "ghost" {
			sawGhost = true
		}
	}
	if !sawGhost {
		t.Errorf("requested host 'ghost' (not in inventory) should be Rejected, got %+v", res4)
	}

	// Git scope → read-only (git auto-imports via sync).
	gid := db.NewID()
	pool.Exec(`INSERT INTO scopes(id,name,source,created_at) VALUES(?,'g','git','t')`, gid)
	if _, gerr := ImportScopeHosts(ctx, pool, gid, nil, false, "op"); !errors.Is(gerr, ErrInventoryGitReadOnly) {
		t.Errorf("git import = %v, want ErrInventoryGitReadOnly", gerr)
	}
}

// TestPutScopeInventory_DegradeKeepsMembership: a degraded authored inventory must
// NOT truncate scope_hosts to the partial ConnHosts — it falls back to the naive
// full set (degrade-independent, like git), so hosts after the offending line stay
// targetable.
func TestPutScopeInventory_DegradeKeepsMembership(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	id := db.NewID()
	pool.Exec(`INSERT INTO scopes(id,name,source,created_at,supported_types) VALUES(?,'edge','amadeus','t','["bash"]')`, id)
	// web[01:99] is an out-of-subset host range → degrades; web3 sits AFTER it.
	doc, le, err := PutScopeInventory(ctx, pool, id, "[web]\nweb1\nweb2\nweb[01:99]\nweb3\n", "ini", "op")
	if err != nil || len(le) != 0 {
		t.Fatalf("put: %v %v", err, le)
	}
	if doc.ParseStatus != "unavailable" {
		t.Fatalf("expected a degraded projection, got %q", doc.ParseStatus)
	}
	var web3 int
	pool.QueryRow(`SELECT COUNT(*) FROM scope_hosts WHERE scope_id=? AND host='web3'`, id).Scan(&web3)
	if web3 != 1 {
		t.Errorf("web3 (after the degrade line) should still be a scope member — membership must not truncate on degrade")
	}
}

func TestCreateScopeWithInventory(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	// 1. Create with secret-bearing inventory -> should fail validation
	secretInv := "[all:vars]\nansible_become_pass=hunter2\n"
	_, err := CreateScope(ctx, pool, LocalScopeInput{
		Scope:          "secscope",
		SupportedTypes: []string{"bash"},
		RawInventory:   &secretInv,
	}, "op")
	if err == nil {
		t.Fatal("expected validation error for secret-bearing inventory")
	}
	if _, ok := errors.AsType[InventoryValidationError](err); !ok {
		t.Fatalf("expected InventoryValidationError, got %v", err)
	}

	// 2. Create with valid inventory
	raw := "[web]\n10.0.0.1\n10.0.0.2\n[db]\n10.0.0.3\n"
	sc, err := CreateScope(ctx, pool, LocalScopeInput{
		Scope:          "invscope",
		SupportedTypes: []string{"bash"},
		RawInventory:   &raw,
	}, "op")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if sc.HasInventory == false {
		t.Error("expected HasInventory to be true")
	}
	if sc.ProjectionStatus == nil || *sc.ProjectionStatus != "ok" {
		t.Errorf("expected projection status 'ok', got %v", sc.ProjectionStatus)
	}
	// Check hosts are inserted
	var hostCount int
	pool.QueryRow(`SELECT COUNT(*) FROM scope_hosts WHERE scope_id=?`, sc.ID).Scan(&hostCount)
	if hostCount != 3 {
		t.Errorf("expected 3 hosts, got %d", hostCount)
	}
	// Check projection groups
	var groupCount int
	pool.QueryRow(`SELECT COUNT(*) FROM scope_groups WHERE scope_id=?`, sc.ID).Scan(&groupCount)
	if groupCount != 2 {
		t.Errorf("expected 2 projection groups, got %d", groupCount)
	}

	// 3. Update scope with new inventory
	updatedRaw := "[web]\n10.0.0.1\n"
	_, _, err = UpdateScope(ctx, pool, sc.ID, LocalScopeInput{
		Scope:          "invscope",
		SupportedTypes: []string{"bash"},
		RawInventory:   &updatedRaw,
	}, "op")
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	pool.QueryRow(`SELECT COUNT(*) FROM scope_hosts WHERE scope_id=?`, sc.ID).Scan(&hostCount)
	if hostCount != 1 {
		t.Errorf("expected 1 host after update, got %d", hostCount)
	}

	// 4. Revert inventory to flat host list
	emptyInv := ""
	scFlat, _, err := UpdateScope(ctx, pool, sc.ID, LocalScopeInput{
		Scope:          "invscope",
		SupportedTypes: []string{"bash"},
		Hosts:          []string{"192.168.1.1", "192.168.1.2"},
		RawInventory:   &emptyInv,
	}, "op")
	if err != nil {
		t.Fatalf("revert failed: %v", err)
	}
	if scFlat.HasInventory {
		t.Error("expected HasInventory to be false after revert")
	}
	pool.QueryRow(`SELECT COUNT(*) FROM scope_hosts WHERE scope_id=?`, sc.ID).Scan(&hostCount)
	if hostCount != 2 {
		t.Errorf("expected 2 hosts after revert, got %d", hostCount)
	}
	// Projection groups should be empty
	pool.QueryRow(`SELECT COUNT(*) FROM scope_groups WHERE scope_id=?`, sc.ID).Scan(&groupCount)
	if groupCount != 0 {
		t.Errorf("expected 0 projection groups after revert, got %d", groupCount)
	}
}

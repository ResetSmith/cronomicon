package settings

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

func TestGetScopeInventory_Projection(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	id := db.NewID()

	if _, err := pool.Exec(`INSERT INTO scopes(id,name,source,created_at,raw_inventory,inventory_format,projection_status)
	    VALUES(?,?,'git','t','rawcontent','ini','ok')`, id, "prod"); err != nil {
		t.Fatal(err)
	}
	for _, ex := range [][]any{
		{`INSERT INTO scope_hosts(scope_id,host) VALUES(?,?)`, id, "web1"},
		{`INSERT INTO scope_groups(scope_id,name) VALUES(?,?)`, id, "web"},
		{`INSERT INTO scope_group_hosts(scope_id,group_name,host) VALUES(?,?,?)`, id, "web", "web1"},
		{`INSERT INTO scope_host_vars(scope_id,host,key,value) VALUES(?,?,?,?)`, id, "web1", "ansible_host", "10.0.0.1"},
		{`INSERT INTO scope_group_vars(scope_id,group_name,key,value) VALUES(?,?,?,?)`, id, "web", "ansible_port", "22"},
	} {
		if _, err := pool.Exec(ex[0].(string), ex[1:]...); err != nil {
			t.Fatalf("seed %q: %v", ex[0], err)
		}
	}

	doc, err := GetScopeInventory(ctx, pool, id)
	if err != nil {
		t.Fatal(err)
	}
	if doc == nil {
		t.Fatal("nil doc")
	}
	if !doc.HasInventory || doc.ParseStatus != "ok" {
		t.Fatalf("hasInventory=%v parseStatus=%q", doc.HasInventory, doc.ParseStatus)
	}
	if doc.Raw != nil {
		t.Errorf("git-source scope raw must NOT be exposed, got %q", *doc.Raw)
	}
	if doc.Projection == nil || !doc.Projection.Advisory {
		t.Fatalf("expected an advisory projection, got %+v", doc.Projection)
	}
	if len(doc.Projection.Hosts) != 1 || doc.Projection.Hosts[0].Name != "web1" ||
		doc.Projection.Hosts[0].Vars["ansible_host"] != "10.0.0.1" {
		t.Errorf("projection hosts = %+v", doc.Projection.Hosts)
	}
	if len(doc.Projection.Groups) != 1 {
		t.Fatalf("groups = %+v", doc.Projection.Groups)
	}
	g := doc.Projection.Groups[0]
	if g.Name != "web" || len(g.Hosts) != 1 || g.Hosts[0] != "web1" || g.Vars["ansible_port"] != "22" {
		t.Errorf("group = %+v", g)
	}
}

func TestGetScopeInventory_Unavailable(t *testing.T) {
	pool := openTestPool(t)
	id := db.NewID()
	if _, err := pool.Exec(`INSERT INTO scopes(id,name,source,created_at,raw_inventory,inventory_format,projection_status,projection_json)
	    VALUES(?,?,'git','t','rawcontent','ini','unavailable',?)`, id, "ranged",
		`{"reason":"host range patterns are unsupported in the preview: web[01:50]","line":2}`); err != nil {
		t.Fatal(err)
	}
	doc, err := GetScopeInventory(context.Background(), pool, id)
	if err != nil {
		t.Fatal(err)
	}
	if doc.ParseStatus != "unavailable" {
		t.Errorf("parseStatus = %q, want unavailable", doc.ParseStatus)
	}
	if doc.Projection != nil {
		t.Error("an unavailable projection must be omitted (the raw still ships)")
	}
	if doc.ParseReason == nil || doc.ParseLine == nil || *doc.ParseLine != 2 {
		t.Errorf("expected parseReason + parseLine=2, got reason=%v line=%v", doc.ParseReason, doc.ParseLine)
	}
}

func TestGetScopeInventory_NotFound(t *testing.T) {
	pool := openTestPool(t)
	doc, err := GetScopeInventory(context.Background(), pool, "does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	if doc != nil {
		t.Errorf("expected nil for a missing scope, got %+v", doc)
	}
}

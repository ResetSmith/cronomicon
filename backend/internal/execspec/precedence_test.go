package execspec_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// TestHostByNamePrecedence (M4 / D4): when a hostname has both an imported (git)
// row and an operator (cronomicon) row, the cronomicon row wins, and HostByName carries
// the resolved row's id (for the source-qualified TOFU write). Also locks the scan
// shape with the new id/source columns against a NULL-port/NULL-address row.
func TestHostByNamePrecedence(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "prec.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// A scope 'prod' with a git-imported host row + an operator (cronomicon) overlay
	// for the SAME hostname.
	exec(`INSERT INTO scopes(id,name,source,created_at) VALUES('sprod','prod','git','t')`)
	exec(`INSERT INTO ssh_hosts(id,source,scope_id,hostname,address,port,username,auth_key_env_var,created_at,last_modified_at)
	      VALUES('git1','git','sprod','web1','10.0.0.1',22,'gituser','GIT_KEY','t','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO ssh_hosts(id,source,hostname,address,port,username,auth_key_env_var,created_at,last_modified_at)
	      VALUES('am1','cronomicon','web1','10.9.9.9',2222,'opuser','OP_KEY','t','2026-01-02T00:00:00Z')`)

	tgt, err := execspec.HostByName(ctx, pool, "prod", "web1")
	if err != nil || tgt == nil {
		t.Fatalf("HostByName web1: %v (%+v)", err, tgt)
	}
	if tgt.ID != "am1" || tgt.User != "opuser" || tgt.AuthKeyEnvVar != "OP_KEY" || tgt.Port != 2222 || tgt.Address != "10.9.9.9" {
		t.Errorf("precedence resolved %+v, want the cronomicon overlay (am1/opuser/OP_KEY/2222)", tgt)
	}

	// Scope isolation: a git row from ANOTHER scope is NOT a candidate (no
	// wrong-host dial). web2 lives only in scope 'other'; resolving it under 'prod'
	// (with no cronomicon overlay) returns nothing.
	exec(`INSERT INTO scopes(id,name,source,created_at) VALUES('soth','other','git','t')`)
	exec(`INSERT INTO ssh_hosts(id,source,scope_id,hostname,address,created_at) VALUES('g2','git','soth','web2','10.2.2.2','t')`)
	if got, _ := execspec.HostByName(ctx, pool, "prod", "web2"); got != nil {
		t.Errorf("web2 (only in scope 'other') resolved under 'prod' = %+v, want nil", got)
	}
	if got, _ := execspec.HostByName(ctx, pool, "other", "web2"); got == nil || got.ID != "g2" {
		t.Errorf("web2 under its own scope should resolve to g2, got %+v", got)
	}

	// Deterministic tiebreak: two cronomicon rows, same hostname, identical
	// last_modified_at → a stable pick (id DESC), not query-plan luck.
	exec(`INSERT INTO ssh_hosts(id,source,hostname,created_at,last_modified_at) VALUES('amA','cronomicon','dup','t','2026-02-01T00:00:00Z')`)
	exec(`INSERT INTO ssh_hosts(id,source,hostname,created_at,last_modified_at) VALUES('amB','cronomicon','dup','t','2026-02-01T00:00:00Z')`)
	d1, _ := execspec.HostByName(ctx, pool, "prod", "dup")
	d2, _ := execspec.HostByName(ctx, pool, "prod", "dup")
	if d1 == nil || d2 == nil || d1.ID != d2.ID || d1.ID != "amB" {
		t.Errorf("tie resolution unstable/unexpected: %v vs %v (want stable amB)", d1, d2)
	}

	// Cronomicon SCOPE isolation (M5): two cronomicon scopes that both imported a host
	// named 'shared' must each resolve THEIR OWN row — a scoped cronomicon import is
	// not a global overlay. A NULL-scope operator overlay still wins for both.
	exec(`INSERT INTO scopes(id,name,source,created_at) VALUES('sa','scopeA','cronomicon','t')`)
	exec(`INSERT INTO scopes(id,name,source,created_at) VALUES('sb','scopeB','cronomicon','t')`)
	exec(`INSERT INTO ssh_hosts(id,source,scope_id,hostname,address,created_at,last_modified_at) VALUES('ha','cronomicon','sa','shared','10.0.0.1','t','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO ssh_hosts(id,source,scope_id,hostname,address,created_at,last_modified_at) VALUES('hb','cronomicon','sb','shared','10.0.0.2','t','2026-01-02T00:00:00Z')`)
	if a, _ := execspec.HostByName(ctx, pool, "scopeA", "shared"); a == nil || a.ID != "ha" || a.Address != "10.0.0.1" {
		t.Errorf("scopeA 'shared' = %+v, want its own row ha/10.0.0.1 (not scopeB's newer one)", a)
	}
	if b, _ := execspec.HostByName(ctx, pool, "scopeB", "shared"); b == nil || b.ID != "hb" {
		t.Errorf("scopeB 'shared' = %+v, want its own row hb", b)
	}
	// A global operator overlay (NULL scope_id), even OLDER, wins over a scoped import.
	exec(`INSERT INTO ssh_hosts(id,source,hostname,address,created_at,last_modified_at) VALUES('ov','cronomicon','shared','10.9.9.9','t','2025-01-01T00:00:00Z')`)
	if ov, _ := execspec.HostByName(ctx, pool, "scopeA", "shared"); ov == nil || ov.ID != "ov" {
		t.Errorf("global operator overlay should win over a scoped import, got %+v", ov)
	}

	// Scan shape: a bare row with NULL address/port/username still scans, with id set.
	exec(`INSERT INTO ssh_hosts(id,source,hostname,created_at) VALUES('bare','cronomicon','bareh','t')`)
	bt, err := execspec.HostByName(ctx, pool, "prod", "bareh")
	if err != nil || bt == nil {
		t.Fatalf("HostByName bareh (NULL cols): %v", err)
	}
	if bt.ID != "bare" || bt.Address != "" {
		t.Errorf("bare host scan = %+v, want id=bare, empty address", bt)
	}
}

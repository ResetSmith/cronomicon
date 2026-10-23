package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// 2.5.1 review: once the server has bound every value (the marker), rewrap
// refuses an unbound row instead of opening it and re-sealing it bound to the
// row it was copied into.
func TestRewrapRefusesUnboundAfterTheMarker(t *testing.T) {
	t.Cleanup(func() { secrets.RefuseUnboundSeals(false) })
	k1, k1b64 := newKEK(t)
	_ = k1
	cfgV1 := &config.Config{SecretKEKEnv: k1b64, SecretKEKVersion: 1}
	pool, _ := seedV1(t, cfgV1) // unbound rows, as 2.5.0 left them
	defer pool.Close()
	if _, err := pool.Exec(`INSERT INTO settings (key, value) VALUES ('upgrade.2.5.1.bindSeals', 'bound')`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	done, err := secrets.ApplyBindSealsMarker(ctx, pool)
	if err != nil || !done || !secrets.UnboundSealsRefused() {
		t.Fatalf("marker not applied: %v %v", done, err)
	}
	_, k2b64 := newKEK(t)
	cfgV2 := &config.Config{SecretKEKEnv: k2b64, SecretKEKVersion: 2}
	t.Setenv("CRONOMICON_KEK_1", k1b64)
	_, failed := rewrapAll(ctx, pool, cfgV2, secrets.NewSealer(cfgV2), 2)
	if failed == 0 {
		t.Fatal("rewrap re-sealed unbound values after the marker")
	}
	var format int
	if err := pool.QueryRow(`SELECT seal_format FROM secrets WHERE id = 'sec1'`).Scan(&format); err != nil || format != secrets.SealUnbound {
		t.Fatalf("the unbound secret was rewritten (seal_format %d, %v)", format, err)
	}
}

// 2.5.1 review: --file that is the database it would replace is refused
// before anything is removed.
func TestRestoreFileRefusesTheLiveDatabase(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "cronomicon.db")
	smallDB(t, live, "live")
	if code := restoreLocalFile(live, live, nil, true); code == 0 {
		t.Fatal("restoring the live database onto itself was accepted")
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the live database is gone: %v", err)
	}
	if markerOf(t, live) != "live" {
		t.Fatal("the live database changed")
	}
}

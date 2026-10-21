package settings

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// 2.5.1 SG2.8: a push target's URL is sealed at rest and read back plain; a
// URL stored plain before 2.5.1 is sealed by the start pass; with no KEK a
// URL is stored and read as before.
func TestPushTargetURLsAreSealed(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek), SecretKEKVersion: 1}

	made, err := CreateAlertTarget(ctx, pool, cfg, AlertTargetInput{Label: "ops", Service: "slack", URL: "slack://TOKEN-A/B/C", Enabled: true}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if made.URL != "slack://TOKEN-A/B/C" {
		t.Fatalf("read back %q", made.URL)
	}
	var stored string
	if err := pool.QueryRow(`SELECT url FROM alert_targets WHERE id = ?`, made.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "TOKEN-A") || !strings.HasPrefix(stored, "v2:") {
		t.Fatalf("stored %q, want a sealed token", stored)
	}

	// A URL from before 2.5.1, plain; the start pass seals it, once.
	if _, err := pool.Exec(`INSERT INTO alert_targets (id, owner_agency, label, service, url, enabled, created_at) VALUES ('t-old', 'global', 'old', 'slack', 'slack://OLD-TOKEN', 1, 'now')`); err != nil {
		t.Fatal(err)
	}
	if n, err := SealAlertTargetURLs(ctx, pool, cfg); err != nil || n != 1 {
		t.Fatalf("seal pass: %d %v", n, err)
	}
	if n, err := SealAlertTargetURLs(ctx, pool, cfg); err != nil || n != 0 {
		t.Fatalf("second pass: %d %v, want nothing to do", n, err)
	}
	old, err := GetAlertTarget(ctx, pool, cfg, "t-old")
	if err != nil || old.URL != "slack://OLD-TOKEN" {
		t.Fatalf("the sealed old target reads %+v (%v)", old, err)
	}
	list, err := ListAlertTargets(ctx, pool, cfg)
	if err != nil || len(list) != 2 {
		t.Fatalf("list %v %v", list, err)
	}

	// No KEK: stored and read as before, and the pass leaves it alone.
	none := &config.Config{}
	plain, err := CreateAlertTarget(ctx, pool, none, AlertTargetInput{Label: "nokek", Service: "slack", URL: "slack://PLAIN", Enabled: true}, "ana")
	if err != nil || plain.URL != "slack://PLAIN" {
		t.Fatalf("no-KEK create: %+v %v", plain, err)
	}
	if n, err := SealAlertTargetURLs(ctx, pool, none); err != nil || n != 0 {
		t.Fatalf("no-KEK pass: %d %v", n, err)
	}
	// Deleting needs no key.
	if ok, err := DeleteAlertTarget(ctx, pool, made.ID, "ana"); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
}

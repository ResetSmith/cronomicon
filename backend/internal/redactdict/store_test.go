package redactdict

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

func openPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "redactdict.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func kekCfg(t *testing.T) *config.Config {
	t.Helper()
	kek := make([]byte, 32)
	if _, err := rand.Read(kek); err != nil {
		t.Fatal(err)
	}
	return &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek), SecretKEKVersion: 1}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const pem = "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
	"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAAB\n" +
	"-----END OPENSSH PRIVATE KEY-----"

// TestBuildUnionsEverySourceAndKeepsVariablesVisible: a stored secret, a
// multi-line env_var (key material) and an encrypted settings column all land
// in the dictionary; a single-line Variable does not (D7).
func TestBuildUnionsEverySourceAndKeepsVariablesVisible(t *testing.T) {
	pool, cfg, ctx := openPool(t), kekCfg(t), context.Background()
	svc := secrets.New(pool, cfg, quietLog())
	if _, err := svc.Create(ctx, secrets.CreateInput{Key: "DB_PASS", Source: "stored", Value: "stored-secret-value"}, "t"); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	for _, r := range []struct{ id, key, val string }{{"ev1", "APP_URL", "plain-visible-value"}, {"ev2", "SSH_KEY", pem}} {
		if _, err := pool.Exec(`INSERT INTO env_vars(id, key, scope, value, created_at) VALUES(?,?,?,?,?)`, r.id, r.key, "prod", r.val, "2026-01-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	tok, err := secrets.EncryptString(cfg, "s3-secret-key-value")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO log_storage_config(id, backend, s3_secret_key_enc, last_modified_by, last_modified_at) VALUES(1,'local',?, 't', '2026-01-01T00:00:00Z')`, tok); err != nil {
		t.Fatalf("seed settings column: %v", err)
	}

	d, err := Build(ctx, pool, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, want := range []string{"stored-secret-value", "s3-secret-key-value", "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAAB"} {
		if got := d.RedactString("x " + want + " y"); strings.Contains(got, want) {
			t.Errorf("%q not masked: %q", want, got)
		}
	}
	if got := d.RedactString("plain-visible-value"); got != "plain-visible-value" {
		t.Errorf("single-line Variable was masked: %q", got)
	}
}

// TestBuildReportsUndecryptableAsPartial: rows the KEK cannot open come back
// as a PARTIAL dictionary with ErrUndecryptable — not as an empty, complete one.
func TestBuildReportsUndecryptableAsPartial(t *testing.T) {
	pool, cfg, ctx := openPool(t), kekCfg(t), context.Background()
	svc := secrets.New(pool, cfg, quietLog())
	if _, err := svc.Create(ctx, secrets.CreateInput{Key: "K", Source: "stored", Value: "value-under-lost-kek"}, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO env_vars(id, key, scope, value, created_at) VALUES('e','SSH_KEY','*',?, '2026-01-01T00:00:00Z')`, pem); err != nil {
		t.Fatal(err)
	}
	d, err := Build(ctx, pool, &config.Config{}) // no KEK at all
	if !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("err = %v, want ErrUndecryptable", err)
	}
	if d == nil || d.Len() == 0 {
		t.Fatal("partial build must still return what it could read (the env_var key)")
	}
	if got := d.RedactString("value-under-lost-kek"); got != "value-under-lost-kek" {
		t.Error("a value the KEK cannot open cannot be in the dictionary")
	}
	// And an EMPTY store is complete, not partial.
	if _, err := Build(ctx, openPool(t), &config.Config{}); err != nil {
		t.Errorf("empty tables with no KEK must be a complete build, got %v", err)
	}
}

// TestStoreLifecycle: first Get builds; Invalidate + a new secret ⇒ the next
// Get masks it; a failed rebuild keeps the last good dictionary (AM-Q3(c)).
func TestStoreLifecycle(t *testing.T) {
	pool, cfg, ctx := openPool(t), kekCfg(t), context.Background()
	svc := secrets.New(pool, cfg, quietLog())
	st := NewStore(pool, cfg)
	if _, _, err := st.Current(); err == nil {
		t.Fatal("Current before any build must report never-built")
	}
	d, err := st.Get(ctx)
	if err != nil || d == nil {
		t.Fatalf("first Get: dict=%v err=%v", d, err)
	}
	if _, err := svc.Create(ctx, secrets.CreateInput{Key: "K", Source: "stored", Value: "rotated-token-value"}, "t"); err != nil {
		t.Fatal(err)
	}
	// Not yet invalidated (this test bypasses the hook): still the old dictionary.
	if d, _ := st.Get(ctx); d.RedactString("rotated-token-value") != "rotated-token-value" {
		t.Fatal("no Invalidate, no rebuild expected")
	}
	st.Invalidate()
	if d, _ := st.Get(ctx); d.RedactString("rotated-token-value") == "rotated-token-value" {
		t.Fatal("after Invalidate the next Get must see the new value")
	}
	gen := st.Generation()

	// Break the database: the rebuild fails and the last good dictionary stays.
	pool.Close()
	st.Invalidate()
	d, err = st.Get(ctx)
	if err == nil || errors.Is(err, ErrUndecryptable) {
		t.Fatalf("expected a database failure, got %v", err)
	}
	if d == nil || d.RedactString("rotated-token-value") == "rotated-token-value" {
		t.Fatal("last good dictionary must survive a failed rebuild")
	}
	if st.Generation() != gen+1 {
		t.Errorf("generation = %d, want %d", st.Generation(), gen+1)
	}
}

// TestStoreConcurrentReadersNeverBlockOnRebuild — run under -race. Readers
// during a rebuild get the previous dictionary; nothing panics; the store ends
// consistent.
func TestStoreConcurrentReadersNeverBlockOnRebuild(t *testing.T) {
	pool, cfg, ctx := openPool(t), kekCfg(t), context.Background()
	svc := secrets.New(pool, cfg, quietLog())
	if _, err := svc.Create(ctx, secrets.CreateInput{Key: "K", Source: "stored", Value: "concurrent-secret-value"}, "t"); err != nil {
		t.Fatal(err)
	}
	st := NewStore(pool, cfg)
	if _, err := st.Get(ctx); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if i%4 == 0 {
					st.Invalidate()
				}
				d, _ := st.Get(ctx)
				if d == nil {
					t.Error("Get returned nil after the first build")
					return
				}
				if d.RedactString("concurrent-secret-value") == "concurrent-secret-value" {
					t.Error("installed dictionary lost a value mid-rebuild")
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestConfigureWiresTheChangeHook: the default store is invalidated by the
// secrets hook, so a writer that calls RedactionSourceChanged refreshes it and
// MaskString sees the new value.
func TestConfigureWiresTheChangeHook(t *testing.T) {
	pool, cfg, ctx := openPool(t), kekCfg(t), context.Background()
	t.Cleanup(func() { std.Store(nil); secrets.SetRedactionChangeHook(nil) })
	if got := MaskString("before-configure"); got != "before-configure" {
		t.Fatal("MaskString before Configure must be a no-op")
	}
	Configure(pool, cfg)
	svc := secrets.New(pool, cfg, quietLog())
	if got := MaskString("hook-secret-value"); got != "hook-secret-value" {
		t.Fatal("nothing stored yet")
	}
	// Service.Create calls RedactionSourceChanged itself (AM-4b).
	if _, err := svc.Create(ctx, secrets.CreateInput{Key: "K", Source: "stored", Value: "hook-secret-value"}, "t"); err != nil {
		t.Fatal(err)
	}
	if got := MaskString("x hook-secret-value y"); got != "x "+Mask+" y" {
		t.Errorf("MaskString after a hooked write = %q", got)
	}
}

package secrets

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// ── envelope encryption unit tests ───────────────────────────────────────────

func TestEncryptDecryptStringRoundTrip(t *testing.T) {
	kek, _ := randomBytes(32)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek)}

	token, err := EncryptString(cfg, "smtp-pa$$word")
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	if token == "smtp-pa$$word" {
		t.Fatal("token must not be the plaintext")
	}
	got, err := DecryptString(cfg, token)
	if err != nil {
		t.Fatalf("DecryptString: %v", err)
	}
	if got != "smtp-pa$$word" {
		t.Fatalf("round-trip mismatch: %q", got)
	}

	// No KEK ⇒ EncryptString returns ErrNoKEK (callers can detect this).
	if _, err := EncryptString(&config.Config{}, "x"); err != ErrNoKEK {
		t.Fatalf("expected ErrNoKEK, got %v", err)
	}
}

func TestEnvelopeEncryptRoundTrip(t *testing.T) {
	kek, err := randomBytes(32)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("super secret value 🔐")

	ciphertext, nonce, wrappedDEK, err := envelopeEncrypt(kek, plaintext)
	if err != nil {
		t.Fatalf("envelopeEncrypt: %v", err)
	}
	if len(ciphertext) == 0 || len(nonce) == 0 || len(wrappedDEK) == 0 {
		t.Fatal("expected non-empty outputs from envelopeEncrypt")
	}

	recovered, err := envelopeDecrypt(kek, ciphertext, nonce, wrappedDEK)
	if err != nil {
		t.Fatalf("envelopeDecrypt: %v", err)
	}
	if string(recovered) != string(plaintext) {
		t.Fatalf("round-trip mismatch: got %q, want %q", recovered, plaintext)
	}
}

func TestEnvelopeEncryptProducesUniqueOutputs(t *testing.T) {
	kek, _ := randomBytes(32)
	plain := []byte("same value")

	ct1, n1, wd1, _ := envelopeEncrypt(kek, plain)
	ct2, n2, wd2, _ := envelopeEncrypt(kek, plain)

	// Different nonces → different ciphertexts (IND-CPA).
	if string(n1) == string(n2) {
		t.Error("nonces should be unique per encryption")
	}
	if string(ct1) == string(ct2) {
		t.Error("ciphertexts should differ with distinct nonces")
	}
	if string(wd1) == string(wd2) {
		t.Error("wrapped DEKs should differ (random DEK per call)")
	}

	// Both should still decrypt to the same plaintext.
	r1, err1 := envelopeDecrypt(kek, ct1, n1, wd1)
	r2, err2 := envelopeDecrypt(kek, ct2, n2, wd2)
	if err1 != nil || err2 != nil {
		t.Fatalf("decrypt errors: %v / %v", err1, err2)
	}
	if string(r1) != string(plain) || string(r2) != string(plain) {
		t.Error("decryption should recover original plaintext")
	}
}

func TestEnvelopeDecryptWrongKEK(t *testing.T) {
	kek, _ := randomBytes(32)
	wrongKEK, _ := randomBytes(32)

	ct, n, wd, _ := envelopeEncrypt(kek, []byte("secret"))
	_, err := envelopeDecrypt(wrongKEK, ct, n, wd)
	if err == nil {
		t.Fatal("expected decryption error with wrong KEK")
	}
}

// ── service-level round-trip (DB backed) ─────────────────────────────────────

func openTestDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	return pool, func() { pool.Close() }
}

func TestServiceCreateRevealRoundTrip(t *testing.T) {
	pool, cleanup := openTestDB(t)
	defer cleanup()

	kek, _ := randomBytes(32)
	cfg := &config.Config{
		SecretKEKEnv: base64.StdEncoding.EncodeToString(kek),
	}
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	wantValue := "my-super-secret-db-password"
	sc, err := svc.Create(ctx, CreateInput{
		Key:    "DB_PASSWORD",
		Source: "stored",
		Value:  wantValue,
	}, "alice@example.com")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sc.ID == "" || sc.Source != "stored" {
		t.Fatalf("unexpected secret metadata: %+v", sc)
	}

	// List should not expose the value (no Value field in Secret struct). nil scopes
	// ⇒ unrestricted (admin) view.
	list, err := svc.List(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 secret, got %d", len(list))
	}

	// Reveal should return the plaintext.
	got, err := svc.Reveal(ctx, sc.ID)
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if got != wantValue {
		t.Fatalf("reveal mismatch: got %q, want %q", got, wantValue)
	}
}

func TestServiceUpdateSecret(t *testing.T) {
	pool, cleanup := openTestDB(t)
	defer cleanup()

	kek, _ := randomBytes(32)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek)}
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	sc, _ := svc.Create(ctx, CreateInput{Key: "MY_KEY", Source: "stored", Value: "v1"}, "alice@example.com")
	_, err := svc.Update(ctx, sc.ID, UpdateInput{Key: "MY_KEY", Source: "stored", Value: "v2"}, "alice@example.com")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := svc.Reveal(ctx, sc.ID)
	if err != nil {
		t.Fatalf("Reveal after update: %v", err)
	}
	if got != "v2" {
		t.Fatalf("expected v2, got %q", got)
	}
}

func TestServiceVaultSourceNotRevealable(t *testing.T) {
	pool, cleanup := openTestDB(t)
	defer cleanup()

	cfg := &config.Config{}
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	sc, err := svc.Create(ctx, CreateInput{
		Key:       "VAULT_SECRET",
		Source:    "vault",
		VaultPath: "secret/data/app#VAULT_SECRET",
	}, "alice@example.com")
	if err != nil {
		t.Fatalf("Create vault secret: %v", err)
	}
	if sc.Source != "vault" {
		t.Fatalf("source should be vault, got %s", sc.Source)
	}

	_, err = svc.Reveal(ctx, sc.ID)
	if err == nil {
		t.Fatal("expected error revealing vault-source secret")
	}
}

func TestRedactionValues(t *testing.T) {
	pool, cleanup := openTestDB(t)
	defer cleanup()

	kek, _ := randomBytes(32)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek)}
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	values := []string{"secret1", "secret2", "secret3"}
	for i, v := range values {
		key := string([]byte{'A' + byte(i)})
		_, err := svc.Create(ctx, CreateInput{
			Key:    "KEY_" + key,
			Source: "stored",
			Value:  v,
		}, "alice@example.com")
		if err != nil {
			t.Fatalf("create secret %d: %v", i, err)
		}
	}
	// Add a vault-source secret — should not appear in redaction values.
	_, err := svc.Create(ctx, CreateInput{
		Key: "VAULT_KEY", Source: "vault", VaultPath: "secret/data/app#VAULT_KEY",
	}, "alice@example.com")
	if err != nil {
		t.Fatalf("create vault secret: %v", err)
	}

	got, err := RedactionValues(ctx, pool, cfg)
	if err != nil {
		t.Fatalf("RedactionValues: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 redaction values (stored only), got %d: %v", len(got), got)
	}
	gotSet := map[string]bool{}
	for _, v := range got {
		gotSet[v] = true
	}
	for _, v := range values {
		if !gotSet[v] {
			t.Errorf("expected value %q in redaction set, not found", v)
		}
	}
}

func TestNoKEKReturnsNilRedactionValues(t *testing.T) {
	pool, cleanup := openTestDB(t)
	defer cleanup()

	cfg := &config.Config{} // no KEK configured
	ctx := context.Background()

	got, err := RedactionValues(ctx, pool, cfg)
	if err != nil {
		t.Fatalf("RedactionValues should not error when KEK absent, got: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

// TestRedactionValuesIncludesSshCredentials guards the SK.14 regression: a key
// created directly as a stored ssh_credentials row (no backing secret) must still
// enter the redaction dictionary, or its private material could echo into run logs.
func TestRedactionValuesIncludesSshCredentials(t *testing.T) {
	pool, cleanup := openTestDB(t)
	defer cleanup()

	kek, _ := randomBytes(32)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek)}
	ctx := context.Background()

	material := "CRONOMICON-TEST-SSH-PRIVATE-KEY-7f3a9c2e"
	sealed, err := NewSealer(cfg).Seal([]byte(material))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := pool.Exec(
		`INSERT INTO ssh_credentials (id, label, source, ciphertext, nonce, wrapped_dek, kek_version, created_at)
		 VALUES ('c1', 'prod-key', 'stored', ?, ?, ?, ?, 't')`,
		sealed.Ciphertext, sealed.Nonce, sealed.WrappedDEK, sealed.KEKVersion); err != nil {
		t.Fatalf("insert credential: %v", err)
	}

	vals, err := RedactionValues(ctx, pool, cfg)
	if err != nil {
		t.Fatalf("RedactionValues: %v", err)
	}
	found := slices.Contains(vals, material)
	if !found {
		t.Fatalf("stored SSH credential material not in redaction dictionary (got %d values)", len(vals))
	}
}

func TestLoadKEKPrecedenceFileOverEnv(t *testing.T) {
	kek1, _ := randomBytes(32)
	kek2, _ := randomBytes(32)

	tmpFile := filepath.Join(t.TempDir(), "kek.key")
	kek1B64 := base64.StdEncoding.EncodeToString(kek1)
	if err := os.WriteFile(tmpFile, []byte(kek1B64), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		SecretKEKFile: tmpFile,
		SecretKEKEnv:  base64.StdEncoding.EncodeToString(kek2),
	}
	got, err := loadKEK(cfg)
	if err != nil {
		t.Fatalf("loadKEK: %v", err)
	}
	if string(got) != string(kek1) {
		t.Error("file should take precedence over env var")
	}
}

func TestLoadKEKNoConfig(t *testing.T) {
	cfg := &config.Config{}
	_, err := loadKEK(cfg)
	if err == nil {
		t.Fatal("expected error when no KEK configured")
	}
}

// TestKEKRotation proves PP-L5: after rotating the active KEK version, a secret
// wrapped under the old version still reveals (via the historical key supplied at
// CRONOMICON_KEK_<N>), while new writes are wrapped under the new active version.
func TestKEKRotation(t *testing.T) {
	pool, cleanup := openTestDB(t)
	defer cleanup()
	ctx := context.Background()

	kek1, _ := randomBytes(32)
	kek2, _ := randomBytes(32)
	kek1B64 := base64.StdEncoding.EncodeToString(kek1)
	kek2B64 := base64.StdEncoding.EncodeToString(kek2)

	// Phase 1: active version 1, write a secret.
	cfgV1 := &config.Config{SecretKEKEnv: kek1B64, SecretKEKVersion: 1}
	svc1 := New(pool, cfgV1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sc, err := svc1.Create(ctx, CreateInput{Key: "OLD", Source: "stored", Value: "old-value"}, "alice")
	if err != nil {
		t.Fatalf("create under v1: %v", err)
	}
	var storedVer int
	_ = pool.QueryRow(`SELECT kek_version FROM secrets WHERE id=?`, sc.ID).Scan(&storedVer)
	if storedVer != 1 {
		t.Fatalf("secret written under v1 has kek_version=%d, want 1", storedVer)
	}

	// Phase 2: rotate — active version 2 (new key), old key kept at CRONOMICON_KEK_1.
	t.Setenv("CRONOMICON_KEK_1", kek1B64)
	cfgV2 := &config.Config{SecretKEKEnv: kek2B64, SecretKEKVersion: 2}
	svc2 := New(pool, cfgV2, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// The old secret must still reveal via the historical KEK.
	got, err := svc2.Reveal(ctx, sc.ID)
	if err != nil {
		t.Fatalf("reveal old secret after rotation: %v", err)
	}
	if got != "old-value" {
		t.Fatalf("old secret reveal mismatch: got %q, want old-value", got)
	}

	// A new secret is wrapped under the active version (2).
	scNew, err := svc2.Create(ctx, CreateInput{Key: "NEW", Source: "stored", Value: "new-value"}, "alice")
	if err != nil {
		t.Fatalf("create under v2: %v", err)
	}
	_ = pool.QueryRow(`SELECT kek_version FROM secrets WHERE id=?`, scNew.ID).Scan(&storedVer)
	if storedVer != 2 {
		t.Fatalf("secret written under v2 has kek_version=%d, want 2", storedVer)
	}
	gotNew, err := svc2.Reveal(ctx, scNew.ID)
	if err != nil || gotNew != "new-value" {
		t.Fatalf("reveal new secret: got %q err=%v", gotNew, err)
	}

	// RedactionValues must surface BOTH (mixed-version table), proving per-row
	// KEK selection in the redaction path.
	vals, err := RedactionValues(ctx, pool, cfgV2)
	if err != nil {
		t.Fatalf("RedactionValues after rotation: %v", err)
	}
	set := map[string]bool{}
	for _, v := range vals {
		set[v] = true
	}
	if !set["old-value"] || !set["new-value"] {
		t.Fatalf("redaction set missing rotated values: %v", vals)
	}
}

// TestEncryptStringRotation proves config-blob tokens survive a KEK rotation: a
// token written under v1 still decrypts via the historical key, and new tokens
// carry the active version.
func TestEncryptStringRotation(t *testing.T) {
	kek1, _ := randomBytes(32)
	kek2, _ := randomBytes(32)
	kek1B64 := base64.StdEncoding.EncodeToString(kek1)
	kek2B64 := base64.StdEncoding.EncodeToString(kek2)

	cfgV1 := &config.Config{SecretKEKEnv: kek1B64, SecretKEKVersion: 1}
	tok, err := EncryptString(cfgV1, "pat-token")
	if err != nil {
		t.Fatalf("encrypt under v1: %v", err)
	}
	if got := tok[:5]; got != "v2:1:" {
		t.Fatalf("token prefix = %q, want v2:1:", got)
	}

	// Rotate to v2; keep v1 key available.
	t.Setenv("CRONOMICON_KEK_1", kek1B64)
	cfgV2 := &config.Config{SecretKEKEnv: kek2B64, SecretKEKVersion: 2}
	got, err := DecryptString(cfgV2, tok)
	if err != nil {
		t.Fatalf("decrypt v1 token after rotation: %v", err)
	}
	if got != "pat-token" {
		t.Fatalf("decrypt mismatch: got %q", got)
	}

	// A legacy "v1:"-prefixed token (pre-PP-L5 format) decrypts as version 1 too.
	legacy := "v1:" + tok[5:]
	got, err = DecryptString(cfgV2, legacy)
	if err != nil {
		t.Fatalf("decrypt legacy v1 token: %v", err)
	}
	if got != "pat-token" {
		t.Fatalf("legacy decrypt mismatch: got %q", got)
	}
}

func TestDeleteSecret(t *testing.T) {
	pool, cleanup := openTestDB(t)
	defer cleanup()

	kek, _ := randomBytes(32)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek)}
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	sc, _ := svc.Create(ctx, CreateInput{Key: "TO_DELETE", Source: "stored", Value: "val"}, "alice@example.com")
	found, err := svc.Delete(ctx, sc.ID)
	if err != nil || !found {
		t.Fatalf("Delete: found=%v err=%v", found, err)
	}
	// Second delete should return found=false.
	found, err = svc.Delete(ctx, sc.ID)
	if err != nil || found {
		t.Fatalf("double delete: found=%v err=%v", found, err)
	}
}

// TestLoadKEKForVersionNames proves the historical-KEK lookup reads the
// CRONOMICON_KEK_<N>[_FILE] names. (The deprecated CRONOMICON_SECRET_KEK_<N> fallback
// was removed in v1.5.41.)
func TestLoadKEKForVersionNames(t *testing.T) {
	kek, _ := randomBytes(32)
	b64 := base64.StdEncoding.EncodeToString(kek)
	// Active version is 5, so version 3 goes through the historical path.
	cfg := &config.Config{SecretKEKVersion: 5}

	t.Run("new name", func(t *testing.T) {
		t.Setenv("CRONOMICON_KEK_3", b64)
		got, err := loadKEKForVersion(cfg, 3)
		if err != nil {
			t.Fatalf("loadKEKForVersion via CRONOMICON_KEK_3: %v", err)
		}
		if !bytes.Equal(got, kek) {
			t.Fatal("KEK mismatch via new name")
		}
	})

}

// TestListScopeFilter (P1.7/D3): List returns only secrets visible to the actor's
// scope grants — global (NULL/”) rows always; scoped rows only when allowed; an
// empty allowedScopes (admin) sees everything.
func TestListScopeFilter(t *testing.T) {
	pool, cleanup := openTestDB(t)
	defer cleanup()
	kek, _ := randomBytes(32)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek)}
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	mk := func(key string, scope *string) {
		t.Helper()
		if _, err := svc.Create(ctx, CreateInput{Key: key, Source: "stored", Scope: scope, Value: "v"}, "seed"); err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
	}
	sp := func(s string) *string { return &s }
	mk("GLOBAL_ONE", nil)
	mk("PROD_ONE", sp("prod"))
	mk("STAGING_ONE", sp("staging"))

	keys := func(canRead func(string) bool) map[string]bool {
		t.Helper()
		list, err := svc.List(ctx, canRead)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		out := map[string]bool{}
		for _, sc := range list {
			out[sc.Key] = true
		}
		return out
	}
	// A5 scope-model: List takes the caller's read decision. "unrestricted" reaches
	// every scope; a restricted actor reaches global (scope "") plus its granted set.
	unrestricted := func(string) bool { return true }
	restrictedTo := func(scopes ...string) func(string) bool {
		set := map[string]bool{}
		for _, s := range scopes {
			set[s] = true
		}
		return func(scope string) bool { return scope == "" || set[scope] }
	}

	// Unrestricted (admin): all three.
	if got := keys(unrestricted); len(got) != 3 {
		t.Errorf("unrestricted List = %v, want all 3", got)
	}
	// Restricted to prod: global + prod, never staging.
	got := keys(restrictedTo("prod"))
	if !got["GLOBAL_ONE"] || !got["PROD_ONE"] || got["STAGING_ONE"] {
		t.Errorf("prod-restricted List = %v, want GLOBAL_ONE+PROD_ONE only", got)
	}
	// Restricted to a scope with no scoped secrets: global only.
	if got := keys(restrictedTo("dev")); !got["GLOBAL_ONE"] || got["PROD_ONE"] || got["STAGING_ONE"] {
		t.Errorf("dev-restricted List = %v, want GLOBAL_ONE only", got)
	}
}

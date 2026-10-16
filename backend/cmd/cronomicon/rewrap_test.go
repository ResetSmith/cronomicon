package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

func newKEK(t *testing.T) (raw []byte, b64 string) {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k, base64.StdEncoding.EncodeToString(k)
}

// seedV1 stands up a migrated DB holding one of each thing the command must
// cover, all sealed under KEK v1: an envelope row in each of the two envelope
// stores, and one EncryptString settings token.
func seedV1(t *testing.T, cfgV1 *config.Config) (*sql.DB, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "rewrap.db")
	pool, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	sealer := secrets.NewSealer(cfgV1)
	seal := func(plain string) secrets.Sealed {
		s, err := sealer.Seal([]byte(plain))
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		return s
	}

	s1 := seal("secret-value-1")
	if _, err := pool.Exec(
		`INSERT INTO secrets (id, key, scope, source, ciphertext, nonce, wrapped_dek, kek_version, created_at)
		 VALUES ('sec1','API_KEY','', 'stored', ?, ?, ?, ?, '2026-08-26T00:00:00Z')`,
		s1.Ciphertext, s1.Nonce, s1.WrappedDEK, s1.KEKVersion); err != nil {
		t.Fatalf("insert secret: %v", err)
	}

	s2 := seal("-----BEGIN OPENSSH PRIVATE KEY-----")
	if _, err := pool.Exec(
		`INSERT INTO ssh_credentials (id, label, source, ciphertext, nonce, wrapped_dek, kek_version, created_at)
		 VALUES ('cred1','prod-key','stored', ?, ?, ?, ?, '2026-08-26T00:00:00Z')`,
		s2.Ciphertext, s2.Nonce, s2.WrappedDEK, s2.KEKVersion); err != nil {
		t.Fatalf("insert ssh_credential: %v", err)
	}

	tok, err := secrets.EncryptString(cfgV1, "glpat-supersecret")
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}
	// Global's repository's token, on its row of git_repos (migration 1310).
	if _, err := pool.Exec(`UPDATE git_repos SET token_enc=? WHERE id='global'`, tok); err != nil {
		t.Fatalf("set Global's token: %v", err)
	}
	return pool, dbPath
}

// A repository is a row, and every repository's token and webhook secrets are
// re-wrapped: Global's and an agency's. The previous webhook secret, which a
// rotation keeps for an overlap, is one of them (it was on no list until 2.4.0).
// Two rows that hold the SAME stored token are two items, each moved and each
// counted: the update is addressed by the row, not by the token value alone.
func TestRewrapCoversEveryRepository(t *testing.T) {
	_, k1b64 := newKEK(t)
	cfgV1 := &config.Config{SecretKEKEnv: k1b64, SecretKEKVersion: 1}
	pool, _ := seedV1(t, cfgV1)
	defer pool.Close()
	enc := func(plain string) string {
		t.Helper()
		tok, err := secrets.EncryptString(cfgV1, plain)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	var globalTok string
	if err := pool.QueryRow(`SELECT token_enc FROM git_repos WHERE id='global'`).Scan(&globalTok); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`UPDATE git_repos SET webhook_secret_enc=?, webhook_secret_prev_enc=? WHERE id='global'`,
		enc("global-hook"), enc("global-hook-previous")); err != nil {
		t.Fatal(err)
	}
	// The agency's row holds the very same stored token as Global's.
	if _, err := pool.Exec(`INSERT INTO git_repos (id, agency_id, token_enc, webhook_secret_enc, webhook_secret_prev_enc) VALUES ('repo-b', 'ag-b', ?, ?, ?)`,
		globalTok, enc("agency-hook"), enc("agency-hook-previous")); err != nil {
		t.Fatal(err)
	}

	_, k2b64 := newKEK(t)
	cfgV2 := &config.Config{SecretKEKEnv: k2b64, SecretKEKVersion: 2}
	t.Setenv("CRONOMICON_KEK_1", k1b64)
	ctx := context.Background()

	// The secret, the SSH credential, and three tokens of each repository.
	before, err := survey(ctx, pool, 2)
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	if before.outstanding() != 8 {
		t.Fatalf("outstanding before the pass = %d, want 8", before.outstanding())
	}
	moved, failed := rewrapAll(ctx, pool, cfgV2, secrets.NewSealer(cfgV2), 2)
	if moved != 8 || failed != 0 {
		t.Fatalf("moved=%d failed=%d, want 8 and 0", moved, failed)
	}
	after, err := survey(ctx, pool, 2)
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	if after.outstanding() != 0 {
		t.Fatalf("%d still outstanding after the pass", after.outstanding())
	}
	for _, c := range []struct{ id, col, want string }{
		{"global", "token_enc", "glpat-supersecret"},
		{"global", "webhook_secret_enc", "global-hook"},
		{"global", "webhook_secret_prev_enc", "global-hook-previous"},
		{"repo-b", "token_enc", "glpat-supersecret"},
		{"repo-b", "webhook_secret_enc", "agency-hook"},
		{"repo-b", "webhook_secret_prev_enc", "agency-hook-previous"},
	} {
		var tok string
		if err := pool.QueryRow(`SELECT `+c.col+` FROM git_repos WHERE id=?`, c.id).Scan(&tok); err != nil {
			t.Fatal(err)
		}
		if v, _ := secrets.TokenKEKVersion(tok); v != 2 {
			t.Errorf("%s of %s should be at v2, got v%d", c.col, c.id, v)
		}
		if plain, err := secrets.DecryptString(cfgV2, tok); err != nil || plain != c.want {
			t.Errorf("%s of %s under the new key = %q (%v), want %q", c.col, c.id, plain, err, c.want)
		}
	}
}

// The whole point of DR-5, proven end to end: everything moves to the new
// version AND still decrypts to the same plaintext afterwards. A re-wrap that
// moved the version but corrupted a value would be the worst possible outcome,
// so the assertions are on the recovered plaintexts, not on kek_version alone.
func TestRewrapMovesEveryStoreAndPreservesPlaintext(t *testing.T) {
	k1, k1b64 := newKEK(t)
	_ = k1
	cfgV1 := &config.Config{SecretKEKEnv: k1b64, SecretKEKVersion: 1}
	pool, _ := seedV1(t, cfgV1)
	defer pool.Close()

	_, k2b64 := newKEK(t)
	cfgV2 := &config.Config{SecretKEKEnv: k2b64, SecretKEKVersion: 2}
	t.Setenv("CRONOMICON_KEK_1", k1b64) // the superseded key stays available

	ctx := context.Background()
	before, err := survey(ctx, pool, 2)
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	if before.outstanding() != 3 {
		t.Fatalf("expected 3 outstanding items before the pass, got %d", before.outstanding())
	}

	moved, failed := rewrapAll(ctx, pool, cfgV2, secrets.NewSealer(cfgV2), 2)
	if failed != 0 {
		t.Fatalf("re-wrap reported %d failure(s)", failed)
	}
	if moved != 3 {
		t.Fatalf("expected 3 items moved, got %d", moved)
	}

	after, err := survey(ctx, pool, 2)
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	if after.outstanding() != 0 {
		t.Fatalf("rotation should be complete, %d still outstanding", after.outstanding())
	}

	// Values must round-trip under the NEW key alone — which is what makes it
	// safe to drop the old one.
	sealer2 := secrets.NewSealer(cfgV2)
	openRow := func(table, id string) string {
		var ct, nonce, wrapped []byte
		var ver int
		if err := pool.QueryRow(
			"SELECT ciphertext, nonce, wrapped_dek, COALESCE(kek_version,1) FROM "+table+" WHERE id=?", id,
		).Scan(&ct, &nonce, &wrapped, &ver); err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		if ver != 2 {
			t.Errorf("%s %s should be at v2, got v%d", table, id, ver)
		}
		plain, err := sealer2.Open(ct, nonce, wrapped, ver)
		if err != nil {
			t.Fatalf("open %s under the new key: %v", table, err)
		}
		return string(plain)
	}
	if got := openRow("secrets", "sec1"); got != "secret-value-1" {
		t.Errorf("secret plaintext changed: %q", got)
	}
	if got := openRow("ssh_credentials", "cred1"); got != "-----BEGIN OPENSSH PRIVATE KEY-----" {
		t.Errorf("ssh credential plaintext changed: %q", got)
	}

	var tok string
	if err := pool.QueryRow(`SELECT token_enc FROM git_repos WHERE id='global'`).Scan(&tok); err != nil {
		t.Fatal(err)
	}
	if v, _ := secrets.TokenKEKVersion(tok); v != 2 {
		t.Errorf("settings token should be at v2, got v%d", v)
	}
	plain, err := secrets.DecryptString(cfgV2, tok)
	if err != nil {
		t.Fatalf("decrypt token under the new key: %v", err)
	}
	if plain != "glpat-supersecret" {
		t.Errorf("token plaintext changed: %q", plain)
	}
}

// Idempotence: re-running is the documented recovery for a partial pass, so a
// second run must be a no-op rather than double-wrapping anything.
func TestRewrapIsIdempotent(t *testing.T) {
	_, k1b64 := newKEK(t)
	cfgV1 := &config.Config{SecretKEKEnv: k1b64, SecretKEKVersion: 1}
	pool, _ := seedV1(t, cfgV1)
	defer pool.Close()

	_, k2b64 := newKEK(t)
	cfgV2 := &config.Config{SecretKEKEnv: k2b64, SecretKEKVersion: 2}
	t.Setenv("CRONOMICON_KEK_1", k1b64)

	ctx := context.Background()
	if moved, failed := rewrapAll(ctx, pool, cfgV2, secrets.NewSealer(cfgV2), 2); moved != 3 || failed != 0 {
		t.Fatalf("first pass: moved=%d failed=%d", moved, failed)
	}
	moved, failed := rewrapAll(ctx, pool, cfgV2, secrets.NewSealer(cfgV2), 2)
	if moved != 0 || failed != 0 {
		t.Fatalf("second pass must be a no-op, got moved=%d failed=%d", moved, failed)
	}
}

// A missing historical key must be reported, not silently skipped — otherwise a
// partial pass reads as complete and an operator drops a key that is still load
// bearing. The residual outstanding count is the second half of that guard.
func TestRewrapReportsMissingHistoricalKey(t *testing.T) {
	_, k1b64 := newKEK(t)
	cfgV1 := &config.Config{SecretKEKEnv: k1b64, SecretKEKVersion: 1}
	pool, _ := seedV1(t, cfgV1)
	defer pool.Close()

	_, k2b64 := newKEK(t)
	cfgV2 := &config.Config{SecretKEKEnv: k2b64, SecretKEKVersion: 2}
	// Deliberately do NOT provide CRONOMICON_KEK_1.

	ctx := context.Background()
	moved, failed := rewrapAll(ctx, pool, cfgV2, secrets.NewSealer(cfgV2), 2)
	if moved != 0 {
		t.Errorf("nothing should move without the old key, moved=%d", moved)
	}
	if failed != 3 {
		t.Errorf("all 3 items should be reported as failures, got %d", failed)
	}
	after, err := survey(ctx, pool, 2)
	if err != nil {
		t.Fatal(err)
	}
	if after.outstanding() != 3 {
		t.Errorf("survey must still report the work as outstanding, got %d", after.outstanding())
	}
}

// The token-column table is the half an implementation written against the
// envelope columns alone would miss. Pin that every entry addresses a real
// column, so a typo or a renamed column fails here rather than silently skipping
// a credential during a real rotation.
func TestTokenColumnsAllAddressRealColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cols.db")
	pool, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	for _, tc := range tokenColumns {
		if _, err := readTokens(ctx, pool, tc); err != nil {
			t.Errorf("%s (%s.%s WHERE %s): %v", tc.Label, tc.Table, tc.Column, tc.Where, err)
		}
	}
}

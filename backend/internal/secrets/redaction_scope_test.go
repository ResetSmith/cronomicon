package secrets

import (
	"context"
	"encoding/base64"
	"slices"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// 2.5.1 SG2.8 (SG-18): a run's dictionary holds its own agency's values and
// Global's, never another agency's — secrets, keys and repository tokens
// alike — while the process-wide report still holds every one.
func TestRedactionValuesForKeepsOtherAgenciesOut(t *testing.T) {
	pool, cleanup := openTestDB(t)
	defer cleanup()
	kek, _ := randomBytes(32)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek), SecretKEKVersion: 1}
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin','FIN','now'), ('ag-tax','TAX','now')`,
		`INSERT INTO git_repos (id, agency_id) VALUES ('repo-fin','ag-fin'), ('repo-tax','ag-tax')`,
	} {
		if _, err := pool.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	seal := func(table, id, owner, plain string) {
		t.Helper()
		ct, nonce, wrapped, err := envelopeEncrypt(kek, []byte(plain))
		if err != nil {
			t.Fatal(err)
		}
		q := `INSERT INTO secrets (id, key, source, ciphertext, nonce, wrapped_dek, kek_version, created_at, owner_agency) VALUES (?, ?, 'stored', ?, ?, ?, 1, 'now', ?)`
		if table == "ssh_credentials" {
			q = `INSERT INTO ssh_credentials (id, label, source, ciphertext, nonce, wrapped_dek, kek_version, created_at, last_modified_at, owner_agency) VALUES (?, ?, 'stored', ?, ?, ?, 1, 'now', 'now', ?)`
		}
		if _, err := pool.ExecContext(ctx, q, id, id, ct, nonce, wrapped, owner); err != nil {
			t.Fatalf("seal %s: %v", id, err)
		}
	}
	seal("secrets", "s-fin", "ag-fin", "fin-secret-value")
	seal("secrets", "s-tax", "ag-tax", "tax-secret-value")
	seal("secrets", "s-glob", "global", "global-secret-value")
	seal("ssh_credentials", "k-tax", "ag-tax", "tax-key-material")
	for repo, plain := range map[string]string{"repo-fin": "fin-repo-token", "repo-tax": "tax-repo-token"} {
		tok, err := EncryptString(cfg, plain)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, `UPDATE git_repos SET token_enc = ? WHERE id = ?`, tok, repo); err != nil {
			t.Fatal(err)
		}
	}

	fin, err := RedactionValuesFor(ctx, pool, cfg, []string{"ag-fin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"fin-secret-value", "global-secret-value", "fin-repo-token"} {
		if !slices.Contains(fin, v) {
			t.Errorf("FIN's run does not mask %q", v)
		}
	}
	for _, v := range []string{"tax-secret-value", "tax-key-material", "tax-repo-token"} {
		if slices.Contains(fin, v) {
			t.Errorf("FIN's run masks TAX's %q (a value one agency can test another's guess against)", v)
		}
	}
	all, _, err := RedactionReport(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"tax-secret-value", "tax-key-material", "tax-repo-token", "fin-secret-value"} {
		if !slices.Contains(all, v) {
			t.Errorf("the process-wide report lost %q", v)
		}
	}
}

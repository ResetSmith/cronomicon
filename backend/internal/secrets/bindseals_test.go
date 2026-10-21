package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// 2.5.1 SG2.10 (question 134(a)): BindSeals binds every value sealed before
// 2.5.1 to its row, then refuses unbound seals; a bound value copied into
// another row does not open there.
func TestBindSeals(t *testing.T) {
	t.Cleanup(func() { RefuseUnboundSeals(false) })
	pool, cleanup := openTestDB(t)
	defer cleanup()
	kek, _ := randomBytes(32)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek), SecretKEKVersion: 1}
	ctx := context.Background()
	sealer := NewSealer(cfg)

	// A database as 2.5.0 left it: unbound rows and an unbound (v2) token.
	legacy := func(table, id, plain string) {
		t.Helper()
		s, err := sealer.SealLegacy([]byte(plain))
		if err != nil {
			t.Fatal(err)
		}
		q := `INSERT INTO secrets (id, key, source, ciphertext, nonce, wrapped_dek, kek_version, created_at) VALUES (?, ?, 'stored', ?, ?, ?, 1, 'now')`
		if table == "ssh_credentials" {
			q = `INSERT INTO ssh_credentials (id, label, source, ciphertext, nonce, wrapped_dek, kek_version, created_at, last_modified_at) VALUES (?, ?, 'stored', ?, ?, ?, 1, 'now', 'now')`
		}
		if _, err := pool.ExecContext(ctx, q, id, id, s.Ciphertext, s.Nonce, s.WrappedDEK); err != nil {
			t.Fatal(err)
		}
	}
	legacy("secrets", "s1", "first-secret")
	legacy("secrets", "s2", "second-secret")
	legacy("ssh_credentials", "k1", "key-material")
	tok, err := EncryptString(cfg, "smtp-pass")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO notification_config(id, smtp_password_enc, last_modified_by, last_modified_at) VALUES(1, ?, 't', 'now')
		ON CONFLICT(id) DO UPDATE SET smtp_password_enc = excluded.smtp_password_enc`, tok); err != nil {
		t.Fatal(err)
	}

	rep, err := BindSeals(ctx, pool, cfg)
	if err != nil || !rep.Complete || rep.Bound != 4 || rep.Failed != 0 {
		t.Fatalf("BindSeals = %+v, %v; want 4 bound, complete", rep, err)
	}
	if !UnboundSealsRefused() {
		t.Fatal("unbound seals are not refused after a complete pass")
	}
	svc := New(pool, cfg, nil)
	for id, want := range map[string]string{"s1": "first-secret", "s2": "second-secret"} {
		if got, err := svc.Reveal(ctx, id); err != nil || got != want {
			t.Errorf("reveal %s after binding = %q, %v", id, got, err)
		}
	}
	var stored string
	_ = pool.QueryRowContext(ctx, `SELECT smtp_password_enc FROM notification_config WHERE id=1`).Scan(&stored)
	if !TokenBound(stored) {
		t.Fatalf("the SMTP token is not bound: %q", stored[:3])
	}
	if got, err := DecryptSetting(cfg, SettingsBinding(LabelSMTPPassword, ""), stored); err != nil || got != "smtp-pass" {
		t.Errorf("the bound SMTP token opens as %q, %v", got, err)
	}
	if _, err := DecryptSetting(cfg, SettingsBinding(LabelGitToken, "repo-x"), stored); err == nil {
		t.Error("a bound token opened under another row's binding")
	}

	// A bound secret copied into another row does not open there.
	if _, err := pool.ExecContext(ctx, `UPDATE secrets SET ciphertext = (SELECT ciphertext FROM secrets WHERE id='s1'),
		nonce = (SELECT nonce FROM secrets WHERE id='s1'), wrapped_dek = (SELECT wrapped_dek FROM secrets WHERE id='s1') WHERE id='s2'`); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Reveal(ctx, "s2"); err == nil {
		t.Errorf("s1's value copied into s2 opened there: %q", got)
	}

	// An unbound value slipped in after the pass is refused.
	legacy("secrets", "s3", "slipped-in")
	if _, err := svc.Reveal(ctx, "s3"); !errors.Is(err, ErrSealUnbound) {
		t.Errorf("an unbound value after the pass: %v, want ErrSealUnbound", err)
	}
	if _, err := DecryptSetting(cfg, SettingsBinding(LabelSMTPPassword, ""), tok); !errors.Is(err, ErrSealUnbound) {
		t.Errorf("an unbound token after the pass: %v, want ErrSealUnbound", err)
	}

	// A second start reads the marker and changes nothing.
	RefuseUnboundSeals(false)
	rep, err = BindSeals(ctx, pool, cfg)
	if err != nil || !rep.Complete || rep.Bound != 0 || !UnboundSealsRefused() {
		t.Fatalf("second BindSeals = %+v, %v", rep, err)
	}
}

// A value BindSeals cannot open (a KEK version not configured) is left as it
// is, no marker is written, and unbound values stay readable.
func TestBindSealsLeavesWhatItCannotOpen(t *testing.T) {
	t.Cleanup(func() { RefuseUnboundSeals(false) })
	pool, cleanup := openTestDB(t)
	defer cleanup()
	kek, _ := randomBytes(32)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek), SecretKEKVersion: 1}
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `INSERT INTO secrets (id, key, source, ciphertext, nonce, wrapped_dek, kek_version, created_at)
		VALUES ('s-old', 'OLD', 'stored', x'00', x'00', x'00', 7, 'now')`); err != nil {
		t.Fatal(err)
	}
	rep, err := BindSeals(ctx, pool, cfg)
	if err != nil || rep.Complete || rep.Failed != 1 || UnboundSealsRefused() {
		t.Fatalf("BindSeals = %+v, %v; want one failure, no marker, unbound still readable", rep, err)
	}
	var n int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key = ?`, bindSealsMarker).Scan(&n)
	if n != 0 {
		t.Error("the marker was written with a value left unbound")
	}
	if rep, _ := BindSeals(ctx, pool, &config.Config{}); !rep.NoKEK {
		t.Errorf("with no KEK: %+v", rep)
	}
}

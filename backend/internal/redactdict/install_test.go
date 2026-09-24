package redactdict

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

func resetGlobals(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		auditlog.SetRedactor(nil)
		secrets.SetRedactionChangeHook(nil)
		std.Store(nil)
	})
}

func countRows(t *testing.T, pool *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := pool.QueryRowContext(context.Background(), q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestInstallMasksAuditRowsEndToEnd: with Install wired, a change_log row that
// carries a stored secret's plaintext is stored masked; after the secret is
// ROTATED through the service (which fires the hook), a row carrying the new
// value is masked too — without anyone calling Invalidate by hand.
func TestInstallMasksAuditRowsEndToEnd(t *testing.T) {
	resetGlobals(t)
	pool, cfg, ctx := openPool(t), kekCfg(t), context.Background()
	Install(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc := secrets.New(pool, cfg, quietLog())

	sec, err := svc.Create(ctx, secrets.CreateInput{Key: "TOKEN", Source: "stored", Value: "first-token-value"}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := auditlog.WriteChangeLog(ctx, pool, "alice", "Secrets", "created", "TOKEN", "value was first-token-value"); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := pool.QueryRowContext(ctx, `SELECT details FROM change_log WHERE action='created'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "first-token-value") || !strings.Contains(stored, Mask) {
		t.Fatalf("row not masked: %q", stored)
	}

	newVal := "second-token-value"
	if _, err := svc.Update(ctx, sec.ID, secrets.UpdateInput{Key: "TOKEN", Source: "stored", Value: newVal}, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := auditlog.WriteChangeLog(ctx, pool, "alice", "Secrets", "rotated", "TOKEN", "value is now "+newVal); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRowContext(ctx, `SELECT details FROM change_log WHERE action='rotated'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, newVal) {
		t.Fatalf("the rotation's own audit row leaked the new value: %q — the hook did not invalidate", stored)
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM change_log WHERE category='Audit'`); n != 0 {
		t.Errorf("healthy install wrote %d self-audit rows, want 0", n)
	}
}

// TestInstallReportsAnOutageOnceAndItsRecoveryOnce: a KEK the rows were not
// wrapped under makes every build partial. The first masked write records ONE
// redactor-unavailable row; further writes record none; once the KEK is back
// and a rebuild succeeds, ONE redactor-restored row.
func TestInstallReportsAnOutageOnceAndItsRecoveryOnce(t *testing.T) {
	resetGlobals(t)
	pool, cfg, ctx := openPool(t), kekCfg(t), context.Background()
	svc := secrets.New(pool, cfg, quietLog())
	if _, err := svc.Create(ctx, secrets.CreateInput{Key: "K", Source: "stored", Value: "value-behind-lost-kek"}, "alice"); err != nil {
		t.Fatal(err)
	}
	goodKEK := cfg.SecretKEKEnv
	cfg.SecretKEKEnv = "" // the KEK is gone when the process comes up
	st := Install(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	for i := 0; i < 3; i++ {
		if err := auditlog.WriteChangeLog(ctx, pool, "alice", "Jobs", "Triggered", "j", "manual run"); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM change_log WHERE action='redactor-unavailable'`); n != 1 {
		t.Fatalf("redactor-unavailable rows = %d, want exactly 1 across 3 writes", n)
	}
	var details string
	if err := pool.QueryRowContext(ctx, `SELECT details FROM change_log WHERE action='redactor-unavailable'`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, "1 value(s)") {
		t.Errorf("self-audit row should say how many values could not be decrypted: %q", details)
	}

	cfg.SecretKEKEnv = goodKEK
	st.Invalidate()
	for i := 0; i < 3; i++ {
		if err := auditlog.WriteChangeLog(ctx, pool, "alice", "Jobs", "Triggered", "j", "manual run "+"value-behind-lost-kek"); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM change_log WHERE action='redactor-restored'`); n != 1 {
		t.Fatalf("redactor-restored rows = %d, want exactly 1", n)
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM change_log WHERE details LIKE '%value-behind-lost-kek%'`); n != 0 {
		t.Errorf("%d rows carry the value after recovery — the rebuilt dictionary was not applied", n)
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM change_log WHERE action='redactor-unavailable'`); n != 1 {
		t.Errorf("recovery must not re-report the outage: %d rows", n)
	}
}

package secrets

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// TestRedactionReportCoversEverySettingsColumn: each of the
// EncryptedSettingsColumns decrypts into the dictionary — the table is shared
// with rewrap-secrets precisely so the two cannot drift again (AM-4a).
func TestRedactionReportCoversEverySettingsColumn(t *testing.T) {
	pool, cleanup := openTestDB(t)
	defer cleanup()
	kek, _ := randomBytes(32)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek), SecretKEKVersion: 1}
	ctx := context.Background()

	want := map[string]string{}
	for i, sc := range EncryptedSettingsColumns {
		plain := fmt.Sprintf("settings-plaintext-%d-%s", i, sc.Label)
		tok, err := EncryptString(cfg, plain)
		if err != nil {
			t.Fatal(err)
		}
		var q string
		switch sc.Table {
		case "settings":
			q = `INSERT INTO settings(key, value, last_modified_by, last_modified_at) VALUES('obs.bearerTokenEnc', ?, 't', '2026-01-01T00:00:00Z')`
		case "gitlab_config":
			q = fmt.Sprintf(`INSERT INTO gitlab_config(id, %s, last_modified_by, last_modified_at) VALUES(1, ?, 't', '2026-01-01T00:00:00Z') ON CONFLICT(id) DO UPDATE SET %s=excluded.%s`, sc.Column, sc.Column, sc.Column)
		case "vault_config":
			q = fmt.Sprintf(`INSERT INTO vault_config(id, addr, auth_method, %s, last_modified_by, last_modified_at) VALUES(1, 'http://v', 'approle', ?, 't', '2026-01-01T00:00:00Z') ON CONFLICT(id) DO UPDATE SET %s=excluded.%s`, sc.Column, sc.Column, sc.Column)
		case "notification_config":
			q = fmt.Sprintf(`INSERT INTO notification_config(id, %s, last_modified_by, last_modified_at) VALUES(1, ?, 't', '2026-01-01T00:00:00Z') ON CONFLICT(id) DO UPDATE SET %s=excluded.%s`, sc.Column, sc.Column, sc.Column)
		case "log_storage_config":
			q = fmt.Sprintf(`INSERT INTO log_storage_config(id, backend, %s, last_modified_by, last_modified_at) VALUES(1, 'local', ?, 't', '2026-01-01T00:00:00Z') ON CONFLICT(id) DO UPDATE SET %s=excluded.%s`, sc.Column, sc.Column, sc.Column)
		default:
			t.Fatalf("no seeding rule for table %q — add one when adding a column", sc.Table)
		}
		if _, err := pool.ExecContext(ctx, q, tok); err != nil {
			t.Fatalf("seed %s: %v", sc.Label, err)
		}
		want[sc.Label] = plain
	}

	values, undecryptable, err := RedactionReport(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if undecryptable != 0 {
		t.Errorf("undecryptable = %d, want 0", undecryptable)
	}
	joined := strings.Join(values, "\n")
	for label, plain := range want {
		if !strings.Contains(joined, plain) {
			t.Errorf("%s did not reach the dictionary", label)
		}
	}

	// Lose the KEK: every one of them is now counted, none returned.
	svc := New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := svc.Create(ctx, CreateInput{Key: "K", Source: "stored", Value: "stored-secret-value"}, "t"); err != nil {
		t.Fatal(err)
	}
	values, undecryptable, err = RedactionReport(ctx, pool, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 {
		t.Errorf("without a KEK nothing can be returned, got %d values", len(values))
	}
	if wantN := len(EncryptedSettingsColumns) + 1; undecryptable != wantN {
		t.Errorf("undecryptable = %d, want %d (seven columns + one secret)", undecryptable, wantN)
	}
}

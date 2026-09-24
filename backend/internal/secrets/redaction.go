package secrets

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// SettingsColumn is one config-table column holding an EncryptString
// self-describing token. These are a DIFFERENT code path from the four-column
// envelope stores (secrets, ssh_credentials), and are what an implementation
// written only against ciphertext/nonce/wrapped_dek silently misses.
type SettingsColumn struct {
	Label  string // dotted settings path, for messages
	Table  string
	Column string
	Where  string
}

// EncryptedSettingsColumns is the complete set, shared by `amadeus
// rewrap-secrets` (which re-wraps each under the active KEK) and the redaction
// dictionary (which decrypts each so the value is masked wherever it is echoed).
// Adding an encrypted settings field means adding a row here, or rotation will
// silently skip it AND its value will print in clear.
//
// AM-4a: before the two consumers shared this table, the redaction side listed
// three of the seven — the GitLab webhook secret, the S3 log-storage key, the
// Vault role id and the observability bearer token were re-wrapped by rotation
// but never masked.
var EncryptedSettingsColumns = []SettingsColumn{
	{"gitlab.token", "gitlab_config", "pat_enc", "id=1"},
	{"gitlab.webhookSecret", "gitlab_config", "webhook_secret_enc", "id=1"},
	{"logStorage.s3SecretKey", "log_storage_config", "s3_secret_key_enc", "id=1"},
	{"vault.roleId", "vault_config", "role_id", "id=1"},
	{"vault.secretId", "vault_config", "secret_id_enc", "id=1"},
	{"notifications.smtpPassword", "notification_config", "smtp_password_enc", "id=1"},
	{"observability.bearerToken", "settings", "value", "key='obs.bearerTokenEnc'"},
}

// ReadSettingsToken returns the raw (still encrypted) token in one settings
// column, "" when the row or value is absent.
func ReadSettingsToken(ctx context.Context, database *sql.DB, sc SettingsColumn) (string, error) {
	var v sql.NullString
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s", sc.Column, sc.Table, sc.Where) //nolint:gosec // identifiers from the fixed table above
	switch err := database.QueryRowContext(ctx, q).Scan(&v); {
	case err == sql.ErrNoRows:
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read %s: %w", sc.Label, err)
	}
	if !v.Valid {
		return "", nil
	}
	return v.String, nil
}

// RedactionReport returns every plaintext the server can decrypt from its three
// encrypted stores — stored secrets, stored SSH credentials (SK.14) and the
// encrypted settings columns (PP-L3) — plus a count of the encrypted values it
// COULD NOT decrypt: rows wrapped under a KEK version that is not configured,
// undecryptable ciphertext, or every encrypted value when no KEK is configured
// at all.
//
// The count is what distinguishes "nothing to mask" from "cannot mask" (AM-Q3):
// an empty dictionary over an empty table is complete, while an empty
// dictionary over a table of rows the KEK cannot open is a masking outage. Both
// used to return nil, nil. err is reserved for the database itself.
//
// IMPORTANT: never log or persist the returned values (S7).
func RedactionReport(ctx context.Context, database *sql.DB, cfg *config.Config) (values []string, undecryptable int, err error) {
	kekCache := map[int][]byte{}
	kekFor := func(ver int) []byte {
		if k, ok := kekCache[ver]; ok {
			return k
		}
		k, kerr := loadKEKForVersion(cfg, ver)
		if kerr != nil {
			k = nil // remember the miss so we don't retry every row
		}
		kekCache[ver] = k
		return k
	}

	// The two envelope stores share one column layout. Only source='stored'
	// rows hold a value; a vault-sourced row holds a reference. A key created
	// directly as a credential has NO backing secret row, so without the second
	// table that private key would never enter the dictionary.
	for _, table := range []string{"secrets", "ssh_credentials"} {
		rows, qerr := database.QueryContext(ctx,
			`SELECT ciphertext, nonce, wrapped_dek, COALESCE(kek_version, 1) FROM `+table+
				` WHERE source='stored' AND ciphertext IS NOT NULL`)
		if qerr != nil {
			if table == "secrets" {
				return nil, 0, fmt.Errorf("query %s for redaction: %w", table, qerr)
			}
			continue // a missing ssh_credentials table (schema upgrade in progress) must not fail the secrets above
		}
		for rows.Next() {
			var ciphertext, nonce, wrappedDEK []byte
			var kekVer int
			if serr := rows.Scan(&ciphertext, &nonce, &wrappedDEK, &kekVer); serr != nil {
				undecryptable++
				continue
			}
			kek := kekFor(kekVer)
			if kek == nil {
				undecryptable++
				continue
			}
			plain, derr := envelopeDecrypt(kek, ciphertext, nonce, wrappedDEK)
			if derr != nil {
				undecryptable++
				continue
			}
			if len(plain) > 0 {
				values = append(values, string(plain))
			}
		}
		rerr := rows.Err()
		rows.Close()
		if rerr != nil {
			return nil, 0, fmt.Errorf("read %s for redaction: %w", table, rerr)
		}
	}

	// The encrypted settings columns (PP-L3): integration credentials that a
	// git clone error or a Vault client error can print. A missing table or
	// column (schema upgrade in progress) is skipped; an unreadable token counts.
	for _, sc := range EncryptedSettingsColumns {
		tok, terr := ReadSettingsToken(ctx, database, sc)
		if terr != nil || tok == "" {
			continue
		}
		if _, isToken := TokenKEKVersion(tok); !isToken {
			continue // an un-encrypted legacy value set by hand; not ours to count
		}
		plain, derr := DecryptString(cfg, tok)
		if derr != nil {
			undecryptable++
			continue
		}
		if plain != "" {
			values = append(values, plain)
		}
	}
	return values, undecryptable, nil
}

// changeHook is called after any write that changes what RedactionReport would
// return: a secret, SSH credential or encrypted settings column written,
// rotated or deleted, and env_vars (whose multi-line values are key material).
// It lives here, not in the package that consumes it, because every writer of
// those stores already imports secrets and the consumer (redactdict) imports
// secrets too — a hook in the consumer would be an import cycle.
var changeHook atomic.Pointer[func()]

// SetRedactionChangeHook installs the function RedactionSourceChanged calls.
// Nil removes it. Installed once by main; tests install and restore their own.
func SetRedactionChangeHook(f func()) {
	if f == nil {
		changeHook.Store(nil)
		return
	}
	changeHook.Store(&f)
}

// RedactionSourceChanged is called by every writer of a redaction source AFTER
// its write is committed. Calling it inside the writer's transaction is the
// documented mistake: a rebuild that reads before the commit sees the old row.
// The set of writers that must call it is pinned by
// redactdict.TestEveryRedactionSourceWriterNotifies.
func RedactionSourceChanged() {
	if p := changeHook.Load(); p != nil {
		(*p)()
	}
}

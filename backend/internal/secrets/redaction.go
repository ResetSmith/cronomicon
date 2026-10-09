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
	// Key names the column that identifies a row, for a table that holds one
	// token PER ROW (git_repos: a token and two webhook secrets per repository).
	// Empty for a singleton, whose Where selects its one row. With a Key, Where
	// selects the rows to cover and every one of them is read, masked and
	// re-wrapped. A many-row table listed WITHOUT a Key would be read with a
	// single-row query and silently cover its first row only.
	Key string
}

// SettingsToken is one stored token of a SettingsColumn: the raw (still
// encrypted) value, and the Key of the row it is in ("" for a singleton).
type SettingsToken struct {
	Key   string
	Token string
}

// EncryptedSettingsColumns is the complete set, shared by `cronomicon
// rewrap-secrets` (which re-wraps each under the active KEK) and the redaction
// dictionary (which decrypts each so the value is masked wherever it is echoed).
// Adding an encrypted settings field means adding a row here, or rotation will
// silently skip it AND its value will print in clear.
//
// AM-4a: before the two consumers shared this table, the redaction side listed
// three of the seven — the GitLab webhook secret, the S3 log-storage key, the
// Vault role id and the observability bearer token were re-wrapped by rotation
// but never masked.
//
// 2.4.0: a repository is a row of git_repos (migration 1310), one per agency
// to come, so its three entries carry a Key and cover every row. The third is
// the PREVIOUS webhook secret, which a rotation keeps (and the webhook still
// accepts) for an overlap: it was on neither side of this list until now.
var EncryptedSettingsColumns = []SettingsColumn{
	{Label: "gitlab.token", Table: "git_repos", Column: "token_enc", Where: "1=1", Key: "id"},
	{Label: "gitlab.webhookSecret", Table: "git_repos", Column: "webhook_secret_enc", Where: "1=1", Key: "id"},
	{Label: "gitlab.webhookSecretPrevious", Table: "git_repos", Column: "webhook_secret_prev_enc", Where: "1=1", Key: "id"},
	{Label: "logStorage.s3SecretKey", Table: "log_storage_config", Column: "s3_secret_key_enc", Where: "id=1"},
	{Label: "vault.roleId", Table: "vault_config", Column: "role_id", Where: "id=1"},
	{Label: "vault.secretId", Table: "vault_config", Column: "secret_id_enc", Where: "id=1"},
	{Label: "notifications.smtpPassword", Table: "notification_config", Column: "smtp_password_enc", Where: "id=1"},
	{Label: "observability.bearerToken", Table: "settings", Column: "value", Where: "key='obs.bearerTokenEnc'"},
}

// ReadSettingsTokens returns every raw (still encrypted) token stored in one
// settings column: at most one for a singleton, one per row that has a value
// for a column with a Key. An absent row, a NULL and an empty value yield
// nothing. It is the ONLY reader, for both consumers, so that neither can read
// a many-row column as if it had one row.
func ReadSettingsTokens(ctx context.Context, database *sql.DB, sc SettingsColumn) ([]SettingsToken, error) {
	key := "''"
	order := ""
	if sc.Key != "" {
		key = sc.Key
		order = " ORDER BY " + sc.Key
	}
	q := fmt.Sprintf("SELECT %s, %s FROM %s WHERE (%s) AND %s IS NOT NULL AND %s <> ''%s", //nolint:gosec // identifiers from the fixed table above
		key, sc.Column, sc.Table, sc.Where, sc.Column, sc.Column, order)
	rows, err := database.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", sc.Label, err)
	}
	defer rows.Close()
	var out []SettingsToken
	for rows.Next() {
		var t SettingsToken
		if err := rows.Scan(&t.Key, &t.Token); err != nil {
			return nil, fmt.Errorf("read %s: %w", sc.Label, err)
		}
		out = append(out, t)
		if sc.Key == "" {
			break // a singleton is one row by definition
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", sc.Label, err)
	}
	return out, nil
}

// RowLabel names one token for a message: the column's label, and the row's
// key when the column has one ("gitlab.token[global]").
func (sc SettingsColumn) RowLabel(key string) string {
	if sc.Key == "" {
		return sc.Label
	}
	return sc.Label + "[" + key + "]"
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
		toks, terr := ReadSettingsTokens(ctx, database, sc)
		if terr != nil {
			continue
		}
		for _, t := range toks {
			if _, isToken := TokenKEKVersion(t.Token); !isToken {
				continue // an un-encrypted legacy value set by hand; not ours to count
			}
			plain, derr := DecryptString(cfg, t.Token)
			if derr != nil {
				undecryptable++
				continue
			}
			if plain != "" {
				values = append(values, plain)
			}
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

package secrets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// bindSealsMarker records that every sealed value is bound (2.5.1 SG2.10).
const bindSealsMarker = "upgrade.2.5.1.bindSeals"

// ApplyBindSealsMarker reads whether BindSeals has bound every value and, if
// it has, refuses unbound seals in this process. Every process that opens
// sealed values calls it (the server through BindSeals, rewrap-secrets on its
// own): a process that skipped it would open, and re-seal bound, an unbound
// value copied into a row after the pass (2.5.1 review).
func ApplyBindSealsMarker(ctx context.Context, database *sql.DB) (bool, error) {
	var done sql.NullString
	err := database.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, bindSealsMarker).Scan(&done)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read %s: %w", bindSealsMarker, err)
	}
	if done.Valid {
		RefuseUnboundSeals(true)
		return true, nil
	}
	return false, nil
}

// BindSealsReport is what BindSeals did.
type BindSealsReport struct {
	Bound  int // values re-sealed bound to their rows
	Failed int // values it could not open (a KEK version not configured, a damaged value)
	// Complete: every value is bound and the marker is written, so unbound
	// seals are refused from now on.
	Complete bool
	// NoKEK: no KEK is configured, so nothing sealed can be opened or bound.
	NoKEK bool
}

// BindSeals re-seals every value sealed before 2.5.1 bound to where it lives
// (Binding): the stored secrets and SSH keys, and every encrypted settings
// column. Run at start, after the migrations, before anything opens a value.
//
// When it has bound everything without a failure it writes a marker, and
// from then on (this run, and every later start, which reads the marker
// first) unbound seals are refused (RefuseUnboundSeals): an old unbound value
// copied into another row does not open. A failure leaves unbound values
// readable, so nothing stops working, and the next start tries again; the
// caller logs the count. Each write is guarded on the value it read, so a
// concurrent save wins.
//
// It is what makes 2.5.1 one-way: 2.5.0 cannot open a bound value.
func BindSeals(ctx context.Context, database *sql.DB, cfg *config.Config) (BindSealsReport, error) {
	var rep BindSealsReport
	done, err := ApplyBindSealsMarker(ctx, database)
	if err != nil {
		return rep, err
	}
	if done {
		rep.Complete = true
		return rep, nil
	}
	if _, err := loadKEK(cfg); errors.Is(err, errNoKEK) {
		rep.NoKEK = true
		return rep, nil
	}
	sealer := NewSealer(cfg)

	for _, table := range []string{"secrets", "ssh_credentials"} {
		type row struct {
			id              string
			ct, nonce, wrap []byte
			ver             int
		}
		rows, err := database.QueryContext(ctx, `SELECT id, ciphertext, nonce, wrapped_dek, COALESCE(kek_version, 1) FROM `+table+ //nolint:gosec // fixed identifiers
			` WHERE source = 'stored' AND ciphertext IS NOT NULL AND seal_format <> ?`, SealBound)
		if err != nil {
			return rep, fmt.Errorf("read %s: %w", table, err)
		}
		var todo []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.ct, &r.nonce, &r.wrap, &r.ver); err != nil {
				rows.Close()
				return rep, err
			}
			todo = append(todo, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return rep, err
		}
		for _, r := range todo {
			plain, err := sealer.Open("", SealUnbound, r.ct, r.nonce, r.wrap, r.ver)
			if err != nil {
				rep.Failed++
				continue
			}
			sealed, err := sealer.Seal(RowBinding(table, r.id), plain)
			zero(plain)
			if err != nil {
				rep.Failed++
				continue
			}
			if _, err := database.ExecContext(ctx, `UPDATE `+table+ //nolint:gosec // fixed identifiers
				` SET ciphertext = ?, nonce = ?, wrapped_dek = ?, kek_version = ?, seal_format = ?
				  WHERE id = ? AND ciphertext = ? AND seal_format <> ?`,
				sealed.Ciphertext, sealed.Nonce, sealed.WrappedDEK, sealed.KEKVersion, sealed.Format, r.id, r.ct, SealBound); err != nil {
				return rep, fmt.Errorf("bind %s %s: %w", table, r.id, err)
			}
			rep.Bound++
		}
	}

	for _, sc := range EncryptedSettingsColumns {
		toks, err := ReadSettingsTokens(ctx, database, sc)
		if err != nil {
			// The migrations have run, so the table is there: this is a read that
			// failed (a lock, a cancelled start). Counted, so the marker is not
			// written over tokens left unbound, which would then never open.
			rep.Failed++
			continue
		}
		for _, t := range toks {
			if _, isToken := TokenKEKVersion(t.Token); !isToken || TokenBound(t.Token) {
				continue // plain (not ours), or bound already
			}
			plain, err := DecryptString(cfg, t.Token)
			if err != nil {
				rep.Failed++
				continue
			}
			next, err := EncryptSetting(cfg, sc.Binding(t.Key), plain)
			if err != nil {
				rep.Failed++
				continue
			}
			q := fmt.Sprintf("UPDATE %s SET %s = ? WHERE (%s) AND %s = ?", sc.Table, sc.Column, sc.Where, sc.Column) //nolint:gosec // fixed identifiers
			args := []any{next, t.Token}
			if sc.Key != "" {
				q += fmt.Sprintf(" AND %s = ?", sc.Key)
				args = append(args, t.Key)
			}
			if _, err := database.ExecContext(ctx, q, args...); err != nil {
				return rep, fmt.Errorf("bind %s: %w", sc.RowLabel(t.Key), err)
			}
			rep.Bound++
		}
	}

	if rep.Bound > 0 {
		RedactionSourceChanged()
	}
	if rep.Failed > 0 {
		return rep, nil
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO settings (key, value, last_modified_by, last_modified_at) VALUES (?, ?, 'system (2.5.1 upgrade)', ?)
		ON CONFLICT(key) DO NOTHING`, bindSealsMarker, "bound", time.Now().UTC().Format(time.RFC3339)); err != nil {
		return rep, fmt.Errorf("write %s: %w", bindSealsMarker, err)
	}
	RefuseUnboundSeals(true)
	rep.Complete = true
	return rep, nil
}

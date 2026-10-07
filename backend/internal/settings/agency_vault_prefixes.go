package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/vaultpath"
)

// ErrGlobalHasNoVaultPrefixes is returned when prefixes are set for Global. Its
// Vault-backed rows are unrestricted and a global administrator's; a prefix
// there would read as a limit and be none. Mapped 422 `builtin_agency`.
var ErrGlobalHasNoVaultPrefixes = errors.New("Global has no Vault path prefixes: its Vault-backed secrets and keys are a global administrator's, with no path limit")

// ListVaultPrefixes returns the Vault path prefixes an agency may use (LR-80),
// sorted. None means the agency can name no Vault path.
func ListVaultPrefixes(ctx context.Context, database *sql.DB, agencyID string) ([]string, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT prefix FROM agency_vault_prefixes WHERE agency_id = ? ORDER BY prefix`, agencyID)
	if err != nil {
		return nil, fmt.Errorf("list vault prefixes: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("list vault prefixes: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetVaultPrefixes replaces an agency's Vault path prefixes. Each is trimmed of
// surrounding white space (it is typed by a person) and then normalised by
// vaultpath, which refuses anything whose meaning to Vault is in doubt; a
// refusal names the entry and nothing is written. Repeats are one prefix, and a
// prefix that lies inside another of the same save is dropped: it adds nothing
// and would outlive the wider one being removed later, unnoticed.
//
// It returns the stored list. Removing a prefix does not touch the secrets and
// keys already written under it (LR-81); the notices inbox lists them.
func SetVaultPrefixes(ctx context.Context, database *sql.DB, agencyID string, prefixes []string, actor string) ([]string, error) {
	if agencyID == agencyid.Global {
		return nil, ErrGlobalHasNoVaultPrefixes
	}
	ag, err := GetAgency(ctx, database, agencyID)
	if err != nil {
		return nil, err
	}
	if ag == nil {
		return nil, ErrUnknownAgency
	}
	var norm []string
	for _, p := range prefixes {
		n, err := vaultpath.Normalize(strings.TrimSpace(p))
		if err != nil {
			return nil, err
		}
		if !slices.Contains(norm, n) {
			norm = append(norm, n)
		}
	}
	slices.Sort(norm)
	var keep []string
	for _, p := range norm {
		inside := false
		for _, q := range norm {
			if q == p {
				continue
			}
			if ok, _ := vaultpath.Under(p, q); ok {
				inside = true
				break
			}
		}
		if !inside {
			keep = append(keep, p)
		}
	}
	before, err := ListVaultPrefixes(ctx, database, agencyID)
	if err != nil {
		return nil, err
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("set vault prefixes: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM agency_vault_prefixes WHERE agency_id = ?`, agencyID); err != nil {
		return nil, fmt.Errorf("set vault prefixes: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, p := range keep {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO agency_vault_prefixes (agency_id, prefix, created_by, created_at) VALUES (?, ?, ?, ?)`,
			agencyID, p, actor, now); err != nil {
			return nil, fmt.Errorf("set vault prefixes: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("set vault prefixes: %w", err)
	}
	if !slices.Equal(before, keep) {
		// Which Vault paths an agency may reach is an access-control fact. The
		// prefixes are names of places, not secrets, and are recorded in full.
		detail := "none"
		if len(keep) > 0 {
			detail = strings.Join(keep, ", ")
		}
		audit(ctx, database, actor, "Agencies", "vault-prefixes-set", ag.Name, detail)
	}
	if keep == nil {
		keep = []string{}
	}
	return keep, nil
}

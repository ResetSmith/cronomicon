package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// A Vault-backed secret or SSH key names a path in its owner's Vault: the
// agency's own when it has connected one, the installation's otherwise (2.5.1
// SG-21). A move to an owner on another Vault reads the same path somewhere
// else from then on: FIN's own-Vault secret moved to Global read the
// installation's value at that path, Global's moved to FIN read FIN's (the
// pre-release review's A5). So such a move is refused, with the rows, unless
// the request confirms it, and a confirmed one says so in the change log
// (2.5.1 SG6.2). Checked inside the writer's transaction over the owner moves
// it made, so it holds for every route that moves an owner, the removal that
// settles a held row on its last member included.

// ErrVaultChanges is the sentinel of a *VaultChangesError (409 vault_changes).
var ErrVaultChanges = errors.New("a Vault-backed row would be read from another Vault")

// VaultChange is one row a move would take to another Vault. FromVault and
// ToVault are "installation" or "own" (the From or To agency's own Vault).
type VaultChange struct {
	Kind      string `json:"kind"` // "secret" or "ssh-credential"
	ID        string `json:"id"`
	Name      string `json:"name"`
	From      string `json:"from"`
	FromName  string `json:"fromName"`
	FromVault string `json:"fromVault"`
	To        string `json:"to"`
	ToName    string `json:"toName"`
	ToVault   string `json:"toVault"`
}

// VaultChangesError lists them.
type VaultChangesError struct{ Rows []VaultChange }

func (e *VaultChangesError) Error() string {
	return fmt.Sprintf("%v: %d row(s); confirm the move to read them there", ErrVaultChanges, len(e.Rows))
}

func (e *VaultChangesError) Unwrap() error { return ErrVaultChanges }

// MoveOptions are a membership write's confirmations.
type MoveOptions struct {
	// ConfirmVaultChange lets a move take a Vault-backed row to an owner on
	// another Vault.
	ConfirmVaultChange bool
}

func moveOptions(opts []MoveOptions) MoveOptions {
	if len(opts) == 0 {
		return MoveOptions{}
	}
	return opts[0]
}

// rowNamesVault is each store's own question: a secret whose source is not
// exactly "stored", a key whose source is exactly "vault", either with a ref.
func rowNamesVault(ctx context.Context, tx *sql.Tx, catalog, id string) (bool, error) {
	var source string
	var ref sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(source, ''), vault_ref FROM `+catalog+` WHERE id = ?`, id).Scan(&source, &ref); err != nil {
		return false, err
	}
	if ref.String != "" {
		return true, nil
	}
	if catalog == "secrets" {
		return source != "stored", nil
	}
	return source == "vault", nil
}

// vaultOf is which Vault an owner's rows resolve in: "own" or "installation".
func vaultOf(ctx context.Context, tx *sql.Tx, agency string) (string, error) {
	agency = ownerOrGlobalAgency(agency)
	if agency == agencyid.Global {
		return "installation", nil
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agency_vaults WHERE agency_id = ?`, agency).Scan(&n); err != nil {
		return "", err
	}
	if n > 0 {
		return "own", nil
	}
	return "installation", nil
}

// checkVaultMoves refuses the moves that change a Vault-backed row's Vault
// unless confirmed, and marks the confirmed ones for the change log. Two
// agencies each with its own are two Vaults.
func checkVaultMoves(ctx context.Context, tx *sql.Tx, moves []catalogMove, opt MoveOptions) error {
	var changes []VaultChange
	for i := range moves {
		m := &moves[i]
		kind := ""
		switch m.catalog {
		case "secrets":
			kind = string(MemberSecret)
		case "ssh_credentials":
			kind = string(MemberSSHCredential)
		default:
			continue
		}
		named, err := rowNamesVault(ctx, tx, m.catalog, m.mv.id)
		if err != nil {
			return err
		}
		if !named {
			continue
		}
		from, to := ownerOrGlobalAgency(m.mv.from), ownerOrGlobalAgency(m.mv.to)
		fv, err := vaultOf(ctx, tx, from)
		if err != nil {
			return err
		}
		tv, err := vaultOf(ctx, tx, to)
		if err != nil {
			return err
		}
		// A move is between two agencies, so two "own" are two Vaults.
		if fv == "installation" && tv == "installation" {
			continue
		}
		m.vaultConfirmed = true
		changes = append(changes, VaultChange{
			Kind: kind, ID: m.mv.id, Name: m.mv.name,
			From: from, FromName: agencyNameTx(ctx, tx, from), FromVault: fv,
			To: to, ToName: agencyNameTx(ctx, tx, to), ToVault: tv,
		})
	}
	if len(changes) > 0 && !opt.ConfirmVaultChange {
		return &VaultChangesError{Rows: changes}
	}
	return nil
}

func agencyNameTx(ctx context.Context, tx *sql.Tx, id string) string {
	var name string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM agencies WHERE id = ?`, id).Scan(&name); err != nil || name == "" {
		return id
	}
	return name
}

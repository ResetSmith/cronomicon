package sshkeys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"golang.org/x/crypto/ssh"
)

// ResolveSigner is the free-function form for callers that hold db+cfg rather than
// a Service (the in-app SSH executor's loadSigner, SK.5). vault must be the app's
// configured Vault client (secrets.Service.Vault()) so a vault-source credential
// resolves through the same client (P2.4); pass nil when the caller has no Vault
// wired (stored-source credentials still resolve).
func ResolveSigner(ctx context.Context, database *sql.DB, cfg *config.Config, vault secrets.VaultClient, id string) (ssh.Signer, error) {
	return resolveSigner(ctx, database, secrets.NewSealer(cfg), vault, id)
}

func resolveSigner(ctx context.Context, database *sql.DB, sealer *secrets.Sealer, vault secrets.VaultClient, id string) (ssh.Signer, error) {
	material, err := decryptCredential(ctx, database, sealer, vault, id)
	if err != nil {
		return nil, err
	}
	return parseSigner(material)
}

// IDByLabel resolves an ssh_credentials LABEL to its row id — the dispatch-time
// half of the CA rule that authored surfaces carry labels (CA-Q2) while the
// executor's signer path keys on id. found is false (nil error) when no
// credential carries that label.
func IDByLabel(ctx context.Context, database *sql.DB, label string) (id string, found bool, err error) {
	scanErr := database.QueryRowContext(ctx,
		`SELECT id FROM ssh_credentials WHERE label = ? LIMIT 1`, label).Scan(&id)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return "", false, nil
	}
	if scanErr != nil {
		return "", false, fmt.Errorf("lookup ssh credential %q: %w", label, scanErr)
	}
	return id, true, nil
}

// ResolveMaterialByName returns the decrypted private-key PEM for the SSH
// credential whose label matches name — the raw material behind an
// AMADEUS_KEY_<label> reference, for callers that must write a key FILE for remote
// key injection (runref.Resolver, P1.2/D8) rather than build an in-process signer.
// found is false (nil error) when no credential carries that label. vault must be
// the app's configured Vault client so a vault-source credential resolves (P2.4);
// pass nil when no Vault is wired.
func ResolveMaterialByName(ctx context.Context, database *sql.DB, cfg *config.Config, vault secrets.VaultClient, name string) (material string, found bool, err error) {
	var id string
	scanErr := database.QueryRowContext(ctx,
		`SELECT id FROM ssh_credentials WHERE label = ? LIMIT 1`, name).Scan(&id)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return "", false, nil
	}
	if scanErr != nil {
		return "", false, fmt.Errorf("lookup ssh credential %q: %w", name, scanErr)
	}
	m, err := decryptCredential(ctx, database, secrets.NewSealer(cfg), vault, id)
	if err != nil {
		return "", false, err
	}
	return m, true, nil
}

// ResolveMaterialByID is ResolveMaterialByName's by-id twin, for callers that have
// already chosen WHICH credential row they mean.
//
// RA-19 (Phase E): once two departments may hold the same LABEL, resolving by label
// with `LIMIT 1` silently picks one of them. The reference resolver therefore selects
// the row itself — through the one owner-aware predicate, which fails closed on an
// ambiguity instead of guessing — and calls this with the id it settled on. Resolving
// by label remains correct for the single-owner paths that have not changed.
func ResolveMaterialByID(ctx context.Context, database *sql.DB, cfg *config.Config, vault secrets.VaultClient, id string) (material string, found bool, err error) {
	var one int
	scanErr := database.QueryRowContext(ctx, `SELECT 1 FROM ssh_credentials WHERE id = ?`, id).Scan(&one)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return "", false, nil
	}
	if scanErr != nil {
		return "", false, fmt.Errorf("lookup ssh credential %q: %w", id, scanErr)
	}
	m, err := decryptCredential(ctx, database, secrets.NewSealer(cfg), vault, id)
	if err != nil {
		return "", false, err
	}
	return m, true, nil
}

// decryptCredential loads and returns a stored SSH credential's private-key PEM by
// id: a stored-source row is decrypted via the KEK sealer; a vault-source row is
// fetched live through the supplied Vault client at its vault_ref (P2.4). Shared by
// resolveSigner (→ ssh.Signer) and ResolveMaterialByName (→ raw PEM for key-file
// injection). (Distinct from backfill.go's resolveMaterial, which resolves legacy
// key material by NAME across the secrets/env_vars fallback chain.)
func decryptCredential(ctx context.Context, database *sql.DB, sealer *secrets.Sealer, vault secrets.VaultClient, id string) (string, error) {
	row := database.QueryRowContext(ctx,
		`SELECT source, ciphertext, nonce, wrapped_dek, COALESCE(kek_version, 1), vault_ref
		 FROM ssh_credentials WHERE id = ?`, id)
	var source string
	var ciphertext, nonce, wrappedDEK []byte
	var kekVer int
	var vaultRef sql.NullString
	if err := row.Scan(&source, &ciphertext, &nonce, &wrappedDEK, &kekVer, &vaultRef); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("ssh credential %q not found", id)
		}
		return "", fmt.Errorf("load ssh credential: %w", err)
	}
	if source == "vault" {
		// Fetch the private-key PEM live from Vault at the stored vault_ref. With no
		// client wired (nil or the stub) this fails closed — a vault-source key is
		// unusable rather than silently empty.
		if vault == nil {
			return "", fmt.Errorf("vault-source SSH credential %q requires a configured Vault client", id)
		}
		if !vaultRef.Valid || vaultRef.String == "" {
			return "", fmt.Errorf("vault-source SSH credential %q has no vault_ref", id)
		}
		material, err := vault.Fetch(vaultRef.String)
		if err != nil {
			return "", fmt.Errorf("fetch vault SSH credential %q: %w", id, err)
		}
		return material, nil
	}
	plain, err := sealer.Open(ciphertext, nonce, wrappedDEK, kekVer)
	if err != nil {
		return "", fmt.Errorf("decrypt ssh credential: %w", err)
	}
	return string(plain), nil
}

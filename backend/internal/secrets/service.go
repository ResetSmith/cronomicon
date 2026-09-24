package secrets

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/envref"
)

// Service implements CRUD + envelope encryption for the secrets table (S14).
// It also exposes RedactionValues for B4 log-ingest redaction (S7).
type Service struct {
	db     *sql.DB
	cfg    *config.Config
	log    *slog.Logger
	vault  VaultClient
	sealer *Sealer
}

// New creates a Service with a stub Vault client. settings.WireVaultClient
// replaces it (via WithVaultClient) with the dynamic client that resolves the
// live Vault config per operation; while Vault is unconfigured, vault-source
// rows report unavailable either way.
func New(database *sql.DB, cfg *config.Config, log *slog.Logger) *Service {
	return &Service{
		db:     database,
		cfg:    cfg,
		log:    log,
		vault:  stubVaultClient{},
		sealer: NewSealer(cfg),
	}
}

// WithVaultClient replaces the stub with a real Vault implementation.
func (s *Service) WithVaultClient(v VaultClient) { s.vault = v }

// Vault returns the configured Vault client (the stub when Vault is unconfigured).
// It lets sibling packages (sshkeys, P2.4) resolve vault-source rows through the
// SAME client this Service uses rather than each building its own — a single
// AppRole session, one hardening config, one token cache.
func (s *Service) Vault() VaultClient { return s.vault }

// VaultConfigured reports whether a USABLE Vault client is wired — the "actually
// usable" signal (L7). A client that can self-report (settings' dynamic client,
// which resolves the current config per call) is asked directly, so the answer
// tracks live config: a bad CA bundle or a half-filled config reads false even
// though a client object is wired, and configuring Vault flips it true without a
// restart. Anything else falls back to the stub type-check. Distinct from
// settings.ResolveVaultRuntime, which only reports that config is PRESENT.
func (s *Service) VaultConfigured() bool {
	if r, ok := s.vault.(interface{ Configured() bool }); ok {
		return r.Configured()
	}
	_, isStub := s.vault.(stubVaultClient)
	return !isStub
}

// Secret is the wire-safe representation (value never included).
type Secret struct {
	ID             string  `json:"id"`
	Key            string  `json:"key"`
	Reference      string  `json:"reference"` // derived AMADEUS_SECRET_<key> (read-only; namespace contract)
	Source         string  `json:"source"`
	Scope          *string `json:"scope"`
	Description    *string `json:"description"`
	VaultPath      *string `json:"vaultPath,omitempty"`
	CreatedBy      string  `json:"createdBy"`
	CreatedAt      string  `json:"createdAt"`
	LastModifiedBy string  `json:"lastModifiedBy"`
	LastModifiedAt string  `json:"lastModifiedAt"`
	// Tags are operator-authored, SQLite-only labels (migration 470). Plaintext
	// metadata in their own column — NEVER part of the AES-256-GCM envelope — so
	// they are written via PUT /api/v1/env-secret-tags/{id} without touching the
	// crypto path. Always serialized ([] when none).
	Tags []string `json:"tags"`
	// OwnerAgency is the NAME of the department that owns this row (RA-15, Phase E),
	// or "" for shared infrastructure. See settings.EnvVar.OwnerAgency.
	OwnerAgency string `json:"ownerAgency"`
}

// parseTags decodes the secrets.tags JSON-array column into a non-nil slice:
// empty / "[]" / malformed → []string{}. Mirrors api.parseTags.
func parseTags(raw string) []string {
	if raw == "" || raw == "[]" {
		return []string{}
	}
	var t []string
	if err := json.Unmarshal([]byte(raw), &t); err != nil || t == nil {
		return []string{}
	}
	return t
}

// CreateInput is the caller-supplied payload for POST /env-secrets.
type CreateInput struct {
	Key         string
	Source      string // "stored" | "vault"
	Scope       *string
	Description *string
	Value       string // required when source=stored
	VaultPath   string // required when source=vault
	// OwnerAgency is the agency ID that OWNS this row (RA-15, Phase E); "" = shared
	// infrastructure, which is every pre-Phase-E row and every row an unrestricted
	// admin creates without choosing one.
	//
	// It must be set AT INSERT, not patched on afterwards: uniqueness is
	// (key, scope, owner_agency), so a row inserted as unowned and re-owned later
	// would collide with an existing shared row of the same key in the same scope —
	// which is precisely the case Phase E exists to allow.
	OwnerAgency string
}

// UpdateInput is the caller-supplied payload for PUT /env-secrets/{id}.
type UpdateInput = CreateInput

// List returns secret metadata rows (no values) VISIBLE to an actor per the
// canRead predicate (P1.7 / D3). canRead is the caller's resolved scope decision —
// the API layer passes auth's ScopeReadable/ScopeGrant.CanRead so the "unrestricted
// vs restricted vs empty=zero" rule lives in ONE place (the A5 scope-model fix);
// secrets stays free of any auth import. A global row (scope NULL/"") is readable
// per that predicate. This stops any session from enumerating out-of-scope secret
// keys, scopes, and vault refs. A nil canRead is treated as fully unrestricted (a
// defensive default; every real caller passes a predicate).
func (s *Service) List(ctx context.Context, canRead func(scope string) bool) ([]Secret, error) {
	if canRead == nil {
		canRead = func(string) bool { return true }
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.key, s.source, s.scope, s.description, s.vault_ref, s.created_by, s.created_at,
		        s.last_modified_by, s.last_modified_at, s.tags, COALESCE(ag.name,'')
		 FROM secrets s LEFT JOIN agencies ag ON ag.id = s.owner_agency
		 ORDER BY s.created_at`)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	defer rows.Close()
	var out []Secret
	for rows.Next() {
		var sc Secret
		var vaultRef sql.NullString
		var scope, desc, tags sql.NullString
		if err := rows.Scan(&sc.ID, &sc.Key, &sc.Source, &scope, &desc, &vaultRef,
			&sc.CreatedBy, &sc.CreatedAt, &sc.LastModifiedBy, &sc.LastModifiedAt, &tags, &sc.OwnerAgency); err != nil {
			return nil, err
		}
		// Scope filter — the decision (incl. global-always-readable) is the caller's.
		if !canRead(scope.String) {
			continue
		}
		if scope.Valid {
			sc.Scope = &scope.String
		}
		if desc.Valid {
			sc.Description = &desc.String
		}
		if vaultRef.Valid && sc.Source == "vault" {
			sc.VaultPath = &vaultRef.String
		}
		sc.Tags = parseTags(tags.String)
		sc.Reference = envref.SecretReference(sc.Key)
		out = append(out, sc)
	}
	return out, rows.Err()
}

// Get returns a single secret by ID.
func (s *Service) Get(ctx context.Context, id string) (*Secret, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT s.id, s.key, s.source, s.scope, s.description, s.vault_ref, s.created_by, s.created_at,
		        s.last_modified_by, s.last_modified_at, s.tags, COALESCE(ag.name,'')
		 FROM secrets s LEFT JOIN agencies ag ON ag.id = s.owner_agency WHERE s.id = ?`, id)
	var sc Secret
	var vaultRef, scope, desc, tags sql.NullString
	if err := row.Scan(&sc.ID, &sc.Key, &sc.Source, &scope, &desc, &vaultRef,
		&sc.CreatedBy, &sc.CreatedAt, &sc.LastModifiedBy, &sc.LastModifiedAt, &tags, &sc.OwnerAgency); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get secret %s: %w", id, err)
	}
	if scope.Valid {
		sc.Scope = &scope.String
	}
	if desc.Valid {
		sc.Description = &desc.String
	}
	if vaultRef.Valid && sc.Source == "vault" {
		sc.VaultPath = &vaultRef.String
	}
	sc.Tags = parseTags(tags.String)
	sc.Reference = envref.SecretReference(sc.Key)
	return &sc, nil
}

// ErrKeyConflict is returned when a secret's (key, scope) collides with an
// existing env var of the same key+scope. A13 unifies env vars + secrets into one
// "Env Var" namespace, so a key must be unique ACROSS both tables per scope.
var ErrKeyConflict = errors.New("an env var with this key already exists for this scope")

// ErrSecretNotFound is returned by Reveal when the secret row does not exist (e.g.
// deleted between a lookup and the reveal — a TOCTOU). L3: Reveal previously
// returned ("", nil) on ErrNoRows, so the dispatch resolver injected an empty
// string instead of failing closed, and POST /env-secrets/{id}/reveal returned
// 200 {"value":""} for a just-deleted secret. Callers must fail closed on this.
var ErrSecretNotFound = errors.New("secret not found")

// envVarKeyExists reports whether an env var already uses (key, scope) — NULL and
// ” scope both mean "global". Standalone query, never a nested iterator.
// RA-16 (Phase E): scoped to the same OWNER, mirroring settings.secretKeyExists —
// see that function for why the A13 conflict narrows in step with the uniqueness key.
func (s *Service) envVarKeyExists(ctx context.Context, key string, scope *string, owner string) bool {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM env_vars
		 WHERE key = ? AND COALESCE(scope,'') = COALESCE(?,'') AND COALESCE(owner_agency,'') = ?`,
		key, scope, owner).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// Create inserts a new secret record.
func (s *Service) Create(ctx context.Context, inp CreateInput, actor string) (*Secret, error) {
	if err := envref.ValidateSecretRowName(inp.Key); err != nil {
		return nil, err
	}
	if s.envVarKeyExists(ctx, inp.Key, inp.Scope, inp.OwnerAgency) {
		return nil, ErrKeyConflict
	}
	now := time.Now().UTC().Format(time.RFC3339)
	id := db.NewID()

	var out *Secret
	var err error
	if inp.Source == "stored" {
		out, err = s.createStored(ctx, id, inp, actor, now)
	} else {
		out, err = s.createVault(ctx, id, inp, actor, now)
	}
	if err == nil {
		RedactionSourceChanged() // AM-4b: the dictionary must learn the new value
	}
	return out, err
}

func (s *Service) createStored(ctx context.Context, id string, inp CreateInput, actor, now string) (*Secret, error) {
	sealed, err := s.sealer.Seal([]byte(inp.Value))
	if err != nil {
		return nil, fmt.Errorf("encrypt secret: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO secrets (id, key, scope, description, source, ciphertext, nonce, wrapped_dek, kek_version,
		                      created_by, created_at, last_modified_by, last_modified_at, owner_agency)
		 VALUES (?, ?, ?, ?, 'stored', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, inp.Key, inp.Scope, inp.Description, sealed.Ciphertext, sealed.Nonce, sealed.WrappedDEK, sealed.KEKVersion, actor, now, actor, now, inp.OwnerAgency)
	if err != nil {
		return nil, fmt.Errorf("insert secret: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *Service) createVault(ctx context.Context, id string, inp CreateInput, actor, now string) (*Secret, error) {
	if inp.VaultPath == "" {
		return nil, fmt.Errorf("vault_path required for vault-source secrets")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO secrets (id, key, scope, description, source, vault_ref, created_by, created_at, last_modified_by, last_modified_at, owner_agency)
		 VALUES (?, ?, ?, ?, 'vault', ?, ?, ?, ?, ?, ?)`,
		id, inp.Key, inp.Scope, inp.Description, inp.VaultPath, actor, now, actor, now, inp.OwnerAgency)
	if err != nil {
		return nil, fmt.Errorf("insert vault secret: %w", err)
	}
	return s.Get(ctx, id)
}

// Update replaces the value (and optionally metadata) for an existing secret.
func (s *Service) Update(ctx context.Context, id string, inp UpdateInput, actor string) (*Secret, error) {
	if err := envref.ValidateSecretRowName(inp.Key); err != nil {
		return nil, err
	}
	// RA-16: judged against THIS ROW'S owner (read from the row — ownership is not
	// editable here), mirroring settings.UpdateEnvVar.
	var owner string
	if oerr := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(owner_agency,'') FROM secrets WHERE id = ?`, id).Scan(&owner); oerr != nil {
		owner = ""
	}
	if s.envVarKeyExists(ctx, inp.Key, inp.Scope, owner) {
		return nil, ErrKeyConflict
	}
	now := time.Now().UTC().Format(time.RFC3339)
	existing, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}

	if inp.Source == "stored" {
		sealed, err := s.sealer.Seal([]byte(inp.Value))
		if err != nil {
			return nil, fmt.Errorf("encrypt secret: %w", err)
		}
		_, err = s.db.ExecContext(ctx,
			`UPDATE secrets SET key=?, scope=?, description=?, source='stored', vault_ref=NULL,
			  ciphertext=?, nonce=?, wrapped_dek=?, kek_version=?,
			  last_modified_by=?, last_modified_at=?
			 WHERE id=?`,
			inp.Key, inp.Scope, inp.Description, sealed.Ciphertext, sealed.Nonce, sealed.WrappedDEK, sealed.KEKVersion, actor, now, id)
		if err != nil {
			return nil, fmt.Errorf("update secret: %w", err)
		}
	} else {
		if inp.VaultPath == "" {
			return nil, fmt.Errorf("vault_path required for vault-source secrets")
		}
		_, err = s.db.ExecContext(ctx,
			`UPDATE secrets SET key=?, scope=?, description=?, source='vault', vault_ref=?,
			  ciphertext=NULL, nonce=NULL, wrapped_dek=NULL, kek_version=NULL,
			  last_modified_by=?, last_modified_at=?
			 WHERE id=?`,
			inp.Key, inp.Scope, inp.Description, inp.VaultPath, actor, now, id)
		if err != nil {
			return nil, fmt.Errorf("update secret: %w", err)
		}
	}
	RedactionSourceChanged() // AM-4b
	return s.Get(ctx, id)
}

// Delete removes a secret from the DB.
func (s *Service) Delete(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE id=?`, id)
	if err != nil {
		return false, fmt.Errorf("delete secret: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		RedactionSourceChanged() // AM-4b
	}
	return n > 0, nil
}

// Reveal decrypts and returns the plaintext for a stored-source secret.
// For vault-source secrets it delegates to the VaultClient.
// The caller MUST write a change_log row after a successful reveal.
func (s *Service) Reveal(ctx context.Context, id string) (string, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT source, vault_ref, ciphertext, nonce, wrapped_dek, COALESCE(kek_version, 1) FROM secrets WHERE id=?`, id)
	var source string
	var vaultRef sql.NullString
	var ciphertext, nonce, wrappedDEK []byte
	var kekVer int
	if err := row.Scan(&source, &vaultRef, &ciphertext, &nonce, &wrappedDEK, &kekVer); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// L3: fail closed — a missing row must NOT resolve to an empty value (which
			// the dispatch resolver would inject blindly, and the reveal API would hand
			// back as 200 {"value":""}).
			return "", ErrSecretNotFound
		}
		return "", fmt.Errorf("reveal scan: %w", err)
	}
	if source == "vault" {
		// Delegate to the configured VaultClient. With the stub (no Vault wired)
		// this returns ErrVaultUnavailable, preserving prior behavior; with a real
		// client it fetches the value at the stored vault_ref (KV v2 path#field).
		if !vaultRef.Valid || vaultRef.String == "" {
			return "", fmt.Errorf("vault-source secret has no vault_ref")
		}
		return s.vault.Fetch(vaultRef.String)
	}
	plain, err := s.sealer.Open(ciphertext, nonce, wrappedDEK, kekVer)
	if err != nil {
		return "", fmt.Errorf("decrypt secret: %w", err)
	}
	return string(plain), nil
}

// MigrateToVault decrypts a stored secret and writes it to Vault at vaultPath,
// then updates the DB row to vault-source (zeroing the ciphertext).
// Returns the updated Secret, or an error if Vault is unavailable.
func (s *Service) MigrateToVault(ctx context.Context, id, vaultPath, actor string) (*Secret, error) {
	existing, err := s.Get(ctx, id)
	if err != nil || existing == nil {
		return nil, err
	}
	if existing.Source == "vault" {
		return nil, fmt.Errorf("already vault-source")
	}
	plaintext, err := s.Reveal(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("decrypt for migration: %w", err)
	}
	if err := s.vault.Write(vaultPath, plaintext); err != nil {
		return nil, fmt.Errorf("vault write: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx,
		`UPDATE secrets SET source='vault', vault_ref=?,
		  ciphertext=NULL, nonce=NULL, wrapped_dek=NULL, kek_version=NULL,
		  last_modified_by=?, last_modified_at=?
		 WHERE id=?`,
		vaultPath, actor, now, id)
	if err != nil {
		return nil, fmt.Errorf("update secret after vault migration: %w", err)
	}
	RedactionSourceChanged() // AM-4b: the value left the stored table
	return s.Get(ctx, id)
}

// RedactionValues returns the decrypted plaintext values for all stored-source
// secrets, stored SSH key credentials (ssh_credentials, SK.14), AND the
// encrypted settings columns (EncryptedSettingsColumns), to be used at log
// ingest for redaction (S7). It is RedactionReport without the count.
//
// IMPORTANT: never log or persist the returned values (S7).
func RedactionValues(ctx context.Context, database *sql.DB, cfg *config.Config) ([]string, error) {
	values, _, err := RedactionReport(ctx, database, cfg)
	return values, err
}

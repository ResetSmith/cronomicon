package sshkeys

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
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// Credential is the wire-safe representation of an ssh_credentials row — the
// private key material is NEVER included, only the derived public metadata.
type Credential struct {
	ID             string  `json:"id"`
	Label          string  `json:"label"`
	Reference      string  `json:"reference"` // derived AMADEUS_KEY_<label> (read-only; resolves to a key-file PATH)
	Description    *string `json:"description"`
	Source         string  `json:"source"` // "stored" | "vault"
	KeyType        *string `json:"keyType"`
	Fingerprint    *string `json:"fingerprint"`
	PublicKey      *string `json:"publicKey"`
	VaultRef       *string `json:"vaultRef,omitempty"`
	CreatedBy      string  `json:"createdBy"`
	CreatedAt      string  `json:"createdAt"`
	LastModifiedBy string  `json:"lastModifiedBy"`
	LastModifiedAt string  `json:"lastModifiedAt"`
	// OwnerAgency is the NAME of the department that owns this credential (RA-19),
	// or "" for a shared key. Two departments may hold the same LABEL, so this is
	// what tells them apart in a picker.
	OwnerAgency string `json:"ownerAgency"`
	// Tags are operator-authored, SQLite-only labels (migration 470). Plaintext
	// metadata stored beside label/description — NEVER the envelope columns — so
	// they are written via PUT /api/v1/ssh-credential-tags/{id} without touching
	// the key material or selectCols' deliberate envelope exclusion. Always
	// serialized ([] when none).
	Tags []string `json:"tags"`
}

// parseTags decodes the ssh_credentials.tags JSON-array column into a non-nil
// slice: empty / "[]" / malformed → []string{}. Mirrors api.parseTags.
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

// CreateInput is the caller-supplied payload. Material (the private key) is
// required for stored source; VaultRef for vault source.
type CreateInput struct {
	Label       string
	Description *string
	Source      string // "stored" | "vault"
	Material    string // required when source=stored
	VaultRef    string // required when source=vault
	// OwnerAgency is the owning agency ID (RA-19, Phase E); "" = shared. Labels have
	// no scope dimension, so per-owner uniqueness here is (label, owner_agency) —
	// which means a department may hold a key labelled the same as a GLOBAL one, and
	// RA-17's owned-beats-shared tier decides which a run gets.
	OwnerAgency string
}

// UpdateInput mirrors CreateInput. For stored source an empty Material means a
// metadata-only update (relabel/redescribe) that preserves the existing key; a
// non-empty Material rotates the key in place — the id, and therefore every
// host/bastion FK reference, is preserved.
type UpdateInput = CreateInput

// Usage lists the hosts and bastions that reference a credential by FK — the
// reverse lookup the name-string model could not do. Drives the delete guard.
type Usage struct {
	Hosts    []Ref `json:"hosts"`
	Bastions []Ref `json:"bastions"`
}

// Ref is a referencing row's id + display name.
type Ref struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ErrCredentialInUse is returned by Delete (without force) when hosts or bastions
// still reference the credential (SK-D10). The API maps it to 409.
var ErrCredentialInUse = errors.New("ssh credential is referenced by one or more hosts or bastions")

// Input-validation sentinels — the API maps these (and the parse sentinels in
// parse.go) to 422 rather than 500.
var (
	ErrLabelRequired    = errors.New("label is required")
	ErrVaultRefRequired = errors.New("vaultRef is required for vault-source credentials")
	ErrMaterialRequired = errors.New("material is required to set a stored credential")
)

// IsValidationError reports whether err is an operator-input error (→ 422) rather
// than an internal failure (→ 500). An envref.Error (bad label under the
// namespace contract) also maps to 422.
func IsValidationError(err error) bool {
	var refErr *envref.Error
	return errors.Is(err, ErrPassphraseProtected) ||
		errors.Is(err, ErrInvalidKey) ||
		errors.Is(err, ErrLabelRequired) ||
		errors.Is(err, ErrVaultRefRequired) ||
		errors.Is(err, ErrMaterialRequired) ||
		errors.As(err, &refErr)
}

// Service is CRUD + envelope-sealed key storage for the ssh_credentials table. It
// reuses the secrets envelope scheme via a Sealer (SK.2), so there is one crypto
// path and KEK rotation applies uniformly.
type Service struct {
	db     *sql.DB
	sealer *secrets.Sealer
	log    *slog.Logger
}

// New constructs a Service bound to the process KEK configuration.
func New(database *sql.DB, cfg *config.Config, log *slog.Logger) *Service {
	return &Service{db: database, sealer: secrets.NewSealer(cfg), log: log}
}

// selectCols is the metadata projection — never the envelope columns. Aliased and
// joined to agencies so the OWNER's name (RA-19) rides every read: two departments
// may hold the same LABEL now, and the by-ID pickers (SSH Targets, the Connect-as
// dropdown) are unusable if the two are indistinguishable in a list.
const selectCols = `c.id, c.label, c.description, c.source, c.key_type, c.fingerprint, c.public_key, c.vault_ref,
                    c.created_by, c.created_at, c.last_modified_by, c.last_modified_at, c.tags,
                    COALESCE(ag.name,'')`

// selectFrom joins the owner for selectCols. Kept beside it so the two cannot drift.
const selectFrom = ` FROM ssh_credentials c LEFT JOIN agencies ag ON ag.id = c.owner_agency`

// List returns all credential metadata rows (no key material).
func (s *Service) List(ctx context.Context) ([]Credential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+selectCols+selectFrom+` ORDER BY c.label`)
	if err != nil {
		return nil, fmt.Errorf("list ssh credentials: %w", err)
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// Get fetches one credential's metadata by id (nil when absent).
func (s *Service) Get(ctx context.Context, id string) (*Credential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+selectCols+selectFrom+` WHERE c.id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	return scanCredential(rows)
}

func scanCredential(rows *sql.Rows) (*Credential, error) {
	var c Credential
	var desc, keyType, fp, pub, vaultRef, tags sql.NullString
	if err := rows.Scan(&c.ID, &c.Label, &desc, &c.Source, &keyType, &fp, &pub, &vaultRef,
		&c.CreatedBy, &c.CreatedAt, &c.LastModifiedBy, &c.LastModifiedAt, &tags, &c.OwnerAgency); err != nil {
		return nil, err
	}
	c.Tags = parseTags(tags.String)
	c.Reference = envref.KeyReference(c.Label)
	if desc.Valid {
		c.Description = &desc.String
	}
	if keyType.Valid {
		c.KeyType = &keyType.String
	}
	if fp.Valid {
		c.Fingerprint = &fp.String
	}
	if pub.Valid {
		c.PublicKey = &pub.String
	}
	if vaultRef.Valid {
		c.VaultRef = &vaultRef.String
	}
	return &c, nil
}

// Create inserts a new credential. Stored source is validated on save (SK.4) and
// sealed (SK.2); vault source stores only the reference.
func (s *Service) Create(ctx context.Context, inp CreateInput, actor string) (*Credential, error) {
	if inp.Label == "" {
		return nil, ErrLabelRequired
	}
	// The label is the bare row name behind an AMADEUS_KEY_<label> reference, so it
	// must be a POSIX identifier and may not itself start with AMADEUS_ (W3).
	if err := envref.ValidateRowName(inp.Label); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	id := db.NewID()

	if inp.Source == "vault" {
		if inp.VaultRef == "" {
			return nil, ErrVaultRefRequired
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO ssh_credentials (id, label, description, source, vault_ref,
			                              created_by, created_at, last_modified_by, last_modified_at, owner_agency)
			 VALUES (?, ?, ?, 'vault', ?, ?, ?, ?, ?, ?)`,
			id, inp.Label, inp.Description, inp.VaultRef, actor, now, actor, now, inp.OwnerAgency); err != nil {
			return nil, fmt.Errorf("insert ssh credential: %w", err)
		}
		return s.Get(ctx, id)
	}

	keyType, fingerprint, publicKey, err := Parse(inp.Material)
	if err != nil {
		return nil, err
	}
	sealed, err := s.sealer.Seal([]byte(inp.Material))
	if err != nil {
		return nil, fmt.Errorf("encrypt ssh credential: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO ssh_credentials (id, label, description, source, ciphertext, nonce, wrapped_dek, kek_version,
		                              key_type, fingerprint, public_key,
		                              created_by, created_at, last_modified_by, last_modified_at, owner_agency)
		 VALUES (?, ?, ?, 'stored', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, inp.Label, inp.Description, sealed.Ciphertext, sealed.Nonce, sealed.WrappedDEK, sealed.KEKVersion,
		keyType, fingerprint, publicKey, actor, now, actor, now, inp.OwnerAgency); err != nil {
		return nil, fmt.Errorf("insert ssh credential: %w", err)
	}
	secrets.RedactionSourceChanged() // AM-4b
	return s.Get(ctx, id)
}

// Update relabels/redescribes, rotates the key material, or converts source.
func (s *Service) Update(ctx context.Context, id string, inp UpdateInput, actor string) (*Credential, error) {
	existing, err := s.Get(ctx, id)
	if err != nil || existing == nil {
		return nil, err
	}
	if inp.Label == "" {
		return nil, ErrLabelRequired
	}
	if err := envref.ValidateRowName(inp.Label); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)

	switch {
	case inp.Source == "vault":
		if inp.VaultRef == "" {
			return nil, ErrVaultRefRequired
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE ssh_credentials SET label=?, description=?, source='vault', vault_ref=?,
			  ciphertext=NULL, nonce=NULL, wrapped_dek=NULL, kek_version=NULL,
			  key_type=NULL, fingerprint=NULL, public_key=NULL,
			  last_modified_by=?, last_modified_at=? WHERE id=?`,
			inp.Label, inp.Description, inp.VaultRef, actor, now, id); err != nil {
			return nil, fmt.Errorf("update ssh credential: %w", err)
		}
	case inp.Material != "":
		// Rotate key material in place (id preserved → FK references follow).
		keyType, fingerprint, publicKey, perr := Parse(inp.Material)
		if perr != nil {
			return nil, perr
		}
		sealed, serr := s.sealer.Seal([]byte(inp.Material))
		if serr != nil {
			return nil, fmt.Errorf("encrypt ssh credential: %w", serr)
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE ssh_credentials SET label=?, description=?, source='stored', vault_ref=NULL,
			  ciphertext=?, nonce=?, wrapped_dek=?, kek_version=?,
			  key_type=?, fingerprint=?, public_key=?,
			  last_modified_by=?, last_modified_at=? WHERE id=?`,
			inp.Label, inp.Description, sealed.Ciphertext, sealed.Nonce, sealed.WrappedDEK, sealed.KEKVersion,
			keyType, fingerprint, publicKey, actor, now, id); err != nil {
			return nil, fmt.Errorf("update ssh credential: %w", err)
		}
	default:
		// Metadata-only update; preserves the existing stored key. Converting a
		// vault credential to stored requires new material, so reject that here.
		if existing.Source != "stored" {
			return nil, ErrMaterialRequired
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE ssh_credentials SET label=?, description=?, last_modified_by=?, last_modified_at=? WHERE id=?`,
			inp.Label, inp.Description, actor, now, id); err != nil {
			return nil, fmt.Errorf("update ssh credential: %w", err)
		}
	}
	secrets.RedactionSourceChanged() // AM-4b (a metadata-only update is a harmless extra rebuild)
	return s.Get(ctx, id)
}

// Usage returns the hosts and bastions that reference the credential by FK.
func (s *Service) Usage(ctx context.Context, id string) (*Usage, error) {
	u := &Usage{}
	hostRows, err := s.db.QueryContext(ctx,
		`SELECT id, hostname FROM ssh_hosts WHERE auth_credential_id = ? ORDER BY hostname`, id)
	if err != nil {
		return nil, fmt.Errorf("usage hosts: %w", err)
	}
	defer hostRows.Close()
	for hostRows.Next() {
		var r Ref
		if err := hostRows.Scan(&r.ID, &r.Name); err != nil {
			return nil, err
		}
		u.Hosts = append(u.Hosts, r)
	}
	if err := hostRows.Err(); err != nil {
		return nil, err
	}
	bRows, err := s.db.QueryContext(ctx,
		`SELECT id, name FROM bastions WHERE auth_credential_id = ? ORDER BY name`, id)
	if err != nil {
		return nil, fmt.Errorf("usage bastions: %w", err)
	}
	defer bRows.Close()
	for bRows.Next() {
		var r Ref
		if err := bRows.Scan(&r.ID, &r.Name); err != nil {
			return nil, err
		}
		u.Bastions = append(u.Bastions, r)
	}
	return u, bRows.Err()
}

// Delete removes a credential. When it is still referenced, it returns
// ErrCredentialInUse unless force is set; force orphans the referencing rows back
// to no-credential (the FK is NO ACTION, so the delete would otherwise be
// rejected) and deletes atomically.
func (s *Service) Delete(ctx context.Context, id string, force bool) (bool, error) {
	u, err := s.Usage(ctx, id)
	if err != nil {
		return false, err
	}
	inUse := len(u.Hosts) > 0 || len(u.Bastions) > 0
	if inUse && !force {
		return false, ErrCredentialInUse
	}
	if !inUse {
		res, err := s.db.ExecContext(ctx, `DELETE FROM ssh_credentials WHERE id=?`, id)
		if err != nil {
			return false, fmt.Errorf("delete ssh credential: %w", err)
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op
	if _, err := tx.ExecContext(ctx, `UPDATE ssh_hosts SET auth_credential_id=NULL WHERE auth_credential_id=?`, id); err != nil {
		return false, fmt.Errorf("clear host refs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE bastions SET auth_credential_id=NULL WHERE auth_credential_id=?`, id); err != nil {
		return false, fmt.Errorf("clear bastion refs: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM ssh_credentials WHERE id=?`, id)
	if err != nil {
		return false, fmt.Errorf("delete ssh credential: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		secrets.RedactionSourceChanged() // AM-4b — after the commit, never inside it
	}
	return n > 0, nil
}

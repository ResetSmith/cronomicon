package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// hostKeyMeta derives the non-secret pin metadata (FU-1) from a stored
// authorized-key line: whether a key is pinned, its SHA256 fingerprint for
// out-of-band comparison, and its algorithm. The raw line is never returned. An
// empty line ⇒ unpinned; a non-empty-but-unparseable line still reports pinned
// (so the UI shows "pinned" rather than silently claiming unpinned).
func hostKeyMeta(line string) (pinned bool, fingerprint, keyType string) {
	if strings.TrimSpace(line) == "" {
		return false, "", ""
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return true, "", ""
	}
	return true, ssh.FingerprintSHA256(pub), pub.Type()
}

// SshHost is the wire shape for a host record.
type SshHost struct {
	ID               string  `json:"id"`
	Source           string  `json:"source"` // git (imported from inventory, read-only) | amadeus (operator-authored)
	Hostname         string  `json:"hostname"`
	Address          *string `json:"address"`
	Port             int     `json:"port"`
	OS               *string `json:"os"`
	Via              *string `json:"via"`
	AuthKeyEnvVar    *string `json:"authKeyEnvVar"`
	AuthCredentialID *string `json:"authCredentialId"`
	User             *string `json:"user"`
	Status           string  `json:"status"`
	LastCheckedAt    *string `json:"lastCheckedAt"`
	// HostKey* expose the pinned SSH host key in a non-secret way (FU-1): whether
	// a key is pinned, its SHA256 fingerprint for out-of-band comparison, and its
	// algorithm. The raw known_hosts line is never surfaced. Derived from the
	// stored ssh_hosts.host_key (empty ⇒ unpinned; TOFU-captured on first connect).
	HostKeyPinned      bool   `json:"hostKeyPinned"`
	HostKeyFingerprint string `json:"hostKeyFingerprint,omitempty"`
	HostKeyType        string `json:"hostKeyType,omitempty"`
	CreatedBy          string `json:"createdBy"`
	CreatedAt          string `json:"createdAt"`
	LastModifiedBy     string `json:"lastModifiedBy"`
	LastModifiedAt     string `json:"lastModifiedAt"`
}

// SshHostInput is the caller-supplied payload.
type SshHostInput struct {
	Hostname         string
	Address          *string
	Port             int
	OS               *string
	Via              *string
	AuthKeyEnvVar    *string
	AuthCredentialID *string
	User             *string
}

// SshBastion is the wire shape for a bastion record.
type SshBastion struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	Address          string  `json:"address"`
	Port             int     `json:"port"`
	Username         *string `json:"username"`
	AuthKeyEnvVar    *string `json:"authKeyEnvVar"`
	AuthCredentialID *string `json:"authCredentialId"`
	Zone             *string `json:"zone"`
	Status           string  `json:"status"`
	LastCheckedAt    *string `json:"lastCheckedAt"`
	// HostKey* mirror SshHost — the non-secret pin metadata for the bastion's own
	// host key (bastions.host_key, SU-4). See SshHost for semantics.
	HostKeyPinned      bool   `json:"hostKeyPinned"`
	HostKeyFingerprint string `json:"hostKeyFingerprint,omitempty"`
	HostKeyType        string `json:"hostKeyType,omitempty"`
	CreatedBy          string `json:"createdBy"`
	CreatedAt          string `json:"createdAt"`
	LastModifiedBy     string `json:"lastModifiedBy"`
	LastModifiedAt     string `json:"lastModifiedAt"`
}

// SshBastionInput is the caller payload.
type SshBastionInput struct {
	Name             string
	Address          string
	Port             int
	Username         *string
	AuthKeyEnvVar    *string
	AuthCredentialID *string
	Zone             *string
}

// ListSshHosts returns all SSH hosts.
func ListSshHosts(ctx context.Context, database *sql.DB) ([]SshHost, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT id, source, hostname, address, port, os, via, auth_key_env_var, auth_credential_id, username,
		        created_by, created_at, last_modified_by, last_modified_at, status, last_checked_at, host_key
		 FROM ssh_hosts ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list ssh hosts: %w", err)
	}
	defer rows.Close()
	var out []SshHost
	for rows.Next() {
		h, err := scanSshHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *h)
	}
	return out, rows.Err()
}

// GetSshHost fetches by ID.
func GetSshHost(ctx context.Context, database *sql.DB, id string) (*SshHost, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT id, source, hostname, address, port, os, via, auth_key_env_var, auth_credential_id, username,
		        created_by, created_at, last_modified_by, last_modified_at, status, last_checked_at, host_key
		 FROM ssh_hosts WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	return scanSshHost(rows)
}

func scanSshHost(rows *sql.Rows) (*SshHost, error) {
	var h SshHost
	var address, os, via, authKeyEnvVar, authCredentialID, username, status, lastCheckedAt, hostKey sql.NullString
	var port sql.NullInt64
	err := rows.Scan(&h.ID, &h.Source, &h.Hostname, &address, &port, &os, &via, &authKeyEnvVar, &authCredentialID, &username,
		&h.CreatedBy, &h.CreatedAt, &h.LastModifiedBy, &h.LastModifiedAt, &status, &lastCheckedAt, &hostKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if address.Valid {
		h.Address = &address.String
	}
	if os.Valid {
		h.OS = &os.String
	}
	if via.Valid {
		h.Via = &via.String
	}
	if authKeyEnvVar.Valid {
		h.AuthKeyEnvVar = &authKeyEnvVar.String
	}
	if authCredentialID.Valid {
		h.AuthCredentialID = &authCredentialID.String
	}
	if username.Valid {
		h.User = &username.String
	}
	h.Port = 22
	if port.Valid {
		h.Port = int(port.Int64)
	}
	h.Status = "unverified"
	if status.Valid && status.String != "" {
		h.Status = status.String
	}
	if lastCheckedAt.Valid && lastCheckedAt.String != "" {
		h.LastCheckedAt = &lastCheckedAt.String
	}
	h.HostKeyPinned, h.HostKeyFingerprint, h.HostKeyType = hostKeyMeta(hostKey.String)
	return &h, nil
}

// ClearSshHostKey clears a pinned host key (FU-1) so the next connect re-captures
// it via TOFU — the supported way to re-key a legitimately rotated host without a
// raw SQL UPDATE. Status is reset to unverified so the UI reflects the re-pin
// pending. Returns whether a row was updated (false ⇒ unknown id). Audited.
func ClearSshHostKey(ctx context.Context, database *sql.DB, actor, id string) (bool, error) {
	h, _ := GetSshHost(ctx, database, id)
	res, err := database.ExecContext(ctx,
		`UPDATE ssh_hosts SET host_key=NULL, status='unverified' WHERE id=?`, id)
	if err != nil {
		return false, fmt.Errorf("clear ssh host key: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 && h != nil {
		audit(ctx, database, actor, "SSH Hosts", "host key cleared", h.Hostname, "")
	}
	return n > 0, nil
}

// ClearBastionHostKey mirrors ClearSshHostKey for a bastion's own host key.
func ClearBastionHostKey(ctx context.Context, database *sql.DB, actor, id string) (bool, error) {
	b, _ := GetBastion(ctx, database, id)
	res, err := database.ExecContext(ctx,
		`UPDATE bastions SET host_key=NULL, status='unverified' WHERE id=?`, id)
	if err != nil {
		return false, fmt.Errorf("clear bastion host key: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 && b != nil {
		audit(ctx, database, actor, "Bastions", "host key cleared", b.Name, "")
	}
	return n > 0, nil
}

// SetSshHostStatus records the outcome of a connection test (ssh-update.md TC.3).
func SetSshHostStatus(ctx context.Context, database *sql.DB, id, status, checkedAt string) error {
	_, err := database.ExecContext(ctx,
		`UPDATE ssh_hosts SET status=?, last_checked_at=? WHERE id=?`, status, checkedAt, id)
	if err != nil {
		return fmt.Errorf("set ssh host status: %w", err)
	}
	return nil
}

// CreateSshHost inserts a new SSH host.
func CreateSshHost(ctx context.Context, database *sql.DB, inp SshHostInput, actor string) (*SshHost, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	id := db.NewID()
	port := inp.Port
	if port == 0 {
		port = 22
	}
	_, err := database.ExecContext(ctx,
		`INSERT INTO ssh_hosts (id, source, hostname, address, port, os, via, auth_key_env_var, auth_credential_id, username,
		                         created_by, created_at, last_modified_by, last_modified_at)
		 VALUES (?, 'amadeus', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, inp.Hostname, inp.Address, port, inp.OS, inp.Via, inp.AuthKeyEnvVar, inp.AuthCredentialID, inp.User,
		actor, now, actor, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create ssh host: %w", err)
	}
	audit(ctx, database, actor, "SSH Hosts", "created", inp.Hostname, "")
	return GetSshHost(ctx, database, id)
}

// UpdateSshHost replaces an SSH host. git-source rows (imported from inventory)
// are read-only — they are owned by sync; an operator overlay is a separate
// amadeus row (M4 / §9.5).
func UpdateSshHost(ctx context.Context, database *sql.DB, id string, inp SshHostInput, actor string) (*SshHost, error) {
	if existing, _ := GetSshHost(ctx, database, id); existing != nil && existing.Source == "git" {
		return nil, fmt.Errorf("only amadeus-source hosts are editable (this host is imported from inventory by sync)")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	port := inp.Port
	if port == 0 {
		port = 22
	}
	// Fail-closed: the WHERE itself excludes git rows, so even if the pre-check
	// above was skipped (e.g. a transient GetSshHost error), a git row is never
	// mutated.
	res, err := database.ExecContext(ctx,
		`UPDATE ssh_hosts SET hostname=?, address=?, port=?, os=?, via=?, auth_key_env_var=?, auth_credential_id=?, username=?,
		  last_modified_by=?, last_modified_at=? WHERE id=? AND source='amadeus'`,
		inp.Hostname, inp.Address, port, inp.OS, inp.Via, inp.AuthKeyEnvVar, inp.AuthCredentialID, inp.User,
		actor, now, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update ssh host: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, nil
	}
	audit(ctx, database, actor, "SSH Hosts", "updated", inp.Hostname, "")
	return GetSshHost(ctx, database, id)
}

// DeleteSshHost removes an SSH host. git-source rows are sync-owned and not
// operator-deletable (they reappear on the next sync); only amadeus rows delete.
func DeleteSshHost(ctx context.Context, database *sql.DB, id, actor string) (bool, error) {
	h, _ := GetSshHost(ctx, database, id)
	if h != nil && h.Source == "git" {
		return false, fmt.Errorf("only amadeus-source hosts can be deleted (this host is imported from inventory by sync)")
	}
	// Fail-closed: the WHERE excludes git rows even if the pre-check was skipped.
	res, err := database.ExecContext(ctx, `DELETE FROM ssh_hosts WHERE id=? AND source='amadeus'`, id)
	if err != nil {
		return false, fmt.Errorf("delete ssh host: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 && h != nil {
		audit(ctx, database, actor, "SSH Hosts", "deleted", h.Hostname, "")
	}
	return n > 0, nil
}

// ListBastions returns all bastions.
func ListBastions(ctx context.Context, database *sql.DB) ([]SshBastion, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT id, name, address, port, username, auth_key_env_var, auth_credential_id, zone,
		        created_by, created_at, last_modified_by, last_modified_at, status, last_checked_at, host_key
		 FROM bastions ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list bastions: %w", err)
	}
	defer rows.Close()
	var out []SshBastion
	for rows.Next() {
		b, err := scanBastion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// GetBastion fetches by ID.
func GetBastion(ctx context.Context, database *sql.DB, id string) (*SshBastion, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT id, name, address, port, username, auth_key_env_var, auth_credential_id, zone,
		        created_by, created_at, last_modified_by, last_modified_at, status, last_checked_at, host_key
		 FROM bastions WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	return scanBastion(rows)
}

func scanBastion(rows *sql.Rows) (*SshBastion, error) {
	var b SshBastion
	var username, authKeyEnvVar, authCredentialID, zone, status, lastCheckedAt, hostKey sql.NullString
	var port sql.NullInt64
	err := rows.Scan(&b.ID, &b.Name, &b.Address, &port, &username, &authKeyEnvVar, &authCredentialID, &zone,
		&b.CreatedBy, &b.CreatedAt, &b.LastModifiedBy, &b.LastModifiedAt, &status, &lastCheckedAt, &hostKey)
	if err != nil {
		return nil, err
	}
	if username.Valid {
		b.Username = &username.String
	}
	if authKeyEnvVar.Valid {
		b.AuthKeyEnvVar = &authKeyEnvVar.String
	}
	if authCredentialID.Valid {
		b.AuthCredentialID = &authCredentialID.String
	}
	if zone.Valid {
		b.Zone = &zone.String
	}
	b.Port = 22
	if port.Valid {
		b.Port = int(port.Int64)
	}
	b.Status = "unverified"
	if status.Valid && status.String != "" {
		b.Status = status.String
	}
	if lastCheckedAt.Valid && lastCheckedAt.String != "" {
		b.LastCheckedAt = &lastCheckedAt.String
	}
	b.HostKeyPinned, b.HostKeyFingerprint, b.HostKeyType = hostKeyMeta(hostKey.String)
	return &b, nil
}

// SetBastionStatus records the outcome of a connection test (ssh-update.md TC.3).
func SetBastionStatus(ctx context.Context, database *sql.DB, id, status, checkedAt string) error {
	_, err := database.ExecContext(ctx,
		`UPDATE bastions SET status=?, last_checked_at=? WHERE id=?`, status, checkedAt, id)
	if err != nil {
		return fmt.Errorf("set bastion status: %w", err)
	}
	return nil
}

// CreateBastion inserts a new bastion.
func CreateBastion(ctx context.Context, database *sql.DB, inp SshBastionInput, actor string) (*SshBastion, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	id := db.NewID()
	port := inp.Port
	if port == 0 {
		port = 22
	}
	_, err := database.ExecContext(ctx,
		`INSERT INTO bastions (id, name, hostname, address, port, username, auth_key_env_var, auth_credential_id, zone,
		                       created_by, created_at, last_modified_by, last_modified_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, inp.Name, inp.Name, inp.Address, port, inp.Username, inp.AuthKeyEnvVar, inp.AuthCredentialID, inp.Zone,
		actor, now, actor, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create bastion: %w", err)
	}
	audit(ctx, database, actor, "Bastions", "created", inp.Name, "")
	return GetBastion(ctx, database, id)
}

// UpdateBastion replaces a bastion.
func UpdateBastion(ctx context.Context, database *sql.DB, id string, inp SshBastionInput, actor string) (*SshBastion, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	port := inp.Port
	if port == 0 {
		port = 22
	}
	res, err := database.ExecContext(ctx,
		`UPDATE bastions SET name=?, hostname=?, address=?, port=?, username=?, auth_key_env_var=?, auth_credential_id=?, zone=?,
		  last_modified_by=?, last_modified_at=? WHERE id=?`,
		inp.Name, inp.Name, inp.Address, port, inp.Username, inp.AuthKeyEnvVar, inp.AuthCredentialID, inp.Zone,
		actor, now, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update bastion: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, nil
	}
	audit(ctx, database, actor, "Bastions", "updated", inp.Name, "")
	return GetBastion(ctx, database, id)
}

// DeleteBastion removes a bastion.
func DeleteBastion(ctx context.Context, database *sql.DB, id, actor string) (bool, error) {
	b, _ := GetBastion(ctx, database, id)
	res, err := database.ExecContext(ctx, `DELETE FROM bastions WHERE id=?`, id)
	if err != nil {
		return false, fmt.Errorf("delete bastion: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 && b != nil {
		audit(ctx, database, actor, "Bastions", "deleted", b.Name, "")
	}
	return n > 0, nil
}

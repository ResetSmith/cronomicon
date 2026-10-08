package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// The host key shown on a host record or a bastion is the one the LOCAL RUNNER
// trusts for the address the record is dialled at: the key in force in its
// ledger (host_key_ledger), approved by an operator or carried over by the
// 2.3.0 upgrade. Until 2.3.0 it was a key stored on the record itself, captured
// on the first connection; those columns are gone (migration 1270).
//
// trustedKeySQL is that key for a record, in authorized-key form ("type
// base64"), or NULL. addrSQL and portSQL are the record's dial address and
// port; the known_hosts host is built from them the way hostkeys.Pattern does
// (the bare address on port 22, [address]:port otherwise).
func trustedKeySQL(addrSQL, portSQL string) string {
	pattern := `CASE WHEN COALESCE(NULLIF(` + portSQL + `, 0), 22) = 22 THEN ` + addrSQL +
		` ELSE '[' || ` + addrSQL + ` || ']:' || ` + portSQL + ` END`
	return `(SELECT substr(l.known_hosts_line, instr(l.known_hosts_line, ' ') + 1)
	           FROM host_key_ledger l JOIN runners rn ON rn.id = l.runner_id AND rn.kind = 'server'
	          WHERE l.host = ` + pattern + `
	            AND l.decision = 'approved' AND l.superseded_at IS NULL
	          ORDER BY l.id DESC LIMIT 1)`
}

var (
	hostTrustedKeySQL    = trustedKeySQL(`COALESCE(NULLIF(ssh_hosts.address, ''), ssh_hosts.hostname)`, `ssh_hosts.port`)
	bastionTrustedKeySQL = trustedKeySQL(`COALESCE(NULLIF(bastions.address, ''), bastions.name)`, `bastions.port`)
)

// hostKeyMeta derives the non-secret metadata (FU-1) from that key: whether
// there is one, its SHA256 fingerprint for out-of-band comparison, and its
// algorithm. The raw line is never returned. An empty line ⇒ none; a
// non-empty-but-unparseable line still reports a key (so the UI does not
// silently claim there is none).
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
	Source           string  `json:"source"` // git (imported from inventory, read-only) | cronomicon (operator-authored)
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
	// HostKey* expose, in a non-secret way (FU-1), the host key the LOCAL RUNNER
	// trusts for this record's address: whether it has one, its SHA256
	// fingerprint for out-of-band comparison, and its algorithm. It is the key
	// an operator approved for the local runner (Runners → Local runner → Host
	// keys), or one the 2.3.0 upgrade carried over; nothing is captured on
	// first connect any more. The field names predate that.
	HostKeyPinned      bool   `json:"hostKeyPinned"`
	HostKeyFingerprint string `json:"hostKeyFingerprint,omitempty"`
	HostKeyType        string `json:"hostKeyType,omitempty"`
	CreatedBy          string `json:"createdBy"`
	CreatedAt          string `json:"createdAt"`
	LastModifiedBy     string `json:"lastModifiedBy"`
	LastModifiedAt     string `json:"lastModifiedAt"`
	// ScopeID is set on a record imported for a scope and nil on one written by
	// hand. It decides who the record belongs to (LR-69): an imported record is
	// its scope's agency's; a hand-written one is its owner_agency's.
	ScopeID *string `json:"scopeId"`
	// OwnerAgency is the id of the agency the record belongs to by that rule —
	// Global for an imported record whose scope is in several agencies or none —
	// and OwnerAgencyName its name. Read-only on an imported record.
	OwnerAgency     string `json:"ownerAgency"`
	OwnerAgencyName string `json:"ownerAgencyName"`
}

// SshHostInput is the caller-supplied payload.
type SshHostInput struct {
	// OwnerAgency is the owning agency's id for a hand-written record. Required
	// on create (the API fills it in); on update, empty keeps the owner.
	OwnerAgency      string
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
	// HostKey* mirror SshHost — the key the local runner trusts for the
	// bastion's own address (SU-4). See SshHost for semantics.
	HostKeyPinned      bool   `json:"hostKeyPinned"`
	HostKeyFingerprint string `json:"hostKeyFingerprint,omitempty"`
	HostKeyType        string `json:"hostKeyType,omitempty"`
	CreatedBy          string `json:"createdBy"`
	CreatedAt          string `json:"createdAt"`
	LastModifiedBy     string `json:"lastModifiedBy"`
	LastModifiedAt     string `json:"lastModifiedAt"`
	// OwnerAgency is the id of the agency the bastion belongs to (LR-69); a host
	// routes only through a bastion of its own agency or Global's (LR-70).
	OwnerAgency     string `json:"ownerAgency"`
	OwnerAgencyName string `json:"ownerAgencyName"`
}

// SshBastionInput is the caller payload.
type SshBastionInput struct {
	// OwnerAgency is the owning agency's id. Required on create (the API fills
	// it in); on update, empty keeps the owner.
	OwnerAgency      string
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
		        created_by, created_at, last_modified_by, last_modified_at, status, last_checked_at, `+hostTrustedKeySQL+`,
		        scope_id, `+hostOwnerSQL+`, (SELECT name FROM agencies WHERE id = `+hostOwnerSQL+`)
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
		        created_by, created_at, last_modified_by, last_modified_at, status, last_checked_at, `+hostTrustedKeySQL+`,
		        scope_id, `+hostOwnerSQL+`, (SELECT name FROM agencies WHERE id = `+hostOwnerSQL+`)
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

// hostOwnerSQL is the id of the agency an ssh_hosts row belongs to (LR-69): its
// scope's ONE agency when it was imported for a scope, its own owner_agency when
// it was written by hand. A scope in several agencies (the state from before
// 2.3.0) or in none has no single answer, and its imported records read as
// Global's, which makes them a global administrator's to change.
const hostOwnerSQL = `CASE WHEN ssh_hosts.scope_id IS NOT NULL
     THEN COALESCE((SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END
                      FROM scope_agencies sa WHERE sa.scope_id = ssh_hosts.scope_id), 'global')
     ELSE ssh_hosts.owner_agency END`

func scanSshHost(rows *sql.Rows) (*SshHost, error) {
	var h SshHost
	var address, os, via, authKeyEnvVar, authCredentialID, username, status, lastCheckedAt, hostKey sql.NullString
	var scopeID, ownerName sql.NullString
	var port sql.NullInt64
	err := rows.Scan(&h.ID, &h.Source, &h.Hostname, &address, &port, &os, &via, &authKeyEnvVar, &authCredentialID, &username,
		&h.CreatedBy, &h.CreatedAt, &h.LastModifiedBy, &h.LastModifiedAt, &status, &lastCheckedAt, &hostKey,
		&scopeID, &h.OwnerAgency, &ownerName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if scopeID.Valid && scopeID.String != "" {
		h.ScopeID = &scopeID.String
	}
	h.OwnerAgencyName = ownerName.String
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
	owner := inp.OwnerAgency
	if owner == "" {
		owner = agencyid.Global
	}
	_, err := database.ExecContext(ctx,
		`INSERT INTO ssh_hosts (id, source, hostname, address, port, os, via, auth_key_env_var, auth_credential_id, username,
		                         created_by, created_at, last_modified_by, last_modified_at, owner_agency)
		 VALUES (?, 'cronomicon', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, inp.Hostname, inp.Address, port, inp.OS, inp.Via, inp.AuthKeyEnvVar, inp.AuthCredentialID, inp.User,
		actor, now, actor, now, owner,
	)
	if err != nil {
		return nil, fmt.Errorf("create ssh host: %w", err)
	}
	audit(ctx, database, actor, "SSH Hosts", "created", inp.Hostname, "")
	return GetSshHost(ctx, database, id)
}

// UpdateSshHost replaces an SSH host. git-source rows (imported from inventory)
// are read-only — they are owned by sync; an operator overlay is a separate
// cronomicon row (M4 / §9.5).
func UpdateSshHost(ctx context.Context, database *sql.DB, id string, inp SshHostInput, actor string) (*SshHost, error) {
	if existing, _ := GetSshHost(ctx, database, id); existing != nil && existing.Source == "git" {
		return nil, fmt.Errorf("only cronomicon-source hosts are editable (this host is imported from inventory by sync)")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	port := inp.Port
	if port == 0 {
		port = 22
	}
	// Fail-closed: the WHERE itself excludes git rows, so even if the pre-check
	// above was skipped (e.g. a transient GetSshHost error), a git row is never
	// mutated.
	// The owner moves only when one is named, and only on a hand-written record:
	// an imported record is its scope's (the CASE keeps what is stored).
	res, err := database.ExecContext(ctx,
		`UPDATE ssh_hosts SET hostname=?, address=?, port=?, os=?, via=?, auth_key_env_var=?, auth_credential_id=?, username=?,
		  last_modified_by=?, last_modified_at=?,
		  owner_agency = CASE WHEN ? <> '' AND scope_id IS NULL THEN ? ELSE owner_agency END
		  WHERE id=? AND source='cronomicon'`,
		inp.Hostname, inp.Address, port, inp.OS, inp.Via, inp.AuthKeyEnvVar, inp.AuthCredentialID, inp.User,
		actor, now, inp.OwnerAgency, inp.OwnerAgency, id,
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
// operator-deletable (they reappear on the next sync); only cronomicon rows delete.
func DeleteSshHost(ctx context.Context, database *sql.DB, id, actor string) (bool, error) {
	h, _ := GetSshHost(ctx, database, id)
	if h != nil && h.Source == "git" {
		return false, fmt.Errorf("only cronomicon-source hosts can be deleted (this host is imported from inventory by sync)")
	}
	// Fail-closed: the WHERE excludes git rows even if the pre-check was skipped.
	res, err := database.ExecContext(ctx, `DELETE FROM ssh_hosts WHERE id=? AND source='cronomicon'`, id)
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
		        created_by, created_at, last_modified_by, last_modified_at, status, last_checked_at, `+bastionTrustedKeySQL+`,
		        owner_agency, (SELECT name FROM agencies WHERE id = bastions.owner_agency)
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
		        created_by, created_at, last_modified_by, last_modified_at, status, last_checked_at, `+bastionTrustedKeySQL+`,
		        owner_agency, (SELECT name FROM agencies WHERE id = bastions.owner_agency)
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
	var username, authKeyEnvVar, authCredentialID, zone, status, lastCheckedAt, hostKey, ownerName sql.NullString
	var port sql.NullInt64
	err := rows.Scan(&b.ID, &b.Name, &b.Address, &port, &username, &authKeyEnvVar, &authCredentialID, &zone,
		&b.CreatedBy, &b.CreatedAt, &b.LastModifiedBy, &b.LastModifiedAt, &status, &lastCheckedAt, &hostKey,
		&b.OwnerAgency, &ownerName)
	if err != nil {
		return nil, err
	}
	b.OwnerAgencyName = ownerName.String
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
	owner := inp.OwnerAgency
	if owner == "" {
		owner = agencyid.Global
	}
	_, err := database.ExecContext(ctx,
		`INSERT INTO bastions (id, name, hostname, address, port, username, auth_key_env_var, auth_credential_id, zone,
		                       created_by, created_at, last_modified_by, last_modified_at, owner_agency)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, inp.Name, inp.Name, inp.Address, port, inp.Username, inp.AuthKeyEnvVar, inp.AuthCredentialID, inp.Zone,
		actor, now, actor, now, owner,
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
		  last_modified_by=?, last_modified_at=?,
		  owner_agency = CASE WHEN ? <> '' THEN ? ELSE owner_agency END
		  WHERE id=?`,
		inp.Name, inp.Name, inp.Address, port, inp.Username, inp.AuthKeyEnvVar, inp.AuthCredentialID, inp.Zone,
		actor, now, inp.OwnerAgency, inp.OwnerAgency, id,
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

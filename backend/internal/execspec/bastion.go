package execspec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// BastionByRef resolves a host record's `via` to the bastion record the SERVER
// would hop through, as a dial target (no nested via).
//
// A host's `via` stores the bastion's name, so id, name and hostname all match,
// and the `address` column is preferred for the dial (hostname can be a display
// name). The bastion's own key reference comes back with it, so the hop is
// authenticated with the BASTION's key rather than the target's (PP-H4).
//
// LR-70: a host routes only through a bastion that the host record's own agency
// owns, or Global's. `owners` is the record's (Target.Owners); a bastion of any
// other agency does not exist as far as this lookup is concerned, so a record
// cannot be pointed through another agency's jump host by naming it. Of two
// bastions that answer to one reference, the agency's own is taken before
// Global's.
//
// It lives here, not with the engine that dials, because two things must agree
// with the engine about WHICH machine a `via` names: the scan that captures the
// bastion's host key for the local runner, and the coverage that says whether
// the local runner trusts it. An agent resolves `via` itself, as an address.
func BastionByRef(ctx context.Context, q rowQueryer, ref string, owners []string) (*Target, error) {
	ownersJSON, err := json.Marshal(append([]string{}, owners...))
	if err != nil {
		return nil, err
	}
	row := q.QueryRowContext(ctx, `
		SELECT id, hostname, address, port, username, auth_key_env_var, auth_credential_id, owner_agency FROM bastions
		WHERE (id = ? OR name = ? OR hostname = ?)
		  AND (owner_agency = ? OR owner_agency IN (SELECT value FROM json_each(?)))
		ORDER BY (owner_agency = ?) ASC, id LIMIT 1`, ref, ref, ref, agencyid.Global, string(ownersJSON), agencyid.Global)
	var id, hostname, owner string
	var address, username, authKey, authCredID sql.NullString
	var port sql.NullInt64
	if err := row.Scan(&id, &hostname, &address, &port, &username, &authKey, &authCredID, &owner); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %q", ErrNoBastion, ref)
		}
		return nil, err
	}
	dialHost := address.String
	if dialHost == "" {
		dialHost = hostname
	}
	// The Target type is the bastion's carrier too: DialAddr() applies the
	// address→name and port→22 fallbacks, and its address is what the hop's
	// host key is verified under.
	return &Target{
		ID:               id,
		Name:             hostname,
		Address:          dialHost,
		Port:             int(port.Int64),
		User:             username.String,
		AuthKeyEnvVar:    authKey.String,
		AuthCredentialID: authCredID.String,
		Owners:           []string{owner},
	}, nil
}

// ErrNoBastion is BastionByRef finding no bastion the host record's agency may
// route through.
var ErrNoBastion = errors.New("bastion not found among this host's agency's bastions or Global's")

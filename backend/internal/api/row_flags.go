package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
)

// Per-row permission flags (2.4.0 Phase R8; the owner's answer to question
// 49: "gating reads server-computed flags only"). The lists of scopes,
// secrets, variables, SSH keys, host records, bastions and agencies carry, on
// each row, what the caller may do with it — worked out by the SAME check the
// row's write route makes, so a screen never offers what the server refuses
// and never hides what it allows:
//
//   - canEdit: the row's PUT/PATCH would pass its agency gate.
//   - canDelete: its DELETE would. Every one of these routes has the same gate
//     as its edit today; the two are kept apart so that they may differ.
//   - canRepair: a change of its agency would pass (I4, IN-7): a global
//     administrator's, the same on every row.
//
// A row whose agency cannot be told (in no agency: damage) is no one's to edit
// and a global administrator's to repair, as its routes answer.
type rowFlags struct {
	CanEdit   bool `json:"canEdit"`
	CanDelete bool `json:"canDelete"`
	CanRepair bool `json:"canRepair"`
}

// withRowFlags writes each row with its flags beside its own fields.
func withRowFlags[T any](rows []T, flagsOf func(T) (rowFlags, error)) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		fl, err := flagsOf(row)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		extra, _ := json.Marshal(fl)
		raw = bytes.TrimSpace(raw)
		if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
			return nil, fmt.Errorf("withRowFlags: a row is not an object")
		}
		merged := append([]byte{}, raw[:len(raw)-1]...)
		if len(raw) > 2 {
			merged = append(merged, ',')
		}
		merged = append(merged, extra[1:]...)
		out = append(out, merged)
	}
	return out, nil
}

// flagsByMembership is the flags of a row whose write gate is its agency
// membership (requireEntityAgency): a scope, a secret, a variable, a key.
func (s *Server) flagsByMembership(ctx context.Context, id auth.Identity, perm, joinTable, joinCol, rowID string) (rowFlags, error) {
	ok, _, err := s.entityAgencyPermitted(ctx, id, perm, joinTable, joinCol, rowID)
	if errors.Is(err, errInNoAgency) {
		ok, err = false, nil
	}
	if err != nil {
		return rowFlags{}, err
	}
	return rowFlags{CanEdit: ok, CanDelete: ok, CanRepair: id.GlobalAdmin(auth.PermConfigureApp)}, nil
}

// flagsWithScope narrows a secret's or a variable's membership flags by the
// scope check its write makes too (loadSecretWritable, loadEnvVarWritable).
func (s *Server) flagsWithScope(ctx context.Context, id auth.Identity, fl rowFlags, table, rowID string, scope *string) (rowFlags, error) {
	if !fl.CanEdit {
		return fl, nil
	}
	owner, err := s.rowOwnerAgency(ctx, table, rowID)
	if err != nil {
		return rowFlags{}, err
	}
	sc := ""
	if scope != nil {
		sc = *scope
	}
	if !auth.ScopeWritableIn(id, owner, sc) {
		fl.CanEdit, fl.CanDelete = false, false
	}
	return fl, nil
}

// hostRecordFlags is requireHostOwner as flags: an imported record is its
// scope's to change, one written by hand its owner agency's.
func (s *Server) hostRecordFlags(ctx context.Context, id auth.Identity, hostID string) (rowFlags, error) {
	var scopeID sql.NullString
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT scope_id, owner_agency FROM ssh_hosts WHERE id = ?`, hostID).Scan(&scopeID, &owner)
	if err != nil {
		return rowFlags{}, err
	}
	if scopeID.Valid && scopeID.String != "" {
		return s.flagsByMembership(ctx, id, auth.PermConfigureApp, "scope_agencies", "scope_id", scopeID.String)
	}
	ok := id.CanAgency(auth.PermConfigureApp, owner)
	return rowFlags{CanEdit: ok, CanDelete: ok, CanRepair: id.GlobalAdmin(auth.PermConfigureApp)}, nil
}

// bastionFlags is requireBastionOwner as flags.
func (s *Server) bastionFlags(ctx context.Context, id auth.Identity, bastionID string) (rowFlags, error) {
	var owner string
	if err := s.db.QueryRowContext(ctx, `SELECT owner_agency FROM bastions WHERE id = ?`, bastionID).Scan(&owner); err != nil {
		return rowFlags{}, err
	}
	ok := id.CanAgency(auth.PermConfigureApp, owner)
	return rowFlags{CanEdit: ok, CanDelete: ok, CanRepair: id.GlobalAdmin(auth.PermConfigureApp)}, nil
}

// agencyFlags is the agency catalog's gate (requireGlobal): a global
// administrator edits and deletes an agency; Global, the built-in agency, is
// neither renamed nor deleted (422 builtin_agency).
func agencyFlags(id auth.Identity, agencyID string) rowFlags {
	g := id.GlobalAdmin(auth.PermConfigureApp)
	mutable := g && agencyID != agencyid.Global
	return rowFlags{CanEdit: mutable, CanDelete: mutable, CanRepair: g}
}

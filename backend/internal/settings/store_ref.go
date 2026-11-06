package settings

import (
	"context"
	"database/sql"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/secretstore"
)

// StoreRefVerdict is whether an agency may name a reference (2.5.5 SM-10):
// Code is "" when it may, else the API code of the refusal and its Reason.
type StoreRefVerdict struct {
	OK     bool   `json:"ok"`
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// JudgeStoreRef is the one statement of whether a named agency (not the
// built-in one, which the callers handle) may name ref in its store, for the
// write check and for the form's own check before it saves. current is the
// reference the row names now ("" for a new row): left as it was, it is not
// judged again where only the grammar would judge it (rows stored before
// 2.5.5 in an agency with its own store and no prefixes). label is the noun a
// sentence uses ("secret", "SSH key").
func JudgeStoreRef(ctx context.Context, database *sql.DB, agency, ref, current, label string) (StoreRefVerdict, error) {
	store, err := StoreProviderOf(ctx, database, agency)
	if err != nil {
		return StoreRefVerdict{}, err
	}
	prefixes, err := ListVaultPrefixes(ctx, database, agency)
	if err != nil {
		return StoreRefVerdict{}, err
	}
	// 2.5.1 SG-26: in an agency's own store every reference is its own, and
	// its prefixes, if it set any, limit only itself.
	own, err := AgencyHasOwnVault(ctx, database, agency)
	if err != nil {
		return StoreRefVerdict{}, err
	}
	if own && len(prefixes) == 0 {
		if current != "" && ref == current {
			return StoreRefVerdict{OK: true}, nil
		}
		if err := store.ValidateRef(ref); err != nil {
			return StoreRefVerdict{Code: "validation_failed", Reason: "not a " + store.Label() + " reference: " + err.Error()}, nil
		}
		return StoreRefVerdict{OK: true}, nil
	}
	if len(prefixes) == 0 {
		return StoreRefVerdict{Code: "vault_path_not_allowed", Reason: "this " + label + "'s agency has no Vault paths assigned in the installation's Vault, so it cannot name one: " +
			"a global administrator assigns an agency its Vault path prefixes, or the agency connects a secret store of its own"}, nil
	}
	ok, err := secretstore.Allowed(store, ref, prefixes)
	if err != nil {
		return StoreRefVerdict{Code: "validation_failed", Reason: err.Error()}, nil
	}
	if !ok {
		return StoreRefVerdict{Code: "vault_path_not_allowed", Reason: "that reference is outside the paths assigned to this " + label + "'s agency (" + strings.Join(prefixes, ", ") + ")"}, nil
	}
	return StoreRefVerdict{OK: true}, nil
}

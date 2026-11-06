package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/secretstore"
	_ "github.com/ResetSmith/cronomicon/internal/secretstore/all"
)

// StoreProviderOf is the provider an agency's external references are judged
// and read by (2.5.5 SM-1, SM-9): its own connection's, else the
// installation's store's (HashiCorp/OpenBao, SM-11). The built-in agency and
// an empty owner are the installation's. A provider this build does not have
// is an error: the caller refuses rather than judge with another's grammar.
func StoreProviderOf(ctx context.Context, q rowQuerier, agency string) (secretstore.Provider, error) {
	agency = ownerOrGlobalAgency(agency)
	if agency == agencyid.Global {
		return secretstore.Lookup(secretstore.Default)
	}
	var id string
	err := q.QueryRowContext(ctx, `SELECT provider FROM agency_vaults WHERE agency_id = ?`, agency).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return secretstore.Lookup(secretstore.Default)
	case err != nil:
		return nil, fmt.Errorf("read the agency's secret store: %w", err)
	}
	return secretstore.Lookup(id)
}

// ErrProviderMismatch refuses moving an external secret or key to an agency
// whose store is another provider (2.5.5 SM-9): its reference is written in
// its own provider's grammar and names nothing in the other. 422
// provider_mismatch.
var ErrProviderMismatch = errors.New("the row's reference is for another kind of secret store")

// ProviderMismatchError names the row and both providers.
type ProviderMismatchError struct {
	Kind, ID, Name     string
	FromName, ToName   string
	FromLabel, ToLabel string
}

func (e *ProviderMismatchError) Error() string {
	return fmt.Sprintf("%v: %s %s reads %s's %s; %s's secret store is %s. Change the row to a stored value, or "+
		"give it a reference in %s's store, before moving it", ErrProviderMismatch, e.Kind, e.Name, e.FromName, e.FromLabel, e.ToName, e.ToLabel, e.ToName)
}

func (e *ProviderMismatchError) Unwrap() error { return ErrProviderMismatch }

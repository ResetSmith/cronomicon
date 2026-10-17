package runner

import (
	"context"
	"database/sql"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// rowQuerier is a *sql.DB or a *sql.Tx.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// runnerAgency is the agency of the runner id, by the no-owner rule: the
// agency an activity row about the runner is of (2.5.0 C1, question 74).
// Global's when the runner is not there.
func runnerAgency(ctx context.Context, q rowQuerier, runnerID string) string {
	return ownerOf(ctx, q, `SELECT `+agencyid.ColumnOwnerSQL("", "owner_agency")+` FROM runners WHERE id = ?`, runnerID)
}

// tokenAgency is the agency a registration token enrols for.
func tokenAgency(ctx context.Context, q rowQuerier, rowID int64) string {
	return ownerOf(ctx, q, `SELECT `+agencyid.ColumnOwnerSQL("", "agency_id")+` FROM registration_tokens WHERE id = ?`, rowID)
}

func ownerOf(ctx context.Context, q rowQuerier, query string, arg any) string {
	var a string
	if err := q.QueryRowContext(ctx, query, arg).Scan(&a); err != nil || a == "" {
		return agencyid.Global
	}
	return a
}

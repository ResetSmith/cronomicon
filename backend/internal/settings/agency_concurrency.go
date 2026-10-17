package settings

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// SetAgencyConcurrency sets an agency's own concurrency cap (2.5.0 C6, QT-1),
// or removes it with nil. A cap is a positive number of runs; Global may have
// one like any agency, its runs being runs. It returns the agency as stored.
//
// Lowering a cap below what is running stops nothing: the runs finish, and
// the agency's next run waits for a slot.
func SetAgencyConcurrency(ctx context.Context, database *sql.DB, agencyID string, maxConcurrent *int, actor string) (*Agency, error) {
	if maxConcurrent != nil && *maxConcurrent < 1 {
		return nil, fmt.Errorf("%w: maxConcurrent must be a positive number of runs, or null for none", ErrValidation)
	}
	ag, err := GetAgency(ctx, database, agencyID)
	if err != nil {
		return nil, err
	}
	if ag == nil {
		return nil, ErrUnknownAgency
	}
	var value any
	if maxConcurrent != nil {
		value = *maxConcurrent
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := database.ExecContext(ctx,
		`UPDATE agencies SET max_concurrent = ?, last_modified_by = ?, last_modified_at = ? WHERE id = ?`,
		value, actor, now, agencyID); err != nil {
		return nil, fmt.Errorf("set agency concurrency: %w", err)
	}
	if !sameCap(ag.MaxConcurrent, maxConcurrent) {
		detail := "none"
		if maxConcurrent != nil {
			detail = strconv.Itoa(*maxConcurrent)
		}
		// On the agency's row (C1): its people see what was set for them.
		audit(ctx, database, agencyID, actor, "Agencies", "concurrency-set", ag.Name, detail)
	}
	return GetAgency(ctx, database, agencyID)
}

func sameCap(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

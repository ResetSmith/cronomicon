package settings

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// The first start's working agency (2.5.3 CA-16, question 213). Central runs
// nothing, so an installation with no ordinary agency gets one, Operations,
// made once: a solo administrator or a small team renames it and works there,
// and a second agency later is an ordinary one, so nothing ever moves. The same
// rule holds at an upgrade: an installation that held only Central gets
// Operations as the place for what Central held; one with an agency gets none.

// FirstAgencyName is the name the first start gives its agency.
const FirstAgencyName = "Operations"

// firstAgencyKey marks that the step ran, whether or not it made an agency, so
// an Operations deleted later is never made again.
const firstAgencyKey = "first_agency_step"

// firstAgencyActor is who the agency's creation is recorded as.
const firstAgencyActor = "system (first start)"

// EnsureFirstAgency makes Operations when no agency but Central exists and the
// step has not run before, and records that it ran. It answers the new
// agency's id, or "" when it made none, with why for the log.
func EnsureFirstAgency(ctx context.Context, database *sql.DB) (id, why string, err error) {
	var done int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settings WHERE key = ?`, firstAgencyKey).Scan(&done); err != nil {
		return "", "", fmt.Errorf("first agency: %w", err)
	}
	if done > 0 {
		return "", "it ran before", nil
	}
	var ordinary, named int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agencies WHERE id <> ?`, agencyid.Global).Scan(&ordinary); err != nil {
		return "", "", fmt.Errorf("first agency: %w", err)
	}
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agencies WHERE name = ?`, FirstAgencyName).Scan(&named); err != nil {
		return "", "", fmt.Errorf("first agency: %w", err)
	}
	switch {
	case ordinary > 0:
		why = "an agency exists"
	case named > 0:
		// Central itself is called Operations: the name is taken, and the
		// first-agency card asks for one instead.
		why = "the built-in agency is named " + FirstAgencyName
	default:
		a, err := CreateAgency(ctx, database, AgencyInput{Name: FirstAgencyName}, firstAgencyActor)
		if err != nil {
			return "", "", fmt.Errorf("first agency: %w", err)
		}
		id = a.ID
	}
	// Recorded after the agency, not before: a start stopped in between finds
	// the agency on the next and records the step then, so it never makes two.
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := database.ExecContext(ctx, `
		INSERT INTO settings (key, value, last_modified_by, last_modified_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO NOTHING`,
		firstAgencyKey, now, firstAgencyActor, now); err != nil {
		return id, why, fmt.Errorf("first agency: record the step: %w", err)
	}
	return id, why, nil
}

package settings

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// maxAgencyContact matches an annotation's contact (api/annotations_mount.go):
// the same kind of text, an on-call rotation or a mailing list.
const maxAgencyContact = 256

// SetAgencyContact sets who the central team calls about an agency's runs when
// the job or workflow names no contact of its own (2.5.2 OP-28). Trimmed; empty
// clears it. Who may is the caller's to judge (the agency's administrators or a
// global administrator); this records the change on the agency's row of the
// change log, so its people see what was set for them.
func SetAgencyContact(ctx context.Context, database *sql.DB, agencyID, contact, actor string) (*Agency, error) {
	contact = strings.TrimSpace(contact)
	if utf8.RuneCountInString(contact) > maxAgencyContact {
		return nil, fmt.Errorf("%w: contact is at most %d characters", ErrValidation, maxAgencyContact)
	}
	ag, err := GetAgency(ctx, database, agencyID)
	if err != nil {
		return nil, err
	}
	if ag == nil {
		return nil, ErrUnknownAgency
	}
	if ag.Contact == contact {
		return ag, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := database.ExecContext(ctx,
		`UPDATE agencies SET contact = ?, last_modified_by = ?, last_modified_at = ? WHERE id = ?`,
		contact, actor, now, agencyID); err != nil {
		return nil, fmt.Errorf("set agency contact: %w", err)
	}
	detail := contact
	if detail == "" {
		detail = "none"
	}
	audit(ctx, database, agencyID, actor, "Agencies", "contact-set", ag.Name, detail)
	return GetAgency(ctx, database, agencyID)
}

// AgencyContact is one agency's contact; empty when it has none, when the
// agency is unknown, and when the read fails (display-only, as the annotation
// reads are: it never costs the page that shows it).
func AgencyContact(ctx context.Context, database *sql.DB, agencyID string) string {
	var contact string
	_ = database.QueryRowContext(ctx, `SELECT contact FROM agencies WHERE id = ?`, agencyID).Scan(&contact)
	return contact
}

// AgencyContacts is every agency's contact by id, those with one only: one read
// for a page that shows several agencies' runs (the overview).
func AgencyContacts(ctx context.Context, database *sql.DB) (map[string]string, error) {
	rows, err := database.QueryContext(ctx, `SELECT id, contact FROM agencies WHERE contact <> ''`)
	if err != nil {
		return nil, fmt.Errorf("agency contacts: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, contact string
		if err := rows.Scan(&id, &contact); err != nil {
			return nil, fmt.Errorf("agency contacts: %w", err)
		}
		out[id] = contact
	}
	return out, rows.Err()
}

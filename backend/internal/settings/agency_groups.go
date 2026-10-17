package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// ErrGlobalHasNoGroups is returned when a group list is set for Global. No
// grant names Global (LR-9): a global administrator is an all-agencies grant,
// so a list there would limit nothing. Mapped 422 `builtin_agency`.
var ErrGlobalHasNoGroups = errors.New("Global has no group list: no grant names Global, and a global administrator's grant names every agency")

// ListAgencyGroups returns the directory groups an agency's grants may name
// (2.5.0 C5, GA-1), sorted. None means its delegates can grant to no group.
func ListAgencyGroups(ctx context.Context, database *sql.DB, agencyID string) ([]string, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT ad_group FROM agency_groups WHERE agency_id = ? ORDER BY ad_group`, agencyID)
	if err != nil {
		return nil, fmt.Errorf("list agency groups: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, fmt.Errorf("list agency groups: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// AgencyGroupListed reports whether group is on the agency's list. The match
// is exact (question 85), as login matches a grant's group: a list that
// accepted "sg-tax" for "SG-Tax" would pass a grant login never matches.
func AgencyGroupListed(ctx context.Context, database *sql.DB, agencyID, group string) (bool, error) {
	var n int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agency_groups WHERE agency_id = ? AND ad_group = ?`, agencyID, group).Scan(&n); err != nil {
		return false, fmt.Errorf("read agency groups: %w", err)
	}
	return n > 0, nil
}

// SetAgencyGroups replaces an agency's group list. Each group is trimmed of
// surrounding white space (it is typed by a person; a grant's group is
// trimmed the same way) and otherwise kept as typed; an empty one is dropped,
// and repeats are one group.
//
// It returns the stored list. Taking a group off the list does not touch the
// grants that name it (GA-2): they keep working and the notices inbox lists
// them.
func SetAgencyGroups(ctx context.Context, database *sql.DB, agencyID string, groups []string, actor string) ([]string, error) {
	if agencyID == agencyid.Global {
		return nil, ErrGlobalHasNoGroups
	}
	ag, err := GetAgency(ctx, database, agencyID)
	if err != nil {
		return nil, err
	}
	if ag == nil {
		return nil, ErrUnknownAgency
	}
	keep := []string{}
	for _, g := range groups {
		g = strings.TrimSpace(g)
		if g != "" && !slices.Contains(keep, g) {
			keep = append(keep, g)
		}
	}
	slices.Sort(keep)
	before, err := ListAgencyGroups(ctx, database, agencyID)
	if err != nil {
		return nil, err
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("set agency groups: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM agency_groups WHERE agency_id = ?`, agencyID); err != nil {
		return nil, fmt.Errorf("set agency groups: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, g := range keep {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO agency_groups (agency_id, ad_group, created_by, created_at) VALUES (?, ?, ?, ?)`,
			agencyID, g, actor, now); err != nil {
			return nil, fmt.Errorf("set agency groups: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("set agency groups: %w", err)
	}
	if !slices.Equal(before, keep) {
		// Which groups an agency may grant to is an access-control fact; the
		// names are recorded in full, on that agency's row (C1).
		detail := "none"
		if len(keep) > 0 {
			detail = strings.Join(keep, ", ")
		}
		audit(ctx, database, agencyID, actor, "Agencies", "groups-set", ag.Name, detail)
	}
	return keep, nil
}

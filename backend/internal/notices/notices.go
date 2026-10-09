// Package notices is the installation's one inbox for standing conditions an
// administrator has to act on (LR-85): what an upgrade could not decide by
// itself, what a boot check found, what a sync left over.
//
// A notice is a CONDITION, not an event. It is keyed by (kind, subject): the
// check that finds the condition writes the row, writing it again changes
// nothing but its last-seen time, and the same check resolves it when the cause
// is gone. Dismissing is a person saying "seen, and left as it is"; it hides
// the notice and records who, and it does not survive the condition going away
// and coming back.
//
// Every notice belongs to an agency, Global for one about the installation, and
// is read and dismissed by whoever administers that agency. The table has no
// foreign key to agencies: a notice about an agency must be able to outlive it.
package notices

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// The kinds this release writes. A kind is a stable identifier: it is stored,
// returned by the API and matched by the frontend.
const (
	// KindAgencyRenamed — migration 1220 found an agency called Global and had
	// to rename it. Subject: the agency's id. Written only by the migration.
	KindAgencyRenamed = "agency_renamed"
	// KindSharedOwnership — a secret, variable or SSH key that Global owns and
	// only some agencies may use: what several agencies shared before 2.3.0,
	// and what the ownership backfill held back. Subject: "<kind>:<id>".
	KindSharedOwnership = "shared_ownership"
	// KindOrphaned — a scope, runner, secret, variable or SSH key that belongs
	// to no agency at all, which no route but a global administrator's
	// re-homing will touch. Subject: "<kind>:<id>".
	KindOrphaned = "orphaned"
	// KindScopeSeveralAgencies — a scope that is in more than one agency, which
	// nothing has been able to create since 2.3.0 (LR-7). It works as it did;
	// its agency is settled by setting it. Subject: the scope's id.
	KindScopeSeveralAgencies = "scope_several_agencies"
	// KindTargetHostOutsideScope — a job whose fixed target_host is not one of
	// its scope's hosts (LR-71). Its runs fail for that host. Subject: the job's
	// uid. Filed under the scope's agency.
	KindTargetHostOutsideScope = "target_host_outside_scope"
	// KindRecordKeyOutsideOwner — a hand-written host record or a bastion that
	// names an SSH key its owner may not use (LR-72): one that predates 2.3.0,
	// when such a record belonged to nobody. A run that reaches it fails for
	// that host. Subject: "ssh-host:<id>" or "bastion:<id>".
	KindRecordKeyOutsideOwner = "record_key_outside_owner"
	// KindVaultPathOutsidePrefix — a Vault-backed secret or SSH key that an
	// agency owns and whose path is not inside the Vault paths assigned to that
	// agency (LR-81). It keeps resolving; it cannot be edited until the agency
	// is assigned a prefix that covers it. Subject: "<kind>:<id>".
	KindVaultPathOutsidePrefix = "vault_path_outside_prefix"
	// KindLegacyPlacement — an agent whose serve list is not exactly its owner
	// (MA-9, MA-28): one that served several agencies before 2.3.0, or what is
	// left of one after narrowing. It works as it did; it is Global's, can be
	// narrowed and never widened. Subject: the runner's id. Filed under Global.
	KindLegacyPlacement = "legacy_placement"
	// KindGitSyncProblems — a repository whose last sync reported errors about
	// its files, or could not fetch it (2.4.0, GR-30). The rows are in
	// git_sync_problems, warnings among them; the notice counts both and
	// quotes the first few errors. Warnings alone open none.
	// Written and resolved by the sync itself, in its transaction, not by a
	// check. Subject: the repository's id. Filed under the repository's agency.
	KindGitSyncProblems = "git_sync_problems"
)

// Notice is one row of the inbox.
type Notice struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	AgencyID    string  `json:"agencyId"`
	Subject     string  `json:"subject"`
	Detail      string  `json:"detail"`
	FirstSeenAt string  `json:"firstSeenAt"`
	LastSeenAt  string  `json:"lastSeenAt"`
	ResolvedAt  *string `json:"resolvedAt,omitempty"`
	DismissedAt *string `json:"dismissedAt,omitempty"`
	DismissedBy *string `json:"dismissedBy,omitempty"`
}

// Finding is a condition a check found to hold right now.
type Finding struct {
	AgencyID string
	Subject  string
	Detail   string
}

// execer is *sql.DB or *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// Upsert records that a condition holds. First sight inserts the row. A later
// sight refreshes the detail, the agency and last_seen_at and leaves first
// sight and a dismissal alone — unless the notice had been RESOLVED, in which
// case the condition has come back: it is open again, and whoever dismissed the
// earlier occurrence did not dismiss this one.
func Upsert(ctx context.Context, x execer, kind string, f Finding) error {
	if kind == "" || f.Subject == "" || f.AgencyID == "" {
		return errors.New("notices: kind, subject and agency are required")
	}
	ts := now()
	_, err := x.ExecContext(ctx, `
		INSERT INTO notices (id, kind, agency_id, subject, detail, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (kind, subject) DO UPDATE SET
		    agency_id    = excluded.agency_id,
		    detail       = excluded.detail,
		    last_seen_at = excluded.last_seen_at,
		    dismissed_at = CASE WHEN notices.resolved_at IS NOT NULL THEN NULL ELSE notices.dismissed_at END,
		    dismissed_by = CASE WHEN notices.resolved_at IS NOT NULL THEN NULL ELSE notices.dismissed_by END,
		    first_seen_at = CASE WHEN notices.resolved_at IS NOT NULL THEN excluded.first_seen_at ELSE notices.first_seen_at END,
		    resolved_at  = NULL`,
		db.NewID(), kind, f.AgencyID, f.Subject, f.Detail, ts, ts)
	if err != nil {
		return fmt.Errorf("notices: upsert %s %s: %w", kind, f.Subject, err)
	}
	return nil
}

// Resolve marks one condition as gone. Unknown or already resolved is not an
// error.
func Resolve(ctx context.Context, x execer, kind, subject string) error {
	_, err := x.ExecContext(ctx,
		`UPDATE notices SET resolved_at = ? WHERE kind = ? AND subject = ? AND resolved_at IS NULL`,
		now(), kind, subject)
	if err != nil {
		return fmt.Errorf("notices: resolve %s %s: %w", kind, subject, err)
	}
	return nil
}

// Reconcile makes the open notices of one kind equal to what a check found:
// every finding is upserted, and every open notice of the kind that the check
// did not find is resolved. It is how a check that can enumerate its condition
// keeps the inbox true in both directions with one call. One transaction, so a
// reader never sees the kind half-rebuilt.
func Reconcile(ctx context.Context, database *sql.DB, kind string, current []Finding) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("notices: reconcile %s: %w", kind, err)
	}
	defer func() { _ = tx.Rollback() }()
	seen := make(map[string]bool, len(current))
	for _, f := range current {
		if err := Upsert(ctx, tx, kind, f); err != nil {
			return err
		}
		seen[f.Subject] = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT subject FROM notices WHERE kind = ? AND resolved_at IS NULL`, kind)
	if err != nil {
		return fmt.Errorf("notices: reconcile %s: %w", kind, err)
	}
	var gone []string
	for rows.Next() {
		var subject string
		if err := rows.Scan(&subject); err != nil {
			rows.Close()
			return fmt.Errorf("notices: reconcile %s: %w", kind, err)
		}
		if !seen[subject] {
			gone = append(gone, subject)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("notices: reconcile %s: %w", kind, err)
	}
	rows.Close()
	for _, subject := range gone {
		if err := Resolve(ctx, tx, kind, subject); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("notices: reconcile %s: %w", kind, err)
	}
	return nil
}

// ListOpen returns every notice that still needs someone: not resolved and not
// dismissed. Unfiltered: who may see which is the caller's to decide, by the
// notice's agency. Oldest first within a kind, so a list does not reshuffle as
// checks re-run.
func ListOpen(ctx context.Context, database *sql.DB) ([]Notice, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT id, kind, agency_id, subject, detail, first_seen_at, last_seen_at
		  FROM notices
		 WHERE resolved_at IS NULL AND dismissed_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("notices: list: %w", err)
	}
	defer rows.Close()
	out := []Notice{}
	for rows.Next() {
		var n Notice
		if err := rows.Scan(&n.ID, &n.Kind, &n.AgencyID, &n.Subject, &n.Detail, &n.FirstSeenAt, &n.LastSeenAt); err != nil {
			return nil, fmt.Errorf("notices: list: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notices: list: %w", err)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].FirstSeenAt != out[j].FirstSeenAt {
			return out[i].FirstSeenAt < out[j].FirstSeenAt
		}
		return out[i].Subject < out[j].Subject
	})
	return out, nil
}

// Get returns one notice by id, or nil.
func Get(ctx context.Context, database *sql.DB, id string) (*Notice, error) {
	var n Notice
	err := database.QueryRowContext(ctx, `
		SELECT id, kind, agency_id, subject, detail, first_seen_at, last_seen_at, resolved_at, dismissed_at, dismissed_by
		  FROM notices WHERE id = ?`, id).
		Scan(&n.ID, &n.Kind, &n.AgencyID, &n.Subject, &n.Detail, &n.FirstSeenAt, &n.LastSeenAt,
			&n.ResolvedAt, &n.DismissedAt, &n.DismissedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("notices: get %s: %w", id, err)
	}
	return &n, nil
}

// Dismiss hides an open notice and records who did. It reports whether a row
// changed: an id that is unknown, resolved or already dismissed is not an error
// (two administrators may be looking at one list).
func Dismiss(ctx context.Context, database *sql.DB, id, by string) (bool, error) {
	res, err := database.ExecContext(ctx, `
		UPDATE notices SET dismissed_at = ?, dismissed_by = ?
		 WHERE id = ? AND dismissed_at IS NULL AND resolved_at IS NULL`, now(), by, id)
	if err != nil {
		return false, fmt.Errorf("notices: dismiss %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

package settings

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// AlertTarget is a push target (2.5.0 C4, AL-2; migration 1540): an Apprise
// service URL one agency's rules send to. A rule names its targets
// (AlertRule.TargetIDs), from its own agency's only (question 79; IN-6).
//
// URL is often the credential itself (a Slack webhook, a PagerDuty key): the
// API returns it only to a caller holding configureApp on the target's agency
// (question 81, as GC-14 for Global's); URLSet says one is stored.
type AlertTarget struct {
	ID             string `json:"id"`
	OwnerAgency    string `json:"ownerAgency"`
	Label          string `json:"label"`
	Service        string `json:"service"`
	URL            string `json:"url,omitempty"`
	URLSet         bool   `json:"urlSet"`
	Enabled        bool   `json:"enabled"`
	CreatedBy      string `json:"createdBy"`
	CreatedAt      string `json:"createdAt"`
	LastModifiedBy string `json:"lastModifiedBy"`
	LastModifiedAt string `json:"lastModifiedAt"`
}

// AlertTargetInput is the caller-supplied payload. OwnerAgency is read by
// CreateAlertTarget only: a target keeps its agency.
type AlertTargetInput struct {
	Label       string
	Service     string
	URL         string
	Enabled     bool
	OwnerAgency string
}

const alertTargetCols = `id, owner_agency, label, service, url, enabled,
	COALESCE(created_by, ''), created_at, COALESCE(last_modified_by, ''), COALESCE(last_modified_at, '')`

func scanAlertTarget(sc interface{ Scan(...any) error }) (*AlertTarget, error) {
	var t AlertTarget
	var enabled int
	if err := sc.Scan(&t.ID, &t.OwnerAgency, &t.Label, &t.Service, &t.URL, &enabled,
		&t.CreatedBy, &t.CreatedAt, &t.LastModifiedBy, &t.LastModifiedAt); err != nil {
		return nil, err
	}
	t.Enabled = enabled == 1
	t.URLSet = t.URL != ""
	return &t, nil
}

// ListAlertTargets returns every push target, each agency's in its order.
func ListAlertTargets(ctx context.Context, database *sql.DB) ([]AlertTarget, error) {
	rows, err := database.QueryContext(ctx, `SELECT `+alertTargetCols+` FROM alert_targets ORDER BY owner_agency, position, id`)
	if err != nil {
		return nil, fmt.Errorf("list push targets: %w", err)
	}
	defer rows.Close()
	out := []AlertTarget{}
	for rows.Next() {
		t, err := scanAlertTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// GetAlertTarget fetches one push target; nil when there is none.
func GetAlertTarget(ctx context.Context, database *sql.DB, id string) (*AlertTarget, error) {
	t, err := scanAlertTarget(database.QueryRowContext(ctx, `SELECT `+alertTargetCols+` FROM alert_targets WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}

// CreateAlertTarget adds a push target at the end of its agency's list.
func CreateAlertTarget(ctx context.Context, database *sql.DB, inp AlertTargetInput, actor string) (*AlertTarget, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	id := db.NewID()
	owner := ownerOrGlobalAgency(inp.OwnerAgency)
	if _, err := database.ExecContext(ctx, `INSERT INTO alert_targets
		(id, owner_agency, label, service, url, enabled, position, created_by, created_at, last_modified_by, last_modified_at)
		VALUES (?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(position) + 1, 0) FROM alert_targets WHERE owner_agency = ?), ?, ?, ?, ?)`,
		id, owner, inp.Label, inp.Service, inp.URL, boolInt(inp.Enabled), owner, actor, now, actor, now); err != nil {
		return nil, fmt.Errorf("create push target: %w", err)
	}
	audit(ctx, database, owner, actor, "Alerts", "created", alertTargetName(inp.Label, id), "")
	return GetAlertTarget(ctx, database, id)
}

// UpdateAlertTarget replaces a push target's label, service, URL and switch;
// its agency stays. Nil when there is none.
func UpdateAlertTarget(ctx context.Context, database *sql.DB, id string, inp AlertTargetInput, actor string) (*AlertTarget, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := database.ExecContext(ctx, `UPDATE alert_targets SET label=?, service=?, url=?, enabled=?,
		last_modified_by=?, last_modified_at=? WHERE id=?`,
		inp.Label, inp.Service, inp.URL, boolInt(inp.Enabled), actor, now, id)
	if err != nil {
		return nil, fmt.Errorf("update push target: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}
	audit(ctx, database, RowAgency(ctx, database, "alert_targets", "owner_agency", id), actor, "Alerts", "updated", alertTargetName(inp.Label, id), "")
	return GetAlertTarget(ctx, database, id)
}

// DeleteAlertTarget removes a push target; the rules that named it stop
// naming it (alert_rule_targets cascades).
func DeleteAlertTarget(ctx context.Context, database *sql.DB, id, actor string) (bool, error) {
	t, _ := GetAlertTarget(ctx, database, id)
	res, err := database.ExecContext(ctx, `DELETE FROM alert_targets WHERE id=?`, id)
	if err != nil {
		return false, fmt.Errorf("delete push target: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 && t != nil {
		audit(ctx, database, ownerOrGlobalAgency(t.OwnerAgency), actor, "Alerts", "deleted", alertTargetName(t.Label, id), "")
	}
	return n > 0, nil
}

func alertTargetName(label, id string) string {
	if label != "" {
		return "push target " + label
	}
	return "push target " + id
}

// AlertTargetsOutsideAgency is the ids, of those a rule names, that are not
// push targets of the rule's agency (question 79: an agency's rule names its
// own agency's targets; a Global rule, Global's).
func AlertTargetsOutsideAgency(ctx context.Context, database *sql.DB, ids []string, agency string) ([]string, error) {
	var out []string
	for _, id := range ids {
		var n int
		if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_targets WHERE id = ? AND owner_agency = ?`,
			id, ownerOrGlobalAgency(agency)).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			out = append(out, id)
		}
	}
	return out, nil
}

// setRuleTargets makes the rule name these targets and no others.
func setRuleTargets(ctx context.Context, database *sql.DB, ruleID string, ids []string) error {
	if _, err := database.ExecContext(ctx, `DELETE FROM alert_rule_targets WHERE rule_id = ?`, ruleID); err != nil {
		return fmt.Errorf("set rule targets: %w", err)
	}
	for _, t := range ids {
		if _, err := database.ExecContext(ctx, `INSERT OR IGNORE INTO alert_rule_targets (rule_id, target_id) VALUES (?, ?)`, ruleID, t); err != nil {
			return fmt.Errorf("set rule targets: %w", err)
		}
	}
	return nil
}

// ruleTargetIDs is the targets each rule names, by rule id (all rules when
// ruleID is empty).
func ruleTargetIDs(ctx context.Context, database *sql.DB, ruleID string) (map[string][]string, error) {
	q := `SELECT rt.rule_id, rt.target_id FROM alert_rule_targets rt JOIN alert_targets t ON t.id = rt.target_id`
	var args []any
	if ruleID != "" {
		q += ` WHERE rt.rule_id = ?`
		args = append(args, ruleID)
	}
	rows, err := database.QueryContext(ctx, q+` ORDER BY t.position, t.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("read rule targets: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var r, t string
		if err := rows.Scan(&r, &t); err != nil {
			return nil, err
		}
		out[r] = append(out[r], t)
	}
	return out, rows.Err()
}

// cleanIDs trims, drops blanks and repeats.
func cleanIDs(ids []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

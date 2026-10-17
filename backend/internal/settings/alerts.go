package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// alertJobUIDSubquery resolves a rule's target name to a job uid at write time
// (R2-4).
//
// Resolved server-side rather than accepted from the caller so that every
// existing client — which sends only a job name — produces an
// identity-targeted rule with no change. alert_config has no source column, so
// a name owned in BOTH catalogs resolves to NULL and the rule goes on matching
// by name: the R2-1 activity rule, for the same reason. Silently picking a
// catalog would point the rule at the wrong department's job.
//
// An agency's rule names a job of its own agency (2.5.0 C4, AL-1): the name is
// resolved there (?2 the rule's agency), as names are per agency since I2. A
// Global rule matches every agency's runs, and resolves the name across them
// as before.
var alertJobUIDSubquery = `(SELECT j.uid FROM jobs j WHERE j.name = ?1
	AND (?2 = '` + agencyid.Global + `' OR ` + agencyid.JobOwnerSQL("j") + ` = ?2)
	AND (SELECT COUNT(*) FROM jobs j2 WHERE j2.name = j.name
	       AND (?2 = '` + agencyid.Global + `' OR ` + agencyid.JobOwnerSQL("j2") + ` = ?2)) = 1)`

// AlertRule is the wire shape (S1/S4 audit fields included).
type AlertRule struct {
	ID         string  `json:"id"`
	TargetMode string  `json:"targetMode"`
	JobName    *string `json:"jobName"`
	// JobUID is the targeted job's permanent identity (R2-4), resolved from
	// JobName at write time — callers keep naming the job and get identity
	// targeting for free. Read-only, and null when the name matched no job or
	// matched more than one, in which case the rule keeps matching by name.
	JobUID     *string  `json:"jobUid"`
	Trigger    string   `json:"trigger"`
	Channels   []string `json:"channels"`
	Recipients *string  `json:"recipients"`
	Owner      *string  `json:"owner"`
	// OwnerAgency is whose the rule is (2.5.0 C4, AL-1): it matches only runs
	// of that agency; Global's match every agency's. Set at create, from the
	// agency the request works in; not changed by an edit.
	OwnerAgency    string `json:"ownerAgency"`
	Enabled        bool   `json:"enabled"`
	CreatedBy      string `json:"createdBy"`
	CreatedAt      string `json:"createdAt"`
	LastModifiedBy string `json:"lastModifiedBy"`
	LastModifiedAt string `json:"lastModifiedAt"`
}

// AlertRuleInput is the caller-supplied payload.
type AlertRuleInput struct {
	TargetMode string
	JobName    *string
	Trigger    string
	Channels   []string
	Recipients *string
	Owner      *string
	Enabled    bool
	// OwnerAgency is read by CreateAlertRule only (Global's when empty).
	OwnerAgency string
}

// ListAlertRules returns all alert rules.
func ListAlertRules(ctx context.Context, database *sql.DB) ([]AlertRule, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT id, target_mode, job_name, trigger, channels, recipients, owner, enabled,
		        created_by, created_at, last_modified_by, last_modified_at, job_uid, owner_agency
		 FROM alert_config ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list alert rules: %w", err)
	}
	defer rows.Close()
	var out []AlertRule
	for rows.Next() {
		ar, err := scanAlertRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ar)
	}
	return out, rows.Err()
}

// GetAlertRule fetches a single rule by ID.
func GetAlertRule(ctx context.Context, database *sql.DB, id string) (*AlertRule, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT id, target_mode, job_name, trigger, channels, recipients, owner, enabled,
		        created_by, created_at, last_modified_by, last_modified_at, job_uid, owner_agency
		 FROM alert_config WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	return scanAlertRule(rows)
}

func scanAlertRule(rows *sql.Rows) (*AlertRule, error) {
	var ar AlertRule
	var jobName, recipients, owner, jobUID sql.NullString
	var enabled int
	var channelsJSON string
	err := rows.Scan(
		&ar.ID, &ar.TargetMode, &jobName, &ar.Trigger, &channelsJSON, &recipients, &owner, &enabled,
		&ar.CreatedBy, &ar.CreatedAt, &ar.LastModifiedBy, &ar.LastModifiedAt, &jobUID, &ar.OwnerAgency,
	)
	if err != nil {
		return nil, err
	}
	if jobName.Valid {
		ar.JobName = &jobName.String
	}
	if jobUID.Valid {
		ar.JobUID = &jobUID.String
	}
	if recipients.Valid {
		ar.Recipients = &recipients.String
	}
	if owner.Valid {
		ar.Owner = &owner.String
	}
	ar.Enabled = enabled == 1
	_ = json.Unmarshal([]byte(channelsJSON), &ar.Channels)
	if ar.Channels == nil {
		ar.Channels = []string{}
	}
	return &ar, nil
}

// CreateAlertRule inserts a new alert rule.
func CreateAlertRule(ctx context.Context, database *sql.DB, inp AlertRuleInput, actor string) (*AlertRule, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	id := db.NewID()
	channelsJSON, _ := json.Marshal(nullSlice(inp.Channels))
	owner := ownerOrGlobalAgency(inp.OwnerAgency)
	uid, err := alertJobUID(ctx, database, inp.JobName, owner)
	if err != nil {
		return nil, fmt.Errorf("create alert rule: %w", err)
	}
	_, err = database.ExecContext(ctx,
		`INSERT INTO alert_config
		 (id, target_mode, job_name, trigger, channels, recipients, owner, enabled,
		  created_by, created_at, last_modified_by, last_modified_at, job_uid, owner_agency)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		inp.TargetMode, inp.JobName, inp.Trigger,
		string(channelsJSON), inp.Recipients, inp.Owner, boolInt(inp.Enabled),
		actor, now, actor, now, uid, owner,
	)
	if err != nil {
		return nil, fmt.Errorf("create alert rule: %w", err)
	}
	audit(ctx, database, owner, actor, "Alerts", "created", alertName(inp), "")
	return GetAlertRule(ctx, database, id)
}

// UpdateAlertRule replaces an alert rule.
func UpdateAlertRule(ctx context.Context, database *sql.DB, id string, inp AlertRuleInput, actor string) (*AlertRule, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	channelsJSON, _ := json.Marshal(nullSlice(inp.Channels))
	// The rule keeps its agency; its job is resolved in it.
	owner := RowAgency(ctx, database, "alert_config", "owner_agency", id)
	uid, err := alertJobUID(ctx, database, inp.JobName, owner)
	if err != nil {
		return nil, fmt.Errorf("update alert rule: %w", err)
	}
	res, err := database.ExecContext(ctx,
		`UPDATE alert_config SET
		  target_mode=?, job_name=?, trigger=?, channels=?, recipients=?, owner=?, enabled=?,
		  last_modified_by=?, last_modified_at=?, job_uid=?
		 WHERE id=?`,
		inp.TargetMode, inp.JobName, inp.Trigger, string(channelsJSON), inp.Recipients,
		inp.Owner, boolInt(inp.Enabled),
		actor, now, uid, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update alert rule: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, nil
	}
	audit(ctx, database, owner, actor, "Alerts", "updated", alertName(inp), "")
	return GetAlertRule(ctx, database, id)
}

// DeleteAlertRule removes an alert rule.
func DeleteAlertRule(ctx context.Context, database *sql.DB, id, actor string) (bool, error) {
	ar, _ := GetAlertRule(ctx, database, id)
	res, err := database.ExecContext(ctx, `DELETE FROM alert_config WHERE id=?`, id)
	if err != nil {
		return false, fmt.Errorf("delete alert rule: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 && ar != nil {
		audit(ctx, database, ownerOrGlobalAgency(ar.OwnerAgency), actor, "Alerts", "deleted", alertName(AlertRuleInput{TargetMode: ar.TargetMode, Trigger: ar.Trigger}), "")
	}
	return n > 0, nil
}

// helpers

// alertJobUID resolves a rule's job name to its uid in the rule's agency
// (alertJobUIDSubquery); nil for no name, and for a name it does not resolve
// to one job.
func alertJobUID(ctx context.Context, database *sql.DB, jobName *string, owner string) (any, error) {
	if jobName == nil || *jobName == "" {
		return nil, nil
	}
	var uid sql.NullString
	err := database.QueryRowContext(ctx, `SELECT `+alertJobUIDSubquery, *jobName, owner).Scan(&uid)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if !uid.Valid {
		return nil, nil
	}
	return uid.String, nil
}

// AlertJobInAgency reports whether an agency has a job of the name, for a
// rule that names one (2.5.0 C4: an agency's rule names its own agency's job).
func AlertJobInAgency(ctx context.Context, database *sql.DB, name, agency string) (bool, error) {
	var n int
	err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs j WHERE j.name = ? AND `+agencyid.JobOwnerSQL("j")+` = ?`, name, agency).Scan(&n)
	return n > 0, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func alertName(inp AlertRuleInput) string {
	if inp.JobName != nil && *inp.JobName != "" {
		return *inp.JobName + " " + inp.Trigger
	}
	return inp.TargetMode + " " + inp.Trigger
}

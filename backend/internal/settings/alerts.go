package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

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
const alertJobUIDSubquery = `(SELECT j.uid FROM jobs j WHERE j.name = ?
	AND (SELECT COUNT(*) FROM jobs j2 WHERE j2.name = j.name) = 1)`

// AlertRule is the wire shape (S1/S4 audit fields included).
type AlertRule struct {
	ID         string  `json:"id"`
	TargetMode string  `json:"targetMode"`
	JobName    *string `json:"jobName"`
	// JobUID is the targeted job's permanent identity (R2-4), resolved from
	// JobName at write time — callers keep naming the job and get identity
	// targeting for free. Read-only, and null when the name matched no job or
	// matched more than one, in which case the rule keeps matching by name.
	JobUID         *string  `json:"jobUid"`
	Trigger        string   `json:"trigger"`
	Channels       []string `json:"channels"`
	Recipients     *string  `json:"recipients"`
	Owner          *string  `json:"owner"`
	Enabled        bool     `json:"enabled"`
	CreatedBy      string   `json:"createdBy"`
	CreatedAt      string   `json:"createdAt"`
	LastModifiedBy string   `json:"lastModifiedBy"`
	LastModifiedAt string   `json:"lastModifiedAt"`
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
}

// ListAlertRules returns all alert rules.
func ListAlertRules(ctx context.Context, database *sql.DB) ([]AlertRule, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT id, target_mode, job_name, trigger, channels, recipients, owner, enabled,
		        created_by, created_at, last_modified_by, last_modified_at, job_uid
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
		        created_by, created_at, last_modified_by, last_modified_at, job_uid
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
		&ar.CreatedBy, &ar.CreatedAt, &ar.LastModifiedBy, &ar.LastModifiedAt, &jobUID,
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
	_, err := database.ExecContext(ctx,
		`INSERT INTO alert_config
		 (id, target_mode, job_name, trigger, channels, recipients, owner, enabled,
		  created_by, created_at, last_modified_by, last_modified_at, job_uid)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+alertJobUIDSubquery+`)`,
		id,
		inp.TargetMode, inp.JobName, inp.Trigger,
		string(channelsJSON), inp.Recipients, inp.Owner, boolInt(inp.Enabled),
		actor, now, actor, now, inp.JobName,
	)
	if err != nil {
		return nil, fmt.Errorf("create alert rule: %w", err)
	}
	audit(ctx, database, actor, "Alerts", "created", alertName(inp), "")
	return GetAlertRule(ctx, database, id)
}

// UpdateAlertRule replaces an alert rule.
func UpdateAlertRule(ctx context.Context, database *sql.DB, id string, inp AlertRuleInput, actor string) (*AlertRule, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	channelsJSON, _ := json.Marshal(nullSlice(inp.Channels))
	res, err := database.ExecContext(ctx,
		`UPDATE alert_config SET
		  target_mode=?, job_name=?, trigger=?, channels=?, recipients=?, owner=?, enabled=?,
		  last_modified_by=?, last_modified_at=?,
		  job_uid=`+alertJobUIDSubquery+`
		 WHERE id=?`,
		inp.TargetMode, inp.JobName, inp.Trigger, string(channelsJSON), inp.Recipients,
		inp.Owner, boolInt(inp.Enabled),
		actor, now, inp.JobName, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update alert rule: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, nil
	}
	audit(ctx, database, actor, "Alerts", "updated", alertName(inp), "")
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
		audit(ctx, database, actor, "Alerts", "deleted", alertName(AlertRuleInput{TargetMode: ar.TargetMode, Trigger: ar.Trigger}), "")
	}
	return n > 0, nil
}

// helpers

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

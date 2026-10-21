package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/secrets"

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

func scanAlertTarget(cfg *config.Config, sc interface{ Scan(...any) error }) (*AlertTarget, error) {
	var t AlertTarget
	var enabled int
	if err := sc.Scan(&t.ID, &t.OwnerAgency, &t.Label, &t.Service, &t.URL, &enabled,
		&t.CreatedBy, &t.CreatedAt, &t.LastModifiedBy, &t.LastModifiedAt); err != nil {
		return nil, err
	}
	t.Enabled = enabled == 1
	t.URLSet = t.URL != ""
	// 2.5.1 SG2.8: the URL is sealed at rest; a plain one from before reads as is.
	url, err := secrets.OpenSetting(cfg, secrets.SettingsBinding(secrets.LabelAlertTargetURL, t.ID), t.URL)
	if err != nil {
		return nil, fmt.Errorf("open push target %s's URL: %w", t.ID, err)
	}
	t.URL = url
	return &t, nil
}

// sealTargetURL is what is stored for url: a token when a KEK is configured,
// the URL itself when none is (SealAlertTargetURLs seals it once there is).
func sealTargetURL(cfg *config.Config, id, url string) (string, error) {
	if url == "" {
		return "", nil
	}
	tok, err := secrets.EncryptSetting(cfg, secrets.SettingsBinding(secrets.LabelAlertTargetURL, id), url)
	if errors.Is(err, secrets.ErrNoKEK) {
		return url, nil
	}
	return tok, err
}

// SealAlertTargetURLs seals every push-target URL still stored in plain text
// (2.5.1 SG2.8): those from before 2.5.1, and those written while no KEK was
// configured. Run at start; a no-op once each is sealed, and when there is no
// KEK. Each UPDATE is guarded on the value read, so a concurrent save wins.
// Returns how many it sealed.
func SealAlertTargetURLs(ctx context.Context, database *sql.DB, cfg *config.Config) (int, error) {
	rows, err := database.QueryContext(ctx, `SELECT id, url FROM alert_targets WHERE url <> ''`)
	if err != nil {
		return 0, err
	}
	type plain struct{ id, url string }
	var todo []plain
	for rows.Next() {
		var p plain
		if err := rows.Scan(&p.id, &p.url); err != nil {
			rows.Close()
			return 0, err
		}
		if _, sealed := secrets.TokenKEKVersion(p.url); !sealed {
			todo = append(todo, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, p := range todo {
		tok, err := secrets.EncryptSetting(cfg, secrets.SettingsBinding(secrets.LabelAlertTargetURL, p.id), p.url)
		if errors.Is(err, secrets.ErrNoKEK) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		res, err := database.ExecContext(ctx, `UPDATE alert_targets SET url = ? WHERE id = ? AND url = ?`, tok, p.id, p.url)
		if err != nil {
			return n, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
		}
	}
	if n > 0 {
		secrets.RedactionSourceChanged()
	}
	return n, nil
}

// ListAlertTargets returns every push target, each agency's in its order.
func ListAlertTargets(ctx context.Context, database *sql.DB, cfg *config.Config) ([]AlertTarget, error) {
	rows, err := database.QueryContext(ctx, `SELECT `+alertTargetCols+` FROM alert_targets ORDER BY owner_agency, position, id`)
	if err != nil {
		return nil, fmt.Errorf("list push targets: %w", err)
	}
	defer rows.Close()
	out := []AlertTarget{}
	for rows.Next() {
		t, err := scanAlertTarget(cfg, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// GetAlertTarget fetches one push target; nil when there is none.
func GetAlertTarget(ctx context.Context, database *sql.DB, cfg *config.Config, id string) (*AlertTarget, error) {
	t, err := scanAlertTarget(cfg, database.QueryRowContext(ctx, `SELECT `+alertTargetCols+` FROM alert_targets WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}

// CreateAlertTarget adds a push target at the end of its agency's list.
func CreateAlertTarget(ctx context.Context, database *sql.DB, cfg *config.Config, inp AlertTargetInput, actor string) (*AlertTarget, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	id := db.NewID() // before the seal: the URL is bound to its row (2.5.1 SG2.10)
	stored, err := sealTargetURL(cfg, id, inp.URL)
	if err != nil {
		return nil, fmt.Errorf("seal push target URL: %w", err)
	}
	owner := ownerOrGlobalAgency(inp.OwnerAgency)
	if _, err := database.ExecContext(ctx, `INSERT INTO alert_targets
		(id, owner_agency, label, service, url, enabled, position, created_by, created_at, last_modified_by, last_modified_at)
		VALUES (?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(position) + 1, 0) FROM alert_targets WHERE owner_agency = ?), ?, ?, ?, ?)`,
		id, owner, inp.Label, inp.Service, stored, boolInt(inp.Enabled), owner, actor, now, actor, now); err != nil {
		return nil, fmt.Errorf("create push target: %w", err)
	}
	secrets.RedactionSourceChanged()
	audit(ctx, database, owner, actor, "Alerts", "created", alertTargetName(inp.Label, id), "")
	return GetAlertTarget(ctx, database, cfg, id)
}

// UpdateAlertTarget replaces a push target's label, service, URL and switch;
// its agency stays. Nil when there is none.
func UpdateAlertTarget(ctx context.Context, database *sql.DB, cfg *config.Config, id string, inp AlertTargetInput, actor string) (*AlertTarget, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	stored, err := sealTargetURL(cfg, id, inp.URL)
	if err != nil {
		return nil, fmt.Errorf("seal push target URL: %w", err)
	}
	res, err := database.ExecContext(ctx, `UPDATE alert_targets SET label=?, service=?, url=?, enabled=?,
		last_modified_by=?, last_modified_at=? WHERE id=?`,
		inp.Label, inp.Service, stored, boolInt(inp.Enabled), actor, now, id)
	if err != nil {
		return nil, fmt.Errorf("update push target: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}
	secrets.RedactionSourceChanged()
	audit(ctx, database, RowAgency(ctx, database, "alert_targets", "owner_agency", id), actor, "Alerts", "updated", alertTargetName(inp.Label, id), "")
	return GetAlertTarget(ctx, database, cfg, id)
}

// DeleteAlertTarget removes a push target; the rules that named it stop
// naming it (alert_rule_targets cascades).
func DeleteAlertTarget(ctx context.Context, database *sql.DB, id, actor string) (bool, error) {
	// The label and owner only: a delete has no business opening the URL.
	var t *AlertTarget
	var label, owner string
	if database.QueryRowContext(ctx, `SELECT label, owner_agency FROM alert_targets WHERE id=?`, id).Scan(&label, &owner) == nil {
		t = &AlertTarget{Label: label, OwnerAgency: owner}
	}
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

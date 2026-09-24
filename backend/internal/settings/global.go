package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// ErrValidation marks a settings update rejected for a bad input value (as
// opposed to a storage failure). Handlers map it to HTTP 422 via errors.Is.
var ErrValidation = errors.New("settings validation failed")

// ResolveEffectiveTimezone returns the zone the app schedules + displays in:
// the stored setting if set and loadable, else time.Local (which Go derives
// from $TZ — the "TZ fallback"). Never returns nil. A bad stored value never
// wedges the scheduler — it degrades to time.Local (validate-on-save is the
// gate that normally keeps a bad value out; this is defense-in-depth).
func ResolveEffectiveTimezone(gs *GlobalSettings) *time.Location {
	if gs != nil {
		if tz := strings.TrimSpace(gs.Timezone); tz != "" {
			if loc, err := time.LoadLocation(tz); err == nil {
				return loc
			}
			// bad stored value — fall through to time.Local (principle §2.2)
		}
	}
	return time.Local
}

// GlobalSettings is the aggregate of all global settings keys.
type GlobalSettings struct {
	AppName           string         `json:"appName"`
	Timezone          string         `json:"timezone"`
	MaxConcurrent     int            `json:"maxConcurrent"`
	JobTimeoutSeconds int            `json:"jobTimeoutSeconds"`
	SessionPolicy     *SessionPolicy `json:"sessionPolicy,omitempty"`
	// DefaultExecutor is the global executor default applied when neither a
	// per-trigger override nor a job's spec.executor is set (R5.1). ssh|runner;
	// empty/unknown ⇒ falls through to the run-type capability default at
	// trigger time (shell types ⇒ ssh, ansible/terraform ⇒ runner).
	DefaultExecutor string `json:"defaultExecutor"`
}

// SessionPolicy is a nested object within GeneralSettings.
type SessionPolicy struct {
	TimeoutMinutes int  `json:"timeoutMinutes"`
	Reauth         bool `json:"reauth"`
}

// GetGlobalSettings reads the settings KV table and assembles the aggregate.
func GetGlobalSettings(ctx context.Context, database *sql.DB) (*GlobalSettings, error) {
	rows, err := database.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	defer rows.Close()
	kv := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		kv[k] = v
	}
	return assembleGlobalSettings(kv), rows.Err()
}

// UpdateGlobalSettings persists the settings and writes audit rows.
func UpdateGlobalSettings(ctx context.Context, database *sql.DB, inp GlobalSettings, actor string) (*GlobalSettings, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	// defaultExecutor is an enum (R5.1): empty ⇒ reset to the ssh default; any
	// other value must be one of ssh|runner.
	if inp.DefaultExecutor != "" && inp.DefaultExecutor != "ssh" && inp.DefaultExecutor != "runner" {
		return nil, fmt.Errorf("%w: invalid defaultExecutor %q (want ssh|runner)", ErrValidation, inp.DefaultExecutor)
	}

	// timezone must be a loadable IANA zone — this is the gate that keeps a bad
	// value out of the scheduler/display path (timezone-update §6). Empty ⇒ the
	// effective zone falls back to time.Local at resolve time.
	if tz := strings.TrimSpace(inp.Timezone); tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return nil, fmt.Errorf("%w: invalid timezone %q", ErrValidation, tz)
		}
	}

	kvs := flattenGlobalSettings(inp)
	for k, v := range kvs {
		_, err := database.ExecContext(ctx,
			`INSERT INTO settings (key, value, last_modified_by, last_modified_at)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value,
			  last_modified_by=excluded.last_modified_by,
			  last_modified_at=excluded.last_modified_at`,
			k, v, actor, now)
		if err != nil {
			return nil, fmt.Errorf("upsert setting %q: %w", k, err)
		}
	}
	audit(ctx, database, actor, "Settings", "updated", "general", "")
	return GetGlobalSettings(ctx, database)
}

func flattenGlobalSettings(s GlobalSettings) map[string]string {
	m := map[string]string{
		"appName":           s.AppName,
		"timezone":          s.Timezone,
		"maxConcurrent":     fmt.Sprintf("%d", s.MaxConcurrent),
		"jobTimeoutSeconds": fmt.Sprintf("%d", s.JobTimeoutSeconds),
		"defaultExecutor":   s.DefaultExecutor,
	}
	if s.SessionPolicy != nil {
		b, _ := json.Marshal(s.SessionPolicy)
		m["sessionPolicy"] = string(b)
	}
	return m
}

func assembleGlobalSettings(kv map[string]string) *GlobalSettings {
	s := &GlobalSettings{
		AppName:           kvStr(kv, "appName", "Cronomicon"),
		Timezone:          kvStr(kv, "timezone", "UTC"),
		MaxConcurrent:     kvInt(kv, "maxConcurrent", 10),
		JobTimeoutSeconds: kvInt(kv, "jobTimeoutSeconds", 3600),
		DefaultExecutor:   kvStr(kv, "defaultExecutor", "ssh"),
	}
	if raw, ok := kv["sessionPolicy"]; ok && raw != "" {
		var sp SessionPolicy
		if err := json.Unmarshal([]byte(raw), &sp); err == nil {
			s.SessionPolicy = &sp
		}
	}
	return s
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func kvStr(m map[string]string, k, def string) string {
	if v, ok := m[k]; ok && v != "" {
		return v
	}
	return def
}

func kvInt(m map[string]string, k string, def int) int {
	if v, ok := m[k]; ok && v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

// NotificationConfig is the operator-managed notification config.
type NotificationConfig struct {
	Provider string         `json:"provider"`
	Apprise  *AppriseConfig `json:"apprise,omitempty"`
	SMTP     *SMTPConfig    `json:"smtp,omitempty"`
	// LastSent is read-only: when each transport last delivered, and whether it
	// worked (K-2). It replaced a per-destination "last fired" stamp that could
	// only ever mean "something sent over this transport" — see migration 740.
	LastSent *LastSentByTransport `json:"lastSent,omitempty"`
}

// LastSentByTransport carries one entry per transport that has ever sent. A nil
// entry means "never", which is a different fact from "failed" and is why these
// are pointers rather than zero-valued structs.
type LastSentByTransport struct {
	Email   *TransportSend `json:"email,omitempty"`
	Apprise *TransportSend `json:"apprise,omitempty"`
}

// TransportSend is one transport's most recent dispatch attempt.
type TransportSend struct {
	At     string `json:"at"`     // RFC3339 UTC
	Status string `json:"status"` // "ok" | "error"
}

// AppriseConfig holds Apprise-specific fields. Targets are the Apprise service
// endpoints (e.g. "slack://...", "mailto://...") the gateway fans out to.
type AppriseConfig struct {
	Enabled bool            `json:"enabled"`
	APIURL  string          `json:"apiUrl"`
	Targets []AppriseTarget `json:"targets"`
}

// AppriseTarget is a single Apprise service endpoint. URL is the service DSN the
// gateway delivers to; Label/Service are operator-facing metadata; Enabled gates
// whether the target actually receives sends. ID is server-assigned (read-only)
// and exists only to give the UI a stable key — it is not persisted.
type AppriseTarget struct {
	ID      int    `json:"id,omitempty"`
	Label   string `json:"label,omitempty"`
	Service string `json:"service,omitempty"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

// SMTPConfig holds SMTP fields. Password is write-only: it is accepted on input
// (and envelope-encrypted before storage), never returned. PasswordSet tells the
// UI whether a password is on file.
type SMTPConfig struct {
	Host        string   `json:"host"`
	Port        int      `json:"port"`
	Encryption  string   `json:"encryption"`
	Username    string   `json:"username"`
	Password    string   `json:"password,omitempty"`
	PasswordSet bool     `json:"passwordSet"`
	FromAddress string   `json:"fromAddress"`
	FromName    string   `json:"fromName"`
	Recipients  []string `json:"recipients"`
}

// GetNotificationConfig reads the notification_config singleton row. The SMTP
// password is never returned — only PasswordSet (whether one is stored).
func GetNotificationConfig(ctx context.Context, database *sql.DB) (*NotificationConfig, error) {
	row := database.QueryRowContext(ctx, `
		SELECT provider, smtp_host, smtp_port, smtp_from, smtp_from_name, smtp_username,
		       smtp_password_enc, smtp_encryption, smtp_recipients,
		       apprise_enabled, apprise_url, apprise_targets,
		       last_email_at, last_email_status, last_apprise_at, last_apprise_status
		FROM notification_config WHERE id=1`)
	var provider, smtpHost, smtpFrom, fromName, username, pwEnc, encryption sql.NullString
	var recipientsJSON, appriseURL, appriseTargets sql.NullString
	var lastEmailAt, lastEmailStatus, lastAppriseAt, lastAppriseStatus sql.NullString
	var smtpPort, appriseEnabled sql.NullInt64
	if err := row.Scan(&provider, &smtpHost, &smtpPort, &smtpFrom, &fromName, &username,
		&pwEnc, &encryption, &recipientsJSON, &appriseEnabled, &appriseURL, &appriseTargets,
		&lastEmailAt, &lastEmailStatus, &lastAppriseAt, &lastAppriseStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &NotificationConfig{Provider: "smtp"}, nil
		}
		return nil, fmt.Errorf("read notification_config: %w", err)
	}
	cfg := &NotificationConfig{Provider: kvStrOr(provider.String, "smtp")}
	if smtpHost.Valid && smtpHost.String != "" {
		cfg.SMTP = &SMTPConfig{
			Host:        smtpHost.String,
			Port:        int(smtpPort.Int64),
			Encryption:  encryption.String,
			Username:    username.String,
			PasswordSet: pwEnc.Valid && pwEnc.String != "",
			FromAddress: smtpFrom.String,
			FromName:    fromName.String,
			Recipients:  parseStrList(recipientsJSON.String),
		}
	}
	targets := ParseAppriseTargets(appriseTargets.String)
	if appriseEnabled.Int64 == 1 || (appriseURL.Valid && appriseURL.String != "") || len(targets) > 0 {
		cfg.Apprise = &AppriseConfig{
			Enabled: appriseEnabled.Int64 == 1,
			APIURL:  appriseURL.String,
			Targets: targets,
		}
	}
	if send := transportSend(lastEmailAt, lastEmailStatus); send != nil {
		cfg.LastSent = &LastSentByTransport{Email: send}
	}
	if send := transportSend(lastAppriseAt, lastAppriseStatus); send != nil {
		if cfg.LastSent == nil {
			cfg.LastSent = &LastSentByTransport{}
		}
		cfg.LastSent.Apprise = send
	}
	return cfg, nil
}

// transportSend builds a TransportSend, or nil when the transport has never
// sent. An absent timestamp is "never" — not a send with an empty date.
func transportSend(at, status sql.NullString) *TransportSend {
	if !at.Valid || at.String == "" {
		return nil
	}
	return &TransportSend{At: at.String, Status: status.String}
}

// UpdateNotificationConfig upserts the notification_config singleton. A non-empty
// SMTP password is envelope-encrypted (KEK) before storage; a blank password
// preserves the stored one (write-only field).
func UpdateNotificationConfig(ctx context.Context, database *sql.DB, appCfg *config.Config, inp NotificationConfig, actor string) (*NotificationConfig, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	var smtpHost, smtpFrom, fromName, username, encryption *string
	var smtpPort *int
	var recipientsJSON = "[]"
	if inp.SMTP != nil {
		smtpHost = &inp.SMTP.Host
		smtpFrom = &inp.SMTP.FromAddress
		fromName = &inp.SMTP.FromName
		username = &inp.SMTP.Username
		encryption = &inp.SMTP.Encryption
		smtpPort = &inp.SMTP.Port
		if b, err := json.Marshal(nullSlice(inp.SMTP.Recipients)); err == nil {
			recipientsJSON = string(b)
		}
	}

	// Password: encrypt if newly provided; otherwise keep the existing token.
	pwEnc, err := resolveSMTPPassword(ctx, database, appCfg, inp)
	if err != nil {
		return nil, err
	}

	var appriseEnabled int
	var appriseURL *string
	// apprise_targets: set from input when an Apprise block is provided, else
	// preserve the stored value (so an SMTP-only update doesn't wipe targets).
	appriseTargetsJSON := "[]"
	if inp.Apprise != nil {
		if inp.Apprise.Enabled {
			appriseEnabled = 1
		}
		appriseURL = &inp.Apprise.APIURL
		// id is read-only/server-assigned — don't persist whatever the client echoed back.
		stored := make([]AppriseTarget, len(inp.Apprise.Targets))
		for i, t := range inp.Apprise.Targets {
			t.ID = 0
			stored[i] = t
		}
		if b, err := json.Marshal(stored); err == nil {
			appriseTargetsJSON = string(b)
		}
	} else {
		var existing sql.NullString
		if err := database.QueryRowContext(ctx, `SELECT apprise_targets FROM notification_config WHERE id=1`).Scan(&existing); err == nil && existing.Valid && existing.String != "" {
			appriseTargetsJSON = existing.String
		}
	}

	_, err = database.ExecContext(ctx, `
		INSERT INTO notification_config
		  (id, provider, smtp_host, smtp_port, smtp_from, smtp_from_name, smtp_username,
		   smtp_password_enc, smtp_encryption, smtp_recipients, apprise_enabled, apprise_url,
		   apprise_targets, last_modified_by, last_modified_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  provider=excluded.provider, smtp_host=excluded.smtp_host, smtp_port=excluded.smtp_port,
		  smtp_from=excluded.smtp_from, smtp_from_name=excluded.smtp_from_name,
		  smtp_username=excluded.smtp_username, smtp_password_enc=excluded.smtp_password_enc,
		  smtp_encryption=excluded.smtp_encryption, smtp_recipients=excluded.smtp_recipients,
		  apprise_enabled=excluded.apprise_enabled, apprise_url=excluded.apprise_url,
		  apprise_targets=excluded.apprise_targets,
		  last_modified_by=excluded.last_modified_by, last_modified_at=excluded.last_modified_at`,
		kvStrOr(inp.Provider, "smtp"), smtpHost, smtpPort, smtpFrom, fromName, username,
		pwEnc, encryption, recipientsJSON, appriseEnabled, appriseURL, appriseTargetsJSON, actor, now,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert notification_config: %w", err)
	}
	secrets.RedactionSourceChanged() // AM-4b
	audit(ctx, database, actor, "Settings", "updated", "notification", "")
	return GetNotificationConfig(ctx, database)
}

// resolveSMTPPassword returns the encrypted password token to store: a freshly
// encrypted value when the input carries a new password, else the existing token
// (so a blank password on update doesn't wipe the stored one).
func resolveSMTPPassword(ctx context.Context, database *sql.DB, appCfg *config.Config, inp NotificationConfig) (*string, error) {
	if inp.SMTP != nil && inp.SMTP.Password != "" {
		token, err := secrets.EncryptString(appCfg, inp.SMTP.Password)
		if err != nil {
			return nil, fmt.Errorf("encrypt SMTP password (is a KEK configured?): %w", err)
		}
		return &token, nil
	}
	var existing sql.NullString
	err := database.QueryRowContext(ctx, `SELECT smtp_password_enc FROM notification_config WHERE id=1`).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read existing smtp password: %w", err)
	}
	if existing.Valid && existing.String != "" {
		return &existing.String, nil
	}
	return nil, nil
}

func kvStrOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func parseStrList(s string) []string {
	if s == "" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

// ParseAppriseTargets reads the apprise_targets JSON column: an array of
// {label,service,url,enabled} objects. (The pre-rich bare-string form was
// dropped in v1.5.41 and migration 1150 rewrote any stored row carrying it, so
// there is exactly one shape on disk; an entry that is not an object skips like
// any other unparseable one.) IDs are (re)assigned 1..N so the UI always has a
// stable key.
//
// Exported because the notification DISPATCHER reads the same column through
// its own SELECT (internal/notify). It parsed the column itself until v1.5.43,
// which is how the two readers came to disagree about the legacy form: the
// settings reader dropped it and the dispatcher kept delivering to it. One
// parser, one answer — do not add a second.
func ParseAppriseTargets(s string) []AppriseTarget {
	if strings.TrimSpace(s) == "" {
		return []AppriseTarget{}
	}
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return []AppriseTarget{}
	}
	out := make([]AppriseTarget, 0, len(raw))
	for _, r := range raw {
		trimmed := strings.TrimSpace(string(r))
		if trimmed == "" {
			continue
		}
		var t AppriseTarget
		if err := json.Unmarshal(r, &t); err != nil {
			continue
		}
		t.ID = len(out) + 1
		out = append(out, t)
	}
	return out
}

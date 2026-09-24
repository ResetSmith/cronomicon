package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

type ObservabilityConfig struct {
	Enabled        bool            `json:"enabled"`
	Path           string          `json:"path"`
	AuthType       string          `json:"authType"` // none | bearer
	BearerToken    string          `json:"bearerToken,omitempty"`
	BearerTokenSet bool            `json:"bearerTokenSet"`
	Metrics        map[string]bool `json:"metrics"`
	LastModifiedBy string          `json:"lastModifiedBy,omitempty"`
	LastModifiedAt string          `json:"lastModifiedAt,omitempty"`
}

// GetObservabilityConfig reads the observability settings from settings table.
func GetObservabilityConfig(ctx context.Context, database *sql.DB, appCfg *config.Config) (*ObservabilityConfig, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT key, value, last_modified_by, last_modified_at
		FROM settings
		WHERE key LIKE 'obs.%'`)
	if err != nil {
		return nil, fmt.Errorf("query settings for obs: %w", err)
	}
	defer rows.Close()

	kv := make(map[string]string)
	var lastModBy, lastModAt string
	for rows.Next() {
		var k, v, lmBy, lmAt sql.NullString
		if err := rows.Scan(&k, &v, &lmBy, &lmAt); err != nil {
			return nil, err
		}
		kv[k.String] = v.String
		if lmBy.Valid && lmBy.String != "" {
			lastModBy = lmBy.String
		}
		if lmAt.Valid && lmAt.String != "" {
			lastModAt = lmAt.String
		}
	}

	cfg := &ObservabilityConfig{
		Enabled:        true,
		Path:           "/metrics",
		AuthType:       "none",
		Metrics:        make(map[string]bool),
		LastModifiedBy: lastModBy,
		LastModifiedAt: lastModAt,
	}

	if val, ok := kv["obs.enabled"]; ok {
		cfg.Enabled = val == "true"
	}
	if val, ok := kv["obs.path"]; ok {
		cfg.Path = val
	}
	if val, ok := kv["obs.authType"]; ok {
		cfg.AuthType = val
	}
	if tokenEnc, ok := kv["obs.bearerTokenEnc"]; ok && tokenEnc != "" {
		cfg.BearerTokenSet = true
		decrypted, err := secrets.DecryptString(appCfg, tokenEnc)
		if err == nil {
			cfg.BearerToken = maskSecret(decrypted)
		}
	}
	if val, ok := kv["obs.metrics"]; ok && val != "" {
		_ = json.Unmarshal([]byte(val), &cfg.Metrics)
	}

	return cfg, nil
}

// UpdateObservabilityConfig updates the observability settings.
func UpdateObservabilityConfig(ctx context.Context, database *sql.DB, appCfg *config.Config, inp ObservabilityConfig, actor string) (*ObservabilityConfig, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	switch inp.AuthType {
	case "", "none", "bearer":
		// Only these two: basic auth on the scrape path is a reverse-proxy job
		// (see deploy/deployment-guide.md "Health endpoints"), so the enum
		// deliberately stops at bearer rather than carrying a value the gate
		// could never honour.
	default:
		return nil, &ValidationError{Field: "authType", Message: "authType must be 'none' or 'bearer'"}
	}
	if inp.AuthType == "" {
		inp.AuthType = "none"
	}
	if inp.AuthType == "bearer" && inp.BearerToken == "" {
		// Bearer without a token (new or stored) would lock every scraper out.
		var existing sql.NullString
		err := database.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='obs.bearerTokenEnc'`).Scan(&existing)
		if err != nil || !existing.Valid || existing.String == "" {
			return nil, &ValidationError{Field: "bearerToken", Message: "bearerToken is required when authType is 'bearer'"}
		}
	}
	if inp.Path == "" {
		inp.Path = "/metrics"
	}
	if !strings.HasPrefix(inp.Path, "/") || strings.HasPrefix(inp.Path, "/api") {
		return nil, &ValidationError{Field: "path", Message: "path must start with '/' and must not be under /api"}
	}

	kvs := map[string]string{
		"obs.enabled":  boolStr(inp.Enabled),
		"obs.path":     inp.Path,
		"obs.authType": inp.AuthType,
	}

	if inp.BearerToken != "" {
		token, err := secrets.EncryptString(appCfg, inp.BearerToken)
		if err != nil {
			return nil, fmt.Errorf("encrypt bearer token: %w", err)
		}
		kvs["obs.bearerTokenEnc"] = token
	} else {
		var existing sql.NullString
		err := database.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='obs.bearerTokenEnc'`).Scan(&existing)
		if err == nil && existing.Valid && existing.String != "" {
			kvs["obs.bearerTokenEnc"] = existing.String
		}
	}

	if inp.Metrics == nil {
		inp.Metrics = make(map[string]bool)
	}
	b, _ := json.Marshal(inp.Metrics)
	kvs["obs.metrics"] = string(b)

	for k, v := range kvs {
		_, err := database.ExecContext(ctx, `
			INSERT INTO settings (key, value, last_modified_by, last_modified_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET
			  value=excluded.value,
			  last_modified_by=excluded.last_modified_by,
			  last_modified_at=excluded.last_modified_at`,
			k, v, actor, now,
		)
		if err != nil {
			return nil, fmt.Errorf("upsert setting %q: %w", k, err)
		}
	}

	secrets.RedactionSourceChanged() // AM-4b
	_ = WriteChangeLog(ctx, database, actor, "Settings", "updated", "Observability Settings", "")
	return GetObservabilityConfig(ctx, database, appCfg)
}

// MetricsGuard is the runtime view of the observability settings the /metrics
// route needs per scrape: gate on enabled, optionally require a bearer token.
type MetricsGuard struct {
	Enabled     bool
	AuthType    string // none | bearer
	BearerToken string // decrypted; empty unless AuthType == "bearer"
}

// GetMetricsGuard reads the live observability gate. Called per scrape (a
// 3-key read on the settings table) so enabled/auth changes apply without a
// restart; the path is read once at startup. Defaults preserve production
// decision #9: enabled, no auth.
func GetMetricsGuard(ctx context.Context, database *sql.DB, appCfg *config.Config) MetricsGuard {
	g := MetricsGuard{Enabled: true, AuthType: "none"}
	rows, err := database.QueryContext(ctx, `
		SELECT key, value FROM settings
		WHERE key IN ('obs.enabled', 'obs.authType', 'obs.bearerTokenEnc')`)
	if err != nil {
		return g
	}
	defer rows.Close()
	kv := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return g
		}
		kv[k] = v
	}
	if val, ok := kv["obs.enabled"]; ok {
		g.Enabled = val == "true"
	}
	if val, ok := kv["obs.authType"]; ok && val != "" {
		g.AuthType = val
	}
	if g.AuthType == "bearer" {
		if enc, ok := kv["obs.bearerTokenEnc"]; ok && enc != "" {
			if decrypted, err := secrets.DecryptString(appCfg, enc); err == nil {
				g.BearerToken = decrypted
			}
		}
	}
	return g
}

// ResolveMetricsPath returns the configured scrape path (default /metrics).
// Read once at startup — path changes take effect on restart.
func ResolveMetricsPath(ctx context.Context, database *sql.DB) string {
	var path sql.NullString
	err := database.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='obs.path'`).Scan(&path)
	if err == nil && path.Valid && strings.HasPrefix(path.String, "/") {
		return path.String
	}
	return "/metrics"
}

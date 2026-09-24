package settings

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// Sentinel errors for webhook-secret rotation, so the handler can map them to
// the right HTTP status instead of string-matching error text.
var (
	// ErrGitlabUnreachable → 503: GitLab could not be reached/answered; the
	// secret was NOT rotated (per the spec's 503 semantics).
	ErrGitlabUnreachable = errors.New("gitlab unreachable")
	// ErrRotateNotConfigured → 422: rotation with updateGitlab=true needs a
	// repo URL and PAT.
	ErrRotateNotConfigured = errors.New("rotation not configured")
	// ErrWebhookSecretEnvPinned → 409: AMADEUS_GITLAB_WEBHOOK_SECRET is set, so
	// webhook validation only accepts the env value — rotating the DB secret
	// (and updating the GitLab hook) would break every webhook delivery.
	ErrWebhookSecretEnvPinned = errors.New("webhook secret is pinned by AMADEUS_GITLAB_WEBHOOK_SECRET; unset it to rotate via the API")
)

type GitlabConfig struct {
	Pat                   string        `json:"pat,omitempty"`
	PatSet                bool          `json:"patSet"`
	BotName               string        `json:"botName"`
	BotEmail              string        `json:"botEmail"`
	WriteBranch           string        `json:"writeBranch"`
	RepoUrl               string        `json:"repoUrl"`
	TokenExpiryNotifyDays int           `json:"tokenExpiryNotifyDays"`
	WebhookEnabled        bool          `json:"webhookEnabled"`
	WebhookEvents         WebhookEvents `json:"webhookEvents"`
	// WebhookSecretEnvPinned is read-only (LB7): true when the webhook secret is
	// pinned by the AMADEUS_GITLAB_WEBHOOK_SECRET env var, in which case API/UI
	// rotation is rejected (ErrWebhookSecretEnvPinned). Set on read; ignored on write.
	WebhookSecretEnvPinned bool   `json:"webhookSecretEnvPinned"`
	LastModifiedBy         string `json:"lastModifiedBy,omitempty"`
	LastModifiedAt         string `json:"lastModifiedAt,omitempty"`
}

type WebhookEvents struct {
	Push bool `json:"push"`
	Mr   bool `json:"mr"`
	Tag  bool `json:"tag"`
}

// WebhookPolicy is the delivery half of the GitLab webhook settings, read on the
// unauthenticated webhook path (F2-3). It is deliberately NOT GetGitlabConfig:
// that one decrypts the PAT, applies env overrides and needs *config.Config,
// none of which a delivery decision should depend on — and the webhook handler
// must not fail because a PAT will not decrypt.
type WebhookPolicy struct {
	Enabled bool
	Events  WebhookEvents
}

// Accepts reports whether a delivery of the given X-Gitlab-Event should trigger
// a sync. An EMPTY header means yes (subject to the push flag): GitLab always
// sends one, so a bare POST is our own tooling or a curl smoke-test, and today
// that syncs. An event we have no flag for — Issue Hook, Pipeline Hook, a
// wildcard hook someone pointed at us — is ignored rather than resynced: the
// three flags enumerate what this integration reacts to.
func (p WebhookPolicy) Accepts(event string) bool {
	switch event {
	case "", "Push Hook":
		return p.Events.Push
	case "Tag Push Hook":
		return p.Events.Tag
	case "Merge Request Hook":
		return p.Events.Mr
	default:
		return false
	}
}

// GetWebhookPolicy reads the four webhook columns from the gitlab_config
// singleton.
//
// It FAILS OPEN — no row, no table, any read error → enabled with every event
// on. That is not laziness about errors: these flags were stored but never read
// until F2-3, so every install's *effective* policy has always been "on", and a
// transient DB error must not be the thing that silently stops a repo syncing.
// Migration 730 back-fills existing rows to that same effective policy, so
// wiring the control changes what an operator can turn OFF, never what a working
// deployment does on upgrade.
func GetWebhookPolicy(ctx context.Context, database *sql.DB) WebhookPolicy {
	open := WebhookPolicy{Enabled: true, Events: WebhookEvents{Push: true, Mr: true, Tag: true}}

	var enabled, push, mr, tag sql.NullInt64
	err := database.QueryRowContext(ctx, `
		SELECT webhook_enabled, webhook_events_push, webhook_events_mr, webhook_events_tag
		FROM gitlab_config WHERE id=1`).Scan(&enabled, &push, &mr, &tag)
	if err != nil {
		return open
	}
	return WebhookPolicy{
		Enabled: enabled.Int64 == 1,
		Events: WebhookEvents{
			Push: push.Int64 == 1,
			Mr:   mr.Int64 == 1,
			Tag:  tag.Int64 == 1,
		},
	}
}

func maskSecret(val string) string {
	if val == "" {
		return ""
	}
	if len(val) <= 4 {
		return "••••"
	}
	return "••••" + val[len(val)-4:]
}

// GetGitlabConfig reads the gitlab_config singleton.
func GetGitlabConfig(ctx context.Context, database *sql.DB, appCfg *config.Config) (*GitlabConfig, error) {
	row := database.QueryRowContext(ctx, `
		SELECT pat_enc, bot_name, bot_email, write_branch, repo_url, token_expiry_notify_days,
		       webhook_enabled, webhook_events_push, webhook_events_mr, webhook_events_tag,
		       last_modified_by, last_modified_at
		FROM gitlab_config WHERE id=1`)
	var patEnc, botName, botEmail, writeBranch, repoUrl, lastModBy, lastModAt sql.NullString
	var tokenExpiryNotifyDays, webhookEnabled, push, mr, tag sql.NullInt64

	var dbExists = true
	if err := row.Scan(&patEnc, &botName, &botEmail, &writeBranch, &repoUrl, &tokenExpiryNotifyDays,
		&webhookEnabled, &push, &mr, &tag, &lastModBy, &lastModAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			dbExists = false
		} else {
			return nil, fmt.Errorf("read gitlab_config: %w", err)
		}
	}

	cfg := &GitlabConfig{
		BotName:               "amadeus-bot",
		BotEmail:              "amadeus-bot@amadeus.io",
		WriteBranch:           "main",
		TokenExpiryNotifyDays: 7,
	}

	if dbExists {
		cfg.BotName = kvStrOr(botName.String, "amadeus-bot")
		cfg.BotEmail = kvStrOr(botEmail.String, "amadeus-bot@amadeus.io")
		cfg.WriteBranch = kvStrOr(writeBranch.String, "main")
		cfg.RepoUrl = repoUrl.String
		cfg.TokenExpiryNotifyDays = int(tokenExpiryNotifyDays.Int64)
		cfg.WebhookEnabled = webhookEnabled.Int64 == 1
		cfg.WebhookEvents = WebhookEvents{
			Push: push.Int64 == 1,
			Mr:   mr.Int64 == 1,
			Tag:  tag.Int64 == 1,
		}
		cfg.LastModifiedBy = lastModBy.String
		cfg.LastModifiedAt = lastModAt.String

		if patEnc.Valid && patEnc.String != "" {
			cfg.PatSet = true
			decrypted, err := secrets.DecryptString(appCfg, patEnc.String)
			if err == nil {
				cfg.Pat = maskSecret(decrypted)
			}
		}
	}

	// Apply environment overrides if set (E.3)
	if envURL := appCfg.GitLabBaseURL; envURL != "" {
		cfg.RepoUrl = envURL
	}
	if envToken := os.Getenv("AMADEUS_GITLAB_TOKEN"); envToken != "" {
		cfg.PatSet = true
		cfg.Pat = maskSecret(envToken)
	}

	// LB7: surface whether the webhook secret is env-pinned so the UI can disable
	// rotation (which would otherwise 409 with ErrWebhookSecretEnvPinned).
	cfg.WebhookSecretEnvPinned = os.Getenv("AMADEUS_GITLAB_WEBHOOK_SECRET") != ""

	return cfg, nil
}

// UpdateGitlabConfig updates the gitlab_config singleton.
func UpdateGitlabConfig(ctx context.Context, database *sql.DB, appCfg *config.Config, inp GitlabConfig, actor string) (*GitlabConfig, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	var patEnc *string
	if inp.Pat != "" {
		token, err := secrets.EncryptString(appCfg, inp.Pat)
		if err != nil {
			return nil, fmt.Errorf("encrypt PAT: %w", err)
		}
		patEnc = &token
	} else {
		var existing sql.NullString
		err := database.QueryRowContext(ctx, `SELECT pat_enc FROM gitlab_config WHERE id=1`).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("read existing pat: %w", err)
		}
		if existing.Valid && existing.String != "" {
			patEnc = &existing.String
		}
	}

	webhookEnabledVal := 0
	if inp.WebhookEnabled {
		webhookEnabledVal = 1
	}
	pushVal := 0
	if inp.WebhookEvents.Push {
		pushVal = 1
	}
	mrVal := 0
	if inp.WebhookEvents.Mr {
		mrVal = 1
	}
	tagVal := 0
	if inp.WebhookEvents.Tag {
		tagVal = 1
	}

	_, err := database.ExecContext(ctx, `
		INSERT INTO gitlab_config
		  (id, pat_enc, bot_name, bot_email, write_branch, repo_url, token_expiry_notify_days,
		   webhook_enabled, webhook_events_push, webhook_events_mr, webhook_events_tag,
		   last_modified_by, last_modified_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  pat_enc=excluded.pat_enc, bot_name=excluded.bot_name, bot_email=excluded.bot_email,
		  write_branch=excluded.write_branch, repo_url=excluded.repo_url,
		  token_expiry_notify_days=excluded.token_expiry_notify_days,
		  webhook_enabled=excluded.webhook_enabled,
		  webhook_events_push=excluded.webhook_events_push,
		  webhook_events_mr=excluded.webhook_events_mr,
		  webhook_events_tag=excluded.webhook_events_tag,
		  last_modified_by=excluded.last_modified_by, last_modified_at=excluded.last_modified_at`,
		patEnc, inp.BotName, inp.BotEmail, inp.WriteBranch, inp.RepoUrl, inp.TokenExpiryNotifyDays,
		webhookEnabledVal, pushVal, mrVal, tagVal, actor, now,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert gitlab_config: %w", err)
	}

	secrets.RedactionSourceChanged() // AM-4b
	_ = WriteChangeLog(ctx, database, actor, "Settings", "updated", "GitLab Connection", "")
	return GetGitlabConfig(ctx, database, appCfg)
}

// RotateWebhookSecret rotates the GitLab webhook secret.
func RotateWebhookSecret(ctx context.Context, database *sql.DB, appCfg *config.Config, updateGitlab bool, overlapMinutes int, actor string) (string, bool, time.Time, error) {
	// Rotation is meaningless while the env override pins validation to a
	// single value — and worse, updating the GitLab hook to the new secret
	// would break every webhook delivery.
	if os.Getenv("AMADEUS_GITLAB_WEBHOOK_SECRET") != "" {
		return "", false, time.Time{}, ErrWebhookSecretEnvPinned
	}

	newSecretBytes := make([]byte, 32)
	if _, err := rand.Read(newSecretBytes); err != nil {
		return "", false, time.Time{}, fmt.Errorf("generate random webhook secret: %w", err)
	}
	newSecret := hex.EncodeToString(newSecretBytes)

	newSecretEnc, err := secrets.EncryptString(appCfg, newSecret)
	if err != nil {
		return "", false, time.Time{}, fmt.Errorf("encrypt webhook secret: %w", err)
	}

	var currentEnc sql.NullString
	err = database.QueryRowContext(ctx, `SELECT webhook_secret_enc FROM gitlab_config WHERE id=1`).Scan(&currentEnc)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, time.Time{}, fmt.Errorf("read current webhook secret: %w", err)
	}

	overlapUntil := time.Now().UTC().Add(time.Duration(overlapMinutes) * time.Minute)
	overlapUntilStr := overlapUntil.Format(time.RFC3339)

	var prevEnc *string
	if currentEnc.Valid && currentEnc.String != "" {
		prevEnc = &currentEnc.String
	}

	gitlabUpdated := false
	if updateGitlab {
		gitlabCfg, err := GetGitlabConfig(ctx, database, appCfg)
		if err != nil {
			return "", false, time.Time{}, fmt.Errorf("get gitlab config: %w", err)
		}

		if gitlabCfg.RepoUrl == "" {
			return "", false, time.Time{}, fmt.Errorf("%w: repo URL not set", ErrRotateNotConfigured)
		}

		var pat string
		if envToken := os.Getenv("AMADEUS_GITLAB_TOKEN"); envToken != "" {
			pat = envToken
		} else if gitlabCfg.PatSet {
			var patEnc sql.NullString
			if err := database.QueryRowContext(ctx, `SELECT pat_enc FROM gitlab_config WHERE id=1`).Scan(&patEnc); err == nil && patEnc.Valid {
				decrypted, err := secrets.DecryptString(appCfg, patEnc.String)
				if err == nil {
					pat = decrypted
				}
			}
		}

		if pat == "" {
			return "", false, time.Time{}, fmt.Errorf("%w: PAT not set", ErrRotateNotConfigured)
		}

		baseURL, projectPath, err := parseGitLabProjectPath(gitlabCfg.RepoUrl)
		if err != nil {
			return "", false, time.Time{}, fmt.Errorf("parse gitlab project path: %w", err)
		}

		reqURL := fmt.Sprintf("%s/api/v4/projects/%s/hooks", baseURL, projectPath)
		client := &http.Client{
			Timeout: 10 * time.Second,
			// SU-8: refuse redirects. Go's stdlib does NOT strip the custom
			// Private-Token header on a cross-host redirect, so following one to an
			// attacker host would leak the PAT. The GitLab REST API needs no redirect
			// here; this covers both the GET below and the PUT that follows.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			// SU-7: guard egress (SSRF) on the operator-configured GitLab host.
			Transport: httpx.SafeTransport(nil, httpx.EgressPolicy{
				AllowPrivate: appCfg.OutboundAllowPrivate, AllowLoopback: appCfg.OutboundAllowLoopback,
			}),
		}
		req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
		if err != nil {
			return "", false, time.Time{}, err
		}
		req.Header.Set("Private-Token", pat)

		resp, err := client.Do(req)
		if err != nil {
			return "", false, time.Time{}, fmt.Errorf("%w: %v", ErrGitlabUnreachable, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return "", false, time.Time{}, fmt.Errorf("%w: api returned status %d", ErrGitlabUnreachable, resp.StatusCode)
		}

		var hooks []struct {
			ID  int    `json:"id"`
			URL string `json:"url"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&hooks); err != nil {
			return "", false, time.Time{}, fmt.Errorf("parse gitlab hooks response: %w", err)
		}

		var hookID int
		for _, h := range hooks {
			if strings.HasSuffix(h.URL, "/api/v1/webhooks/gitlab") {
				hookID = h.ID
				break
			}
		}

		if hookID != 0 {
			updateURL := fmt.Sprintf("%s/api/v4/projects/%s/hooks/%d", baseURL, projectPath, hookID)
			body := map[string]string{"token": newSecret}
			bodyBytes, _ := json.Marshal(body)
			putReq, err := http.NewRequestWithContext(ctx, "PUT", updateURL, strings.NewReader(string(bodyBytes)))
			if err != nil {
				return "", false, time.Time{}, err
			}
			putReq.Header.Set("Private-Token", pat)
			putReq.Header.Set("Content-Type", "application/json")

			putResp, err := client.Do(putReq)
			if err != nil {
				return "", false, time.Time{}, fmt.Errorf("%w: update hook failed: %v", ErrGitlabUnreachable, err)
			}
			putResp.Body.Close()

			if putResp.StatusCode == http.StatusOK {
				gitlabUpdated = true
			}
		}
	}

	nowTime := time.Now().UTC().Format(time.RFC3339)
	_, err = database.ExecContext(ctx, `
		INSERT INTO gitlab_config
		  (id, webhook_secret_enc, webhook_secret_prev_enc, webhook_overlap_until, last_modified_by, last_modified_at)
		VALUES (1, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  webhook_secret_enc=excluded.webhook_secret_enc,
		  webhook_secret_prev_enc=excluded.webhook_secret_prev_enc,
		  webhook_overlap_until=excluded.webhook_overlap_until,
		  last_modified_by=excluded.last_modified_by,
		  last_modified_at=excluded.last_modified_at`,
		newSecretEnc, prevEnc, overlapUntilStr, actor, nowTime,
	)
	if err != nil {
		return "", false, time.Time{}, fmt.Errorf("save rotated webhook secret: %w", err)
	}

	secrets.RedactionSourceChanged() // AM-4b
	_ = WriteChangeLog(ctx, database, actor, "Settings", "rotated", "GitLab Webhook Secret", "")
	return newSecret, gitlabUpdated, overlapUntil, nil
}

// ResolveGitlabRuntime returns the effective repo URL and PAT for the sync
// service: env (AMADEUS_GITLAB_BASE_URL / AMADEUS_GITLAB_TOKEN) wins when set
// (E.3 precedence); otherwise the DB-backed gitlab_config is used. Read at
// startup by mountGit — changes take effect on restart.
func ResolveGitlabRuntime(ctx context.Context, database *sql.DB, appCfg *config.Config) (repoURL, pat string) {
	repoURL = appCfg.GitLabBaseURL
	pat = os.Getenv("AMADEUS_GITLAB_TOKEN")
	if repoURL != "" && pat != "" {
		return repoURL, pat
	}

	row := database.QueryRowContext(ctx, `SELECT repo_url, pat_enc FROM gitlab_config WHERE id=1`)
	var dbRepoURL, patEnc sql.NullString
	if err := row.Scan(&dbRepoURL, &patEnc); err != nil {
		return repoURL, pat
	}
	if repoURL == "" {
		repoURL = dbRepoURL.String
	}
	if pat == "" && patEnc.Valid && patEnc.String != "" {
		if decrypted, err := secrets.DecryptString(appCfg, patEnc.String); err == nil {
			pat = decrypted
		}
	}
	return repoURL, pat
}

func parseGitLabProjectPath(repoURL string) (baseURL, projectPath string, err error) {
	repoURL = strings.TrimSuffix(repoURL, ".git")
	u, err := url.Parse(repoURL)
	if err != nil {
		return "", "", err
	}
	baseURL = fmt.Sprintf("%s://%s", u.Scheme, u.Host)
	path := strings.Trim(u.Path, "/")
	projectPath = url.QueryEscape(path)
	return baseURL, projectPath, nil
}

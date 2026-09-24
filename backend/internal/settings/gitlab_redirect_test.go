package settings

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// TestRotateWebhookDoesNotFollowRedirectWithPAT (SU-8): Go's stdlib does not strip
// the custom Private-Token header on a cross-host redirect, so a GitLab endpoint
// that 3xx-redirects to an attacker host would leak the PAT. RotateWebhookSecret's
// REST client sets CheckRedirect to ErrUseLastResponse, so the PAT is not delivered
// to the redirect target.
func TestRotateWebhookDoesNotFollowRedirectWithPAT(t *testing.T) {
	// Keep the rotation path off the env-pinned early return.
	t.Setenv("AMADEUS_GITLAB_WEBHOOK_SECRET", "")
	t.Setenv("AMADEUS_GITLAB_TOKEN", "")

	var attackerHits atomic.Int32
	var attackerGotPAT atomic.Bool
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits.Add(1)
		if r.Header.Get("Private-Token") != "" {
			attackerGotPAT.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(attacker.Close)

	// The GitLab REST endpoint redirects to the attacker host.
	gitlab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/steal", http.StatusFound)
	}))
	t.Cleanup(gitlab.Close)

	pool := openTestPool(t)
	ctx := context.Background()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	// Allow loopback so the guarded REST client can reach the httptest servers; the
	// SU-8 redirect refusal (not the SU-7 dial guard) is what this test exercises.
	cfg := &config.Config{
		SecretKEKEnv:          base64.StdEncoding.EncodeToString(kek),
		OutboundAllowPrivate:  true,
		OutboundAllowLoopback: true,
	}

	if _, err := UpdateGitlabConfig(ctx, pool, cfg, GitlabConfig{
		Pat:         "super-secret-pat",
		BotName:     "amadeus-bot",
		BotEmail:    "bot@example.com",
		WriteBranch: "main",
		RepoUrl:     gitlab.URL + "/org/repo.git",
	}, "tester"); err != nil {
		t.Fatalf("update gitlab config: %v", err)
	}

	// The rotation's GitLab call hits a 302; it will error, but the PAT must never
	// reach the redirect target.
	_, _, _, _ = RotateWebhookSecret(ctx, pool, cfg, true, 10, "tester")

	if attackerGotPAT.Load() {
		t.Errorf("Private-Token was leaked to the redirect target")
	}
	if hits := attackerHits.Load(); hits != 0 {
		t.Errorf("client followed the redirect to the attacker host (%d hits); CheckRedirect must stop it", hits)
	}
}

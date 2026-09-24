package settings

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// TestRotateWebhookRefusesMetadataTarget (SU-7): the guarded GitLab REST client
// refuses a rotation whose configured host is the cloud-metadata address, proving
// the SSRF guard is wired into this outbound path (not just the dialer). Metadata
// is blocked regardless of the private/loopback posture.
func TestRotateWebhookRefusesMetadataTarget(t *testing.T) {
	t.Setenv("CRONOMICON_GITLAB_WEBHOOK_SECRET", "")
	t.Setenv("CRONOMICON_GITLAB_TOKEN", "")

	pool := openTestPool(t)
	ctx := context.Background()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	// Default posture: private allowed, loopback/metadata blocked.
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek), OutboundAllowPrivate: true}

	if _, err := UpdateGitlabConfig(ctx, pool, cfg, GitlabConfig{
		Pat:         "super-secret-pat",
		BotName:     "amadeus-bot",
		BotEmail:    "bot@example.com",
		WriteBranch: "main",
		RepoUrl:     "https://169.254.169.254/org/repo.git",
	}, "tester"); err != nil {
		t.Fatalf("update gitlab config: %v", err)
	}

	_, _, _, err := RotateWebhookSecret(ctx, pool, cfg, true, 10, "tester")
	if err == nil {
		t.Fatal("expected rotation to fail on a cloud-metadata SSRF target")
	}
	// The dial guard refuses it; the REST path surfaces that as unreachable.
	if !errors.Is(err, ErrGitlabUnreachable) {
		t.Errorf("want ErrGitlabUnreachable (dial refused by SSRF guard), got %v", err)
	}
}

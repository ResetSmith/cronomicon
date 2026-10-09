package settings

// Phase R0 of 2.4.0 (GR, a repository per agency): today's behaviour, pinned.
// The test passes on the 2.3.2 code and names the phase of 2.4.0 that inverts
// it. No production code changes.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/repoid"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// Saving the GitLab connection without typing a new token replaces the stored
// token with its own mask. The read returns the token masked in `pat`; the
// form keeps what the read returned and sends all of it back, adding a token
// only when one was typed (frontend/src/views/settings/Integrations.tsx, the
// SaveBtn of GitlabForm); and the write encrypts whatever non-empty `pat` it
// is handed. Nothing shows it at the time: the mask of the mask is the mask,
// and the running sync service keeps the token it read at start-up. The next
// start authenticates with eight characters of bullets and a suffix.
//
// Phase R2 inverts this, and must: it makes the connection a row per
// repository whose service restarts on a connection write (GR-11), so the
// broken token would be in use the moment the form is saved.
func TestGR0_SavingTheConnectionBackStoresTheMaskAsTheToken(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek)}

	const token = "glpat-a-real-token-1234"
	if _, err := UpdateGitlabConfig(ctx, pool, cfg, repoid.Global, GitlabConfig{
		Pat: token, WriteBranch: "main", RepoUrl: "https://gitlab.example.com/org/repo.git",
	}, "tester"); err != nil {
		t.Fatal(err)
	}
	stored := func() string {
		t.Helper()
		var enc string
		if err := pool.QueryRowContext(ctx, `SELECT token_enc FROM git_repos WHERE id = 'global'`).Scan(&enc); err != nil {
			t.Fatalf("read token_enc: %v", err)
		}
		plain, err := secrets.DecryptString(cfg, enc)
		if err != nil {
			t.Fatalf("decrypt token_enc: %v", err)
		}
		return plain
	}
	if got := stored(); got != token {
		t.Fatalf("the token as first stored = %q, want %q", got, token)
	}

	// What the form holds, sent back unchanged: no new token was typed.
	form, err := GetGitlabConfig(ctx, pool, cfg, repoid.Global)
	if err != nil {
		t.Fatal(err)
	}
	if form.Pat == "" || form.Pat == token {
		t.Fatalf("the read should return the token masked, got %q", form.Pat)
	}
	if _, err := UpdateGitlabConfig(ctx, pool, cfg, repoid.Global, *form, "tester"); err != nil {
		t.Fatal(err)
	}

	switch got := stored(); got {
	case form.Pat:
		// Today: the mask is now the token.
	case token:
		t.Errorf("the token survived a save of the unchanged form: this is fixed, and the test is to be inverted (Phase R2)")
	default:
		t.Errorf("the stored token is %q: neither the token nor its mask %q", got, form.Pat)
	}
}

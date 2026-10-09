package settings

// Phase R0 of 2.4.0 (GR, a repository per agency) pinned what saving the
// connection form did to the stored token
// (TestGR0_SavingTheConnectionBackStoresTheMaskAsTheToken). Phase R2 fixed it,
// before the commit that makes a save restart the repository's sync service,
// and the tests here say what is true instead.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/repoid"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// Saving the GitLab connection without typing a new token keeps the stored
// token.
//
// Until Phase R2 (present defect 1) it replaced the token with its own mask.
// The read returns the token masked in `pat`; the form keeps what the read
// returned and sends all of it back, adding a token only when one was typed
// (frontend/src/views/settings/Integrations.tsx, the SaveBtn of GitlabForm);
// and the write encrypted whatever non-empty `pat` it was handed. Nothing
// showed it at the time: the mask of the mask is the mask, and the running
// sync service kept the token it had read at start-up. The next start
// authenticated with four bullets and a suffix. Since R2 a save restarts the
// service (GR-11), so the broken token would have been in use at once.
func TestGR2_SavingTheConnectionBackKeepsTheToken(t *testing.T) {
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
		if err := pool.QueryRowContext(ctx, `SELECT COALESCE(token_enc, '') FROM git_repos WHERE id = 'global'`).Scan(&enc); err != nil {
			t.Fatalf("read token_enc: %v", err)
		}
		if enc == "" {
			return ""
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
	form.WriteBranch = "release"
	saved, err := UpdateGitlabConfig(ctx, pool, cfg, repoid.Global, *form, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if got := stored(); got != token {
		t.Errorf("after a save of the form as read, the stored token is %q, want the token kept (%q)", got, token)
	}
	if saved.WriteBranch != "release" || !saved.PatSet || saved.Pat != form.Pat {
		t.Errorf("the save's answer: %+v; want the rest of the form saved and the same mask", saved)
	}
	// And again: a second save of what the first one answered.
	if _, err := UpdateGitlabConfig(ctx, pool, cfg, repoid.Global, *saved, "tester"); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got != token {
		t.Errorf("after a second save, the stored token is %q", got)
	}

	// A token that IS typed replaces the stored one, as before.
	form.Pat = "glpat-the-next-token-5678"
	if _, err := UpdateGitlabConfig(ctx, pool, cfg, repoid.Global, *form, "tester"); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got != "glpat-the-next-token-5678" {
		t.Errorf("a typed token was not stored: %q", got)
	}
	// An omitted token keeps the stored one, as before.
	form.Pat = ""
	if _, err := UpdateGitlabConfig(ctx, pool, cfg, repoid.Global, *form, "tester"); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got != "glpat-the-next-token-5678" {
		t.Errorf("an omitted token did not keep the stored one: %q", got)
	}
}

// The mask of a token that is NOT the stored one is still a mask. With the
// token set by the environment the read returns the environment's token,
// masked; a save of that form used to write the mask into the row, where it
// waited for the day the environment variable was removed.
func TestGR2_AMaskIsNeverStoredAsAToken(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek)}
	t.Setenv("CRONOMICON_GITLAB_TOKEN", "env-token-abcd")

	form, err := GetGitlabConfig(ctx, pool, cfg, repoid.Global)
	if err != nil {
		t.Fatal(err)
	}
	if form.Pat != maskSecret("env-token-abcd") {
		t.Fatalf("the read should return the environment's token masked, got %q", form.Pat)
	}
	if _, err := UpdateGitlabConfig(ctx, pool, cfg, repoid.Global, *form, "tester"); err != nil {
		t.Fatal(err)
	}
	var enc string
	if err := pool.QueryRowContext(ctx, `SELECT COALESCE(token_enc, '') FROM git_repos WHERE id = 'global'`).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if enc != "" {
		plain, _ := secrets.DecryptString(cfg, enc)
		t.Errorf("a save of the form stored %q as the repository's token; want nothing stored (the token is the environment's)", plain)
	}

	for val, want := range map[string]bool{
		"":                       false,
		"glpat-a-real-token":     false,
		"••••":                   true,
		"••••1234":               true,
		maskSecret("anything"):   true,
		maskSecret("abc"):        true,
		"•••a-three-bullet-name": false,
	} {
		if got := isMaskedSecret(val); got != want {
			t.Errorf("isMaskedSecret(%q) = %v, want %v", val, got, want)
		}
	}
}

// The observability bearer token has the same form and had the same defect:
// saving the settings without typing a token stored the mask as the token, and
// every scraper holding the real one was locked out at the next save.
func TestGR2_SavingObservabilityBackKeepsTheBearerToken(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek)}

	const token = "scrape-bearer-token-9876"
	if _, err := UpdateObservabilityConfig(ctx, pool, cfg, ObservabilityConfig{
		Enabled: true, Path: "/metrics", AuthType: "bearer", BearerToken: token,
	}, "tester"); err != nil {
		t.Fatal(err)
	}
	stored := func() string {
		t.Helper()
		var enc string
		if err := pool.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'obs.bearerTokenEnc'`).Scan(&enc); err != nil {
			t.Fatalf("read the bearer token: %v", err)
		}
		plain, err := secrets.DecryptString(cfg, enc)
		if err != nil {
			t.Fatalf("decrypt the bearer token: %v", err)
		}
		return plain
	}
	form, err := GetObservabilityConfig(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if form.BearerToken == "" || form.BearerToken == token {
		t.Fatalf("the read should return the token masked, got %q", form.BearerToken)
	}
	form.Path = "/prom"
	if _, err := UpdateObservabilityConfig(ctx, pool, cfg, *form, "tester"); err != nil {
		t.Fatalf("save the form as read: %v", err)
	}
	if got := stored(); got != token {
		t.Errorf("after a save of the form as read, the stored bearer token is %q, want the token kept (%q)", got, token)
	}
	// A mask with NO stored token is not a token either: bearer auth with
	// nothing behind it is refused, as an omitted token is.
	if _, err := pool.ExecContext(ctx, `DELETE FROM settings WHERE key = 'obs.bearerTokenEnc'`); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateObservabilityConfig(ctx, pool, cfg, *form, "tester"); err == nil {
		t.Errorf("bearer auth was saved with only a mask for a token")
	}
}

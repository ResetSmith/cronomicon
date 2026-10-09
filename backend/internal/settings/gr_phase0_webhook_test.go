package settings

// Phase R0 of 2.4.0 read these two about the webhook secret and did not run
// them. They were reproduced here (as TestGR0_…, in c630401) before Phase R2
// changed anything. R2's first commit fixed both, and each test now says what
// is true instead, with what it used to pin in its comment.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/repoid"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

func countRows(t *testing.T, pool *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func grWebhookCfg() *config.Config {
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	return &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek), SecretKEKVersion: 1}
}

// Rotating the webhook secret changes the secret and nothing else: a webhook
// that was on is on afterwards, on an installation whose connection was never
// saved (configured by environment alone) as on any other.
//
// Until Phase R2 (present defect 16) such an installation had no row for its
// connection, the policy read "no row" as enabled, and the first rotation
// wrote the row naming only the secret columns, so the webhook flags took
// their column default, 0: every delivery was refused from then on. Global's
// row is written by migration 1310 on every installation, with the webhook
// on, and the rotation updates three columns of it.
func TestGR2_RotatingTheSecretLeavesTheWebhookOn(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	cfg := grWebhookCfg()
	on := func(p WebhookPolicy) bool {
		return p.Enabled && p.Accepts("Push Hook") && p.Accepts("Tag Push Hook") && p.Accepts("Merge Request Hook")
	}

	if p := GetWebhookPolicy(ctx, pool, repoid.Global); !on(p) {
		t.Fatalf("with no connection saved the webhook is expected to be on, got %+v", p)
	}
	if _, _, _, err := RotateWebhookSecret(ctx, pool, cfg, repoid.Global, false, 10, "tester"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if p := GetWebhookPolicy(ctx, pool, repoid.Global); !on(p) {
		t.Errorf("after the first rotation the policy is %+v, want the webhook on with every event", p)
	}
	// The same with the row gone altogether. (It cannot be, since 1310; the
	// writer does not depend on that.)
	if _, err := pool.ExecContext(ctx, `DELETE FROM git_repos`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := RotateWebhookSecret(ctx, pool, cfg, repoid.Global, false, 10, "tester"); err != nil {
		t.Fatalf("rotate with no row: %v", err)
	}
	if p := GetWebhookPolicy(ctx, pool, repoid.Global); !on(p) {
		t.Errorf("after a rotation that had to write the row, the policy is %+v, want the webhook on", p)
	}
	// An operator who turned the webhook OFF keeps it off through a rotation.
	if _, err := pool.ExecContext(ctx, `UPDATE git_repos SET webhook_enabled = 0 WHERE id = 'global'`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := RotateWebhookSecret(ctx, pool, cfg, repoid.Global, false, 10, "tester"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if p := GetWebhookPolicy(ctx, pool, repoid.Global); p.Enabled {
		t.Errorf("a rotation switched a disabled webhook on")
	}
	// A repository that does not exist is not created by rotating its secret.
	if _, _, _, err := RotateWebhookSecret(ctx, pool, cfg, "no-such-repository", false, 10, "tester"); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("rotating the secret of a repository that does not exist: %v, want ErrRepoNotFound", err)
	}
	if n := countRows(t, pool, `SELECT COUNT(*) FROM git_repos`); n != 1 {
		t.Errorf("git_repos rows = %d, want Global's alone", n)
	}
}

// A rotation keeps the previous webhook secret for an overlap, during which it
// is still accepted, and for as long as it is stored it is masked wherever it
// is echoed.
//
// Until Phase R2 (present defect 18) it was in neither the redaction
// dictionary nor `rewrap-secrets`: both read one list of encrypted settings
// columns, and the previous secret's column was not on it.
func TestGR2_ThePreviousWebhookSecretIsMasked(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	cfg := grWebhookCfg()

	first, _, _, err := RotateWebhookSecret(ctx, pool, cfg, repoid.Global, false, 10, "tester")
	if err != nil {
		t.Fatalf("first rotation: %v", err)
	}
	second, _, _, err := RotateWebhookSecret(ctx, pool, cfg, repoid.Global, false, 10, "tester")
	if err != nil {
		t.Fatalf("second rotation: %v", err)
	}
	values, _, err := secrets.RedactionReport(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(values, "\n")
	if !strings.Contains(joined, second) {
		t.Fatalf("the current webhook secret is not in the redaction dictionary")
	}
	if !strings.Contains(joined, first) {
		t.Errorf("the previous webhook secret, still accepted, is not in the redaction dictionary")
	}
}

// The connection functions take the repository they mean, and the environment
// overrides are Global's alone (GR-21): a second repository reads its own row.
func TestGR2_TheEnvironmentOverridesAreGlobalsAlone(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	cfg := grWebhookCfg()
	cfg.GitLabBaseURL = "https://git.example/global/defs.git"
	cfg.GitLabWriteBranch = "env-branch"
	t.Setenv("CRONOMICON_GITLAB_TOKEN", "env-token-for-global")
	t.Setenv("CRONOMICON_GITLAB_WEBHOOK_SECRET", "env-webhook-secret")

	enc, err := secrets.EncryptString(cfg, "agency-token-9876")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO git_repos (id, agency_id, url, branch, token_enc) VALUES ('r-fin', 'ag-fin', 'https://git.example/finance/defs.git', 'trunk', ?)`, enc); err != nil {
		t.Fatal(err)
	}

	if u, tok := ResolveGitlabRuntime(ctx, pool, cfg, repoid.Global); u != "https://git.example/global/defs.git" || tok != "env-token-for-global" {
		t.Errorf("Global's runtime = %q, %q; want the environment's", u, tok)
	}
	if u, tok := ResolveGitlabRuntime(ctx, pool, cfg, "r-fin"); u != "https://git.example/finance/defs.git" || tok != "agency-token-9876" {
		t.Errorf("the agency's repository's runtime = %q, %q; want its own row's, not the environment's", u, tok)
	}
	if u, tok := ResolveGitlabRuntime(ctx, pool, cfg, "no-such-repository"); u != "" || tok != "" {
		t.Errorf("a repository that does not exist resolved to %q, %q", u, tok)
	}

	g, err := GetGitlabConfig(ctx, pool, cfg, repoid.Global)
	if err != nil {
		t.Fatal(err)
	}
	if g.RepoUrl != "https://git.example/global/defs.git" || !g.WebhookSecretEnvPinned || g.Pat != maskSecret("env-token-for-global") {
		t.Errorf("Global's connection as read: %+v; want the environment's URL, token mask and pin", g)
	}
	a, err := GetGitlabConfig(ctx, pool, cfg, "r-fin")
	if err != nil {
		t.Fatal(err)
	}
	if a.RepoUrl != "https://git.example/finance/defs.git" || a.WriteBranch != "trunk" || a.WebhookSecretEnvPinned || a.Pat != maskSecret("agency-token-9876") {
		t.Errorf("the agency's connection as read: %+v; want its own row's, with no pin", a)
	}
	if _, err := GetGitlabConfig(ctx, pool, cfg, "no-such-repository"); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("reading a repository that does not exist: %v, want ErrRepoNotFound", err)
	}

	// The pin refuses a rotation of Global's secret, and of no other's.
	if _, _, _, err := RotateWebhookSecret(ctx, pool, cfg, repoid.Global, false, 10, "tester"); !errors.Is(err, ErrWebhookSecretEnvPinned) {
		t.Errorf("rotating Global's pinned secret: %v, want ErrWebhookSecretEnvPinned", err)
	}
	if _, _, _, err := RotateWebhookSecret(ctx, pool, cfg, "r-fin", false, 10, "tester"); err != nil {
		t.Errorf("rotating the agency's repository's secret: %v, want it rotated (the pin is Global's)", err)
	}
	if EnvBranch(cfg, repoid.Global) != "env-branch" || EnvBranch(cfg, "r-fin") != "" {
		t.Errorf("the branch override: Global %q, the agency %q; want it for Global alone", EnvBranch(cfg, repoid.Global), EnvBranch(cfg, "r-fin"))
	}

	// A write names its repository, and does not create one.
	if _, err := UpdateGitlabConfig(ctx, pool, cfg, "r-fin", GitlabConfig{WriteBranch: "release", RepoUrl: "https://git.example/finance/defs.git", WebhookEnabled: true}, "tester"); err != nil {
		t.Fatalf("update the agency's connection: %v", err)
	}
	if got := countRows(t, pool, `SELECT COUNT(*) FROM git_repos WHERE id='r-fin' AND branch='release' AND token_enc IS NOT NULL`); got != 1 {
		t.Errorf("the agency's row after its update: branch release with its token kept = %d rows, want 1", got)
	}
	if got := countRows(t, pool, `SELECT COUNT(*) FROM git_repos WHERE id='global' AND branch='release'`); got != 0 {
		t.Errorf("an update of the agency's connection wrote Global's row")
	}
	if _, err := UpdateGitlabConfig(ctx, pool, cfg, "no-such-repository", GitlabConfig{RepoUrl: "https://x"}, "tester"); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("updating a repository that does not exist: %v, want ErrRepoNotFound", err)
	}
}

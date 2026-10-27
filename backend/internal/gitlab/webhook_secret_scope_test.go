package gitlab

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/repoid"
)

// The environment's webhook secret (CRONOMICON_GITLAB_WEBHOOK_SECRET) and the
// one read at start-up open no repository's webhook since 2.5.3 (question
// 207): they configured Global's repository, and the built-in agency has none
// (CA-6). The registry starts no Service for it, an agency's Service is not
// handed them, and the environment's is not read even by a Service of
// Global's built by hand. A repository accepts its own row's secret only.
//
// Until 2.5.3 (TestWebhookSecretsOfTheEnvironmentAreGlobalsAlone) they opened
// Global's webhook and no other repository's (GR-21), the environment's
// pinning it.
func TestWebhookSecretsOfTheEnvironmentOpenNoRepository(t *testing.T) {
	db := mustOpenDB(t)
	if _, err := db.Exec(`INSERT INTO git_repos(id, agency_id) VALUES('repo-b', 'ag-b')`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t.Setenv("CRONOMICON_GITLAB_WEBHOOK_SECRET", "env-secret")

	// By hand, as before: the environment's secret is not read for any.
	global := &Service{db: db}
	other := &Service{db: db, repoID: "repo-b", agencyID: "ag-b"}
	gone := &Service{db: db, repoID: "no-such-repository"}
	for name, svc := range map[string]*Service{"Global's": global, "another repository's": other, "a repository with no row's": gone} {
		if svc.ValidateWebhookToken(ctx, "env-secret") {
			t.Errorf("%s webhook accepted the environment's secret", name)
		}
	}

	// The registry, given the secret read at start-up.
	reg := NewRegistry(db, slog.New(slog.NewTextHandler(io.Discard, nil)), &config.Config{WebhookSecret: "boot-secret"})
	clones := t.TempDir()
	reg.cloneDir = func(repoID string) string { return filepath.Join(clones, repoID) }
	t.Cleanup(reg.Close)
	if err := reg.Restart(ctx, repoid.Global); err != nil || reg.Service(repoid.Global) != nil {
		t.Fatalf("restart of Central's repository: %v; want no Service", err)
	}
	if err := reg.Restart(ctx, "repo-b"); err != nil {
		t.Fatal(err)
	}
	b := reg.Service("repo-b")
	if b == nil {
		t.Fatal("no Service for the agency's repository")
	}
	for _, secret := range []string{"boot-secret", "env-secret"} {
		if b.ValidateWebhookToken(ctx, secret) {
			t.Errorf("the agency's repository's webhook accepted %s", secret)
		}
	}

	// Delivered: every route answers as a wrong token does.
	h := NewRegistryHandlers(reg)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/webhooks/gitlab", h.WebhookGitLab)
	mux.HandleFunc("POST /api/v1/webhooks/gitlab/{repoId}", h.WebhookGitLab)
	for _, path := range []string{"/api/v1/webhooks/gitlab", "/api/v1/webhooks/gitlab/global", "/api/v1/webhooks/gitlab/repo-b", "/api/v1/webhooks/gitlab/no-such-repository"} {
		for _, secret := range []string{"boot-secret", "env-secret"} {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
			req.Header.Set("X-Gitlab-Token", secret)
			req.Header.Set("X-Gitlab-Event", "Push Hook")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("a delivery to %s with the %s = %d, want 401", path, secret, rec.Code)
			}
		}
	}
}

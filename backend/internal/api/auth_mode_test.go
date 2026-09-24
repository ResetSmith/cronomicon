package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/api"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// serverForMode boots an HTTP handler in the given auth mode against a fresh DB.
func serverForMode(t *testing.T, cfg *config.Config) http.Handler {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "mode.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := auth.NewService(context.Background(), cfg, pool, log)
	return api.New(api.Options{Config: cfg, Logger: log, Auth: authSvc, DB: pool}).Handler()
}

// TestAuthModeRouteMounting verifies the OIDC relying-party endpoints are mounted
// only in oidc mode and absent in trusted-header mode (A.1/A.10).
func TestAuthModeRouteMounting(t *testing.T) {
	t.Run("trusted-header: OIDC routes absent", func(t *testing.T) {
		h := serverForMode(t, &config.Config{
			AuthMode:       config.AuthModeTrustedHeader,
			TrustedProxies: []string{"10.0.0.0/8"},
		})
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/auth/login", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("/auth/login in trusted-header mode: code=%d want 404 (not mounted)", rec.Code)
		}
	})

	t.Run("oidc: OIDC routes present", func(t *testing.T) {
		h := serverForMode(t, &config.Config{AuthMode: config.AuthModeOIDC})
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/auth/login", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		// Mounted, but OIDC is unconfigured here, so the handler returns 503 —
		// the point is that it is NOT a 404 (route exists).
		if rec.Code == http.StatusNotFound {
			t.Fatalf("/auth/login in oidc mode: got 404, route should be mounted")
		}
	})
}

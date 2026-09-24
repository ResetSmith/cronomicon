package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/web"
	"gopkg.in/yaml.v3"
)

// TestSpecRouteConformance asserts every operation in the frozen openapi.yaml
// has a mux route (E.6 drift guard). This is what "frozen spec" means in
// practice: the four /settings/* endpoints shipped months after the spec
// promised them, and nothing failed until a user opened the page.
//
// Route existence only — response-shape conformance is covered per-endpoint by
// the settings/integration tests.
func TestSpecRouteConformance(t *testing.T) {
	specPath := filepath.Join("..", "..", "openapi.yaml")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}

	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	if len(spec.Paths) == 0 {
		t.Fatal("spec has no paths — wrong file?")
	}

	// Build the real route table (no middleware — we only match patterns).
	pool, err := db.Open(filepath.Join(t.TempDir(), "conformance.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg := &config.Config{
		Addr: ":0", DevAuth: true,
		SecretKEKEnv: "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(Options{
		Config: cfg,
		Logger: log,
		Auth:   auth.NewService(context.Background(), cfg, pool, log),
		DB:     pool,
		WebFS:  web.DistFS(),
	})
	mux := srv.buildMux()

	// Known gaps: spec operations with NO backend implementation yet. Each is
	// tracked work (the endpoint-update plan follow-ups) — do NOT add to this
	// list to silence a failure; implement the endpoint instead. The test
	// fails if an entry here becomes routed, so the list can't go stale.
	knownGaps := map[string]bool{}

	paramRe := regexp.MustCompile(`\{[^}]+\}`)
	methods := []string{"get", "post", "put", "patch", "delete"}

	for specRoute, ops := range spec.Paths {
		for _, m := range methods {
			if _, ok := ops[m]; !ok {
				continue
			}
			op := strings.ToUpper(m) + " " + specRoute
			// Spec paths are relative to the /api/v1 server base; substitute
			// path params with a literal so the mux pattern can match.
			url := "/api/v1" + paramRe.ReplaceAllString(specRoute, "x")
			req := httptest.NewRequest(strings.ToUpper(m), url, nil)
			_, pattern := mux.Handler(req)
			if pattern == "" || pattern == "/" {
				// Health endpoints are served at root by design (T13), not
				// under the /api/v1 server base the spec declares.
				rootReq := httptest.NewRequest(strings.ToUpper(m), paramRe.ReplaceAllString(specRoute, "x"), nil)
				if _, rootPattern := mux.Handler(rootReq); rootPattern != "" && rootPattern != "/" {
					continue
				}
				if knownGaps[op] {
					t.Logf("known gap (tracked, unimplemented): %s", op)
					continue
				}
				// "/" is the SPA catch-all: the operation is not actually routed.
				t.Errorf("spec operation %s has no mux route", op)
			} else if knownGaps[op] {
				t.Errorf("%s is routed but still listed in knownGaps — remove the entry", op)
			}
		}
	}
}

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/api"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/web"
)

// tzReloadSpy captures invocations of the ScheduleTimezoneReload hook so the test
// can assert that a zone change reschedules with the right location.
type tzReloadSpy struct {
	mu    sync.Mutex
	calls []string
}

func (s *tzReloadSpy) hook(_ context.Context, loc *time.Location) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, loc.String())
	return nil
}

func (s *tzReloadSpy) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// newTimezoneTestServer builds a server wired with a timezone-reload spy.
func newTimezoneTestServer(t *testing.T) (*httptest.Server, *tzReloadSpy) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "tz.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg := &config.Config{
		Addr:                 ":0",
		CookieSecure:         false,
		RunnerBootstrapToken: "boot-token",
		DevAuth:              true,
		SecretKEKEnv:         "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := auth.NewService(context.Background(), cfg, pool, log)
	spy := &tzReloadSpy{}
	srv := api.New(api.Options{
		Config:                 cfg,
		Logger:                 log,
		Auth:                   authSvc,
		DB:                     pool,
		ReadyChecks:            []api.ReadyCheck{{Name: "database", Check: db.ReadyCheck(pool)}},
		WebFS:                  web.DistFS(),
		ScheduleTimezoneReload: spy.hook,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, spy
}

// TestGeneralSettingsTimezone covers the M1 API surface: GET surfaces the
// resolved appTimezone; PUT with a bad zone is a 422; PUT with a changed valid
// zone returns 200 AND fires the reschedule hook with that zone (timezone-update §8).
func TestGeneralSettingsTimezone(t *testing.T) {
	ts, spy := newTimezoneTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	do := func(method, path string, body any) *http.Response {
		t.Helper()
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rdr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	// GET surfaces appTimezone (default "UTC" on a fresh DB).
	resp := do(http.MethodGet, "/api/v1/settings/general", nil)
	var got struct {
		Timezone    string `json:"timezone"`
		AppTimezone string `json:"appTimezone"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d, want 200", resp.StatusCode)
	}
	if got.AppTimezone != "UTC" {
		t.Errorf("appTimezone = %q, want UTC", got.AppTimezone)
	}

	// PUT with a bad zone → 422, no reschedule.
	resp = do(http.MethodPut, "/api/v1/settings/general", map[string]any{"timezone": "Not/AZone"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT bad zone = %d, want 422", resp.StatusCode)
	}
	if n := len(spy.snapshot()); n != 0 {
		t.Errorf("bad zone fired reschedule %d times, want 0", n)
	}

	// PUT with a changed valid zone → 200 and one reschedule with that zone.
	resp = do(http.MethodPut, "/api/v1/settings/general", map[string]any{"timezone": "America/New_York"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT good zone = %d, want 200", resp.StatusCode)
	}
	calls := spy.snapshot()
	if len(calls) != 1 || calls[0] != "America/New_York" {
		t.Fatalf("reschedule calls = %v, want [America/New_York]", calls)
	}

	// GET reflects the new appTimezone.
	resp = do(http.MethodGet, "/api/v1/settings/general", nil)
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.AppTimezone != "America/New_York" {
		t.Errorf("appTimezone after update = %q, want America/New_York", got.AppTimezone)
	}

	// Saving the SAME zone again must NOT re-fire the hook (idempotent at the
	// handler: only a change triggers a reschedule).
	resp = do(http.MethodPut, "/api/v1/settings/general", map[string]any{"timezone": "America/New_York"})
	resp.Body.Close()
	if n := len(spy.snapshot()); n != 1 {
		t.Errorf("unchanged-zone save fired reschedule again (total %d, want 1)", n)
	}
}

package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/api"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/logarchive/fakes3"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"github.com/ResetSmith/cronomicon/web"
)

// fakeSyncer stands in for the logsync.Sweeper behind api.LogArchiveSyncer.
type fakeSyncer struct {
	inProgress atomic.Bool
	mu         sync.Mutex
	calls      []string // "actor|reconcile"
	done       chan struct{}
}

func (f *fakeSyncer) Sync(_ context.Context, reconcile bool, actor string) error {
	f.mu.Lock()
	r := "0"
	if reconcile {
		r = "1"
	}
	f.calls = append(f.calls, actor+"|"+r)
	f.mu.Unlock()
	select {
	case f.done <- struct{}{}:
	default:
	}
	return nil
}
func (f *fakeSyncer) InProgress() bool { return f.inProgress.Load() }

// newSyncTestServer builds a server with the fake syncer wired and, when s3 is
// non-nil, the S3 archive backend stored against that fake bucket so the mount
// builds a store.
func newSyncTestServer(t *testing.T, syncer api.LogArchiveSyncer, s3 *fakes3.Server) (*httptest.Server, *sql.DB) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Addr: ":0", CookieSecure: false, RunnerBootstrapToken: "boot-token", DevAuth: true,
		SecretKEKEnv:          "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
		RunnerOfflineAfter:    5 * time.Minute,
		RunnerDeregisterAfter: 336 * time.Hour,
		OutboundAllowPrivate:  true, OutboundAllowLoopback: true,
	}
	if s3 != nil {
		useSSL := false
		if _, err := settings.UpdateLogStorageConfig(context.Background(), pool, cfg, settings.LogStorageConfig{
			Backend: "s3",
			Local:   &settings.LocalLogConfig{Path: t.TempDir()},
			S3: &settings.S3LogConfig{Endpoint: s3.Endpoint(), Bucket: "logs", Region: "us-east-1",
				AccessKey: "AK", SecretKey: "SK", Prefix: "amadeus/", UseSSL: &useSSL},
		}, "seed"); err != nil {
			t.Fatalf("seed s3 settings: %v", err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := api.New(api.Options{
		Config: cfg, Logger: log, Auth: auth.NewService(context.Background(), cfg, pool, log), DB: pool,
		ReadyChecks:    []api.ReadyCheck{{Name: "database", Check: db.ReadyCheck(pool)}},
		WebFS:          web.DistFS(),
		LogArchiveSync: syncer,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, pool
}

func postSync(t *testing.T, client *http.Client, ts *httptest.Server, csrf, query string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/settings/log-storage/sync"+query, nil)
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, body
}

func TestLogStorageSyncNow(t *testing.T) {
	fake := fakes3.New("logs")
	defer fake.Close()
	syncer := &fakeSyncer{done: make(chan struct{}, 1)}
	ts, _ := newSyncTestServer(t, syncer, fake)
	client, csrf := devLoginWithCSRF(t, ts)

	// 202: the tick is started in the background with the session actor, and
	// the response reports inProgress.
	resp, body := postSync(t, client, ts, csrf, "?reconcile=1")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST sync = %d, body %s", resp.StatusCode, body)
	}
	var cfg settings.LogStorageConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Archive == nil || !cfg.Archive.InProgress {
		t.Fatalf("202 body should report inProgress: %+v", cfg.Archive)
	}
	select {
	case <-syncer.done:
	case <-time.After(3 * time.Second):
		t.Fatal("syncer was not called")
	}
	syncer.mu.Lock()
	calls := append([]string(nil), syncer.calls...)
	syncer.mu.Unlock()
	if len(calls) != 1 || calls[0] == "|1" || calls[0][len(calls[0])-2:] != "|1" {
		t.Fatalf("calls = %v, want one call with the session actor and reconcile", calls)
	}

	// 409 while a tick runs; GET overlays the flag.
	syncer.inProgress.Store(true)
	resp, body = postSync(t, client, ts, csrf, "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("POST sync during tick = %d, body %s", resp.StatusCode, body)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/settings/log-storage", nil)
	r, _ := client.Do(req)
	var got settings.LogStorageConfig
	json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	if got.Archive == nil || !got.Archive.InProgress {
		t.Fatalf("GET should overlay inProgress from the sweep: %+v", got.Archive)
	}
	syncer.inProgress.Store(false)
}

func TestLogStorageSyncNowRefusals(t *testing.T) {
	// 422 when the backend is local (no store).
	syncer := &fakeSyncer{done: make(chan struct{}, 1)}
	ts, _ := newSyncTestServer(t, syncer, nil)
	client, csrf := devLoginWithCSRF(t, ts)
	resp, body := postSync(t, client, ts, csrf, "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST sync on local = %d, body %s", resp.StatusCode, body)
	}

	// 503 when no sweep is wired.
	fake := fakes3.New("logs")
	defer fake.Close()
	ts2, _ := newSyncTestServer(t, nil, fake)
	client2, csrf2 := devLoginWithCSRF(t, ts2)
	resp, body = postSync(t, client2, ts2, csrf2, "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("POST sync without sweep = %d, body %s", resp.StatusCode, body)
	}

	// 401 without a session.
	resp, _ = postSync(t, http.DefaultClient, ts2, "", "")
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST sync unauthenticated = %d", resp.StatusCode)
	}
}

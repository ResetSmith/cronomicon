package gitlab

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// F2-3 (the visual-update-followups plan) — the Webhook Enabled and
// Webhook Events settings now decide whether a delivery is acted on. Before
// this they were parsed, persisted, shown in Settings → Integrations and read by
// nothing, so the handler synced whatever they said.
//
// One case per branch, because "the setting now does something" is exactly the
// claim that regresses silently: a later refactor that drops the policy read
// puts the app back to where it was, with every test still green unless the
// branches are pinned here.

// The four columns under test, on Global's row of git_repos (migration 1310;
// they were gitlab_config's). The package's mustOpenDB writes that row with the
// columns at their default, on.
func withGitlabConfig(t *testing.T, pool *sql.DB, enabled, push, mr, tag int) {
	t.Helper()
	if _, err := pool.Exec(`
		UPDATE git_repos SET webhook_enabled=?, webhook_events_push=?, webhook_events_mr=?, webhook_events_tag=?
		 WHERE id='global'`,
		enabled, push, mr, tag); err != nil {
		t.Fatalf("seed the webhook policy: %v", err)
	}
}

// A service with NO repo URL: cloneOrFetch then fails immediately with "not
// configured", touching no network, and records a git_sync_events row on the way
// out. That row is the observable "a sync was actually triggered" — the thing
// these branches are really about, since every case below returns a 2xx/4xx that
// on its own cannot tell a sync from a no-op.
func webhookSvc(t *testing.T, pool *sql.DB) *Handlers {
	t.Helper()
	return NewHandlers(&Service{
		db:            pool,
		cloneDir:      filepath.Join(t.TempDir(), "clone"),
		webhookSecret: "correct-secret",
	})
}

func syncCount(t *testing.T, pool *sql.DB) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM git_sync_events`).Scan(&n); err != nil {
		t.Fatalf("count git_sync_events: %v", err)
	}
	return n
}

// waitSync waits for an accepted delivery's asynchronous sync to record itself
// AND for the service to go idle, so the next delivery cannot be swallowed by
// TriggerSync's in-flight coalescing guard.
func waitSync(t *testing.T, h *Handlers, pool *sql.DB, want int) {
	t.Helper()
	for range 200 {
		h.svc.mu.Lock()
		idle := !h.svc.syncing
		h.svc.mu.Unlock()
		if idle && syncCount(t, pool) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d recorded syncs (have %d)", want, syncCount(t, pool))
}

func deliver(t *testing.T, h *Handlers, event string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", strings.NewReader(`{}`))
	req.Header.Set("X-Gitlab-Token", "correct-secret")
	if event != "" {
		req.Header.Set("X-Gitlab-Event", event)
	}
	rec := httptest.NewRecorder()
	h.WebhookGitLab(rec, req)
	return rec
}

func TestWebhookGitLab_DisabledRefuses(t *testing.T) {
	pool := mustOpenDB(t)
	withGitlabConfig(t, pool, 0, 1, 1, 1) // events all on, master switch off
	h := webhookSvc(t, pool)

	rec := deliver(t, h, "Push Hook")
	if rec.Code != http.StatusForbidden {
		t.Errorf("disabled webhook: want 403, got %d", rec.Code)
	}
	// The body has to say why, because GitLab's delivery log is where an
	// operator debugging "my push did not sync" will read it.
	if !strings.Contains(rec.Body.String(), "webhook_disabled") {
		t.Errorf("disabled webhook: body = %q, want a webhook_disabled code", rec.Body.String())
	}
}

func TestWebhookGitLab_DisabledStillRejectsABadTokenFirst(t *testing.T) {
	pool := mustOpenDB(t)
	withGitlabConfig(t, pool, 0, 1, 1, 1)
	h := webhookSvc(t, pool)

	// An unauthenticated caller must not be able to tell a disabled install from
	// a wrong secret — the token check comes first for that reason.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", strings.NewReader(`{}`))
	req.Header.Set("X-Gitlab-Token", "wrong-secret")
	rec := httptest.NewRecorder()
	h.WebhookGitLab(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token on a disabled webhook: want 401, got %d", rec.Code)
	}
}

func TestWebhookGitLab_EnabledEventFiltering(t *testing.T) {
	pool := mustOpenDB(t)
	// Enabled; push and tag accepted, merge requests NOT.
	withGitlabConfig(t, pool, 1, 1, 0, 1)
	h := webhookSvc(t, pool)

	// Every case returns 202 — an unselected event is not a delivery error. What
	// differs is whether a sync ran, which the git_sync_events row records.
	synced := 0
	for _, tc := range []struct {
		event     string
		wantSync  bool
		rationale string
	}{
		{"Push Hook", true, "push is on"},
		{"Tag Push Hook", true, "tag is on"},
		{"Merge Request Hook", false, "mr is off — the flag now means something"},
		{"Issue Hook", false, "no flag covers it, so it is not a resync trigger"},
		{"", true, "no header: our own tooling / a curl smoke-test, gated by push"},
	} {
		rec := deliver(t, h, tc.event)
		if rec.Code != http.StatusAccepted {
			t.Errorf("event %q (%s): want 202, got %d", tc.event, tc.rationale, rec.Code)
		}
		if tc.wantSync {
			synced++
			waitSync(t, h, pool, synced)
			continue
		}
		// No goroutine was started, so this is not a race: a branch that did not
		// call TriggerSync can never record a row later.
		if got := syncCount(t, pool); got != synced {
			t.Errorf("event %q (%s): recorded %d syncs, want %d — the flag did not gate it", tc.event, tc.rationale, got, synced)
		}
	}
}

func TestWebhookGitLab_FailsOpenWithoutConfig(t *testing.T) {
	// With no row for the repository, and then with no table at all, the policy
	// read finds nothing or errors. It must fail OPEN: these flags were never
	// enforced, so a missing row or a transient DB error must not be the thing
	// that stops a repo syncing.
	pool := mustOpenDB(t)
	h := webhookSvc(t, pool)

	if _, err := pool.Exec(`DELETE FROM git_repos`); err != nil {
		t.Fatal(err)
	}
	if rec := deliver(t, h, "Push Hook"); rec.Code != http.StatusAccepted {
		t.Errorf("no row for the repository: want 202 (fail open), got %d", rec.Code)
	}
	if _, err := pool.Exec(`DROP TABLE git_repos`); err != nil {
		t.Fatal(err)
	}
	if rec := deliver(t, h, "Push Hook"); rec.Code != http.StatusAccepted {
		t.Errorf("no git_repos table: want 202 (fail open), got %d", rec.Code)
	}
}

package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A publish refused because the branch has commits the last sync does not have
// starts a sync (2.4.0, with present defect 32's fix). Nothing polls a
// repository on a timer, and whoever publishes may not be somebody who can
// start a sync: without this the retry the answer asks for would be refused
// for the same reason until a webhook delivery or an administrator's sync. The
// answer names the branch's head on the remote, which is the base the retry is
// to carry (the publish form's "reload" takes it from there), and the retry
// goes through once the sync has run.
func TestAPublishRefusedForUnsyncedCommitsStartsASync(t *testing.T) {
	svc, bare, branch := bareFixture(t)
	ctx := context.Background()
	synced := svc.SyncBlocking(ctx, "manual")
	if synced.Status == "failed" {
		t.Fatalf("sync: %s", synced.ErrorMessage)
	}
	work := t.TempDir()
	sysGit(t, work, "clone", "-q", "--branch", branch, bare, ".")
	sysGit(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "a colleague's commit")
	sysGit(t, work, "push", "-q", "origin", branch)
	colleague := sysGit(t, bare, "rev-parse", "refs/heads/"+branch)

	h := NewHandlers(svc)
	publish := func(base string) (int, map[string]any) {
		t.Helper()
		// It names the repository: a publish that names none was Global's, which
		// has no repository since 2.5.3 (CA-6).
		body := `{"repoId":"` + fixtureRepo + `","filePath":"jobs/from-the-app.yaml","content":` + string(mustJSON(t, grScopedJob("from-the-app", fixtureScope))) + `}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/schedules/publish", strings.NewReader(body))
		req.Header.Set("If-Match", base)
		rec := httptest.NewRecorder()
		h.PublishSchedule("a@example.com", nil, rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, out := publish(synced.SHA)
	if code != http.StatusPreconditionFailed || out["code"] != "base_sha_mismatch" {
		t.Fatalf("the publish answered %d %v, want 412 base_sha_mismatch", code, out)
	}
	if out["currentSha"] != colleague {
		t.Errorf("the answer's currentSha is %v, want the branch's head on the remote, %s", out["currentSha"], colleague)
	}
	if msg, _ := out["message"].(string); !strings.Contains(msg, "A sync has been started") {
		t.Errorf("the answer does not say that a sync was started: %q", msg)
	}
	// The sync it started brings the clone up, with nobody asking for one.
	deadline := time.Now().Add(30 * time.Second)
	for sysGit(t, svc.cloneDir, "rev-parse", "HEAD") != colleague {
		if time.Now().After(deadline) {
			t.Fatalf("the clone is still at %s: the refusal started no sync", sysGit(t, svc.cloneDir, "rev-parse", "HEAD"))
		}
		time.Sleep(50 * time.Millisecond)
	}
	svc.wg.Wait()

	// The retry, with the base the answer named.
	code, out = publish(colleague)
	if code != http.StatusCreated {
		t.Fatalf("the retry answered %d %v, want 201", code, out)
	}
	if parent := sysGit(t, bare, "log", "--format=%P", "-1", "refs/heads/"+branch); parent != colleague {
		t.Errorf("the published commit's parent is %s, want the colleague's commit %s", parent, colleague)
	}

	// A base that is merely stale, with the clone level with the remote,
	// starts no sync and says what it always said.
	events := func() int {
		var n int
		if err := svc.db.QueryRow(`SELECT COUNT(*) FROM git_sync_events`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := events()
	code, out = publish(colleague)
	if code != http.StatusPreconditionFailed {
		t.Fatalf("a publish on a stale base answered %d %v, want 412", code, out)
	}
	if msg, _ := out["message"].(string); strings.Contains(msg, "A sync has been started") {
		t.Errorf("a stale base with nothing unsynced is told a sync was started: %q", msg)
	}
	svc.wg.Wait()
	if after := events(); after != before {
		t.Errorf("a stale base with nothing unsynced started a sync (%d sync events, was %d)", after, before)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

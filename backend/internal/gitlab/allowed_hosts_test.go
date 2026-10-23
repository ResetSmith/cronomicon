package gitlab

import (
	"context"
	"strings"
	"testing"
)

// GR-10 is asked at every sync and publish, not only when a connection is
// written: an agency's repository on a host that is not allowed (never was,
// or was taken off the list since) is not fetched and not pushed to, and the
// sync's failure says why. Global's repository is exempt.
func TestASyncAndAPublishAreHeldToTheAllowedHosts(t *testing.T) {
	a, _, _ := newSyncFixture(t)
	ctx := context.Background()
	if r := a.SyncBlocking(ctx, "manual"); r.Status == "failed" {
		t.Fatalf("Global's sync, from a path on disk, which the list does not govern: %s", r.ErrorMessage)
	}
	if _, err := a.db.Exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-b', 'B', 't')`); err != nil {
		t.Fatal(err)
	}
	b := &Service{db: a.db, log: a.log, repoID: "repo-b", agencyID: "ag-b", cloneDir: t.TempDir() + "/clone", Cfg: a.Cfg}
	allow := func(hosts string) {
		t.Helper()
		if _, err := a.db.Exec(`INSERT INTO settings (key, value, last_modified_by, last_modified_at) VALUES ('gitAllowedHosts', ?, 't', 't')
		                        ON CONFLICT(key) DO UPDATE SET value = excluded.value`, hosts); err != nil {
			t.Fatal(err)
		}
	}
	sync := func(url string) string {
		t.Helper()
		b.repoURL = url
		r := b.SyncBlocking(ctx, "manual")
		if r.Status != "failed" {
			t.Fatalf("a sync of %s: %s, want failed (nothing answers there)", url, r.Status)
		}
		return r.ErrorMessage
	}

	// Nothing is listed.
	if msg := sync("http://127.0.0.1:1/b.git"); !strings.Contains(msg, "no host is allowed") {
		t.Errorf("with nothing listed the sync failed with %q, want the list's refusal", msg)
	}
	// Another host is listed.
	allow(`["git.example.com"]`)
	if msg := sync("http://127.0.0.1:1/b.git"); !strings.Contains(msg, "127.0.0.1:1 is not on the list") {
		t.Errorf("with another host listed the sync failed with %q, want the list's refusal", msg)
	}
	// A scheme the guard does not cover, on a listed host.
	if msg := sync("ssh://git@git.example.com/b.git"); !strings.Contains(msg, "ssh:// is not accepted") {
		t.Errorf("an ssh URL failed with %q, want the scheme's refusal", msg)
	}
	if msg := sync("file:///etc"); !strings.Contains(msg, "is not accepted") && !strings.Contains(msg, "https://") {
		t.Errorf("a file URL failed with %q, want the scheme's refusal", msg)
	}
	// Its host is listed: the list lets it through, and it fails as a fetch
	// that nothing answers (the outbound guard, or the dial).
	allow(`["127.0.0.1:1"]`)
	if msg := sync("http://127.0.0.1:1/b.git"); strings.Contains(msg, "gitAllowedHosts") || strings.Contains(msg, "not accepted") {
		t.Errorf("with its host listed the sync was still refused by the list: %q", msg)
	}
	// Taken off the list again: the next sync is refused, with nothing saved anew.
	allow(`[]`)
	if msg := sync("http://127.0.0.1:1/b.git"); !strings.Contains(msg, "no host is allowed") {
		t.Errorf("after its host was taken off the list the sync failed with %q, want the list's refusal", msg)
	}

	// A publish is held to it too, before anything is written.
	b.repoURL = "http://127.0.0.1:1/b.git"
	_, err := b.Publish(ctx, PublishRequest{FilePath: "jobs/p.yaml", Content: jobYAML("p")}, "0000", "a@example.com")
	if err == nil || !strings.Contains(err.Error(), "no host is allowed") {
		t.Errorf("a publish to a host that is not allowed answered %v, want the list's refusal", err)
	}
}

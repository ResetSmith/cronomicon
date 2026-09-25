package api_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestScriptSlashName proves the FB2 routing fix (scripts-browsing.md): a script
// whose name is a repo-relative path (it lives in a sub-folder) resolves on both
// the detail and content routes via the {name...} trailing wildcard — for the
// %2F-encoded form the generated client emits AND a literal-slash URL.
func TestScriptSlashName(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()

	clone := t.TempDir()
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", clone) // getScriptContent reads gitlab.DefaultCloneDir()
	if err := os.MkdirAll(filepath.Join(clone, "scripts", "ops-playbooks"), 0o750); err != nil {
		t.Fatal(err)
	}
	body := "---\n- name: site\n  hosts: all\n"
	if err := os.WriteFile(filepath.Join(clone, "scripts", "ops-playbooks", "site.yml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO scripts(name, run_type, script_path, content_hash, synced_at) VALUES(?,?,?,?,?)`,
		"ops-playbooks/site.yml", "ansible", "scripts/ops-playbooks/site.yml", "sha256:z", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	client := devLoginClient(t, ts)

	// Detail — both the %2F-encoded form (what openapi-fetch sends) and a literal
	// slash resolve to the full multi-segment name.
	for _, url := range []string{
		ts.URL + "/api/v1/scripts/ops-playbooks%2Fsite.yml",
		ts.URL + "/api/v1/scripts/ops-playbooks/site.yml",
	} {
		var d struct {
			Name    string `json:"name"`
			RunType string `json:"runType"`
		}
		getJSON(t, client, url, &d)
		if d.Name != "ops-playbooks/site.yml" || d.RunType != "ansible" {
			t.Errorf("detail %s: got %+v, want name=ops-playbooks/site.yml runType=ansible", url, d)
		}
	}

	// Content — the new /script-content/{name...} route serves the body.
	var c struct {
		Content  *string `json:"content"`
		Encoding string  `json:"encoding"`
	}
	getJSON(t, client, ts.URL+"/api/v1/script-content/ops-playbooks%2Fsite.yml", &c)
	if c.Encoding != "utf8" || c.Content == nil || *c.Content != body {
		t.Errorf("content: got %+v, want utf8 body %q", c, body)
	}

	// A genuinely missing nested name -> 404 (not a route miss).
	assertStatus(t, client, ts.URL+"/api/v1/scripts/ops-playbooks%2Fnope.yml", http.StatusNotFound, "")
	assertStatus(t, client, ts.URL+"/api/v1/script-content/ops-playbooks%2Fnope.yml", http.StatusNotFound, "")
}

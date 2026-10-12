package api_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/gittest"
)

// TestScriptContent exercises GET /script-content/{name}: inline bodies from the
// DB, a scriptPath body read from the synced clone, a binary file, a 404 for an
// unknown script, and a 409 (no path leak) for a scriptPath whose file is absent.
func TestScriptContent(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()

	clone := t.TempDir()
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", clone) // getScriptContent reads gitlab.DefaultCloneDir()
	deployBody := "#!/bin/bash\necho deploy \"$CRONOMICON_SECRET_DB_PASS\"\n"
	// Committed, and recorded as the synced commit: the body is read from the
	// commit's objects (present defect 27).
	gittest.CommitSynced(t, pool, "global", clone, map[string]string{
		"scripts/deploy.sh": deployBody,
		"scripts/blob.sh":   "\x00\xff\xfe",
	})
	// In the working tree and in no commit: not a body.
	if err := os.WriteFile(filepath.Join(clone, "scripts", "nope.sh"), []byte("echo only in the working tree\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT INTO scripts(name, run_type, command, content_hash, synced_at) VALUES('inline-cmd','bash','echo hi','sha256:a','t')`)
	seed(`INSERT INTO scripts(name, run_type, script_path, content_hash, synced_at) VALUES('path-script','bash','scripts/deploy.sh','sha256:c','t')`)
	seed(`INSERT INTO scripts(name, run_type, script_path, content_hash, synced_at) VALUES('binary-script','bash','scripts/blob.sh','sha256:d','t')`)
	seed(`INSERT INTO scripts(name, run_type, script_path, content_hash, synced_at) VALUES('missing-file','bash','scripts/nope.sh','sha256:e','t')`)

	client := devLoginClient(t, ts)
	type resp struct {
		Content    *string `json:"content"`
		Encoding   string  `json:"encoding"`
		Truncated  bool    `json:"truncated"`
		ByteLength int     `json:"byteLength"`
		RunType    string  `json:"runType"`
	}

	// Inline command: served from the DB column, utf8.
	var cmd resp
	getJSON(t, client, ts.URL+"/api/v1/script-content/inline-cmd", &cmd)
	if cmd.Encoding != "utf8" || cmd.Content == nil || *cmd.Content != "echo hi" {
		t.Errorf("inline-cmd: %+v", cmd)
	}

	// scriptPath: read on demand from the clone.
	var ps resp
	getJSON(t, client, ts.URL+"/api/v1/script-content/path-script", &ps)
	if ps.Content == nil || *ps.Content != deployBody {
		t.Errorf("path-script content = %q, want %q", derefOr(ps.Content), deployBody)
	}
	if ps.RunType != "bash" || ps.Truncated {
		t.Errorf("path-script meta: %+v", ps)
	}

	// Binary file: encoding=binary, content null, byteLength reported.
	var bin resp
	getJSON(t, client, ts.URL+"/api/v1/script-content/binary-script", &bin)
	if bin.Encoding != "binary" || bin.Content != nil || bin.ByteLength != 3 {
		t.Errorf("binary-script: %+v", bin)
	}

	// Unknown script -> 404.
	assertStatus(t, client, ts.URL+"/api/v1/script-content/does-not-exist", http.StatusNotFound, clone)
	// scriptPath whose file is absent -> 409, and the body must not leak the abs path.
	assertStatus(t, client, ts.URL+"/api/v1/script-content/missing-file", http.StatusConflict, clone)

	// Present defect 27: a sync rewrites the working tree in place. With the
	// file truncated there, what is shown and what is scanned is still the
	// committed file, whole.
	if err := os.WriteFile(filepath.Join(clone, "scripts", "deploy.sh"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var again resp
	getJSON(t, client, ts.URL+"/api/v1/script-content/path-script", &again)
	if again.Content == nil || *again.Content != deployBody {
		t.Errorf("with the working-tree file truncated the content shown = %q, want the committed file %q", derefOr(again.Content), deployBody)
	}
	var scan struct {
		Suggested []struct {
			Reference string `json:"reference"`
		} `json:"suggested"`
	}
	getJSON(t, client, ts.URL+"/api/v1/script-reference-scan/path-script", &scan)
	if len(scan.Suggested) != 1 || scan.Suggested[0].Reference != "CRONOMICON_SECRET_DB_PASS" {
		t.Errorf("with the working-tree file truncated the scan suggests %+v, want the one reference in the committed file", scan.Suggested)
	}
	// A file that is only in the working tree is not scanned either.
	assertStatus(t, client, ts.URL+"/api/v1/script-reference-scan/missing-file", http.StatusConflict, clone)
}

func assertStatus(t *testing.T, client *http.Client, url string, want int, secretPath string) {
	t.Helper()
	r, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != want {
		t.Fatalf("GET %s = %d, want %d; body=%s", url, r.StatusCode, want, body)
	}
	if secretPath != "" && strings.Contains(string(body), secretPath) {
		t.Errorf("GET %s leaked the filesystem path %q in the response: %s", url, secretPath, body)
	}
}

func derefOr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

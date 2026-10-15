package gitlab

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gogithttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	githttpclient "github.com/go-git/go-git/v5/plumbing/transport/client"
)

// smartHTTPRemote serves a bare repository with one commit over Git's smart
// HTTP, by git http-backend itself: read-only unless writable, as a host
// serves a token that may only read.
func smartHTTPRemote(t *testing.T, writable bool) string {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("no git: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	root := t.TempDir()
	work := t.TempDir()
	runGit(t, root, "init", "--bare", "-b", "main", "defs.git")
	runGit(t, work, "init", "-b", "main", ".")
	runGit(t, work, "commit", "--allow-empty", "-m", "first")
	runGit(t, work, "push", filepath.Join(root, "defs.git"), "main")
	if writable {
		runGit(t, filepath.Join(root, "defs.git"), "config", "http.receivepack", "true")
	}
	srv := httptest.NewServer(&cgi.Handler{
		Path: backend,
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	})
	t.Cleanup(srv.Close)
	return srv.URL + "/defs.git"
}

// The connection test asks for the push handshake, and a token that may only
// read is told so (§0a, IN-2; Phase I3; the owner's answer to question 38).
func TestI3_TheConnectionTestAsksWhetherTheTokenCanPush(t *testing.T) {
	// The guarded transport refuses loopback, and is installed once per
	// process: a plain client for this test's remote, the guarded one after.
	// The guard is installed first, as it would be, so that its one install
	// is spent on the guard and not on this test's client; it is put back
	// after.
	(&Service{}).installGuardedGitTransport()
	prevHTTP := githttpclient.Protocols["http"]
	githttpclient.InstallProtocol("http", gogithttp.NewClient(http.DefaultClient))
	t.Cleanup(func() { githttpclient.InstallProtocol("http", prevHTTP) })

	readOnly := TestConnection(context.Background(), nil, smartHTTPRemote(t, false), "tok", "main")
	if !readOnly.OK || !readOnly.BranchFound {
		t.Fatalf("the read-only remote did not answer the listing: %+v", readOnly)
	}
	if readOnly.CanPush || readOnly.PushError == "" {
		t.Errorf("a remote that refuses pushes tested as pushable: %+v", readOnly)
	}
	if strings.Contains(readOnly.PushError, "tok") {
		t.Errorf("the push error carries the token: %s", readOnly.PushError)
	}

	writable := TestConnection(context.Background(), nil, smartHTTPRemote(t, true), "tok", "main")
	if !writable.OK || !writable.CanPush || writable.PushError != "" {
		t.Errorf("a remote that accepts pushes: %+v, want ok and canPush", writable)
	}
}

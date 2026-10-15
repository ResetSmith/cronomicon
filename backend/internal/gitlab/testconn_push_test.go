package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttpclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	gogithttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// smartHTTPRemote serves a bare repository with one commit over Git's smart
// HTTP, by git http-backend itself: read-only unless writable, as a host
// serves a token that may only read.
func smartHTTPRemote(t *testing.T, writable bool) string {
	t.Helper()
	url, _ := smartHTTPRemoteAt(t, writable)
	return url
}

// smartHTTPRemoteAt is smartHTTPRemote, with the bare repository's directory.
func smartHTTPRemoteAt(t *testing.T, writable bool) (url, bare string) {
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
	return srv.URL + "/defs.git", filepath.Join(root, "defs.git")
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
	// What canPush cannot say is said with it: a protected branch shows at
	// the first publish.
	if !strings.Contains(writable.PushNote, "Maintainer") {
		t.Errorf("the push note: %q, want it to say a protected branch needs Maintainer access", writable.PushNote)
	}
	if readOnly.PushNote != "" {
		t.Errorf("a refused handshake carries the push note: %q", readOnly.PushNote)
	}
}

// A push the remote refuses because the branch is protected is told so, in
// the remote's own words (what GitLab sends as "remote:" lines while it
// receives), and any other refusal is left as it was.
func TestAPushToAProtectedBranchIsToldSo(t *testing.T) {
	(&Service{}).installGuardedGitTransport()
	prevHTTP := githttpclient.Protocols["http"]
	githttpclient.InstallProtocol("http", gogithttp.NewClient(http.DefaultClient))
	t.Cleanup(func() { githttpclient.InstallProtocol("http", prevHTTP) })

	push := func(hookSays string) error {
		t.Helper()
		url, bare := smartHTTPRemoteAt(t, true)
		hook := filepath.Join(bare, "hooks", "pre-receive")
		if err := os.WriteFile(hook, []byte("#!/bin/sh\necho \""+hookSays+"\" >&2\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		work := t.TempDir()
		repo, err := gogit.PlainClone(work, false, &gogit.CloneOptions{URL: url})
		if err != nil {
			t.Fatalf("clone: %v", err)
		}
		wt, _ := repo.Worktree()
		if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _ = wt.Add("f.txt")
		if _, err := wt.Commit("c", &gogit.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t"}}); err != nil {
			t.Fatal(err)
		}
		var said strings.Builder
		perr := pushHearingRemote(context.Background(), repo, &gogit.PushOptions{
			RemoteName: "origin",
			RefSpecs:   []gogitconfig.RefSpec{"refs/heads/main:refs/heads/main"},
		}, &said)
		return protectedBranchRefusal(perr, said.String(), "main")
	}

	err := push("GitLab: You are not allowed to push code to protected branches on this project.")
	var pb *ProtectedBranchError
	if !errors.As(err, &pb) || pb.Branch != "main" || !strings.Contains(pb.Remote, "protected branches") {
		t.Fatalf("a push refused for a protected branch: %v (%#v), want a ProtectedBranchError with the remote's words", err, pb)
	}
	if err := push("this server is read-only: pushes are refused"); err == nil || errors.As(err, &pb) {
		t.Errorf("a push refused for another reason: %v, want the refusal as it was", err)
	}
}

package gitlab

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stallingReceivePack is stallingUploadPack for the program a PUSH runs on a
// local remote: a `git-receive-pack` first in PATH that never answers for a
// remote whose path contains what stall was last given, and is git's own for
// every other.
func stallingReceivePack(t *testing.T) (stall func(pathPart string)) {
	t.Helper()
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("git binary not available: %v", err)
	}
	own := filepath.Join(strings.TrimSpace(string(out)), "git-receive-pack")
	if _, err := os.Stat(own); err != nil {
		t.Skipf("git's own git-receive-pack: %v", err)
	}
	dir := t.TempDir()
	which := filepath.Join(dir, "stall")
	script := "#!/bin/sh\n" +
		"pat=$(cat '" + which + "' 2>/dev/null)\n" +
		"if [ -n \"$pat\" ]; then case \"$1\" in *\"$pat\"*) exec sleep 60 ;; esac; fi\n" +
		"exec '" + own + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git-receive-pack"), []byte(script), 0o755); err != nil {
		t.Fatalf("write the stalling git-receive-pack: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func(pathPart string) {
		t.Helper()
		if err := os.WriteFile(which, []byte(pathPart), 0o644); err != nil {
			t.Fatalf("say which remote stalls: %v", err)
		}
	}
}

// A publish's two transfers are bounded as a sync's are (the owner's decision
// of 2026-10-12): the fetch it starts with, and its push. A publish waits in
// the one queue every repository's syncs and publishes wait in, so a remote
// that takes the connection and then says nothing would otherwise hold every
// repository for as long as the connection stayed open. Given up at the bound,
// the publish is an error that names the bound, nothing is left on the clone,
// and the next publish goes through.
func TestAPublishIsGivenUpAtTheBound(t *testing.T) {
	stallFetch := stallingUploadPack(t)
	stallPush := stallingReceivePack(t)
	svc, bare, branch := bareFixture(t)
	ctx := context.Background()
	synced := svc.SyncBlocking(ctx, "manual")
	if synced.Status == "failed" {
		t.Fatalf("sync: %s", synced.ErrorMessage)
	}
	before := sysGit(t, bare, "for-each-ref")
	svc.Cfg.GitFetchTimeout = 300 * time.Millisecond
	givenUp := func(what, transfer string) {
		t.Helper()
		began := time.Now()
		res, err := svc.Publish(ctx, PublishRequest{FilePath: "jobs/bounded.yaml", Content: jobYAML("bounded")}, synced.SHA, "a@example.com")
		if took := time.Since(began); took > time.Minute {
			t.Fatalf("%s: the publish took %s: the bound did not end it", what, took)
		}
		if err == nil {
			t.Fatalf("%s: the publish answered success: %+v", what, res)
		}
		for _, want := range []string{transfer, "given up after 300ms", "CRONOMICON_GIT_FETCH_TIMEOUT"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the publish's error %q does not say %q", what, err, want)
			}
		}
		if got := sysGit(t, svc.cloneDir, "rev-parse", "HEAD"); got != synced.SHA {
			t.Errorf("%s: the clone's head is %s, want it where the last sync left it, %s", what, got, synced.SHA)
		}
		if st := sysGit(t, svc.cloneDir, "status", "--porcelain"); st != "" {
			t.Errorf("%s: the clone is not clean:\n%s", what, st)
		}
		if after := sysGit(t, bare, "for-each-ref"); after != before {
			t.Errorf("%s: the publish reached the remote:\n%s", what, after)
		}
	}

	stallFetch(bare)
	givenUp("a remote that stalls the fetch", "git fetch:")
	stallFetch("")

	stallPush(bare)
	givenUp("a remote that stalls the push", "git push:")
	stallPush("")

	// The remote answers again.
	svc.Cfg.GitFetchTimeout = 0
	res, err := svc.Publish(ctx, PublishRequest{FilePath: "jobs/bounded.yaml", Content: jobYAML("bounded")}, synced.SHA, "a@example.com")
	if err != nil {
		t.Fatalf("the publish after the remote came back: %v", err)
	}
	if got := sysGit(t, bare, "rev-parse", "refs/heads/"+branch); got != res.CommitSHA {
		t.Errorf("the branch is at %s, the publish answered %s", got, res.CommitSHA)
	}
}

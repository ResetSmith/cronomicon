package gitlab

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// The bound on one transfer (config.GitFetchTimeout, the owner's decision of
// 2026-10-10: ten minutes, one figure, CRONOMICON_GIT_FETCH_TIMEOUT).
//
// Every repository's syncs and publishes wait in one queue (GR-11). Until
// 2.4.0 a transfer had no bound but the 30 seconds a server is given to begin
// its answer, so a remote that answered and then went quiet held that queue
// for as long as the connection stayed open: no repository synced, and nothing
// said why.

// stallingUploadPack puts a `git-upload-pack` first in PATH that never answers
// for a remote whose path contains what stall was last given, and is git's own
// for every other. It is how a test has a remote that accepts the connection
// and then says nothing: go-git reaches a local remote by running that
// program, exactly as it reaches a remote over SSH.
func stallingUploadPack(t *testing.T) (stall func(pathPart string)) {
	t.Helper()
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("git binary not available: %v", err)
	}
	own := filepath.Join(strings.TrimSpace(string(out)), "git-upload-pack")
	if _, err := os.Stat(own); err != nil {
		t.Skipf("git's own git-upload-pack: %v", err)
	}
	dir := t.TempDir()
	which := filepath.Join(dir, "stall")
	script := "#!/bin/sh\n" +
		"pat=$(cat '" + which + "' 2>/dev/null)\n" +
		// Far longer than any bound a test sets, and not so long that what a
		// transfer given up leaves running outlives the test run by much.
		"if [ -n \"$pat\" ]; then case \"$1\" in *\"$pat\"*) exec sleep 60 ;; esac; fi\n" +
		"exec '" + own + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git-upload-pack"), []byte(script), 0o755); err != nil {
		t.Fatalf("write the stalling git-upload-pack: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func(pathPart string) {
		t.Helper()
		if err := os.WriteFile(which, []byte(pathPart), 0o644); err != nil {
			t.Fatalf("say which remote stalls: %v", err)
		}
	}
}

// fetchProblems is the repository's "could not be fetched" rows.
func fetchProblems(t *testing.T, svc *Service) []string {
	t.Helper()
	rows, err := svc.db.Query(`SELECT message FROM git_sync_problems
		 WHERE repo_id = ? AND kind = 'repository' AND severity = 'error' ORDER BY message`, svc.repo())
	if err != nil {
		t.Fatalf("read the sync problems: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, m)
	}
	return out
}

// A fetch that stalls is given up at the bound. The sync fails, its history
// and the repository's problems say why, and the next sync is not held.
func TestAFetchThatStallsIsGivenUpAtTheBound(t *testing.T) {
	stall := stallingUploadPack(t)
	svc, _, remote := newSyncFixture(t)
	ctx := context.Background()
	if r := svc.SyncBlocking(ctx, "manual"); r.Status != "success" {
		t.Fatalf("the first sync, which clones: %s: %s", r.Status, r.ErrorMessage)
	}

	stall(remote)
	svc.Cfg.GitFetchTimeout = 300 * time.Millisecond
	began := time.Now()
	res := svc.SyncBlocking(ctx, "manual")
	if took := time.Since(began); took > time.Minute {
		t.Fatalf("the sync of a stalled remote took %s: the bound did not end it", took)
	}
	if res.Status != "failed" {
		t.Fatalf("the sync of a stalled remote: %s, want failed", res.Status)
	}
	for _, want := range []string{"git fetch:", "given up after 300ms", "CRONOMICON_GIT_FETCH_TIMEOUT"} {
		if !strings.Contains(res.ErrorMessage, want) {
			t.Errorf("the sync's error %q does not say %q", res.ErrorMessage, want)
		}
	}
	var status, msg string
	if err := svc.db.QueryRow(`SELECT status, COALESCE(error_message, '') FROM git_sync_events ORDER BY id DESC LIMIT 1`).Scan(&status, &msg); err != nil {
		t.Fatalf("read the sync's history: %v", err)
	}
	if status != "failed" || !strings.Contains(msg, "CRONOMICON_GIT_FETCH_TIMEOUT") {
		t.Errorf("the history's last row is %s %q, want failed and the bound named", status, msg)
	}
	probs := fetchProblems(t, svc)
	if len(probs) != 1 || !strings.Contains(probs[0], "the repository could not be fetched:") ||
		!strings.Contains(probs[0], "given up after 300ms") {
		t.Fatalf("the repository's problems are %q, want the one row that says the fetch was given up", probs)
	}

	// The remote answers again: the next sync is a sync like any other.
	stall("")
	svc.Cfg.GitFetchTimeout = 0
	if r := svc.SyncBlocking(ctx, "manual"); r.Status != "success" {
		t.Fatalf("the sync after the remote came back: %s: %s", r.Status, r.ErrorMessage)
	}
	if probs := fetchProblems(t, svc); len(probs) != 0 {
		t.Errorf("the repository's problems after a sync that fetched: %q, want none", probs)
	}
}

// What the bound is for: another repository's sync, waiting in the one queue
// behind a transfer that has stalled, runs when that transfer is given up.
func TestAStalledRepositoryDoesNotHoldAnothersSync(t *testing.T) {
	stall := stallingUploadPack(t)
	slow, _, slowRemote := newSyncFixture(t)
	other, _, _ := newSyncFixture(t)
	gate := make(chan struct{}, 1)
	slow.gate, other.gate = gate, gate
	ctx := context.Background()

	stall(slowRemote)
	slow.Cfg.GitFetchTimeout = 500 * time.Millisecond
	slowDone := make(chan SyncResult, 1)
	go func() { slowDone <- slow.SyncBlocking(ctx, "manual") }()
	eventually(t, "the stalled repository's sync to hold the queue", func() bool { return len(gate) == 1 })

	began := time.Now()
	res := other.SyncBlocking(ctx, "manual")
	if res.Status != "success" {
		t.Fatalf("the other repository's sync: %s: %s", res.Status, res.ErrorMessage)
	}
	if took := time.Since(began); took > time.Minute {
		t.Fatalf("the other repository's sync waited %s behind the stalled one", took)
	}
	if r := <-slowDone; r.Status != "failed" || !strings.Contains(r.ErrorMessage, "CRONOMICON_GIT_FETCH_TIMEOUT") {
		t.Fatalf("the stalled repository's sync: %s %q, want failed at the bound", r.Status, r.ErrorMessage)
	}
}

// A first clone is bound as a fetch is, the failure does not quote the
// connection's URL to the agency, and what a clone that was given up leaves
// behind does not stop the next one.
func TestACloneThatStallsIsGivenUpAtTheBound(t *testing.T) {
	stall := stallingUploadPack(t)
	svc, _, remote := newSyncFixture(t)
	ctx := context.Background()

	stall(remote)
	svc.Cfg.GitFetchTimeout = 300 * time.Millisecond
	res := svc.SyncBlocking(ctx, "manual")
	if res.Status != "failed" {
		t.Fatalf("the sync of a remote that stalls its first clone: %s, want failed", res.Status)
	}
	for _, want := range []string{"git clone", "given up after 300ms", "CRONOMICON_GIT_FETCH_TIMEOUT"} {
		if !strings.Contains(res.ErrorMessage, want) {
			t.Errorf("the sync's error %q does not say %q", res.ErrorMessage, want)
		}
	}
	probs := fetchProblems(t, svc)
	if len(probs) != 1 || !strings.Contains(probs[0], "given up after 300ms") {
		t.Fatalf("the repository's problems are %q, want the one row that says the clone was given up", probs)
	}
	if strings.Contains(probs[0], remote) {
		t.Errorf("the problem row quotes the connection's URL: %q", probs[0])
	}

	stall("")
	svc.Cfg.GitFetchTimeout = 0
	if r := svc.SyncBlocking(ctx, "manual"); r.Status != "success" {
		t.Fatalf("the sync after the remote came back: %s: %s", r.Status, r.ErrorMessage)
	}
	var jobs int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE source = 'git'`).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("%d jobs synced after the clone (%v), want the repository's one", jobs, err)
	}
}

// A caller that goes away is not the bound: the sync does not say that
// CRONOMICON_GIT_FETCH_TIMEOUT ended it, and the repository gets no problem
// row for a sync that was merely abandoned.
func TestACallerThatGoesAwayIsNotTheBound(t *testing.T) {
	stall := stallingUploadPack(t)
	svc, _, remote := newSyncFixture(t)
	if r := svc.SyncBlocking(context.Background(), "manual"); r.Status != "success" {
		t.Fatalf("the first sync: %s: %s", r.Status, r.ErrorMessage)
	}

	stall(remote)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res := svc.SyncBlocking(ctx, "manual")
	if res.Status != "failed" {
		t.Fatalf("the abandoned sync: %s, want failed", res.Status)
	}
	if strings.Contains(res.ErrorMessage, "CRONOMICON_GIT_FETCH_TIMEOUT") {
		t.Errorf("the abandoned sync blames the bound, which is ten minutes and was not reached: %q", res.ErrorMessage)
	}
	if probs := fetchProblems(t, svc); len(probs) != 0 {
		t.Errorf("the repository's problems after an abandoned sync: %q, want none", probs)
	}
}

// A submodule's update is a transfer of its own under the same bound. One
// that stalls is skipped with a warning, as one that fails always was, and
// the repository's own files are synced.
func TestASubmoduleThatStallsIsSkippedAtTheBound(t *testing.T) {
	stall := stallingUploadPack(t)
	root := t.TempDir()

	pb := filepath.Join(root, "playbooks-origin")
	mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all\n  tasks: []\n")
	runGit(t, pb, "init", "-b", "main")
	runGit(t, pb, "add", ".")
	runGit(t, pb, "commit", "-m", "init playbooks")

	super := filepath.Join(root, "super-origin")
	mustWrite(t, filepath.Join(super, "scripts", "loose.sh"), "#!/bin/sh\necho hi\n")
	runGit(t, super, "init", "-b", "main")
	runGit(t, super, "add", ".")
	runGit(t, super, "commit", "-m", "init super")
	runGit(t, super, "-c", "protocol.file.allow=always", "submodule", "add", pb, "scripts/playbooks")
	runGit(t, super, "commit", "-m", "add playbooks submodule")

	// Only the submodule's remote stalls. The bound leaves the repository's own
	// clone, which is a few files from a local path, all the room it needs.
	stall("playbooks-origin")
	var buf bytes.Buffer
	clone := filepath.Join(root, "clone")
	svc := &Service{
		cloneDir: clone, repoURL: super,
		log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Cfg: &config.Config{GitFetchTimeout: 3 * time.Second},
	}
	began := time.Now()
	if _, err := svc.cloneOrFetch(t.Context(), "main"); err != nil {
		t.Fatalf("cloneOrFetch: %v", err)
	}
	if took := time.Since(began); took > time.Minute {
		t.Fatalf("the clone took %s: the bound did not end the submodule's update", took)
	}
	if _, err := os.Stat(filepath.Join(clone, "scripts", "loose.sh")); err != nil {
		t.Fatalf("the repository's own script is not checked out: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clone, "scripts", "playbooks", "lsblk.yml")); err == nil {
		t.Fatalf("the submodule's file is checked out: its remote did not stall, and the test proves nothing")
	}
	logs := buf.String()
	if !strings.Contains(logs, "update submodule failed") || !strings.Contains(logs, "CRONOMICON_GIT_FETCH_TIMEOUT") {
		t.Fatalf("no warning that the submodule's update was given up at the bound; logs:\n%s", logs)
	}
}

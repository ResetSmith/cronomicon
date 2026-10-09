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
)

// A sync reads the files of its repository and nothing else: a committed
// symbolic link that leads out of the repository is not read, and the sync
// says which file it was without saying where the link points or what is
// there. A link that stays inside the repository is followed, as before.
//
// Until Phase R3 only a script's body was read through a reader that checks
// (execspec.SafeReadRepoFile). Job, schedule, workflow and inventory files
// were read with os.ReadFile: a link named `inventory/x.ini` was read from
// wherever it pointed and its content stored as a scope's inventory, or quoted
// in a parse error. With clones side by side in one directory (GR-12), that is
// one agency's committer reading another agency's repository.
func TestGR3_ASymlinkOutOfTheRepositoryIsNotRead(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	var logs bytes.Buffer
	svc.log = slog.New(slog.NewTextHandler(&logs, nil))

	// What is outside: another repository's clone, as it would lie beside this one.
	outside := t.TempDir()
	const marker = "TOP-SECRET-OF-ANOTHER-AGENCY"
	mustWrite(t, filepath.Join(outside, "their.ini"), "[web]\nstolen-host ansible_host=10.9.9.9 # "+marker+"\n")
	mustWrite(t, filepath.Join(outside, "their-job.yaml"), strings.Replace(jobYAML("stolen-job"), "echo hi", "echo "+marker, 1))
	mustWrite(t, filepath.Join(outside, "their-script.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: stolen-script\nspec:\n  run_type: bash\n  command: echo "+marker+"\n")
	mustWrite(t, filepath.Join(outside, "their-schedule.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: Schedule\nmetadata:\n  name: stolen-schedule\nspec:\n  cron: \"0 3 1 1 *\"\n  description: "+marker+"\n")

	link := func(rel, target string) {
		t.Helper()
		abs := filepath.Join(remote, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, abs); err != nil {
			t.Fatalf("symlink %s: %v", rel, err)
		}
	}
	// RELATIVE targets, from where the link will lie in the server's clone: the
	// way out that works. (An absolute target does not: the Git library puts it
	// back under the clone's root when it checks the link out, so the link
	// dangles. One of those is here too, to show that it says nothing either.)
	out := func(dir, file string) string {
		t.Helper()
		rel, err := filepath.Rel(filepath.Join(svc.cloneDir, dir), filepath.Join(outside, file))
		if err != nil {
			t.Fatal(err)
		}
		return rel
	}
	link("inventory/leak.ini", out("inventory", "their.ini"))
	link("jobs/leak.yaml", out("jobs", "their-job.yaml"))
	link("scripts/leak.yaml", out("scripts", "their-script.yaml"))
	link("schedules/leak.yaml", out("schedules", "their-schedule.yaml"))
	link("jobs/absolute.yaml", filepath.Join(outside, "their-job.yaml"))
	// A link that stays inside the repository, and a plain file beside them.
	mustWrite(t, filepath.Join(remote, "shared", "aliased.yaml"), jobYAML("aliased-job"))
	link("jobs/alias.yaml", filepath.Join("..", "shared", "aliased.yaml"))
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"inventory/leak.ini", "jobs/leak.yaml", "scripts/leak.yaml", "schedules/leak.yaml", "jobs/absolute.yaml", "shared/aliased.yaml", "jobs/alias.yaml"} {
		if _, err := wt.Add(rel); err != nil {
			t.Fatalf("add %s: %v", rel, err)
		}
	}
	gitCommitFile(t, repo, remote, "jobs/plain.yaml", jobYAML("plain-job"), "links, and a plain job")

	res := svc.SyncBlocking(context.Background(), "manual")
	if res.Status == "failed" {
		t.Fatalf("sync failed outright: %s", res.ErrorMessage)
	}
	// The links are there in the clone, as links that lead to what is outside (or
	// this test proves nothing).
	if fi, err := os.Lstat(filepath.Join(svc.cloneDir, "inventory", "leak.ini")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the clone's inventory/leak.ini is not a symbolic link (%v)", err)
	}
	if data, err := os.ReadFile(filepath.Join(svc.cloneDir, "inventory", "leak.ini")); err != nil || !strings.Contains(string(data), marker) {
		t.Fatalf("the clone's inventory/leak.ini does not lead to the file outside (%v): the fixture is wrong", err)
	}

	for what, q := range map[string]string{
		"scope":    `SELECT COUNT(*) FROM scopes WHERE name = 'leak'`,
		"host":     `SELECT COUNT(*) FROM scope_hosts WHERE host = 'stolen-host'`,
		"record":   `SELECT COUNT(*) FROM ssh_hosts WHERE hostname = 'stolen-host'`,
		"job":      `SELECT COUNT(*) FROM jobs WHERE name = 'stolen-job'`,
		"script":   `SELECT COUNT(*) FROM scripts WHERE name = 'stolen-script'`,
		"schedule": `SELECT COUNT(*) FROM schedules WHERE name = 'stolen-schedule'`,
	} {
		if n := grCount(t, svc.db, q); n != 0 {
			t.Errorf("the %s behind the link was imported (count %d)", what, n)
		}
	}
	// Nowhere: not in a table, not in the result, not in the history, not in the log.
	var dump strings.Builder
	dump.WriteString(res.ErrorMessage)
	dump.WriteString(logs.String())
	for _, q := range []string{
		`SELECT COALESCE(GROUP_CONCAT(COALESCE(error_message,''), ' '), '') FROM git_sync_events`,
		`SELECT COALESCE(GROUP_CONCAT(COALESCE(raw_inventory,'') || COALESCE(git_meta_json,''), ' '), '') FROM scopes`,
		`SELECT COALESCE(GROUP_CONCAT(COALESCE(command,'') || COALESCE(script,''), ' '), '') FROM jobs`,
		`SELECT COALESCE(GROUP_CONCAT(COALESCE(summary,'') || COALESCE(details,''), ' '), '') FROM activity`,
	} {
		dump.WriteString(grString(t, svc.db, q))
	}
	if strings.Contains(dump.String(), marker) {
		t.Errorf("what is behind a link out of the repository was read: its content is in a table, the result or the log")
	}
	if strings.Contains(res.ErrorMessage, outside) || strings.Contains(res.ErrorMessage, filepath.Base(outside)) || strings.Contains(res.ErrorMessage, "their") {
		t.Errorf("the sync's error names where a link points: %s", res.ErrorMessage)
	}
	if !strings.Contains(res.ErrorMessage, "absolute.yaml") || !strings.Contains(res.ErrorMessage, "cannot be followed") {
		t.Errorf("the sync's error does not name the link that cannot be followed: %s", res.ErrorMessage)
	}
	// And it says so, per file.
	if res.Status != "partial" || !strings.Contains(res.ErrorMessage, "leads out of the repository") {
		t.Errorf("the sync is %q with %q; want partial, naming the links it did not read", res.Status, res.ErrorMessage)
	}
	for _, name := range []string{"leak.ini", "leak.yaml"} {
		if !strings.Contains(res.ErrorMessage, name) {
			t.Errorf("the sync's error does not name %s: %s", name, res.ErrorMessage)
		}
	}
	// The link that stays inside the repository was followed; the plain file was read.
	for _, job := range []string{"aliased-job", "plain-job", "keep"} {
		if jobCount(t, svc.db, job) != 1 {
			t.Errorf("the job %s was not imported", job)
		}
	}
}

// An agency's repository's submodules are not fetched (Phase R0's addition to
// R3, accepted by the owner): a submodule's URL is whatever a committer wrote
// in .gitmodules, and it would be fetched with the repository's token, outside
// the allowlist an agency's connection is held to (GR-10). The sync says so.
// Global's repository's submodules are fetched as before
// (TestCloneOrFetch_PopulatesSubmodules).
func TestGR3_AnAgencysRepositorysSubmodulesAreNotFetched(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
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

	for _, c := range []struct {
		repoID  string
		fetched bool
	}{{"", true}, {"repo-b", false}} {
		var logs bytes.Buffer
		clone := filepath.Join(root, "clone-"+c.repoID)
		svc := &Service{cloneDir: clone, repoURL: super, repoID: c.repoID, log: slog.New(slog.NewTextHandler(&logs, nil))}
		// Twice: the first is the clone, the second the fetch into it.
		for pass := 1; pass <= 2; pass++ {
			if _, err := svc.cloneOrFetch(t.Context(), "main"); err != nil {
				t.Fatalf("repository %q, pass %d: cloneOrFetch: %v", c.repoID, pass, err)
			}
			_, err := os.Stat(filepath.Join(clone, "scripts", "playbooks", "lsblk.yml"))
			if got := err == nil; got != c.fetched {
				t.Errorf("repository %q, pass %d: the submodule's file is present = %v, want %v", c.repoID, pass, got, c.fetched)
			}
		}
		said := strings.Contains(logs.String(), "declares submodules")
		if said == c.fetched {
			t.Errorf("repository %q: the sync said that submodules are not fetched = %v, want %v", c.repoID, said, !c.fetched)
		}
		// Its own files are read either way.
		if _, err := os.Stat(filepath.Join(clone, "scripts", "loose.sh")); err != nil {
			t.Errorf("repository %q: its own script is missing: %v", c.repoID, err)
		}
	}
}

// The ways out that the first reading of this phase left open, each found by
// its review: a link in place of a scope's sidecar, a link in place of a
// script's body, and a link into the clone's own .git, which is inside the
// directory and is no part of what the repository holds (its config names the
// connection's URL).
func TestGR3_NeitherASidecarNorABodyNorTheClonesGitDirIsReadThroughALink(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	var logs bytes.Buffer
	svc.log = slog.New(slog.NewTextHandler(&logs, nil))

	outside := t.TempDir()
	const marker = "TOP-SECRET-OF-ANOTHER-AGENCY"
	mustWrite(t, filepath.Join(outside, "their-sidecar.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: ScopeMeta\nmetadata:\n  name: web\nspec:\n  owner: "+marker+"\n  description: "+marker+"\n")
	mustWrite(t, filepath.Join(outside, "their-body.sh"), "#!/bin/sh\necho "+marker+"\n")

	link := func(rel, target string) {
		t.Helper()
		abs := filepath.Join(remote, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, abs); err != nil {
			t.Fatalf("symlink %s: %v", rel, err)
		}
		wt, err := repo.Worktree()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add(rel); err != nil {
			t.Fatalf("add %s: %v", rel, err)
		}
	}
	out := func(dir, file string) string {
		t.Helper()
		rel, err := filepath.Rel(filepath.Join(svc.cloneDir, dir), filepath.Join(outside, file))
		if err != nil {
			t.Fatal(err)
		}
		return rel
	}
	link("inventory/web.cronomicon.yaml", out("inventory", "their-sidecar.yaml"))
	link("scripts/body.sh", out("scripts", "their-body.sh"))
	link("inventory/gitconf.ini", filepath.Join("..", ".git", "config"))
	link("scripts/gitconf.sh", filepath.Join("..", ".git", "config"))
	grCommitFiles(t, repo, remote, map[string]string{
		"inventory/web.ini":    "# cronomicon:v1 owner=its-own\n[web]\nweb1 ansible_host=10.0.0.1\n",
		"scripts/wrapped.yaml": "apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: wrapped\nspec:\n  run_type: bash\n  scriptPath: scripts/body.sh\n",
	}, "links, a scope with a real inventory, and a script whose body is a link")

	res := svc.SyncBlocking(context.Background(), "manual")
	if res.Status == "failed" {
		t.Fatalf("sync failed outright: %s", res.ErrorMessage)
	}
	// The fixture: the links are there in the clone and lead where they say.
	if data, err := os.ReadFile(filepath.Join(svc.cloneDir, "inventory", "web.cronomicon.yaml")); err != nil || !strings.Contains(string(data), marker) {
		t.Fatalf("the clone's sidecar link does not lead to the file outside (%v): the fixture is wrong", err)
	}
	conf, err := os.ReadFile(filepath.Join(svc.cloneDir, "inventory", "gitconf.ini"))
	if err != nil || !strings.Contains(string(conf), "[remote") {
		t.Fatalf("the clone's inventory/gitconf.ini does not lead to .git/config (%v): the fixture is wrong", err)
	}

	// The scope is there, from its real inventory, as its own file says.
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM scopes WHERE source='git' AND name='web'`); n != 1 {
		t.Fatalf("the scope with a real inventory was not imported (count %d)", n)
	}
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM scopes WHERE name='gitconf'`); n != 0 {
		t.Errorf("the clone's .git/config was imported as a scope's inventory")
	}

	var dump strings.Builder
	dump.WriteString(res.ErrorMessage)
	dump.WriteString(logs.String())
	for _, q := range []string{
		`SELECT COALESCE(GROUP_CONCAT(COALESCE(error_message,''), ' '), '') FROM git_sync_events`,
		`SELECT COALESCE(GROUP_CONCAT(COALESCE(raw_inventory,'') || COALESCE(git_meta_json,'') || COALESCE(description,''), ' '), '') FROM scopes`,
		`SELECT COALESCE(GROUP_CONCAT(COALESCE(command,'') || COALESCE(script,'') || COALESCE(description,''), ' '), '') FROM scripts`,
		`SELECT COALESCE(GROUP_CONCAT(COALESCE(command,'') || COALESCE(script,''), ' '), '') FROM jobs`,
		`SELECT COALESCE(GROUP_CONCAT(message, ' '), '') FROM git_sync_problems`,
		`SELECT COALESCE(GROUP_CONCAT(detail, ' '), '') FROM notices`,
	} {
		dump.WriteString(grString(t, svc.db, q))
	}
	all := dump.String()
	if strings.Contains(all, marker) {
		t.Errorf("what is behind a link out of the repository was read: its content is in a table, the result or the log")
	}
	if strings.Contains(all, "[remote") || strings.Contains(all, "repositoryformatversion") {
		t.Errorf("the clone's .git/config was read: its content is in a table, the result or the log")
	}
	// Nor where a link leads: a file that is there and one that is not must not
	// answer differently to whoever reads the repository's problems.
	for _, where := range []string{outside, filepath.Base(outside), "their-body", "their-sidecar"} {
		if strings.Contains(res.ErrorMessage, where) || strings.Contains(grString(t, svc.db, `SELECT COALESCE(GROUP_CONCAT(message, ' '), '') FROM git_sync_problems`), where) {
			t.Errorf("the sync says where a link leads (%q): %s", where, res.ErrorMessage)
		}
	}
	rows := grProblems(t, svc, "global")
	if !grHas(rows, "error script scripts/wrapped.yaml:", "could not be read") {
		t.Errorf("no row saying the script's body could not be read:\n%s", strings.Join(rows, "\n"))
	}
	if !grHas(rows, "error scope inventory/gitconf.ini:", "leads out of the repository") {
		t.Errorf("no row saying the link into .git was not read:\n%s", strings.Join(rows, "\n"))
	}
}

// `inventory` itself as a link to a directory outside the repository: nothing
// in it is listed, read or named.
func TestGR3_ALinkedInventoryDirectoryIsNotListed(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	outside := t.TempDir()
	const marker = "TOP-SECRET-OF-ANOTHER-AGENCY"
	mustWrite(t, filepath.Join(outside, "theirscope.ini"), "# cronomicon:v1 owner=t\n[web]\nstolen-host ansible_host=10.9.9.9 # "+marker+"\n")
	target, err := filepath.Rel(svc.cloneDir, outside)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(remote, "inventory")); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("inventory"); err != nil {
		t.Fatalf("add the link: %v", err)
	}
	gitCommitFile(t, repo, remote, "jobs/plain.yaml", jobYAML("plain-job"), "inventory is a link out of the repository")

	res := svc.SyncBlocking(context.Background(), "manual")
	if res.Status == "failed" {
		t.Fatalf("sync failed outright: %s", res.ErrorMessage)
	}
	if entries, err := os.ReadDir(filepath.Join(svc.cloneDir, "inventory")); err != nil || len(entries) != 1 {
		t.Fatalf("the clone's inventory does not lead to the directory outside (%v): the fixture is wrong", err)
	}
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM scopes WHERE source='git'`); n != 0 {
		t.Errorf("a scope was imported from a directory outside the repository (count %d)", n)
	}
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM scope_hosts WHERE host='stolen-host'`); n != 0 {
		t.Errorf("a host was imported from a directory outside the repository")
	}
	rows := grProblems(t, svc, "global")
	said := res.ErrorMessage + " " + strings.Join(rows, " ")
	for _, leak := range []string{"theirscope", marker, outside, filepath.Base(outside)} {
		if strings.Contains(said, leak) {
			t.Errorf("the sync names what is in the directory outside (%q): %s", leak, said)
		}
	}
	if res.Status != "partial" || !grHas(rows, "error scope inventory:", "leads out of the repository") {
		t.Errorf("the sync is %q; want partial with a row saying inventory was not read:\n%s", res.Status, strings.Join(rows, "\n"))
	}
	if jobCount(t, svc.db, "plain-job") != 1 {
		t.Errorf("the repository's own job was not imported")
	}
}

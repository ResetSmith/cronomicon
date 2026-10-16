package gitlab

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A publish does not read or write through a symbolic link (2.4.0 Phase R6;
// found by the review of the phase).
//
// A publish names a file of the repository, reads the one that is there (for
// the diff of a refused publish, and for the scope of a job it would replace)
// and writes the new one, in the server's clone. The path is held to the
// repository's directories as a STRING. A file of the repository can be a
// symbolic link, committed by whoever commits to the repository, and reading
// or writing it by its path follows the link: out of the clone, to any file
// the server can read or write. A sync refuses such a link ("leads out of the
// repository"); a publish did not ask.
//
// While only Global's repository existed, its committers were the
// installation's administrators. Since this phase an agency connects its own
// repository and its approvers publish to it: one agency's committer could
// read the server's secrets, or another agency's clone, in the diff of a
// refused publish, and overwrite any file the server can write.
func TestAPublishDoesNotReadOrWriteThroughASymbolicLink(t *testing.T) {
	svc, bare, branch := bareFixture(t)
	ctx := context.Background()
	outside := t.TempDir()
	secret := filepath.Join(outside, "server-secret.txt")
	if err := os.WriteFile(secret, []byte("KEK=the-servers-own-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The repository's committer adds: a file that is a link out of the
	// repository, one that is a link to the clone's own .git, one that is a
	// link to another file of the repository, and a DIRECTORY that is a link.
	//
	// RELATIVE links, as they resolve from where the server's clone is: go-git
	// checks an absolute link out re-rooted inside the clone, where it points at
	// nothing, and a relative one as it is written.
	work := t.TempDir()
	sysGit(t, work, "clone", "-q", "--branch", branch, bare, ".")
	toSecret, err := filepath.Rel(filepath.Join(svc.cloneDir, "schedules"), secret)
	if err != nil {
		t.Fatal(err)
	}
	toDir, err := filepath.Rel(svc.cloneDir, filepath.Join(outside, "dir"))
	if err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"schedules/peek.yaml":   toSecret,
		"schedules/config.yaml": "../.git/config",
		"schedules/inside.yaml": "../jobs/real.yaml",
		"workflows":             toDir,
	}
	for link, target := range links {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, link)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(work, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(work, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "jobs", "real.yaml"), []byte(jobYAML("real")), 0o644); err != nil {
		t.Fatal(err)
	}
	sysGit(t, work, "add", "-A")
	sysGit(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "links")
	sysGit(t, work, "push", "-q", "origin", branch)
	synced := svc.SyncBlocking(ctx, "manual")
	if synced.SHA != sysGit(t, bare, "rev-parse", "refs/heads/"+branch) {
		t.Fatalf("the sync did not bring the links in: %+v", synced)
	}
	if got, err := os.ReadFile(filepath.Join(svc.cloneDir, "schedules", "peek.yaml")); err != nil || !strings.Contains(string(got), "the-servers-own-secret") {
		t.Fatalf("the fixture's link does not lead to the file outside the clone (%v): this test is of one that does", err)
	}
	const content = "apiVersion: cronomicon.io/v1\nkind: Schedule\nmetadata:\n  name: x\nspec:\n  cron: '0 2 * * *'\n"
	head := func() string { return sysGit(t, bare, "rev-parse", "refs/heads/"+branch) }

	for _, path := range []string{"schedules/peek.yaml", "schedules/config.yaml", "schedules/inside.yaml", "workflows/new.yaml"} {
		// A base that is not the head: the refusal carries a diff of "the file
		// that is there". It must not be the link's target.
		_, err = svc.Publish(ctx, PublishRequest{FilePath: path, Content: content}, strings.Repeat("0", 40), "a@example.com")
		var pe *PreconditionError
		if errors.As(err, &pe) {
			t.Errorf("%s on a stale base: answered with a diff of %d bytes, want the path refused", path, len(pe.Diff))
			if strings.Contains(pe.Diff, "the-servers-own-secret") || strings.Contains(pe.Diff, "[remote") || strings.Contains(pe.Diff, "repositoryformatversion") {
				t.Errorf("%s: THE DIFF CARRIES WHAT THE LINK POINTS TO:\n%s", path, pe.Diff)
			}
		}
		var ve *ValidationFailedError
		if !errors.As(err, &ve) || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("%s on a stale base: answered %v, want it refused as a symbolic link", path, err)
		}
		// On the head: it must not be written.
		_, err = svc.Publish(ctx, PublishRequest{FilePath: path, Content: content}, synced.SHA, "a@example.com")
		if !errors.As(err, &ve) || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("%s on the head: answered %v, want it refused as a symbolic link", path, err)
		}
		// What the route asks before a publish: nothing is read through it,
		// and the file is reported as one whose scope is not known.
		if target := svc.publishTarget(PublishRequest{FilePath: strings.Replace(path, "schedules/", "jobs/../schedules/", 0), Content: content}); target.Kind == "job" && (!target.OldExists || target.OldParsed) {
			t.Errorf("%s: the target reads %+v", path, target)
		}
	}
	if got, _ := os.ReadFile(secret); string(got) != "KEK=the-servers-own-secret\n" {
		t.Errorf("the file outside the repository was written through the link: %q", got)
	}
	if entries, _ := os.ReadDir(filepath.Join(outside, "dir")); len(entries) != 0 {
		t.Errorf("a file was written outside the repository through the linked directory: %v", entries)
	}
	if got, _ := os.ReadFile(filepath.Join(svc.cloneDir, "jobs", "real.yaml")); string(got) != jobYAML("real") {
		t.Errorf("a file of the repository was written through a link to it: %q", got)
	}
	if head() != synced.SHA {
		t.Errorf("a refused publish moved the branch")
	}
	// Each link in the clone is still the link it was: nothing replaced one
	// with a file, and nothing was written beside them.
	for link, target := range links {
		if got, err := os.Readlink(filepath.Join(svc.cloneDir, link)); err != nil || got != target {
			t.Errorf("%s in the clone is now %q (%v), want the link to %q it was", link, got, err, target)
		}
	}
	if cfg, _ := os.ReadFile(filepath.Join(svc.cloneDir, ".git", "config")); !strings.Contains(string(cfg), "[remote") || strings.Contains(string(cfg), "apiVersion") {
		t.Errorf("the clone's own .git/config was written through the link:\n%s", cfg)
	}

	// A job file that is a link: what the route's check is told of "the file
	// that is there" is that its scope is not known, so nobody but a global
	// publisher is let through, and Publish then refuses it.
	// (The link leads to a job file of somebody else's, with a scope in it:
	// read through, it would be reported as that scope's job.)
	others := filepath.Join(outside, "others-job.yaml")
	if err := os.WriteFile(others, []byte("apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: theirs\nspec:\n  run_type: bash\n  command: echo\n  scope: another-agencys-hosts\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(others, filepath.Join(svc.cloneDir, "jobs", "linked.yaml")); err != nil {
		t.Fatal(err)
	}
	if target := svc.publishTarget(PublishRequest{FilePath: "jobs/linked.yaml", Content: jobYAML("linked")}); !target.OldExists || target.OldParsed || target.OldScope != "" {
		t.Errorf("a job file that is a link is reported as %+v, want a file that is there with nothing read of it", target)
	}
	// And the diff, asked on its own account.
	if diff, err := svc.buildDiff(nil, "jobs/linked.yaml", "x"); err == nil || strings.Contains(diff, "another-agencys-hosts") {
		t.Errorf("the diff of a path that is a link: %q (%v), want it refused", diff, err)
	}
	if err := os.Remove(filepath.Join(svc.cloneDir, "jobs", "linked.yaml")); err != nil {
		t.Fatal(err)
	}

	// A path with no link in it is published as ever.
	if _, err := svc.Publish(ctx, PublishRequest{FilePath: "schedules/real.yaml", Content: content}, synced.SHA, "a@example.com"); err != nil {
		t.Fatalf("a publish of a path with no link in it: %v", err)
	}
	if head() == synced.SHA {
		t.Errorf("the ordinary publish did not reach the branch")
	}
}

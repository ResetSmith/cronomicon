package gitlab

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// A publish that fails before its push leaves the clone as it found it (2.5.0
// C7.5, question 100; found in C10.1's survey). Until then only a failed PUSH
// took the commit back: a failure staging or committing the file left the file
// the publish had written in the clone's working tree until the next sync reset
// it, so a second publish, or a read of the clone, saw a file that is in no
// commit.
//
// The failure is made real, not simulated: the clone's object store is made
// read-only, so staging the file (which writes its blob) fails.
func readOnlyObjects(t *testing.T, clone string) {
	t.Helper()
	// root writes through a read-only directory, so the failure cannot be made
	// this way under root (a container's test run, say): skip rather than
	// fail on the precondition.
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory does not stop root writing to it")
	}
	objects := filepath.Join(clone, ".git", "objects")
	var dirs []string
	_ = filepath.WalkDir(objects, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for _, d := range dirs {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, d := range dirs {
			_ = os.Chmod(d, 0o755)
		}
	})
}

func TestAPublishThatFailsToStageLeavesNoFileBehind(t *testing.T) {
	svc, _, _ := bareFixture(t)
	ctx := context.Background()
	r := svc.SyncBlocking(ctx, "manual")
	if r.Status == "failed" {
		t.Fatalf("sync: %s", r.ErrorMessage)
	}
	readOnlyObjects(t, svc.cloneDir)

	if _, err := svc.Publish(ctx, PublishRequest{FilePath: "jobs/new.yaml", Content: jobYAML("new")}, r.SHA, "a@example.com"); err == nil {
		t.Fatal("the publish succeeded with a read-only object store: this test is of one that fails to stage")
	}
	if _, err := os.Stat(filepath.Join(svc.cloneDir, "jobs", "new.yaml")); !os.IsNotExist(err) {
		t.Errorf("the file of a publish that failed to stage is still in the clone (stat: %v)", err)
	}
}

func TestAPublishThatFailsToStageRestoresTheFileItReplaced(t *testing.T) {
	svc, _, _ := bareFixture(t)
	ctx := context.Background()
	r := svc.SyncBlocking(ctx, "manual")
	if r.Status == "failed" {
		t.Fatalf("sync: %s", r.ErrorMessage)
	}
	keep := filepath.Join(svc.cloneDir, "jobs", "keep.yaml")
	before, err := os.ReadFile(keep)
	if err != nil {
		t.Fatal(err)
	}
	readOnlyObjects(t, svc.cloneDir)

	if _, err := svc.Publish(ctx, PublishRequest{FilePath: "jobs/keep.yaml", Content: jobYAML("changed")}, r.SHA, "a@example.com"); err == nil {
		t.Fatal("the publish succeeded with a read-only object store")
	}
	after, err := os.ReadFile(keep)
	if err != nil {
		t.Fatalf("the replaced file is gone: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("a publish that failed to stage left its content in the clone:\n%s", after)
	}
}

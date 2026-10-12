package execspec

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/ResetSmith/cronomicon/internal/gittest"
)

func mustOpen(t *testing.T, dir string) *gogit.Repository {
	t.Helper()
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	return repo
}

func mustHash(sha string) plumbing.Hash { return plumbing.NewHash(sha) }

// Links as the git program stores them: the target is the blob, word for word.
// (A link added through a go-git working tree has an absolute target rewritten
// relative to the repository, so the test above cannot hold these two.)
func TestALinkStoredByGitIsNotMisread(t *testing.T) {
	clone := filepath.Join(t.TempDir(), "global")
	sha := gittest.CommitObjects(t, clone,
		map[string]string{
			"scripts/real.sh": "echo real\n",
			// Where an absolute target would land if it were read as relative
			// to the link's directory.
			"scripts/etc/hostname": "echo not this one\n",
		},
		map[string]string{
			"scripts/absolute.sh": "/etc/hostname",
			"scripts/link.sh":     "real.sh",
			"scripts/a.sh":        "b.sh",
			"scripts/b.sh":        "a.sh",
		})

	if got, _, err := ReadCommitFile(clone, sha, "scripts/link.sh", 0); err != nil || string(got) != "echo real\n" {
		t.Fatalf("a link to the file beside it = %q (%v)", got, err)
	}
	var unread *CommitReadError

	got, _, err := ReadCommitFile(clone, sha, "scripts/absolute.sh", 0)
	if err == nil || !errors.As(err, &unread) || !strings.Contains(err.Error(), "escapes repository root") {
		t.Errorf("a link to an absolute path = %q, err %v; want it refused as leaving the repository", got, err)
	}

	// A loop is given up at the bound, counted: each link is followed once per
	// hop, so the bound is what ends it and not some other limit.
	hops := 0
	repo := mustOpen(t, clone)
	got, _, err = readInCommit(repo, mustHash(sha), "scripts/a.sh", "scripts/a.sh", 0, &hops)
	if err == nil || !errors.As(err, &unread) || !strings.Contains(err.Error(), "too many symbolic links") {
		t.Errorf("a loop of links = %q, err %v; want it given up", got, err)
	}
	if hops != maxLinkHops+1 {
		t.Errorf("a loop of links was followed %d times, want it given up at the bound of %d", hops, maxLinkHops)
	}
}

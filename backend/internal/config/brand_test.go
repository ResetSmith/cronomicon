package config

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Why this file exists (RN band, 2.0.0).
//
// The rename was a clean break: no compatibility readers, no aliases, no
// legacy spellings anywhere a process reads, an operator types or a user
// writes. That property decays one commit at a time — a pasted snippet, a
// fixture copied from an old branch, a doc example — and nothing else in the
// build would notice. This test is the exit criterion made permanent: any
// case-insensitive occurrence of the former product name, anywhere in the
// repository, fails the build.
//
// The token is assembled at runtime so this file does not contain it. The
// scan reads BYTES rather than shelling out to grep: three frontend sources
// carry raw NUL separators, which grep treats as binary and silently skips.

// brandExemptDirs are skipped wherever they appear by name, on top of what
// git already ignores.
var brandExemptDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	".aidata":      true, // the private planning archive — the record of the rename
	".claude":      true, // local harness files, never published
}

// brandExemptFiles are repository-relative paths that may carry the token.
var brandExemptFiles = map[string]string{
	"CHANGELOG.md": "history before 2.0.0 keeps its original text under the name-change note",
}

// repoFiles lists what git tracks plus untracked files it would not ignore —
// the set that could ever be committed. Ignored paths (build outputs, local
// secrets, dev databases) are a developer's own business and never scanned.
func repoFiles(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v (this guard needs git; run it from a clone)", err)
	}
	var files []string
	for f := range bytes.SplitSeq(out, []byte{0}) {
		if len(f) > 0 {
			files = append(files, string(f))
		}
	}
	return files
}

func TestNoFormerBrandAnywhere(t *testing.T) {
	// "ama" + "deus": the former name, never written whole in this tree.
	former := []byte(strings.ToLower("ama" + "deus"))

	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// CHANGELOG.md marks the root: it is tracked and lives only there. (Not
	// AGENTS.md, which is gitignored and absent from every fresh clone.)
	if _, err := os.Stat(filepath.Join(root, "CHANGELOG.md")); err != nil {
		t.Fatalf("repository root not found at %s: %v", root, err)
	}

	var hits []string
	scanned := 0
	for _, rel := range repoFiles(t, root) {
		if first, _, _ := strings.Cut(rel, "/"); brandExemptDirs[first] {
			continue
		}
		if _, ok := brandExemptFiles[rel]; ok {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue // deleted in the working tree, or a symlink
		}
		if bytes.Contains(bytes.ToLower([]byte(rel)), former) {
			hits = append(hits, rel+" (file name)")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		scanned++
		lower := bytes.ToLower(b)
		line := 1
		for off := 0; ; {
			i := bytes.Index(lower[off:], former)
			if i < 0 {
				break
			}
			line += bytes.Count(lower[off:off+i], []byte("\n"))
			hits = append(hits, rel+":"+itoa(line))
			off += i + len(former)
		}
	}
	if scanned < 500 {
		t.Fatalf("scanned only %d files under %s; the listing is not seeing the repository", scanned, root)
	}
	if len(hits) > 0 {
		sort.Strings(hits)
		t.Errorf("the former product name appears %d time(s); the 2.0.0 rename admits no legacy spelling "+
			"(see .aidata/20260924-rebrand-clean-break.md §5):\n  %s", len(hits), strings.Join(hits, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

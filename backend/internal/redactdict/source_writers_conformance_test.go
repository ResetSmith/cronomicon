package redactdict

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// AM-4b — every writer of a redaction source calls secrets.RedactionSourceChanged.
//
// The dictionary is rebuilt lazily on invalidation (plus a TTL backstop). A
// writer that forgets to invalidate leaves a freshly rotated value unmasked for
// up to TTL — and the rotation's own audit row is the row most likely to carry
// it. This is the rule that fails silently, so it gets a source scan, the same
// shape as scheduler/run_writer_conformance_test.go.
//
// A "writer" is any non-test file that writes to secrets / ssh_credentials /
// env_vars or calls secrets.EncryptString (the settings columns). Exempt:
// internal/seed (demo data, before any dictionary exists) and
// cmd/amadeus/rewrap.go (re-wraps ciphertext; the plaintext, which is what the
// dictionary holds, is unchanged).

var (
	sourceWrite = regexp.MustCompile(`(?i)(INSERT\s+INTO|INSERT\s+OR\s+REPLACE\s+INTO|UPDATE|DELETE\s+FROM)\s+(secrets|ssh_credentials|env_vars)\b|\bEncryptString\(`)
	notifies    = regexp.MustCompile(`\bRedactionSourceChanged\(`)
)

func TestEveryRedactionSourceWriterNotifies(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	exempt := map[string]bool{"cmd/amadeus/rewrap.go": true, "internal/secrets/blob.go": true}
	var silent []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "seed" && filepath.Base(filepath.Dir(path)) == "internal" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if exempt[rel] {
				return nil
			}
			src, err := os.ReadFile(path) //nolint:gosec // walking our own source tree
			if err != nil {
				return err
			}
			if sourceWrite.Match(src) && !notifies.Match(src) {
				silent = append(silent, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(silent)
	if len(silent) != 0 {
		t.Errorf("redaction-source writers that never call secrets.RedactionSourceChanged — a rotated value stays unmasked until the TTL:\n  %s",
			strings.Join(silent, "\n  "))
	}
}

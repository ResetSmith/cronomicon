package scheduler

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// RR-3 — there is exactly ONE `INSERT INTO runs` in production code, and it is
// InsertRun's (runrow.go). the run-row-unification plan.
//
// Five hand-rolled column lists drifted four times in eleven days before RR-2
// collapsed them; claimRun and the manifest read dispatch policy from the run
// row, so every drift was a run dispatched on incomplete policy. This test is
// what makes a sixth writer a build failure instead of the next drift. It is
// the same shape as gate_conformance_test.go and deleted_filter_sites_test.go:
// an invariant the repo already guards by scanning its own source.
//
// Exempt: internal/seed (demo fixtures, not a producer) and _test.go files
// (fixtures that write rows directly to set up a claim or a history read).
//
// If you are here because this failed: you do not need a new INSERT. Add the
// field to RunRow, add the column to InsertRun, and cut the goldens again
// (ROWGOLDEN_UPDATE=1) — the diff you review IS the change you made.

var insertIntoRuns = regexp.MustCompile(`(?i)INSERT\s+INTO\s+runs\b`)

func TestRunWriterConformance_ExactlyOneInsert(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var writers []string
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
			src, err := os.ReadFile(path) //nolint:gosec // walking our own source tree
			if err != nil {
				return err
			}
			if insertIntoRuns.Match(src) {
				rel, _ := filepath.Rel(root, path)
				writers = append(writers, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(writers)
	want := []string{"internal/scheduler/runrow.go"}
	if strings.Join(writers, "\n") != strings.Join(want, "\n") {
		t.Fatalf("expected exactly one INSERT INTO runs, in InsertRun; found:\n  %s\n"+
			"A new producer must build a RunRow and call scheduler.InsertRun — see the comment at the top of this file.",
			strings.Join(writers, "\n  "))
	}
}

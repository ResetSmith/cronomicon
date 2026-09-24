package auditlog

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// AM-3 — every INSERT into the three audit tables lives in THIS package.
// the audit-redaction plan.
//
// The masking hook (SetRedactor) and the length bound (TrimForAudit) are applied
// inside these writers, before the row is stored. A raw INSERT anywhere else
// bypasses both without any call site noticing — which is exactly how the
// break-glass CLI wrote auth_events until this test existed. Same shape as
// scheduler/run_writer_conformance_test.go: an invariant the repo guards by
// scanning its own source.
//
// Exempt: _test.go files (fixtures that write rows directly to set up a read),
// and internal/seed — which DOES use the shared writers, but labels each step
// with the literal "INSERT INTO activity" / "INSERT INTO change_log" for its
// error message, and that string is what this regex would match.
//
// If you are here because this failed: call WriteChangeLog / WriteActivity /
// WriteAuthEvent. They take an Execer, so a *sql.Tx works too.

var insertIntoAuditTable = regexp.MustCompile(`(?i)INSERT\s+INTO\s+(activity|change_log|auth_events)\b`)

func TestAuditWriterConformance_OnlyThisPackageInserts(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	self, _ := filepath.Abs(".")
	var offenders []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path == self {
					return filepath.SkipDir
				}
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
			if insertIntoAuditTable.Match(src) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(offenders)
	if len(offenders) != 0 {
		t.Errorf("raw INSERT into an audit table outside internal/auditlog — the masking hook and length bound do not apply there:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

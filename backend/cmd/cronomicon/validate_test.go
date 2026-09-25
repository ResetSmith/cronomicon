package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFixture writes a definition file under a temp repo, creating parent dirs.
func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestValidatePaths is the CLI-level e2e for `cronomicon validate` (V1.1-1): it
// exercises the runValidate core (validatePaths) the way CI does, asserting the
// exit code AND the line-numbered stderr format the .gitlab-ci.yml template
// relies on. Library-level parsing is covered by internal/gitlab; this guards
// the command wrapper's contract (exit 0/1/2, "<file>:<line>:" output).
func TestValidatePaths(t *testing.T) {
	// No args → usage error, exit 2.
	t.Run("usage", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if code := validatePaths(nil, &out, &errOut); code != 2 {
			t.Fatalf("want exit 2, got %d", code)
		}
		if !strings.Contains(errOut.String(), "usage:") {
			t.Fatalf("want usage message on stderr, got %q", errOut.String())
		}
	})

	// A well-formed file → exit 0, "ok" on stdout, nothing on stderr.
	t.Run("valid file", func(t *testing.T) {
		good := filepath.Join(t.TempDir(), "good.yaml")
		writeFixture(t, good, "apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: ok-script\nspec:\n  run_type: bash\n  command: echo hi\n")
		var out, errOut bytes.Buffer
		if code := validatePaths([]string{good}, &out, &errOut); code != 0 {
			t.Fatalf("want exit 0, got %d (stderr=%q)", code, errOut.String())
		}
		if !strings.Contains(out.String(), "ok") {
			t.Fatalf("want ok on stdout, got %q", out.String())
		}
	})

	// A file with an unknown apiVersion (T10) → exit 1 with a line-numbered
	// "<file>:<line>: …" error — the exact shape the CI template surfaces.
	t.Run("invalid file is line-numbered", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "bad.yaml")
		writeFixture(t, bad, "apiVersion: cronomicon.io/v999\nkind: Script\nmetadata:\n  name: bad-script\nspec:\n  run_type: bash\n  command: echo hi\n")
		var out, errOut bytes.Buffer
		if code := validatePaths([]string{bad}, &out, &errOut); code != 1 {
			t.Fatalf("want exit 1, got %d", code)
		}
		// stderr must carry the file:line prefix (apiVersion is on line 1).
		if !strings.Contains(errOut.String(), bad+":1:") {
			t.Fatalf("want line-numbered %q:1: error, got %q", bad, errOut.String())
		}
	})

	// A repo dir with a dangling script_ref (B-Git) → exit 1; the offending job
	// file is named in the error. Valid sibling job validates clean.
	t.Run("repo dir cross-ref", func(t *testing.T) {
		repo := t.TempDir()
		writeFixture(t, filepath.Join(repo, "scripts", "backup.yaml"),
			"apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: backup\nspec:\n  run_type: bash\n  command: pg_dump\n")
		writeFixture(t, filepath.Join(repo, "jobs", "ok.yaml"),
			"apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: ok\nspec:\n  script_ref: backup\n  scope: Prod\n")
		writeFixture(t, filepath.Join(repo, "jobs", "broken.yaml"),
			"apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: broken\nspec:\n  script_ref: does-not-exist\n  scope: Prod\n")
		var out, errOut bytes.Buffer
		if code := validatePaths([]string{repo}, &out, &errOut); code != 1 {
			t.Fatalf("want exit 1, got %d (stderr=%q)", code, errOut.String())
		}
		if !strings.Contains(errOut.String(), "broken") {
			t.Fatalf("want the broken job named in stderr, got %q", errOut.String())
		}
	})
}

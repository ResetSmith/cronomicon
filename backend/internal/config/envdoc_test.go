package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Why this file exists (LU-14 / §7.3).
//
// Every knob this process reads is part of its operator contract, and a knob
// nobody documented is a knob nobody can find. That failure is silent in both
// directions: the operator does not know the setting exists, and nothing in CI
// notices that the reference is now incomplete. The security plan called for
// this guard and it was never written; four phases of new logging knobs is a
// good moment to stop relying on remembering.
//
// The check is deliberately shallow — presence of the name in the reference —
// because anything deeper (does the documented default match the code's?) would
// need a parser for prose and would break on rewording. Presence is the property
// that actually decays.
//
// Pattern borrowed from TestNoConfigNameUnderReservedPrefix in this package
// (regex over config.go's literals, since that file is the single source of
// truth for config reads) plus the self-expiring allowlist from
// spec_conformance_test.go — an allowlist entry that becomes documented FAILS,
// so the list cannot quietly rot into a permanent exemption list.

// envMatrix is the canonical operator reference for environment configuration.
const envMatrix = "../../deploy/env-matrix.md"

// undocumentedByDesign are names read by config.go that must NOT appear in the
// reference, each with the reason. Anything else missing is a documentation bug.
//
// Keep this list short and justified. If an entry ever becomes documented, the
// test fails and the entry should be deleted — that is the self-expiry.
//
// Currently empty, and that is the right state: the deprecated KEK aliases were
// the obvious candidates, but the matrix documents them alongside their
// replacements, which is more useful to an operator staring at an old deployment
// than hiding them would be.
var undocumentedByDesign = map[string]string{}

// configEnvNames returns every CRONOMICON_* name config.go reads.
func configEnvNames(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	lit := regexp.MustCompile(`"(CRONOMICON_[A-Z0-9_]+)"`)
	seen := map[string]bool{}
	var out []string
	for _, m := range lit.FindAllStringSubmatch(string(src), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	if len(out) < 20 {
		// The regex is the load-bearing part of this test. If a refactor moves the
		// literals somewhere it cannot see, every assertion below would pass
		// vacuously — so assert the extraction itself found a plausible number.
		t.Fatalf("only %d env names extracted from config.go; the literal regex has stopped matching", len(out))
	}
	return out
}

// allEnvNamesInTree collects every CRONOMICON_* literal appearing in non-test Go
// under root, which is the closest thing available to "names this codebase
// actually reads".
func allEnvNamesInTree(t *testing.T, root string) map[string]bool {
	t.Helper()
	lit := regexp.MustCompile(`"(CRONOMICON_[A-Z0-9_]+)"`)
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil //nolint:nilerr // unreadable entries are not evidence either way
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, m := range lit.FindAllStringSubmatch(string(b), -1) {
			found[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return found
}

// TestEveryConfigEnvVarIsDocumented is the drift guard: a knob added to
// config.go without a row in the env matrix fails here, at the moment it is
// added, rather than being discovered by an operator who cannot find it.
func TestEveryConfigEnvVarIsDocumented(t *testing.T) {
	doc, err := os.ReadFile(envMatrix)
	if err != nil {
		t.Fatalf("read %s: %v", envMatrix, err)
	}
	matrix := string(doc)

	var missing []string
	for _, name := range configEnvNames(t) {
		documented := strings.Contains(matrix, name)
		reason, exempt := undocumentedByDesign[name]

		switch {
		case exempt && documented:
			// Self-expiry: the exemption has outlived its reason.
			t.Errorf("%s is listed in undocumentedByDesign (%q) but IS documented in %s — delete the allowlist entry",
				name, reason, envMatrix)
		case exempt:
			// Deliberately absent; nothing to do.
		case !documented:
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d environment variable(s) are read by config.go but absent from %s:\n  %s\n\n"+
			"Add a row for each. If one is deliberately undocumented, add it to undocumentedByDesign with the reason.",
			len(missing), envMatrix, strings.Join(missing, "\n  "))
	}
}

// TestEnvMatrixDocumentsNothingFictional is the reverse direction. A row for a
// variable the code no longer reads is worse than a missing row: an operator
// sets it, observes no effect, and has no way to tell whether they typo'd it or
// the feature is broken. Renames leave exactly this residue.
//
// Scoped to CRONOMICON_* names appearing in the matrix's own table rows, so prose
// mentioning a runner-side or third-party variable is not mistaken for a claim
// about this binary.
func TestEnvMatrixDocumentsNothingFictional(t *testing.T) {
	doc, err := os.ReadFile(envMatrix)
	if err != nil {
		t.Fatalf("read %s: %v", envMatrix, err)
	}
	// Scan the WHOLE backend, not just config.go. config.go is the contract for
	// the forward direction — a knob added there must be documented — but it is
	// not the only place a variable is read: several are pulled with a direct
	// os.Getenv where they are used (the GitLab token, the clone dir), and the
	// runner agent is a separate binary with its own config that this matrix also
	// documents. Scoping this direction to config.go would have reported those as
	// fictional, which is how a correct reference gets "corrected" into a wrong
	// one.
	known := allEnvNamesInTree(t, "..")

	// Only inspect table rows (lines starting with "|"), where a name is a claim
	// that the variable exists, rather than prose that may reference a removed
	// knob historically.
	row := regexp.MustCompile("`(CRONOMICON_[A-Z0-9_]+)`")
	var fictional []string
	seen := map[string]bool{}
	for line := range strings.SplitSeq(string(doc), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		for _, m := range row.FindAllStringSubmatch(line, -1) {
			name := m[1]
			if !known[name] && !seen[name] {
				seen[name] = true
				fictional = append(fictional, name)
			}
		}
	}
	if len(fictional) > 0 {
		sort.Strings(fictional)
		t.Errorf("%s has table rows for %d variable(s) no binary reads:\n  %s\n\n"+
			"Remove the rows, or restore the reads. A documented knob that does nothing is worse than an undocumented one.",
			envMatrix, len(fictional), strings.Join(fictional, "\n  "))
	}
}

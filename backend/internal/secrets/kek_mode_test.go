package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// DR-6 / DR-Q4: a permissive KEK file is two-tier — group-readable warns and
// starts, world-readable refuses. The policy is tested through the pure helper
// rather than VerifyKEKFileMode, which memoises its verdict for the process
// lifetime and so can only be exercised once.
func TestEvaluateKEKFileMode(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name     string
		mode     os.FileMode
		wantWarn bool
		wantErr  bool
	}{
		{"owner-only is the expected posture", 0o400, false, false},
		{"owner rw is still fine", 0o600, false, false},
		{"group-readable warns but starts", 0o440, true, false},
		{"group rw warns but starts", 0o660, true, false},
		{"world-readable is fatal", 0o444, false, true},
		{"the classic 0644 is fatal", 0o644, false, true},
		{"world-execute-only is still an other bit", 0o401, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_"))
			if err := os.WriteFile(path, []byte("dGVzdA=="), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			warn, err := evaluateKEKFileMode(path)
			if tc.wantErr && err == nil {
				t.Fatalf("mode %04o must be refused", tc.mode)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("mode %04o must be accepted: %v", tc.mode, err)
			}
			if warn != tc.wantWarn {
				t.Errorf("mode %04o: warn = %v, want %v", tc.mode, warn, tc.wantWarn)
			}
			if tc.wantErr {
				// The refusal has to be actionable: an operator reading it in a
				// crash-looping container needs the remedy, not just the verdict.
				if !strings.Contains(err.Error(), "chmod 0400") {
					t.Errorf("refusal must name the fix: %v", err)
				}
				if !strings.Contains(err.Error(), path) {
					t.Errorf("refusal must name the file: %v", err)
				}
			}
		})
	}
}

func TestEvaluateKEKFileModeIgnoresMissingFile(t *testing.T) {
	// A Stat failure must not pre-empt loadKEK's own "read KEK file" error, which
	// names a better fix than a permissions verdict could.
	warn, err := evaluateKEKFileMode(filepath.Join(t.TempDir(), "absent"))
	if err != nil || warn {
		t.Fatalf("a missing file is loadKEK's error to report, got warn=%v err=%v", warn, err)
	}
}

func TestVerifyKEKFileModeNoopForEnvKEK(t *testing.T) {
	// An env-supplied KEK has no file and no mode; the startup refusal has
	// nothing to say about it (the manual covers why the file form is preferred).
	if err := VerifyKEKFileMode(nil); err != nil {
		t.Fatalf("nil config must be a no-op: %v", err)
	}
}

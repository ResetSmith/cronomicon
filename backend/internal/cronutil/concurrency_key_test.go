package cronutil_test

import (
	"testing"

	"github.com/ResetSmith/cronomicon/internal/cronutil"
)

// R2-3 — the precedence the six gate-key producers share.
func TestConcurrencyKeyPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		custom, uid, source, jobN string
		want                      string
	}{
		{"uid is the default", "", "uid-abc", "git", "backup", "uid-abc"},
		{"operator key outranks the uid", "shared-gate", "uid-abc", "git", "backup", "shared-gate"},
		{"operator key outranks the fallback too", "shared-gate", "", "git", "backup", "shared-gate"},
		{"falls back to source/name without a uid", "", "", "git", "backup", "git/backup"},
		// The important negative: never empty. An empty key is not "no gate
		// configured", it silently REMOVES the run from the partial unique index,
		// turning a Forbid job into an Allow one.
		{"never empty", "", "", "", "", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cronutil.ConcurrencyKey(tc.custom, tc.uid, tc.source, tc.jobN); got != tc.want {
				t.Errorf("ConcurrencyKey(%q,%q,%q,%q) = %q, want %q",
					tc.custom, tc.uid, tc.source, tc.jobN, got, tc.want)
			}
		})
	}
}

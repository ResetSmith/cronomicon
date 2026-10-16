package cronutil_test

import (
	"testing"

	"github.com/ResetSmith/cronomicon/internal/cronutil"
)

// R2-3 — the precedence the six gate-key producers share. And question 56
// (2.4.2): an operator's key is shared within the job's agency, so two
// agencies' keys of one name are two gates.
func TestConcurrencyKeyPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		custom, agency, uid, source, jobN string
		want                              string
	}{
		{"uid is the default", "", "ag-fin", "uid-abc", "git", "backup", "uid-abc"},
		{"operator key outranks the uid, within its agency", "shared-gate", "ag-fin", "uid-abc", "git", "backup", "ag-fin:shared-gate"},
		{"another agency's key of the name is another gate", "shared-gate", "ag-tax", "uid-def", "git", "backup", "ag-tax:shared-gate"},
		{"operator key outranks the fallback too", "shared-gate", "ag-fin", "", "git", "backup", "ag-fin:shared-gate"},
		{"an operator key with no agency told is as written", "shared-gate", "", "uid-abc", "git", "backup", "shared-gate"},
		{"falls back to source/name without a uid", "", "ag-fin", "", "git", "backup", "git/backup"},
		// The important negative: never empty. An empty key is not "no gate
		// configured", it silently REMOVES the run from the partial unique index,
		// turning a Forbid job into an Allow one.
		{"never empty", "", "", "", "", "", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cronutil.ConcurrencyKey(tc.custom, tc.agency, tc.uid, tc.source, tc.jobN); got != tc.want {
				t.Errorf("ConcurrencyKey(%q,%q,%q,%q,%q) = %q, want %q",
					tc.custom, tc.agency, tc.uid, tc.source, tc.jobN, got, tc.want)
			}
		})
	}
}

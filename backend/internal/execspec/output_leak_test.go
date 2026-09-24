package execspec

import "testing"

// TestFirstOutputLeakingSecret unit-checks the detector: contains-match, empty
// dictionaries, and deterministic (sorted) offender selection. It moved here from
// internal/runner when the helper was promoted to execspec so both execution
// paths (runner ingest + in-app SSH executor) share one detector (SU-1).
func TestFirstOutputLeakingSecret(t *testing.T) {
	secrets := []string{"s3cr3t"}
	if got := FirstOutputLeakingSecret(map[string]string{"A": "prefix-s3cr3t-suffix"}, secrets); got != "A" {
		t.Errorf("contains-match not detected, got %q", got)
	}
	if got := FirstOutputLeakingSecret(map[string]string{"A": "clean"}, secrets); got != "" {
		t.Errorf("false positive on clean output, got %q", got)
	}
	if got := FirstOutputLeakingSecret(map[string]string{"A": "x"}, nil); got != "" {
		t.Errorf("empty dictionary must never flag, got %q", got)
	}
	if got := FirstOutputLeakingSecret(nil, secrets); got != "" {
		t.Errorf("no outputs must never flag, got %q", got)
	}
	// An empty-string secret must not match everything.
	if got := FirstOutputLeakingSecret(map[string]string{"A": "anything"}, []string{""}); got != "" {
		t.Errorf("empty-string secret must not match, got %q", got)
	}
	// Deterministic: the alphabetically-first leaking output wins.
	got := FirstOutputLeakingSecret(map[string]string{"ZZZ": "s3cr3t", "AAA": "s3cr3t"}, secrets)
	if got != "AAA" {
		t.Errorf("expected sorted-first offender AAA, got %q", got)
	}
}

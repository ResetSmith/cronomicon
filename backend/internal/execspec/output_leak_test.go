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

// ParseRunnerOutputMarker finds a marker behind the "[host] " prefix an agent
// puts on every remote line, and nowhere else.
func TestParseRunnerOutputMarker(t *testing.T) {
	for _, tc := range []struct {
		line, key, value string
		ok               bool
	}{
		{"::cronomicon-output name=A::1", "A", "1", true},
		{"[web-01] ::cronomicon-output name=A::1", "A", "1", true},
		{"[10.0.0.5] ::cronomicon-output name=A::has ] and [ in it", "A", "has ] and [ in it", true},
		{"[web-01] ::cronomicon-output name=A::", "A", "", true},
		{"[web-01] ::cronomicon-output name=A::1\r\n", "A", "1", true},
		// One prefix, once: a second one is the job's own text.
		{"[web-01] [web-02] ::cronomicon-output name=A::1", "", "", false},
		{"[web-01]::cronomicon-output name=A::1", "", "", false},
		{"web-01] ::cronomicon-output name=A::1", "", "", false},
		{"[web-01] echo ::cronomicon-output name=A::1", "", "", false},
		{"[web-01] ::cronomicon-output name=1BAD::1", "", "", false},
		{"[web-01", "", "", false},
		{"", "", "", false},
	} {
		k, v, ok := ParseRunnerOutputMarker(tc.line, true)
		if k != tc.key || v != tc.value || ok != tc.ok {
			t.Errorf("ParseRunnerOutputMarker(%q) = %q, %q, %v; want %q, %q, %v", tc.line, k, v, ok, tc.key, tc.value, tc.ok)
		}
	}
	// On a run the agent does not prefix (ansible, terraform), nothing is read
	// past: a bracketed tag there is the tool's or the job's own text, and data
	// echoed after it must not become an output. A bare marker still counts.
	if _, _, ok := ParseRunnerOutputMarker("[INFO] ::cronomicon-output name=A::1", false); ok {
		t.Error("a marker behind a bracketed tag was captured on a run the agent does not prefix")
	}
	if k, v, ok := ParseRunnerOutputMarker("::cronomicon-output name=A::1", false); !ok || k != "A" || v != "1" {
		t.Errorf("a bare marker on an unprefixed run = %q, %q, %v", k, v, ok)
	}
	for runType, want := range map[string]bool{"bash": true, "python": true, "perl": true, "powershell": true, "ansible": false, "terraform": false} {
		if got := AgentPrefixesOutput(runType); got != want {
			t.Errorf("AgentPrefixesOutput(%q) = %v, want %v", runType, got, want)
		}
	}
}

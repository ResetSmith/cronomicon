package api_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"gopkg.in/yaml.v3"
)

// TestConcurrencyPolicyEnumsAreHonoured is the guard QP found missing.
//
// `Replace` was accepted by the spec, accepted by both write paths, stored on
// the row — and read by nothing. Every gate in the tree compared against
// "Forbid". So a job configured never to overlap, using the value that sounds
// like it does something stricter, overlapped freely for releases. Nobody
// noticed, because a concurrency policy that silently means Allow is
// indistinguishable from a system that never had two runs at once.
//
// That is the same shape as the VF-15 alert-enum defect, and it gets the same
// treatment: assert BOTH directions between the spec and the implementation.
// The alert enums have had this guard since J-6; concurrency policy did not,
// which is precisely why it drifted.
func TestConcurrencyPolicyEnumsAreHonoured(t *testing.T) {
	specValues := loadConcurrencyPolicyEnum(t)

	// ── spec → implementation ────────────────────────────────────────────────
	for _, v := range specValues {
		if !cronutil.ValidPolicy(v) {
			t.Errorf("openapi.yaml accepts concurrencyPolicy %q but cronutil.ValidPolicy rejects it — "+
				"a job created with it would be silently coerced to Allow, which is exactly how "+
				"Replace survived for releases while doing nothing", v)
		}
	}

	// ── implementation → spec ────────────────────────────────────────────────
	inSpec := map[string]bool{}
	for _, v := range specValues {
		inSpec[v] = true
	}
	for _, v := range cronutil.ConcurrencyPolicies {
		if !inSpec[v] {
			t.Errorf("cronutil honours concurrencyPolicy %q but openapi.yaml forbids it — "+
				"working behaviour the UI and API cannot reach", v)
		}
	}

	// Strength check: if the loader stops finding the enum, every assertion
	// above passes vacuously forever.
	if len(specValues) < 3 {
		t.Fatalf("only %d policy values found in the spec; the loader has stopped working", len(specValues))
	}

	// The removed value must be gone from BOTH sides, or it comes back by
	// accident the next time someone copies a nearby enum.
	if cronutil.ValidPolicy("Replace") {
		t.Error("cronutil still honours Replace; it was removed in 940 because nothing implemented it")
	}
	if inSpec["Replace"] {
		t.Error("openapi.yaml still offers Replace")
	}
}

// loadConcurrencyPolicyEnum reads every concurrencyPolicy enum in the frozen
// spec — there is more than one (the job read model and the compose input), and
// a guard that checked only the first would miss exactly the drift that lets two
// surfaces disagree.
func loadConcurrencyPolicyEnum(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	seen := map[string]bool{}
	var out []string
	var walk func(node any, key string)
	walk = func(node any, key string) {
		switch n := node.(type) {
		case map[string]any:
			if key == "concurrencyPolicy" {
				if vals, ok := n["enum"].([]any); ok {
					for _, v := range vals {
						if sv, ok := v.(string); ok && !seen[sv] {
							seen[sv] = true
							out = append(out, sv)
						}
					}
				}
			}
			for k, v := range n {
				walk(v, k)
			}
		case []any:
			for _, v := range n {
				walk(v, key)
			}
		}
	}
	walk(doc, "")
	return out
}

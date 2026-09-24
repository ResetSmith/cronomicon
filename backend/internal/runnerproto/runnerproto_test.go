package runnerproto

import "testing"

// TestConfigDigestCanonicalization: the digest is order-insensitive over
// capabilities and bakes in the server-side normalization (maxConcurrent
// <= 0 → 5, inventory "" → "amadeus") so agent and server agree by
// construction; any real field change produces a different digest.
func TestConfigDigestCanonicalization(t *testing.T) {
	base := ConfigDigest("r1", "Linux", []string{"bash", "ansible", "checkout"}, 5, "amadeus", "1.0", 4)

	// Capability order must not matter (detection order is unstable).
	if got := ConfigDigest("r1", "Linux", []string{"checkout", "bash", "ansible"}, 5, "amadeus", "1.0", 4); got != base {
		t.Errorf("capability order changed the digest")
	}
	// Normalization: 0 maxConcurrent == the server default 5; "" inventory == "amadeus".
	if got := ConfigDigest("r1", "Linux", []string{"bash", "ansible", "checkout"}, 0, "", "1.0", 4); got != base {
		t.Errorf("normalized defaults (0 → 5, \"\" → amadeus) must digest identically")
	}

	// Real changes must change the digest.
	changed := map[string]string{
		"name":            ConfigDigest("r2", "Linux", []string{"bash", "ansible", "checkout"}, 5, "amadeus", "1.0", 4),
		"capabilities":    ConfigDigest("r1", "Linux", []string{"bash", "ansible"}, 5, "amadeus", "1.0", 4),
		"maxConcurrent":   ConfigDigest("r1", "Linux", []string{"bash", "ansible", "checkout"}, 8, "amadeus", "1.0", 4),
		"inventory":       ConfigDigest("r1", "Linux", []string{"bash", "ansible", "checkout"}, 5, "local", "1.0", 4),
		"version":         ConfigDigest("r1", "Linux", []string{"bash", "ansible", "checkout"}, 5, "amadeus", "2.0", 4),
		"protocolVersion": ConfigDigest("r1", "Linux", []string{"bash", "ansible", "checkout"}, 5, "amadeus", "1.0", 5),
	}
	for field, got := range changed {
		if got == base {
			t.Errorf("changing %s did not change the digest", field)
		}
	}

	// Field framing: values must not bleed across field boundaries.
	a := ConfigDigest("ab", "c", nil, 5, "amadeus", "1.0", 4)
	b := ConfigDigest("a", "bc", nil, 5, "amadeus", "1.0", 4)
	if a == b {
		t.Errorf("field framing broken: adjacent fields bled together")
	}
}

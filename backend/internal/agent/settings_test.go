package agent

import (
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

//go:fix inline

//go:fix inline

//go:fix inline

//go:fix inline

func TestSettingsStoreApplyAndAck(t *testing.T) {
	s := &settingsStore{}
	if s.applied() != 0 {
		t.Fatalf("fresh store applied = %d, want 0", s.applied())
	}
	// Applying v0 (the default) is a no-op — nothing to ack.
	if s.apply(&runnerproto.PollSettings{Version: 0}) {
		t.Errorf("applying version 0 should be a no-op")
	}
	// Applying v2 advances the ack.
	if !s.apply(&runnerproto.PollSettings{Version: 2, Values: runnerproto.PollSettingsValues{MaxConcurrent: new(9)}}) {
		t.Errorf("applying a newer version should return true")
	}
	if s.applied() != 2 {
		t.Errorf("applied = %d, want 2", s.applied())
	}
	// Re-applying the same version is a no-op (server keeps sending until acked).
	if s.apply(&runnerproto.PollSettings{Version: 2}) {
		t.Errorf("re-applying the current version should be a no-op")
	}
}

func TestSettingsStoreEffectiveValues(t *testing.T) {
	s := &settingsStore{}
	// No overrides → local values pass through.
	if got := s.maxConcurrent(5); got != 5 {
		t.Errorf("maxConcurrent(local) = %d, want 5", got)
	}
	base := Config{MaxConcurrent: 5, SandboxMemoryMax: "1G", AllowCheckout: false, CheckoutRepos: []string{"local"}}
	if eff := s.effectiveConfig(base); eff.SandboxMemoryMax != "1G" || eff.AllowCheckout {
		t.Errorf("no-override effectiveConfig changed base: %+v", eff)
	}

	// Apply overrides.
	s.apply(&runnerproto.PollSettings{Version: 1, Values: runnerproto.PollSettingsValues{
		MaxConcurrent:    new(12),
		SandboxMemoryMax: new("4G"),
		AllowCheckout:    new(true),
		CheckoutRepos:    new([]string{"https://gitlab/x.git"}),
		CapabilityMask:   []string{"python"},
	}})
	if got := s.maxConcurrent(5); got != 12 {
		t.Errorf("managed maxConcurrent = %d, want 12", got)
	}
	eff := s.effectiveConfig(base)
	if eff.SandboxMemoryMax != "4G" || !eff.AllowCheckout || len(eff.CheckoutRepos) != 1 || eff.CheckoutRepos[0] != "https://gitlab/x.git" {
		t.Errorf("effectiveConfig did not apply overrides: %+v", eff)
	}
	// The base value must be untouched (snapshot semantics).
	if base.SandboxMemoryMax != "1G" || base.AllowCheckout {
		t.Errorf("effectiveConfig mutated the base config: %+v", base)
	}
	if !s.masks("python") || s.masks("bash") {
		t.Errorf("mask: python should be masked, bash not")
	}
}

func TestSettingsStoreNilSafe(t *testing.T) {
	var s *settingsStore // nil — a zero-value executor/agent has no store yet
	if s.applied() != 0 || s.maxConcurrent(7) != 7 || s.masks("bash") || s.apply(nil) {
		t.Errorf("nil store must be inert")
	}
	base := Config{SandboxMemoryMax: "2G"}
	if eff := s.effectiveConfig(base); eff.SandboxMemoryMax != "2G" {
		t.Errorf("nil store effectiveConfig must return base unchanged")
	}
}

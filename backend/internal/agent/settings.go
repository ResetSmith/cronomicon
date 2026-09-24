package agent

import (
	"slices"
	"sync"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// settingsStore holds the server-managed operational overrides the agent has
// applied (runner-install-update-2.md Phase 4). The poll goroutine writes it
// (apply); run goroutines read it (the accessors), so it is mutex-guarded —
// unlike the digest, which stays single-goroutine.
//
// Nothing here feeds the config digest: the digest hashes DECLARED config, so a
// managed override never looks like drift. Overrides layer on top of the local
// declared config per read; they never mutate a.cfg / a.exec.cfg.
type settingsStore struct {
	mu    sync.RWMutex
	acked int // the version the agent has applied (echoed as the poll ack)
	vals  runnerproto.PollSettingsValues
}

// applied returns the version the agent currently reflects — the value it sends
// back as the settingsVersion poll param. Nil-safe (a zero-value executor/agent
// in tests has no store yet).
func (s *settingsStore) applied() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.acked
}

// apply installs a settings payload and returns true if it advanced the applied
// version. Re-applying the current version is a no-op (the server keeps sending
// until it sees the ack). A payload with empty Values but a newer version is a
// real change — the operator cleared overrides, so the agent reverts to local.
func (s *settingsStore) apply(p *runnerproto.PollSettings) bool {
	if s == nil || p == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Version == s.acked {
		return false
	}
	s.vals = p.Values
	s.acked = p.Version
	return true
}

// maxConcurrent returns the effective concurrency limit: the managed override
// when set (and positive), else the local declared value.
func (s *settingsStore) maxConcurrent(local int) int {
	if s == nil {
		return local
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.vals.MaxConcurrent != nil && *s.vals.MaxConcurrent > 0 {
		return *s.vals.MaxConcurrent
	}
	return local
}

// masks reports whether a run-type is removed by the server's capability mask.
// The server already refuses to assign masked types; this is agent-side
// symmetry (and a guard during a network partition where a stale assignment
// could arrive).
func (s *settingsStore) masks(runType string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Contains(s.vals.CapabilityMask, runType)
}

// effectiveConfig returns base with the managed sandbox/checkout overrides
// layered on — a per-run snapshot, so the executor's existing lock-free reads
// of the returned value are race-free (the value doesn't change mid-run).
func (s *settingsStore) effectiveConfig(base Config) Config {
	if s == nil {
		return base
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	v := s.vals
	if v.SandboxMemoryMax != nil {
		base.SandboxMemoryMax = *v.SandboxMemoryMax
	}
	if v.SandboxCPUQuota != nil {
		base.SandboxCPUQuota = *v.SandboxCPUQuota
	}
	if v.SandboxTasksMax != nil {
		base.SandboxTasksMax = *v.SandboxTasksMax
	}
	if v.AllowCheckout != nil {
		base.AllowCheckout = *v.AllowCheckout
	}
	if v.CheckoutRepos != nil {
		base.CheckoutRepos = *v.CheckoutRepos
	}
	return base
}

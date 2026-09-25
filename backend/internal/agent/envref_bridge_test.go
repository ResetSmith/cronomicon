package agent

import (
	"slices"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

func envHas(env []string, key, val string) bool {
	return slices.Contains(env, key+"="+val)
}

// TestResolveKeyPathStripsCronomiconKeyPrefix proves the agent key resolver treats
// an CRONOMICON_KEY_<bare> reference as the bare key-map/key-dir name (W3): files and
// map entries are never renamed.
func TestResolveKeyPathStripsCronomiconKeyPrefix(t *testing.T) {
	dir, keyPath := keyDirWith(t, "ansible_rh8_key")

	if p, src, ok := resolveKeyPath(nil, dir, "CRONOMICON_KEY_ansible_rh8_key"); !ok || p != keyPath || src != "key-dir" {
		t.Errorf("prefixed key-dir lookup = (%q,%q,%v), want (%q,key-dir,true)", p, src, ok, keyPath)
	}
	// Bare name still resolves (transition).
	if p, _, ok := resolveKeyPath(nil, dir, "ansible_rh8_key"); !ok || p != keyPath {
		t.Errorf("bare key-dir lookup = (%q,%v), want (%q,true)", p, ok, keyPath)
	}
	// Prefixed key-map entry resolves to the bare map key.
	if p, src, ok := resolveKeyPath(map[string]string{"deploy": "/keys/deploy"}, "", "CRONOMICON_KEY_deploy"); !ok || p != "/keys/deploy" || src != "key-map" {
		t.Errorf("prefixed key-map lookup = (%q,%q,%v), want (/keys/deploy,key-map,true)", p, src, ok)
	}
}

// TestBuildChildEnvPrefixedToBareFallback proves the env bridge resolves a
// value reference (CRONOMICON_SECRET_/CRONOMICON_VAR_) that is absent under its prefixed
// name from the BARE name in the runner env — so an updated inventory works
// against an un-migrated secrets.env (N-D5 decoupling).
func TestBuildChildEnvPrefixedToBareFallback(t *testing.T) {
	m := &runnerproto.ManifestResponse{RunType: "ansible", EnvPassthrough: []string{"CRONOMICON_SECRET_DB_PW"}}
	out, prov, err := buildChildEnv(m, Config{}, []string{"DB_PW=s3cr3t"}, nil)
	if err != nil {
		t.Fatalf("buildChildEnv: %v", err)
	}
	if !envHas(out, "CRONOMICON_SECRET_DB_PW", "s3cr3t") {
		t.Fatalf("want CRONOMICON_SECRET_DB_PW=s3cr3t via bare fallback, got %v", out)
	}
	var sawProv bool
	for _, p := range prov {
		if strings.Contains(p, "prefixed→bare fallback") {
			sawProv = true
		}
	}
	if !sawProv {
		t.Errorf("expected a prefixed→bare provenance line, got %v", prov)
	}
}

// TestBuildChildEnvPrefixedWinsOverBare: an explicit prefixed value in the runner
// env is used directly (no fallback).
func TestBuildChildEnvPrefixedWinsOverBare(t *testing.T) {
	m := &runnerproto.ManifestResponse{RunType: "ansible", EnvPassthrough: []string{"CRONOMICON_SECRET_X"}}
	out, _, err := buildChildEnv(m, Config{}, []string{"CRONOMICON_SECRET_X=prefixed", "X=bare"}, nil)
	if err != nil {
		t.Fatalf("buildChildEnv: %v", err)
	}
	if !envHas(out, "CRONOMICON_SECRET_X", "prefixed") {
		t.Fatalf("prefixed value must win, got %v", out)
	}
}

// TestBuildChildEnvKeyReferenceResolvesToPath: an CRONOMICON_KEY_<bare> passthrough
// resolves via the key bridge to the key-dir FILE path (not a value).
func TestBuildChildEnvKeyReferenceResolvesToPath(t *testing.T) {
	dir, keyPath := keyDirWith(t, "mykey")
	m := &runnerproto.ManifestResponse{RunType: "ansible", EnvPassthrough: []string{"CRONOMICON_KEY_mykey"}}
	out, _, err := buildChildEnv(m, Config{KeyDir: dir}, nil, nil)
	if err != nil {
		t.Fatalf("buildChildEnv: %v", err)
	}
	if !envHas(out, "CRONOMICON_KEY_mykey", keyPath) {
		t.Fatalf("want CRONOMICON_KEY_mykey=%s, got %v", keyPath, out)
	}
}

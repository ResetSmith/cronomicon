package api

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenAPIMirrorByteIdentical guards the two openapi.yaml copies against drift
// (§13.1-B / §13.2 Phase 7). backend/openapi.yaml is canonical (oapi-codegen and
// the frontend client read it); the repo-root /openapi.yaml is an unguarded mirror
// — until now. Every contract change must update both; this test fails if they
// diverge by a single byte.
func TestOpenAPIMirrorByteIdentical(t *testing.T) {
	backend, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read backend/openapi.yaml: %v", err)
	}
	root, err := os.ReadFile(filepath.Join("..", "..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read root /openapi.yaml: %v", err)
	}
	if !bytes.Equal(backend, root) {
		t.Fatalf("openapi.yaml copies diverged (%d vs %d bytes) — re-run `cp backend/openapi.yaml openapi.yaml` after any contract change",
			len(backend), len(root))
	}
}

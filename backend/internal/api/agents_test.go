package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// Agent binary distribution (provisioning D1, Phase 3): allowlisted filenames
// served from CRONOMICON_AGENT_DIR; everything else — unknown names, unbundled
// deployments — is a clean 404 with a distinguishing error code.

func agentTestServer(t *testing.T, dir string) *Server {
	t.Helper()
	return &Server{cfg: &config.Config{AgentDir: dir}}
}

func getAgent(t *testing.T, s *Server, name string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/agents/"+name, nil)
	req.SetPathValue("filename", name)
	rec := httptest.NewRecorder()
	s.handleAgentDownload(rec, req)
	return rec
}

func TestAgentDownloadServesAllowlistedFile(t *testing.T) {
	dir := t.TempDir()
	content := []byte("#!fake-binary\n")
	if err := os.WriteFile(filepath.Join(dir, "cronomicon-runner-linux-amd64"), content, 0o755); err != nil {
		t.Fatal(err)
	}

	rec := getAgent(t, agentTestServer(t, dir), "cronomicon-runner-linux-amd64")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != string(content) {
		t.Errorf("body = %q, want the file content", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="cronomicon-runner-linux-amd64"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

func TestAgentDownloadServesChecksums(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte("abc  cronomicon-runner-linux-amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := getAgent(t, agentTestServer(t, dir), "SHA256SUMS")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

func TestAgentDownloadRejectsUnknownFilename(t *testing.T) {
	dir := t.TempDir()
	// Present on disk but NOT allowlisted — must still 404 (no directory serving).
	if err := os.WriteFile(filepath.Join(dir, "secrets.env"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"secrets.env", "cronomicon-runner-windows-amd64.exe", "..", "index.html"} {
		rec := getAgent(t, agentTestServer(t, dir), name)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /agents/%s = %d, want 404", name, rec.Code)
		}
		var body struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Code != "not_found" {
			t.Errorf("GET /agents/%s error code = %q, want not_found (body: %s)", name, body.Code, rec.Body.String())
		}
	}
}

func TestAgentDownloadUnbundledDeployment(t *testing.T) {
	// Allowlisted name, but the agent dir doesn't exist (bare-metal build).
	rec := getAgent(t, agentTestServer(t, filepath.Join(t.TempDir(), "missing")), "cronomicon-runner-linux-arm64")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("non-JSON 404 body: %s", rec.Body.String())
	}
	if body.Code != "agents_not_bundled" {
		t.Errorf("error code = %q, want agents_not_bundled", body.Code)
	}
}

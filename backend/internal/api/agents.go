package api

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// Runner-agent binary distribution (runner provisioning plan D1, Phase 3).
//
// GET /agents/{filename} serves the cross-compiled amadeus-runner binaries and
// their SHA256SUMS from a directory baked into the server image (Dockerfile
// `agents` stage → /usr/share/amadeus/agents, overridable via
// CRONOMICON_AGENT_DIR). A runner host by definition reaches the Cronomicon server,
// so this removes the "build the binary yourself / reach GitLab" prerequisite:
// runner-install.sh --download fetches from here and verifies the checksum.
//
// Unauthenticated BY DESIGN (like /runner-install.sh and the SPA assets): the
// binary is not a secret — it's the same artifact any operator can build from
// source — and the install flow runs before any credential exists on the host.
// Registration still requires a token; serving the binary grants nothing.
// Rationale recorded in deploy/security-review.md.
//
// The filename is allowlisted (no directory serving, no path traversal): only
// the published linux binaries (D5: amd64 + arm64 — no Windows install story
// yet) and the checksum file.

var agentFiles = map[string]string{
	"amadeus-runner-linux-amd64": "application/octet-stream",
	"amadeus-runner-linux-arm64": "application/octet-stream",
	"SHA256SUMS":                 "text/plain; charset=utf-8",
}

func (s *Server) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("filename")
	contentType, ok := agentFiles[name]
	if !ok {
		httpx.Fail(w, http.StatusNotFound, "not_found",
			"unknown agent artifact — published files are amadeus-runner-linux-{amd64,arm64} and SHA256SUMS")
		return
	}

	path := filepath.Join(s.cfg.AgentDir, name)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		// Bare-metal / source-built deployments don't bundle the binaries.
		httpx.Fail(w, http.StatusNotFound, "agents_not_bundled",
			"this deployment does not bundle runner-agent binaries — build one yourself: CGO_ENABLED=0 go build ./cmd/amadeus-runner (see /runner-install.html §2), or set CRONOMICON_AGENT_DIR")
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	// ServeFile handles Content-Length, ranges, and conditional requests.
	http.ServeFile(w, r, path)
}

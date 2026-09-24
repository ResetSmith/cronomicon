package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"golang.org/x/crypto/ssh"
)

// inTestSSHServer stands up a minimal x/crypto/ssh server that accepts the given
// client public key and, on exec, echoes a line + exits 0. Modeled on the
// pattern in internal/sshexec/sshexec_test.go. Returns its address and host key.
func inTestSSHServer(t *testing.T, clientPub ssh.PublicKey) (addr string, hostKey ssh.PublicKey) {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(clientPub.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, io.EOF
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSSHConn(conn, cfg)
		}
	}()
	return ln.Addr().String(), hostSigner.PublicKey()
}

func serveSSHConn(nConn net.Conn, cfg *ssh.ServerConfig) {
	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				if req.Type == "exec" {
					req.Reply(true, nil)
					io.WriteString(ch, "ran the job\n")
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
					ch.Close()
					return
				}
				req.Reply(false, nil)
			}
		}()
	}
}

// fakeServer fakes the four Cronomicon endpoints the agent drives: register, poll,
// manifest, and log ingest. It hands out one assignment then 204s.
type fakeServer struct {
	mu sync.Mutex

	manifest runnerproto.ManifestResponse
	assigned bool // true once the assignment has been handed out

	// captured outcomes
	registered  bool
	logBody     []byte
	logComplete bool
}

func (s *fakeServer) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/runners/register", s.handleRegister)
	mux.HandleFunc("GET /api/v1/runners/{id}/poll", s.handlePoll)
	mux.HandleFunc("GET /api/v1/runs/{traceId}/manifest", s.handleManifest)
	mux.HandleFunc("POST /api/v1/runs/{traceId}/log", s.handleLog)
	return mux
}

func (s *fakeServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer test-reg-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	// Assert the agent sent a protocolVersion (R0.2 handshake).
	if _, ok := body["protocolVersion"]; !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.registered = true
	s.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"runner": map[string]any{"id": "runner-1"},
		"apiKey": "amt_run_fake",
	})
}

func (s *fakeServer) handlePoll(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer amt_run_fake" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.assigned {
		s.assigned = true
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(runnerproto.PollResponse{
			Assignment: &runnerproto.PollAssignment{
				TraceID: s.manifest.TraceID,
				JobName: s.manifest.JobName,
				RunType: s.manifest.RunType,
				Scope:   s.manifest.Scope,
			},
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *fakeServer) handleManifest(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer amt_run_fake" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(s.manifest)
}

func (s *fakeServer) handleLog(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer amt_run_fake" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// Honor resume offset like the real server.
	s.mu.Lock()
	have := int64(len(s.logBody))
	s.mu.Unlock()
	if hdr := r.Header.Get("X-Resume-Offset"); hdr != "" {
		off, _ := strconv.ParseInt(hdr, 10, 64)
		if off != have {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"persistedOffset": have})
			return
		}
	}
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.logBody = append(s.logBody, body...)
	if strings.Contains(string(s.logBody), `"endedAt"`) {
		s.logComplete = true
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// TestAgentReRegisterOp drives the protocol v4 resync round-trip: the server
// delivers PollControl{Op:"re-register"} and the agent must POST an
// id-preserving redeclare authenticated with its EXISTING runner API key —
// never the registration token — carrying the full declared config, and keep
// polling with the same identity afterwards (no discard, no fresh register).
func TestAgentReRegisterOp(t *testing.T) {
	var (
		mu             sync.Mutex
		registers      int
		redeclareAuth  string
		redeclareBody  map[string]any
		pollsAfterRDcl int
		opSent         bool
		redeclared     bool
		lastPollDigest string
	)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/runners/register", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		registers++
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runner": map[string]any{"id": "runner-rr"},
			"apiKey": "amt_run_rr",
		})
	})
	mux.HandleFunc("GET /api/v1/runners/{id}/poll", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		lastPollDigest = r.URL.Query().Get("configDigest")
		if !opSent {
			opSent = true
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(runnerproto.PollResponse{
				Control: []runnerproto.PollControl{{Op: "re-register"}},
			})
			return
		}
		if redeclared {
			pollsAfterRDcl++
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/runners/{id}/redeclare", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		redeclareAuth = r.Header.Get("Authorization")
		redeclareBody = body
		redeclared = true
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("id")})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := Config{
		ServerURL:         srv.URL,
		RegistrationToken: "test-reg-token",
		Name:              "rr",
		OS:                "Linux",
		Capabilities:      []string{"bash"},
		MaxConcurrent:     2,
		Inventory:         "amadeus",
		IdentityFile:      filepath.Join(t.TempDir(), "id.json"),
		PollInterval:      20 * time.Millisecond,
		LogRetryBudget:    1,
		NoSandbox:         true,
	}
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()

	deadline := time.After(4 * time.Second)
	for {
		mu.Lock()
		ok := redeclared && pollsAfterRDcl >= 2
		mu.Unlock()
		if ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for redeclare + continued polling")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if registers != 1 {
		t.Errorf("registers = %d, want 1 — the op must NOT trigger a fresh token-based registration", registers)
	}
	if redeclareAuth != "Bearer amt_run_rr" {
		t.Errorf("redeclare auth = %q, want the existing runner key (Bearer amt_run_rr)", redeclareAuth)
	}
	if v, ok := redeclareBody["protocolVersion"].(float64); !ok || int(v) != runnerproto.ProtocolVersion {
		t.Errorf("redeclare protocolVersion = %v, want %d", redeclareBody["protocolVersion"], runnerproto.ProtocolVersion)
	}
	for _, field := range []string{"name", "os", "capabilities", "version", "maxConcurrent", "inventory"} {
		if _, ok := redeclareBody[field]; !ok {
			t.Errorf("redeclare body missing declared field %q", field)
		}
	}

	// Phase 5: every poll carries the declared-config digest, and after the
	// redeclare it matches exactly the set the redeclare body declared —
	// computed with the shared canonicalization, so the server-side drift
	// check sees a converged (quiet) digest.
	if lastPollDigest == "" {
		t.Fatal("poll carried no configDigest (Phase 5 drift detection)")
	}
	var caps []string
	if raw, ok := redeclareBody["capabilities"].([]any); ok {
		for _, v := range raw {
			if sv, ok := v.(string); ok {
				caps = append(caps, sv)
			}
		}
	}
	want := runnerproto.ConfigDigest(
		redeclareBody["name"].(string), redeclareBody["os"].(string), caps,
		int(redeclareBody["maxConcurrent"].(float64)),
		redeclareBody["inventory"].(string), redeclareBody["version"].(string),
		int(redeclareBody["protocolVersion"].(float64)))
	if lastPollDigest != want {
		t.Errorf("poll digest %q does not match the digest of the declared set %q", lastPollDigest, want)
	}
}

// TestAgentHappyPathSSH drives the full agent: register → poll → claim → fetch
// manifest → SSH-execute against an in-test SSH server → stream log + trailing
// envelope. Asserts the agent registers, the log carries host-prefixed output,
// and the envelope reports exit 0.
func TestAgentHappyPathSSH(t *testing.T) {
	// Agent's OWN key (model b): the server only references it by name.
	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, _ := ssh.NewSignerFromKey(clientPriv)
	pemBlock, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "agent.key")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		t.Fatal(err)
	}

	sshAddr, hostKey := inTestSSHServer(t, clientSigner.PublicKey())
	host, portStr, _ := net.SplitHostPort(sshAddr)
	port, _ := strconv.Atoi(portStr)

	// known_hosts so the agent verifies the in-test server strictly. A
	// non-standard port requires the "[host]:port" hostname form that
	// knownhosts.New expects (matching what the ssh dial presents).
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	khHost := host
	if port != 22 {
		khHost = "[" + host + "]:" + portStr
	}
	khLine := khHost + " " + string(ssh.MarshalAuthorizedKey(hostKey))
	if err := os.WriteFile(knownHosts, []byte(khLine), 0o600); err != nil {
		t.Fatal(err)
	}

	fs := &fakeServer{
		manifest: runnerproto.ManifestResponse{
			TraceID:       "trace-ssh-1",
			JobName:       "j1",
			RunType:       "bash",
			Executor:      "runner",
			Interp:        []string{"bash", "-c"},
			Body:          "echo hi",
			InventoryMode: "amadeus",
			Targets: []runnerproto.ManifestTarget{{
				Name:          "testhost",
				Address:       host,
				Port:          port,
				User:          "tester",
				AuthKeyEnvVar: "AGENT_KEY",
			}},
		},
	}
	srv := httptest.NewServer(fs.mux())
	defer srv.Close()

	cfg := Config{
		ServerURL:         srv.URL,
		RegistrationToken: "test-reg-token",
		Name:              "r1",
		OS:                "Linux",
		Capabilities:      []string{"bash"},
		MaxConcurrent:     2,
		Inventory:         "amadeus",
		IdentityFile:      filepath.Join(t.TempDir(), "id.json"),
		PollInterval:      20 * time.Millisecond,
		KeyMap:            map[string]string{"AGENT_KEY": keyPath},
		KnownHostsFile:    knownHosts,
		LogRetryBudget:    5,
		FanOut:            4,
		NoSandbox:         true, // SSH integration test — skip the systemd-run startup probe
	}

	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	// Run the agent until the log is complete, then cancel.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()

	deadline := time.After(4 * time.Second)
	for {
		fs.mu.Lock()
		complete := fs.logComplete
		fs.mu.Unlock()
		if complete {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for log completion")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if !fs.registered {
		t.Error("agent did not register")
	}
	logStr := string(fs.logBody)
	if !strings.Contains(logStr, "[testhost] ran the job") {
		t.Errorf("log missing host-prefixed SSH output:\n%s", logStr)
	}
	// Trailing envelope present with exit 0.
	lines := strings.Split(strings.TrimRight(logStr, "\n"), "\n")
	var env runnerproto.TrailingEnvelope
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &env); err != nil {
		t.Fatalf("trailing line is not an envelope: %q (%v)", lines[len(lines)-1], err)
	}
	if env.ExitCode != 0 {
		t.Errorf("envelope exitCode = %d, want 0", env.ExitCode)
	}
	if env.EndedAt == "" {
		t.Error("envelope missing endedAt")
	}
}

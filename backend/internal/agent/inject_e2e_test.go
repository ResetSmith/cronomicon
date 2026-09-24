package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"golang.org/x/crypto/ssh"
)

// inTestEchoSSHServer is a loopback sshd that, on exec, streams back the received
// command line AND the bytes it received on stdin — so an e2e can inspect what the
// agent actually delivered to the target (the H1 stdin env prelude, incl. the D8
// AMADEUS_KEY_* path) without the material ever appearing in argv.
func inTestEchoSSHServer(t *testing.T, clientPub ssh.PublicKey) (addr string, hostKey ssh.PublicKey) {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
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
			go serveEchoStdin(conn, cfg)
		}
	}()
	return ln.Addr().String(), hostSigner.PublicKey()
}

func serveEchoStdin(nConn net.Conn, cfg *ssh.ServerConfig) {
	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				if req.Type == "exec" {
					_ = req.Reply(true, nil)
					var execReq struct{ Command string }
					_ = ssh.Unmarshal(req.Payload, &execReq)
					stdin, _ := io.ReadAll(ch) // the H1 env prelude (never argv)
					_, _ = io.WriteString(ch, "cmd: "+execReq.Command+"\n")
					if len(stdin) > 0 {
						_, _ = io.WriteString(ch, "stdin:\n"+string(stdin)+"\n")
					}
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
					_ = ch.Close()
					return
				}
				_ = req.Reply(false, nil)
			}
		}()
	}
}

// TestAgentInjectsSecretAndDeliversKeyE2E is the §6 runner-agent e2e for injection +
// D8 key delivery, driven through the REAL agent (register → poll → manifest → SSH
// exec → log). It proves, end to end:
//   - a bound SECRET is delivered on stdin (H1), never in argv;
//   - a bound KEY's material is materialized to a 0600 file OFF the run tree, its
//     PATH exposed as AMADEUS_KEY_<name> (the material itself never reaches the
//     target), and the file is WIPED once the run completes.
func TestAgentInjectsSecretAndDeliversKeyE2E(t *testing.T) {
	// Agent's OWN connection key (model b): the target references it by name.
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

	sshAddr, hostKey := inTestEchoSSHServer(t, clientSigner.PublicKey())
	host, portStr, _ := net.SplitHostPort(sshAddr)
	port, _ := strconv.Atoi(portStr)

	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	khHost := host
	if port != 22 {
		khHost = "[" + host + "]:" + portStr
	}
	if err := os.WriteFile(knownHosts, []byte(khHost+" "+string(ssh.MarshalAuthorizedKey(hostKey))), 0o600); err != nil {
		t.Fatal(err)
	}

	const secretVal = "INJECTED-SECRET-VALUE-XYZ"
	const keyMaterial = "PEM-DELIVERED-KEY-MATERIAL-ABC"
	fs := &fakeServer{
		manifest: runnerproto.ManifestResponse{
			TraceID:       "trace-inj-e2e",
			JobName:       "j1",
			RunType:       "bash",
			Executor:      "runner",
			Interp:        []string{"bash", "-c"},
			Body:          "echo hi",
			InventoryMode: "amadeus",
			Targets: []runnerproto.ManifestTarget{{
				Name: "testhost", Address: host, Port: port, User: "tester", AuthKeyEnvVar: "AGENT_KEY",
			}},
			Secrets: map[string]string{"AMADEUS_SECRET_TOKEN": secretVal},
			Keys: []runnerproto.ManifestKey{
				{Name: "deploy_key", Reference: "AMADEUS_KEY_deploy_key", Material: keyMaterial},
			},
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
	logStr := string(fs.logBody)
	fs.mu.Unlock()

	// H1: env delivered on stdin (bash -s), never argv. The command line (the echoed
	// "cmd: " line, which is host-prefixed) carries no injected value/path.
	for line := range strings.SplitSeq(logStr, "\n") {
		if strings.Contains(line, "cmd: ") && (strings.Contains(line, "AMADEUS_SECRET") || strings.Contains(line, "AMADEUS_KEY")) {
			t.Errorf("injected env leaked onto the command line (argv): %q", line)
		}
	}
	if !strings.Contains(logStr, "cmd: bash -s") {
		t.Errorf("expected the stdin-reader command form:\n%s", logStr)
	}

	// The bound secret is delivered via the stdin export prelude.
	if !strings.Contains(logStr, "export AMADEUS_SECRET_TOKEN='"+secretVal+"'") {
		t.Errorf("bound secret not injected on stdin:\n%s", logStr)
	}

	// D8: the key's PATH is exposed; its MATERIAL never reaches the target.
	if strings.Contains(logStr, keyMaterial) {
		t.Errorf("delivered key MATERIAL reached the target (should stay on the runner):\n%s", logStr)
	}
	deliveredPath := exportValue(logStr, "AMADEUS_KEY_deploy_key")
	if deliveredPath == "" {
		t.Fatalf("AMADEUS_KEY_deploy_key path not exposed on stdin:\n%s", logStr)
	}
	// Off the run tree: a dedicated materialize dir, not the job workdir.
	if !strings.Contains(deliveredPath, "amadeus-keys-") {
		t.Errorf("delivered key path is not in a dedicated key dir: %q", deliveredPath)
	}
	// Wiped after the run: cleanup ran when executor.run returned, before the log
	// completed, so the file must be gone now.
	if _, err := os.Stat(deliveredPath); !os.IsNotExist(err) {
		t.Errorf("delivered key file was not wiped after the run (stat err=%v): %q", err, deliveredPath)
	}
}

// exportValue extracts V from an `export NAME='V'` assignment appearing anywhere in
// s (each streamed line is host-prefixed, so match the marker as a substring). The
// value is single-quoted with no embedded quote — sufficient for the test's paths.
func exportValue(s, name string) string {
	marker := "export " + name + "='"
	for line := range strings.SplitSeq(s, "\n") {
		_, after, ok := strings.Cut(line, marker)
		if !ok {
			continue
		}
		rest := after
		if before, _, ok := strings.Cut(rest, "'"); ok {
			return before
		}
	}
	return ""
}

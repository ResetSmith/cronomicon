// Command cronomicon-runner is the Cronomicon runner agent: an out-of-process worker
// that registers with the Cronomicon server, long-polls for runs the server has
// assigned to it (executor='runner'), executes them, and streams their logs
// back. It is the second execution method alongside the in-app SSH executor —
// it exists to run ansible/terraform (which need a local toolchain the app's
// distroless image lacks) and to reach network-isolated targets the server
// can't.
//
// Key-custody posture — credential model (b) (runners-update.md D1): the server
// NEVER decrypts or ships private-key bytes. The execution manifest carries host
// *references only* (a target's AuthKeyEnvVar is the NAME of an env var / key
// file, never the key itself). This agent holds its OWN keys and resolves those
// names against local custody (a key-map, an env var, or a key directory). No
// secret or KEK material ever flows server→agent.
//
// Lifecycle: register (or resume a persisted identity) → poll on a cadence
// (default 60s, which doubles as heartbeat and the kill/drain control channel)
// → on assignment fetch the manifest, execute (SSH fan-out for bash/perl/
// powershell, or local ansible/terraform), and stream stdout+stderr line-by-line
// to the server, closing with the trailing exit/duration envelope. The agent
// keeps polling while runs execute so it stays heartbeat-fresh and can be
// killed/drained.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agent"
)

// Build metadata, stamped via -ldflags at build time.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("cronomicon-runner %s (commit %s)\n", version, commit)
		return
	}

	// `doctor` is an environment preflight: it resolves the same config the
	// agent would, runs bounded checks, prints a report, and exits non-zero on
	// any FAIL. Safe to wire as a systemd ExecStartPre (add --quick there to
	// skip the slow toolchain/sandbox probes). Turns "installed but never
	// registers" into a labeled report instead of a silent hang.
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		os.Exit(runDoctor(os.Args[2:]))
	}

	cfg, err := agent.Resolve(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cronomicon-runner: "+err.Error())
		os.Exit(2)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log) // so best-effort helpers (e.g. capability probes) log to the same sink
	agent.SetVersion(version)

	a, err := agent.New(cfg, log)
	if err != nil {
		log.Error("agent init failed", "error", err)
		os.Exit(1)
	}

	// Context cancelled on SIGINT/SIGTERM → graceful drain of active runs.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("agent exited with error", "error", err)
		os.Exit(1)
	}
}

// runDoctor resolves config (skipping the "doctor" arg), runs the preflight, and
// returns a process exit code: 0 = all checks passed, 1 = a FAIL (or config
// couldn't even be resolved). A "--quick" arg skips the slow probes.
//
// "doctor --auth NAME [user@host]" switches to the auth-chain probe: it resolves
// the credential NAME the way a run would and, given a target, dials it with the
// agent's own SSH stack — turning the multi-step "why won't this key work" loop
// into one command.
func runDoctor(args []string) int {
	quick := false
	authName := ""
	authTarget := ""
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--quick" || a == "-quick":
			quick = true
		case a == "--auth" || a == "-auth":
			if i+1 < len(args) {
				authName = args[i+1]
				i++
			}
			// An optional target (user@host) is the next non-flag token.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				authTarget = args[i+1]
				i++
			}
		default:
			rest = append(rest, a)
		}
	}
	agent.SetVersion(version)

	cfg, err := agent.Resolve(rest, os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FAIL] config             %s\n", err)
		fmt.Fprintln(os.Stderr, "doctor: configuration could not be resolved")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if authName != "" {
		checks, ok := agent.DoctorAuth(ctx, cfg, authName, authTarget)
		for _, c := range checks {
			fmt.Printf("[%-4s] %-18s %s\n", c.Status, c.Name, c.Detail)
		}
		if ok {
			fmt.Println("doctor: auth chain OK")
			return 0
		}
		fmt.Fprintln(os.Stderr, "doctor: auth check FAILED — resolve the items above")
		return 1
	}

	checks, ok := agent.Doctor(ctx, cfg, quick)
	for _, c := range checks {
		fmt.Printf("[%-4s] %-18s %s\n", c.Status, c.Name, c.Detail)
	}
	if ok {
		fmt.Println("doctor: all checks passed")
		return 0
	}
	fmt.Fprintln(os.Stderr, "doctor: one or more checks FAILED — resolve the items above before the runner can register")
	return 1
}

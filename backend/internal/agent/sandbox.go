package agent

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// sandboxProbeTimeout bounds the startup probe. A systemd-run that can't create
// a scope promptly (e.g. no reachable manager — it retries the bus for ~25s
// before failing) is not usable for per-run wrapping anyway, and the agent must
// never hang at startup waiting to find out.
const sandboxProbeTimeout = 3 * time.Second

// Tier 2 host-native sandbox (ansible-update.md RX.10/§6.3). A local-toolchain
// process (ansible/terraform, checkout AND body-only) is wrapped in a per-run
// `systemd-run --scope` transient cgroup that carries the resource caps
// (MemoryMax/CPUQuota/TasksMax) the agent-level unit can't apply per run.
//
// The filesystem/namespace/syscall hardening (ProtectSystem=strict, PrivateTmp,
// NoNewPrivileges, the syscall filter) is NOT re-declared here — a scope unit
// ignores exec directives. It is INHERITED from the agent's own service unit
// (deploy/amadeus-runner.service), which every child process runs inside; the
// per-run workdir lives under the unit's single ReadWritePaths (StateDirectory),
// so the tree is writable and everything else is read-only. The scope adds the
// per-run resource bound (and is where an egress IPAddressAllow would attach).
//
// Availability is DETECTED, never assumed (§6.3): many runner environments lack
// a reachable systemd manager (non-privileged containers, Alpine/OpenRC), so a
// probe actually creates+collects a trivial scope and the result gates wrapping.
// When unavailable the run executes unsandboxed and is reported as such.

// probeSandbox reports whether a usable `systemd-run --scope` is available: the
// binary must exist AND be able to create a scope (a container can ship the
// binary yet have no reachable manager). Non-Linux is always false. Best-effort
// and quick — bounded by ctx.
func probeSandbox(ctx context.Context) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, sandboxProbeTimeout)
	defer cancel()
	ok := runAbandonable(pctx, func() bool {
		// exec.LookPath stats every $PATH entry; a dir on a hung mount (stale NFS
		// / autofs to an unreachable server) blocks that stat uninterruptibly
		// with no context to honor — THIS, not cmd.Run, was the real startup hang
		// — so LookPath must run INSIDE the abandonable section, not before it.
		if _, err := exec.LookPath("systemd-run"); err != nil {
			return false
		}
		cmd := exec.CommandContext(pctx, "systemd-run", "--scope", "--quiet", "--collect", "--", "true")
		cmd.Env = os.Environ()
		// Kill the whole group and force-close pipes on cancel — same backstop as
		// every other agent-spawned process (see exec_local_unix.go).
		configureProcGroup(cmd)
		cmd.WaitDelay = time.Second
		return cmd.Run() == nil
	})
	if !ok && pctx.Err() != nil {
		slog.Warn("sandbox probe did not finish within its deadline — treating systemd-run as unavailable; runner registers and runs unsandboxed",
			"timeout", sandboxProbeTimeout)
	}
	return ok
}

// runAbandonable runs probe and reports its result, but is GUARANTEED to return
// once pctx is done even if probe() never does. Probe work — exec.LookPath's
// stat of $PATH dirs, or a systemd-run wedged reaching an unavailable manager —
// can block in uninterruptible I/O that ignores even a ctx-triggered SIGKILL, so
// running it inline would hang startup before the runner ever registers
// (observed: no logs, then a systemd SIGKILL after 5 min). Startup must never
// block on a best-effort probe: on deadline we abandon it and report the
// negative result; the leaked goroutine and any orphaned process are left for
// the OS to reap.
func runAbandonable(pctx context.Context, probe func() bool) bool {
	done := make(chan bool, 1)
	go func() { done <- probe() }()
	select {
	case ok := <-done:
		return ok
	case <-pctx.Done():
		return false
	}
}

// sandboxWrap prepends the `systemd-run --scope` wrapper (with configured
// resource caps) to argv when the sandbox is available and not disabled. Returns
// the (possibly unchanged) argv and whether it is sandboxed. The wrapped command
// remains an ordinary descendant of the agent, so stdout/stderr/stdin, cwd, env,
// exit code, and the process-group kill all behave exactly as for an unwrapped
// run.
func sandboxWrap(cfg Config, argv []string) (wrapped []string, sandboxed bool) {
	if cfg.NoSandbox || !cfg.SandboxAvailable || runtime.GOOS != "linux" || len(argv) == 0 {
		return argv, false
	}
	out := []string{"systemd-run", "--scope", "--quiet", "--collect"}
	for _, p := range sandboxProps(cfg) {
		out = append(out, "--property="+p)
	}
	out = append(out, "--")
	out = append(out, argv...)
	return out, true
}

// sandboxProps returns the cgroup resource-cap properties for the per-run scope
// (only the properties that actually apply to a scope unit). Empty caps are
// omitted.
func sandboxProps(cfg Config) []string {
	var props []string
	if cfg.SandboxMemoryMax != "" {
		props = append(props, "MemoryMax="+cfg.SandboxMemoryMax)
	}
	if cfg.SandboxCPUQuota != "" {
		props = append(props, "CPUQuota="+cfg.SandboxCPUQuota)
	}
	if cfg.SandboxTasksMax != "" {
		props = append(props, "TasksMax="+cfg.SandboxTasksMax)
	}
	return props
}

// sandboxProvenance returns the per-run provenance line describing the sandbox
// posture (RX.12/§6.3). When unsandboxed AND checkout is enabled it is a loud
// WARNING — an operator must never falsely assume checkout runs are sandboxed.
func sandboxProvenance(cfg Config, sandboxed bool) string {
	if sandboxed {
		caps := sandboxProps(cfg)
		suffix := ""
		if len(caps) > 0 {
			suffix = " [" + strings.Join(caps, " ") + "]"
		}
		return "amadeus: sandbox: systemd-run --scope" + suffix + "; fs/syscall hardening inherited from the agent unit"
	}
	if cfg.AllowCheckout {
		return "amadeus: WARNING: running UNSANDBOXED — no usable systemd-run; this checkout run is NOT resource-capped by a per-run scope (only its timeout applies)"
	}
	return "amadeus: sandbox: unsandboxed (no usable systemd-run; per-run resource caps unavailable)"
}

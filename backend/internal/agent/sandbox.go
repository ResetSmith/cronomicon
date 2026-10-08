package agent

import (
	"bytes"
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
// (deploy/cronomicon-runner.service), which every child process runs inside; the
// per-run workdir lives under the unit's single ReadWritePaths (StateDirectory),
// so the tree is writable and everything else is read-only. The scope adds the
// per-run resource bound (and is where an egress IPAddressAllow would attach).
//
// Availability is DETECTED, never assumed (§6.3): many runner environments lack
// a reachable systemd manager (non-privileged containers, Alpine/OpenRC), so a
// probe actually creates+collects a trivial scope and the result gates wrapping.
// When unavailable the run executes unsandboxed and is reported as such.
//
// THE USUAL RESULT IS "UNAVAILABLE", and not for want of a manager. Creating a
// scope is a request to the system manager, and polkit refuses it to a user
// who is not root: "Interactive authentication required". The installed unit
// runs the agent as its own unprivileged user, so on an ordinary systemd host
// the probe fails, under any hardening and under none, while root passes it
// under all of it (RHEL 8.10, systemd 239, 2026-10-08; an unprivileged user is
// refused the same way by systemd 255). The permission
// that would grant it is the one to manage units, which is more than an agent
// should hold, so no rule is shipped. What bounds such an agent is a limit on
// its unit (sandboxHint); the per-run scope is for an agent that runs as root.

// sandboxHint is what to do about an agent that cannot create a scope. Shared
// by the agent's startup line and the doctor's check.
const sandboxHint = "an agent that does not run as root cannot create a scope (polkit refuses it), and one in a container has no systemd to ask, so its runs have no resource cap of their own; to bound this agent and everything it runs, limit its unit: systemctl set-property <unit> MemoryMax=… CPUQuota=… TasksMax=… (runner-install.sh: --memory-max, --cpu-quota, --tasks-max), or limit the container"

// logNoSandbox is the agent's one startup line about having no per-run
// sandbox. The reason and the remedy ride as attributes: "no usable
// systemd-run" alone read as a missing program on hosts where the request had
// been refused, which is nearly every installed agent. Loud (a warning) only
// when checkout is on, where an operator must never assume runs are capped.
func logNoSandbox(log *slog.Logger, allowCheckout bool, why string) {
	if allowCheckout {
		log.Warn("SANDBOX UNAVAILABLE — -allow-checkout is set but no usable systemd-run scope could be created; checkout runs will execute UNSANDBOXED (only their timeout applies). Each run is reported as unsandboxed; a job may require [sandboxed] to avoid landing here.",
			"reason", why, "hint", sandboxHint)
		return
	}
	log.Info("tier-2 sandbox unavailable (no usable systemd-run scope) — local-toolchain runs execute unsandboxed",
		"reason", why, "hint", sandboxHint)
}

// probeSandbox reports whether a usable `systemd-run --scope` is available: the
// binary must exist AND be able to create a scope (a container can ship the
// binary yet have no reachable manager, and an unprivileged agent is refused
// one). Non-Linux is always false. Best-effort and quick — bounded by ctx.
//
// reason is why not, in systemd-run's own words where it gave any: the line
// that tells a refused request from a missing manager from a missing binary.
func probeSandbox(ctx context.Context) (ok bool, reason string) {
	if runtime.GOOS != "linux" {
		return false, "not Linux"
	}
	pctx, cancel := context.WithTimeout(ctx, sandboxProbeTimeout)
	defer cancel()
	why := make(chan string, 1) // buffered: the probe may finish after it was abandoned
	ok = runAbandonable(pctx, func() bool {
		// exec.LookPath stats every $PATH entry; a dir on a hung mount (stale NFS
		// / autofs to an unreachable server) blocks that stat uninterruptibly
		// with no context to honor — THIS, not cmd.Run, was the real startup hang
		// — so LookPath must run INSIDE the abandonable section, not before it.
		if _, err := exec.LookPath("systemd-run"); err != nil {
			why <- "systemd-run is not on $PATH"
			return false
		}
		cmd := exec.CommandContext(pctx, "systemd-run", "--scope", "--quiet", "--collect", "--", "true")
		cmd.Env = os.Environ()
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		// Kill the whole group and force-close pipes on cancel — same backstop as
		// every other agent-spawned process (see exec_local_unix.go).
		configureProcGroup(cmd)
		cmd.WaitDelay = time.Second
		err := cmd.Run()
		if err != nil {
			why <- probeFailure(stderr.String(), err)
		}
		return err == nil
	})
	if ok {
		return true, ""
	}
	if pctx.Err() != nil {
		slog.Warn("sandbox probe did not finish within its deadline — treating systemd-run as unavailable; runner registers and runs unsandboxed",
			"timeout", sandboxProbeTimeout)
		return false, "systemd-run did not answer within " + sandboxProbeTimeout.String()
	}
	select {
	case reason = <-why:
	default:
	}
	return false, reason
}

// probeFailure is the first line systemd-run wrote when it could not create
// the scope, or the error from running it when it wrote nothing.
func probeFailure(stderr string, err error) string {
	for _, line := range strings.Split(stderr, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return err.Error()
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
		return "cronomicon: sandbox: systemd-run --scope" + suffix + "; fs/syscall hardening inherited from the agent unit"
	}
	if cfg.AllowCheckout {
		return "cronomicon: WARNING: running UNSANDBOXED — no usable systemd-run; this checkout run is NOT resource-capped by a per-run scope (only its timeout applies)"
	}
	return "cronomicon: sandbox: unsandboxed (no usable systemd-run; per-run resource caps unavailable)"
}

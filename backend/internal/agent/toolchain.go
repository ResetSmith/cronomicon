package agent

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Toolchains is the detected-toolchain DETAIL an agent reports at registration
// for the Runners page (display only, RX.7). It is separate from the flat
// capability TOKENS in Capabilities that claimRun actually gates against.
type Toolchains struct {
	AnsibleCore string            `json:"ansibleCore,omitempty"` // e.g. "2.16.3"
	Collections map[string]string `json:"collections,omitempty"` // fqcn → version
	Checkout    bool              `json:"checkout"`              // -allow-checkout enabled
	Vault       bool              `json:"vault"`                 // -vault-password-file configured
	Sandboxed   bool              `json:"sandboxed"`             // a usable systemd-run scope is available (§6.3)
	// BecomeFile reports whether this runner can honor a server-delivered become
	// password (RA-12): ansible-core >= 2.12, where --become-password-file landed.
	BecomeFile bool `json:"becomeFile"`
	// Undetermined names the local run-types this agent could not LOOK for: the
	// PATH lookup did not return (lookupHint). That is not "not installed", and
	// since an agent always has the shell types to claim it no longer stops the
	// agent from starting — so it is reported, or an agent that has lost ansible
	// this way looks exactly like one that never had it. Display-only; rides
	// additionalProperties:true like KeyNames.
	Undetermined []string `json:"undetermined,omitempty"`
	// KeyNames is the set of credential NAMES this runner can resolve to a local
	// key (key-map keys + key-dir basenames) — names only, NEVER paths or key
	// material (R6/Phase 4). Display-only in the Runner detail so an operator sees
	// credential coverage before a run fails. Rides additionalProperties:true (no
	// spec/protocol bump); a pre-Phase-4 agent omits it.
	KeyNames []string `json:"keyNames,omitempty"`
}

// declaredKeyNames returns the credential NAMES this runner can resolve to a
// local key — key-map keys plus key-dir file basenames (stripped of .pem/.key,
// ignoring .pub siblings) — for the Runner-detail display (R6). Names ONLY,
// never paths or key material, and it never reads a key's contents. Sorted +
// deduped for a stable declaration payload (nil when the runner holds no keys).
func declaredKeyNames(cfg Config) []string {
	set := map[string]bool{}
	for name := range cfg.KeyMap {
		if name != "" {
			set[name] = true
		}
	}
	if cfg.KeyDir != "" {
		if entries, err := os.ReadDir(cfg.KeyDir); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				fn := e.Name()
				if strings.HasSuffix(fn, ".pub") {
					continue // public-key sibling, not a credential name
				}
				if name := keyNameFromFile(fn); name != "" {
					set[name] = true
				}
			}
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ansibleCoreRe extracts the core version from `ansible --version`'s first line,
// e.g. "ansible [core 2.16.3]" or "ansible-core 2.16.3".
var ansibleCoreRe = regexp.MustCompile(`(?:core |ansible-core )(\d+\.\d+\.\d+[^\s\]]*)`)

// shellRunTypes are the run-types an agent runs over SSH ON THE TARGET, with
// the target's interpreter. An agent that names no capabilities claims all four
// without looking at its own host: its PATH says nothing about a machine it
// dials, and the name differs anyway (the remote invocation is `powershell` and
// `python3`, execspec.ResolveCommand). Probing the agent for them left a
// shell-less image with no capabilities at all, and kept an agent with no local
// pwsh off every Windows fleet. The local runner's list is the same four
// (settings.LocalRunnerCapabilities).
var shellRunTypes = []string{"bash", "perl", "powershell", "python"}

// localToolchainProbes maps each run-type that runs ON the agent host
// (localRunTypes) to the executables whose presence on PATH claims it (any one
// match suffices). These are the only types an agent's own host decides.
var localToolchainProbes = []struct {
	runType string
	bins    []string
}{
	{"ansible", []string{"ansible-playbook"}},
	{"terraform", []string{"terraform"}},
}

// detectRunTypes returns the run-types an agent claims when it names none
// (D1: 1B — unset capabilities means "decide at startup" instead of a config
// error): every shell type, and each local toolchain found on PATH. It is
// never empty, so an agent with nothing installed still registers.
// detectCapabilities dedupes + sorts downstream.
//
// undetermined is the local run-types it could not look for: every lookup for
// the type ran out its deadline and none found the toolchain. The type is not
// claimed, and the caller says so — absent and unanswered are different facts.
func detectRunTypes(ctx context.Context) (types, undetermined []string) {
	types = slices.Clone(shellRunTypes)
	for _, p := range localToolchainProbes {
		found, timedOut := false, false
		for _, bin := range p.bins {
			_, ok, late := lookPathBounded(ctx, bin)
			if ok {
				found = true
				break
			}
			timedOut = timedOut || late
		}
		switch {
		case found:
			types = append(types, p.runType)
		case timedOut:
			undetermined = append(undetermined, p.runType)
		}
	}
	return types, undetermined
}

// lookupHint names the two known causes of a PATH lookup that does not return.
// Shared by the agent's log line and the doctor's check.
const lookupHint = "on systemd 239 (RHEL 8) the unit needs SystemCallErrorNumber=EPERM (the upgrade command adds it; or systemctl edit <unit>, [Service], SystemCallErrorNumber=EPERM); otherwise a $PATH directory is on a hung mount"

// detectCapabilities augments the operator-configured run-type capabilities with
// the flat tokens the runner can actually satisfy (RX.7), and returns the
// display detail. Tokens added on top of the configured run-types:
//   - "ansible"          iff an ansible toolchain is present
//   - "checkout"         iff -allow-checkout (RX.6)
//   - "vault"            iff -vault-password-file is configured (RX.13)
//   - "collection:<fqcn>" per installed collection (skip-if-satisfied + gating)
//
// Empty configured capabilities means auto-detect (D1: 1B): detectRunTypes
// decides, and it always yields the shell types, so a runner can never come
// out of here with nothing to claim. Set -capabilities /
// CRONOMICON_RUNNER_CAPABILITIES explicitly to narrow ("this agent must not
// run python jobs").
//
// Detection is best-effort: a missing ansible/ansible-galaxy simply yields no
// toolchain tokens (the configured run-types still register). The token set is
// deduped + sorted for a stable registration payload.
func detectCapabilities(ctx context.Context, cfg Config) ([]string, Toolchains) {
	set := map[string]bool{}
	for _, c := range cfg.Capabilities {
		if c = strings.TrimSpace(c); c != "" {
			set[c] = true
		}
	}
	var tc Toolchains
	if len(set) == 0 {
		detected, undetermined := detectRunTypes(ctx)
		for _, rt := range detected {
			set[rt] = true
		}
		if len(undetermined) > 0 {
			tc.Undetermined = undetermined
			slog.Error("could not look for local toolchains (the PATH lookup did not return) — this agent does NOT claim them, and their runs will stay queued",
				"runTypes", undetermined, "hint", lookupHint)
		}
	}

	if core := detectAnsibleCore(ctx); core != "" {
		tc.AnsibleCore = core
		set["ansible"] = true
		// RA-12/RA-20a: --become-password-file landed in ansible-core 2.12. A job with
		// a become password claim-gates on this token, so an older core simply never
		// takes the run — it QUEUES for a capable runner instead of being assigned and
		// then refused at manifest time, which with a fleet of one is indistinguishable
		// but with a fleet of five burns an assignment a capable runner could have had.
		//
		// The 2.16 pin some runners carry (RHEL8 targets need python 3.6, dropped in
		// core 2.17) is comfortably above the floor, so pinning costs nothing here.
		if coreAtLeast(core, 2, 12) {
			set["become-file"] = true
			tc.BecomeFile = true
		}
	}
	if cfg.AllowCheckout {
		set["checkout"] = true
		tc.Checkout = true
	}
	// ET-D. Advertised only when BOTH the opt-in and a non-empty allowlist are
	// present: an agent that would accept watch specs and then refuse every path
	// is worse than one that never claimed the capability, because the server
	// would believe the job was covered.
	if cfg.AllowWatch && len(cfg.WatchPaths) > 0 {
		set["watch"] = true
	}
	if cfg.VaultPasswordFile != "" {
		set["vault"] = true
		tc.Vault = true
	}
	if cfg.SandboxAvailable {
		// A job may claim-gate to a sandboxed runner via requires:[sandboxed]
		// (§6.3) — this reuses the Phase 4 capability/claim machinery for free.
		set["sandboxed"] = true
		tc.Sandboxed = true
	}
	if colls := galaxyBaseCollections(ctx); len(colls) > 0 {
		tc.Collections = colls
		for fqcn := range colls {
			set["collection:"+fqcn] = true
		}
	}
	// Declared key NAMES (R6/Phase 4): display-only credential coverage. Names
	// only — never paths or key bytes. Not folded into `set` (these aren't claim
	// tokens, just detail).
	tc.KeyNames = declaredKeyNames(cfg)

	caps := make([]string, 0, len(set))
	for c := range set {
		caps = append(caps, c)
	}
	sort.Strings(caps)
	return caps, tc
}

// toolchainProbeTimeout hard-bounds each best-effort startup probe (ansible
// --version, ansible-galaxy collection list). These run BEFORE registration, so
// an unbounded probe blocks the runner from ever coming online: on an Ansible
// control node `ansible-galaxy collection list` routinely takes many seconds
// (and can hang for minutes on an NFS-mounted collection path), which once left
// a runner emitting no logs until systemd SIGKILLed it. A probe that overruns
// this is treated as "not detected" and registration proceeds.
const toolchainProbeTimeout = 15 * time.Second

// runProbe executes a best-effort detection command under toolchainProbeTimeout
// and returns its stdout (nil on any error/timeout — callers already treat an
// empty result as "not detected"). It runs in its own process group and force-
// closes the pipes shortly after cancel (configureProcGroup + WaitDelay, the
// same backstop the run path uses) so a hung child whose grandchildren hold the
// stdout pipe open — e.g. ansible-galaxy's python subprocesses — cannot keep
// the agent blocked past the deadline. A timeout is logged, never silent.
func runProbe(ctx context.Context, name string, args ...string) []byte {
	return runProbeWithTimeout(ctx, toolchainProbeTimeout, name, args...)
}

func runProbeWithTimeout(ctx context.Context, d time.Duration, name string, args ...string) []byte {
	pctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		// exec.CommandContext resolves `name` through exec.LookPath synchronously,
		// so the construct AND the run must live inside the abandonable goroutine:
		// a $PATH dir on a hung mount blocks LookPath's stat with no deadline, and
		// that — not the command itself — is enough to wedge startup.
		cmd := exec.CommandContext(pctx, name, args...)
		cmd.Env = os.Environ()
		configureProcGroup(cmd)
		cmd.WaitDelay = 2 * time.Second
		out, err := cmd.Output()
		ch <- result{out, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil
		}
		return r.out
	case <-pctx.Done():
		slog.Warn("capability probe timed out — skipping (registration continues)",
			"cmd", strings.TrimSpace(name+" "+strings.Join(args, " ")), "timeout", d)
		return nil
	}
}

// lookPathTimeout bounds a single PATH lookup. exec.LookPath stats each $PATH
// entry; if one lives on a hung mount (stale NFS / autofs to an unreachable
// server) that stat blocks uninterruptibly with no context to honor, so a bare
// LookPath can wedge startup — this was the real registration hang.
var lookPathTimeout = 3 * time.Second

// lookPath is exec.LookPath, replaceable so a test can make a lookup that
// never returns (the real one cannot be made to, portably).
var lookPath = exec.LookPath

// lookupCanary names a file whose lookup shows whether lookups return on this
// host at all: this binary. It has to be a file that is THERE. exec.LookPath
// stats each candidate first and asks whether it may be executed (faccessat2,
// the call a systemd 239 filter with no error number kills the thread for)
// only once the stat has succeeded, so a lookup for a tool that is not
// installed answers "not found" at once under the very unit that hangs every
// lookup that finds something. Looking for ansible-playbook passed on a host
// without Ansible whose systemd-run lookup never returned (RHEL 8.10,
// 2026-10-08). A name with a slash skips the $PATH walk and goes straight to
// that question; the directories are the doctor's path-dirs check.
//
// The fallback is the first local toolchain, as before: no worse, and only
// reached when the process cannot name or see its own file.
func lookupCanary() string {
	if exe, err := executable(); err == nil {
		if _, err := os.Stat(exe); err == nil {
			return exe
		}
	}
	return localToolchainProbes[0].bins[0]
}

// executable is os.Executable, replaceable so a test can take it away.
var executable = os.Executable

// lookPathBounded is exec.LookPath with a hard deadline: abandoned on timeout it
// reports "not found" (best-effort — the leaked goroutine unblocks if the mount
// ever recovers), so a hung $PATH entry degrades a probe rather than hanging it.
// timedOut tells the two "not found"s apart: the lookup answered no, or it
// never answered.
func lookPathBounded(ctx context.Context, bin string) (path string, found, timedOut bool) {
	type result struct {
		path string
		err  error
	}
	ch := make(chan result, 1)
	look := lookPath // read here, not in the goroutine that may outlive the call
	go func() { p, e := look(bin); ch <- result{p, e} }()
	tctx, cancel := context.WithTimeout(ctx, lookPathTimeout)
	defer cancel()
	select {
	case r := <-ch:
		return r.path, r.err == nil, false
	case <-tctx.Done():
		slog.Warn("PATH lookup timed out — treating tool as absent (a $PATH dir may be on a hung mount)", "bin", bin)
		return "", false, true
	}
}

// detectAnsibleCore returns the ansible-core version, or "" if ansible is absent
// (or its version probe overran toolchainProbeTimeout).
func detectAnsibleCore(ctx context.Context) string {
	out := runProbe(ctx, "ansible", "--version")
	if len(out) == 0 {
		return ""
	}
	first, _, _ := strings.Cut(string(out), "\n")
	if m := ansibleCoreRe.FindStringSubmatch(first); m != nil {
		return m[1]
	}
	return ""
}

// coreAtLeast reports whether an ansible-core version string is >= major.minor.
// Parses only the leading two components: a core version is "2.16.3" or "2.16.3rc1"
// and the patch/suffix never decides a feature gate. An unparseable version reports
// FALSE — an unknown toolchain must not advertise a capability it may not have, and
// the cost of a false negative is a queued run rather than a broken escalation.
func coreAtLeast(version string, major, minor int) bool {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return false
	}
	maj, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return false
	}
	// Trim any non-digit tail ("16rc1" → "16").
	minStr := strings.TrimSpace(parts[1])
	end := 0
	for end < len(minStr) && minStr[end] >= '0' && minStr[end] <= '9' {
		end++
	}
	min, err := strconv.Atoi(minStr[:end])
	if err != nil {
		return false
	}
	if maj != major {
		return maj > major
	}
	return min >= minor
}

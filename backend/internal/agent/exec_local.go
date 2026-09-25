package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ResetSmith/cronomicon/internal/envref"
	"github.com/ResetSmith/cronomicon/internal/remotecmd"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// runLocalToolchain executes a local run-type (ansible/terraform) ON the agent
// host — the reason runners exist (the distroless app image lacks these
// toolchains). It shells out to ansible-playbook / terraform with the manifest
// body + env, streaming combined stdout+stderr line-by-line. The process is
// killed if ctx is cancelled (kill control op or timeout).
//
// Every run gets a fresh 0700 per-run workdir under StateDir (§6.1): it is the
// process cwd, holds the materialized inventory temp file, and is removed on
// EVERY exit path — success, failure, and kill (the process dies via ctx; the
// defer still runs). Previously only the inventory temp file was cleaned.
func runLocalToolchain(ctx context.Context, m *runnerproto.ManifestResponse, cfg Config, emit func(line string)) int {
	workdir, err := os.MkdirTemp(cfg.StateDir, "cronomicon-run-*")
	if err != nil {
		emit("cronomicon: create run workdir: " + err.Error())
		return -1
	}
	defer func() { _ = os.RemoveAll(workdir) }()

	// Checkout mode (§7): materialize the pinned repo tree into workdir BEFORE
	// building the command — the entry playbook is run from the tree (cwd =
	// workdir). Refusal gates + fetch failures abort loudly; there is no
	// body-only fallback for a run the server marked as checkout.
	var galaxyEnv map[string]string
	if m.Checkout != nil {
		if cerr := materializeCheckout(ctx, m, cfg, workdir, emit); cerr != nil {
			emit("cronomicon: " + cerr.Error())
			return -1
		}
		emitAnsibleCoreVersion(ctx, emit) // provenance (RX.12)
		// Per-run galaxy install (RX.4): a project's requirements.yml is a
		// CANDIDATE path (ReqPath = <project_root>/requirements.yml); install only
		// when it actually exists in the pinned tree. Failures abort the run.
		if m.Checkout.ReqPath != "" {
			reqAbs := filepath.Join(workdir, filepath.FromSlash(m.Checkout.ReqPath))
			if fi, serr := os.Stat(reqAbs); serr == nil && !fi.IsDir() {
				ge, gerr := galaxyInstall(ctx, cfg, workdir, m.Checkout.ReqPath, emit)
				if gerr != nil {
					emit("cronomicon: " + gerr.Error())
					return -1
				}
				galaxyEnv = ge
			}
		}
	}

	argv, stdinBody, cmdProv, err := localCommand(m, cfg, workdir)
	if err != nil {
		emit("cronomicon: " + err.Error())
		return -1
	}
	// Argv-shaping provenance (Phase 3): the auto --private-key decision (wired,
	// or skipped-and-why for a multi-key run).
	for _, line := range cmdProv {
		emit(line)
	}

	// Scoped child env (RX.9): when the manifest carries an EnvPassthrough
	// allowlist the child sees ONLY the base safe-list + manifest Env + the
	// allowlisted names — not the agent's full environment (secrets.env). An
	// unset allowlisted name fails the run loudly here rather than surfacing as
	// a silently-empty lookup inside the playbook. galaxyEnv (collection/role
	// paths into the workdir) is layered on so the playbook resolves per-run deps.
	childEnv, provenance, err := buildChildEnv(m, cfg, os.Environ(), galaxyEnv)
	if err != nil {
		emit("cronomicon: " + err.Error())
		return -1
	}
	// Auth/trust provenance (Phase 1): show exactly which local key each bridged
	// name resolved to and which known_hosts store verifies the hosts, so a run's
	// log — not a post-mortem — reveals what authenticated it.
	for _, line := range provenance {
		emit(line)
	}

	// Tier 2 sandbox (§6.3): wrap the process in a per-run systemd-run scope when
	// available. The wrapped command stays an ordinary descendant of the agent,
	// so the cwd/env/stdio/kill machinery below is unchanged. Emit the sandbox
	// posture as a provenance line (a loud WARNING when a checkout run is
	// unsandboxed — an operator must never assume otherwise).
	runArgv, sandboxed := sandboxWrap(cfg, argv)
	emit(sandboxProvenance(cfg, sandboxed))

	cmd := exec.CommandContext(ctx, runArgv[0], runArgv[1:]...)
	cmd.Dir = workdir
	cmd.Env = childEnv
	// Kill the whole process GROUP on ctx-cancel/timeout (Unix; see
	// exec_local_unix.go) — a killed toolchain must not leave grandchildren
	// holding the output pipes (and the run) open. WaitDelay is the backstop:
	// if anything still holds the pipes past it, they are force-closed so the
	// scanners (and the run) end.
	configureProcGroup(cmd)
	cmd.WaitDelay = 10 * time.Second
	if stdinBody != "" {
		cmd.Stdin = strings.NewReader(stdinBody)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		emit("cronomicon: stdout pipe: " + err.Error())
		return -1
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		emit("cronomicon: stderr pipe: " + err.Error())
		return -1
	}
	if err := cmd.Start(); err != nil {
		emit("cronomicon: start " + argv[0] + ": " + err.Error())
		return -1
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); scanLines(stdout, emit) }()
	go func() { defer wg.Done(); scanLines(stderr, emit) }()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		emit("cronomicon: " + err.Error())
		return -1
	}
	return 0
}

// localCommand maps a local run-type manifest to an argv and optional stdin.
// Any temp artifact (the materialized inventory) is written INSIDE workdir, the
// caller-owned per-run directory removed on every exit path — localCommand
// itself owns no cleanup.
//
// ansible: the playbook body is fed on stdin via `ansible-playbook /dev/stdin`.
// An cronomicon-mode run ships a managed inventory (m.Inventory); it is materialized
// to a temp file and passed with `-i` so the playbook targets the resolved hosts.
// A local-mode run carries no inventory and runs as before (no -i; the runner's
// own ansible config supplies any inventory). `--limit` is appended when set.
// terraform: the body is a sequence of terraform subcommand args (one per line),
// defaulting to `terraform apply -auto-approve` when blank.
func localCommand(m *runnerproto.ManifestResponse, cfg Config, workdir string) (argv []string, stdin string, provenance []string, err error) {
	switch m.RunType {
	case "ansible":
		argv = []string{"ansible-playbook"}
		// RP-9 — the run's "connect as" identity override, delivered as connection
		// EXTRA-VARS. The channel is the decision (RP-Q1): `-e` is ansible's
		// highest-precedence tier, so the override beats inventory-authored
		// ansible_user / ansible_ssh_private_key_file on every host in the run —
		// matching the SSH executor, where an override replaces the per-host wiring
		// outright rather than racing it. The obvious-looking alternative (`-u` /
		// `--private-key`) sits in the LOWEST tier and would lose to the inventory
		// on exactly the hosts an admin bothered to wire, applying silently and
		// only sometimes.
		//
		// Values are safe to pass verbatim: the username is charset-validated at
		// both authoring boundaries (execspec.ValidSSHUser), the key path is
		// agent-generated, and exec.Cmd never goes through a shell.
		//
		// ORDER IS LOAD-BEARING: the operator's own extra-vars are emitted FIRST,
		// the identity pair second. With repeated `-e` ansible takes the LAST
		// occurrence, so this ordering means an operator extra-var can never
		// override the run's connect-as identity — which would let a run connect as
		// someone other than what its own audit record claims. The trigger boundary
		// also refuses the two identity names outright; this is the second lock.
		optArgs, optProv := ansibleOptionArgs(m)
		argv = append(argv, optArgs...)
		provenance = append(provenance, optProv...)
		idArgs, idProv, ierr := identityArgs(m, cfg)
		if ierr != nil {
			return nil, "", nil, ierr
		}
		argv = append(argv, idArgs...)
		provenance = append(provenance, idProv...)
		// Auto --private-key (R2/Phase 3): when every target shares ONE key name
		// that resolves to a local key file, wire it up front so a new scope needs
		// neither an inventory ansible_ssh_private_key_file nor the env lookup.
		// Additive — inventory host/group vars still override per host — and placed
		// before -i so the existing flag order is unchanged. Skipped for multi-key
		// runs (inventory owns that) and when the operator disabled the bridge.
		//
		// RP-9: skipped entirely when the run carries an override key. The two
		// would otherwise wire the same concept through two precedence tiers, and
		// the auto-wire's tier is the one the inventory beats — so leaving it on
		// would mean the effective key differs per host depending on what the
		// inventory happens to say. The override is the operator's explicit choice
		// for the whole run; it wins alone.
		if m.SSHKeyRef == "" {
			pkArgs, pkProv := privateKeyArgs(m, cfg)
			argv = append(argv, pkArgs...) // no-op when empty
			if pkProv != "" {
				provenance = append(provenance, pkProv)
			}
		} else if names := distinctAuthKeyNames(m.Targets); len(names) > 0 {
			provenance = append(provenance, "cronomicon: auth: --private-key auto-wiring skipped — this run carries an SSH key override: "+strings.Join(names, ", "))
		}
		// Defense-in-depth (no-leak, D2): the server never ships an inventory to a
		// local-mode runner. If one arrives anyway (server bug/regression), refuse
		// to use it — the agent must not honor a host list it should never receive.
		if m.Inventory != nil && m.Inventory.Raw != "" && cfg.Inventory != "local" {
			invPath, ferr := writeInventoryFile(workdir, m.Inventory)
			if ferr != nil {
				return nil, "", nil, ferr
			}
			argv = append(argv, "-i", invPath)
		}
		if m.Limit != "" {
			argv = append(argv, "--limit", m.Limit)
		}
		// RA-12 — --become-password-file. The server ships a REFERENCE, not a path
		// (it has no idea where the agent writes files); materializeSecretFiles put
		// the path in m.Secrets under that reference. Refuse loudly when it is absent
		// rather than dropping the flag: a run that asked to escalate and silently did
		// NOT does not fail cleanly — `become: true` with no password hangs on a sudo
		// prompt or escalates through whatever passwordless rule happens to exist. That
		// is the same class of bug the v8 option gate exists to prevent.
		if m.BecomePasswordRef != "" {
			path := m.Secrets[m.BecomePasswordRef]
			if path == "" {
				return nil, "", nil, fmt.Errorf("run declares a become password (%s) but the runner received no such secret file; "+
					"the runner may not be flagged for secret injection", m.BecomePasswordRef)
			}
			argv = append(argv, "--become-password-file", path)
		}
		if m.Checkout != nil {
			// Vault opt-in (RX.13): a run that requested vault must find a
			// runner-local password file. If UsesVault arrives without one, refuse
			// loudly — same defense-in-depth shape as the checkout refusal gates.
			// The password never travels (D1); the agent supplies it from ITS config.
			if m.Checkout.UsesVault {
				if cfg.VaultPasswordFile == "" {
					return nil, "", nil, fmt.Errorf("run requires Ansible Vault but this runner has no -vault-password-file configured")
				}
				argv = append(argv, "--vault-password-file", cfg.VaultPasswordFile)
			}
			// Checkout mode (§7): the tree was materialized into workdir (the cwd);
			// run the entry playbook FROM it. Body is ignored — no /dev/stdin.
			argv = append(argv, m.Checkout.Entry)
			return argv, "", provenance, nil
		}
		argv = append(argv, "/dev/stdin")
		return argv, m.Body, provenance, nil
	case "terraform":
		args := []string{"terraform"}
		body := strings.TrimSpace(m.Body)
		if body == "" {
			args = append(args, "apply", "-auto-approve")
		} else {
			for line := range strings.SplitSeq(body, "\n") {
				if f := strings.Fields(line); len(f) > 0 {
					args = append(args, f...)
				}
			}
		}
		return args, "", nil, nil
	default:
		return nil, "", nil, fmt.Errorf("run-type %q is not a local toolchain run-type", m.RunType)
	}
}

// ansibleOptionArgs renders the per-run advanced ansible options (Phase 3,
// RP-18) into argv, plus provenance lines for the two that change what the run
// MEANS rather than how it is reported.
//
// Every value here was validated at the trigger boundary (tag charset, verbosity
// range, become-user charset, extra-var names), and exec.Cmd never goes through
// a shell, so nothing needs quoting or re-validation.
//
// The check-mode provenance line matters more than it looks: a dry run produces
// a normal-looking success in History, and "why did nothing change?" is then the
// first question asked. The run's own log answers it.
func ansibleOptionArgs(m *runnerproto.ManifestResponse) (args []string, provenance []string) {
	o := m.AnsibleOptions
	if o == nil {
		return nil, nil
	}
	if o.Check {
		args = append(args, "--check")
		provenance = append(provenance, "cronomicon: ansible: --check — DRY RUN, no changes are applied")
	}
	if o.Diff {
		args = append(args, "--diff")
	}
	if len(o.Tags) > 0 {
		args = append(args, "--tags", strings.Join(o.Tags, ","))
		provenance = append(provenance, "cronomicon: ansible: --tags "+strings.Join(o.Tags, ",")+" — only tasks carrying these tags run")
	}
	if len(o.SkipTags) > 0 {
		args = append(args, "--skip-tags", strings.Join(o.SkipTags, ","))
		provenance = append(provenance, "cronomicon: ansible: --skip-tags "+strings.Join(o.SkipTags, ","))
	}
	if o.Verbosity > 0 {
		v := min(o.Verbosity,
			// belt to the boundary's range check; -vvvvv is not a thing
			4)
		args = append(args, "-"+strings.Repeat("v", v))
	}
	if o.Become {
		args = append(args, "--become")
	}
	if o.BecomeUser != "" {
		args = append(args, "--become-user", o.BecomeUser)
	}
	// Sorted so one run's argv is reproducible from its envelope — map iteration
	// order would otherwise make two identical runs render differently.
	if len(o.ExtraVars) > 0 {
		names := make([]string, 0, len(o.ExtraVars))
		for k := range o.ExtraVars {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			args = append(args, "-e", k+"="+o.ExtraVars[k])
		}
	}
	return args, provenance
}

// identityArgs builds the connection extra-vars for a run carrying a "connect
// as" identity override (RP-9). It returns the argv fragment (empty when the run
// carries no override) plus provenance lines naming what was applied, so the run
// log states the identity it actually connected with.
//
// The key half fails CLOSED: the server only sets SSHKeyRef after delivering
// that key's material (the implicit KindKey binding, CA-3b), so an unresolvable
// reference here means delivery did not happen — from -no-auth-bridge, a runner
// not flagged for secret injection, or a key that never landed. Running anyway
// would fall back to the inventory's key while the run record claims the
// operator's, which is precisely the silent-wrong-identity outcome the whole
// override path exists to prevent.
func identityArgs(m *runnerproto.ManifestResponse, cfg Config) (args []string, provenance []string, err error) {
	if m.SSHUser == "" && m.SSHKeyRef == "" {
		return nil, nil, nil
	}
	if m.SSHUser != "" {
		args = append(args, "-e", "ansible_user="+m.SSHUser)
		provenance = append(provenance, "cronomicon: auth: connect as "+m.SSHUser+" (-e ansible_user, overrides inventory)")
	}
	if m.SSHKeyRef != "" {
		path, src, ok := resolveKeyPath(cfg.KeyMap, cfg.KeyDir, m.SSHKeyRef)
		if !ok {
			return nil, nil, fmt.Errorf("run carries an SSH key override (%s) but no key material was delivered to this runner and no local key file matches; the run would have connected with the inventory's key instead", m.SSHKeyRef)
		}
		args = append(args, "-e", "ansible_ssh_private_key_file="+path)
		provenance = append(provenance, fmt.Sprintf("cronomicon: auth: -e ansible_ssh_private_key_file %s → %s (%s, overrides inventory)", m.SSHKeyRef, path, src))
	}
	return args, provenance, nil
}

// privateKeyArgs decides the auto --private-key wiring for an ansible run (R2).
// It returns the argv fragment (empty when nothing is wired) plus one provenance
// line (empty when there's nothing to say). It fires only when every resolved
// target shares exactly ONE AuthKeyEnvVar that resolves to a local key file; a
// multi-key run wires nothing and names the keys it left to the inventory / env
// bridge. Disabled entirely by -no-auth-bridge (the operator owns ansible auth).
func privateKeyArgs(m *runnerproto.ManifestResponse, cfg Config) (args []string, provenance string) {
	if cfg.NoAuthBridge {
		return nil, ""
	}
	// Defense-in-depth (D2, mirrors the -i gate below): a local-mode runner
	// resolves its own hosts and must never receive a server Targets list — if one
	// arrives anyway (server bug/regression), don't honor it to auto-wire a key.
	if cfg.Inventory == "local" {
		return nil, ""
	}
	names := distinctAuthKeyNames(m.Targets)
	switch len(names) {
	case 0:
		return nil, ""
	case 1:
		if path, src, ok := resolveKeyPath(cfg.KeyMap, cfg.KeyDir, names[0]); ok {
			return []string{"--private-key", path}, fmt.Sprintf("cronomicon: auth: --private-key %s → %s (%s)", names[0], path, src)
		}
		// Single key but no local FILE (env-PEM, or unresolved): the env bridge and
		// inventory lookup still apply — nothing to auto-wire, nothing to warn.
		return nil, ""
	default:
		return nil, "cronomicon: auth: multiple key names — --private-key not auto-wired (inventory ansible_ssh_private_key_file / env bridge applies): " + strings.Join(names, ", ")
	}
}

// distinctAuthKeyNames returns the sorted, deduped, non-empty AuthKeyEnvVar names
// across the resolved targets.
func distinctAuthKeyNames(targets []runnerproto.ManifestTarget) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range targets {
		if t.AuthKeyEnvVar == "" || seen[t.AuthKeyEnvVar] {
			continue
		}
		seen[t.AuthKeyEnvVar] = true
		out = append(out, t.AuthKeyEnvVar)
	}
	sort.Strings(out)
	return out
}

// writeInventoryFile materializes a manifest inventory to a 0600 temp file under
// dir (os.TempDir() when dir==""), with an extension matching the format so
// ansible's `-i` source-type auto-detection works. os.CreateTemp creates the
// file 0600. The file lives inside the caller's per-run workdir and is removed
// with it.
func writeInventoryFile(dir string, inv *runnerproto.ManifestInventory) (string, error) {
	ext := "ini"
	switch strings.ToLower(inv.Format) {
	case "yaml", "yml":
		ext = "yaml"
	}
	f, err := os.CreateTemp(dir, "cronomicon-inv-*."+ext)
	if err != nil {
		return "", fmt.Errorf("create inventory temp file: %w", err)
	}
	if _, err := f.WriteString(inv.Raw); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write inventory temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("close inventory temp file: %w", err)
	}
	return f.Name(), nil
}

// envBaseSafeList is the explicit base of agent env-var NAMES a scoped child
// env inherits (RX.9/§6.1). Deliberately a safe-list, not an ad-hoc minimum:
// dropping system/locale vars breaks ansible in surprising ways (output
// decoding, module execution, temp paths). LC_* is matched by prefix in
// buildChildEnv; SSH_AUTH_SOCK is default-in but excludable (it grants signing
// access to every key the runner's ssh-agent holds — cfg.ExcludeSSHAuthSock).
var envBaseSafeList = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "TMPDIR", "TZ", "LANG",
}

// buildChildEnv computes the environment for a local-toolchain child process
// (RX.9). environ is the agent's own environment (os.Environ(); injectable for
// tests).
//
// Scoped env is now ALWAYS ON (Phase 5 — the RX.9 rollout is complete): the
// child sees ONLY the base safe-list (+ cfg.EnvBaseExtra) resolved from the
// agent env, then the manifest Env snapshot, then the galaxy dep-path extras,
// then the passthrough NAMES resolved from the agent env (last wins on
// collision, so a runner-local secret beats a same-named server config value).
// A nil EnvPassthrough (an old server that never sends the field) is treated as
// an EMPTY allowlist — base + manifest Env only, NEVER the agent's full
// environment. A passthrough name set in neither the agent env nor the manifest
// Env, but resolvable as a local key FILE (key-map / key-dir), is bridged to its
// path (R1) so one provisioned key serves bash and ansible; only a name that
// resolves NOWHERE is the loud per-run error — never a silently-empty
// lookup('env'). For ansible runs it also injects ANSIBLE_SSH_COMMON_ARGS to
// verify hosts against the agent's known_hosts (R3), unless the operator supplied
// that var or -no-auth-bridge is set. Returns the child env plus provenance lines
// (one per bridged key + one for the trust store) for the caller to emit.
func buildChildEnv(m *runnerproto.ManifestResponse, cfg Config, environ []string, extra map[string]string) (env []string, provenance []string, err error) {
	agentEnv := make(map[string]string, len(environ))
	names := make([]string, 0, len(environ))
	for _, e := range environ {
		if k, v, ok := strings.Cut(e, "="); ok {
			if _, dup := agentEnv[k]; !dup {
				names = append(names, k)
			}
			agentEnv[k] = v
		}
	}
	sort.Strings(names)

	base := make(map[string]bool, len(envBaseSafeList)+len(cfg.EnvBaseExtra)+1)
	for _, n := range envBaseSafeList {
		base[n] = true
	}
	var refusedBase []string
	for _, n := range cfg.EnvBaseExtra {
		// DR-1: -env-base-extra widens the base allowlist, which reaches EVERY child
		// — not just runs that declare a passthrough — so an agent-config name here
		// would be a broader leak than the passthrough path this change closes.
		// Refused rather than honoured, and reported through provenance so the
		// refusal is visible per run instead of silently dropped.
		if envref.IsAgentConfig(n) {
			refusedBase = append(refusedBase, n)
			continue
		}
		base[n] = true
	}
	if !cfg.ExcludeSSHAuthSock {
		base["SSH_AUTH_SOCK"] = true
	}
	for _, n := range refusedBase {
		provenance = append(provenance, fmt.Sprintf(
			"cronomicon: env: refused %q from -env-base-extra (%s is agent config, never readable by a job)",
			n, envref.PrefixRunnerConfig))
	}

	var out []string
	for _, n := range names {
		if base[n] || strings.HasPrefix(n, "LC_") {
			out = append(out, n+"="+agentEnv[n])
		}
	}
	out = append(out, envSlice(m.Env)...)
	// Dispatch-time resolved reference values (vault-integration.md P1.4): the
	// CRONOMICON_SECRET_*/CRONOMICON_VAR_* the server resolved from the run's declared
	// bindings. Appended after the plaintext Env snapshot (last wins) so an
	// injected reference value beats a same-named snapshot entry. Never persisted:
	// they live only in this child env, which dies with the process.
	out = append(out, envSlice(m.Secrets)...)
	// Galaxy per-run dep paths (ANSIBLE_COLLECTIONS_PATH / ANSIBLE_ROLES_PATH):
	// runner-computed config, not a secret — always forwarded so the playbook
	// resolves the deps installed into the workdir.
	out = append(out, envSlice(extra)...)

	var missing, denied []string
	for _, n := range m.EnvPassthrough {
		if envref.IsAgentConfig(n) {
			// DR-1: refused BEFORE any lookup, so no resolution path — agent env,
			// manifest Env, the derived-reference bridge or the key-path bridge —
			// can reach the runner's own configuration namespace. Collected and
			// raised loudly below rather than skipped: a passthrough that silently
			// resolved to empty is the exact failure mode this function is written
			// to avoid.
			denied = append(denied, n)
			continue
		}
		if _, inSecrets := m.Secrets[n]; inSecrets {
			// Supplied by the dispatch-time resolved Secrets block (appended above).
			// Checked FIRST so the server-resolved (possibly rotated) value wins over
			// ANY runner-local env var of the same name — including the exact
			// prefixed form CRONOMICON_SECRET_X a migrated secrets.env might hold, not
			// just the bare-name fallback (P1.4).
			continue
		}
		if v, ok := agentEnv[n]; ok {
			// An explicit env value (including a field-provisioned NAME=path or a
			// PEM) always wins over the bridge.
			out = append(out, n+"="+v)
			continue
		}
		if _, inManifest := m.Env[n]; inManifest {
			// Supplied by the manifest Env snapshot (already appended above).
			continue
		}
		// Derived-reference bridge (W3, N-D5 decoupling): a value reference
		// (CRONOMICON_SECRET_X / CRONOMICON_VAR_X) absent under its prefixed name falls
		// back to the BARE name in the runner env, so an updated inventory keeps
		// resolving against an un-migrated secrets.env that still uses bare names.
		// The child still receives the value under the referenced (prefixed) name.
		if bare, ok := envref.StripValueReference(n); ok {
			// DR-1: the fallback reads a DIFFERENT key than the one denied at the top
			// of the loop, so it needs its own guard — a passthrough of
			// CRONOMICON_SECRET_CRONOMICON_RUNNER_REGISTRATION_TOKEN strips to the agent's
			// own config name and would otherwise resolve it out of the agent env and
			// hand it to the child under the prefixed name.
			if envref.IsAgentConfig(bare) {
				denied = append(denied, n)
				continue
			}
			if v, present := agentEnv[bare]; present {
				out = append(out, n+"="+v)
				provenance = append(provenance, fmt.Sprintf("cronomicon: env: %s ← bare %q (prefixed→bare fallback)", n, bare))
				continue
			}
		}
		// Bridge (R1/Phase 1): a passthrough NAME with no env value may still be a
		// key the agent holds as a FILE (key-map / key-dir). Resolve it to a path so
		// ONE provisioned key file serves bash AND ansible, instead of forcing the
		// same name to be provisioned twice (file + env-var-holding-a-path). Per D-A
		// env-PEMs are not bridged — a PEM isn't a path ansible can use.
		if !cfg.NoAuthBridge {
			if path, src, ok := resolveKeyPath(cfg.KeyMap, cfg.KeyDir, n); ok {
				out = append(out, n+"="+path)
				provenance = append(provenance, fmt.Sprintf("cronomicon: auth: key %q → %s (%s)", n, path, src))
				continue
			}
		}
		missing = append(missing, n)
	}
	if len(denied) > 0 {
		// Named separately from `missing` on purpose: the missing-var advice is
		// "set each as an env var", which is precisely the wrong remedy here and
		// would walk an operator into putting the token back where it can leak.
		return nil, nil, fmt.Errorf("env passthrough var(s) refused: %s — %s is the runner agent's own configuration namespace and can never be read by a job. Rename the variable, or bind the value as a reference (%sNAME / %sNAME)",
			strings.Join(denied, ", "), envref.PrefixRunnerConfig, envref.PrefixSecret, envref.PrefixVar)
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("env passthrough var(s) not resolvable on this runner: %s — set each as an env var, add a -key-map entry (NAME=path), or drop a key file named <NAME>{,.pem,.key} in -key-dir; or remove them from the job's env_passthrough / inventory env lookups",
			strings.Join(missing, ", "))
	}

	// Trust-store unification (R3/Phase 1): point ansible's OpenSSH child at the
	// agent's known_hosts — the SAME store Scan & approve seeds — so host-key
	// verification is one store for bash and ansible (no ~/.ssh/known_hosts
	// split-brain). Only for ansible runs, only when the bridge is enabled, and
	// only when the operator hasn't already supplied ANSIBLE_SSH_COMMON_ARGS via
	// any channel (manifest Env, -env-base-extra, or passthrough) — we never
	// silently clobber an operator value. Per D-C, an explicit
	// -ansible-ssh-common-args override is injected INSTEAD (ProxyJump/cipher
	// estates that must own the whole arg string).
	if m.RunType == "ansible" && !cfg.NoAuthBridge && !envSliceHasKey(out, "ANSIBLE_SSH_COMMON_ARGS") {
		var val, prov string
		switch {
		case cfg.AnsibleSSHCommonArgs != "":
			val = cfg.AnsibleSSHCommonArgs
			prov = "cronomicon: trust: ANSIBLE_SSH_COMMON_ARGS ← operator override (-ansible-ssh-common-args)"
		case cfg.KnownHostsFile != "":
			val = fmt.Sprintf("-o UserKnownHostsFile=%s -o StrictHostKeyChecking=yes", cfg.KnownHostsFile)
			prov = "cronomicon: trust: known_hosts → " + cfg.KnownHostsFile
		}
		if val != "" {
			out = append(out, "ANSIBLE_SSH_COMMON_ARGS="+val)
			provenance = append(provenance, prov)
		}
	}
	return out, provenance, nil
}

// envSliceHasKey reports whether a "K=V" env slice already carries the given
// key (any value) — used to avoid clobbering an operator-supplied value.
func envSliceHasKey(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// envSlice renders an env map as "K=V" entries for exec.Cmd.Env (POSIX-ident
// keys only, matching the remote-shell injection rules).
func envSlice(env map[string]string) []string {
	keys := remotecmd.SortedIdentKeys(env)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// scanLines reads newline-delimited output and calls fn per line. Shared by the
// SSH and local-toolchain execution paths. 1 MiB max line (matches server).
func scanLines(r io.Reader, fn func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		fn(sc.Text())
	}
}

// ── local-inventory scope resolution (D8 "local" mode, T-b) ──────────────────

// localInventory is the agent's own inventory used in "local" mode: a map of
// scope name → host targets the agent (and only the agent) can reach. This is
// the simplest documented convention — a JSON file of the shape:
//
//	{ "prod": [ {"name":"h1","address":"10.0.0.1","port":22,"user":"deploy",
//	             "authKeyEnvVar":"PROD_KEY","via":"bastion.local:22"} ] }
//
// In "local" mode the server ships only the scope name (manifest.Targets is
// empty); the agent expands it here against hosts Cronomicon can't see.
type localInventory map[string][]runnerproto.ManifestTarget

func loadLocalInventory(path string) (localInventory, error) {
	if path == "" {
		return nil, fmt.Errorf("local inventory mode requires -local-inventory")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read local inventory %q: %w", path, err)
	}
	var inv localInventory
	if err := json.Unmarshal(data, &inv); err != nil {
		return nil, fmt.Errorf("parse local inventory %q: %w", path, err)
	}
	return inv, nil
}

// resolveLocalScope expands a scope name to host targets from the agent's local
// inventory. An unknown scope is an error (not a silent empty), mirroring the
// server's "unmatched host is a failure, not a skip" rule.
func resolveLocalScope(inv localInventory, scope string) ([]runnerproto.ManifestTarget, error) {
	if scope == "" {
		return nil, fmt.Errorf("local inventory mode: run has no scope to resolve")
	}
	hosts, ok := inv[scope]
	if !ok {
		return nil, fmt.Errorf("scope %q not found in local inventory", scope)
	}
	return hosts, nil
}

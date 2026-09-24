package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

func TestLocalCommandAnsible(t *testing.T) {
	// No managed inventory (local-mode / no scope inventory): runs as before, no -i.
	m := &runnerproto.ManifestResponse{RunType: "ansible", Body: "- hosts: all\n  tasks: []\n"}
	argv, stdin, _, err := localCommand(m, Config{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ansible-playbook", "/dev/stdin"}
	if len(argv) != len(want) || argv[0] != want[0] || argv[1] != want[1] {
		t.Errorf("ansible argv = %v, want %v", argv, want)
	}
	if stdin != m.Body {
		t.Errorf("ansible should feed body on stdin")
	}
}

func TestLocalCommandAnsibleWithInventory(t *testing.T) {
	workdir := t.TempDir()
	raw := "[web]\nweb1 ansible_host=10.0.0.1\n"
	m := &runnerproto.ManifestResponse{
		RunType:   "ansible",
		Body:      "- hosts: web\n  tasks: []\n",
		Inventory: &runnerproto.ManifestInventory{Raw: raw, Format: "ini"},
	}
	argv, stdin, _, err := localCommand(m, Config{}, workdir)
	if err != nil {
		t.Fatal(err)
	}
	// Expect: ansible-playbook -i <path> /dev/stdin
	if len(argv) != 4 || argv[0] != "ansible-playbook" || argv[1] != "-i" || argv[3] != "/dev/stdin" {
		t.Fatalf("argv = %v, want [ansible-playbook -i <path> /dev/stdin]", argv)
	}
	invPath := argv[2]
	// The inventory is materialized INSIDE the per-run workdir so the workdir
	// teardown removes it (no separate cleanup path).
	if filepath.Dir(invPath) != workdir {
		t.Errorf("inventory temp file %q not inside workdir %q", invPath, workdir)
	}
	if filepath.Ext(invPath) != ".ini" {
		t.Errorf("inventory temp file %q lacks .ini extension", invPath)
	}
	got, rerr := os.ReadFile(invPath)
	if rerr != nil {
		t.Fatalf("read materialized inventory: %v", rerr)
	}
	if string(got) != raw {
		t.Errorf("materialized inventory = %q, want byte-exact %q", got, raw)
	}
	if stdin != m.Body {
		t.Errorf("ansible should still feed body on stdin")
	}
}

func TestLocalCommandAnsibleWithLimit(t *testing.T) {
	m := &runnerproto.ManifestResponse{
		RunType:   "ansible",
		Body:      "- hosts: web\n",
		Inventory: &runnerproto.ManifestInventory{Raw: "[web]\nweb1\n", Format: "ini"},
		Limit:     "web",
	}
	argv, _, _, err := localCommand(m, Config{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Expect: [ansible-playbook -i <path> --limit web /dev/stdin]
	if len(argv) != 6 || argv[3] != "--limit" || argv[4] != "web" || argv[5] != "/dev/stdin" {
		t.Fatalf("argv = %v, want [ansible-playbook -i <path> --limit web /dev/stdin]", argv)
	}
}

func TestLocalCommandTerraform(t *testing.T) {
	m := &runnerproto.ManifestResponse{RunType: "terraform", Body: "plan -out=tfplan\n"}
	argv, _, _, err := localCommand(m, Config{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"terraform", "plan", "-out=tfplan"}
	if len(argv) != len(want) {
		t.Fatalf("terraform argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}

	// Blank body defaults to apply -auto-approve.
	m2 := &runnerproto.ManifestResponse{RunType: "terraform"}
	argv2, _, _, _ := localCommand(m2, Config{}, t.TempDir())
	if len(argv2) != 3 || argv2[1] != "apply" || argv2[2] != "-auto-approve" {
		t.Errorf("default terraform argv = %v", argv2)
	}
}

// ── auto --private-key for single-key runs (R2, Phase 3) ─────────────────────

// keyDirWith writes a dummy key file named `name` into a fresh temp dir and
// returns (dir, fullPath). The bytes don't need to parse — localCommand wires by
// path (resolveKeyPath stats the file); parsing is loadSigner/doctor's job.
func keyDirWith(t *testing.T, name string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

// argIndex returns the position of want in argv, or -1.
func argIndex(argv []string, want string) int {
	for i, a := range argv {
		if a == want {
			return i
		}
	}
	return -1
}

func TestLocalCommandPrivateKeySingleKeyHit(t *testing.T) {
	dir, keyPath := keyDirWith(t, "ansible_rh8_key")
	m := &runnerproto.ManifestResponse{
		RunType:   "ansible",
		Body:      "- hosts: all\n",
		Inventory: &runnerproto.ManifestInventory{Raw: "[web]\nweb1\n", Format: "ini"},
		Targets: []runnerproto.ManifestTarget{
			{Name: "web1", AuthKeyEnvVar: "ansible_rh8_key"},
			{Name: "web2", AuthKeyEnvVar: "ansible_rh8_key"}, // same name → still single
		},
	}
	argv, _, prov, err := localCommand(m, Config{KeyDir: dir}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pk := argIndex(argv, "--private-key")
	if pk < 0 || argv[pk+1] != keyPath {
		t.Fatalf("expected --private-key %s in argv, got %v", keyPath, argv)
	}
	// Placed BEFORE -i (plan 3.1), and -i/ /dev/stdin order otherwise unchanged.
	if i := argIndex(argv, "-i"); i < 0 || pk > i {
		t.Errorf("--private-key must come before -i: %v", argv)
	}
	if argv[len(argv)-1] != "/dev/stdin" {
		t.Errorf("/dev/stdin must stay last: %v", argv)
	}
	if !containsSubstr(prov, "--private-key") || !containsSubstr(prov, "ansible_rh8_key") {
		t.Errorf("expected a provenance line naming the wired key: %v", prov)
	}
}

func TestLocalCommandPrivateKeySingleKeyMiss(t *testing.T) {
	// One key name but no local file resolves → no --private-key, no provenance
	// (the env bridge / inventory lookup still applies).
	m := &runnerproto.ManifestResponse{
		RunType: "ansible",
		Body:    "- hosts: all\n",
		Targets: []runnerproto.ManifestTarget{{Name: "web1", AuthKeyEnvVar: "UNRESOLVED"}},
	}
	argv, _, prov, err := localCommand(m, Config{KeyDir: t.TempDir()}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if argIndex(argv, "--private-key") >= 0 {
		t.Errorf("no key file resolves → no --private-key: %v", argv)
	}
	if len(prov) != 0 {
		t.Errorf("single-key miss should be silent: %v", prov)
	}
}

func TestLocalCommandPrivateKeyMultiKey(t *testing.T) {
	// Two distinct key names → skip --private-key, emit one provenance line naming
	// the un-wired keys (D-B(a)). Even if both resolve to files.
	dir := t.TempDir()
	for _, n := range []string{"KEY_A", "KEY_B"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := &runnerproto.ManifestResponse{
		RunType: "ansible",
		Body:    "- hosts: all\n",
		Targets: []runnerproto.ManifestTarget{
			{Name: "a", AuthKeyEnvVar: "KEY_A"},
			{Name: "b", AuthKeyEnvVar: "KEY_B"},
		},
	}
	argv, _, prov, err := localCommand(m, Config{KeyDir: dir}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if argIndex(argv, "--private-key") >= 0 {
		t.Errorf("multi-key run must not auto-wire --private-key: %v", argv)
	}
	if !containsSubstr(prov, "KEY_A") || !containsSubstr(prov, "KEY_B") {
		t.Errorf("multi-key provenance must name the un-wired keys: %v", prov)
	}
}

func TestLocalCommandPrivateKeyCheckout(t *testing.T) {
	// Checkout run: --private-key still wired (before the entry playbook), vault
	// flag ordering preserved.
	dir, keyPath := keyDirWith(t, "DEPLOY")
	m := &runnerproto.ManifestResponse{
		RunType:  "ansible",
		Checkout: &runnerproto.ManifestCheckout{Entry: "site.yml", UsesVault: true},
		Targets:  []runnerproto.ManifestTarget{{Name: "h1", AuthKeyEnvVar: "DEPLOY"}},
	}
	cfg := Config{KeyDir: dir, VaultPasswordFile: "/etc/amadeus/vault.pw"}
	argv, stdin, _, err := localCommand(m, cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if stdin != "" {
		t.Errorf("checkout mode feeds no stdin, got %q", stdin)
	}
	pk := argIndex(argv, "--private-key")
	if pk < 0 || argv[pk+1] != keyPath {
		t.Fatalf("checkout run should wire --private-key: %v", argv)
	}
	// Entry playbook stays last; --vault-password-file still present.
	if argv[len(argv)-1] != "site.yml" {
		t.Errorf("entry playbook must stay last: %v", argv)
	}
	if argIndex(argv, "--vault-password-file") < 0 {
		t.Errorf("vault flag must be preserved: %v", argv)
	}
}

func TestLocalCommandPrivateKeyNoAuthBridge(t *testing.T) {
	// -no-auth-bridge disables auto --private-key (operator owns ansible auth).
	dir, _ := keyDirWith(t, "ansible_rh8_key")
	m := &runnerproto.ManifestResponse{
		RunType: "ansible",
		Body:    "- hosts: all\n",
		Targets: []runnerproto.ManifestTarget{{Name: "web1", AuthKeyEnvVar: "ansible_rh8_key"}},
	}
	argv, _, prov, err := localCommand(m, Config{KeyDir: dir, NoAuthBridge: true}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if argIndex(argv, "--private-key") >= 0 || len(prov) != 0 {
		t.Errorf("NoAuthBridge must suppress auto --private-key: argv=%v prov=%v", argv, prov)
	}
}

func TestLocalCommandPrivateKeyLocalModeNoTargets(t *testing.T) {
	// Local-mode ansible: Targets empty → nothing to wire, argv unchanged.
	m := &runnerproto.ManifestResponse{RunType: "ansible", Body: "- hosts: all\n"}
	argv, _, prov, err := localCommand(m, Config{KeyDir: t.TempDir()}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if argIndex(argv, "--private-key") >= 0 || len(prov) != 0 {
		t.Errorf("no targets → no wiring: argv=%v prov=%v", argv, prov)
	}
}

func TestLocalCommandPrivateKeyLocalModeRefusesStrayTargets(t *testing.T) {
	// Defense-in-depth (D2): a local-mode runner must not auto-wire from a Targets
	// list it should never have received, even if the keys resolve.
	dir, _ := keyDirWith(t, "STRAY_KEY")
	m := &runnerproto.ManifestResponse{
		RunType: "ansible",
		Body:    "- hosts: all\n",
		Targets: []runnerproto.ManifestTarget{{Name: "h1", AuthKeyEnvVar: "STRAY_KEY"}},
	}
	argv, _, prov, err := localCommand(m, Config{KeyDir: dir, Inventory: "local"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if argIndex(argv, "--private-key") >= 0 || len(prov) != 0 {
		t.Errorf("local-mode must refuse to wire from stray Targets: argv=%v prov=%v", argv, prov)
	}
}

// ── scoped child env (RX.9, Phase 1) ─────────────────────────────────────────

// envNames extracts the set of names present in a K=V env slice.
func envNames(env []string) map[string]string {
	out := map[string]string{}
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			out[k] = v
		}
	}
	return out
}

func TestBuildChildEnvNilPassthroughStillScopes(t *testing.T) {
	// Phase 5: scoped env is ALWAYS ON. A nil EnvPassthrough (an old server that
	// never sends the field) is treated as an EMPTY allowlist — base safe-list +
	// manifest Env only, NEVER the agent's full environment.
	environ := []string{"PATH=/usr/bin", "AGENT_SECRET=hunter2"}
	m := &runnerproto.ManifestResponse{Env: map[string]string{"STAGE": "prod"}}
	env, _, err := buildChildEnv(m, Config{}, environ, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := envNames(env)
	if _, leaked := got["AGENT_SECRET"]; leaked {
		t.Errorf("scoped env is always-on; a nil passthrough must NOT inherit the full agent env: %v", env)
	}
	if got["PATH"] != "/usr/bin" {
		t.Errorf("base safe-list PATH missing: %v", env)
	}
	if got["STAGE"] != "prod" {
		t.Errorf("manifest env missing: %v", env)
	}
}

func TestBuildChildEnvScoped(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin", "HOME=/home/runner", "LANG=en_US.UTF-8", "LC_ALL=C.UTF-8",
		"SSH_AUTH_SOCK=/run/agent.sock",
		"AGENT_SECRET=hunter2", // secrets.env-style var, NOT allowlisted
		"WEB_PASS=s3cret",      // allowlisted via passthrough
		"CUSTOM_BASE=v",        // allowlisted via -env-base-extra
	}
	m := &runnerproto.ManifestResponse{
		Env:            map[string]string{"STAGE": "prod"},
		EnvPassthrough: []string{"WEB_PASS"},
	}
	cfg := Config{EnvBaseExtra: []string{"CUSTOM_BASE"}}
	env, _, err := buildChildEnv(m, cfg, environ, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := envNames(env)
	for name, want := range map[string]string{
		"PATH": "/usr/bin", "HOME": "/home/runner", "LANG": "en_US.UTF-8",
		"LC_ALL":        "C.UTF-8", // LC_* prefix survives (R5)
		"SSH_AUTH_SOCK": "/run/agent.sock",
		"STAGE":         "prod",   // manifest env
		"WEB_PASS":      "s3cret", // passthrough, resolved from agent env
		"CUSTOM_BASE":   "v",      // -env-base-extra
	} {
		if got[name] != want {
			t.Errorf("scoped env %s = %q, want %q (env: %v)", name, got[name], want, env)
		}
	}
	// The core hardening: a non-allowlisted agent var must NOT reach the child.
	if _, leaked := got["AGENT_SECRET"]; leaked {
		t.Errorf("scoped env leaked AGENT_SECRET: %v", env)
	}
}

func TestBuildChildEnvScopedEmptyList(t *testing.T) {
	// PRESENCE of the (empty) list flips scoping — "no extra names" ≠ "no scoping".
	environ := []string{"PATH=/usr/bin", "AGENT_SECRET=hunter2"}
	m := &runnerproto.ManifestResponse{EnvPassthrough: []string{}}
	env, _, err := buildChildEnv(m, Config{}, environ, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := envNames(env)
	if _, leaked := got["AGENT_SECRET"]; leaked {
		t.Errorf("empty-but-present passthrough must still scope; leaked AGENT_SECRET: %v", env)
	}
	if got["PATH"] != "/usr/bin" {
		t.Errorf("base safe-list PATH missing: %v", env)
	}
}

func TestBuildChildEnvUnsetPassthroughFails(t *testing.T) {
	m := &runnerproto.ManifestResponse{EnvPassthrough: []string{"NOT_PROVISIONED"}}
	_, _, err := buildChildEnv(m, Config{}, []string{"PATH=/usr/bin"}, nil)
	if err == nil {
		t.Fatal("expected loud error for unset passthrough name, got nil")
	}
	if !strings.Contains(err.Error(), "NOT_PROVISIONED") {
		t.Errorf("error should name the missing var: %v", err)
	}
	// The message should point at the bridging remedies (key-map / key-dir) and
	// no longer reference the retired secrets.env example.
	if !strings.Contains(err.Error(), "key-map") || !strings.Contains(err.Error(), "key-dir") {
		t.Errorf("error should mention key-map/key-dir bridging: %v", err)
	}
	if strings.Contains(err.Error(), "secrets.env") {
		t.Errorf("error should drop the stale secrets.env example: %v", err)
	}
}

// ── DR-1: the agent's config namespace is unreachable from a job ─────────────

func TestBuildChildEnvRefusesAgentConfigPassthrough(t *testing.T) {
	// A job that names the agent's own configuration namespace must be refused,
	// not quietly resolved: under a multi-use bootstrap token this value is a
	// permanent enrollment credential, and a passthrough value is not a declared
	// secret binding, so nothing would redact it out of the run log.
	environ := []string{"PATH=/usr/bin", "AMADEUS_RUNNER_REGISTRATION_TOKEN=amt_reg_supersecret"}
	m := &runnerproto.ManifestResponse{EnvPassthrough: []string{"AMADEUS_RUNNER_REGISTRATION_TOKEN"}}

	env, _, err := buildChildEnv(m, Config{}, environ, nil)
	if err == nil {
		t.Fatal("expected a refusal for an AMADEUS_RUNNER_* passthrough, got nil")
	}
	if env != nil {
		t.Fatalf("a refused run must yield no child env at all, got %v", env)
	}
	if !strings.Contains(err.Error(), "AMADEUS_RUNNER_REGISTRATION_TOKEN") {
		t.Errorf("error should name the refused var: %v", err)
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("error should read as a refusal, not a missing var: %v", err)
	}
	// The missing-var remedy ("set each as an env var") is the WRONG advice here
	// and would walk an operator into putting the token back where it leaks.
	if strings.Contains(err.Error(), "not resolvable") {
		t.Errorf("refusal must not reuse the missing-var message: %v", err)
	}
	if strings.Contains(err.Error(), "amt_reg_supersecret") {
		t.Errorf("the error must never echo the secret value: %v", err)
	}
}

func TestBuildChildEnvAgentConfigRefusalPrecedesResolution(t *testing.T) {
	// The refusal must come before every resolution path, so a name that WOULD
	// have resolved (manifest Env, or the dispatch-time Secrets block) is still
	// refused rather than served from the other source.
	m := &runnerproto.ManifestResponse{
		Env:            map[string]string{"AMADEUS_RUNNER_SERVER": "https://evil.example"},
		Secrets:        map[string]string{"AMADEUS_RUNNER_CHECKOUT_TOKEN": "glpat-xxx"},
		EnvPassthrough: []string{"AMADEUS_RUNNER_CHECKOUT_TOKEN"},
	}
	if _, _, err := buildChildEnv(m, Config{}, []string{"PATH=/usr/bin"}, nil); err == nil {
		t.Fatal("a manifest-supplied agent-config name must still be refused")
	}
}

func TestBuildChildEnvAgentConfigNotReachableViaBareFallback(t *testing.T) {
	// The derived-reference bridge falls back to the BARE name in the agent env,
	// which is a different key than the passthrough name — so denying the
	// passthrough name alone is not enough. A reference whose bare name is the
	// agent's own config would otherwise be resolved and handed to the child under
	// the prefixed name.
	environ := []string{"PATH=/usr/bin", "AMADEUS_RUNNER_REGISTRATION_TOKEN=amt_reg_supersecret"}
	m := &runnerproto.ManifestResponse{
		EnvPassthrough: []string{"AMADEUS_SECRET_AMADEUS_RUNNER_REGISTRATION_TOKEN"},
	}

	env, _, err := buildChildEnv(m, Config{}, environ, nil)
	if err == nil {
		t.Fatal("a reference whose bare name is agent config must be refused")
	}
	for _, e := range env {
		if strings.Contains(e, "amt_reg_supersecret") {
			t.Fatalf("agent config leaked through the bare-name fallback: %q", e)
		}
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("expected a refusal: %v", err)
	}
}

func TestBuildChildEnvReferencePassthroughsUnaffected(t *testing.T) {
	// The DR-Q1 guard: AMADEUS_RUNNER_ is denied, but the four reserved REFERENCE
	// prefixes must keep resolving. AMADEUS_RUN_ in particular differs from
	// AMADEUS_RUNNER_ only by "NER" and must not be caught by the denial.
	environ := []string{
		"PATH=/usr/bin",
		"AMADEUS_SECRET_RH8_BECOME_PASS=becomepw",
		"AMADEUS_KEY_ANSIBLE_RH8=/etc/amadeus-runner/keys/rh8",
		"AMADEUS_RUN_ID=01a03520-0000-7000-0000-000000000000",
	}
	m := &runnerproto.ManifestResponse{EnvPassthrough: []string{
		"AMADEUS_SECRET_RH8_BECOME_PASS", "AMADEUS_KEY_ANSIBLE_RH8", "AMADEUS_RUN_ID",
	}}

	env, _, err := buildChildEnv(m, Config{}, environ, nil)
	if err != nil {
		t.Fatalf("reference-prefixed passthroughs must still resolve: %v", err)
	}
	got := envNames(env)
	for _, n := range []string{"AMADEUS_SECRET_RH8_BECOME_PASS", "AMADEUS_KEY_ANSIBLE_RH8", "AMADEUS_RUN_ID"} {
		if _, ok := got[n]; !ok {
			t.Errorf("%s should have resolved: %v", n, env)
		}
	}
}

func TestBuildChildEnvBaseExtraCannotReadmitAgentConfig(t *testing.T) {
	// -env-base-extra widens the base allow-list, which reaches EVERY child — not
	// only runs declaring a passthrough — so it must not become a second door to
	// the same namespace. The refusal is reported through provenance rather than
	// dropped silently.
	environ := []string{"PATH=/usr/bin", "AMADEUS_RUNNER_REGISTRATION_TOKEN=amt_reg_supersecret"}
	cfg := Config{EnvBaseExtra: []string{"AMADEUS_RUNNER_REGISTRATION_TOKEN"}}
	m := &runnerproto.ManifestResponse{}

	env, provenance, err := buildChildEnv(m, cfg, environ, nil)
	if err != nil {
		t.Fatalf("a refused base-extra entry should not fail the run: %v", err)
	}
	if _, leaked := envNames(env)["AMADEUS_RUNNER_REGISTRATION_TOKEN"]; leaked {
		t.Fatalf("-env-base-extra must not re-admit an agent-config name: %v", env)
	}
	joined := strings.Join(provenance, "\n")
	if !strings.Contains(joined, "AMADEUS_RUNNER_REGISTRATION_TOKEN") {
		t.Errorf("the refusal should be visible in provenance, got %q", joined)
	}
	if strings.Contains(joined, "amt_reg_supersecret") {
		t.Errorf("provenance must never echo the secret value: %q", joined)
	}
}

// ── auth bridge + trust store (Phase 1, R1/R3) ───────────────────────────────

func TestResolveKeyPath(t *testing.T) {
	dir := t.TempDir()
	// A key-dir file per candidate extension.
	for _, name := range []string{"plainkey", "pemkey.pem", "dotkey.key"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mapped := filepath.Join(dir, "mapped.pem")
	if err := os.WriteFile(mapped, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyMap := map[string]string{"MAPPED": mapped, "BOTH": mapped}
	// A key-dir file that ALSO has a map entry, to prove key-map precedence.
	if err := os.WriteFile(filepath.Join(dir, "BOTH"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		wantOK   bool
		wantSrc  string
		wantPath string
	}{
		{"MAPPED", true, "key-map", mapped},
		{"BOTH", true, "key-map", mapped}, // key-map beats key-dir
		{"plainkey", true, "key-dir", filepath.Join(dir, "plainkey")},
		{"pemkey", true, "key-dir", filepath.Join(dir, "pemkey.pem")},
		{"dotkey", true, "key-dir", filepath.Join(dir, "dotkey.key")},
		{"nonesuch", false, "", ""},
		{"", false, "", ""},
	}
	for _, tc := range tests {
		path, src, ok := resolveKeyPath(keyMap, dir, tc.name)
		if ok != tc.wantOK || src != tc.wantSrc || path != tc.wantPath {
			t.Errorf("resolveKeyPath(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.name, path, src, ok, tc.wantPath, tc.wantSrc, tc.wantOK)
		}
	}

	// No key-dir configured: only the map is consulted.
	if _, _, ok := resolveKeyPath(keyMap, "", "plainkey"); ok {
		t.Errorf("empty key-dir must not resolve a key-dir name")
	}
}

func TestBuildChildEnvBridgesPassthroughToKeyFile(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "ansible_rh8_key")
	if err := os.WriteFile(keyFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &runnerproto.ManifestResponse{
		RunType:        "ansible",
		EnvPassthrough: []string{"ansible_rh8_key"},
	}
	// The name is NOT in the agent env — it only exists as a key-dir FILE.
	cfg := Config{KeyDir: dir}
	env, prov, err := buildChildEnv(m, cfg, []string{"PATH=/usr/bin"}, nil)
	if err != nil {
		t.Fatalf("bridge should resolve the key file, not error: %v", err)
	}
	if got := envNames(env)["ansible_rh8_key"]; got != keyFile {
		t.Errorf("bridged env var = %q, want the key path %q", got, keyFile)
	}
	if !containsSubstr(prov, "auth: key") || !containsSubstr(prov, keyFile) || !containsSubstr(prov, "key-dir") {
		t.Errorf("expected an auth provenance line naming the path+source, got %v", prov)
	}
}

func TestBuildChildEnvExplicitEnvWinsOverBridge(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "KEYNAME"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &runnerproto.ManifestResponse{RunType: "ansible", EnvPassthrough: []string{"KEYNAME"}}
	cfg := Config{KeyDir: dir}
	// KEYNAME is set in the agent env (e.g. the field workaround: a path value).
	env, prov, err := buildChildEnv(m, cfg, []string{"PATH=/usr/bin", "KEYNAME=/explicit/path"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := envNames(env)["KEYNAME"]; got != "/explicit/path" {
		t.Errorf("explicit env value must win over the bridge, got %q", got)
	}
	if containsSubstr(prov, "auth: key") {
		t.Errorf("no bridge provenance expected when the env value wins: %v", prov)
	}
}

func TestBuildChildEnvNoAuthBridgeDisablesBridge(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "KEYNAME"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &runnerproto.ManifestResponse{RunType: "ansible", EnvPassthrough: []string{"KEYNAME"}}
	cfg := Config{KeyDir: dir, NoAuthBridge: true}
	// With the bridge off, an unset passthrough name is the loud error even though
	// a resolvable key file exists.
	if _, _, err := buildChildEnv(m, cfg, []string{"PATH=/usr/bin"}, nil); err == nil {
		t.Fatal("NoAuthBridge must not bridge a passthrough name to a key file")
	}
}

func TestBuildChildEnvAnsibleTrustInjection(t *testing.T) {
	m := &runnerproto.ManifestResponse{RunType: "ansible"}
	cfg := Config{KnownHostsFile: "/var/lib/amadeus-runner/known_hosts"}
	env, prov, err := buildChildEnv(m, cfg, []string{"PATH=/usr/bin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := envNames(env)["ANSIBLE_SSH_COMMON_ARGS"]
	want := "-o UserKnownHostsFile=/var/lib/amadeus-runner/known_hosts -o StrictHostKeyChecking=yes"
	if got != want {
		t.Errorf("ANSIBLE_SSH_COMMON_ARGS = %q, want %q", got, want)
	}
	if !containsSubstr(prov, "trust: known_hosts") {
		t.Errorf("expected a trust provenance line, got %v", prov)
	}
}

func TestBuildChildEnvAnsibleTrustOverride(t *testing.T) {
	// D-C: an operator -ansible-ssh-common-args override is injected INSTEAD.
	m := &runnerproto.ManifestResponse{RunType: "ansible"}
	cfg := Config{
		KnownHostsFile:       "/var/lib/amadeus-runner/known_hosts",
		AnsibleSSHCommonArgs: "-o ProxyJump=bastion -o StrictHostKeyChecking=yes",
	}
	env, _, err := buildChildEnv(m, cfg, []string{"PATH=/usr/bin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := envNames(env)["ANSIBLE_SSH_COMMON_ARGS"]; got != cfg.AnsibleSSHCommonArgs {
		t.Errorf("override must be injected verbatim, got %q", got)
	}
}

func TestBuildChildEnvAnsibleTrustNoClobber(t *testing.T) {
	// An operator value supplied through a channel (here: passthrough) is never
	// overridden by the bridge injection.
	m := &runnerproto.ManifestResponse{
		RunType:        "ansible",
		EnvPassthrough: []string{"ANSIBLE_SSH_COMMON_ARGS"},
	}
	cfg := Config{KnownHostsFile: "/agent/known_hosts"}
	environ := []string{"PATH=/usr/bin", "ANSIBLE_SSH_COMMON_ARGS=-o ControlMaster=no"}
	env, _, err := buildChildEnv(m, cfg, environ, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := envNames(env)["ANSIBLE_SSH_COMMON_ARGS"]; got != "-o ControlMaster=no" {
		t.Errorf("operator-supplied ANSIBLE_SSH_COMMON_ARGS must not be clobbered, got %q", got)
	}
}

func TestBuildChildEnvTerraformNoTrustInjection(t *testing.T) {
	m := &runnerproto.ManifestResponse{RunType: "terraform"}
	cfg := Config{KnownHostsFile: "/agent/known_hosts"}
	env, _, err := buildChildEnv(m, cfg, []string{"PATH=/usr/bin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := envNames(env)["ANSIBLE_SSH_COMMON_ARGS"]; present {
		t.Errorf("terraform runs must not get ANSIBLE_SSH_COMMON_ARGS: %v", env)
	}
}

// containsSubstr reports whether any line in ss contains sub.
func containsSubstr(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestBuildChildEnvManifestEnvSatisfiesPassthrough(t *testing.T) {
	// A passthrough name supplied by the manifest Env snapshot is not "missing".
	m := &runnerproto.ManifestResponse{
		Env:            map[string]string{"FROM_SERVER": "x"},
		EnvPassthrough: []string{"FROM_SERVER"},
	}
	env, _, err := buildChildEnv(m, Config{}, []string{"PATH=/usr/bin"}, nil)
	if err != nil {
		t.Fatalf("manifest-env-satisfied passthrough should not error: %v", err)
	}
	if envNames(env)["FROM_SERVER"] != "x" {
		t.Errorf("FROM_SERVER missing: %v", env)
	}
}

func TestBuildChildEnvExcludeSSHAuthSock(t *testing.T) {
	environ := []string{"PATH=/usr/bin", "SSH_AUTH_SOCK=/run/agent.sock"}
	m := &runnerproto.ManifestResponse{EnvPassthrough: []string{}}
	env, _, err := buildChildEnv(m, Config{ExcludeSSHAuthSock: true}, environ, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := envNames(env)["SSH_AUTH_SOCK"]; present {
		t.Errorf("ExcludeSSHAuthSock must drop SSH_AUTH_SOCK: %v", env)
	}
}

// ── per-run workdir + end-to-end scoped exec ─────────────────────────────────

// fakeToolchain installs a fake ansible-playbook shell script on PATH and
// returns the collector for emitted lines.
func fakeToolchain(t *testing.T, script string) (emit func(string), lines func() []string) {
	t.Helper()
	binDir := t.TempDir()
	path := filepath.Join(binDir, "ansible-playbook")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	var mu sync.Mutex
	var got []string
	emit = func(line string) { mu.Lock(); got = append(got, line); mu.Unlock() }
	lines = func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }
	return emit, lines
}

// assertNoRunDirs fails if any amadeus-run-* workdir survives under stateDir.
func assertNoRunDirs(t *testing.T, stateDir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(stateDir, "amadeus-run-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("per-run workdir not torn down: %v", matches)
	}
}

func TestRunLocalToolchainScopedEnvAndWorkdir(t *testing.T) {
	stateDir := t.TempDir()
	emit, lines := fakeToolchain(t, "env\npwd\n")
	t.Setenv("AGENT_SECRET", "hunter2")

	m := &runnerproto.ManifestResponse{
		RunType:        "ansible",
		Body:           "- hosts: all\n",
		Env:            map[string]string{"STAGE": "prod"},
		EnvPassthrough: []string{}, // present ⇒ scoped
	}
	code := runLocalToolchain(context.Background(), m, Config{StateDir: stateDir}, emit)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; lines: %v", code, lines())
	}
	out := strings.Join(lines(), "\n")
	if !strings.Contains(out, "STAGE=prod") {
		t.Errorf("manifest env did not reach the child:\n%s", out)
	}
	if strings.Contains(out, "AGENT_SECRET") {
		t.Errorf("scoped run leaked AGENT_SECRET to the child:\n%s", out)
	}
	// cwd is the per-run workdir (under StateDir), torn down after the run.
	if !strings.Contains(out, filepath.Join(stateDir, "amadeus-run-")) {
		t.Errorf("child cwd is not the per-run workdir under StateDir:\n%s", out)
	}
	assertNoRunDirs(t, stateDir)
}

func TestRunLocalToolchainNilPassthroughScopes(t *testing.T) {
	stateDir := t.TempDir()
	emit, lines := fakeToolchain(t, "env\n")
	t.Setenv("AGENT_SECRET", "hunter2")

	// Phase 5: scoped env is always-on. Even with no EnvPassthrough field (an old
	// server), a secrets.env-style var must NOT reach the child.
	m := &runnerproto.ManifestResponse{RunType: "ansible", Body: "- hosts: all\n"}
	code := runLocalToolchain(context.Background(), m, Config{StateDir: stateDir}, emit)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; lines: %v", code, lines())
	}
	if strings.Contains(strings.Join(lines(), "\n"), "AGENT_SECRET=hunter2") {
		t.Errorf("scoped env is always-on; a nil-passthrough run must NOT leak the agent env")
	}
	assertNoRunDirs(t, stateDir)
}

func TestRunLocalToolchainWorkdirTornDownOnFailure(t *testing.T) {
	stateDir := t.TempDir()
	// Empty PATH dir: ansible-playbook cannot be found, start fails.
	t.Setenv("PATH", t.TempDir())
	m := &runnerproto.ManifestResponse{RunType: "ansible", Body: "- hosts: all\n"}
	var mu sync.Mutex
	var got []string
	emit := func(line string) { mu.Lock(); got = append(got, line); mu.Unlock() }
	if code := runLocalToolchain(context.Background(), m, Config{StateDir: stateDir}, emit); code != -1 {
		t.Fatalf("exit = %d, want -1", code)
	}
	assertNoRunDirs(t, stateDir)
}

func TestRunLocalToolchainUnsetPassthroughFailsRun(t *testing.T) {
	stateDir := t.TempDir()
	emit, lines := fakeToolchain(t, "env\n")
	m := &runnerproto.ManifestResponse{
		RunType:        "ansible",
		Body:           "- hosts: all\n",
		EnvPassthrough: []string{"NOT_PROVISIONED_ANYWHERE"},
	}
	code := runLocalToolchain(context.Background(), m, Config{StateDir: stateDir}, emit)
	if code != -1 {
		t.Fatalf("exit = %d, want -1 (loud refusal)", code)
	}
	if !strings.Contains(strings.Join(lines(), "\n"), "NOT_PROVISIONED_ANYWHERE") {
		t.Errorf("refusal must name the missing var: %v", lines())
	}
	assertNoRunDirs(t, stateDir)
}

// ── process-level timeout (§6.1) ─────────────────────────────────────────────

func TestExecutorRunTimeout(t *testing.T) {
	stateDir := t.TempDir()
	emit, _ := fakeToolchain(t, "sleep 30\n")
	_ = emit // executor writes via the log buffer, not the collector

	m := &runnerproto.ManifestResponse{
		RunType:        "ansible",
		Body:           "- hosts: all\n",
		TimeoutSeconds: 1,
	}
	e := &executor{cfg: Config{StateDir: stateDir}}
	buf := &logBuffer{}
	start := time.Now()
	code := e.run(context.Background(), m, buf)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout not enforced: run took %v", elapsed)
	}
	if code == 0 {
		t.Errorf("timed-out run must not exit 0")
	}
	if out := string(buf.pending()); !strings.Contains(out, "timed out after 1s") {
		t.Errorf("expected a timeout line in the log, got:\n%s", out)
	}
	assertNoRunDirs(t, stateDir)
}

func TestResolveLocalScope(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inv.json")
	if err := os.WriteFile(path, []byte(`{
		"prod": [ {"name":"h1","address":"10.0.0.1","port":22,"user":"deploy","authKeyEnvVar":"PROD_KEY"} ]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	inv, err := loadLocalInventory(path)
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := resolveLocalScope(inv, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Name != "h1" || hosts[0].Address != "10.0.0.1" {
		t.Errorf("resolved hosts = %+v", hosts)
	}
	// Unknown scope is an error (not a silent empty).
	if _, err := resolveLocalScope(inv, "nope"); err == nil {
		t.Error("expected error for unknown scope")
	}
}

// ── RP-9 · identity override as connection extra-vars ────────────────────────
//
// RP-Q1: `-e` is ansible's highest-precedence tier, so the operator's override
// beats inventory-authored ansible_user / ansible_ssh_private_key_file on every
// host. The obvious alternative (`-u` / `--private-key`) sits in the LOWEST tier
// and would lose to the inventory on exactly the hosts an admin bothered to
// wire — applying silently and only sometimes.

// extraVar returns the value of the first `-e name=value` pair in argv, or "".
func extraVar(argv []string, name string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-e" && strings.HasPrefix(argv[i+1], name+"=") {
			return strings.TrimPrefix(argv[i+1], name+"=")
		}
	}
	return ""
}

func TestLocalCommandIdentityExtraVars(t *testing.T) {
	dir, keyPath := keyDirWith(t, "prod-key")
	m := &runnerproto.ManifestResponse{
		RunType:   "ansible",
		Body:      "- hosts: all\n",
		Inventory: &runnerproto.ManifestInventory{Raw: "[web]\nweb1\n", Format: "ini"},
		SSHUser:   "deploy",
		SSHKeyRef: "AMADEUS_KEY_prod-key",
	}
	argv, _, prov, err := localCommand(m, Config{KeyDir: dir}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := extraVar(argv, "ansible_user"); got != "deploy" {
		t.Errorf("-e ansible_user = %q, want deploy; argv %v", got, argv)
	}
	// The derived AMADEUS_KEY_<label> reference resolves through the same key
	// custody as everything else — including a key DELIVERED for this run, which
	// the executor merges into KeyMap before calling us.
	if got := extraVar(argv, "ansible_ssh_private_key_file"); got != keyPath {
		t.Errorf("-e ansible_ssh_private_key_file = %q, want %s; argv %v", got, keyPath, argv)
	}
	if argv[len(argv)-1] != "/dev/stdin" {
		t.Errorf("/dev/stdin must stay last: %v", argv)
	}
	if !containsSubstr(prov, "ansible_user") || !containsSubstr(prov, "overrides inventory") {
		t.Errorf("expected provenance naming the applied identity: %v", prov)
	}
}

// The override and the --private-key auto-wire must never BOTH fire: they wire
// the same concept through two precedence tiers, and the auto-wire's tier is the
// one the inventory beats — so leaving it on would make the effective key differ
// per host depending on what the inventory happens to say.
func TestLocalCommandIdentitySuppressesAutoPrivateKey(t *testing.T) {
	dir, _ := keyDirWith(t, "prod-key")
	// A target key that WOULD auto-wire on its own (single distinct name, and the
	// file exists in the same key dir).
	if err := os.WriteFile(filepath.Join(dir, "inventory_key"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &runnerproto.ManifestResponse{
		RunType:   "ansible",
		Body:      "- hosts: all\n",
		SSHKeyRef: "AMADEUS_KEY_prod-key",
		Targets:   []runnerproto.ManifestTarget{{Name: "web1", AuthKeyEnvVar: "inventory_key"}},
	}
	argv, _, prov, err := localCommand(m, Config{KeyDir: dir}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if i := argIndex(argv, "--private-key"); i >= 0 {
		t.Errorf("--private-key must not be auto-wired under an override: %v", argv)
	}
	if extraVar(argv, "ansible_ssh_private_key_file") == "" {
		t.Errorf("the override key must still be applied as an extra-var: %v", argv)
	}
	if !containsSubstr(prov, "auto-wiring skipped") {
		t.Errorf("the skipped auto-wire must be stated in the run log: %v", prov)
	}
}

// Fail CLOSED: the server only sets sshKeyRef after delivering that key, so an
// unresolvable reference means delivery did not happen. Running anyway would
// connect with the INVENTORY's key while the run record claims the operator's —
// the exact silent-wrong-identity outcome the override path exists to prevent.
func TestLocalCommandIdentityUndeliveredKeyFails(t *testing.T) {
	m := &runnerproto.ManifestResponse{
		RunType:   "ansible",
		Body:      "- hosts: all\n",
		SSHKeyRef: "AMADEUS_KEY_never-delivered",
	}
	_, _, _, err := localCommand(m, Config{KeyDir: t.TempDir()}, t.TempDir())
	if err == nil {
		t.Fatal("expected a hard failure when the override key was not delivered")
	}
	if !strings.Contains(err.Error(), "never-delivered") {
		t.Errorf("error should name the missing reference: %v", err)
	}
}

// A user-only override needs no key delivery at all.
func TestLocalCommandIdentityUserOnly(t *testing.T) {
	m := &runnerproto.ManifestResponse{RunType: "ansible", Body: "- hosts: all\n", SSHUser: "deploy"}
	argv, _, _, err := localCommand(m, Config{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := extraVar(argv, "ansible_user"); got != "deploy" {
		t.Errorf("-e ansible_user = %q, want deploy", got)
	}
	if extraVar(argv, "ansible_ssh_private_key_file") != "" {
		t.Errorf("no key was overridden, so no key extra-var belongs in argv: %v", argv)
	}
}

// Terraform never carries identity (the server never sets the fields for it);
// if one ever arrived, the terraform branch must ignore it rather than invent a
// flag its toolchain has no concept of.
func TestLocalCommandTerraformIgnoresIdentity(t *testing.T) {
	m := &runnerproto.ManifestResponse{RunType: "terraform", Body: "", SSHUser: "deploy", SSHKeyRef: "AMADEUS_KEY_prod-key"}
	argv, _, _, err := localCommand(m, Config{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range argv {
		if strings.Contains(a, "ansible_user") || strings.Contains(a, "ansible_ssh_private_key_file") {
			t.Errorf("terraform argv carried an ansible identity var: %v", argv)
		}
	}
}

// ── Phase 3 · advanced ansible options in argv (RP-18) ──────────────────────

func TestLocalCommandAnsibleOptions(t *testing.T) {
	m := &runnerproto.ManifestResponse{
		RunType:   "ansible",
		Body:      "- hosts: all\n",
		Inventory: &runnerproto.ManifestInventory{Raw: "[web]\nweb1\n", Format: "ini"},
		AnsibleOptions: &runnerproto.ManifestAnsibleOptions{
			Check: true, Diff: true,
			Tags: []string{"certs", "config"}, SkipTags: []string{"reboot"},
			Verbosity: 3, Become: true, BecomeUser: "svc",
			ExtraVars: map[string]string{"env_name": "staging", "region": "eu"},
		},
	}
	argv, _, prov, err := localCommand(m, Config{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if argIndex(argv, "--check") < 0 || argIndex(argv, "--diff") < 0 || argIndex(argv, "--become") < 0 {
		t.Errorf("missing flags: %v", argv)
	}
	if i := argIndex(argv, "--tags"); i < 0 || argv[i+1] != "certs,config" {
		t.Errorf("--tags mis-rendered: %v", argv)
	}
	if i := argIndex(argv, "--skip-tags"); i < 0 || argv[i+1] != "reboot" {
		t.Errorf("--skip-tags mis-rendered: %v", argv)
	}
	if argIndex(argv, "-vvv") < 0 {
		t.Errorf("verbosity 3 should render -vvv: %v", argv)
	}
	if i := argIndex(argv, "--become-user"); i < 0 || argv[i+1] != "svc" {
		t.Errorf("--become-user mis-rendered: %v", argv)
	}
	if got := extraVar(argv, "env_name"); got != "staging" {
		t.Errorf("-e env_name = %q, want staging", got)
	}
	// Sorted, so the same envelope always renders the same argv.
	if strings.Index(strings.Join(argv, " "), "env_name=") > strings.Index(strings.Join(argv, " "), "region=") {
		t.Errorf("extra-vars should render in sorted order: %v", argv)
	}
	if !containsSubstr(prov, "DRY RUN") {
		t.Errorf("check mode must be stated in the run log: %v", prov)
	}
	if argv[len(argv)-1] != "/dev/stdin" {
		t.Errorf("/dev/stdin must stay last: %v", argv)
	}
}

// The ordering rule that keeps an operator from forging the run's identity:
// ansible takes the LAST occurrence of a repeated var, so the identity pair must
// be emitted after the operator's own extra-vars. (The trigger boundary also
// refuses the two identity names — this is the second lock, and the one that
// still holds if a run is constructed some other way.)
func TestLocalCommandIdentityWinsOverOperatorExtraVars(t *testing.T) {
	dir, keyPath := keyDirWith(t, "prod-key")
	m := &runnerproto.ManifestResponse{
		RunType:   "ansible",
		Body:      "- hosts: all\n",
		SSHUser:   "deploy",
		SSHKeyRef: "AMADEUS_KEY_prod-key",
		AnsibleOptions: &runnerproto.ManifestAnsibleOptions{
			ExtraVars: map[string]string{"ansible_user": "attacker", "ansible_ssh_private_key_file": "/tmp/evil"},
		},
	}
	argv, _, _, err := localCommand(m, Config{KeyDir: dir}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	// Both occurrences are present, but the identity one comes LAST and therefore
	// wins in ansible's precedence.
	if strings.LastIndex(joined, "ansible_user=deploy") < strings.LastIndex(joined, "ansible_user=attacker") {
		t.Errorf("identity ansible_user must be emitted after any operator extra-var: %v", argv)
	}
	if strings.LastIndex(joined, "ansible_ssh_private_key_file="+keyPath) < strings.LastIndex(joined, "/tmp/evil") {
		t.Errorf("identity key must be emitted after any operator extra-var: %v", argv)
	}
}

// A negative verbosity must never reach strings.Repeat (it panics), and an
// out-of-range positive one is clamped rather than rendered as -vvvvvvv.
func TestLocalCommandVerbosityBounds(t *testing.T) {
	for _, v := range []int{-3, 0} {
		m := &runnerproto.ManifestResponse{RunType: "ansible", Body: "x",
			AnsibleOptions: &runnerproto.ManifestAnsibleOptions{Verbosity: v}}
		argv, _, _, err := localCommand(m, Config{}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range argv {
			if strings.HasPrefix(a, "-v") {
				t.Errorf("verbosity %d rendered %q", v, a)
			}
		}
	}
	m := &runnerproto.ManifestResponse{RunType: "ansible", Body: "x",
		AnsibleOptions: &runnerproto.ManifestAnsibleOptions{Verbosity: 99}}
	argv, _, _, err := localCommand(m, Config{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if argIndex(argv, "-vvvv") < 0 {
		t.Errorf("out-of-range verbosity should clamp to -vvvv: %v", argv)
	}
}

// A run with no options must render exactly the argv it rendered before Phase 3.
func TestLocalCommandNoAnsibleOptions(t *testing.T) {
	m := &runnerproto.ManifestResponse{RunType: "ansible", Body: "- hosts: all\n"}
	argv, _, _, err := localCommand(m, Config{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != 2 || argv[0] != "ansible-playbook" || argv[1] != "/dev/stdin" {
		t.Errorf("plain ansible argv changed: %v", argv)
	}
}

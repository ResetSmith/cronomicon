package agent

import (
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// TestBuildChildEnvInjectsSecrets (P1.4): the manifest Secrets block (resolved
// CRONOMICON_SECRET_*/CRONOMICON_VAR_* values) reaches a local-toolchain child env, and
// a delivered secret value wins over a same-named bare runner-local fallback.
func TestBuildChildEnvInjectsSecrets(t *testing.T) {
	environ := []string{"PATH=/usr/bin", "DB_PASS=bare-runner-local-value"}
	m := &runnerproto.ManifestResponse{
		Env:            map[string]string{"STAGE": "prod"},
		Secrets:        map[string]string{"CRONOMICON_SECRET_DB_PASS": "injected-secret", "CRONOMICON_VAR_REGION": "us-east"},
		EnvPassthrough: []string{"CRONOMICON_SECRET_DB_PASS"}, // also requested via passthrough
	}
	env, _, err := buildChildEnv(m, Config{}, environ, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := envNames(env)
	if got["CRONOMICON_SECRET_DB_PASS"] != "injected-secret" {
		t.Errorf("injected secret = %q, want injected-secret (bare fallback must not win): %v", got["CRONOMICON_SECRET_DB_PASS"], env)
	}
	if got["CRONOMICON_VAR_REGION"] != "us-east" {
		t.Errorf("injected var = %q, want us-east", got["CRONOMICON_VAR_REGION"])
	}
	if got["STAGE"] != "prod" {
		t.Errorf("manifest env lost: %v", env)
	}
}

// TestBuildChildEnvInjectedBeatsPrefixedLocal (P1.4 review fix): a server-resolved
// reference value must win even when the runner's OWN env holds the exact prefixed
// name (a migrated secrets.env end-state) and the job lists it in env_passthrough.
func TestBuildChildEnvInjectedBeatsPrefixedLocal(t *testing.T) {
	environ := []string{"PATH=/usr/bin", "CRONOMICON_SECRET_DB_PASS=stale-runner-local"}
	m := &runnerproto.ManifestResponse{
		Secrets:        map[string]string{"CRONOMICON_SECRET_DB_PASS": "fresh-injected"},
		EnvPassthrough: []string{"CRONOMICON_SECRET_DB_PASS"},
	}
	env, _, err := buildChildEnv(m, Config{}, environ, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := envNames(env)["CRONOMICON_SECRET_DB_PASS"]; got != "fresh-injected" {
		t.Errorf("prefixed runner-local value won over injection: got %q, want fresh-injected", got)
	}
}

// TestBuildRemoteCommandInjectsSecrets (P1.4 / H1): the injected Secrets values
// reach the run via stdin exports (sorted with the snapshot env) and NEVER touch
// the command line, so they cannot land on the target's process argv / auditd.
func TestBuildRemoteCommandInjectsSecrets(t *testing.T) {
	m := &runnerproto.ManifestResponse{
		Interp:  []string{"bash", "-c"},
		Body:    "echo hi",
		Env:     map[string]string{"STAGE": "prod"},
		Secrets: map[string]string{"CRONOMICON_SECRET_DB_PASS": "s3cr3t", "CRONOMICON_RUN_ID": "run-1"},
	}
	cmd := buildRemoteCommand(m)
	if cmd.Cmd != "bash -s" {
		t.Errorf("Cmd = %q, want 'bash -s'", cmd.Cmd)
	}
	if strings.Contains(cmd.Cmd, "s3cr3t") {
		t.Fatalf("secret leaked into argv: %q", cmd.Cmd)
	}
	for _, want := range []string{
		"export CRONOMICON_RUN_ID='run-1'",
		"export CRONOMICON_SECRET_DB_PASS='s3cr3t'",
		"export STAGE='prod'",
	} {
		if !strings.Contains(cmd.Stdin, want) {
			t.Errorf("stdin missing %q:\n%s", want, cmd.Stdin)
		}
	}
	if !strings.HasSuffix(cmd.Stdin, "echo hi") {
		t.Errorf("body not appended after exports: %q", cmd.Stdin)
	}
}

// TestMergeInjectedEnvPrecedence: injected values win on a key collision.
func TestMergeInjectedEnvPrecedence(t *testing.T) {
	out := mergeInjectedEnv(map[string]string{"K": "from-env", "A": "1"}, map[string]string{"K": "from-injected"})
	if out["K"] != "from-injected" || out["A"] != "1" {
		t.Errorf("mergeInjectedEnv = %v", out)
	}
	// Nil injected returns the original map unchanged (no allocation contract).
	base := map[string]string{"X": "1"}
	if got := mergeInjectedEnv(base, nil); len(got) != 1 || got["X"] != "1" {
		t.Errorf("mergeInjectedEnv(nil) = %v", got)
	}
}

package agent

import (
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

func TestBuildRemoteCommandBash(t *testing.T) {
	m := &runnerproto.ManifestResponse{
		Interp: []string{"bash", "-c"},
		Body:   "echo hi",
		Env:    map[string]string{"FOO": "bar", "BAZ": "q'x"},
	}
	got := buildRemoteCommand(m)
	// H1: env is delivered on stdin, never argv — the command is just `bash -s`.
	if got.Cmd != "bash -s" {
		t.Errorf("bash Cmd = %q, want 'bash -s'", got.Cmd)
	}
	// Sorted exports (single-quoted values) precede the body on stdin.
	wantStdin := "export BAZ='q'\\''x'\nexport FOO='bar'\necho hi"
	if got.Stdin != wantStdin {
		t.Errorf("bash Stdin =\n  %q\nwant\n  %q", got.Stdin, wantStdin)
	}
}

func TestBuildRemoteCommandPowershell(t *testing.T) {
	m := &runnerproto.ManifestResponse{
		Interp: []string{"powershell", "-NonInteractive", "-Command"},
		Body:   "Get-Process",
		Env:    map[string]string{"K": "v"},
	}
	got := buildRemoteCommand(m)
	if got.Cmd != "powershell -NonInteractive -Command -" {
		t.Errorf("powershell Cmd = %q", got.Cmd)
	}
	if got.Stdin != "$env:K = 'v'\nGet-Process" {
		t.Errorf("powershell Stdin = %q", got.Stdin)
	}
}

func TestBuildRemoteCommandPerlNoEnv(t *testing.T) {
	m := &runnerproto.ManifestResponse{
		Interp: []string{"perl"},
		Body:   "print 1",
	}
	got := buildRemoteCommand(m)
	if got.Cmd != "perl -e 'print 1'" || got.Stdin != "" {
		t.Errorf("perl (no env) = %+v", got)
	}
}

// TestEnvInjectionRejectsBadKeys ensures a non-identifier env key can't break
// out of the assignment (matches the SSH executor's hardening).
func TestEnvInjectionRejectsBadKeys(t *testing.T) {
	m := &runnerproto.ManifestResponse{
		Interp: []string{"bash", "-c"},
		Body:   "true",
		Env:    map[string]string{"GOOD": "1", "bad-key": "x", "2bad": "y", "; rm -rf": "z"},
	}
	got := buildRemoteCommand(m)
	if !strings.Contains(got.Stdin, "export GOOD='1'") {
		t.Errorf("valid key dropped: %q", got.Stdin)
	}
	for _, bad := range []string{"bad-key", "2bad", "rm -rf"} {
		if strings.Contains(got.Stdin, bad) || strings.Contains(got.Cmd, bad) {
			t.Errorf("unsafe env key %q leaked: cmd=%q stdin=%q", bad, got.Cmd, got.Stdin)
		}
	}
}

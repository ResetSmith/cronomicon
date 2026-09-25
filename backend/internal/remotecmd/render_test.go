package remotecmd

import (
	"strings"
	"testing"
)

// TestRenderNoEnvPreservesArgvForm asserts the no-injection path is byte-for-byte
// the pre-H1 command with no stdin attached (so a body reading stdin still sees
// EOF).
func TestRenderNoEnvPreservesArgvForm(t *testing.T) {
	cases := []struct {
		interp string
		body   string
		want   string
	}{
		{"bash", "echo hi", "bash -c 'echo hi'"},
		{"python3", "print(1)", "python3 -c 'print(1)'"},
		{"perl", "print 1", "perl -e 'print 1'"},
		{"powershell", "Get-Process", "powershell -NonInteractive -Command 'Get-Process'"},
		{"", "echo hi", "'echo hi'"},
	}
	for _, c := range cases {
		t.Run(c.interp, func(t *testing.T) {
			got := Render(c.interp, c.body, nil)
			if got.Cmd != c.want {
				t.Errorf("Cmd = %q, want %q", got.Cmd, c.want)
			}
			if got.Stdin != "" {
				t.Errorf("Stdin = %q, want empty", got.Stdin)
			}
		})
	}
}

// TestRenderBashEnvOnStdin: env is delivered via stdin exports, NEVER in the
// command line — the core H1 property.
func TestRenderBashEnvOnStdin(t *testing.T) {
	env := map[string]string{"CRONOMICON_SECRET_DB": "s3cr3t", "STAGE": "prod"}
	got := Render("bash", "echo hi", env)
	if got.Cmd != "bash -s" {
		t.Errorf("Cmd = %q, want 'bash -s'", got.Cmd)
	}
	if strings.Contains(got.Cmd, "s3cr3t") {
		t.Fatalf("secret leaked into argv: %q", got.Cmd)
	}
	// Sorted exports precede the body.
	wantStdin := "export CRONOMICON_SECRET_DB='s3cr3t'\nexport STAGE='prod'\necho hi"
	if got.Stdin != wantStdin {
		t.Errorf("Stdin =\n  %q\nwant\n  %q", got.Stdin, wantStdin)
	}
}

// TestRenderSecretNeverInArgv sweeps every interpreter and asserts the secret
// value appears only on stdin.
func TestRenderSecretNeverInArgv(t *testing.T) {
	const secret = "sup3r-s3cr3t-value"
	env := map[string]string{"CRONOMICON_SECRET_X": secret}
	for _, interp := range []string{"bash", "python3", "perl", "powershell", "othersh"} {
		t.Run(interp, func(t *testing.T) {
			got := Render(interp, "run-body", env)
			if strings.Contains(got.Cmd, secret) {
				t.Errorf("[%s] secret in Cmd: %q", interp, got.Cmd)
			}
			if !strings.Contains(got.Stdin, secret) {
				t.Errorf("[%s] secret missing from Stdin: %q", interp, got.Stdin)
			}
			if !strings.HasSuffix(got.Stdin, "run-body") {
				t.Errorf("[%s] body not appended after prelude: %q", interp, got.Stdin)
			}
		})
	}
}

func TestRenderPython(t *testing.T) {
	got := Render("python3", "print(os.environ['K'])", map[string]string{"K": "v"})
	if got.Cmd != "python3 -" {
		t.Errorf("Cmd = %q, want 'python3 -'", got.Cmd)
	}
	want := "import os\nos.environ['K'] = 'v'\nprint(os.environ['K'])"
	if got.Stdin != want {
		t.Errorf("Stdin =\n  %q\nwant\n  %q", got.Stdin, want)
	}
}

// A value with a quote and newline must produce a valid Python literal.
func TestRenderPythonQuoting(t *testing.T) {
	got := Render("python3", "pass", map[string]string{"K": "a'b\nc\\d"})
	want := "import os\nos.environ['K'] = 'a\\'b\\nc\\\\d'\npass"
	if got.Stdin != want {
		t.Errorf("Stdin = %q, want %q", got.Stdin, want)
	}
}

func TestRenderPerl(t *testing.T) {
	got := Render("perl", "print $ENV{K}", map[string]string{"K": "a'b\\c"})
	if got.Cmd != "perl" {
		t.Errorf("Cmd = %q, want 'perl'", got.Cmd)
	}
	want := "$ENV{'K'} = 'a\\'b\\\\c';\nprint $ENV{K}"
	if got.Stdin != want {
		t.Errorf("Stdin = %q, want %q", got.Stdin, want)
	}
}

func TestRenderPowershell(t *testing.T) {
	got := Render("powershell", "Write-Output $env:K", map[string]string{"K": "a'b"})
	if got.Cmd != "powershell -NonInteractive -Command -" {
		t.Errorf("Cmd = %q", got.Cmd)
	}
	want := "$env:K = 'a''b'\nWrite-Output $env:K"
	if got.Stdin != want {
		t.Errorf("Stdin = %q, want %q", got.Stdin, want)
	}
}

// TestRenderDropsUnsafeKeys: non-identifier keys never reach the prelude.
func TestRenderDropsUnsafeKeys(t *testing.T) {
	env := map[string]string{"OK": "1", "bad key": "2", "2bad": "3", "; rm -rf": "4"}
	got := Render("bash", "true", env)
	if got.Stdin != "export OK='1'\ntrue" {
		t.Errorf("Stdin = %q, want only OK exported", got.Stdin)
	}
}

func TestSortedIdentKeys(t *testing.T) {
	keys := SortedIdentKeys(map[string]string{"GOOD_1": "x", "1bad": "x", "has space": "x", "_ok": "x"})
	want := []string{"GOOD_1", "_ok"}
	if len(keys) != len(want) {
		t.Fatalf("SortedIdentKeys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("SortedIdentKeys[%d] = %q, want %q", i, keys[i], want[i])
		}
	}
}

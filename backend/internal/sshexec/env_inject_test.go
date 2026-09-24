package sshexec

import (
	"strings"
	"testing"
)

// TestRemoteCommandEnvInjection covers the H1 stdin hand-off: with env present,
// the command line is the bare stdin-reader form and every value (incl. injected
// secrets) rides on stdin as sorted exports; with no env the pre-H1 argv form is
// preserved byte-for-byte with empty stdin.
func TestRemoteCommandEnvInjection(t *testing.T) {
	cases := []struct {
		name      string
		interp    []string
		body      string
		envJSON   string
		injected  map[string]string
		wantCmd   string
		wantStdin string
	}{
		{
			name:    "bash no env unchanged",
			interp:  []string{"bash", "-c"},
			body:    "echo hi",
			wantCmd: "bash -c 'echo hi'",
		},
		{
			name:      "injected reference merges with env_json (sorted)",
			interp:    []string{"bash", "-c"},
			body:      "echo hi",
			envJSON:   `{"STAGE":"prod"}`,
			injected:  map[string]string{"AMADEUS_SECRET_DB_PASS": "s3cr3t", "AMADEUS_RUN_ID": "run-1"},
			wantCmd:   "bash -s",
			wantStdin: "export AMADEUS_RUN_ID='run-1'\nexport AMADEUS_SECRET_DB_PASS='s3cr3t'\nexport STAGE='prod'\necho hi",
		},
		{
			name:      "injected only, no env_json",
			interp:    []string{"bash", "-c"},
			body:      "echo hi",
			injected:  map[string]string{"AMADEUS_VAR_REGION": "us-east"},
			wantCmd:   "bash -s",
			wantStdin: "export AMADEUS_VAR_REGION='us-east'\necho hi",
		},
		{
			name:      "injected wins over an env_json key collision",
			interp:    []string{"bash", "-c"},
			body:      "echo hi",
			envJSON:   `{"K":"from-env"}`,
			injected:  map[string]string{"K": "from-injected"},
			wantCmd:   "bash -s",
			wantStdin: "export K='from-injected'\necho hi",
		},
		{
			name:      "perl env prelude on stdin",
			interp:    []string{"perl"},
			body:      "print 1",
			envJSON:   `{"K":"v"}`,
			wantCmd:   "perl",
			wantStdin: "$ENV{'K'} = 'v';\nprint 1",
		},
		{
			name:      "powershell env via stdin",
			interp:    []string{"powershell", "-NonInteractive", "-Command"},
			body:      "Write-Output hi",
			envJSON:   `{"STAGE":"prod"}`,
			wantCmd:   "powershell -NonInteractive -Command -",
			wantStdin: "$env:STAGE = 'prod'\nWrite-Output hi",
		},
		{
			name:      "bash value with single quote is POSIX-escaped",
			interp:    []string{"bash", "-c"},
			body:      "echo hi",
			envJSON:   `{"K":"a'b"}`,
			wantCmd:   "bash -s",
			wantStdin: "export K='a'\\''b'\necho hi",
		},
		{
			name:      "unsafe key is dropped",
			interp:    []string{"bash", "-c"},
			body:      "echo hi",
			envJSON:   `{"OK":"1","bad key":"2"}`,
			wantCmd:   "bash -s",
			wantStdin: "export OK='1'\necho hi",
		},
		{
			name:    "invalid json yields no env (argv form)",
			interp:  []string{"bash", "-c"},
			body:    "echo hi",
			envJSON: `not json`,
			wantCmd: "bash -c 'echo hi'",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := remoteCommand(c.interp, c.body, c.envJSON, c.injected)
			if got.Cmd != c.wantCmd {
				t.Errorf("Cmd =\n  %q\nwant\n  %q", got.Cmd, c.wantCmd)
			}
			if got.Stdin != c.wantStdin {
				t.Errorf("Stdin =\n  %q\nwant\n  %q", got.Stdin, c.wantStdin)
			}
			// Invariant: an injected secret value must never appear in the argv.
			if v, ok := c.injected["AMADEUS_SECRET_DB_PASS"]; ok && strings.Contains(got.Cmd, v) {
				t.Errorf("secret leaked into Cmd: %q", got.Cmd)
			}
		})
	}
}

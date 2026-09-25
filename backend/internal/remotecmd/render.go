// Package remotecmd renders a run's interpreter + body + environment into a
// remote SSH invocation. It is the single source of truth shared by the in-app
// SSH executor (internal/sshexec) and the runner agent (internal/agent) so the
// two paths cannot drift (H1).
//
// # Why stdin hand-off (H1 / DEC-1)
//
// The env snapshot may carry INJECTED SECRET VALUES (resolved CRONOMICON_SECRET_*/
// vault-source values, vault-integration.md P1.3). The previous renderer emitted
// them as a "K='value' bash -c '…'" prefix concatenated into the single command
// string passed to session.Run — so the secret sat in the sshd-spawned process's
// argv on the TARGET host: world-readable via /proc/<pid>/cmdline, permanently
// recorded by auditd execve logging (acute on the RHEL8 fleet) and by Windows
// 4688 / Sysmon for PowerShell. The Cronomicon log redactor cannot mitigate a leak
// on the remote process table.
//
// Render delivers the environment on STDIN instead, so no value ever touches
// argv: POSIX interpreters read an export prelude then the body from stdin
// (bash -s / sh -s), and python3/perl/PowerShell read their whole program
// (native env prelude + body) from stdin (python3 - / perl / powershell
// -Command -). No target-side sshd configuration is required.
//
// Residual (predecessor §8): a job body that itself consumes stdin sees EOF
// after the program is read; for bash -s specifically, a `read` in the body will
// consume subsequent body lines. SSH-exec run-types do not read stdin by default,
// so this is accepted. The /dev-free stdin path also assumes the target shell can
// read its program from stdin (universally true for the supported interpreters).
package remotecmd

import (
	"regexp"
	"sort"
	"strings"
)

// Rendered is a remote invocation split into the command line handed to the SSH
// session and the bytes fed to the session's stdin. Env values are delivered
// ONLY through Stdin, never Cmd. Stdin is empty when the run injects no
// environment, in which case the caller must not attach a stdin stream (so a
// body reading stdin sees EOF exactly as before).
type Rendered struct {
	Cmd   string
	Stdin string
}

// envVarNameRe constrains injected/env keys to a POSIX-safe identifier so an
// attacker-controlled key cannot break out of an assignment into the command.
// (Preserved from the pre-H1 renderers — the single filter for both paths.)
var envVarNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Render builds the remote invocation for interp+body, delivering env via stdin.
// interp is the interpreter token (interp[0] server-side): bash / python3 / perl
// / powershell; anything else falls back to a POSIX shell. env is the fully
// merged snapshot (the caller overlays injected values, which win on collision —
// D5). When env has no identifier-valid keys the original argv form is preserved
// byte-for-byte (empty Stdin) so the no-injection path is unchanged.
func Render(interp, body string, env map[string]string) Rendered {
	keys := sortedIdentKeys(env)

	switch interp {
	case "bash":
		if len(keys) == 0 {
			return Rendered{Cmd: "bash -c " + shellQuote(body)}
		}
		return Rendered{Cmd: "bash -s", Stdin: posixExportPrelude(env, keys) + body}

	case "python3":
		if len(keys) == 0 {
			return Rendered{Cmd: "python3 -c " + shellQuote(body)}
		}
		return Rendered{Cmd: "python3 -", Stdin: pythonEnvPrelude(env, keys) + body}

	case "perl":
		if len(keys) == 0 {
			return Rendered{Cmd: "perl -e " + shellQuote(body)}
		}
		// `perl` with no script argument reads its program from stdin.
		return Rendered{Cmd: "perl", Stdin: perlEnvPrelude(env, keys) + body}

	case "powershell":
		if len(keys) == 0 {
			return Rendered{Cmd: "powershell -NonInteractive -Command " + shellQuote(body)}
		}
		// `-Command -` reads the command text from stdin.
		return Rendered{Cmd: "powershell -NonInteractive -Command -", Stdin: powershellEnvPrelude(env, keys) + body}

	default:
		if len(keys) == 0 {
			return Rendered{Cmd: shellQuote(body)}
		}
		return Rendered{Cmd: "sh -s", Stdin: posixExportPrelude(env, keys) + body}
	}
}

// SortedIdentKeys returns the identifier-valid env keys in deterministic order;
// keys that aren't safe POSIX identifiers are dropped. Exported so the local-exec
// env-slice builder (internal/agent) applies the SAME key filter as remote
// injection, from one source.
func SortedIdentKeys(env map[string]string) []string {
	return sortedIdentKeys(env)
}

// sortedIdentKeys returns the identifier-valid env keys in deterministic order;
// keys that aren't safe POSIX identifiers are dropped.
func sortedIdentKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		if envVarNameRe.MatchString(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// posixExportPrelude renders "export K='v'\n" lines for a POSIX shell (bash/sh),
// piped ahead of the body on stdin. Single-quoting preserves literal newlines in
// values; export makes them visible to the body's own subprocesses (matching the
// old "K='v' bash -c" inheritance).
func posixExportPrelude(env map[string]string, keys []string) string {
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("export ")
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(shellQuote(env[k]))
		b.WriteByte('\n')
	}
	return b.String()
}

// pythonEnvPrelude renders an os.environ prelude prepended to the body on stdin.
func pythonEnvPrelude(env map[string]string, keys []string) string {
	var b strings.Builder
	b.WriteString("import os\n")
	for _, k := range keys {
		b.WriteString("os.environ[")
		b.WriteString(pythonQuote(k))
		b.WriteString("] = ")
		b.WriteString(pythonQuote(env[k]))
		b.WriteByte('\n')
	}
	return b.String()
}

// perlEnvPrelude renders a %ENV prelude prepended to the body on stdin.
func perlEnvPrelude(env map[string]string, keys []string) string {
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("$ENV{")
		b.WriteString(perlQuote(k))
		b.WriteString("} = ")
		b.WriteString(perlQuote(env[k]))
		b.WriteString(";\n")
	}
	return b.String()
}

// powershellEnvPrelude renders "$env:K = 'v'" lines prepended to the body on
// stdin (read via `powershell -Command -`).
func powershellEnvPrelude(env map[string]string, keys []string) string {
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("$env:")
		b.WriteString(k)
		b.WriteString(" = ")
		b.WriteString(powershellQuote(env[k]))
		b.WriteByte('\n')
	}
	return b.String()
}

// shellQuote single-quotes a string for a POSIX remote shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// powershellQuote single-quotes a string for PowerShell (a literal ' is doubled).
func powershellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// pythonReplacer escapes a Python single-quoted string literal (order matters:
// backslash first). Newlines/CR/tab become escapes so a value can span the
// assignment safely; other bytes pass through (values are text).
var pythonReplacer = strings.NewReplacer(
	`\`, `\\`,
	`'`, `\'`,
	"\n", `\n`,
	"\r", `\r`,
	"\t", `\t`,
)

// pythonQuote renders s as a Python single-quoted string literal.
func pythonQuote(s string) string {
	return "'" + pythonReplacer.Replace(s) + "'"
}

// perlReplacer escapes a Perl single-quoted string literal, where only the
// backslash and the quote itself are special (newlines are literal).
var perlReplacer = strings.NewReplacer(
	`\`, `\\`,
	`'`, `\'`,
)

// perlQuote renders s as a Perl single-quoted string literal.
func perlQuote(s string) string {
	return "'" + perlReplacer.Replace(s) + "'"
}

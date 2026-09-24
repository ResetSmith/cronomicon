package gitlab

// Body-lint scan (script-upgrade.md, Workstream B). The sync engine reads every
// script body once to compute its content hash; the same bytes are run through
// ScanScriptBody, and the findings are persisted on the scripts row (the
// `warnings` column, migration 210) so the catalog can flag a likely-broken
// script without re-reading the file on every list.
//
// Warnings are advisory: a finding never blocks a sync, never drops a script
// from the resolved map, never flips SyncResult.Status, and never enters
// content_hash (Decision 8). The motivating case is `crlf` — a Windows-authored
// bash script fails over SSH because the executor runs `bash -c '<body>'` and an
// embedded \r breaks remote parsing — but the framework is general.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// Severity levels for a body-lint Warning. They map to the frontend statusTone
// tokens that color the warning chip.
const (
	sevInfo    = "info"
	sevWarning = "warning"
	sevDanger  = "danger"
)

// oversizedBytes is the advisory upper bound on a script body (1 MiB). It mirrors
// the SSH executor's scanLines buffer; there is no hard execution size cap, so
// the rule is purely informational.
const oversizedBytes = 1 << 20

// Warning is one finding from the body-lint scan. It is persisted as an element
// of scripts.warnings (JSON array) and surfaced on the Script API as a
// ScriptWarning with the identical wire shape.
type Warning struct {
	Rule     string `json:"rule"`           // stable id: crlf, non_utf8, no_shebang, shebang_mismatch, oversized
	Severity string `json:"severity"`       // info | warning | danger
	Message  string `json:"message"`        // human text, includes remediation
	Line     int    `json:"line,omitempty"` // 1-based; 0/omitted when not attributable
}

// Rule inspects a script body for one class of problem. A rule MUST behave as a
// pure function: it never mutates shared state and never errors — ScanScriptBody
// recovers from a panicking rule and treats it as "no findings", so a single
// misbehaving rule can never fail a sync.
type Rule func(body []byte, runType string) []Warning

// registry is the ordered set of body-lint rules run by ScanScriptBody. Append a
// new Rule here to extend the scan.
var registry = []Rule{ruleCRLF, ruleNonUTF8, ruleNoShebang, ruleOversized}

// ScanScriptBody runs every registered lint rule over a script body and returns
// the findings, deduped (by rule+line) and stably sorted (by rule, then line).
// It never returns nil and never errors — every rule is advisory.
func ScanScriptBody(body []byte, runType string) []Warning {
	out := []Warning{}
	seen := map[string]bool{}
	for _, rule := range registry {
		for _, w := range runRule(rule, body, runType) {
			key := fmt.Sprintf("%s:%d", w.Rule, w.Line)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, w)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		return out[i].Line < out[j].Line
	})
	return out
}

// runRule invokes one rule, recovering from any panic so a single bad rule can
// never fail a sync.
func runRule(rule Rule, body []byte, runType string) (ws []Warning) {
	defer func() {
		if recover() != nil {
			ws = nil
		}
	}()
	return rule(body, runType)
}

// marshalWarnings serializes scan findings for the scripts.warnings column,
// defaulting to "[]" when empty or on a (practically impossible) marshal error,
// so the column is always a valid non-null JSON array.
func marshalWarnings(ws []Warning) string {
	if len(ws) == 0 {
		return "[]"
	}
	b, err := json.Marshal(ws)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// ruleCRLF flags Windows CRLF (or lone CR) line endings. The SSH executor runs a
// body as `bash -c '<body>'`; an embedded \r rides on the end of a line and breaks
// remote parsing ($'\r': command not found, syntax errors near tokens) — but only
// on lines bash parses as code. A \r on a comment or blank line is inert (bash
// discards the whole comment, \r and all), so when EVERY \r falls on a comment or
// blank line the finding is downgraded to info: worth cleaning up, but it will not
// break execution (the common case is a CRLF-terminated shebang). We warn rather
// than silently strip \r so the operator fixes the source in Git (where it is
// reviewed and hashed) instead of the executor rewriting what runs.
func ruleCRLF(body []byte, _ string) []Warning {
	before, _, ok := bytes.Cut(body, []byte{'\r'})
	if !ok {
		return nil
	}
	severity, message := crlfFinding(body)
	return []Warning{{
		Rule:     "crlf",
		Severity: severity,
		Message:  message,
		Line:     1 + bytes.Count(before, []byte{'\n'}),
	}}
}

// crlfFinding classifies a CRLF body: a warning when at least one \r lands on a
// line bash parses as code, otherwise info (every \r is on a comment or blank
// line, where the \r is inert).
func crlfFinding(body []byte) (severity, message string) {
	for line := range bytes.SplitSeq(body, []byte{'\n'}) {
		if bytes.IndexByte(line, '\r') < 0 {
			continue
		}
		code := bytes.TrimSpace(bytes.ReplaceAll(line, []byte{'\r'}, nil))
		if len(code) > 0 && code[0] != '#' {
			return sevWarning, `CRLF line endings detected — the remote shell will fail (bash -c chokes on \r). Fix at the source in Git: add "*.sh text eol=lf" to .gitattributes, then run "git add --renormalize . && git commit" (or "dos2unix" the file).`
		}
	}
	return sevInfo, `CRLF line endings on comment/blank lines only — cosmetic (bash ignores them). For cleanliness, add "*.sh text eol=lf" to .gitattributes and run "git add --renormalize . && git commit".`
}

// ruleNonUTF8 flags a body that is not valid UTF-8 or contains a NUL byte —
// almost always a binary or mis-encoded file checked in by mistake.
func ruleNonUTF8(body []byte, _ string) []Warning {
	if utf8.Valid(body) && bytes.IndexByte(body, 0) < 0 {
		return nil
	}
	return []Warning{{
		Rule:     "non_utf8",
		Severity: sevWarning,
		Message:  "Non-UTF8 or NUL bytes in script body — likely a binary or mis-encoded file.",
	}}
}

// shebangInterp maps an interpreted run_type to the interpreter base names that
// satisfy its #! line. ansible/terraform are absent: they are not invoked through
// a shebang and are exempt from the shebang rules. `sh` is accepted for a bash
// run_type — a POSIX-sh shebang is harmless when the body runs through bash, a
// superset of sh; `pwsh` is the modern PowerShell binary name; `python` is
// accepted alongside `python3` for a python run_type (the executor invokes
// python3, but a bare `python` shebang is a common, harmless variant).
var shebangInterp = map[string][]string{
	"bash":       {"bash", "sh"},
	"perl":       {"perl"},
	"powershell": {"powershell", "pwsh"},
	"python":     {"python", "python3"},
}

// ruleNoShebang inspects the #! line of a multi-line interpreted script
// (bash/perl/powershell/python) and emits one of two findings:
//
//   - no_shebang (warning) — the first line is not a usable #! shebang at all;
//   - shebang_mismatch (info) — there IS a shebang, but it names a different
//     interpreter than run_type.
//
// Both are hygiene only: the executor runs the body through `bash -c`/`perl -e`
// and ignores the shebang, so neither by itself breaks execution — which is why a
// present-but-mismatched shebang is info, not warning. Inline one-liners (no
// newline) are skipped — a `command` never carries a shebang, so warning on it is
// noise. ansible/terraform are exempt.
func ruleNoShebang(body []byte, runType string) []Warning {
	accepted, checked := shebangInterp[runType]
	if !checked {
		return nil
	}
	if bytes.IndexByte(body, '\n') < 0 {
		return nil // single-line body (inline command) — shebang N/A
	}
	first := firstLine(body)
	interp := ""
	if strings.HasPrefix(first, "#!") {
		interp = shebangInterpreter(first)
	}
	if interp == "" {
		return []Warning{{
			Rule:     "no_shebang",
			Severity: sevWarning,
			Message:  "Script body has no #! shebang for run_type " + runType + ".",
			Line:     1,
		}}
	}
	if slices.Contains(accepted, interp) {
		return nil
	}
	return []Warning{{
		Rule:     "shebang_mismatch",
		Severity: sevInfo,
		Message: fmt.Sprintf("Script shebang names %q but run_type is %s — advisory only; the executor invokes the configured interpreter and ignores the shebang.",
			interp, runType),
		Line: 1,
	}}
}

// shebangInterpreter extracts the interpreter base name from a #! line, resolving
// the `/usr/bin/env <interp>` form (e.g. "#!/usr/bin/env bash" → "bash",
// "#!/bin/sh" → "sh"). Returns "" when no interpreter token is present.
func shebangInterpreter(line string) string {
	fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "#!")))
	if len(fields) == 0 {
		return ""
	}
	base := fields[0]
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if base == "env" && len(fields) > 1 {
		base = fields[1]
	}
	return base
}

// ruleOversized flags a body larger than the advisory cap. Informational: there
// is no hard execution size limit, but an unusually large body is worth a look.
func ruleOversized(body []byte, _ string) []Warning {
	if len(body) <= oversizedBytes {
		return nil
	}
	return []Warning{{
		Rule:     "oversized",
		Severity: sevInfo,
		Message:  fmt.Sprintf("Script body is unusually large (%d bytes) — advisory only; there is no execution size cap.", len(body)),
	}}
}

// firstLine returns the body's first line with any trailing CR stripped, so the
// shebang check is not defeated by a CRLF terminator.
func firstLine(body []byte) string {
	if before, _, ok := bytes.Cut(body, []byte{'\n'}); ok {
		return strings.TrimRight(string(before), "\r")
	}
	return string(body)
}

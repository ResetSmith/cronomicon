package gitlab

// Variable extraction (script-upgrade.md, Workstream B — sibling of the body-lint
// scan in scan.go). The sync engine already reads every script body once to hash
// AND lint it; the same bytes are run through ExtractScriptVariables, and the
// result is persisted on the scripts row (the `variables` column, migration 230)
// so the catalog, the Run dialog, and the Job Composer can all tell an operator
// which environment variables a script consumes — i.e. what they need to define —
// without re-reading the file or re-implementing the parser in the frontend.
//
// This is a HEURISTIC, advisory scan, not a shell parser: it reads $VAR / ${VAR}
// references (and the env-access idioms of perl/powershell), strips comments, and
// subtracts the names the script assigns to itself plus the shell's own ambient
// and positional parameters. It can over- or under-report on adversarial input
// (heredocs, nested quoting), which is acceptable — a wrong hint is never load-
// bearing: nothing here blocks a sync, drops a script, or enters content_hash.

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Variable is one environment variable a script references. It is persisted as an
// element of scripts.variables (JSON array) and surfaced on the Script API as a
// ScriptVariable with the identical wire shape.
//
// Default carries the literal fallback from a ${VAR:-default} / ${VAR:=default}
// form. A non-nil Default (including a pointer to "") means the reference is
// OPTIONAL — the script supplies a value when the env doesn't. A nil Default means
// the variable is REQUIRED: the operator must provide it or the script sees empty.
type Variable struct {
	Name    string  `json:"name"`
	Default *string `json:"default,omitempty"` // non-nil ⇒ optional, with this fallback
	Line    int     `json:"line,omitempty"`    // 1-based line of the first reference
}

// variableExtractor reads a (comment-stripped) body and returns the env variables
// it references. One per script language; the registry below maps run_type to its
// extractor so ansible/terraform can be added later without touching callers.
type variableExtractor func(body []byte) []Variable

// extractors maps a run_type to its variable extractor. bash/perl/powershell/python
// are the shell family handled today; ansible (Jinja {{ }} / lookup('env')) and
// terraform (var.* / TF_VAR_*) use a different variable model and are intentionally
// absent — a missing entry means "not analyzed yet" (the API returns [] and the UI
// says so) rather than emitting misleading shell-shaped findings.
var extractors = map[string]variableExtractor{
	"bash":       extractShellVars,
	"perl":       extractPerlVars,
	"powershell": extractPowershellVars,
	"python":     extractPythonVars,
}

// ExtractScriptVariables runs the extractor for runType over a script body and
// returns the referenced env variables, deduped by name (first reference wins the
// line; the first default seen is kept) and stably sorted by name. It never errors
// and never returns nil. An unknown/empty run_type falls back to shell extraction,
// matching the SSH executor running an un-typed body through `bash -c`.
func ExtractScriptVariables(body []byte, runType string) []Variable {
	ex, ok := extractors[runType]
	if !ok {
		if runType == "ansible" || runType == "terraform" {
			return []Variable{} // a known-but-unsupported language: analyzed = none
		}
		ex = extractShellVars // unknown/empty ⇒ treat as shell (the executor default)
	}
	found := runExtractor(ex, body)

	// Dedupe by name: keep the earliest line, and the first default encountered so a
	// later bare $VAR doesn't erase an earlier ${VAR:-x} optionality (and vice versa
	// a default found later still upgrades a name first seen without one).
	byName := map[string]*Variable{}
	order := []string{}
	for i := range found {
		v := found[i]
		if v.Name == "" {
			continue
		}
		if cur, ok := byName[v.Name]; ok {
			if cur.Default == nil && v.Default != nil {
				cur.Default = v.Default
			}
			if v.Line > 0 && (cur.Line == 0 || v.Line < cur.Line) {
				cur.Line = v.Line
			}
			continue
		}
		cp := v
		byName[v.Name] = &cp
		order = append(order, v.Name)
	}
	out := make([]Variable, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// runExtractor invokes one extractor, recovering from any panic so a single bad
// pattern can never fail a sync (mirrors scan.go's runRule).
func runExtractor(ex variableExtractor, body []byte) (vs []Variable) {
	defer func() {
		if recover() != nil {
			vs = nil
		}
	}()
	return ex(body)
}

// marshalVariables serializes extracted variables for the scripts.variables column,
// defaulting to "[]" when empty or on a (practically impossible) marshal error, so
// the column is always a valid non-null JSON array (mirrors marshalWarnings).
func marshalVariables(vs []Variable) string {
	if len(vs) == 0 {
		return "[]"
	}
	b, err := json.Marshal(vs)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// MarshalPrompts serializes a job's declared prompt variables (UDV1) for the
// jobs.prompts_json column, defaulting to "[]" when empty or on a marshal error so
// the column is always a valid non-null JSON array (mirrors marshalVariables). It
// drops entries with an empty Name (the one field that must be present to bind to
// the env layer); validation/warnings beyond that are advisory and out of scope for
// the persist path (UDV4 — never blocks a sync). Exported so the in-app compose
// path (api.writeComposedJob) shares the exact same serialization as git sync.
func MarshalPrompts(ps []PromptSpec) string {
	clean := make([]PromptSpec, 0, len(ps))
	for _, p := range ps {
		if strings.TrimSpace(p.Name) == "" {
			continue
		}
		clean = append(clean, p)
	}
	if len(clean) == 0 {
		return "[]"
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// Run-input enforcement modes (JR-Q5), persisted as jobs.prompt_enforcement.
const (
	// EnforceWarn is the default and the pre-Phase-3 behavior (UDV4): a declared
	// required run input left empty never blocks the run; the name is recorded under
	// promptWarnings for audit.
	EnforceWarn = "warn"
	// EnforceBlock rejects the run (422 prompt_required) while a declared required
	// input has no value in the effective env. Opt-in per job — it reaches the API,
	// schedules, and workflow steps, which no client-side gate can.
	EnforceBlock = "block"
)

// NormalizePromptEnforcement maps an author-supplied enforcement value onto the two
// legal modes. Anything unrecognized — a typo, an empty field, a value from a newer
// Cronomicon — degrades to "warn".
//
// Failing OPEN is deliberate here, and it is the opposite of how a security control
// would behave. This flag decides whether Cronomicon refuses to run an operator's job;
// resolving an unrecognized value to "block" would let a one-character typo in Git
// silently take a job offline, and the failure would surface as a confusing 422 at
// 3am rather than at author time. The Composer offers only the two valid choices, and
// the DB CHECK constraint is the backstop for anything written directly.
func NormalizePromptEnforcement(v string) string {
	if strings.TrimSpace(strings.ToLower(v)) == EnforceBlock {
		return EnforceBlock
	}
	return EnforceWarn
}

// MarshalEnvPassthrough serializes a job's env_passthrough NAME list (RX.9) for
// the jobs.env_passthrough column, defaulting to "[]" when empty or on a marshal
// error so the column is always a valid non-null JSON array (mirrors
// MarshalPrompts). Entries are trimmed and blanks dropped; these are env-var
// NAMES only, never values (D1).
func MarshalEnvPassthrough(names []string) string {
	return marshalStringList(names)
}

// MarshalRequires serializes a job's requirement-token list (§5/RX.13) for the
// jobs.requires_json column, same shape/defaulting as MarshalEnvPassthrough.
func MarshalRequires(tokens []string) string {
	return marshalStringList(tokens)
}

// NormalizeRunnerTag cleans a single runner-pin tag (RT-2) to the same rules
// tagutil.Normalize applies to a tag SET, so a pin written in YAML, typed into
// the Composer, or sent on a trigger all reduce to the same string.
//
// Case is deliberately PRESERVED rather than folded: matching is case-insensitive
// at the storage layer (runner_tags.tag is COLLATE NOCASE since mig. 1080,
// RT-G9), so lower-casing here would only make the value display differently from
// what the operator typed while changing nothing about what it matches.
//
// Returns "" for anything unusable — blank, over-long, or containing control
// characters. Every caller treats "" as "no pin", which is why this cannot fail:
// on the sync path a bad value must degrade to unpinned rather than take the
// whole repo's sync down (the must_finish_by / become_password precedent), and on
// the API paths the handler validates before calling.
//
// NOTE for callers holding a tri-state (the trigger body's runnerTag): "" from
// this function means "unusable, treat as absent", which is NOT the same as a
// caller's deliberate "" meaning force-unpinned. Decide the tri-state BEFORE
// normalizing — see RT-G8.
func NormalizeRunnerTag(v string) string {
	t := strings.TrimSpace(v)
	if t == "" || utf8.RuneCountInString(t) > runnerTagMaxLen {
		return ""
	}
	for _, r := range t {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return t
}

// runnerTagMaxLen mirrors tagutil.MaxLen. Duplicated rather than imported because
// gitlab must not depend on tagutil (leaf-package rule, CC.15); the drift risk is
// covered by TestNormalizeRunnerTagMatchesTagutilCaps.
const runnerTagMaxLen = 64

// marshalStringList trims + drops blanks and JSON-encodes to a non-null array
// ("[]" when empty or on error).
func marshalStringList(in []string) string {
	clean := make([]string, 0, len(in))
	for _, n := range in {
		if t := strings.TrimSpace(n); t != "" {
			clean = append(clean, t)
		}
	}
	if len(clean) == 0 {
		return "[]"
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// shellSpecial is the set of shell special/positional parameters that are never an
// operator-supplied env var: positionals ($1..), $@ $* $# $? $$ $! $- $0 $_.
var shellSpecial = map[string]bool{
	"@": true, "*": true, "#": true, "?": true, "$": true,
	"!": true, "-": true, "0": true, "_": true,
}

// shellAmbient is the set of standard variables the shell or login environment
// always provides. The operator does not "define" these, so listing them as
// required would be noise — they are excluded from the result.
var shellAmbient = map[string]bool{
	"HOME": true, "PATH": true, "PWD": true, "OLDPWD": true, "SHELL": true,
	"USER": true, "LOGNAME": true, "HOSTNAME": true, "TERM": true,
	"LANG": true, "LANGUAGE": true, "TZ": true, "TMPDIR": true, "TMP": true, "TEMP": true,
	"UID": true, "EUID": true, "PPID": true, "SHLVL": true, "IFS": true,
	"RANDOM": true, "SECONDS": true, "LINENO": true, "COLUMNS": true, "LINES": true,
	"REPLY": true, "OSTYPE": true, "HOSTTYPE": true, "MACHTYPE": true, "PIPESTATUS": true,
	"FUNCNAME": true, "BASH": true, "BASHPID": true, "BASH_VERSION": true,
	"PS1": true, "PS2": true, "PS3": true, "PS4": true,
}

// isAmbientShellVar reports whether name is a shell-provided variable to omit: an
// exact ambient match, an LC_* locale var, or any BASH_*/COMP_* internal.
func isAmbientShellVar(name string) bool {
	if shellAmbient[name] {
		return true
	}
	return strings.HasPrefix(name, "LC_") ||
		strings.HasPrefix(name, "BASH_") ||
		strings.HasPrefix(name, "COMP_")
}

var (
	// shellBraced matches a ${...} reference, capturing the name (group 1) and the
	// remaining expansion text up to the closing brace (group 2). Group 2 is parsed
	// for a :- / := default by the caller. The leading [#!]? eats the ${#VAR} (length)
	// and ${!VAR} (indirect) sigils; group 2 absorbs array indexing (${VAR[i]}),
	// pattern ops (${VAR#p}, ${VAR%s}), and substitution (${VAR//a/b}).
	shellBraced = regexp.MustCompile(`\$\{[#!]?([A-Za-z_][A-Za-z0-9_]*)([^}]*)\}`)
	// shellBare matches a bare $VAR reference (no braces).
	shellBare = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)
	// shellAssign matches a name bound by the script itself at the start of a line —
	// plain NAME=, or via export/declare/local/readonly/typeset. Such names are
	// produced by the script, not consumed from the environment, so they're subtracted.
	shellAssign = regexp.MustCompile(`^\s*(?:export\s+|declare\s+(?:-\S+\s+)*|local\s+|readonly\s+|typeset\s+(?:-\S+\s+)*)?([A-Za-z_][A-Za-z0-9_]*)=`)
	// shellLoopVar matches the loop/select variable in `for NAME in …` / `select NAME in …`.
	shellLoopVar = regexp.MustCompile(`^\s*(?:for|select)\s+([A-Za-z_][A-Za-z0-9_]*)\s+in\b`)
	// shellReadVars matches a `read [-opts] NAME …` line; the names after the options
	// are bound by the read, so they're subtracted too.
	shellRead = regexp.MustCompile(`^\s*read\b(.*)$`)
)

// extractShellVars finds the env variables a bash/sh body consumes: every $VAR /
// ${VAR} reference, minus the names the script assigns itself (NAME=, export,
// declare, local, read, for) and minus the shell's special and ambient parameters.
// Defaults come from the ${VAR:-default} / ${VAR:=default} forms.
func extractShellVars(body []byte) []Variable {
	assigned := map[string]bool{}
	var refs []Variable

	for i, rawLine := range bytes.Split(stripShellComments(body), []byte{'\n'}) {
		line := string(rawLine)
		lineNo := i + 1

		// Collect names the script binds on this line so they can be subtracted.
		if m := shellAssign.FindStringSubmatch(line); m != nil {
			assigned[m[1]] = true
		}
		if m := shellLoopVar.FindStringSubmatch(line); m != nil {
			assigned[m[1]] = true
		}
		if m := shellRead.FindStringSubmatch(line); m != nil {
			for tok := range strings.FieldsSeq(m[1]) {
				if tok == "" || strings.HasPrefix(tok, "-") {
					continue // skip read options (-r, -p PROMPT handled loosely)
				}
				if isName(tok) {
					assigned[tok] = true
				}
			}
		}

		// Braced refs first (so a default attaches to the name), then bare refs.
		for _, m := range shellBraced.FindAllStringSubmatch(line, -1) {
			var def *string
			// Only :- and := supply a usable default value (⇒ optional). :? is an
			// assert-set and :+ a presence test — both leave the var required (nil).
			if rest := m[2]; strings.HasPrefix(rest, ":-") || strings.HasPrefix(rest, ":=") {
				d := rest[2:]
				def = &d
			}
			refs = append(refs, Variable{Name: m[1], Default: def, Line: lineNo})
		}
		for _, m := range shellBare.FindAllStringSubmatch(line, -1) {
			refs = append(refs, Variable{Name: m[1], Line: lineNo})
		}
	}

	out := refs[:0]
	for _, v := range refs {
		if shellSpecial[v.Name] || isAmbientShellVar(v.Name) || assigned[v.Name] {
			continue
		}
		out = append(out, v)
	}
	return out
}

// extractPerlVars finds %ENV access: $ENV{NAME}, $ENV{'NAME'}, $ENV{"NAME"}. Perl
// lexicals ($foo) are not environment input, so only the %ENV idiom is reported.
var perlEnv = regexp.MustCompile(`\$ENV\{\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?\s*\}`)

func extractPerlVars(body []byte) []Variable {
	var out []Variable
	for i, rawLine := range bytes.Split(stripHashComments(body), []byte{'\n'}) {
		for _, m := range perlEnv.FindAllStringSubmatch(string(rawLine), -1) {
			out = append(out, Variable{Name: m[1], Line: i + 1})
		}
	}
	return out
}

// extractPowershellVars finds environment access: $env:NAME and ${env:NAME}
// (PowerShell is case-insensitive on the `env:` drive prefix). Ordinary $vars are
// script-local and not reported.
var psEnv = regexp.MustCompile(`(?i)\$\{?env:([A-Za-z_][A-Za-z0-9_]*)\}?`)

func extractPowershellVars(body []byte) []Variable {
	var out []Variable
	// PowerShell comments are `#` to EOL (block <# #> not handled — advisory).
	for i, rawLine := range bytes.Split(stripHashComments(body), []byte{'\n'}) {
		for _, m := range psEnv.FindAllStringSubmatch(string(rawLine), -1) {
			out = append(out, Variable{Name: m[1], Line: i + 1})
		}
	}
	return out
}

// extractPythonVars finds os.environ / os.getenv access:
//   - os.environ['NAME'] / os.environ["NAME"] — a direct subscript; a missing key
//     raises KeyError, so the reference is required (no default);
//   - os.environ.get('NAME'[, default]) and os.getenv('NAME'[, default]) — a lookup
//     whose optional second argument is a fallback, so a present default marks the
//     reference optional (mirroring the shell ${VAR:-default} form).
//
// The leading `os.` is optional so `from os import environ, getenv` idioms are still
// detected. Ordinary Python locals are not environment input and are not reported.
// Heuristic, not a parser (mirrors the shell/perl extractors).
// The quotes around a name must match ('X' or "X", not 'X"). RE2 has no
// backreference, so the two quote styles are separate alternatives — the name
// lands in whichever group matched. The get/getenv default is captured
// best-effort: it accepts one level of nested parens (func(), os.path.join(a, b))
// so common calls aren't truncated at the first ')'. The name is captured
// WITHOUT requiring the call's closing ')', so a default the heuristic can't
// fully parse (deeper nesting) still yields the variable name — only its
// (advisory) default string is then partial.
var (
	pyEnvSubscript = regexp.MustCompile(`(?:os\.)?environ\[\s*(?:'([A-Za-z_][A-Za-z0-9_]*)'|"([A-Za-z_][A-Za-z0-9_]*)")\s*\]`)
	pyEnvGet       = regexp.MustCompile(`(?:os\.)?(?:environ\.get|getenv)\(\s*(?:'([A-Za-z_][A-Za-z0-9_]*)'|"([A-Za-z_][A-Za-z0-9_]*)")\s*(?:,\s*((?:[^()]|\([^()]*\))*))?`)
)

// pickName returns the first non-empty capture from the two quote-style groups.
func pickName(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func extractPythonVars(body []byte) []Variable {
	var out []Variable
	for i, rawLine := range bytes.Split(stripHashComments(body), []byte{'\n'}) {
		line := string(rawLine)
		lineNo := i + 1
		for _, m := range pyEnvSubscript.FindAllStringSubmatch(line, -1) {
			out = append(out, Variable{Name: pickName(m[1], m[2]), Line: lineNo})
		}
		for _, m := range pyEnvGet.FindAllStringSubmatch(line, -1) {
			v := Variable{Name: pickName(m[1], m[2]), Line: lineNo}
			// A non-empty second argument is a fallback ⇒ optional. Strip matching
			// surrounding quotes so a literal default renders cleanly; a non-literal
			// default (e.g. a call/expression) is kept verbatim.
			if d := strings.TrimSpace(m[3]); d != "" {
				def := d
				if len(def) >= 2 && (def[0] == '\'' || def[0] == '"') && def[len(def)-1] == def[0] {
					def = def[1 : len(def)-1]
				}
				v.Default = &def
			}
			out = append(out, v)
		}
	}
	return out
}

// stripShellComments blanks out `#`-to-EOL comments so a $VAR in a comment or an
// example block isn't reported. A `#` counts as a comment only at line start (after
// whitespace) or when preceded by whitespace — so `URL=x#frag` (no space) and
// `${VAR#prefix}` (parameter expansion) are left intact. Heuristic, not a lexer:
// a `#` inside a quoted string preceded by a space is still treated as a comment.
func stripShellComments(body []byte) []byte {
	var b strings.Builder
	for line := range strings.SplitSeq(string(body), "\n") {
		b.WriteString(truncateAtComment(line))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// stripHashComments is the simpler comment strip for perl/powershell: drop from the
// first `#` that is at line start or preceded by whitespace.
func stripHashComments(body []byte) []byte { return stripShellComments(body) }

// truncateAtComment returns the line up to the first comment `#` (start-of-line or
// whitespace-preceded), or the whole line when there is none.
func truncateAtComment(line string) string {
	for i := 0; i < len(line); i++ {
		if line[i] != '#' {
			continue
		}
		if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
			return line[:i]
		}
	}
	return line
}

// isName reports whether s is a bare shell identifier (no $, brackets, or options).
func isName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		isAlpha := ch == '_' || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z')
		isDigit := ch >= '0' && ch <= '9'
		if i == 0 && !isAlpha {
			return false
		}
		if !isAlpha && !isDigit {
			return false
		}
	}
	return true
}

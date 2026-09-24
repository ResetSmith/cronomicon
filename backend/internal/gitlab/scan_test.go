package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRuleCRLF covers the motivating bug: a \r anywhere flags crlf at the first
// offending line; a clean LF body flags nothing. A \r on a code line is a warning;
// a \r confined to comment/blank lines (e.g. a CRLF shebang) is downgraded to info.
func TestRuleCRLF(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantHit  bool
		wantLine int
		wantSev  string
	}{
		{"clean LF", "#!/bin/bash\necho hi\n", false, 0, ""},
		{"crlf on code line (first)", "echo one\r\necho two\n", true, 1, sevWarning},
		{"crlf on code line (later)", "#!/bin/bash\necho hi\r\n", true, 2, sevWarning},
		{"crlf on shebang/comment only", "#!/bin/bash\r\necho hi\n", true, 1, sevInfo},
		{"crlf on comment+blank only", "# note\r\n\r\necho hi\n", true, 1, sevInfo},
		{"lone CR on code", "a\rb", true, 1, sevWarning},
		{"empty", "", false, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ruleCRLF([]byte(c.body), "bash")
			if !c.wantHit {
				if len(got) != 0 {
					t.Fatalf("want no findings, got %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Rule != "crlf" {
				t.Fatalf("want one crlf finding, got %+v", got)
			}
			if got[0].Line != c.wantLine {
				t.Fatalf("line = %d, want %d", got[0].Line, c.wantLine)
			}
			if got[0].Severity != c.wantSev {
				t.Fatalf("severity = %q, want %q", got[0].Severity, c.wantSev)
			}
		})
	}
}

func TestRuleNonUTF8(t *testing.T) {
	cases := []struct {
		name    string
		body    []byte
		wantHit bool
	}{
		{"plain ascii", []byte("echo hi\n"), false},
		{"valid utf8", []byte("echo é\n"), false},
		{"nul byte", []byte("echo\x00hi"), true},
		{"invalid utf8", []byte{0xff, 0xfe, 0x00}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ruleNonUTF8(c.body, "bash")
			if c.wantHit != (len(got) == 1) {
				t.Fatalf("wantHit=%v, got %+v", c.wantHit, got)
			}
		})
	}
}

func TestRuleNoShebang(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		runType  string
		wantRule string // "" = no finding
		wantSev  string
	}{
		{"bash with matching shebang", "#!/bin/bash\necho hi\n", "bash", "", ""},
		{"bash env-form shebang", "#!/usr/bin/env bash\necho hi\n", "bash", "", ""},
		{"bash accepts sh shebang", "#!/bin/sh\necho hi\n", "bash", "", ""},
		{"bash missing shebang", "echo hi\necho bye\n", "bash", "no_shebang", sevWarning},
		{"bash bare hashbang no interp", "#!\necho hi\n", "bash", "no_shebang", sevWarning},
		{"bash mismatched shebang", "#!/usr/bin/env python3\nprint(1)\n", "bash", "shebang_mismatch", sevInfo},
		{"inline one-liner exempt", "lsblk", "bash", "", ""},
		{"perl matching", "#!/usr/bin/perl\nprint 1;\n", "perl", "", ""},
		{"perl mismatched", "#!/bin/bash\necho hi\n", "perl", "shebang_mismatch", sevInfo},
		{"powershell accepts pwsh", "#!/usr/bin/pwsh\nWrite-Host hi\n", "powershell", "", ""},
		{"python matching python3", "#!/usr/bin/env python3\nprint(1)\n", "python", "", ""},
		{"python accepts bare python", "#!/usr/bin/python\nprint(1)\n", "python", "", ""},
		{"python missing shebang", "print(1)\nprint(2)\n", "python", "no_shebang", sevWarning},
		{"python mismatched", "#!/bin/bash\necho hi\n", "python", "shebang_mismatch", sevInfo},
		{"ansible exempt", "- hosts: all\n  tasks: []\n", "ansible", "", ""},
		{"terraform exempt", "resource x {}\nresource y {}\n", "terraform", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ruleNoShebang([]byte(c.body), c.runType)
			if c.wantRule == "" {
				if len(got) != 0 {
					t.Fatalf("want no finding, got %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Rule != c.wantRule {
				t.Fatalf("want one %s finding, got %+v", c.wantRule, got)
			}
			if got[0].Severity != c.wantSev {
				t.Fatalf("severity = %q, want %q", got[0].Severity, c.wantSev)
			}
		})
	}
}

func TestRuleOversized(t *testing.T) {
	if got := ruleOversized(bytes.Repeat([]byte("a"), oversizedBytes), "bash"); len(got) != 0 {
		t.Fatalf("at cap should not fire, got %+v", got)
	}
	got := ruleOversized(bytes.Repeat([]byte("a"), oversizedBytes+1), "bash")
	if len(got) != 1 || got[0].Rule != "oversized" || got[0].Severity != sevInfo {
		t.Fatalf("over cap should fire as info, got %+v", got)
	}
}

// TestScanScriptBodyCleanIsEmptyNotNil guards the contract relied on by
// marshalWarnings (and the API parseWarnings): a clean body yields a non-nil
// empty slice, which marshals to "[]".
func TestScanScriptBodyCleanIsEmptyNotNil(t *testing.T) {
	got := ScanScriptBody([]byte("#!/bin/bash\necho hi\n"), "bash")
	if got == nil {
		t.Fatal("ScanScriptBody returned nil; must be non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("clean body should have no findings, got %+v", got)
	}
	if marshalWarnings(got) != "[]" {
		t.Fatalf("marshalWarnings(clean) = %q, want []", marshalWarnings(got))
	}
}

// TestScanScriptBodySorted runs a body that trips multiple rules and asserts the
// output is stably sorted by rule then line (so persisted order is deterministic).
func TestScanScriptBodySorted(t *testing.T) {
	// Missing shebang (no_shebang) + CRLF (crlf), both on a bash body.
	got := ScanScriptBody([]byte("echo one\r\necho two\n"), "bash")
	if len(got) != 2 {
		t.Fatalf("want 2 findings (crlf + no_shebang), got %+v", got)
	}
	if got[0].Rule != "crlf" || got[1].Rule != "no_shebang" {
		t.Fatalf("not sorted by rule: %q then %q", got[0].Rule, got[1].Rule)
	}
}

// TestMarshalWarningsRoundTrip confirms the persisted JSON is the wire shape the
// API decodes back (rule/severity/message/line).
func TestMarshalWarningsRoundTrip(t *testing.T) {
	s := marshalWarnings([]Warning{{Rule: "crlf", Severity: sevWarning, Message: "m", Line: 2}})
	want := `[{"rule":"crlf","severity":"warning","message":"m","line":2}]`
	if s != want {
		t.Fatalf("marshalWarnings = %s, want %s", s, want)
	}
}

// TestResolveScripts_CRLFWarningRoundTrip is the end-to-end guard: a CRLF
// scriptPath synced from Git lands a crlf warning, the warning persists on the
// scripts row, and the body-lint scan does NOT perturb content_hash (it is
// computed from the same bytes hashBodyAt would read — warnings never enter the
// hash, Decision 8).
func TestResolveScripts_CRLFWarningRoundTrip(t *testing.T) {
	pool := mustOpenDB(t)
	clone := t.TempDir()
	if err := os.MkdirAll(filepath.Join(clone, "scripts"), 0o750); err != nil {
		t.Fatal(err)
	}
	// Matching shebang (so only crlf fires), CRLF terminators.
	body := "#!/bin/bash\r\necho hi\r\n"
	if err := os.WriteFile(filepath.Join(clone, "scripts", "crlf.sh"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := &Service{db: pool, cloneDir: clone}

	sc := ScriptYAML{}
	sc.APIVersion = requiredAPIVersion
	sc.Kind = "Script"
	sc.Metadata.Name = "crlf-script"
	sc.Spec.RunType = "bash"
	sc.Spec.ScriptPath = "scripts/crlf.sh"

	resolved, errs := svc.resolveScripts([]ScriptYAML{sc})
	if len(errs) != 0 {
		t.Fatalf("resolveScripts errs: %v", errs)
	}
	rs, ok := resolved["crlf-script"]
	if !ok {
		t.Fatal("crlf-script not resolved")
	}

	var ws []Warning
	if err := json.Unmarshal([]byte(rs.warnings), &ws); err != nil {
		t.Fatalf("warnings not valid JSON (%q): %v", rs.warnings, err)
	}
	if len(ws) != 1 || ws[0].Rule != "crlf" {
		t.Fatalf("want exactly one crlf warning, got %s", rs.warnings)
	}

	// The scan must not change the hash: it equals what the pre-existing hasher
	// produces for the identical file bytes.
	wantHash, err := hashBodyAt(clone, "", "", "scripts/crlf.sh")
	if err != nil {
		t.Fatal(err)
	}
	if rs.contentHash != wantHash {
		t.Fatalf("content_hash = %s, want %s (scan must not perturb the hash)", rs.contentHash, wantHash)
	}

	// Persist and read back: warnings round-trip, hash intact.
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	if err := svc.upsertScripts(ctx, tx, resolved, now, "sha"); err != nil {
		t.Fatalf("upsertScripts: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var gotWarnings, gotHash string
	if err := pool.QueryRow(`SELECT warnings, content_hash FROM scripts WHERE name='crlf-script'`).
		Scan(&gotWarnings, &gotHash); err != nil {
		t.Fatal(err)
	}
	if gotWarnings != rs.warnings {
		t.Fatalf("persisted warnings = %s, want %s", gotWarnings, rs.warnings)
	}
	if gotHash != wantHash {
		t.Fatalf("persisted content_hash = %s, want %s", gotHash, wantHash)
	}
}

// TestResolveScripts_VariablesRoundTrip guards the variable-extraction path the
// same way: a scriptPath synced from Git has its referenced env vars extracted,
// and they persist on the scripts.variables column (migration 230) unchanged.
func TestResolveScripts_VariablesRoundTrip(t *testing.T) {
	pool := mustOpenDB(t)
	clone := t.TempDir()
	if err := os.MkdirAll(filepath.Join(clone, "scripts"), 0o750); err != nil {
		t.Fatal(err)
	}
	// References $TARGET_HOST (required) and ${RETRIES:-3} (optional); LOCAL is
	// assigned in-body, $HOME is ambient — both must be excluded.
	body := "#!/bin/bash\nLOCAL=1\necho \"$TARGET_HOST ${RETRIES:-3} $HOME $LOCAL\"\n"
	if err := os.WriteFile(filepath.Join(clone, "scripts", "deploy.sh"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := &Service{db: pool, cloneDir: clone}

	sc := ScriptYAML{}
	sc.APIVersion = requiredAPIVersion
	sc.Kind = "Script"
	sc.Metadata.Name = "deploy"
	sc.Spec.RunType = "bash"
	sc.Spec.ScriptPath = "scripts/deploy.sh"

	resolved, errs := svc.resolveScripts([]ScriptYAML{sc})
	if len(errs) != 0 {
		t.Fatalf("resolveScripts errs: %v", errs)
	}
	rs := resolved["deploy"]
	var vs []Variable
	if err := json.Unmarshal([]byte(rs.variables), &vs); err != nil {
		t.Fatalf("variables not valid JSON (%q): %v", rs.variables, err)
	}
	if !eqStrs(varNames(vs), []string{"RETRIES", "TARGET_HOST"}) {
		t.Fatalf("extracted = %v, want [RETRIES TARGET_HOST]", varNames(vs))
	}

	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	if err := svc.upsertScripts(ctx, tx, resolved, now, "sha"); err != nil {
		t.Fatalf("upsertScripts: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var gotVariables string
	if err := pool.QueryRow(`SELECT variables FROM scripts WHERE name='deploy'`).Scan(&gotVariables); err != nil {
		t.Fatal(err)
	}
	if gotVariables != rs.variables {
		t.Fatalf("persisted variables = %s, want %s", gotVariables, rs.variables)
	}
}

package gitlab

import (
	"encoding/json"
	"testing"
)

// names returns the extracted variable names in order, for compact assertions.
func varNames(vs []Variable) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Name
	}
	return out
}

// find returns the variable with the given name, or nil.
func find(vs []Variable, name string) *Variable {
	for i := range vs {
		if vs[i].Name == name {
			return &vs[i]
		}
	}
	return nil
}

func eqStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestExtractShellVars(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string // sorted names expected
	}{
		{"bare and braced", "echo $FOO ${BAR}", []string{"BAR", "FOO"}},
		{"default subtracted from required", "echo ${HOST:-localhost}", []string{"HOST"}},
		{
			"locally assigned are excluded",
			"NAME=value\nexport TOKEN=abc\necho $NAME $TOKEN $REAL",
			[]string{"REAL"},
		},
		{
			"special and positional excluded",
			"echo $1 $@ $? $$ $! $# $0 $FOO",
			[]string{"FOO"},
		},
		{
			"ambient shell vars excluded",
			"cd $HOME && echo $PATH $USER $MY_VAR",
			[]string{"MY_VAR"},
		},
		{
			"loop and read vars excluded",
			"for item in $LIST; do echo $item; done\nread -r ANSWER\necho $ANSWER",
			[]string{"LIST"},
		},
		{
			"references in comments are ignored",
			"# example: echo $SECRET_IN_COMMENT\necho $REAL_ONE",
			[]string{"REAL_ONE"},
		},
		{
			"parameter-expansion # is not a comment",
			"echo ${FILE#prefix}",
			[]string{"FILE"},
		},
		{"LC_ and BASH_ internals excluded", "echo $LC_ALL $BASH_SOURCE $APP_ENV", []string{"APP_ENV"}},
		{"dedup keeps single entry", "echo $X $X ${X}", []string{"X"}},
		{"empty body", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := varNames(ExtractScriptVariables([]byte(c.body), "bash"))
			if len(c.want) == 0 && len(got) == 0 {
				return
			}
			if !eqStrs(got, c.want) {
				t.Fatalf("names = %v, want %v", got, c.want)
			}
		})
	}
}

// TestExtractShellVarsDefaults verifies the optional/required distinction: a
// ${VAR:-x} / ${VAR:=x} reference is optional (Default non-nil), a bare reference
// or :? assertion is required (Default nil).
func TestExtractShellVarsDefaults(t *testing.T) {
	vs := ExtractScriptVariables([]byte("echo ${OPT:-fallback} ${SET:=x} ${REQ} ${ASSERT:?must be set}"), "bash")
	opt := find(vs, "OPT")
	if opt == nil || opt.Default == nil || *opt.Default != "fallback" {
		t.Fatalf("OPT should be optional with default 'fallback', got %+v", opt)
	}
	if set := find(vs, "SET"); set == nil || set.Default == nil || *set.Default != "x" {
		t.Fatalf("SET should be optional with default 'x', got %+v", set)
	}
	if req := find(vs, "REQ"); req == nil || req.Default != nil {
		t.Fatalf("REQ should be required (nil default), got %+v", req)
	}
	if as := find(vs, "ASSERT"); as == nil || as.Default != nil {
		t.Fatalf("ASSERT (:?) should be required (nil default), got %+v", as)
	}
}

// TestExtractShellVarsDefaultUpgrade verifies that a name seen once bare and once
// with a default ends up optional (the default is not lost to dedup ordering).
func TestExtractShellVarsDefaultUpgrade(t *testing.T) {
	vs := ExtractScriptVariables([]byte("echo $PORT\nPORT2=${PORT:-8080}"), "bash")
	// PORT2 is locally assigned, so only PORT survives — and it must be optional.
	if !eqStrs(varNames(vs), []string{"PORT"}) {
		t.Fatalf("names = %v, want [PORT]", varNames(vs))
	}
	if vs[0].Default == nil || *vs[0].Default != "8080" {
		t.Fatalf("PORT should carry default 8080, got %+v", vs[0])
	}
}

func TestExtractPerlVars(t *testing.T) {
	body := `my $local = 1;
my $h = $ENV{HOME};
my $t = $ENV{'API_TOKEN'};
my $u = $ENV{"DB_URL"};
# $ENV{IGNORED_IN_COMMENT}
print $local;`
	got := varNames(ExtractScriptVariables([]byte(body), "perl"))
	// HOME is ambient but perl extraction reports %ENV access verbatim (it is not
	// filtered through the shell ambient set) — the operator explicitly read it.
	want := []string{"API_TOKEN", "DB_URL", "HOME"}
	if !eqStrs(got, want) {
		t.Fatalf("perl names = %v, want %v", got, want)
	}
}

func TestExtractPowershellVars(t *testing.T) {
	body := `$local = "x"
Write-Host $env:USERNAME
$t = ${env:API_TOKEN}
Write-Host $Env:Path  # case-insensitive prefix
# $env:IGNORED`
	got := varNames(ExtractScriptVariables([]byte(body), "powershell"))
	want := []string{"API_TOKEN", "Path", "USERNAME"}
	if !eqStrs(got, want) {
		t.Fatalf("powershell names = %v, want %v", got, want)
	}
}

func TestExtractPythonVars(t *testing.T) {
	body := `import os
local = 1
h = os.environ['HOME']
token = os.environ["API_TOKEN"]
url = os.getenv('DB_URL')
region = os.environ.get('AWS_REGION', 'us-east-1')
bare = getenv("BARE_VAR")
# os.environ['IGNORED_IN_COMMENT']
print(local)`
	got := varNames(ExtractScriptVariables([]byte(body), "python"))
	// os.environ / os.getenv access is reported verbatim (HOME included — the
	// operator explicitly read it, as with the perl %ENV extractor). The bare
	// getenv (from `from os import getenv`) is caught via the optional os. prefix.
	want := []string{"API_TOKEN", "AWS_REGION", "BARE_VAR", "DB_URL", "HOME"}
	if !eqStrs(got, want) {
		t.Fatalf("python names = %v, want %v", got, want)
	}
}

// TestExtractPythonVarsDefaults confirms a getenv/.get fallback marks the variable
// optional (Default set), while a bare os.environ[...] subscript stays required.
func TestExtractPythonVarsDefaults(t *testing.T) {
	body := `import os
region = os.environ.get('AWS_REGION', 'us-east-1')
port = os.getenv('PORT', '8080')
required = os.environ['MUST_SET']`
	got := ExtractScriptVariables([]byte(body), "python")
	byName := map[string]*Variable{}
	for i := range got {
		byName[got[i].Name] = &got[i]
	}
	if v := byName["AWS_REGION"]; v == nil || v.Default == nil || *v.Default != "us-east-1" {
		t.Fatalf("AWS_REGION should be optional with default us-east-1, got %+v", v)
	}
	if v := byName["PORT"]; v == nil || v.Default == nil || *v.Default != "8080" {
		t.Fatalf("PORT should be optional with default 8080, got %+v", v)
	}
	if v := byName["MUST_SET"]; v == nil || v.Default != nil {
		t.Fatalf("MUST_SET should be required (no default), got %+v", v)
	}
}

// TestExtractPythonVarsEdgeCases covers defaults containing nested parentheses
// (the default must not truncate at the first ')') and mismatched quotes (which
// are a Python syntax error and must NOT be extracted).
func TestExtractPythonVarsEdgeCases(t *testing.T) {
	body := `import os
timeout = os.getenv('TIMEOUT', str(30))
root = os.environ.get('ROOT_DIR', os.path.join('/srv', 'app'))
deep = os.getenv('DEEP', wrap(inner()))
bad = os.environ['MISMATCHED"]
ok = os.getenv("CLEAN")`
	got := ExtractScriptVariables([]byte(body), "python")
	byName := map[string]*Variable{}
	for i := range got {
		byName[got[i].Name] = &got[i]
	}
	// One level of nested parens in the default is kept whole (not truncated at ')').
	if v := byName["TIMEOUT"]; v == nil || v.Default == nil || *v.Default != "str(30)" {
		t.Fatalf("TIMEOUT default should keep its parens, got %+v", v)
	}
	if v := byName["ROOT_DIR"]; v == nil || v.Default == nil || *v.Default != "os.path.join('/srv', 'app')" {
		t.Fatalf("ROOT_DIR default should keep its nested parens, got %+v", v)
	}
	// Deeper nesting than the heuristic handles: the NAME is still captured (optional,
	// since a default is present) even though its default string is only partial.
	if v := byName["DEEP"]; v == nil || v.Default == nil {
		t.Fatalf("DEEP must still be extracted (name preserved) with a default, got %+v", v)
	}
	// Mismatched quotes are a syntax error and must not be extracted.
	if _, ok := byName["MISMATCHED"]; ok {
		t.Errorf("MISMATCHED (mismatched quotes) should not be extracted")
	}
	if _, ok := byName["CLEAN"]; !ok {
		t.Errorf("CLEAN should be extracted")
	}
}

// TestExtractUnsupportedLanguages confirms ansible/terraform return an empty (but
// non-nil) slice today — "analyzed, found none" rather than shell-shaped noise.
func TestExtractUnsupportedLanguages(t *testing.T) {
	for _, rt := range []string{"ansible", "terraform"} {
		got := ExtractScriptVariables([]byte("{{ some_var }} ${var.thing} $SHELL_LOOKING"), rt)
		if got == nil {
			t.Fatalf("%s: result must be non-nil", rt)
		}
		if len(got) != 0 {
			t.Fatalf("%s: want no findings (not analyzed yet), got %v", rt, varNames(got))
		}
	}
}

// TestExtractUnknownRunTypeFallsBackToShell confirms an empty/unknown run_type is
// scanned as shell, matching the SSH executor running an un-typed body via bash.
func TestExtractUnknownRunTypeFallsBackToShell(t *testing.T) {
	got := varNames(ExtractScriptVariables([]byte("echo $FOO"), ""))
	if !eqStrs(got, []string{"FOO"}) {
		t.Fatalf("unknown run_type names = %v, want [FOO]", got)
	}
}

// TestMarshalVariables confirms the column serialization is always a valid non-null
// JSON array and round-trips, mirroring marshalWarnings.
func TestMarshalVariables(t *testing.T) {
	if got := marshalVariables(nil); got != "[]" {
		t.Fatalf("nil ⇒ %q, want []", got)
	}
	def := "d"
	raw := marshalVariables([]Variable{{Name: "A", Default: &def, Line: 3}, {Name: "B"}})
	var back []Variable
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if len(back) != 2 || back[0].Name != "A" || back[0].Default == nil || *back[0].Default != "d" {
		t.Fatalf("round-trip mismatch: %s", raw)
	}
	// B has no default and zero line ⇒ both omitempty fields drop from the JSON.
	if back[1].Default != nil || back[1].Line != 0 {
		t.Fatalf("B should be bare required var, got %+v", back[1])
	}
}

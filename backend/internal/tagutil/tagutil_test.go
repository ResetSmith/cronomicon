package tagutil

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := map[string][]string{
		``:          {},
		`[]`:        {},
		`not json`:  {},
		`null`:      {},
		`["a","b"]`: {"a", "b"},
		`["only"]`:  {"only"},
	}
	for raw, want := range cases {
		got := Parse(raw)
		if got == nil {
			t.Errorf("Parse(%q) = nil, want non-nil slice", raw)
		}
		if len(got) != len(want) {
			t.Errorf("Parse(%q) = %v, want %v", raw, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("Parse(%q)[%d] = %q, want %q", raw, i, got[i], want[i])
			}
		}
	}
}

func TestNormalize(t *testing.T) {
	// Trim, drop blanks, case-insensitive de-dup (first casing wins), order kept.
	got, err := Normalize([]string{"  Prod ", "prod", "", "Ops", "ops "})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(got) != 2 || got[0] != "Prod" || got[1] != "Ops" {
		t.Errorf("Normalize dedup/trim = %v, want [Prod Ops]", got)
	}

	// A cleared set is a non-nil empty slice (marshals to []).
	if out, err := Normalize([]string{" ", ""}); err != nil || out == nil || len(out) != 0 {
		t.Errorf("Normalize(blanks) = %v, %v; want empty non-nil slice", out, err)
	}

	// Caps + control-char rejection are errors.
	if _, err := Normalize([]string{strings.Repeat("x", MaxLen+1)}); err == nil {
		t.Error("over-long tag: want error")
	}
	if _, err := Normalize([]string{"bad\ttab"}); err == nil {
		t.Error("control character: want error")
	}
	many := make([]string, MaxCount+1)
	for i := range many {
		many[i] = "t" + strings.Repeat("x", i%3) + string(rune('a'+i))
	}
	if _, err := Normalize(many); err == nil {
		t.Error("too many tags: want error")
	}
}

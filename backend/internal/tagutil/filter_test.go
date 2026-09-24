package tagutil

import (
	"net/url"
	"testing"
)

func TestParseQuery(t *testing.T) {
	// Repeated ?tag= plus comma-joined ?tags= are unioned and de-duped.
	q := url.Values{
		"tag":      {"prod", "Ops"},
		"tags":     {"prod,web", " "},
		"tagMatch": {"all"},
	}
	f := ParseQuery(q)
	if !f.MatchAll {
		t.Error("tagMatch=all should set MatchAll")
	}
	want := []string{"prod", "Ops", "web"} // prod de-duped, blank dropped, order preserved
	if len(f.Tags) != len(want) {
		t.Fatalf("ParseQuery tags = %v, want %v", f.Tags, want)
	}
	for i := range want {
		if f.Tags[i] != want[i] {
			t.Errorf("ParseQuery tags[%d] = %q, want %q", i, f.Tags[i], want[i])
		}
	}

	// Default (no tagMatch) is OR; no tags ⇒ empty.
	if ParseQuery(url.Values{"tag": {"a"}}).MatchAll {
		t.Error("default match should be any (OR)")
	}
	if !ParseQuery(url.Values{}).Empty() {
		t.Error("no tags ⇒ Empty()")
	}
}

func TestSQLFilter(t *testing.T) {
	if frag, args := (Filter{}).SQLFilter("j.tags"); frag != "" || args != nil {
		t.Errorf("empty filter = %q,%v; want \"\",nil", frag, args)
	}

	// Exact-element matching, not substring — one EXISTS/json_each clause per tag.
	frag, args := Filter{Tags: []string{"prod", "web"}}.SQLFilter("j.tags")
	if len(args) != 2 || args[0] != "prod" || args[1] != "web" {
		t.Errorf("args = %v, want [prod web]", args)
	}
	for _, want := range []string{"json_each(j.tags)", "value = ?", " OR "} {
		if !contains(frag, want) {
			t.Errorf("SQLFilter fragment %q missing %q", frag, want)
		}
	}
	// AND when MatchAll.
	if frag, _ := (Filter{Tags: []string{"a", "b"}, MatchAll: true}).SQLFilter("t.tags"); !contains(frag, " AND ") {
		t.Errorf("MatchAll fragment %q should use AND", frag)
	}
}

func TestMatch(t *testing.T) {
	row := []string{"prod", "web"}
	cases := []struct {
		f    Filter
		want bool
	}{
		{Filter{}, true},                                             // empty ⇒ all
		{Filter{Tags: []string{"prod"}}, true},                       // any-of hit
		{Filter{Tags: []string{"production"}}, false},                // exact, not substring
		{Filter{Tags: []string{"db"}}, false},                        // any-of miss
		{Filter{Tags: []string{"prod", "db"}}, true},                 // OR: one hit
		{Filter{Tags: []string{"prod", "db"}, MatchAll: true}, false}, // AND: missing db
		{Filter{Tags: []string{"prod", "web"}, MatchAll: true}, true}, // AND: both present
	}
	for i, c := range cases {
		if got := c.f.Match(row); got != c.want {
			t.Errorf("case %d Match(%v) = %v, want %v", i, c.f, got, c.want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

package watchspec

import "testing"

// ET-D — the watch vocabulary.
//
// The validation rules exist for things that would FAIL SILENTLY rather than
// loudly: a relative path resolves against whatever directory the agent happened
// to start in, and a directory glob would match forever.

func TestValidateRejectsWhatWouldFailSilently(t *testing.T) {
	for _, tc := range []struct {
		name  string
		watch Watch
		want  bool // want an error
	}{
		{"absolute glob", Watch{Path: "/srv/incoming/*.csv"}, false},
		{"exact file", Watch{Path: "/srv/incoming/daily.csv"}, false},
		{"relative", Watch{Path: "incoming/*.csv"}, true},
		{"traversal", Watch{Path: "/srv/../etc/*"}, true},
		{"directory", Watch{Path: "/srv/incoming/"}, true},
		{"empty", Watch{Path: "  "}, true},
		{"bad glob", Watch{Path: "/srv/[unclosed"}, true},
		{"negative stability", Watch{Path: "/srv/a/*", StableSeconds: -1}, true},
		{"absurd stability", Watch{Path: "/srv/a/*", StableSeconds: 99999}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := Validate([]Watch{tc.watch})
			if got := len(errs) > 0; got != tc.want {
				t.Errorf("errors = %v (%v), want %v", got, errs, tc.want)
			}
		})
	}
}

func TestValidateRejectsDuplicatesAndOverlongLists(t *testing.T) {
	if errs := Validate([]Watch{{Path: "/a/*"}, {Path: "/a/*"}}); len(errs) == 0 {
		t.Error("a duplicate path was accepted; it would double-report every arrival")
	}
	many := make([]Watch, MaxWatchesPerJob+1)
	for i := range many {
		many[i] = Watch{Path: "/a/" + string(rune('a'+i)) + "/*"}
	}
	if errs := Validate(many); len(errs) == 0 {
		t.Errorf("more than %d watches on one job was accepted", MaxWatchesPerJob)
	}
}

func TestNormalizeAppliesTheStabilityDefault(t *testing.T) {
	got := Normalize([]Watch{{Path: " /srv/a/* "}, {Path: "/srv/b/*", StableSeconds: 30}, {Path: "  "}})
	if len(got) != 2 {
		t.Fatalf("normalized to %d watches, want 2 (the blank one is dropped)", len(got))
	}
	if got[0].Path != "/srv/a/*" {
		t.Errorf("path = %q, want it trimmed", got[0].Path)
	}
	if got[0].StableSeconds != DefaultStableSeconds {
		t.Errorf("stableSeconds = %d, want the default %d — a zero window fires on half-written files",
			got[0].StableSeconds, DefaultStableSeconds)
	}
	if got[1].StableSeconds != 30 {
		t.Errorf("an explicit window was overwritten: %d", got[1].StableSeconds)
	}
}

// The allowlist is the whole security posture: the SERVER picks the globs, so
// the agent is the thing that says no.
func TestPathAllowed(t *testing.T) {
	roots := []string{"/srv/incoming", "/var/spool/amadeus"}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/srv/incoming/a.csv", true},
		{"/srv/incoming/nested/deep/a.csv", true},
		{"/srv/incoming", true},
		{"/var/spool/amadeus/x", true},
		{"/etc/shadow", false},
		{"/srv/incoming-other/a.csv", false}, // prefix-but-not-child
		{"/srv/a.csv", false},
	} {
		if got := PathAllowed(tc.path, roots); got != tc.want {
			t.Errorf("PathAllowed(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
	// The empty allowlist permits NOTHING, matching the scope-grant rule that
	// empty means zero access rather than everything.
	if PathAllowed("/srv/incoming/a.csv", nil) {
		t.Error("an empty allowlist permitted a path; empty must mean nothing, not everything")
	}
}

func TestParseAndMarshalRoundTrip(t *testing.T) {
	for _, empty := range []string{"", "  ", "null", "[]"} {
		if ws, err := Parse(empty); err != nil || len(ws) != 0 {
			t.Errorf("Parse(%q) = %v, %v; want no watches and no error", empty, ws, err)
		}
	}
	if Marshal(nil) != nil {
		t.Error("an empty watch list should store as NULL, not \"[]\" — one value for one meaning")
	}
	raw := Marshal([]Watch{{Path: "/a/*", StableSeconds: 5}})
	back, err := Parse(raw.(string))
	if err != nil || len(back) != 1 || back[0].Path != "/a/*" || back[0].StableSeconds != 5 {
		t.Errorf("round trip lost data: %v (%v)", back, err)
	}
}

package inventory

import "slices"

import "testing"

func contains(xs []string, want string) bool {
	return slices.Contains(xs, want)
}

func TestParseProjection_SupportedSubset(t *testing.T) {
	content := `# production inventory
[web]
web1 ansible_host=10.0.0.1 ansible_user=deploy
web2 ansible_user="deep space"   ; trailing comment

[db]
db1

[prod:children]
web
db

[web:vars]
ansible_port=22
`
	p := ParseProjection(content)
	if p.PreviewUnavailable {
		t.Fatalf("unexpected degrade: %s (line %d)", p.PreviewReason, p.PreviewLine)
	}
	// Hosts is the NAIVE membership (kept identical to the old parseInventoryHosts
	// for scope_hosts compatibility — it also over-collects [:children]/[:vars]
	// body lines; that's pre-existing and not the projection's concern). Just
	// assert the real hosts are present.
	for _, h := range []string{"web1", "web2", "db1"} {
		if !contains(p.Hosts, h) {
			t.Errorf("Hosts %v missing %q", p.Hosts, h)
		}
	}
	// Group membership.
	if got := p.Groups["web"].Hosts; len(got) != 2 || got[0] != "web1" || got[1] != "web2" {
		t.Errorf("web hosts = %v, want [web1 web2]", got)
	}
	if got := p.Groups["db"].Hosts; len(got) != 1 || got[0] != "db1" {
		t.Errorf("db hosts = %v, want [db1]", got)
	}
	// Inline host vars (quoted value with space survives).
	if p.HostVars["web1"]["ansible_host"] != "10.0.0.1" || p.HostVars["web1"]["ansible_user"] != "deploy" {
		t.Errorf("web1 host vars = %v", p.HostVars["web1"])
	}
	if p.HostVars["web2"]["ansible_user"] != "deep space" {
		t.Errorf("web2 ansible_user = %q, want 'deep space'", p.HostVars["web2"]["ansible_user"])
	}
	// Children → child groups exist.
	if got := p.Groups["prod"].Children; len(got) != 2 || got[0] != "web" || got[1] != "db" {
		t.Errorf("prod children = %v, want [web db]", got)
	}
	if _, ok := p.Groups["web"]; !ok {
		t.Errorf("child group 'web' should be ensured as a group")
	}
	// Group vars.
	if p.Groups["web"].Vars["ansible_port"] != "22" {
		t.Errorf("web group vars = %v", p.Groups["web"].Vars)
	}
}

func TestParseProjection_DegradeLoudly(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantLine int
	}{
		{"host range", "[web]\nweb1\nweb[01:50]\n", 3},
		{"bare flag after host", "[web]\nweb1 notakeyvalue\n", 2},
		{"unknown section suffix", "[web:foo]\nweb1\n", 1},
		{"metachar group name", "[web,prod]\nweb1\n", 1},
		{"metachar host name", "[web]\nweb*1\n", 2},
		{"malformed header", "[web\nweb1\n", 1},
		{"unterminated quote swallows bare flag", "[web]\nweb1 k=\"a b notakeyvalue\n", 2},
		{"unterminated quote in host var", "[web]\nweb1 ansible_user=\"deploy\n", 2},
		{"multi-token group vars", "[web:vars]\na=1 b=2\n", 2},
		{"bare token in group vars", "[web:vars]\njustaflag\n", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ParseProjection(tc.content)
			if !p.PreviewUnavailable {
				t.Fatalf("expected degrade, got a clean parse")
			}
			if p.PreviewLine != tc.wantLine {
				t.Errorf("degrade line = %d, want %d (reason: %s)", p.PreviewLine, tc.wantLine, p.PreviewReason)
			}
			// A degraded projection must NOT persist a half-parsed tree.
			if len(p.Groups) != 0 || len(p.HostVars) != 0 {
				t.Errorf("degraded projection must discard the tree, got groups=%v hostVars=%v", p.Groups, p.HostVars)
			}
			// Hosts (naive) are still populated regardless of degrade.
			if len(p.Hosts) == 0 {
				t.Errorf("Hosts should still be populated on degrade")
			}
		})
	}
}

func TestValidName(t *testing.T) {
	ok := []string{"web", "web-01", "db_1", "host.example.com", "10.0.0.1", "A1"}
	bad := []string{"", "web,prod", "web:vars", "all*", "a b", "x!y", "g&h"}
	for _, n := range ok {
		if !ValidName(n) {
			t.Errorf("ValidName(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if ValidName(n) {
			t.Errorf("ValidName(%q) = true, want false", n)
		}
	}
}

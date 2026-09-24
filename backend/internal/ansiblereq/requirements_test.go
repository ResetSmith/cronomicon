package ansiblereq

import "testing"

func TestParseModernAndLegacy(t *testing.T) {
	modern := []byte(`
collections:
  - name: community.vmware
    version: 3.5.0
  - community.general
roles:
  - src: https://github.com/geerlingguy/ansible-role-nginx
    name: nginx
    version: 1111111111111111111111111111111111111111
  - geerlingguy.mysql
`)
	r, err := Parse(modern)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Collections) != 2 || r.Collections[0].Name != "community.vmware" || r.Collections[0].Version != "3.5.0" {
		t.Errorf("collections mis-parsed: %+v", r.Collections)
	}
	if r.Collections[1].Name != "community.general" || r.Collections[1].Version != "" {
		t.Errorf("string-form collection mis-parsed: %+v", r.Collections[1])
	}
	if len(r.Roles) != 2 || !isGitRole(r.Roles[0]) {
		t.Errorf("roles mis-parsed: %+v", r.Roles)
	}

	// Legacy bare-list = roles only.
	legacy := []byte("- geerlingguy.redis\n- src: git@example.com:x/y.git\n  version: 2222222222222222222222222222222222222222\n")
	lr, err := Parse(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if len(lr.Roles) != 2 || len(lr.Collections) != 0 {
		t.Errorf("legacy roles-only mis-parsed: %+v", lr)
	}
}

func TestLintPinning(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	r := Requirements{
		Collections: []Collection{
			{Name: "community.vmware", Version: "3.5.0"},  // ok
			{Name: "ansible.posix", Version: ">=1.0.0"},   // range → flagged
			{Name: "community.general", Version: ""},      // unpinned → flagged
			{Name: "community.crypto", Version: "latest"}, // latest → flagged
		},
		Roles: []Role{
			{Name: "nginx", Src: "https://github.com/x/y", Version: sha},    // ok (git, SHA)
			{Name: "redis", Src: "https://github.com/a/b", Version: "main"}, // git, not SHA → flagged
			{Name: "geerlingguy.mysql", Version: "3.3.0"},                   // ok (galaxy exact)
			{Name: "geerlingguy.php", Version: "^4.0"},                      // galaxy range → flagged
		},
	}
	findings := Lint(r)
	flagged := map[string]bool{}
	for _, f := range findings {
		flagged[f.Name] = true
	}
	for _, want := range []string{"ansible.posix", "community.general", "community.crypto", "redis", "geerlingguy.php"} {
		if !flagged[want] {
			t.Errorf("expected %q to be flagged; findings: %v", want, findings)
		}
	}
	for _, ok := range []string{"community.vmware", "nginx", "geerlingguy.mysql"} {
		if flagged[ok] {
			t.Errorf("%q is properly pinned and should NOT be flagged", ok)
		}
	}
}

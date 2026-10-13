package settings

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// GR-10: an agency's repository is on a host a global administrator has
// listed, reached over http or https, with no user in its URL. Global's is
// exempt. The list is one of the general settings, and a write that does not
// carry it leaves it alone.
func TestAnAgencysRepositoryURLIsHeldToTheAllowedHosts(t *testing.T) {
	pool := membershipDB(t)
	ctx := context.Background()
	check := func(repo, url string) error { return CheckRepoURL(ctx, pool, repo, url) }
	refused := func(what, repo, url, wantSays string) {
		t.Helper()
		err := check(repo, url)
		if !errors.Is(err, ErrRepoURLNotAllowed) || !errors.Is(err, ErrValidation) {
			t.Errorf("%s: %q answered %v, want it refused as a validation error", what, url, err)
			return
		}
		if !strings.Contains(err.Error(), wantSays) {
			t.Errorf("%s: the refusal %q does not say %q", what, err, wantSays)
		}
	}
	allowed := func(what, repo, url string) {
		t.Helper()
		if err := check(repo, url); err != nil {
			t.Errorf("%s: %q is refused: %v", what, url, err)
		}
	}

	// As a new installation is: nothing is listed, and Global's is exempt.
	if got, err := GitAllowedHosts(ctx, pool); err != nil || len(got) != 0 {
		t.Fatalf("the list on a new installation: %v (%v), want empty", got, err)
	}
	refused("nothing listed", "repo-fin", "https://git.example.com/fin.git", "no host is allowed")
	allowed("Global's, nothing listed", "global", "https://anywhere.example/global.git")
	allowed("Global's, a path", "global", "/srv/git/global.git")
	allowed("Global's, by its empty id", "", "ssh://git@host/global.git")
	allowed("no URL yet", "repo-fin", "")

	// A global administrator lists hosts. What is stored is normalized.
	set := func(hosts ...string) error {
		gs, err := GetGlobalSettings(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		gs.GitAllowedHosts = &hosts
		_, err = UpdateGlobalSettings(ctx, pool, *gs, "root@example.com")
		return err
	}
	if err := set(" Git.Example.com ", "internal-git:8443", "git.example.com", "", "[2001:db8::1]:8443"); err != nil {
		t.Fatalf("write the list: %v", err)
	}
	gs, err := GetGlobalSettings(ctx, pool)
	if err != nil || gs.GitAllowedHosts == nil {
		t.Fatalf("read the settings: %+v (%v)", gs, err)
	}
	if want := []string{"[2001:db8::1]:8443", "git.example.com", "internal-git:8443"}; !reflect.DeepEqual(*gs.GitAllowedHosts, want) {
		t.Errorf("the stored list is %v, want %v", *gs.GitAllowedHosts, want)
	}

	allowed("a listed host", "repo-fin", "https://git.example.com/fin.git")
	allowed("a listed host, any port", "repo-fin", "https://git.example.com:8443/fin.git")
	allowed("a listed host, another case", "repo-fin", "https://GIT.Example.COM/fin.git")
	allowed("a listed host, over http", "repo-fin", "http://git.example.com/fin.git")
	allowed("a host listed with its port", "repo-fin", "https://internal-git:8443/fin.git")
	allowed("an IPv6 host listed with its port", "repo-fin", "https://[2001:db8::1]:8443/fin.git")
	refused("a host listed with another port", "repo-fin", "https://internal-git:9000/fin.git", "internal-git:9000 is not on the list")
	refused("a host listed with a port, on the default one", "repo-fin", "https://internal-git/fin.git", "is not on the list")
	refused("a host that is not listed", "repo-fin", "https://evil.example.net/fin.git", "evil.example.net is not on the list")
	refused("a listed host as a suffix", "repo-fin", "https://git.example.com.evil.net/fin.git", "is not on the list")
	refused("a listed host as the user", "repo-fin", "https://git.example.com@evil.net/fin.git", "must not carry a user")
	refused("a token in the URL", "repo-fin", "https://oauth2:tok@git.example.com/fin.git", "must not carry a user")
	refused("a path on the server's disk", "repo-fin", "/var/lib/cronomicon/git-cache/job-definitions", "https://")
	refused("a file URL", "repo-fin", "file:///var/lib/cronomicon/git-cache/job-definitions", "https://")
	refused("ssh", "repo-fin", "ssh://git@git.example.com/fin.git", "ssh:// is not accepted")
	refused("the scp form", "repo-fin", "git@git.example.com:fin.git", "https://")
	refused("git", "repo-fin", "git://git.example.com/fin.git", "git:// is not accepted")

	// A host listed as host:443 allows the default https port.
	if err := set("only-tls.example:443"); err != nil {
		t.Fatal(err)
	}
	allowed("the default port, listed", "repo-fin", "https://only-tls.example/fin.git")
	refused("http on a host listed for 443", "repo-fin", "http://only-tls.example/fin.git", "is not on the list")

	// A write of the general settings that does not carry the list leaves it.
	gs, _ = GetGlobalSettings(ctx, pool)
	gs.GitAllowedHosts = nil
	gs.AppName = "Renamed"
	if _, err := UpdateGlobalSettings(ctx, pool, *gs, "root@example.com"); err != nil {
		t.Fatal(err)
	}
	if got, _ := GitAllowedHosts(ctx, pool); !reflect.DeepEqual(got, []string{"only-tls.example:443"}) {
		t.Errorf("a write without the list left it as %v, want it untouched", got)
	}
	// An empty list, written, empties it.
	if err := set(); err != nil {
		t.Fatal(err)
	}
	if got, _ := GitAllowedHosts(ctx, pool); len(got) != 0 {
		t.Errorf("an empty list, written, left %v", got)
	}

	// An entry that is more than a host is refused, and nothing is stored.
	for _, bad := range []string{"https://git.example.com", "git.example.com/path", "*.example.com", "user@git.example.com", "git.example.com:0", "git.example.com:port", "host name"} {
		if err := set("ok.example", bad); !errors.Is(err, ErrValidation) {
			t.Errorf("the entry %q was accepted (%v), want a validation error", bad, err)
		}
	}
	if got, _ := GitAllowedHosts(ctx, pool); len(got) != 0 {
		t.Errorf("a refused write stored %v", got)
	}
}

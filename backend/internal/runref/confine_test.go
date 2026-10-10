package runref

import (
	"context"
	"testing"
)

// GR-15 — a job of an agency's repository runs on that agency's scopes and on
// no other. The property that matters as much as the refusal is what is NOT
// refused: a job of Global's repository, and a job built in the app, on any
// scope or none.
func TestRepoScopeMismatch(t *testing.T) {
	f := newOwnerFixture(t) // agencies ag-a and ag-b
	f.exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-a', 'ag-a', 'u', 'main')`)
	scope := func(id, name string, agencies ...string) {
		t.Helper()
		f.exec(`INSERT INTO scopes (id, name, source, created_at) VALUES (?, ?, 'cronomicon', 't')`, id, name)
		f.exec(`DELETE FROM scope_agencies WHERE scope_id = ?`, id)
		for _, a := range agencies {
			f.exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, id, a)
		}
	}
	scope("s-a", "a-hosts", "ag-a")
	scope("s-b", "b-hosts", "ag-b")
	scope("s-g", "g-hosts", "global")
	scope("s-ab", "shared-hosts", "ag-a", "ag-b") // from before a scope had one agency
	job := func(uid, name, source string, repo any) {
		t.Helper()
		f.exec(`INSERT INTO jobs (uid, name, source, run_type, synced_at, repo_id) VALUES (?, ?, ?, 'bash', 't', ?)`, uid, name, source, repo)
	}
	job("j-a", "of-a", "git", "repo-a")
	job("j-g", "of-global", "git", "global")
	job("j-old", "from-before", "git", nil) // a Git job that records no repository
	job("j-app", "built", "cronomicon", nil)

	ctx := context.Background()
	for _, c := range []struct {
		what           string
		uid, src, name string
		scope          string
		wantMismatch   bool
	}{
		{"its agency's scope", "j-a", "git", "of-a", "a-hosts", false},
		{"another agency's scope", "j-a", "git", "of-a", "b-hosts", true},
		{"Global's scope", "j-a", "git", "of-a", "g-hosts", true},
		{"no scope", "j-a", "git", "of-a", "", true},
		{"a scope that is not there", "j-a", "git", "of-a", "no-such-scope", true},
		{"a scope several agencies share", "j-a", "git", "of-a", "shared-hosts", true},
		{"found by name, its agency's scope", "", "git", "of-a", "a-hosts", false},
		{"found by name, another agency's scope", "", "git", "of-a", "b-hosts", true},
		{"found by default source, another agency's scope", "", "", "of-a", "b-hosts", true},
		{"Global's repository's job, another agency's scope", "j-g", "git", "of-global", "b-hosts", false},
		{"Global's repository's job, no scope", "j-g", "git", "of-global", "", false},
		{"a Git job with no repository", "j-old", "git", "from-before", "b-hosts", false},
		{"a job built in the app", "j-app", "cronomicon", "built", "b-hosts", false},
		{"a job built in the app, by name", "", "cronomicon", "of-a", "b-hosts", false},
		{"a job that is not there", "no-such-job", "git", "nobody", "b-hosts", false},
	} {
		got, err := RepoScopeMismatch(ctx, f.pool, c.uid, c.src, c.name, c.scope)
		if err != nil {
			t.Errorf("%s: %v", c.what, err)
			continue
		}
		if got != c.wantMismatch {
			t.Errorf("%s: refused = %v, want %v", c.what, got, c.wantMismatch)
		}
	}

	// The scope is given to another agency after the fact: the same question
	// now has the other answer.
	f.exec(`DELETE FROM scope_agencies WHERE scope_id = 's-a'`)
	f.exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('s-a', 'ag-b')`)
	if got, err := RepoScopeMismatch(ctx, f.pool, "j-a", "git", "of-a", "a-hosts"); err != nil || !got {
		t.Errorf("after its scope was given to another agency: refused = %v (%v), want true", got, err)
	}
}

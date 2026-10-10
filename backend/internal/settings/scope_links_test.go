package settings

import (
	"context"
	"errors"
	"testing"
)

// A Git scope's "Edit in GitLab" link leads to the file in the scope's OWN
// repository, on that repository's branch. Until 2.4.0 there was one
// repository and every link was built from it.
func TestAScopesLinkIsToItsOwnRepository(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	exec(`UPDATE git_repos SET url = 'https://git.example/global/defs.git', branch = 'main' WHERE id = 'global'`)
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-b', 'B', 't'), ('ag-c', 'C', 't')`)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-b', 'ag-b', 'https://git.example/finance/defs', 'trunk')`)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-c', 'ag-c', '', '')`)
	scope := func(id, name, repo any) {
		exec(`INSERT INTO scopes (id, name, source, created_by, created_at, source_path, synced_at, repo_id)
		      VALUES (?, ?, 'git', 'gitlab', 't', 'inventory/' || ? || '.ini', 't', ?)`, id, name, name, repo)
	}
	scope("s-g", "of-global", "global")
	scope("s-b", "of-b", "repo-b")
	scope("s-c", "of-c", "repo-c")
	scope("s-old", "from-before", nil) // a Git scope from before a scope recorded its repository

	want := map[string]string{
		"s-g":   "https://git.example/global/defs/-/blob/main/inventory/of-global.ini",
		"s-b":   "https://git.example/finance/defs/-/blob/trunk/inventory/of-b.ini",
		"s-c":   "",
		"s-old": "https://git.example/global/defs/-/blob/main/inventory/from-before.ini",
	}
	list, err := ListScopes(ctx, pool, "git")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, sc := range list {
		w, ok := want[sc.ID]
		if !ok {
			continue
		}
		seen++
		got := ""
		if sc.GitLabURL != nil {
			got = *sc.GitLabURL
		}
		if got != w {
			t.Errorf("list: the scope %s links to %q, want %q", sc.Scope, got, w)
		}
		one, err := GetScope(ctx, pool, sc.ID)
		if err != nil || one == nil {
			t.Fatalf("get %s: %v", sc.ID, err)
		}
		got = ""
		if one.GitLabURL != nil {
			got = *one.GitLabURL
		}
		if got != w {
			t.Errorf("detail: the scope %s links to %q, want %q", sc.Scope, got, w)
		}
	}
	if seen != len(want) {
		t.Errorf("the list returned %d of the %d scopes", seen, len(want))
	}
}

// A scope whose inventory is in an agency's repository belongs to that agency
// (2.4.0, GR-18), and each of the three writers of a scope's agency says so
// with its own error; nothing is written. A scope from Global's repository, or
// one built in the app, moves as before.
func TestAScopeOfAnAgencysRepositoryIsNotMoved(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-b', 'B', 't'), ('ag-c', 'C', 't')`)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-b', 'ag-b', 'u', 'main')`)
	exec(`INSERT INTO scopes (id, name, source, created_by, created_at, repo_id) VALUES ('s-b', 'theirs', 'git', 'gitlab', 't', 'repo-b')`)
	exec(`INSERT INTO scopes (id, name, source, created_by, created_at, repo_id) VALUES ('s-g', 'globals', 'git', 'gitlab', 't', 'global')`)
	exec(`INSERT INTO scopes (id, name, source, created_by, created_at) VALUES ('s-app', 'built', 'cronomicon', 'a', 't')`)
	agencies := func(scope string) string {
		t.Helper()
		var got string
		if err := pool.QueryRowContext(ctx, `SELECT COALESCE(GROUP_CONCAT(agency_id, ','), '') FROM (
			SELECT agency_id FROM scope_agencies WHERE scope_id = ? ORDER BY agency_id)`, scope).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := agencies("s-b"); got != "ag-b" {
		t.Fatalf("the scope of the agency's repository is in %q, want ag-b", got)
	}
	for _, id := range []string{"s-b", "s-g", "s-app", "no-such-scope"} {
		want := map[string]string{"s-b": "ag-b"}[id]
		if got, err := ScopeAgencyFixedTo(ctx, pool, id); err != nil || got != want {
			t.Errorf("ScopeAgencyFixedTo(%s) = %q, %v; want %q", id, got, err, want)
		}
	}

	other, own := "ag-c", "ag-b"
	refused := map[string]func() error{
		"set to another agency": func() error { _, err := SetScopeAgency(ctx, pool, "s-b", &other, "a"); return err },
		"returned to Global":    func() error { _, err := SetScopeAgency(ctx, pool, "s-b", nil, "a"); return err },
		"the matrix, to another agency": func() error {
			return SetAgencyMembership(ctx, pool, MemberScope, []AgencyMembership{{ID: "s-b", AgencyIDs: []string{"ag-c"}}}, "a")
		},
		"the matrix, to Global": func() error {
			return SetAgencyMembership(ctx, pool, MemberScope, []AgencyMembership{{ID: "s-b", AgencyIDs: []string{"global"}}}, "a")
		},
		"added to another agency's members": func() error {
			_, err := SetAgencyMembers(ctx, pool, "ag-c", []AgencyMemberRef{{Kind: "scope", ID: "s-b"}}, "a")
			return err
		},
		"removed from its agency's members": func() error {
			_, err := SetAgencyMembers(ctx, pool, "ag-b", nil, "a")
			return err
		},
	}
	for what, write := range refused {
		if err := write(); !errors.Is(err, ErrScopeAgencyFixed) {
			t.Errorf("%s: %v, want ErrScopeAgencyFixed", what, err)
		}
		if got := agencies("s-b"); got != "ag-b" {
			t.Fatalf("%s: the scope is now in %q", what, got)
		}
	}
	// Naming the agency it already has is not a move.
	if _, err := SetScopeAgency(ctx, pool, "s-b", &own, "a"); err != nil {
		t.Errorf("setting the scope to its own agency: %v", err)
	}
	if err := SetAgencyMembership(ctx, pool, MemberScope, []AgencyMembership{{ID: "s-b", AgencyIDs: []string{"ag-b"}}}, "a"); err != nil {
		t.Errorf("the matrix naming its own agency: %v", err)
	}
	// The others move as before.
	if _, err := SetScopeAgency(ctx, pool, "s-g", &other, "a"); err != nil || agencies("s-g") != "ag-c" {
		t.Errorf("Global's repository's scope: %v, in %q", err, agencies("s-g"))
	}
	if _, err := SetScopeAgency(ctx, pool, "s-app", &other, "a"); err != nil || agencies("s-app") != "ag-c" {
		t.Errorf("a scope built in the app: %v, in %q", err, agencies("s-app"))
	}
}

package settings

import (
	"context"
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

package api_test

import (
	"database/sql"
	"strings"
	"testing"
)

// agenciesGetTheirOwnRepository gives every agency that has no repository one
// of its own ("repo-<agency id>"), holding a copy of each script Global's
// repository has, under the same name.
//
// Since §0a (IN-8) an agency's definitions find Git names in their own
// agency's repository and nowhere else: until then an agency with none found
// them in Global's. Tests written before §0a seed a script into Global's
// repository and compose an agency's job on it; this puts the script where
// such a job now looks, as I3 will have every agency hold a repository.
func agenciesGetTheirOwnRepository(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info('scripts')`)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		if c != "uid" && c != "repo_id" {
			cols = append(cols, c)
		}
	}
	rows.Close()
	list := strings.Join(cols, ", ")
	var agencies []string
	arows, err := db.Query(`SELECT id FROM agencies WHERE id <> 'global' AND id NOT IN (SELECT agency_id FROM git_repos)`)
	if err != nil {
		t.Fatal(err)
	}
	for arows.Next() {
		var a string
		if err := arows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		agencies = append(agencies, a)
	}
	arows.Close()
	for _, a := range agencies {
		repo := "repo-" + a
		if _, err := db.Exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES (?, ?, 'https://git.example/'||?, 'main')`, repo, a, a); err != nil {
			t.Fatalf("give %s a repository: %v", a, err)
		}
		//nolint:gosec // the columns are the table's own
		if _, err := db.Exec(`INSERT INTO scripts (uid, repo_id, `+list+`)
		                      SELECT 'copy-'||lower(hex(randomblob(8))), ?, `+list+` FROM scripts WHERE COALESCE(repo_id, 'global') = 'global'`, repo); err != nil {
			t.Fatalf("copy Global's scripts into %s: %v", repo, err)
		}
	}
}

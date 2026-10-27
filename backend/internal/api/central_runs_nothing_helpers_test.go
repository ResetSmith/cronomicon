package api_test

import (
	"database/sql"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/auth"
)

// Since 2.5.3 (CA) the built-in agency runs nothing: a global administrator who
// names no agency cannot create a job, workflow, schedule, secret, variable,
// SSH key, scope or host record (422 central_runs_nothing), and a definition
// that is the built-in agency's is refused when it would run. A test whose
// subject is something else works in an ordinary agency instead, with these.

// workAgency is the ordinary agency such a test works in.
// Its repository, workRepo, is where its in-app jobs find their scripts and
// schedules (an in-app job's names mean its agency's repository's, IN-8).
const (
	workAgency     = "ag-work"
	workAgencyName = "Work"
	workRepo       = "repo-work"
)

// seedWorkAgency inserts workAgency and its repository (once) and returns its id.
func seedWorkAgency(t *testing.T, pool *sql.DB) string {
	t.Helper()
	if _, err := pool.Exec(`INSERT OR IGNORE INTO agencies (id, name, created_at)
		VALUES (?, ?, '2026-01-01T00:00:00Z')`, workAgency, workAgencyName); err != nil {
		t.Fatalf("seed agency %s: %v", workAgency, err)
	}
	if _, err := pool.Exec(`INSERT OR IGNORE INTO git_repos (id, agency_id, url, branch)
		VALUES (?, ?, 'https://git.example/work', 'main')`, workRepo, workAgency); err != nil {
		t.Fatalf("seed repository %s: %v", workRepo, err)
	}
	seedScopeOf(t, pool, workScopeID, workScope, workAgency)
	return workAgency
}

// seedScopeOf inserts (once) the scope id named name, agency's alone.
func seedScopeOf(t *testing.T, pool *sql.DB, id, name, agency string) {
	t.Helper()
	var have int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM scopes WHERE id = ?`, id).Scan(&have); err != nil {
		t.Fatal(err)
	}
	if have > 0 {
		return
	}
	for _, q := range []string{
		`INSERT INTO scopes (id, name, source, created_at) VALUES (?1, ?2, 'cronomicon', '2026-01-01T00:00:00Z')`,
		`INSERT OR IGNORE INTO scope_agencies (scope_id, agency_id) VALUES (?1, ?3)`,
		`DELETE FROM scope_agencies WHERE scope_id = ?1 AND agency_id <> ?3`,
	} {
		if _, err := pool.Exec(q, id, name, agency); err != nil {
			t.Fatalf("seed scope %s of %s: %v", name, agency, err)
		}
	}
}

// ownScopesByWorkAgency makes the named in-app scopes (seeded past the API, so
// born the built-in agency's) workAgency's alone, with their host records: a
// job finds its scope in its own agency (I2).
func ownScopesByWorkAgency(t *testing.T, pool *sql.DB, names ...string) {
	t.Helper()
	a := seedWorkAgency(t, pool)
	for _, n := range names {
		for _, q := range []string{
			`INSERT OR IGNORE INTO scope_agencies (scope_id, agency_id)
			 SELECT id, ?1 FROM scopes WHERE name = ?2 AND owner_agency = 'global'`,
			`DELETE FROM scope_agencies WHERE agency_id <> ?1
			    AND scope_id IN (SELECT id FROM scopes WHERE name = ?2)
			    AND scope_id IN (SELECT scope_id FROM scope_agencies WHERE agency_id = ?1)`,
			`UPDATE ssh_hosts SET owner_agency = ?1
			  WHERE owner_agency = 'global' AND scope_id IN (SELECT id FROM scopes WHERE name = ?2 AND owner_agency = ?1)`,
		} {
			if _, err := pool.Exec(q, a, n); err != nil {
				t.Fatalf("own scope %s by %s: %v", n, a, err)
			}
		}
	}
}

// seedWorkScope inserts a scope named name of workAgency's alone: a scope
// like "Prod" that a test's job names, free text while the job was the
// built-in agency's, is its agency's scope now that the job is workAgency's
// (runref.RepoScopeMismatch confines its runs to its agency's scopes).
func seedWorkScope(t *testing.T, pool *sql.DB, name string) {
	t.Helper()
	seedScopeOf(t, pool, "sc-work-"+name, name, seedWorkAgency(t, pool))
}

// workScope is a scope of workAgency's alone. A job built in the app on its
// agency's repository's script runs on its agency's scopes and on no other, nor
// on none (runref.RepoScopeMismatch, GR-15): a test that RUNS such a job puts
// it here.
const (
	workScope   = "work-hosts"
	workScopeID = "sc-work"
)

// copyGlobalDefsToWorkRepo puts a copy of each script and Git schedule of the
// built-in agency's repository into workRepo, under the same name, where
// workAgency's in-app jobs look for them (IN-8): for a test that seeds them the
// way it did when the built-in agency composed.
func copyGlobalDefsToWorkRepo(t *testing.T, pool *sql.DB) {
	t.Helper()
	seedWorkAgency(t, pool)
	columns := func(table string, skip ...string) string {
		rows, err := pool.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var cols []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(skip, c) {
				cols = append(cols, c)
			}
		}
		return strings.Join(cols, ", ")
	}
	list := columns("scripts", "uid", "repo_id")
	//nolint:gosec // the columns are the table's own
	if _, err := pool.Exec(`INSERT INTO scripts (uid, repo_id, `+list+`)
		SELECT 'work-'||lower(hex(randomblob(8))), ?, `+list+` FROM scripts s
		 WHERE COALESCE(s.repo_id, 'global') = 'global'
		   AND NOT EXISTS (SELECT 1 FROM scripts w WHERE w.repo_id = ? AND w.name = s.name)`, workRepo, workRepo); err != nil {
		t.Fatalf("copy the built-in agency's scripts into %s: %v", workRepo, err)
	}
	list = columns("schedules", "uid", "repo_id", "owner_agency")
	//nolint:gosec // the columns are the table's own
	if _, err := pool.Exec(`INSERT INTO schedules (uid, repo_id, owner_agency, `+list+`)
		SELECT 'work-'||lower(hex(randomblob(8))), ?, ?, `+list+` FROM schedules s
		 WHERE s.source = 'git' AND COALESCE(s.repo_id, 'global') = 'global'
		   AND NOT EXISTS (SELECT 1 FROM schedules w WHERE w.repo_id = ? AND w.name = s.name)`, workRepo, workAgency, workRepo); err != nil {
		t.Fatalf("copy the built-in agency's schedules into %s: %v", workRepo, err)
	}
}

// agencyTransport names an agency on every request that names none, as the
// shell does for a person working in that agency (agencyctx).
type agencyTransport struct {
	base   http.RoundTripper
	agency string
}

func (a agencyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get(auth.AgencyHeader) == "" {
		r = r.Clone(r.Context())
		r.Header.Set(auth.AgencyHeader, a.agency)
	}
	base := a.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// workInAgency seeds workAgency and makes every request of client work in it.
func workInAgency(t *testing.T, pool *sql.DB, client *http.Client) string {
	t.Helper()
	a := seedWorkAgency(t, pool)
	client.Transport = agencyTransport{base: client.Transport, agency: a}
	return a
}

// namingNoAgency is client (same session) without workInAgency's header: a
// request that names no agency, as a script's does — for what is still created
// outside any agency (an all-agencies service account, an alert rule).
func namingNoAgency(client *http.Client) *http.Client {
	cp := *client
	if at, ok := client.Transport.(agencyTransport); ok {
		cp.Transport = at.base
	}
	return &cp
}

// ownKeyByWorkAgency makes the SSH key id (seeded past the API, so born the
// built-in agency's) workAgency's, owner and membership: since §0a (IN-6) a key
// of the built-in agency's is usable by its own runs only.
func ownKeyByWorkAgency(t *testing.T, pool *sql.DB, id string) {
	t.Helper()
	a := seedWorkAgency(t, pool)
	for _, q := range []string{
		`INSERT OR IGNORE INTO ssh_credential_agencies (credential_id, agency_id) VALUES (?1, ?2)`,
		`DELETE FROM ssh_credential_agencies WHERE credential_id = ?1 AND agency_id <> ?2`,
		`UPDATE ssh_credentials SET owner_agency = ?2 WHERE id = ?1`,
	} {
		if _, err := pool.Exec(q, id, a); err != nil {
			t.Fatalf("own key %s by %s: %v", id, a, err)
		}
	}
}

// ownJobsByWorkAgency makes the named jobs (seeded past the API with no owner
// and no repository, so born the built-in agency's) workAgency's: by their
// owner, which is a Git job's agency too while it records no repository
// (agencyid.JobSQL), and confines it to no scope.
func ownJobsByWorkAgency(t *testing.T, pool *sql.DB, names ...string) {
	t.Helper()
	a := seedWorkAgency(t, pool)
	for _, n := range names {
		if _, err := pool.Exec(`UPDATE jobs SET owner_agency = ? WHERE name = ?`, a, n); err != nil {
			t.Fatalf("own job %s by %s: %v", n, a, err)
		}
	}
}

// ownWorkflowsByWorkAgency makes the named workflows (seeded past the API, so
// born the built-in agency's) workAgency's: a Git one by its repository
// (workRepo; one that records none is Global's repository's,
// agencyid.WorkflowSQL), one built in the app by its owner.
func ownWorkflowsByWorkAgency(t *testing.T, pool *sql.DB, names ...string) {
	t.Helper()
	a := seedWorkAgency(t, pool)
	for _, n := range names {
		if _, err := pool.Exec(`UPDATE workflows SET
			repo_id = CASE WHEN source = 'git' THEN ?1 ELSE repo_id END,
			owner_agency = CASE WHEN source = 'git' THEN owner_agency ELSE ?2 END
			WHERE name = ?3`, workRepo, a, n); err != nil {
			t.Fatalf("own workflow %s by %s: %v", n, a, err)
		}
	}
}

package gitlab

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// grScopedJob is a job that names a scope.
func grScopedJob(name, scope string) string {
	return grJob(name, "echo "+name) + "  scope: " + scope + "\n"
}

// A job in an agency's repository must name a scope that agency owns (Phase
// R4, GR-14). One that names another agency's scope, a scope that is not
// there, Global's scope, or no scope at all is not written; its file is told
// so, in words that do not say whether the scope it named exists; and nothing
// else of the sync is held back by it.
//
// Until Phase R4 a second repository's job was written whatever scope it
// named: an agency's committer could put a job on another agency's hosts, or,
// by naming no scope, make it Global's work.
func TestGR4_AJobInAnAgencysRepositoryMustNameItsAgencysScope(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"inventory/globals.ini": fmt.Sprintf(grScope, "g1", "10.0.0.1"),
		// Global's repository is not confined: any scope, or none.
		"jobs/g-none.yaml":   grJob("g-none", "echo g"),
		"jobs/g-theirs.yaml": grScopedJob("g-theirs", "theirs"),
	}, "Global's repository")
	grSync(t, a, "Global's repository")

	c, repoC, remoteC := grRepo(t, a, "repo-c", "ag-c")
	gitCommitFile(t, repoC, remoteC, "inventory/cs.ini", fmt.Sprintf(grScope, "c1", "10.0.0.3"), "a third agency's scope")
	grSync(t, c, "the third agency's repository")

	b, repoB, remoteB := grSecondRepo(t, a)
	// A scope the agency built in the app is its own too.
	if _, err := a.db.Exec(`INSERT INTO scopes (id, name, source, created_by, created_at) VALUES ('s-built', 'built', 'cronomicon', 'a', 't')`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('s-built', 'ag-b')`); err != nil {
		t.Fatal(err)
	}
	grCommitFiles(t, repoB, remoteB, map[string]string{
		"inventory/theirs.ini":   fmt.Sprintf(grScope, "b1", "10.0.0.2"),
		"jobs/ok.yaml":           grScopedJob("ok", "theirs"), // the scope arrives in this same commit
		"jobs/ok-built.yaml":     grScopedJob("ok-built", "built"),
		"jobs/none.yaml":         grJob("none", "echo none") + "  scope: \"\"\n",
		"jobs/globals.yaml":      grScopedJob("globals", "globals"),
		"jobs/others.yaml":       grScopedJob("others", "cs"),
		"jobs/typo.yaml":         grScopedJob("typo", "no-such-scope"),
		"jobs/team/nested.yaml":  grScopedJob("nested", "cs"),
		"jobs/leaving-soon.yaml": grScopedJob("leaving-soon", "theirs"),
	}, "the agency's repository")
	res := grSync(t, b, "the agency's repository, its first sync")

	has := func(name string) bool {
		t.Helper()
		return grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE source='git' AND repo_id='repo-b' AND name=?`, name) == 1
	}
	for _, name := range []string{"ok", "ok-built", "leaving-soon"} {
		if !has(name) {
			t.Errorf("the job %s, which names one of its agency's scopes, was not synced", name)
		}
	}
	for _, name := range []string{"none", "globals", "others", "typo", "nested"} {
		if has(name) {
			t.Errorf("the job %s was synced although it does not name one of its agency's scopes", name)
		}
	}
	if res.Status != "partial" {
		t.Errorf("the sync with refused files is %q, want partial", res.Status)
	}
	rows := grProblems(t, b, "repo-b")
	for file, want := range map[string]string{
		"jobs/none.yaml":        "must name one of that agency's scopes",
		"jobs/globals.yaml":     `the scope "globals" is not one of this repository's agency's scopes`,
		"jobs/others.yaml":      `the scope "cs" is not one of this repository's agency's scopes`,
		"jobs/typo.yaml":        `the scope "no-such-scope" is not one of this repository's agency's scopes`,
		"jobs/team/nested.yaml": `the scope "cs" is not one of this repository's agency's scopes`,
	} {
		if !grHas(rows, "error job "+file+":", want) {
			t.Errorf("no error row against %s saying %q:\n%s", file, want, strings.Join(rows, "\n"))
		}
	}
	// A scope that is another agency's and one that is nobody's are refused in
	// the same words: which it is, is not this repository's to learn.
	said := func(file string) string {
		for _, r := range rows {
			if strings.Contains(r, " "+file+": ") {
				_, msg, _ := strings.Cut(r, file+": ")
				return msg
			}
		}
		return ""
	}
	if o, n := strings.ReplaceAll(said("jobs/others.yaml"), `"cs"`, `"X"`), strings.ReplaceAll(said("jobs/typo.yaml"), `"no-such-scope"`, `"X"`); o != n || o == "" {
		t.Errorf("another agency's scope and no scope at all are refused differently:\n%s\n%s", o, n)
	}
	if _, open, detail := grNotice(t, b, "repo-b"); !open || !strings.Contains(detail, "jobs/") {
		t.Errorf("the repository's notice: open=%v %q, want it open, naming a refused file", open, detail)
	}
	// Global's repository's jobs are as they were.
	for _, name := range []string{"g-none", "g-theirs", "keep"} {
		if grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE source='git' AND repo_id='global' AND name=?`, name) != 1 {
			t.Errorf("Global's repository's job %s is missing", name)
		}
	}
	if rowsA := grProblems(t, a, "global"); grHas(rowsA, "is not one of this repository's") || grHas(rowsA, "must name one of") {
		t.Errorf("Global's repository's jobs were confined:\n%s", strings.Join(rowsA, "\n"))
	}

	// The refused files are still there at the next sync, and the kind is still
	// PRUNED: a job whose file leaves goes, and a job that was synced and now
	// names another agency's scope goes with it.
	gitRemoveFile(t, repoB, "jobs/leaving-soon.yaml", "a job leaves")
	gitCommitFile(t, repoB, remoteB, "jobs/ok.yaml", grScopedJob("ok", "cs"), "a synced job is pointed at another agency's scope")
	grBackdateAll(t, a)
	grSync(t, b, "the agency's repository again")
	if has("leaving-soon") {
		t.Errorf("a job whose file left was not pruned: a refused file switched the prune off")
	}
	if has("ok") {
		t.Errorf("a job that was synced and now names another agency's scope is still there")
	}
	if !has("ok-built") {
		t.Errorf("the job that still names its agency's scope was pruned")
	}
	if !grHas(grProblems(t, b, "repo-b"), "error job jobs/ok.yaml:", `the scope "cs"`) {
		t.Errorf("no error row for the job that was pointed at another agency's scope")
	}
}

// What a sync warns of about a job's credential or secret is asked within the
// repository's agency: a name only ANOTHER agency holds "names no stored" one
// for this repository, as it will at dispatch. Global's repository, whose jobs
// may name any agency's scope, is asked as before.
//
// Until Phase R4 the question was "does anybody have one of this name", so the
// absence of a warning told an agency's committers which names other agencies
// hold.
func TestGR4_AJobsAdvisoryChecksAskWithinItsAgency(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	b, repoB, remoteB := grSecondRepo(t, a)
	if _, err := a.db.Exec(`INSERT OR IGNORE INTO agencies (id, name, created_at) VALUES ('ag-c', 'Agency C', 't')`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO ssh_credentials (id, label, source, created_at, owner_agency) VALUES ('k-c', 'theirs-key', 'stored', 't', 'ag-c')`,
		`INSERT INTO ssh_credentials (id, label, source, created_at, owner_agency) VALUES ('k-b', 'own-key', 'stored', 't', 'ag-b')`,
		`INSERT INTO ssh_credentials (id, label, source, created_at, owner_agency) VALUES ('k-g', 'global-key', 'stored', 't', 'global')`,
		`INSERT INTO secrets (id, key, scope, source, created_at, owner_agency) VALUES ('s-c', 'THEIRS_PW', '', 'stored', 't', 'ag-c')`,
		`INSERT INTO secrets (id, key, scope, source, created_at, owner_agency) VALUES ('s-b', 'OWN_PW', '', 'stored', 't', 'ag-b')`,
	} {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	job := func(name, scope, key, secret string) string {
		y := "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: " + name + "\nspec:\n  run_type: ansible\n  command: site.yml\n"
		if scope != "" {
			y += "  scope: " + scope + "\n"
		}
		return y + "  ssh_credential: " + key + "\n  become_password_secret: " + secret + "\n"
	}
	files := func(scope string) map[string]string {
		return map[string]string{
			"jobs/j-theirs.yaml": job("j-theirs", scope, "theirs-key", "THEIRS_PW"),
			"jobs/j-own.yaml":    job("j-own", scope, "own-key", "OWN_PW"),
			"jobs/j-global.yaml": job("j-global", scope, "global-key", "OWN_PW"),
		}
	}
	warned := func(svc *Service, repo *bytes.Buffer, name, what string) bool {
		for _, line := range strings.Split(repo.String(), "\n") {
			if strings.Contains(line, "job="+name) && strings.Contains(line, what) {
				return true
			}
		}
		return false
	}

	var logB, logA bytes.Buffer
	b.log = slog.New(slog.NewTextHandler(&logB, nil))
	a.log = slog.New(slog.NewTextHandler(&logA, nil))
	withScope := files("theirs")
	withScope["inventory/theirs.ini"] = fmt.Sprintf(grScope, "b1", "10.0.0.2")
	grCommitFiles(t, repoB, remoteB, withScope, "the agency's repository")
	grSync(t, b, "the agency's repository")
	for name, want := range map[string]bool{"j-theirs": true, "j-own": false, "j-global": false} {
		if got := warned(b, &logB, name, "names no stored SSH credential"); got != want {
			t.Errorf("the agency's repository, %s: warned of its SSH credential = %v, want %v", name, got, want)
		}
	}
	for name, want := range map[string]bool{"j-theirs": true, "j-own": false} {
		if got := warned(b, &logB, name, "names no stored Secret"); got != want {
			t.Errorf("the agency's repository, %s: warned of its secret = %v, want %v", name, got, want)
		}
	}

	grCommitFiles(t, repoA, remoteA, files(""), "Global's repository")
	grSync(t, a, "Global's repository")
	for _, name := range []string{"j-theirs", "j-own", "j-global"} {
		if warned(a, &logA, name, "names no stored SSH credential") || warned(a, &logA, name, "names no stored Secret") {
			t.Errorf("Global's repository, %s: warned of a name that an agency holds", name)
		}
	}
}

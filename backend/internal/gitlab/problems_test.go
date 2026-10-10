package gitlab

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// grProblems reads a repository's problem rows as "severity kind path: message".
func grProblems(t *testing.T, svc *Service, repo string) []string {
	t.Helper()
	rows, err := svc.db.Query(`SELECT severity || ' ' || kind || ' ' || path || ': ' || message
	                             FROM git_sync_problems WHERE repo_id = ? ORDER BY severity, path, message`, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// grNotice reads a repository's problems notice: its agency, whether it is
// open, and its detail ("" when it has none).
func grNotice(t *testing.T, svc *Service, repo string) (agency string, open bool, detail string) {
	t.Helper()
	var resolved *string
	err := svc.db.QueryRow(`SELECT agency_id, resolved_at, detail FROM notices WHERE kind = 'git_sync_problems' AND subject = ?`, repo).
		Scan(&agency, &resolved, &detail)
	if err != nil {
		return "", false, ""
	}
	return agency, resolved == nil, detail
}

func grHas(lines []string, parts ...string) bool {
	for _, l := range lines {
		ok := true
		for _, p := range parts {
			if !strings.Contains(l, p) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// What a sync has to say about a repository's files is a row per problem, for
// that repository, replaced by each of its syncs (GR-30), with one notice in
// the inbox for a repository whose last sync reported ERRORS. A warning is a
// row and is counted; it opens no notice and keeps none open.
//
// Until Phase R3 a file error was a piece of one text column of the sync's
// history row, and a warning was a line in the server's log and nowhere else.
func TestGR3_ASyncsProblemsAreRows(t *testing.T) {
	svc, repo, remote := newSyncFixture(t) // jobs/keep.yaml, which declares no scope
	ctx := context.Background()
	sync := func(what string) SyncResult {
		t.Helper()
		r := svc.SyncBlocking(ctx, "manual")
		if r.Status == "failed" {
			t.Fatalf("%s: sync failed: %s", what, r.ErrorMessage)
		}
		return r
	}
	const bad = "apiVersion: cronomicon.io/v2\nkind: Job\nmetadata:\n  name: bad\n"
	grCommitFiles(t, repo, remote, map[string]string{
		"jobs/bad.yaml":         bad,
		"jobs/team/nested.yaml": jobYAML("nested"),
		"jobs/broken.yaml":      "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: [unclosed\n",
	}, "a file that does not validate, one that does not parse, and a job in a folder")
	if r := sync("first sync"); r.Status != "partial" {
		t.Fatalf("the sync with a file that does not validate is %q, want partial", r.Status)
	}

	rows := grProblems(t, svc, "global")
	if !grHas(rows, "error job jobs/bad.yaml:") {
		t.Errorf("no error row for the file that does not validate:\n%s", strings.Join(rows, "\n"))
	}
	// An error the sync reports as plain text ("parse jobs/broken.yaml: …") is
	// a row against its file all the same, not one against the repository.
	if !grHas(rows, "error job jobs/broken.yaml:") {
		t.Errorf("no error row against the file that does not parse:\n%s", strings.Join(rows, "\n"))
	}
	if grHas(rows, "error repository :") {
		t.Errorf("an error of a file is filed against the repository as a whole:\n%s", strings.Join(rows, "\n"))
	}
	for _, path := range []string{"jobs/keep.yaml", "jobs/team/nested.yaml"} {
		if !grHas(rows, "warning job "+path+":", "declares no scope") {
			t.Errorf("no warning row for %s, which declares no scope:\n%s", path, strings.Join(rows, "\n"))
		}
	}
	// A row says what is wrong with a file of the repository. It does not say
	// where on the server the repository is cloned.
	for _, r := range rows {
		if strings.Contains(r, svc.cloneDir) || strings.Contains(r, "/clone/") {
			t.Errorf("a problem row carries the clone's directory: %s", r)
		}
	}
	agency, open, detail := grNotice(t, svc, "global")
	if !open || agency != "global" {
		t.Fatalf("the repository's notice: open=%v agency=%q, want it open under Global", open, agency)
	}
	count := func(rows []string, severity string) int {
		n := 0
		for _, r := range rows {
			if strings.HasPrefix(r, severity+" ") {
				n++
			}
		}
		return n
	}
	counted := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %ss", n, one)
	}
	if count(rows, "error") == 0 || count(rows, "warning") < 2 {
		t.Fatalf("rows: %d error(s), %d warning(s); want the file's errors and at least the two jobs' warnings", count(rows, "error"), count(rows, "warning"))
	}
	for _, want := range []string{counted(count(rows, "error"), "error"), counted(count(rows, "warning"), "warning"), "jobs/bad.yaml"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the notice does not say %q: %s", want, detail)
		}
	}
	if strings.Contains(detail, svc.cloneDir) {
		t.Errorf("the notice carries the clone's directory: %s", detail)
	}
	// The notice quotes the errors. A warning is counted and not quoted.
	if strings.Contains(detail, "declares no scope") {
		t.Errorf("the notice quotes a warning: %s", detail)
	}

	// Somebody dismisses the notice. The same errors at the next sync leave it
	// dismissed; an error that was not there before opens it again, because
	// nobody has seen that one.
	dismissed := func() bool {
		t.Helper()
		return grCount(t, svc.db, `SELECT COUNT(*) FROM notices WHERE kind='git_sync_problems' AND subject='global' AND dismissed_at IS NOT NULL`) == 1
	}
	if _, err := svc.db.Exec(`UPDATE notices SET dismissed_at='t', dismissed_by='ops@example' WHERE kind='git_sync_problems' AND subject='global'`); err != nil {
		t.Fatal(err)
	}
	sync("a sync with the same errors, after a dismissal")
	if !dismissed() {
		t.Errorf("the dismissal was undone by a sync that reported nothing new")
	}
	gitCommitFile(t, repo, remote, "jobs/worse.yaml", bad, "another file that does not validate")
	sync("a sync with a new error")
	if dismissed() {
		t.Errorf("the notice is still dismissed although the sync reported an error nobody has seen")
	}
	if _, open, detail := grNotice(t, svc, "global"); !open || !strings.Contains(detail, "jobs/worse.yaml") {
		t.Errorf("the notice after a new error: open=%v %q", open, detail)
	}
	gitRemoveFile(t, repo, "jobs/worse.yaml", "remove it again")
	gitRemoveFile(t, repo, "jobs/broken.yaml", "and the file that does not parse")
	grBackdate(t, svc.db)
	sync("back to the one file that does not validate")
	rows = grProblems(t, svc, "global")

	// The same problems at the next sync are the same rows: first seen then,
	// last seen now.
	if _, err := svc.db.Exec(`UPDATE git_sync_problems SET first_seen = '2020-01-01T00:00:00Z', last_seen = '2020-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	before := grCount(t, svc.db, `SELECT COUNT(*) FROM git_sync_problems`)
	sync("second sync, nothing changed")
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM git_sync_problems`); n != before {
		t.Errorf("rows after an unchanged sync = %d, want the same %d", n, before)
	}
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM git_sync_problems WHERE first_seen = '2020-01-01T00:00:00Z' AND last_seen > '2020-01-01T00:00:00Z'`); n != before {
		t.Errorf("rows that kept their first sight and moved their last = %d, want all %d", n, before)
	}

	// The file is corrected: its error goes, the warnings stay, and the notice
	// resolves, since warnings alone hold none open.
	gitCommitFile(t, repo, remote, "jobs/bad.yaml", jobYAML("bad"), "correct the file")
	if r := sync("third sync"); r.Status != "success" {
		t.Fatalf("the sync after the correction is %q (%s), want success", r.Status, r.ErrorMessage)
	}
	rows = grProblems(t, svc, "global")
	if grHas(rows, "error ") {
		t.Errorf("an error row is left after the file was corrected:\n%s", strings.Join(rows, "\n"))
	}
	if !grHas(rows, "warning job jobs/bad.yaml:", "declares no scope") {
		t.Errorf("the corrected file's own warning is missing:\n%s", strings.Join(rows, "\n"))
	}
	if n := count(rows, "warning"); n != 3 {
		t.Errorf("warnings after the correction = %d, want the three jobs that declare no scope:\n%s", n, strings.Join(rows, "\n"))
	}
	if _, open, _ := grNotice(t, svc, "global"); open {
		t.Errorf("the notice is open after the correction, on warnings alone")
	}

	// Every job leaves the repository. That sync warns that a whole kind went;
	// the one after it has nothing to say, and the notice resolves.
	for _, f := range []string{"jobs/bad.yaml", "jobs/keep.yaml", "jobs/team/nested.yaml"} {
		gitRemoveFile(t, repo, f, "remove "+f)
	}
	grBackdate(t, svc.db)
	sync("fourth sync, the jobs removed")
	rows = grProblems(t, svc, "global")
	if len(rows) != 1 || !grHas(rows, "warning repository :", "fully pruned") {
		t.Errorf("after every job was removed, want the one warning that a kind was fully pruned:\n%s", strings.Join(rows, "\n"))
	}
	sync("fifth sync")
	if rows = grProblems(t, svc, "global"); len(rows) != 0 {
		t.Errorf("rows after a sync with nothing to say:\n%s", strings.Join(rows, "\n"))
	}
	if _, open, _ := grNotice(t, svc, "global"); open {
		t.Errorf("the notice is still open although the last sync reported nothing")
	}
}

// A problem's text carries none of the server's directories: not the clone's,
// and not the directory the clones are kept in, which git names when a clone
// cannot be made.
func TestGR3_AProblemsTextCarriesNoServerDirectory(t *testing.T) {
	s := &Service{cloneDir: "/var/lib/cronomicon/git-cache/repo-b"}
	for in, want := range map[string]string{
		"read /var/lib/cronomicon/git-cache/repo-b/jobs/x.yaml: no":     "read jobs/x.yaml: no",
		"clone into /var/lib/cronomicon/git-cache/repo-b failed":        "clone into the repository failed",
		"mkdir /var/lib/cronomicon/git-cache/.tmp-1: permission denied": "mkdir the clone cache/.tmp-1: permission denied",
		"nothing of the server's":                                       "nothing of the server's",
	} {
		if got := s.scrub(in); got != want {
			t.Errorf("scrub(%q) = %q, want %q", in, got, want)
		}
	}
	if got := (&Service{}).scrub("/a/b"); got != "/a/b" {
		t.Errorf("a Service with no clone changed a message: %q", got)
	}

	// The file an error is about, when the error says it only in its text.
	for in, want := range map[string][2]string{
		"read jobs/x.yaml: is a symbolic link":                          {"jobs/x.yaml", "is a symbolic link"},
		"parse workflows/w.yaml: yaml: line 3: did not find":            {"workflows/w.yaml", "yaml: line 3: did not find"},
		"inventory/web.ini:2: secret-bearing variable":                  {"inventory/web.ini", "line 2: secret-bearing variable"},
		"read /var/lib/cronomicon/git-cache/repo-b/inventory/a.ini: no": {"inventory/a.ini", "no"},
		"compute content hash: the script's file could not be read":     {"", ""},
		"read inventory dir: permission denied":                         {"", ""},
		"a sentence with no file in it":                                 {"", ""},
		"read /etc/passwd: no":                                          {"", ""},
	} {
		path, rest, ok := s.pathInMessage(in)
		if (want[0] != "") != ok || path != want[0] || rest != want[1] {
			t.Errorf("pathInMessage(%q) = %q, %q, %v; want %q, %q", in, path, rest, ok, want[0], want[1])
		}
	}
}

// A repository's problems are its own: another repository's sync neither adds
// to them nor removes them, and each has its notice, filed under its agency.
func TestGR3_EachRepositoryHasItsOwnProblems(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	const bad = "apiVersion: cronomicon.io/v2\nkind: Job\nmetadata:\n  name: bad\n"
	gitCommitFile(t, repoA, remoteA, "jobs/bad.yaml", bad, "a file that does not validate")
	a.SyncBlocking(context.Background(), "manual")

	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{
		// A warning of its own: a job in an agency's repository names a scope, so
		// it has no "declares no scope" to warn of.
		"jobs/other.yaml":    grJob("other", "echo other") + "  ssh_credential: no-such-key\n",
		"jobs/alsobad.yaml":  bad,
		".gitmodules":        "[submodule \"x\"]\n\tpath = scripts/x\n\turl = https://elsewhere.example/x.git\n",
		"schedules/odd.yaml": "apiVersion: cronomicon.io/v1\nkind: Schedule\nmetadata:\n  name: odd\nspec:\n  cron: \"0 3 1 1 *\"\n",
	}, "the second repository")
	b.SyncBlocking(context.Background(), "manual")

	rowsA, rowsB := grProblems(t, a, "global"), grProblems(t, a, "repo-b")
	if !grHas(rowsA, "error job jobs/bad.yaml:") || grHas(rowsA, "alsobad") || grHas(rowsA, "other") {
		t.Errorf("Global's rows:\n%s", strings.Join(rowsA, "\n"))
	}
	if !grHas(rowsB, "error job jobs/alsobad.yaml:") || !grHas(rowsB, "warning job jobs/other.yaml:") || grHas(rowsB, "jobs/bad.yaml") || grHas(rowsB, "keep") {
		t.Errorf("the second repository's rows:\n%s", strings.Join(rowsB, "\n"))
	}
	// What a sync says about the repository as a whole is a row too: an
	// agency's repository's submodules are not fetched.
	if !grHas(rowsB, "warning repository .gitmodules:", "submodules") {
		t.Errorf("no row for the submodules that are not fetched:\n%s", strings.Join(rowsB, "\n"))
	}
	if agency, open, _ := grNotice(t, a, "global"); !open || agency != "global" {
		t.Errorf("Global's notice: open=%v agency=%q", open, agency)
	}
	if agency, open, detail := grNotice(t, a, "repo-b"); !open || agency != "ag-b" || !strings.Contains(detail, "jobs/alsobad.yaml") {
		t.Errorf("the second repository's notice: open=%v agency=%q %q, want it open under its agency, naming its file", open, agency, detail)
	}

	// Global's file is corrected and Global syncs: the second repository's rows
	// and notice are as they were.
	gitCommitFile(t, repoA, remoteA, "jobs/bad.yaml", jobYAML("bad"), "correct")
	a.SyncBlocking(context.Background(), "manual")
	if got := grProblems(t, a, "repo-b"); strings.Join(got, "\n") != strings.Join(rowsB, "\n") {
		t.Errorf("the second repository's rows changed on Global's sync:\n%s\nwere:\n%s", strings.Join(got, "\n"), strings.Join(rowsB, "\n"))
	}
	if grHas(grProblems(t, a, "global"), "error ") {
		t.Errorf("Global's error row is left after its file was corrected")
	}
	if _, open, _ := grNotice(t, a, "repo-b"); !open {
		t.Errorf("the second repository's notice was resolved by Global's sync")
	}
}

// Two files of one repository that define the same name are REPORTED, each
// later one against the first, and the sync is partial. One of them is still
// used (the one read last) and no prune is switched off: both would change
// what a repository that has such a pair runs today.
//
// Until Phase R3 (present defect 19;
// TestGR0_ADuplicateNameInOneRepositoryWinsInSilence pinned it) the sync was a
// clean success and said nothing. Migration 1050's header says the sync
// validator refuses a duplicate; only `cronomicon validate` does.
func TestGR3_ADuplicateNameInOneRepositoryIsReported(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	grCommitFiles(t, repo, remote, map[string]string{
		"jobs/a-first.yaml":      grJob("twice", "echo from-a-first"),
		"jobs/z-last.yaml":       grJob("twice", "echo from-z-last"),
		"schedules/a-first.yaml": grSchedule("0 3 1 1 *"),
		"schedules/z-last.yaml":  grSchedule("0 4 2 2 *"),
	}, "two jobs and two schedules of one name each")
	res := svc.SyncBlocking(context.Background(), "manual")
	if res.Status != "partial" || len(res.Errors) != 2 {
		t.Fatalf("the sync is %q with %d error(s) (%s); want partial, one error per ignored file", res.Status, len(res.Errors), res.ErrorMessage)
	}
	rows := grProblems(t, svc, "global")
	for _, want := range [][]string{
		{"error job jobs/z-last.yaml:", `the job "twice"`, "also in jobs/a-first.yaml"},
		{"error schedule schedules/z-last.yaml:", `the schedule "yearly"`, "also in schedules/a-first.yaml"},
	} {
		if !grHas(rows, want...) {
			t.Errorf("no row saying %v:\n%s", want, strings.Join(rows, "\n"))
		}
	}
	// What is used is what was used: one row of each, from the file read last.
	if n := jobCount(t, svc.db, "twice"); n != 1 {
		t.Errorf("jobs named twice = %d, want 1", n)
	}
	if got := grString(t, svc.db, `SELECT command FROM jobs WHERE source='git' AND name='twice'`); got != "echo from-z-last" {
		t.Errorf("the job's command = %q, want the one read last", got)
	}
	// And the kind is still pruned: a job that leaves the repository goes,
	// although two other files share a name.
	gitRemoveFile(t, repo, "jobs/keep.yaml", "remove keep")
	grBackdate(t, svc.db)
	svc.SyncBlocking(context.Background(), "manual")
	if jobCount(t, svc.db, "keep") != 0 {
		t.Errorf("the removed job was not pruned: a duplicate name switched the prune off")
	}
}

// A repository that cannot be fetched says so in a row and in its notice,
// without the connection's URL, and what was known of its files is kept. The
// row goes at the first sync that fetches.
func TestGR3_ARepositoryThatCannotBeFetchedSaysSo(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	ctx := context.Background()
	const bad = "apiVersion: cronomicon.io/v2\nkind: Job\nmetadata:\n  name: bad\n"
	gitCommitFile(t, repo, remote, "jobs/bad.yaml", bad, "a file that does not validate")
	svc.SyncBlocking(ctx, "manual")
	if !grHas(grProblems(t, svc, "global"), "error job jobs/bad.yaml:") {
		t.Fatalf("no row for the file that does not validate")
	}

	good := svc.repoURL
	svc.repoURL = t.TempDir() + "/secret-token@nowhere" // not a repository
	// A clone that does not exist yet has to be made from the URL; with one
	// that exists the fetch goes to the URL too (GR-12).
	if r := svc.SyncBlocking(ctx, "manual"); r.Status != "failed" {
		t.Fatalf("a sync of a URL that is not a repository is %q, want failed", r.Status)
	}
	rows := grProblems(t, svc, "global")
	if !grHas(rows, "error repository :", "the repository could not be fetched") {
		t.Errorf("no row saying the repository could not be fetched:\n%s", strings.Join(rows, "\n"))
	}
	if !grHas(rows, "error job jobs/bad.yaml:") {
		t.Errorf("what was known of the files was dropped when the fetch failed:\n%s", strings.Join(rows, "\n"))
	}
	_, open, detail := grNotice(t, svc, "global")
	if !open || !strings.Contains(detail, "could not be fetched") {
		t.Errorf("the notice after a failed fetch: open=%v %q", open, detail)
	}
	for _, text := range append(rows, detail) {
		if strings.Contains(text, "secret-token") || strings.Contains(text, svc.repoURL) {
			t.Errorf("the connection's URL is in a row or the notice: %s", text)
		}
	}
	// A second failure is the same row, not a second one.
	svc.SyncBlocking(ctx, "manual")
	if n := grCount(t, svc.db, `SELECT COUNT(*) FROM git_sync_problems WHERE message LIKE 'the repository could not be fetched:%'`); n != 1 {
		t.Errorf("rows for the failed fetch after two failures = %d, want 1", n)
	}

	// The connection is put right: the row goes.
	svc.repoURL = good
	if r := svc.SyncBlocking(ctx, "manual"); r.Status == "failed" {
		t.Fatalf("the sync after the URL was put right: %s", r.ErrorMessage)
	}
	if rows := grProblems(t, svc, "global"); grHas(rows, "could not be fetched") {
		t.Errorf("the failed fetch's row is left after a sync that fetched:\n%s", strings.Join(rows, "\n"))
	}

	// A repository with no URL is not a repository that could not be fetched.
	fresh, _, _ := newSyncFixture(t)
	fresh.repoURL = ""
	fresh.SyncBlocking(ctx, "manual")
	if rows := grProblems(t, fresh, "global"); len(rows) != 0 {
		t.Errorf("a connection with no URL has problem rows:\n%s", strings.Join(rows, "\n"))
	}

	// And one whose URL is taken away has nothing left to say: no sync will
	// come to clear what the last one found.
	if !grHas(grProblems(t, svc, "global"), "error job jobs/bad.yaml:") {
		t.Fatalf("the file's error row is not there before the URL is removed")
	}
	if _, open, _ := grNotice(t, svc, "global"); !open {
		t.Fatalf("the notice is not open before the URL is removed")
	}
	svc.repoURL = ""
	svc.SyncBlocking(ctx, "manual")
	if rows := grProblems(t, svc, "global"); len(rows) != 0 {
		t.Errorf("rows left for a repository whose connection has no URL any more:\n%s", strings.Join(rows, "\n"))
	}
	if _, open, _ := grNotice(t, svc, "global"); open {
		t.Errorf("the notice is still open for a repository whose connection has no URL any more")
	}
}

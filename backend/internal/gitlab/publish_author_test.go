package gitlab

import (
	"context"
	"testing"
)

// A publish commit's author is the bot name and email of the repository's
// connection (2.4.0; present defect 20, the owner's decision of 2026-10-09).
// The connection's form has always shown and stored them, and nothing read
// them: every publish commit was written as "Cronomicon Scheduler
// <cronomicon-bot@cronomicon.internal>". Each repository's commits carry its
// own connection's, and a connection that names none gives the columns'
// defaults.
func TestAPublishCommitIsAuthoredByTheConnectionsBot(t *testing.T) {
	svc, bare, branch := bareFixture(t)
	ctx := context.Background()
	base := svc.SyncBlocking(ctx, "manual")
	if base.Status == "failed" {
		t.Fatalf("sync: %s", base.ErrorMessage)
	}
	head := base.SHA
	published := func(what, file string) string {
		t.Helper()
		res, err := svc.Publish(ctx, PublishRequest{FilePath: "jobs/" + file + ".yaml", Content: grScopedJob(file, fixtureScope)}, head, "a@example.com")
		if err != nil {
			t.Fatalf("%s: publish: %v", what, err)
		}
		if r := svc.SyncBlocking(ctx, "manual"); r.Status == "failed" || r.SHA != res.CommitSHA {
			t.Fatalf("%s: the sync after the publish: %+v", what, r)
		}
		head = res.CommitSHA
		return sysGit(t, bare, "log", "--format=%an <%ae>", "-1", "refs/heads/"+branch)
	}
	set := func(repo, name, email string) {
		t.Helper()
		if _, err := svc.db.Exec(`INSERT INTO git_repos (id, agency_id, url, branch, bot_name, bot_email) VALUES (?, ?, 'u', ?, ?, ?)
		                          ON CONFLICT(id) DO UPDATE SET bot_name = excluded.bot_name, bot_email = excluded.bot_email`,
			repo, repo, branch, name, email); err != nil {
			t.Fatalf("set the connection's bot: %v", err)
		}
	}

	// As the connection is first found: what its columns default to.
	if got, want := published("the defaults", "p1"), "cronomicon-bot <cronomicon-bot@cronomicon.io>"; got != want {
		t.Errorf("with nothing set the author is %q, want %q", got, want)
	}
	// What the connection's form stores.
	set(fixtureRepo, "Finance Scheduler", "fin-bot@example.com")
	if got, want := published("the connection's bot", "p2"), "Finance Scheduler <fin-bot@example.com>"; got != want {
		t.Errorf("the author is %q, want the connection's bot, %q", got, want)
	}
	// Emptied, or blank: the defaults, never an empty author.
	set(fixtureRepo, "  ", "")
	if got, want := published("an emptied bot", "p3"), "cronomicon-bot <cronomicon-bot@cronomicon.io>"; got != want {
		t.Errorf("with the bot emptied the author is %q, want the defaults, %q", got, want)
	}
	// Another repository's Service reads its own connection's, not the first's.
	set(fixtureRepo, "First Bot", "first-bot@example.com")
	set("repo-b", "Agency B Bot", "b-bot@example.com")
	svc.repoID = "repo-b"
	res, err := svc.Publish(ctx, PublishRequest{FilePath: "jobs/p4.yaml", Content: grScopedJob("p4", fixtureScope)}, head, "a@example.com")
	if err != nil {
		t.Fatalf("another repository's publish: %v", err)
	}
	if got, want := sysGit(t, bare, "log", "--format=%an <%ae>", "-1", res.CommitSHA), "Agency B Bot <b-bot@example.com>"; got != want {
		t.Errorf("another repository's commit is authored by %q, want its own connection's bot, %q", got, want)
	}
}

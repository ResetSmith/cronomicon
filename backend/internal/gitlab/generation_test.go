package gitlab

import (
	"context"
	"testing"
)

// Any repository's sync advances the definitions generation, in its own
// transaction, and hands the new value to the scheduler's hook (GR-29).
//
// Until Phase R3 the hook was handed the sync's commit and the scheduler
// compared it with the commit of the one repository's last sync. With a
// repository per agency there is no one commit: an agency's repository syncs
// at commits of its own, while Global's has not moved.
func TestGR3_AnyRepositorysSyncAdvancesTheGeneration(t *testing.T) {
	a, _, _ := newSyncFixture(t)
	var got []string
	hook := func(_ context.Context, generation string) { got = append(got, generation) }
	a.SetOnSyncComplete(hook)
	generation := func() string { return grString(t, a.db, `SELECT n FROM definitions_generation WHERE id = 1`) }
	start := generation()

	grSync(t, a, "Global's repository")
	afterA := generation()
	if afterA == start || len(got) != 1 || got[0] != afterA {
		t.Fatalf("after Global's sync: generation %q (was %q), the hook was handed %v", afterA, start, got)
	}
	globalSHA := grString(t, a.db, `SELECT last_sha FROM git_repos WHERE id = 'global'`)

	b, repoB, remoteB := grSecondRepo(t, a)
	b.SetOnSyncComplete(hook)
	grCommitFiles(t, repoB, remoteB, map[string]string{"jobs/other.yaml": grJob("other", "echo other")}, "the second repository")
	grSync(t, b, "the second repository")
	afterB := generation()
	if afterB == afterA || len(got) != 2 || got[1] != afterB {
		t.Errorf("after the second repository's sync: generation %q (was %q), the hook was handed %v", afterB, afterA, got)
	}
	if now := grString(t, a.db, `SELECT last_sha FROM git_repos WHERE id = 'global'`); now != globalSHA {
		t.Errorf("Global's last commit moved from %q to %q on another repository's sync", globalSHA, now)
	}
	// A sync that changes nothing still advances it: the scheduler's other half,
	// the fingerprint, is what decides whether there is anything to reload.
	grSync(t, b, "the second repository again")
	if again := generation(); again == afterB || len(got) != 3 || got[2] != again {
		t.Errorf("after an unchanged sync: generation %q (was %q), the hook was handed %v", again, afterB, got)
	}
}

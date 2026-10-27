package notices_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// An agency with no repository connected is reported to it (§0a, IN-2; Phase
// I3; question 39). It clears when the repository is connected. The built-in
// agency is not asked since 2.5.3 (CA-6): it has no repository, connected or
// not; until then its unconfigured row was reported as Global's.
func TestI3_AnAgencyWithNoRepositoryIsReported(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	mustExec(t, pool, `INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-tax', 'ag-tax', 'https://git.example.com/tax.git', 'main')`)
	mustExec(t, pool, `INSERT OR IGNORE INTO git_repos (id, agency_id, url, branch) VALUES ('global', 'global', '', 'main')`)
	mustExec(t, pool, `UPDATE git_repos SET url = '' WHERE id = 'global'`)
	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindAgencyNoRepository)
	}

	got := check()
	if len(got) != 1 || got["ag-fin"].AgencyID != "ag-fin" {
		t.Fatalf("agency_no_repository = %v, want Finance's alone (not the built-in agency's)", got)
	}

	mustExec(t, pool, `INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag-fin', 'https://git.example.com/fin.git', 'main')`)
	if got = check(); len(got) != 0 {
		t.Errorf("agency_no_repository after Finance connected = %v, want none", got)
	}
}

// 2.5.3 (question 207): settings this version no longer reads are named in a
// notice for global administrators while the server runs with them, and it
// clears when it starts without them.
func TestCA2EnvironmentSettingsThatAreNoLongerReadAreReported(t *testing.T) {
	pool := open(t)
	t.Cleanup(func() { notices.SetIgnoredEnvironmentSettings(nil) })
	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindEnvironmentSettingsIgnored)
	}

	notices.SetIgnoredEnvironmentSettings(nil)
	if got := check(); len(got) != 0 {
		t.Fatalf("with none set: %v, want no notice", got)
	}
	notices.SetIgnoredEnvironmentSettings([]string{"CRONOMICON_GITLAB_BASE_URL", "CRONOMICON_RUNNER_BOOTSTRAP_TOKEN"})
	got := check()
	n, ok := got["environment"]
	if !ok || len(got) != 1 || n.AgencyID != "global" {
		t.Fatalf("with two set: %v, want one notice, the built-in agency's", got)
	}
	for _, want := range []string{"CRONOMICON_GITLAB_BASE_URL", "CRONOMICON_RUNNER_BOOTSTRAP_TOKEN",
		"registration token minted in their agency", "removes that leftover"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the notice does not say %q: %s", want, n.Detail)
		}
	}
	// What it says follows what is set: the agents' advice only for the token.
	notices.SetIgnoredEnvironmentSettings([]string{"CRONOMICON_GITLAB_BASE_URL"})
	if n := check()["environment"]; strings.Contains(n.Detail, "registration token") || !strings.Contains(n.Detail, "removes that leftover") {
		t.Errorf("with the repository's URL alone set, the notice says: %s", n.Detail)
	}
	notices.SetIgnoredEnvironmentSettings(nil)
	if got := check(); len(got) != 0 {
		t.Errorf("after a start without them: %v, want it cleared", got)
	}
}

// 2.5.3 (CA-9): an upgraded installation whose local runner was on finds it
// unassigned and off; a notice for global administrators says so, and clears
// when it is given to an agency or turned off (the reader is settings', given
// at start: stubbed here).
func TestCA3AnUnassignedLocalRunnerThatWasOnIsReported(t *testing.T) {
	pool := open(t)
	state := true
	notices.SetLocalRunnerUnassignedCheck(func(context.Context, *sql.DB) (bool, error) { return state, nil })
	t.Cleanup(func() { notices.SetLocalRunnerUnassignedCheck(nil) })
	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindLocalRunnerUnassigned)
	}
	got := check()
	if n, ok := got["local-runner"]; !ok || len(got) != 1 || n.AgencyID != "global" || !strings.Contains(n.Detail, "approved") {
		t.Fatalf("an unassigned local runner that was on: %v, want one notice, the built-in agency's, saying keys are approved first", got)
	}
	state = false
	if got := check(); len(got) != 0 {
		t.Errorf("once given to an agency or turned off: %v, want cleared", got)
	}
}

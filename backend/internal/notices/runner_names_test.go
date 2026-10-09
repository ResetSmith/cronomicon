package notices_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// Two live agents of one agency under one name are reported, to that agency,
// and the notice resolves when one of them is renamed. What looks the same and
// is not is left alone: the old row of an agent that enrolled again, a
// same-named agent that has gone offline, and another agency's runner of that
// name.
func TestRunnerNameSharedIsTwoLiveAgentsOfOneAgency(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	// id, name, owner, status, registered, last seen
	agent := func(id, name, owner, status, registered, seen string) {
		mustExec(t, pool, `
			INSERT INTO runners (id, name, status, owner_agency, registered_at, created_at, last_seen_at, last_client_ip)
			VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), '10.0.0.' || substr(?, -1))`,
			id, name, status, owner, registered, registered, seen, id)
	}
	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindRunnerNameShared)
	}

	// Two agents of Finance, both called runner-01, both polling today.
	agent("r-a1", "runner-01", "ag-fin", "online", "2026-10-01T00:00:00Z", "2026-10-08T12:00:00Z")
	agent("r-a2", "runner-01", "ag-fin", "online", "2026-10-05T00:00:00Z", "2026-10-08T12:00:05Z")
	// The same name in ANOTHER agency: that agency's own business.
	agent("r-b1", "runner-01", "ag-tax", "online", "2026-10-02T00:00:00Z", "2026-10-08T12:00:00Z")
	// An agent that enrolled again: its old row was last seen BEFORE the new
	// one registered. One agent, two rows; Restore placement is the tool.
	agent("r-c1", "runner-02", "ag-fin", "online", "2026-10-01T00:00:00Z", "2026-10-06T09:00:00Z")
	agent("r-c2", "runner-02", "ag-fin", "online", "2026-10-06T09:00:30Z", "2026-10-08T12:00:00Z")
	// A same-named agent that is offline is not polling beside the other.
	agent("r-d1", "runner-03", "ag-fin", "online", "2026-10-01T00:00:00Z", "2026-10-08T12:00:00Z")
	agent("r-d2", "runner-03", "ag-fin", "offline", "2026-10-02T00:00:00Z", "2026-10-07T12:00:00Z")
	// One that has registered and never polled has not shown it is alive.
	agent("r-e1", "runner-04", "ag-fin", "online", "2026-10-01T00:00:00Z", "2026-10-08T12:00:00Z")
	agent("r-e2", "runner-04", "ag-fin", "online", "2026-10-08T11:59:00Z", "")

	got := check()
	if len(got) != 1 {
		t.Fatalf("notices = %+v; want exactly one, for Finance's two live runner-01 agents", got)
	}
	n, ok := got["ag-fin:runner-01"]
	if !ok {
		t.Fatalf("no notice for ag-fin:runner-01: %+v", got)
	}
	if n.AgencyID != "ag-fin" {
		t.Errorf("filed under %q; the agency that owns the runners reads and settles it", n.AgencyID)
	}
	for _, want := range []string{"2 runner agents owned by Finance", "the name runner-01", "id r-a1", "id r-a2", "last seen from 10.0.0.1", "CRONOMICON_RUNNER_NAME"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the notice must contain %q: %s", want, n.Detail)
		}
	}
	if strings.Contains(n.Detail, "r-b1") {
		t.Errorf("the notice names another agency's runner: %s", n.Detail)
	}

	// The remedy clears it: one of the two re-declares under a name of its own.
	mustExec(t, pool, `UPDATE runners SET name = 'runner-01b' WHERE id = 'r-a2'`)
	if got := check(); len(got) != 0 {
		t.Errorf("after one agent was renamed: %+v; want none", got)
	}

	// A third joining two is one notice that counts three.
	mustExec(t, pool, `UPDATE runners SET name = 'runner-01' WHERE id = 'r-a2'`)
	agent("r-a3", "runner-01", "ag-fin", "draining", "2026-10-07T00:00:00Z", "2026-10-08T12:00:10Z")
	got = check()
	if n := got["ag-fin:runner-01"]; len(got) != 1 || !strings.Contains(n.Detail, "3 runner agents") || !strings.Contains(n.Detail, "id r-a3") {
		t.Errorf("three live agents under one name: %+v", got)
	}
}

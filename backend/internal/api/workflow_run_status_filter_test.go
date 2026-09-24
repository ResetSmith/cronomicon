package api_test

import (
	"testing"
	"time"
)

// F-1/F-2 (20260728-visual-update-followups.md) — the /workflow-runs status
// filter has to speak the DISPLAY vocabulary, because that is what the rows it
// returns are labelled with. Two of those labels are not the raw column:
//
//   - 'cancelled' is not a status at all. Migration 260 keeps the CHECK-legal
//     terminal ('failure') and carries cancellation in an additive flag, so the
//     old filter matched status='cancelled' and returned nothing, while
//     status=danger returned soft-cancelled runs whose own Status column read
//     Cancelled.
//   - 'running' groups queued, because the UI folds the pair into one word.
//
// One case per branch: "the setting now does something" is exactly the claim
// that regresses silently.
func TestWorkflowRunsStatusFilterDisplayVocabulary(t *testing.T) {
	ts, db := newTestServer(t)
	now := time.Now().UTC().Format(time.RFC3339)

	// (id, raw status, cancelled flag) — two of these display as something the
	// raw column does not say.
	seed := []struct {
		id        string
		status    string
		cancelled int
	}{
		{"wr-failure", "failure", 0},
		{"wr-killed", "killed", 0},
		{"wr-cancelled", "failure", 1}, // raw failure, displays 'cancelled'
		{"wr-success", "success", 0},
		{"wr-running", "running", 0},
		{"wr-queued", "queued", 0},
		{"wr-skipped", "skipped", 0},
		{"wr-warning", "warning", 0},
	}
	for _, s := range seed {
		if _, err := db.Exec(`
			INSERT INTO workflow_runs(id, workflow_id, workflow_name, status, triggered_by, trigger_kind, created_at, cancelled)
			VALUES (?, 1, 'wf', ?, 'test', 'manual', ?, ?)`,
			s.id, s.status, now, s.cancelled); err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
	}
	client := devLoginClient(t, ts)

	type env struct {
		TotalItems int `json:"totalItems"`
		Items      []struct {
			TraceID string `json:"traceId"`
			Status  string `json:"status"`
		} `json:"items"`
	}

	cases := []struct {
		filter  string
		want    []string // trace IDs, in any order
		display []string // display statuses a returned row may carry
	}{
		{"danger", []string{"wr-failure", "wr-killed"}, []string{"danger"}}, // NOT the cancelled one
		{"cancelled", []string{"wr-cancelled"}, []string{"cancelled"}},
		// queued folds into Running in the UI's vocabulary, so the filter returns
		// both; the wire keeps them distinct, and statusLabel is what unifies them.
		{"running", []string{"wr-running", "wr-queued"}, []string{"running", "queued"}},
		{"success", []string{"wr-success"}, []string{"success"}},
		{"skipped", []string{"wr-skipped"}, []string{"skipped"}},
		{"warning", []string{"wr-warning"}, []string{"warning"}},
	}
	for _, tc := range cases {
		var e env
		getJSON(t, client, ts.URL+"/api/v1/workflow-runs?status="+tc.filter, &e)
		got := map[string]string{}
		for _, it := range e.Items {
			got[it.TraceID] = it.Status
		}
		if e.TotalItems != len(tc.want) || len(got) != len(tc.want) {
			t.Errorf("status=%s: totalItems = %d, items = %d, want %d (%v)", tc.filter, e.TotalItems, len(got), len(tc.want), tc.want)
		}
		for _, id := range tc.want {
			if _, ok := got[id]; !ok {
				t.Errorf("status=%s: missing %s (got %v)", tc.filter, id, got)
			}
		}
		// Every row returned must display a status the chosen filter covers — the
		// property the whole filter exists to hold. A soft-cancelled run failing
		// this under status=danger is the original defect.
		for id, st := range got {
			ok := false
			for _, allowed := range tc.display {
				if st == allowed {
					ok = true
				}
			}
			if !ok {
				t.Errorf("status=%s: %s came back displaying %q, want one of %v", tc.filter, id, st, tc.display)
			}
		}
	}
}

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Run notes and acknowledgements (2.5.2 OP3): an append-only record on one run,
// shared by everyone who can read the run, in its agency's change log.

type notesBody struct {
	Acknowledged *struct {
		By       string `json:"by"`
		Relation string `json:"relation"`
	} `json:"acknowledged"`
	Notes []struct {
		Kind     string `json:"kind"`
		Body     string `json:"body"`
		Author   string `json:"author"`
		Relation string `json:"relation"`
	} `json:"notes"`
}

func notesCall(t *testing.T, h http.Handler, method, path, groups, body string) (int, notesBody) {
	t.Helper()
	rec := gateReq(t, h, method, path, groups, body)
	var nb notesBody
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &nb); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	return rec.Code, nb
}

func TestOP3ARunsRecordIsAppendOnlyAndItsStateIsTheLatestAcknowledgement(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	const p = "/api/v1/runs/r-fin/notes"

	for _, bad := range []string{`{"kind":"note","body":"  "}`, `{"kind":"pinned","body":"x"}`, `{"kind":"note","body":"` + strings.Repeat("x", 1001) + `"}`} {
		if code, _ := notesCall(t, h, http.MethodPost, p, gFinAdmin, bad); code != http.StatusUnprocessableEntity {
			t.Errorf("%.40s… = %d, want 422", bad, code)
		}
	}

	code, nb := notesCall(t, h, http.MethodPost, p, gFinAdmin, `{"kind":"acknowledged","body":"Looking at it."}`)
	if code != http.StatusOK || nb.Acknowledged == nil || nb.Acknowledged.Relation != "member" || len(nb.Notes) != 1 {
		t.Fatalf("acknowledge: %d %+v", code, nb)
	}
	// A repeat acknowledgement records nothing.
	if _, nb = notesCall(t, h, http.MethodPost, p, gRoot, `{"kind":"acknowledged","body":"me too"}`); len(nb.Notes) != 1 {
		t.Errorf("a repeat acknowledgement added an entry: %+v", nb)
	}
	// The central team (a viewer of everything) adds to the same thread.
	if _, nb = notesCall(t, h, http.MethodPost, p, gAllViewer, `{"kind":"note","body":"Called FIN on-call."}`); len(nb.Notes) != 2 || nb.Notes[1].Relation != "outside" {
		t.Errorf("the viewer's note: %+v", nb)
	}
	if _, nb = notesCall(t, h, http.MethodPost, p, gRoot, `{"kind":"unacknowledged"}`); nb.Acknowledged != nil || len(nb.Notes) != 3 || nb.Notes[2].Relation != "global_admin" {
		t.Errorf("taking it back: %+v", nb)
	}
	if _, nb = notesCall(t, h, http.MethodPost, p, gRoot, `{"kind":"unacknowledged"}`); len(nb.Notes) != 3 {
		t.Errorf("taking back what is not there added an entry: %+v", nb)
	}
	code, nb = notesCall(t, h, http.MethodGet, p, gFinViewer, "")
	if code != http.StatusOK || len(nb.Notes) != 3 || nb.Notes[0].Kind != "acknowledged" || nb.Notes[0].Body != "Looking at it." || nb.Notes[2].Kind != "unacknowledged" {
		t.Errorf("read back: %d %+v", code, nb)
	}

	// In FIN's change log, one row each, the text kept.
	var rows int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM change_log WHERE category = 'Runs' AND agency_id = 'ag:FIN'`).Scan(&rows)
	if rows != 3 {
		t.Errorf("FIN's Runs change-log rows = %d, want 3", rows)
	}
	var details string
	_ = pool.QueryRow(`SELECT details FROM change_log WHERE category = 'Runs' AND action = 'Note added'`).Scan(&details)
	if !strings.Contains(details, "Called FIN on-call.") || !strings.Contains(details, "r-fin") {
		t.Errorf("the note's change-log details %q", details)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM activity WHERE category = 'Runs' AND agency_id = 'ag:FIN'`).Scan(&rows)
	if rows != 3 {
		t.Errorf("FIN's Runs activity rows = %d, want 3", rows)
	}
}

func TestOP3AnotherAgencysRunIsNotThere(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/runs/r-fin/notes", ""},
		{http.MethodPost, "/api/v1/runs/r-fin/notes", `{"kind":"acknowledged"}`},
		{http.MethodGet, "/api/v1/workflow-runs/wr-fin/notes", ""},
		{http.MethodPost, "/api/v1/workflow-runs/wr-fin/notes", `{"kind":"note","body":"x"}`},
		{http.MethodGet, "/api/v1/runs/no-such-run/notes", ""},
	} {
		if code, _ := notesCall(t, h, c.method, c.path, gTaxAdmin, c.body); code != http.StatusNotFound {
			t.Errorf("%s %s as TAX's admin = %d, want 404", c.method, c.path, code)
		}
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM run_notes`).Scan(&n)
	if n != 0 {
		t.Errorf("%d entries written by a refused caller", n)
	}
}

func TestOP3AWorkflowRunHasItsOwnRecord(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	code, nb := notesCall(t, h, http.MethodPost, "/api/v1/workflow-runs/wr-fin/notes", gFinAdmin, `{"kind":"acknowledged","body":"Month-end, rerunning."}`)
	if code != http.StatusOK || nb.Acknowledged == nil || len(nb.Notes) != 1 {
		t.Fatalf("workflow run: %d %+v", code, nb)
	}
	if _, nb = notesCall(t, h, http.MethodGet, "/api/v1/runs/r-fin/notes", gFinAdmin, ""); len(nb.Notes) != 0 || nb.Acknowledged != nil {
		t.Errorf("the job run borrowed the workflow run's record: %+v", nb)
	}
	var target string
	_ = pool.QueryRow(`SELECT target FROM change_log WHERE category = 'Runs'`).Scan(&target)
	if target != "s0-fin-flow" {
		t.Errorf("change-log target %q, want the workflow's name", target)
	}
}

func TestRunNotesGoWithTheirRun(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	notesCall(t, h, http.MethodPost, "/api/v1/runs/r-fin/notes", gFinAdmin, `{"kind":"note","body":"x"}`)
	notesCall(t, h, http.MethodPost, "/api/v1/workflow-runs/wr-fin/notes", gFinAdmin, `{"kind":"note","body":"y"}`)
	notesCall(t, h, http.MethodPost, "/api/v1/runs/r-tax/notes", gTaxAdmin, `{"kind":"note","body":"z"}`)
	mustExec(t, pool)(`DELETE FROM runs WHERE id = 'r-fin'`)
	mustExec(t, pool)(`DELETE FROM workflow_runs WHERE id = 'wr-fin'`)
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM run_notes`).Scan(&n)
	if n != 1 {
		t.Errorf("entries left = %d, want 1 (TAX's): pruning a run takes its notes (OP-16)", n)
	}
}

func TestOP3TheListsAndTheOverviewCarryTheRecordInBrief(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	seedOverview(t, h, exec)
	// TAX's job failed before, and that failure was acknowledged.
	earlier := time.Now().UTC().Add(-26 * time.Hour).Format(time.RFC3339)
	exec(`INSERT INTO runs (id, job_name, job_uid, run_type, status, triggered_by, trigger_kind, created_at, agencies_json)
	      VALUES ('r-tax-prev', 's0-tax-job', 'j-tax', 'bash', 'failure', 'x', 'manual', ?, '["TAX"]')`, earlier)
	notesCall(t, h, http.MethodPost, "/api/v1/runs/r-tax-prev/notes", gTaxAdmin, `{"kind":"acknowledged","body":"Bank file late."}`)
	notesCall(t, h, http.MethodPost, "/api/v1/runs/r-tax-fail/notes", gAllViewer, `{"kind":"acknowledged","body":"Looking."}`)
	notesCall(t, h, http.MethodPost, "/api/v1/runs/r-tax-fail/notes", gAllViewer, `{"kind":"note","body":"Called TAX desk."}`)

	rec := gateReq(t, h, http.MethodGet, "/api/v1/runs?pageSize=200", gRoot, "")
	var page struct {
		Items []struct {
			TraceID string `json:"traceId"`
			Notes   *struct {
				Acknowledged bool `json:"acknowledged"`
				Count        int  `json:"count"`
			} `json:"notes"`
		} `json:"items"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &page) != nil {
		t.Fatalf("runs: %d %s", rec.Code, rec.Body)
	}
	for _, it := range page.Items {
		switch it.TraceID {
		case "r-tax-fail":
			if it.Notes == nil || !it.Notes.Acknowledged || it.Notes.Count != 2 {
				t.Errorf("r-tax-fail notes %+v, want acknowledged with 2", it.Notes)
			}
		case "r-fin-lost":
			if it.Notes != nil {
				t.Errorf("r-fin-lost has no entries but carries %+v", it.Notes)
			}
		}
	}

	rec = gateReq(t, h, http.MethodGet, "/api/v1/overview", gAllViewer, "")
	var ov struct {
		Attention []struct {
			Kind         string `json:"kind"`
			Name         string `json:"name"`
			Acknowledged *struct {
				By       string `json:"by"`
				Relation string `json:"relation"`
			} `json:"acknowledged"`
			LatestNote  string `json:"latestNote"`
			NoteCount   int    `json:"noteCount"`
			PreviousAck *struct {
				By    string `json:"by"`
				RunAt string `json:"runAt"`
			} `json:"previousAck"`
		} `json:"attention"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &ov) != nil {
		t.Fatalf("overview: %d %s", rec.Code, rec.Body)
	}
	seen := 0
	for _, it := range ov.Attention {
		switch it.Kind + " " + it.Name {
		case "job_failed s0-tax-job":
			seen++
			if it.Acknowledged == nil || it.Acknowledged.Relation != "outside" || it.LatestNote != "Called TAX desk." || it.NoteCount != 2 {
				t.Errorf("TAX's item: %+v", it)
			}
			if it.PreviousAck == nil || !strings.HasPrefix(it.PreviousAck.By, "tax-admins") || it.PreviousAck.RunAt != earlier {
				t.Errorf("TAX's previous failure: %+v", it.PreviousAck)
			}
		case "run_lost s0-fin-job":
			seen++
			if it.Acknowledged != nil || it.NoteCount != 0 || it.PreviousAck != nil {
				t.Errorf("FIN's unacknowledged item: %+v", it)
			}
		}
	}
	if seen != 2 {
		t.Errorf("saw %d of the two items (all: %+v)", seen, ov.Attention)
	}
}

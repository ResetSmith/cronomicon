package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// A failed workflow step in Needs attention (2.5.2 OP7, defect 100). A step's
// run is an ordinary job run that records its workflow run, so the job's
// latest failed run can be a step of a workflow run that failed too.

// seedFailedSteps: TAX's workflow run failed at its step (s0-tax-job's latest
// run); FIN's workflow run got past its failed step (continueOnError) and
// succeeded, so its step (s0-fin-job's latest run) is the only failure.
func seedFailedSteps(t *testing.T, exec func(string, ...any)) {
	t.Helper()
	at := time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339)
	exec(`INSERT INTO workflow_runs (id, workflow_name, workflow_source, workflow_uid, status, triggered_by, trigger_kind, created_at) VALUES
	        ('wr-tax-step', 's0-tax-flow', 'cronomicon', 'w-tax', 'failure', 'x', 'manual', ?),
	        ('wr-fin-past', 's0-fin-flow', 'cronomicon', 'w-fin', 'success', 'x', 'workflow', ?)`, at, at)
	exec(`INSERT INTO runs (id, job_name, job_uid, run_type, status, triggered_by, trigger_kind, workflow_run_id, created_at, agencies_json) VALUES
	        ('r-tax-step', 's0-tax-job', 'j-tax', 'bash', 'failure', 'x', 'workflow', 'wr-tax-step', ?, '["TAX"]'),
	        ('r-fin-step', 's0-fin-job', 'j-fin', 'bash', 'failure', 'x', 'workflow', 'wr-fin-past', ?, '["FIN"]')`, at, at)
}

type op7Attention struct {
	Attention []struct {
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		TraceID string `json:"traceId"`
	} `json:"attention"`
	AttentionTotal int `json:"attentionTotal"`
}

func readOP7Attention(t *testing.T, h http.Handler) (op7Attention, map[string]string) {
	t.Helper()
	rec := gateReq(t, h, http.MethodGet, "/api/v1/overview", gAllViewer, "")
	var ov op7Attention
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &ov) != nil {
		t.Fatalf("overview: %d %s", rec.Code, rec.Body)
	}
	byItem := map[string]string{} // "kind name" → trace id
	for _, it := range ov.Attention {
		byItem[it.Kind+" "+it.Name] = it.TraceID
	}
	return ov, byItem
}

func TestOP7AFailedStepIsListedTwiceToday(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	seedFailedSteps(t, mustExec(t, pool))

	ov, items := readOP7Attention(t, h)
	for k, trace := range map[string]string{
		"workflow_failed s0-tax-flow": "wr-tax-step",
		"job_failed s0-tax-job":       "r-tax-step", // defect 100: the same failure again
		"job_failed s0-fin-job":       "r-fin-step",
	} {
		if items[k] != trace {
			t.Errorf("%s: run %q, want %q (all: %v)", k, items[k], trace, items)
		}
	}
	if _, ok := items["workflow_failed s0-fin-flow"]; ok {
		t.Errorf("FIN's workflow succeeded past its step, yet it is listed")
	}
	if ov.AttentionTotal != len(ov.Attention) || ov.AttentionTotal != 3 {
		t.Errorf("attentionTotal %d for %d items, want 3", ov.AttentionTotal, len(ov.Attention))
	}
}

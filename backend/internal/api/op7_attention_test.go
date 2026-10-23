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

func TestOP7AFailedStepIsListedOnce(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	seedFailedSteps(t, exec)

	ov, items := readOP7Attention(t, h)
	for k, trace := range map[string]string{
		"workflow_failed s0-tax-flow": "wr-tax-step",
		// FIN's workflow got past its step: the job's item is the only one.
		"job_failed s0-fin-job": "r-fin-step",
	} {
		if items[k] != trace {
			t.Errorf("%s: run %q, want %q (all: %v)", k, items[k], trace, items)
		}
	}
	if trace, ok := items["job_failed s0-tax-job"]; ok {
		t.Errorf("TAX's step %q is listed beside the workflow it failed", trace)
	}
	if _, ok := items["workflow_failed s0-fin-flow"]; ok {
		t.Errorf("FIN's workflow succeeded past its step, yet it is listed")
	}
	// The total counts the failure once: the drop comes before the count.
	if ov.AttentionTotal != len(ov.Attention) || ov.AttentionTotal != 2 {
		t.Errorf("attentionTotal %d for %d items, want 2", ov.AttentionTotal, len(ov.Attention))
	}

	// TAX's workflow runs again and succeeds: the step's failure is still the
	// job's latest run, and now nothing else shows it.
	exec(`INSERT INTO workflow_runs (id, workflow_name, workflow_source, workflow_uid, status, triggered_by, trigger_kind, created_at) VALUES
	        ('wr-tax-again', 's0-tax-flow', 'cronomicon', 'w-tax', 'success', 'x', 'manual', ?)`,
		time.Now().UTC().Add(-10*time.Minute).Format(time.RFC3339))
	_, items = readOP7Attention(t, h)
	if items["job_failed s0-tax-job"] != "r-tax-step" {
		t.Errorf("TAX's step after its workflow succeeded: %q, want r-tax-step (all: %v)", items["job_failed s0-tax-job"], items)
	}
	if _, ok := items["workflow_failed s0-tax-flow"]; ok {
		t.Errorf("TAX's workflow succeeded since, yet it is listed")
	}
}

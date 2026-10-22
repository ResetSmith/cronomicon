package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// Who to call (2.5.2 OP1): the overview's attention items and the run details
// carry the job's or workflow's annotation contact and critical flag.

type runAnnotationBody struct {
	Contact  string `json:"contact"`
	Critical bool   `json:"critical"`
}

func TestOP1AttentionItemsSayWhoToCall(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	seedOverview(t, h, exec)
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	exec(`INSERT INTO workflow_runs (id, workflow_name, workflow_source, workflow_uid, status, triggered_by, trigger_kind, created_at) VALUES
	        ('wr-tax-fail', 's0-tax-flow', 'cronomicon', 'w-tax', 'failure', 'x', 'manual', ?)`, recent)
	// The decoy is a WORKFLOW annotation under a JOB's uid: the lookup is keyed
	// on (kind, uid), so it must not reach the job's item.
	exec(`INSERT INTO annotations (owner_kind, owner_uid, critical, contact) VALUES
	        ('job',      'j-tax', 1, 'tax on-call'),
	        ('job',      'j-fin', 0, 'fin desk'),
	        ('workflow', 'w-tax', 0, 'tax flow owner'),
	        ('workflow', 'j-fin', 1, 'decoy')`)

	rec := gateReq(t, h, http.MethodGet, "/api/v1/overview", gAllViewer, "")
	var ov struct {
		Attention []struct {
			Kind     string `json:"kind"`
			Name     string `json:"name"`
			Contact  string `json:"contact"`
			Critical bool   `json:"critical"`
		} `json:"attention"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &ov) != nil {
		t.Fatalf("overview: %d %s", rec.Code, rec.Body)
	}
	type want struct {
		contact  string
		critical bool
	}
	wants := map[string]want{
		"job_failed s0-tax-job":       {"tax on-call", true},
		"run_lost s0-fin-job":         {"fin desk", false},
		"workflow_failed s0-tax-flow": {"tax flow owner", false},
	}
	seen := map[string]bool{}
	for _, it := range ov.Attention {
		k := it.Kind + " " + it.Name
		if it.Kind == "sync_failed" && (it.Contact != "" || it.Critical) {
			t.Errorf("%s carries a contact %q / critical %v; a repository has none", k, it.Contact, it.Critical)
		}
		w, ok := wants[k]
		if !ok {
			continue
		}
		seen[k] = true
		if it.Contact != w.contact || it.Critical != w.critical {
			t.Errorf("%s: contact %q critical %v, want %q %v", k, it.Contact, it.Critical, w.contact, w.critical)
		}
	}
	for k := range wants {
		if !seen[k] {
			t.Errorf("no attention item %q (all: %+v)", k, ov.Attention)
		}
	}
}

func TestOP1TheRunDetailSaysWhoToCall(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	exec(`INSERT INTO annotations (owner_kind, owner_uid, critical, contact) VALUES
	        ('job',      'j-tax', 1, 'tax on-call'),
	        ('workflow', 'w-tax', 0, 'tax flow owner')`)
	exec(`INSERT INTO runs (id, job_name, job_uid, run_type, status, triggered_by, trigger_kind, created_at, agencies_json) VALUES
	        ('r-adhoc', 's0-adhoc', NULL, 'bash', 'success', 'x', 'manual', '2026-01-01T00:00:00Z', '[]')`)

	getRun := func(id, groups string) (int, map[string]json.RawMessage) {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, "/api/v1/runs/"+id, groups, "")
		var m map[string]json.RawMessage
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
				t.Fatalf("run %s: %v", id, err)
			}
		}
		return rec.Code, m
	}
	annOf := func(m map[string]json.RawMessage) *runAnnotationBody {
		t.Helper()
		raw, ok := m["annotation"]
		if !ok {
			return nil
		}
		var a runAnnotationBody
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatalf("annotation: %v", err)
		}
		return &a
	}

	// An annotated job's run says who to call; a member of the agency reads it.
	code, m := getRun("r-tax", gTaxAdmin)
	if a := annOf(m); code != http.StatusOK || a == nil || a.Contact != "tax on-call" || !a.Critical {
		t.Errorf("r-tax: %d, annotation %+v; want tax on-call, critical", code, a)
	}
	// An unannotated job's run carries an empty annotation, not a missing one.
	if code, m := getRun("r-glob", gRoot); code != http.StatusOK || annOf(m) == nil || *annOf(m) != (runAnnotationBody{}) {
		t.Errorf("r-glob: %d, annotation %+v; want an empty one", code, annOf(m))
	}
	// A run with no job uid has nothing to read.
	if code, m := getRun("r-adhoc", gRoot); code != http.StatusOK || annOf(m) != nil {
		t.Errorf("r-adhoc: %d, annotation %+v; want none", code, annOf(m))
	}
	// Still another agency's run: 404, and nothing about it.
	if code, _ := getRun("r-tax", gFinAdmin); code != http.StatusNotFound {
		t.Errorf("r-tax as FIN's admin = %d, want 404", code)
	}
	// The list does not pay for it.
	rec := gateReq(t, h, http.MethodGet, "/api/v1/runs?pageSize=200", gRoot, "")
	var page struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &page) != nil {
		t.Fatalf("runs list: %d %s", rec.Code, rec.Body)
	}
	for _, it := range page.Items {
		if _, ok := it["annotation"]; ok {
			t.Errorf("a list row carries an annotation: %s", it["traceId"])
		}
	}

	// A workflow run: its workflow's uid and contact.
	wrec := gateReq(t, h, http.MethodGet, "/api/v1/workflow-runs/wr-tax", gTaxAdmin, "")
	var wr struct {
		WorkflowUID string             `json:"workflowUid"`
		Annotation  *runAnnotationBody `json:"annotation"`
	}
	if wrec.Code != http.StatusOK || json.Unmarshal(wrec.Body.Bytes(), &wr) != nil {
		t.Fatalf("workflow run: %d %s", wrec.Code, wrec.Body)
	}
	if wr.WorkflowUID != "w-tax" || wr.Annotation == nil || wr.Annotation.Contact != "tax flow owner" || wr.Annotation.Critical {
		t.Errorf("wr-tax: uid %q, annotation %+v; want w-tax, tax flow owner", wr.WorkflowUID, wr.Annotation)
	}
}

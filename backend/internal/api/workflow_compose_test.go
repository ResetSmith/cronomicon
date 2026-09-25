package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestWorkflowComposeCRUD exercises the in-app Workflow composition write API
// (A11, Phase 4): create an cronomicon-source workflow over existing jobs, the
// unknown-job 422, the disjoint-namespace 409, that git workflows are read-only
// (409), and that delete cascades schedule bindings.
func TestWorkflowComposeCRUD(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT INTO jobs(name, source, run_type, command, content_hash, source_path, synced_at)
	      VALUES('build','git','bash','make','sha256:a','jobs/build.yaml','t')`)
	seed(`INSERT INTO jobs(name, source, run_type, command, content_hash, source_path, synced_at)
	      VALUES('deploy','git','bash','make deploy','sha256:b','jobs/deploy.yaml','t')`)
	seed(`INSERT INTO workflows(name, source, steps, source_path, synced_at)
	      VALUES('gitpipe','git','[]','workflows/gitpipe.yaml','t')`)

	client, csrf := devLoginWithCSRF(t, ts)
	post := func(method, url string, body any) *http.Response {
		t.Helper()
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, url, rdr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp
	}

	steps := []map[string]any{
		{"type": "job", "name": "build"},
		{"type": "job", "name": "deploy"},
	}

	// ── Create ──────────────────────────────────────────────────────────────────
	resp := post(http.MethodPost, ts.URL+"/api/v1/workflows", map[string]any{
		"name": "release", "steps": steps,
	})
	var created struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Source string `json:"source"`
		Steps  []struct {
			Name string `json:"name"`
		} `json:"steps"`
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.Source != "cronomicon" || len(created.Steps) != 2 {
		t.Errorf("created source=%q steps=%d, want amadeus/2", created.Source, len(created.Steps))
	}

	// ── Unknown job in steps → 422 ──────────────────────────────────────────────
	resp = post(http.MethodPost, ts.URL+"/api/v1/workflows", map[string]any{
		"name": "bad", "steps": []map[string]any{{"type": "job", "name": "ghost"}},
	})
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusUnprocessableEntity {
		t.Errorf("unknown-job steps = %d, want 422", code)
	}

	// ── Duplicate cronomicon name → 409 ────────────────────────────────────────────
	resp = post(http.MethodPost, ts.URL+"/api/v1/workflows", map[string]any{"name": "release", "steps": steps})
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusConflict {
		t.Errorf("duplicate cronomicon workflow = %d, want 409", code)
	}

	// ── Git workflow read-only (409 on PUT) ─────────────────────────────────────
	var gitRowid int64
	_ = pool.QueryRow(`SELECT rowid FROM workflows WHERE source='git' AND name='gitpipe'`).Scan(&gitRowid)
	resp = post(http.MethodPut, ts.URL+"/api/v1/workflows/"+itoa(gitRowid), map[string]any{"steps": steps})
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusConflict {
		t.Errorf("PUT git workflow = %d, want 409", code)
	}

	// ── Delete cronomicon workflow → 204 ───────────────────────────────────────────
	resp = post(http.MethodDelete, ts.URL+"/api/v1/workflows/"+itoa(created.ID), nil)
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", code)
	}
	// RH: delete is now a soft delete — the row is stamped, not removed, so an
	// undelete is one lossless UPDATE. "Deleted" means absent from the catalog.
	var live, binned int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM workflows WHERE source='cronomicon' AND name='release' AND deleted_at IS NULL`).Scan(&live)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM workflows WHERE source='cronomicon' AND name='release' AND deleted_at IS NOT NULL`).Scan(&binned)
	if live != 0 {
		t.Errorf("workflow is still live after delete: %d", live)
	}
	if binned != 1 {
		t.Errorf("workflow rows in the recycle bin = %d, want 1", binned)
	}

	// ── Duplicate job NAME on one path → 422 (PP-H8 a interim guard) ─────────────
	resp = post(http.MethodPost, ts.URL+"/api/v1/workflows", map[string]any{
		"name": "dup-steps",
		"steps": []map[string]any{
			{"type": "job", "name": "build"},
			{"type": "job", "name": "build"},
		},
	})
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusUnprocessableEntity {
		t.Errorf("duplicate job-name on one path = %d, want 422 (PP-H8 a)", code)
	}

	// ── Same job in mutually-exclusive branch arms → ACCEPTED (PP-H7 review) ─────
	// Pass and Fail are independent paths, so reusing a name across them is legal.
	resp = post(http.MethodPost, ts.URL+"/api/v1/workflows", map[string]any{
		"name": "arms-ok",
		"steps": []map[string]any{
			{"type": "job", "name": "build"},
			{
				"type":      "branch",
				"condition": map[string]any{"type": "job_status", "jobRef": "build"},
				"pass":      map[string]any{"steps": []map[string]any{{"type": "job", "name": "deploy"}}},
				"fail":      map[string]any{"steps": []map[string]any{{"type": "job", "name": "deploy"}}},
			},
		},
	})
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusCreated {
		t.Errorf("same job in both branch arms = %d, want 201 (mutually-exclusive paths)", code)
	}
}

// TestWorkflowComposeLayoutRoundTrip exercises the WC-P7 advisory layout column:
// a created workflow round-trips its layout on GET; an edit that OMITS layout
// PRESERVES it (COALESCE — a linear-editor save must never wipe hand-arranged
// positions); an edit WITH a new layout replaces it; and the layout never leaks into
// the steps definition (so it cannot affect steps_hash — it is presentation only).
func TestWorkflowComposeLayoutRoundTrip(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `INSERT INTO jobs(name, source, run_type, command, content_hash, source_path, synced_at)
	      VALUES('build','git','bash','make','sha256:a','jobs/build.yaml','t')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	client, csrf := devLoginWithCSRF(t, ts)
	do := func(method, url string, body any) *http.Response {
		t.Helper()
		rdr := bytes.NewReader(nil)
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, url, rdr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp
	}

	type wfResp struct {
		ID     int64                             `json:"id"`
		Layout map[string]struct{ X, Y float64 } `json:"layout"`
	}
	steps := []map[string]any{{"type": "job", "name": "build"}}

	// ── Create WITH a layout → 201, layout echoed back ──────────────────────────
	resp := do(http.MethodPost, ts.URL+"/api/v1/workflows", map[string]any{
		"name": "laidout", "steps": steps, "layout": map[string]any{"s0": map[string]any{"x": 40, "y": 80}},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	var created wfResp
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.Layout["s0"].X != 40 || created.Layout["s0"].Y != 80 {
		t.Fatalf("create layout = %+v, want s0={40,80}", created.Layout)
	}

	// The layout lives in its own column, NOT in steps — so steps_hash is unaffected.
	var stepsCol, layoutCol string
	_ = pool.QueryRow(`SELECT steps, COALESCE(layout_json,'') FROM workflows WHERE source='cronomicon' AND name='laidout'`).Scan(&stepsCol, &layoutCol)
	if strings.Contains(stepsCol, `"x"`) || strings.Contains(stepsCol, "layout") {
		t.Errorf("layout leaked into steps column: %s", stepsCol)
	}
	if !strings.Contains(layoutCol, `"x":40`) {
		t.Errorf("layout_json column = %q, want the {s0:{x:40,y:80}} blob", layoutCol)
	}
	id := itoa(created.ID)

	// ── Edit WITHOUT layout (linear-editor save) → existing layout PRESERVED ─────
	resp = do(http.MethodPut, ts.URL+"/api/v1/workflows/"+id, map[string]any{"steps": steps})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("edit(no-layout) = %d, want 200", resp.StatusCode)
	}
	var edited wfResp
	_ = json.NewDecoder(resp.Body).Decode(&edited)
	resp.Body.Close()
	if edited.Layout["s0"].X != 40 {
		t.Errorf("layout wiped by a layout-less edit: %+v (COALESCE must preserve it)", edited.Layout)
	}

	// ── Edit WITH a new layout → replaced; GET returns the new positions ─────────
	resp = do(http.MethodPut, ts.URL+"/api/v1/workflows/"+id, map[string]any{
		"steps": steps, "layout": map[string]any{"s0": map[string]any{"x": 5, "y": 6}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("edit(new-layout) = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	resp = do(http.MethodGet, ts.URL+"/api/v1/workflows/"+id, nil)
	var got wfResp
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Layout["s0"].X != 5 || got.Layout["s0"].Y != 6 {
		t.Errorf("GET layout = %+v, want s0={5,6}", got.Layout)
	}
}

// TestWorkflowStructuralValidation exercises WB-S5 (write-time structural/enum
// validation) and WB-A1 (the dry-run /workflows/validate endpoint). A typo'd
// type, a malformed branch, a dangling A12 input-ref, and a bad condition
// operator are each rejected with a 422 on write; the validate endpoint reports
// the same problems as 200 {ok:false, errors[]} without persisting, and passes a
// well-formed graph as {ok:true}.
func TestWorkflowStructuralValidation(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT INTO jobs(name, source, run_type, command, content_hash, source_path, synced_at)
	      VALUES('build','git','bash','make','sha256:a','jobs/build.yaml','t')`)
	seed(`INSERT INTO jobs(name, source, run_type, command, content_hash, source_path, synced_at)
	      VALUES('deploy','git','bash','make deploy','sha256:b','jobs/deploy.yaml','t')`)

	client, csrf := devLoginWithCSRF(t, ts)
	post := func(method, url string, body any) *http.Response {
		t.Helper()
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, url, rdr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp
	}

	type validateResp struct {
		OK     bool `json:"ok"`
		Errors []struct {
			Step, Field, Message string
		} `json:"errors"`
	}
	doValidate := func(body any) validateResp {
		t.Helper()
		resp := post(http.MethodPost, ts.URL+"/api/v1/workflows/validate", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("validate status = %d, want 200", resp.StatusCode)
		}
		var vr validateResp
		_ = json.NewDecoder(resp.Body).Decode(&vr)
		resp.Body.Close()
		return vr
	}

	// ── WB-A1 dry-run: a well-formed graph passes without persisting ─────────────
	if vr := doValidate(map[string]any{
		"name":  "ok-flow",
		"steps": []map[string]any{{"type": "job", "name": "build"}, {"type": "job", "name": "deploy"}},
	}); !vr.OK || len(vr.Errors) != 0 {
		t.Errorf("valid graph: ok=%v errors=%v, want ok/0-errors", vr.OK, vr.Errors)
	}
	var persisted int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM workflows WHERE source='cronomicon' AND name='ok-flow'`).Scan(&persisted)
	if persisted != 0 {
		t.Errorf("validate persisted a workflow: %d rows, want 0 (dry-run)", persisted)
	}

	// ── WB-A1 dry-run: an unknown job reports not-ok with an error ───────────────
	if vr := doValidate(map[string]any{
		"name":  "ghost-flow",
		"steps": []map[string]any{{"type": "job", "name": "ghost"}},
	}); vr.OK || len(vr.Errors) == 0 {
		t.Errorf("unknown-job validate: ok=%v errors=%d, want not-ok/≥1", vr.OK, len(vr.Errors))
	}

	// ── WB-A1 dry-run: a bad name reports not-ok ────────────────────────────────
	if vr := doValidate(map[string]any{
		"name":  "Bad Name",
		"steps": []map[string]any{{"type": "job", "name": "build"}},
	}); vr.OK {
		t.Errorf("bad-name validate: ok=true, want false")
	}

	// ── WB-S5 write-time rejections (each a 422) ────────────────────────────────
	reject := func(label string, steps any) {
		t.Helper()
		resp := post(http.MethodPost, ts.URL+"/api/v1/workflows", map[string]any{"name": "wf-" + label, "steps": steps})
		code := resp.StatusCode
		resp.Body.Close()
		if code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", label, code)
		}
	}

	reject("typo-type", []map[string]any{{"type": "jub", "name": "build"}})
	reject("branch-missing-fail", []map[string]any{
		{"type": "job", "name": "build"},
		{"type": "branch", "condition": map[string]any{"type": "job_status", "jobRef": "build"},
			"pass": map[string]any{"steps": []map[string]any{{"type": "job", "name": "deploy"}}}},
	})
	reject("dangling-input", []map[string]any{
		{"type": "job", "name": "build", "inputs": map[string]any{
			"VERSION": map[string]any{"fromStep": "ghoststep", "fromOutput": "v"}}},
	})
	reject("bad-operator", []map[string]any{
		{"type": "job", "name": "build"},
		{"type": "branch",
			"condition": map[string]any{"type": "output_match", "jobRef": "build", "field": "x", "operator": ">="},
			"pass":      map[string]any{"steps": []map[string]any{{"type": "job", "name": "deploy"}}},
			"fail":      map[string]any{"steps": []map[string]any{{"type": "job", "name": "deploy"}}}},
	})
	reject("parallel-empty", []map[string]any{{"type": "parallel", "jobs": []map[string]any{}}})

	// A valid downstream A12 input-ref is accepted on write.
	resp := post(http.MethodPost, ts.URL+"/api/v1/workflows", map[string]any{
		"name": "a12-ok",
		"steps": []map[string]any{
			{"type": "job", "name": "build"},
			{"type": "job", "name": "deploy", "inputs": map[string]any{
				"VERSION": map[string]any{"fromStep": "build", "fromOutput": "version"}}},
		},
	})
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusCreated {
		t.Errorf("valid A12 input-ref = %d, want 201", code)
	}
}

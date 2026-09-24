package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// R2F-2 — the compose API accepts a step pinned to a job's identity, and refuses
// a pin that disagrees with the name beside it.
//
// The pair travels together: the name is the human-readable reference the whole
// product displays (history, {fromStep}, YAML round-trips), the uid is what
// resolves. A uid pointing at a differently-named job would make the graph show
// one job and run another, so it is a 422 rather than a silent preference for
// either half.
func TestWorkflowComposeStepIdentity(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// Two departments' jobs sharing one name — the state R2-5 made legal and that
	// a name-only step cannot express.
	seed(`INSERT INTO jobs(uid, name, source, run_type, command, content_hash, synced_at)
	      VALUES('uid-deploy-fin','deploy','amadeus','bash','make fin','sha256:a','t')`)
	seed(`INSERT INTO jobs(uid, name, source, run_type, command, content_hash, synced_at)
	      VALUES('uid-deploy-dss','deploy','amadeus','bash','make dss','sha256:b','t')`)
	seed(`INSERT INTO jobs(uid, name, source, run_type, command, content_hash, synced_at)
	      VALUES('uid-build','build','amadeus','bash','make','sha256:c','t')`)

	client, csrf := devLoginWithCSRF(t, ts)
	post := func(body any) *http.Response {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/workflows", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		return resp
	}
	bodyOf := func(resp *http.Response) string {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}

	// A pinned step naming one twin is accepted, and the uid round-trips.
	resp := post(map[string]any{
		"name": "fin-release",
		"steps": []map[string]any{
			{"type": "job", "name": "deploy", "jobUid": "uid-deploy-fin"},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("pinned create = %d (%s), want 201 — an identity-qualified step over twins must be buildable",
			resp.StatusCode, bodyOf(resp))
	}
	var created struct {
		Steps []struct {
			Name   string `json:"name"`
			JobUID string `json:"jobUid"`
		} `json:"steps"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if len(created.Steps) != 1 || created.Steps[0].JobUID != "uid-deploy-fin" || created.Steps[0].Name != "deploy" {
		t.Errorf("round-tripped step = %+v, want name=deploy uid=uid-deploy-fin", created.Steps)
	}

	// A uid naming a DIFFERENT job than the step's name: the display would lie.
	resp = post(map[string]any{
		"name": "liar",
		"steps": []map[string]any{
			{"type": "job", "name": "deploy", "jobUid": "uid-build"},
		},
	})
	body := bodyOf(resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("mismatched pin = %d (%s), want 422", resp.StatusCode, body)
	}
	if !strings.Contains(body, "disagree") {
		t.Errorf("mismatch message = %s, want it to say the name and identity disagree", body)
	}

	// A pin at a job that does not exist is an unknown-job 422, like a bad name.
	resp = post(map[string]any{
		"name": "ghostpin",
		"steps": []map[string]any{
			{"type": "job", "name": "deploy", "jobUid": "uid-nonexistent"},
		},
	})
	body = bodyOf(resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("dangling pin = %d (%s), want 422", resp.StatusCode, body)
	}

	// And the regression that motivates the band: the same graph WITHOUT a pin is
	// still accepted at compose time (the ambiguity is a run-time refusal, not an
	// authoring one — the existence check only asks whether the name is known).
	resp = post(map[string]any{
		"name":  "nameonly",
		"steps": []map[string]any{{"type": "job", "name": "deploy"}},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("name-only create = %d (%s), want 201 — unchanged behaviour", resp.StatusCode, bodyOf(resp))
	}
}

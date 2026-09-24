package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// scriptPrompt mirrors the JobPrompt schema for decoding a Script's declared run
// inputs off the wire.
type scriptPrompt struct {
	Name     string   `json:"name"`
	Label    string   `json:"label,omitempty"`
	Required bool     `json:"required,omitempty"`
	Default  *string  `json:"default,omitempty"`
	Options  []string `json:"options,omitempty"`
}

// TestScriptPromptsOnTheWire (JR-Q6, T2.8): a script's declared run inputs
// (scripts.prompts_json, migration 651) are exposed on BOTH the list and detail
// projections, and a script that declares none reports [] rather than null — the
// Job Composer seeds from this field, so a null would be a silent no-op there.
//
// Both projections matter: the Composer's script picker reads the list, and
// resolving an off-page selection reads the detail. They scan different column
// sets through different helpers, which is exactly how a new column gets wired
// into one and forgotten in the other.
func TestScriptPromptsOnTheWire(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at, prompts_json)
	      VALUES('deploy-app','bash','echo deploying','ssh','sha256:aaa','scripts/deploy-app.yaml','t',?)`,
		`[{"name":"TARGET_ENV","label":"Deployment environment","required":true,"options":["dev","prod"]},{"name":"REPLICAS","default":"3"}]`)
	// A script that declares nothing — the migration DEFAULT applies.
	seed(`INSERT INTO scripts(name, run_type, command, content_hash, source_path, synced_at)
	      VALUES('plain','bash','echo hi','sha256:bbb','scripts/plain.yaml','t')`)

	client, _ := devLoginWithCSRF(t, ts)
	get := func(url string) *http.Response {
		t.Helper()
		resp, err := client.Get(ts.URL + url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("GET %s = %d, want 200", url, resp.StatusCode)
		}
		return resp
	}

	assertDeclared := func(where string, ps []scriptPrompt) {
		t.Helper()
		if len(ps) != 2 {
			t.Fatalf("%s: prompts = %d, want 2: %+v", where, len(ps), ps)
		}
		if ps[0].Name != "TARGET_ENV" || !ps[0].Required || ps[0].Label != "Deployment environment" || len(ps[0].Options) != 2 {
			t.Errorf("%s: TARGET_ENV not round-tripped: %+v", where, ps[0])
		}
		if ps[1].Name != "REPLICAS" || ps[1].Default == nil || *ps[1].Default != "3" {
			t.Errorf("%s: REPLICAS default not round-tripped: %+v", where, ps[1])
		}
	}

	// ── Detail projection (scanScript) ──────────────────────────────────────────
	resp := get("/api/v1/scripts/deploy-app")
	var detail struct {
		Prompts []scriptPrompt `json:"prompts"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&detail)
	resp.Body.Close()
	assertDeclared("detail", detail.Prompts)

	// ── List projection (scanScriptListRow) ─────────────────────────────────────
	resp = get("/api/v1/scripts?pageSize=50")
	var list struct {
		Items []struct {
			Name    string          `json:"name"`
			Prompts *[]scriptPrompt `json:"prompts"`
		} `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	seen := map[string]*[]scriptPrompt{}
	for _, it := range list.Items {
		seen[it.Name] = it.Prompts
	}
	got, ok := seen["deploy-app"]
	if !ok {
		t.Fatalf("deploy-app missing from the list projection: %+v", list.Items)
	}
	if got == nil {
		t.Fatal("list: prompts is null — the field must always serialize (Composer seeds from it)")
	}
	assertDeclared("list", *got)

	// A script declaring none serializes [] on both projections, never null.
	plain, ok := seen["plain"]
	if !ok {
		t.Fatalf("plain missing from the list projection: %+v", list.Items)
	}
	if plain == nil || len(*plain) != 0 {
		t.Errorf("list: plain prompts = %v, want an empty array", plain)
	}
}

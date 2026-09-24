package api_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// TestScriptsListOmitsBodies is the CC.11 regression: the Scripts LIST response
// must not ship the command/script blob columns — it carries a derived
// sourceKind badge instead — while the detail endpoint still returns the body.
func TestScriptsListOmitsBodies(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT INTO scripts(name, run_type, command, content_hash, synced_at)
	      VALUES('cmd-one','bash','echo hi','sha256:a','2026-01-01T00:00:00Z')`)
	seed(`INSERT INTO scripts(name, run_type, script, content_hash, synced_at)
	      VALUES('inline-one','ansible','- hosts: all','sha256:b','2026-01-01T00:00:00Z')`)
	seed(`INSERT INTO scripts(name, run_type, script_path, content_hash, synced_at)
	      VALUES('file-one','bash','scripts/x.sh','sha256:c','2026-01-01T00:00:00Z')`)

	client := devLoginClient(t, ts)

	// Raw list body: must badge each row with sourceKind but never ship a body
	// column. ("script": is the key-with-colon; "sourceKind":"script" does not
	// match it, and "scriptPath": is a different key.)
	resp, err := client.Get(ts.URL + "/api/v1/scripts")
	if err != nil {
		t.Fatalf("GET list: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if body := string(raw); strings.Contains(body, `"command":`) || strings.Contains(body, `"script":`) {
		t.Errorf("list response still ships a body column (CC.11):\n%s", body)
	}

	var list struct {
		Items []struct {
			Name       string  `json:"name"`
			Command    *string `json:"command"`
			Script     *string `json:"script"`
			SourceKind string  `json:"sourceKind"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{"cmd-one": "command", "inline-one": "script", "file-one": "file"}
	got := map[string]string{}
	for _, it := range list.Items {
		got[it.Name] = it.SourceKind
		if it.Command != nil || it.Script != nil {
			t.Errorf("%s: list row carries a body (command=%v script=%v)", it.Name, it.Command, it.Script)
		}
	}
	for name, wk := range want {
		if got[name] != wk {
			t.Errorf("%s sourceKind = %q, want %q", name, got[name], wk)
		}
	}

	// Detail still carries the body AND the sourceKind badge.
	var detail struct {
		Command    *string `json:"command"`
		SourceKind string  `json:"sourceKind"`
	}
	getJSON(t, client, ts.URL+"/api/v1/scripts/cmd-one", &detail)
	if detail.Command == nil || *detail.Command != "echo hi" {
		t.Errorf("detail command = %v, want 'echo hi' (detail must keep the body)", detail.Command)
	}
	if detail.SourceKind != "command" {
		t.Errorf("detail sourceKind = %q, want command", detail.SourceKind)
	}
}

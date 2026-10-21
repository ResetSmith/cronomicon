package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// 2.5.1 SG-11, SG-12, SG-13: a secret's change-log rows say what was done
// (rotated, updated, revealed, deleted by key), and who did it in relation to
// the secret's agency; the in-app change log returns that with the agency.
func TestSecretChangeLogSaysWhatAndWho(t *testing.T) {
	h, _ := gateServer(t)
	rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gFinAdmin, `{"key":"FIN_TOKEN","source":"stored","scope":"fin-hosts","value":"v1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	put := func(group, body string) {
		t.Helper()
		if r := gateReq(t, h, http.MethodPut, "/api/v1/env-secrets/"+created.ID, group, body); r.Code != http.StatusOK {
			t.Fatalf("update by %s = %d %s", group, r.Code, r.Body)
		}
	}
	put(gFinAdmin, `{"key":"FIN_TOKEN","source":"stored","scope":"fin-hosts","description":"the token"}`)
	put(gFinAdmin, `{"key":"FIN_TOKEN","source":"stored","scope":"fin-hosts","value":"v2"}`)
	if r := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets/"+created.ID+"/reveal", gRoot, ""); r.Code != http.StatusOK {
		t.Fatalf("reveal by root = %d %s", r.Code, r.Body)
	}
	if r := gateReq(t, h, http.MethodDelete, "/api/v1/env-secrets/"+created.ID, gFinAdmin, ""); r.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", r.Code, r.Body)
	}

	rec = gateReq(t, h, http.MethodGet, "/api/v1/change-log?category=Secrets&pageSize=50", gFinAdmin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("change log = %d %s", rec.Code, rec.Body)
	}
	var page struct {
		Items []struct {
			Action, Target, Agency, ActorRelation, Details string
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	type row struct{ action, target, relation string }
	got := map[row]bool{}
	for _, it := range page.Items {
		if it.Agency != "ag:FIN" {
			t.Errorf("a FIN secret's row filed under %q", it.Agency)
		}
		got[row{it.Action, it.Target, it.ActorRelation}] = true
	}
	for _, want := range []row{
		{"created", "FIN_TOKEN", "member"},
		{"updated", "FIN_TOKEN", "member"},
		{"rotated", "FIN_TOKEN", "member"},
		{"revealed", "FIN_TOKEN", "global_admin"},
		{"deleted", "FIN_TOKEN", "member"},
	} {
		if !got[want] {
			t.Errorf("missing change-log row %+v; have %+v", want, got)
		}
	}
}

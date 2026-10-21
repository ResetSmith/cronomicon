package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 2.5.1 SG2.11 (SG-19): an agency makes its secret unrevealable; nobody
// reveals it afterwards, a global administrator included; another agency
// cannot lock it; locking twice is fine; and no route undoes it.
func TestUnrevealableSecrets(t *testing.T) {
	h, _ := gateServer(t)
	rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gFinAdmin, `{"key":"FIN_TOKEN","source":"stored","scope":"fin-hosts","value":"v"}`)
	var c struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &c)

	if r := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets/"+c.ID+"/lock-reveal", gTaxAdmin, ""); r.Code != http.StatusNotFound {
		t.Errorf("TAX locking FIN's secret = %d, want 404", r.Code)
	}
	if r := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets/"+c.ID+"/reveal", gFinAdmin, ""); r.Code != http.StatusOK {
		t.Fatalf("a reveal before the lock = %d", r.Code)
	}
	r := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets/"+c.ID+"/lock-reveal", gFinAdmin, "")
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"revealLocked":true`) {
		t.Fatalf("FIN locking its secret = %d %s", r.Code, r.Body)
	}
	if r := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets/"+c.ID+"/lock-reveal", gFinAdmin, ""); r.Code != http.StatusOK {
		t.Errorf("locking twice = %d", r.Code)
	}
	for _, g := range []string{gFinAdmin, gRoot} {
		r := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets/"+c.ID+"/reveal", g, "")
		if r.Code != http.StatusConflict || errCode(r.Body.Bytes()) != "reveal_locked" {
			t.Errorf("%s revealing a locked secret = %d %s, want 409 reveal_locked", g, r.Code, r.Body)
		}
	}
	// A rotation still works.
	if r := gateReq(t, h, http.MethodPut, "/api/v1/env-secrets/"+c.ID, gFinAdmin, `{"key":"FIN_TOKEN","source":"stored","scope":"fin-hosts","value":"v2"}`); r.Code != http.StatusOK {
		t.Errorf("rotating a locked secret = %d %s", r.Code, r.Body)
	}
	// One way: no registered route takes the lock off.
	for route := range registeredWriteRoutes(t) {
		if strings.Contains(route, "unlock") || (strings.Contains(route, "lock-reveal") && !strings.HasPrefix(route, "POST ")) {
			t.Errorf("a route can undo the reveal lock: %s", route)
		}
	}
}

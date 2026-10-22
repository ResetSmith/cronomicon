package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The address alerts link to (2.5.2 OP2.1): recorded from the first global
// administrator's browser, shown and changed on Settings → Notifications. A
// global administrator's; checked when written, so a bad value is refused and
// can never stop the server.

func TestOP2TheAlertLinkAddressIsLearnedOnceAndKeptByHand(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	type addr struct {
		Address string `json:"address"`
		Source  string `json:"source"`
		By      string `json:"by"`
	}
	call := func(method, path, groups, body string) (int, addr) {
		t.Helper()
		rec := gateReq(t, h, method, path, groups, body)
		var a addr
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
		}
		return rec.Code, a
	}
	const base = "/api/v1/settings/alert-link-address"

	if code, a := call(http.MethodGet, base, gRoot, ""); code != http.StatusOK || a.Address != "" {
		t.Fatalf("before anything: %d %+v, want an empty address", code, a)
	}
	for _, who := range []string{gFinAdmin, gAllViewer} {
		if code, _ := call(http.MethodPost, base+"/detect", who, `{"address":"https://x.example.gov"}`); code != http.StatusForbidden {
			t.Errorf("detect as %s = %d, want 403", who, code)
		}
		if code, _ := call(http.MethodGet, base, who, ""); code != http.StatusForbidden {
			t.Errorf("read as %s = %d, want 403", who, code)
		}
	}
	for _, bad := range []string{`{"address":"javascript:alert(1)"}`, `{"address":"cron.example.gov"}`, `{"address":"https://x.example.gov/?a=1"}`, `{}`} {
		if code, _ := call(http.MethodPost, base+"/detect", gRoot, bad); code != http.StatusUnprocessableEntity {
			t.Errorf("detect %s = %d, want 422", bad, code)
		}
	}

	// The first visit decides; a later one through another name changes nothing.
	if code, a := call(http.MethodPost, base+"/detect", gRoot, `{"address":"https://cron.example.gov/"}`); code != http.StatusOK || a.Address != "https://cron.example.gov" || a.Source != "detected" || a.By == "" {
		t.Errorf("first detect: %d %+v", code, a)
	}
	if code, a := call(http.MethodPost, base+"/detect", gRoot, `{"address":"http://10.0.0.5:8080"}`); code != http.StatusOK || a.Address != "https://cron.example.gov" {
		t.Errorf("second detect: %d %+v, want the first address kept", code, a)
	}

	// Typed by hand: replaces it; cleared stays cleared.
	if code, a := call(http.MethodPut, base, gRoot, `{"address":"https://ops.example.gov/cronomicon/"}`); code != http.StatusOK || a.Address != "https://ops.example.gov/cronomicon" || a.Source != "set" {
		t.Errorf("set: %d %+v", code, a)
	}
	if code, _ := call(http.MethodPut, base, gRoot, `{"address":"ftp://x.example.gov"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("set ftp = %d, want 422", code)
	}
	if code, _ := call(http.MethodPut, base, gRoot, `{}`); code != http.StatusUnprocessableEntity {
		t.Errorf("set without the field = %d, want 422 (it must not clear it)", code)
	}
	if code, _ := call(http.MethodPut, base, gFinAdmin, `{"address":"https://x.example.gov"}`); code != http.StatusForbidden {
		t.Errorf("set as FIN's admin = %d, want 403", code)
	}
	if code, a := call(http.MethodPut, base, gRoot, `{"address":""}`); code != http.StatusOK || a.Address != "" {
		t.Errorf("clear: %d %+v", code, a)
	}
	if code, a := call(http.MethodPost, base+"/detect", gRoot, `{"address":"https://cron.example.gov"}`); code != http.StatusOK || a.Address != "" {
		t.Errorf("detect after clearing: %d %+v, want it to stay cleared", code, a)
	}

	var detected, set int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM change_log WHERE action = 'alert-link-address-detected'`).Scan(&detected)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM change_log WHERE action = 'alert-link-address-set'`).Scan(&set)
	if detected != 1 || set != 2 {
		t.Errorf("change log: %d detected, %d set; want 1 and 2", detected, set)
	}
}

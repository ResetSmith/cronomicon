package api_test

import (
	"net/http"
	"testing"
)

// 2.5.1 SG-17: a rule about a secret aims at "all"; backup-key-changed is a
// Global rule's alone; an agency's secret-revealed rule is accepted.
func TestSecretAlertRulesAreRefusedWhenTheyCouldNeverFire(t *testing.T) {
	h, _ := gateServer(t)
	post := func(group, body string) (int, string) {
		rec := gateReq(t, h, http.MethodPost, "/api/v1/alerts", group, body)
		return rec.Code, errCode(rec.Body.Bytes())
	}
	if code, ec := post(gFinAdmin, `{"targetMode":"all","trigger":"secret-revealed","channels":["email"],"recipients":"fin@x","enabled":true}`); code != http.StatusCreated {
		t.Errorf("FIN's secret-revealed rule = %d %s", code, ec)
	}
	if code, ec := post(gFinAdmin, `{"targetMode":"job","jobName":"x","trigger":"secret-changed","channels":["email"],"recipients":"fin@x","enabled":true}`); code != http.StatusUnprocessableEntity || ec != "invalid_target_mode" {
		t.Errorf("a secret rule aimed at a job = %d %s", code, ec)
	}
	if code, ec := post(gFinAdmin, `{"targetMode":"all","trigger":"backup-key-changed","channels":["email"],"recipients":"fin@x","enabled":true}`); code != http.StatusUnprocessableEntity || ec != "trigger_global_only" {
		t.Errorf("FIN's backup-key-changed rule = %d %s", code, ec)
	}
	if code, ec := post(gRoot, `{"targetMode":"all","trigger":"backup-key-changed","channels":["email"],"recipients":"ops@x","enabled":true,"ownerAgency":"global"}`); code != http.StatusCreated {
		t.Errorf("Global's backup-key-changed rule = %d %s", code, ec)
	}
}

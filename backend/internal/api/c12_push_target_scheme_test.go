package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestC12_AnAgencysPushTargetNamesNoGenericWebhook (present defect 95, question
// 112): an agency's administrator may not aim a push target at Apprise's
// generic webhooks (json, xml, form and their TLS forms), which reach any
// address the installation's gateway can, with headers of their choosing — a
// cloud metadata service, Vault on the loopback. GR-10 refuses them that for a
// repository. A global administrator may add one in the agency, which its own
// administrator may then relabel or switch off but not re-point; Global's
// targets are a global administrator's.
func TestC12_AnAgencysPushTargetNamesNoGenericWebhook(t *testing.T) {
	h, _ := gateServer(t)
	create := func(groups, body string) (int, string) {
		t.Helper()
		rec := gateReq(t, h, http.MethodPost, "/api/v1/alert-targets", groups, body)
		var tg struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &tg)
		return rec.Code, tg.ID
	}
	for _, url := range []string{
		"json://169.254.169.254/latest/meta-data",
		"jsons://127.0.0.1:8200/v1/sys/seal?+X-Vault-Token=x",
		"XML://internal/hook",
		"forms://cronomicon-internal:8080/x",
		"ntfy://topic, json://169.254.169.254/x", // Apprise splits at commas
		"ntfy://topic form://10.0.0.1/x",         // and at spaces
		// C12.4's re-review: Apprise finds a scheme after ANY character that is
		// not a letter or digit, and splits at separators Go's IsSpace does not
		// know. Each of these was stored, and Apprise sent the webhook.
		"/json://169.254.169.254/latest/meta-data?method=GET",
		"[json://169.254.169.254/x",
		`\"jsons://127.0.0.1:8200/v1/sys/seal?+X-Vault-Token=x`,
		`ntfy://topic\u001cjson://169.254.169.254/x`,
		`ntfy://topic,\u001fxmls://10.0.0.1/x`,
		// Question 117 (A): Apprise's relay reaches as far, with chosen headers.
		"apprise://127.0.0.1:8200/v1/?+X-Vault-Token=x",
		"APPRISES://internal-host/notify/key",
	} {
		if code, _ := create(gFinAdmin, `{"label":"x","url":"`+url+`"}`); code != http.StatusUnprocessableEntity {
			t.Errorf("FIN's administrator creating %q = %d, want 422", url, code)
		}
	}
	// A scheme a letter or digit precedes is another scheme to Apprise.
	for _, url := range []string{"ntfy://topic", "slack://a/b/c", "mailto://u:p@example.com", "ntfy://topic/xjson://x",
		"gotify://gotify.internal/token", "mmost://mattermost.internal/token"} {
		if code, _ := create(gFinAdmin, `{"label":"x","url":"`+url+`"}`); code != http.StatusCreated {
			t.Errorf("FIN's administrator creating %q = %d, want 201", url, code)
		}
	}

	// A global administrator may, in FIN and in Global.
	code, fin := create(gRoot, `{"label":"hook","url":"json://hooks.internal/fin","ownerAgency":"ag:FIN"}`)
	if code != http.StatusCreated {
		t.Fatalf("root creating FIN's generic webhook = %d, want 201", code)
	}
	if code, _ := create(gRoot, `{"label":"hook","url":"json://hooks.internal/ops"}`); code != http.StatusCreated {
		t.Errorf("root creating Global's generic webhook = %d, want 201", code)
	}

	// FIN's administrator keeps root's URL and may change the rest; not re-point it.
	put := func(body string) int {
		return gateReq(t, h, http.MethodPut, "/api/v1/alert-targets/"+fin, gFinAdmin, body).Code
	}
	if c := put(`{"label":"renamed","url":"json://hooks.internal/fin","enabled":false}`); c != http.StatusOK {
		t.Errorf("FIN's administrator relabelling root's webhook = %d, want 200", c)
	}
	if c := put(`{"label":"renamed","url":"json://169.254.169.254/x"}`); c != http.StatusUnprocessableEntity {
		t.Errorf("FIN's administrator re-pointing it = %d, want 422", c)
	}
	if c := put(`{"label":"renamed","url":"ntfy://topic"}`); c != http.StatusOK {
		t.Errorf("FIN's administrator replacing it with a service = %d, want 200", c)
	}
}

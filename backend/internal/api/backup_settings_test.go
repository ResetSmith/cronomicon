package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/ResetSmith/cronomicon/internal/backup"
)

// Settings → Backups (2.5.1 SG1.3): a global administrator's, both ways; the
// first key needs the custody acknowledgement, the last key's removal the
// confirmation, and a pasted private key is refused by name.
func TestBackupSettingsRoutes(t *testing.T) {
	h, _ := gateServer(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	pub := id.Recipient().String()
	body := func(recips []string, ack, confirm bool) string {
		b, _ := json.Marshal(map[string]any{"recipients": recips, "acknowledgedKeyCustody": ack, "confirmUnencrypted": confirm})
		return string(b)
	}

	for _, g := range []string{gFinAdmin, gMixed, gAllViewer} {
		if rec := gateReq(t, h, http.MethodGet, "/api/v1/settings/backup", g, ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s reading the backup settings = %d, want 403", g, rec.Code)
		}
		if rec := gateReq(t, h, http.MethodPut, "/api/v1/settings/backup", g, body([]string{pub}, true, false)); rec.Code != http.StatusForbidden {
			t.Errorf("%s setting the backup keys = %d, want 403", g, rec.Code)
		}
	}

	rec := gateReq(t, h, http.MethodGet, "/api/v1/settings/backup", gRoot, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"encrypted":false`) || !strings.Contains(rec.Body.String(), `"recipients":[]`) {
		t.Fatalf("reading with no key = %d %s", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/settings/backup", gRoot, `{}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a body with no recipients = %d, want 422 (it must not read as 'remove every key')", rec.Code)
	}
	rec = gateReq(t, h, http.MethodPut, "/api/v1/settings/backup", gRoot, body([]string{pub}, false, false))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "acknowledgement_required") {
		t.Errorf("first key without the acknowledgement = %d %s", rec.Code, rec.Body)
	}
	rec = gateReq(t, h, http.MethodPut, "/api/v1/settings/backup", gRoot, body([]string{id.String()}, true, false))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "invalid_recipient") || !strings.Contains(rec.Body.String(), "PRIVATE") {
		t.Errorf("a private key = %d %s", rec.Code, rec.Body)
	}
	rec = gateReq(t, h, http.MethodPut, "/api/v1/settings/backup", gRoot, body([]string{pub}, true, false))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"encrypted":true`) || !strings.Contains(rec.Body.String(), backup.Fingerprint(pub)) {
		t.Fatalf("turning encryption on = %d %s", rec.Code, rec.Body)
	}
	rec = gateReq(t, h, http.MethodPut, "/api/v1/settings/backup", gRoot, body([]string{}, false, false))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "confirmation_required") {
		t.Errorf("removing the last key without the confirmation = %d %s", rec.Code, rec.Body)
	}
	rec = gateReq(t, h, http.MethodPut, "/api/v1/settings/backup", gRoot, body([]string{}, false, true))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"encrypted":false`) {
		t.Errorf("turning encryption off = %d %s", rec.Code, rec.Body)
	}
}

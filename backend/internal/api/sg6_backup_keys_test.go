package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"filippo.io/age"
)

// 2.5.1 SG6.3 (the review's A8): the Backups card's PUT refused a private key
// pasted with anything around it, and its 422 carried the key back.
func TestSG6_NoRefusalCarriesAPrivateKey(t *testing.T) {
	h, _ := gateServer(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec := id.String()
	for _, in := range []string{
		"# created: 2026-10-21T10:00:00Z # public key: " + id.Recipient().String() + " " + sec,
		`"` + sec + `"`,
		`'` + sec + `'`,
		strings.ToLower(sec),
		"key=" + sec,
	} {
		b, _ := json.Marshal(map[string]any{"recipients": []string{in}, "acknowledgedKeyCustody": true})
		rec := gateReq(t, h, http.MethodPut, "/api/v1/settings/backup", gRoot, string(b))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("PUT with a private key = %d, want 422", rec.Code)
		}
		if strings.Contains(strings.ToUpper(rec.Body.String()), sec[len("AGE-SECRET-KEY-"):]) {
			t.Errorf("the 422 carries the private key: %s", rec.Body)
		}
	}
}

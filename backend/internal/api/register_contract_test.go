package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// TestRegisterRequiresProtocolVersion is the conformance test for DM-3: the
// register request's SHAPE, which nothing pinned before.
//
// The server began refusing an absent protocolVersion in v1.5.40 ("absent
// counts as 0"), turning a spec-omitted optional field into a mandatory one —
// but openapi.yaml still listed `required: [name, os, capabilities, version]`
// with no protocolVersion property and no 426 response, so a client generated
// from the contract sent a body the server rejected with a status the operation
// did not document. Nothing failed at build time because the only register test
// happened to send the field.
//
// This test is the pin in the other direction: it asserts the server behaviour
// the spec now documents, so the two cannot drift apart again silently.
func TestRegisterRequiresProtocolVersion(t *testing.T) {
	ts, _ := newTestServer(t)

	post := func(t *testing.T, body map[string]any) (int, string) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runners/register", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer boot-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		defer resp.Body.Close()
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return resp.StatusCode, e.Code
	}

	base := func() map[string]any {
		return map[string]any{
			"name": "contract-runner", "os": "Linux",
			"capabilities": []string{"bash"}, "version": "1.0.0",
		}
	}

	t.Run("absent is refused", func(t *testing.T) {
		code, errCode := post(t, base())
		if code != http.StatusUpgradeRequired {
			t.Fatalf("register without protocolVersion: status = %d, want 426", code)
		}
		if errCode != "protocol_too_old" {
			t.Errorf("error code = %q, want protocol_too_old", errCode)
		}
	})

	t.Run("below the floor is refused", func(t *testing.T) {
		b := base()
		b["protocolVersion"] = runnerproto.MinProtocolVersion - 1
		if code, _ := post(t, b); code != http.StatusUpgradeRequired {
			t.Fatalf("register below the floor: status = %d, want 426", code)
		}
	})

	t.Run("the current protocol is accepted", func(t *testing.T) {
		b := base()
		b["protocolVersion"] = runnerproto.ProtocolVersion
		code, errCode := post(t, b)
		if code != http.StatusCreated && code != http.StatusOK {
			t.Fatalf("register at the current protocol: status = %d (%s), want 200/201", code, errCode)
		}
	})
}

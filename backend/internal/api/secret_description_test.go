package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestSecretDescriptionRoundTrips is the regression for the bug where a secret's
// description was accepted by the API + SPA form but silently dropped: the
// secrets table had no description column and secrets.Service never wrote or read
// one (the same defect 100 fixed for env_vars). Migration 340 adds the column;
// this test proves description now persists through create → read-back → list →
// update.
func TestSecretDescriptionRoundTrips(t *testing.T) {
	ts, _ := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	const wantDesc = "API token for the nightly backup job"

	// Create a stored secret carrying a description.
	createBody := `{"key":"BACKUP_TOKEN","source":"stored","scope":"All","value":"s3cr3t","description":"` + wantDesc + `"}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/env-secrets", strings.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create secret = %d, want 201: %s", resp.StatusCode, b)
	}
	var created struct {
		ID          string `json:"id"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	// The 201 response is built by re-reading the row (Service.Get), so a correct
	// description here proves both the INSERT and the SELECT carry the column.
	if created.Description != wantDesc {
		t.Errorf("create response description = %q, want %q", created.Description, wantDesc)
	}

	// The list endpoint must also surface the description (Service.List path).
	listResp, err := client.Get(ts.URL + "/api/v1/env-secrets")
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	defer listResp.Body.Close()
	var list []struct {
		ID          string `json:"id"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var found bool
	for _, s := range list {
		if s.ID == created.ID {
			found = true
			if s.Description != wantDesc {
				t.Errorf("list description = %q, want %q", s.Description, wantDesc)
			}
		}
	}
	if !found {
		t.Fatalf("created secret %s not present in list", created.ID)
	}

	// Updating the description must persist the new value (Service.Update path).
	const newDesc = "Rotated — now used by the DR backup job"
	updBody := `{"key":"BACKUP_TOKEN","source":"stored","scope":"All","value":"s3cr3t","description":"` + newDesc + `"}`
	upReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/env-secrets/"+created.ID, strings.NewReader(updBody))
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("X-CSRF-Token", csrf)
	upResp, err := client.Do(upReq)
	if err != nil {
		t.Fatalf("update secret: %v", err)
	}
	defer upResp.Body.Close()
	if upResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(upResp.Body)
		t.Fatalf("update secret = %d, want 200: %s", upResp.StatusCode, b)
	}
	var updated struct {
		Description string `json:"description"`
	}
	if err := json.NewDecoder(upResp.Body).Decode(&updated); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if updated.Description != newDesc {
		t.Errorf("update response description = %q, want %q", updated.Description, newDesc)
	}
}

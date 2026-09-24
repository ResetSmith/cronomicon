package runner

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// mintToken drives HandleMintRegistrationToken directly (the route's
// requirePerm guard is middleware; identity is attribution-only here).
func mintToken(t *testing.T, svc *Service, label string) (id int64, plaintext string) {
	t.Helper()
	body := "{}"
	if label != "" {
		b, _ := json.Marshal(map[string]string{"label": label})
		body = string(b)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/registration-tokens",
		strings.NewReader(body))
	rec := httptest.NewRecorder()
	svc.HandleMintRegistrationToken(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode mint response: %v", err)
	}
	if resp.Token == "" || !strings.HasPrefix(resp.Token, tokenPrefix) {
		t.Fatalf("mint returned no usable plaintext: %q", resp.Token)
	}
	return resp.ID, resp.Token
}

func registerWith(t *testing.T, svc *Service, token, name string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"os":"Linux","capabilities":["bash"],"version":"1.0","protocolVersion":%d}`, name, runnerproto.ProtocolVersion)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	svc.HandleRegisterRunner(rec, req)
	return rec
}

// TestMintListRevoke: mint returns plaintext once with the label; the list
// never carries plaintext and reflects status; revoke kills an unused token
// (and registration with it then fails); revoking a used token is 409.
func TestMintListRevoke(t *testing.T) {
	svc := newTestService(t)

	id1, tok1 := mintToken(t, svc, "web-01")
	id2, tok2 := mintToken(t, svc, "")

	// Minting must NOT revoke other tokens (multiple active rows, Phase 7).
	list := listTokens(t, svc)
	if len(list) != 2 {
		t.Fatalf("list: got %d rows, want 2", len(list))
	}
	for _, item := range list {
		if item.Status != "pending" {
			t.Errorf("token %d status = %q, want pending (mint must not revoke siblings)", item.ID, item.Status)
		}
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), tok1) || strings.Contains(string(raw), tok2) {
		t.Fatalf("list response leaked a plaintext token")
	}
	// Label round-trips.
	var found bool
	for _, item := range list {
		if item.ID == id1 && item.Label != nil && *item.Label == "web-01" {
			found = true
		}
	}
	if !found {
		t.Errorf("minted label not present in list: %s", raw)
	}

	// Revoke the unused token 2 → registration with it fails.
	if got := revokeToken(t, svc, id2).Code; got != http.StatusNoContent {
		t.Fatalf("revoke unused: got %d, want 204", got)
	}
	if got := registerWith(t, svc, tok2, "r-revoked").Code; got != http.StatusUnauthorized {
		t.Errorf("register with revoked token: got %d, want 401", got)
	}

	// Use token 1, then revoking it is 409 (the row is the audit record).
	if got := registerWith(t, svc, tok1, "web-01").Code; got != http.StatusCreated {
		t.Fatalf("register with minted token: got %d, want 201", got)
	}
	rec := revokeToken(t, svc, id1)
	if rec.Code != http.StatusConflict {
		t.Errorf("revoke used token: got %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}

	// Unknown id → 404.
	if got := revokeToken(t, svc, 99999).Code; got != http.StatusNotFound {
		t.Errorf("revoke unknown: got %d, want 404", got)
	}

	// A present-but-malformed body is rejected (422), not silently minted
	// unlabeled — an empty body stays fine (covered by the unlabeled mint above).
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/registration-tokens",
		strings.NewReader(`{"label": web-01}`))
	rec = httptest.NewRecorder()
	svc.HandleMintRegistrationToken(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("malformed mint body: got %d, want 422", rec.Code)
	}
}

// TestSingleUseConsumption: the first registration consumes the token and
// records the audit trail on both sides; the second registration fails with
// the distinct token_used error naming the consuming runner — and distinct
// from the expired error.
func TestSingleUseConsumption(t *testing.T) {
	svc := newTestService(t)
	id, tok := mintToken(t, svc, "db-runner")

	rec := registerWith(t, svc, tok, "db-runner-01")
	if rec.Code != http.StatusCreated {
		t.Fatalf("first register: got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var created struct {
		Runner struct {
			ID                  string `json:"id"`
			RegistrationTokenID *int64 `json:"registrationTokenId"`
		} `json:"runner"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Runner.RegistrationTokenID == nil || *created.Runner.RegistrationTokenID != id {
		t.Errorf("runner provenance registrationTokenId = %v, want %d", created.Runner.RegistrationTokenID, id)
	}

	// Token row records the consumer.
	var usedAt, usedBy string
	if err := svc.db.QueryRow(`SELECT used_at, used_by_runner_id FROM registration_tokens WHERE id=?`, id).
		Scan(&usedAt, &usedBy); err != nil {
		t.Fatalf("read token row: %v", err)
	}
	if usedAt == "" || usedBy != created.Runner.ID {
		t.Errorf("audit trail: used_at=%q used_by=%q, want consumer %q", usedAt, usedBy, created.Runner.ID)
	}
	// The list resolves the runner name.
	list := listTokens(t, svc)
	if len(list) != 1 || list[0].Status != "active" ||
		list[0].UsedByRunnerName == nil || *list[0].UsedByRunnerName != "db-runner-01" {
		t.Errorf("list after use: %+v, want status active by db-runner-01", list)
	}

	// Second registration with the same token: distinct error naming the runner.
	rec = registerWith(t, svc, tok, "db-runner-02")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("second register: got %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "token_used") ||
		!strings.Contains(rec.Body.String(), "db-runner-01") {
		t.Errorf("second register error should carry token_used + consumer name, got: %s", rec.Body.String())
	}
	// No second runner row.
	var n int
	_ = svc.db.QueryRow(`SELECT COUNT(*) FROM runners`).Scan(&n)
	if n != 1 {
		t.Errorf("runners count = %d after double-use attempt, want 1", n)
	}

	// Expired is a DIFFERENT distinct error.
	_, tokExp := mintToken(t, svc, "late")
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := svc.db.Exec(`UPDATE registration_tokens SET expires_at=? WHERE used_at IS NULL`, past); err != nil {
		t.Fatalf("expire token: %v", err)
	}
	rec = registerWith(t, svc, tokExp, "late-runner")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "token_expired") {
		t.Errorf("expired register: got %d %s, want 401 token_expired", rec.Code, rec.Body.String())
	}
}

// TestSingleUseRace: N concurrent registrations presenting the same token —
// exactly one wins (the two-runners-one-token race guard is a single atomic
// UPDATE inside the registration transaction).
func TestSingleUseRace(t *testing.T) {
	svc := newTestService(t)
	_, tok := mintToken(t, svc, "contended")

	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = registerWith(t, svc, tok, fmt.Sprintf("racer-%d", i)).Code
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			wins++
		} else if c != http.StatusUnauthorized {
			t.Errorf("unexpected race status %d (want 201 once, 401 otherwise)", c)
		}
	}
	if wins != 1 {
		t.Fatalf("race: %d registrations won with one token, want exactly 1 (codes %v)", wins, codes)
	}
	var rows int
	_ = svc.db.QueryRow(`SELECT COUNT(*) FROM runners`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("race: %d runner rows created from one token, want 1", rows)
	}
}

// TestBootstrapTokenUnchanged: the env bootstrap token stays multi-use and
// records no provenance/consumption (Phase 7 leaves it alone).
func TestBootstrapTokenUnchanged(t *testing.T) {
	svc := newTestService(t)

	for i := range 3 {
		rec := registerWith(t, svc, "test-bootstrap-token", fmt.Sprintf("boot-%d", i))
		if rec.Code != http.StatusCreated {
			t.Fatalf("bootstrap register #%d: got %d, want 201 (multi-use)", i, rec.Code)
		}
		var created struct {
			Runner struct {
				RegistrationTokenID *int64 `json:"registrationTokenId"`
			} `json:"runner"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &created)
		if created.Runner.RegistrationTokenID != nil {
			t.Errorf("bootstrap registration must carry no token provenance, got %v",
				*created.Runner.RegistrationTokenID)
		}
	}
}

// listTokens drives HandleListRegistrationTokens.
func listTokens(t *testing.T, svc *Service) []registrationTokenInfo {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runners/registration-tokens", nil)
	rec := httptest.NewRecorder()
	svc.HandleListRegistrationTokens(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: got %d, want 200", rec.Code)
	}
	var out []registrationTokenInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return out
}

// revokeToken drives HandleRevokeRegistrationToken.
func revokeToken(t *testing.T, svc *Service, id int64) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/api/v1/runners/registration-tokens/%d", id), nil)
	req.SetPathValue("id", fmt.Sprintf("%d", id))
	rec := httptest.NewRecorder()
	svc.HandleRevokeRegistrationToken(rec, req)
	return rec
}

package runner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// setManagedSettings writes managed_settings + bumps settings_version directly
// (bypasses the HTTP layer), like the PATCH endpoint would.
func setManagedSettings(t *testing.T, svc *Service, runnerID, managedJSON string) {
	t.Helper()
	if _, err := svc.db.Exec(`
		UPDATE runners SET managed_settings = ?, settings_version = settings_version + 1
		WHERE id = ?`, managedJSON, runnerID); err != nil {
		t.Fatalf("setManagedSettings: %v", err)
	}
}

// pollDecode drives HandlePoll for the owning runner and decodes the 200 body.
// A 204 returns (nil, 204).
func pollDecode(t *testing.T, svc *Service, as interface {
	RequireRunner(http.Handler) http.Handler
}, runnerID, token, query string) (*runnerproto.PollResponse, int) {
	t.Helper()
	h := as.RequireRunner(http.HandlerFunc(svc.HandlePoll))
	url := "/api/v1/runners/" + runnerID + "/poll"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.SetPathValue("id", runnerID)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return nil, rec.Code
	}
	var pr runnerproto.PollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pr); err != nil {
		t.Fatalf("decode poll response: %v (body=%s)", err, rec.Body.String())
	}
	return &pr, rec.Code
}

func TestEffectiveClaimCaps(t *testing.T) {
	got := effectiveClaimCaps([]string{"bash", "python", "ansible"}, []string{"python"})
	want := map[string]bool{"bash": true, "ansible": true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Errorf("mask subtract: got %v, want bash+ansible", got)
	}
	// Empty mask is a no-op (same order).
	if same := effectiveClaimCaps([]string{"bash", "python"}, nil); len(same) != 2 {
		t.Errorf("nil mask must not narrow: %v", same)
	}
}

func TestPollDeliversSettingsThenStopsOnAck(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	id, tok := "runner-s", "crn_run_s"
	insertRunner(t, svc, id, "s", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, id)
	setManagedSettings(t, svc, id, `{"maxConcurrent":8}`) // version → 1

	// A settings-capable agent that has applied nothing (settingsVersion=0) gets
	// the payload immediately, no long-poll for work.
	pr, code := pollDecode(t, svc, as, id, tok, "settingsVersion=0")
	if code != http.StatusOK || pr.Settings == nil {
		t.Fatalf("expected settings delivered, got code=%d settings=%v", code, pr.Settings)
	}
	if pr.Settings.Version != 1 || pr.Settings.Values.MaxConcurrent == nil || *pr.Settings.Values.MaxConcurrent != 8 {
		t.Fatalf("wrong settings payload: %+v", pr.Settings)
	}

	// The ack (settings_acked_version) was recorded from the param.
	var acked int
	_ = svc.db.QueryRow(`SELECT settings_acked_version FROM runners WHERE id = ?`, id).Scan(&acked)
	if acked != 0 {
		t.Fatalf("acked should still be 0 (agent sent 0), got %d", acked)
	}

	// Once the agent acks version 1, the server stops sending settings. Give it
	// a claimable run so this poll returns immediately (assignment) and assert
	// no settings ride it — otherwise it would long-poll for work.
	insertQueuedRun(t, svc, "q-s", "s-job", "bash", "")
	pr, code = pollDecode(t, svc, as, id, tok, "settingsVersion=1")
	if code != http.StatusOK || pr.Assignment == nil {
		t.Fatalf("after ack, expected 200 with assignment, got code=%d", code)
	}
	if pr.Settings != nil {
		t.Errorf("settings must NOT be re-sent after the agent acked, got %+v", pr.Settings)
	}
	_ = svc.db.QueryRow(`SELECT settings_acked_version FROM runners WHERE id = ?`, id).Scan(&acked)
	if acked != 1 {
		t.Fatalf("acked should be 1 after the agent echoed version 1, got %d", acked)
	}
}

func TestPollClampsBogusAck(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	id, tok := "runner-c", "crn_run_c"
	insertRunner(t, svc, id, "c", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, id)
	setManagedSettings(t, svc, id, `{"maxConcurrent":8}`) // version → 1

	// A runner that claims to have applied version 999 (buggy or malicious) is
	// treated as caught-up to the current version, not above it: the stored ack
	// is CLAMPED to the version (never a bogus 999 that would corrupt the
	// pending-ack UI), and no settings are re-sent. Give it a claimable run so
	// the poll returns immediately.
	insertQueuedRun(t, svc, "q-c", "c-job", "bash", "")
	pr, code := pollDecode(t, svc, as, id, tok, "settingsVersion=999")
	if code != http.StatusOK || pr.Assignment == nil {
		t.Fatalf("expected 200 with assignment, got code=%d", code)
	}
	if pr.Settings != nil {
		t.Errorf("a caught-up (clamped) ack must not get settings, got %+v", pr.Settings)
	}
	var acked int
	_ = svc.db.QueryRow(`SELECT settings_acked_version FROM runners WHERE id = ?`, id).Scan(&acked)
	if acked != 1 {
		t.Fatalf("stored ack must be clamped to the version (1), not 999, got %d", acked)
	}
}

func TestPollWithholdsSettingsFromOldAgent(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	id, tok := "runner-old", "crn_run_old"
	insertRunner(t, svc, id, "old", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, id)
	setManagedSettings(t, svc, id, `{"maxConcurrent":8}`)

	// A pre-Phase-4 agent sends NO settingsVersion param → it must never receive
	// settings, and its ack column is untouched. Give it a claimable run so the
	// poll returns immediately (no 30s long-poll).
	insertQueuedRun(t, svc, "q-old", "old-job", "bash", "")
	pr, code := pollDecode(t, svc, as, id, tok, "") // no settingsVersion
	if code != http.StatusOK {
		t.Fatalf("expected 200 with assignment, got %d", code)
	}
	if pr.Settings != nil {
		t.Errorf("old agent must not receive settings, got %+v", pr.Settings)
	}
	if pr.Assignment == nil || pr.Assignment.RunType != "bash" {
		t.Errorf("expected the bash assignment, got %+v", pr.Assignment)
	}
	var acked int
	_ = svc.db.QueryRow(`SELECT settings_acked_version FROM runners WHERE id = ?`, id).Scan(&acked)
	if acked != 0 {
		t.Errorf("old agent's ack column must stay 0, got %d", acked)
	}
}

func TestCapabilityMaskSubtractsAtClaim(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	id, tok := "runner-m", "crn_run_m"
	insertRunner(t, svc, id, "m", "online", []string{"bash", "python"})
	bindRunnerToken(t, svc, tok, id)
	setManagedSettings(t, svc, id, `{"capabilityMask":["python"]}`) // version → 1

	// A queued python run + a queued bash run. With python masked, the runner
	// must claim ONLY the bash run.
	insertQueuedRun(t, svc, "q-py", "py-job", "python", "")
	insertQueuedRun(t, svc, "q-bash", "bash-job", "bash", "")

	// First poll delivers settings (immediate). The agent acks on the next poll,
	// which then claims — masked so it grabs bash, never python.
	if pr, _ := pollDecode(t, svc, as, id, tok, "settingsVersion=0"); pr.Settings == nil {
		t.Fatal("expected mask settings delivered first")
	}
	pr, code := pollDecode(t, svc, as, id, tok, "settingsVersion=1")
	if code != http.StatusOK || pr.Assignment == nil {
		t.Fatalf("expected an assignment, got code=%d", code)
	}
	if pr.Assignment.RunType != "bash" {
		t.Errorf("masked runner claimed %q, want bash", pr.Assignment.RunType)
	}
	// The python run must still be queued (never claimed by this runner).
	var status string
	_ = svc.db.QueryRow(`SELECT status FROM runs WHERE id = 'q-py'`).Scan(&status)
	if status != "queued" {
		t.Errorf("masked python run should stay queued, got %q", status)
	}
}

func TestUpdateRunnerSettingsEndpoint(t *testing.T) {
	svc := newTestService(t)
	id := "runner-p"
	insertRunner(t, svc, id, "p", "online", []string{"bash"})

	patch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/runners/"+id+"/settings", strings.NewReader(body))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		svc.HandleUpdateRunnerSettings(rec, req)
		return rec
	}

	// Set maxConcurrent + checkoutRepos → version 1, stored JSON present.
	rec := patch(`{"maxConcurrent":8,"checkoutRepos":["https://gitlab/x.git"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	var ver int
	var raw *string
	_ = svc.db.QueryRow(`SELECT settings_version, managed_settings FROM runners WHERE id = ?`, id).Scan(&ver, &raw)
	if ver != 1 || raw == nil {
		t.Fatalf("after set: version=%d managed=%v", ver, raw)
	}

	// Invalid maxConcurrent → 400, no version bump.
	if rec := patch(`{"maxConcurrent":0}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("maxConcurrent 0 should 400, got %d", rec.Code)
	}
	_ = svc.db.QueryRow(`SELECT settings_version FROM runners WHERE id = ?`, id).Scan(&ver)
	if ver != 1 {
		t.Fatalf("invalid patch must not bump version, got %d", ver)
	}

	// Empty object clears to NULL but still bumps the version (agent reverts).
	if rec := patch(`{}`); rec.Code != http.StatusOK {
		t.Fatalf("clear: %d", rec.Code)
	}
	_ = svc.db.QueryRow(`SELECT settings_version, managed_settings FROM runners WHERE id = ?`, id).Scan(&ver, &raw)
	if ver != 2 || raw != nil {
		t.Fatalf("after clear: version=%d managed=%v (want 2, NULL)", ver, raw)
	}

	// Bad capability-mask entry → 400.
	if rec := patch(`{"capabilityMask":["not-a-runtype"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mask should 400, got %d", rec.Code)
	}
}

func TestManagedSettingsDoNotFlapDrift(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	id, tok := "runner-d", "crn_run_d"
	insertRunner(t, svc, id, "d", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, id)

	// Give the runner a stored declared-config digest and mark it v4 so drift
	// detection is active. A managed override to maxConcurrent must NOT change
	// the declared digest nor trigger a re-register.
	digest := runnerproto.ConfigDigest("d", "Linux", []string{"bash"}, 5, "cronomicon", "1.0", runnerproto.ProtocolVersion)
	if _, err := svc.db.Exec(`UPDATE runners SET config_digest = ?, protocol_version = ? WHERE id = ?`, digest, runnerproto.ProtocolVersion, id); err != nil {
		t.Fatal(err)
	}
	setManagedSettings(t, svc, id, `{"maxConcurrent":99}`) // managed != declared 5

	// Poll with the agent's DECLARED digest (still 5-based) and ack the settings
	// so the response isn't a settings delivery. Expect settings once, then a
	// drift-free poll.
	if pr, _ := pollDecode(t, svc, as, id, tok, "configDigest="+digest+"&settingsVersion=0"); pr.Settings == nil {
		t.Fatal("expected settings delivered first")
	}
	insertQueuedRun(t, svc, "q-d", "d-job", "bash", "")
	pr, code := pollDecode(t, svc, as, id, tok, "configDigest="+digest+"&settingsVersion=1")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	for _, c := range pr.Control {
		if c.Op == "re-register" {
			t.Error("managed maxConcurrent override wrongly triggered a drift re-register")
		}
	}
}

package runner

import (
	"context"
	"encoding/json"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// seedPlacedRunner inserts a runner that is actually placed: two agencies, tags,
// capabilities and an observed client IP — i.e. everything a re-registering
// runner would silently lose.
func seedPlacedRunner(t *testing.T, svc *Service, id, name string) {
	t.Helper()
	if _, err := svc.db.Exec(
		`INSERT INTO agencies (id, name, created_at) VALUES ('ag-carson','Carson','2026-08-26T00:00:00Z')
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed agency: %v", err)
	}
	if _, err := svc.db.Exec(
		`INSERT INTO agencies (id, name, created_at) VALUES ('ag-reno','Reno','2026-08-26T00:00:00Z')
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed agency: %v", err)
	}
	if _, err := svc.db.Exec(`
		INSERT INTO runners (id, name, status, os, capabilities, load, tags, last_client_ip,
		                     protocol_version, registered_at, created_at)
		VALUES (?, ?, 'online', 'Linux', '["ansible","bash"]', 0, '["rh8","prod"]', '10.142.11.7',
		        ?, '2026-08-26T00:00:00Z', '2026-08-26T00:00:00Z')`, id, name, runnerproto.ProtocolVersion); err != nil {
		t.Fatalf("seed runner: %v", err)
	}
	for _, ag := range []string{"ag-carson", "ag-reno"} {
		if _, err := svc.db.Exec(
			`INSERT INTO runner_agencies (runner_id, agency_id) VALUES (?, ?)`, id, ag); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
}

type placementRow struct {
	name, tags, caps, via, by string
	agencies                  []string
	lastIP                    *string
}

func readPlacement(t *testing.T, svc *Service, runnerID string) placementRow {
	t.Helper()
	var p placementRow
	var agencyJSON string
	if err := svc.db.QueryRow(`
		SELECT name, agency_ids, tags, capabilities, last_client_ip, deregistered_via, deregistered_by
		  FROM runner_placement_history WHERE runner_id = ?`, runnerID,
	).Scan(&p.name, &agencyJSON, &p.tags, &p.caps, &p.lastIP, &p.via, &p.by); err != nil {
		t.Fatalf("read placement history: %v", err)
	}
	if err := json.Unmarshal([]byte(agencyJSON), &p.agencies); err != nil {
		t.Fatalf("agency_ids is not JSON: %q", agencyJSON)
	}
	return p
}

// The ordering guard, and the whole reason this table exists. runner_agencies
// carries ON DELETE CASCADE on runner_id, so a capture written after the DELETE
// would record an empty agency set every time — and would look like "this runner
// had no placement" rather than like a bug.
func TestDeregisterCapturesPlacementBeforeTheCascade(t *testing.T) {
	svc := newTestService(t)
	seedPlacedRunner(t, svc, "r1", "ansible-rh8")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/runners/r1", nil)
	req.SetPathValue("id", "r1")
	rec := httptest.NewRecorder()
	svc.HandleDeregisterRunner(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("deregister = %d, want 204 (body %s)", rec.Code, rec.Body)
	}

	// The runner is gone and its membership cascaded away...
	var live int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM runner_agencies WHERE runner_id='r1'`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("expected membership to cascade, %d rows remain", live)
	}

	// ...but the snapshot kept it.
	p := readPlacement(t, svc, "r1")
	if p.name != "ansible-rh8" {
		t.Errorf("name = %q", p.name)
	}
	if len(p.agencies) != 2 || p.agencies[0] != "ag-carson" || p.agencies[1] != "ag-reno" {
		t.Errorf("agency_ids = %v, want both memberships", p.agencies)
	}
	if p.tags != `["rh8","prod"]` {
		t.Errorf("tags = %q", p.tags)
	}
	if p.caps != `["ansible","bash"]` {
		t.Errorf("capabilities = %q", p.caps)
	}
	if p.lastIP == nil || *p.lastIP != "10.142.11.7" {
		t.Errorf("last_client_ip = %v, want the observed address", p.lastIP)
	}
	if p.via != deregisterViaOperator {
		t.Errorf("deregistered_via = %q, want %q", p.via, deregisterViaOperator)
	}
}

// The reaper path matters MORE for disaster recovery than the operator one: in
// an outage runners go offline, are reaped here, and return to a server with no
// record of them. A capture on one path but not the other ships half-working.
func TestReaperCapturesPlacement(t *testing.T) {
	svc := newTestService(t)
	seedPlacedRunner(t, svc, "r2", "ansible-reno")

	svc.deregisterRunner(context.Background(), "r2", "ansible-reno")

	var n int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM runners WHERE id='r2'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("reaper should have deleted the runner")
	}
	p := readPlacement(t, svc, "r2")
	if len(p.agencies) != 2 {
		t.Errorf("reaper capture lost the agency set: %v", p.agencies)
	}
	if p.via != deregisterViaReaper {
		t.Errorf("deregistered_via = %q, want %q", p.via, deregisterViaReaper)
	}
	if p.by != "system" {
		t.Errorf("deregistered_by = %q, want system", p.by)
	}
}

// A runner in no agency must record "[]", not NULL and not "null" — the column
// is NOT NULL and a reader should never have to handle two spellings of empty.
func TestCapturePlacementRecordsEmptyAgencySetAsJSONArray(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.db.Exec(`
		INSERT INTO runners (id, name, status, os, capabilities, load, registered_at, created_at)
		VALUES ('r3','unplaced','online','Linux','[]',0,'2026-08-26T00:00:00Z','2026-08-26T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	svc.deregisterRunner(context.Background(), "r3", "unplaced")

	var agencyJSON string
	var lastIP *string
	if err := svc.db.QueryRow(
		`SELECT agency_ids, last_client_ip FROM runner_placement_history WHERE runner_id='r3'`,
	).Scan(&agencyJSON, &lastIP); err != nil {
		t.Fatalf("read placement: %v", err)
	}
	if agencyJSON != "[]" {
		t.Errorf("agency_ids = %q, want []", agencyJSON)
	}
	if lastIP != nil {
		t.Errorf("last_client_ip should be NULL when never observed, got %q", *lastIP)
	}
}

// ── DR-7 (c): suggestions and acceptance ─────────────────────────────────────

// reRegister simulates what a runner does after losing its identity: a NEW id,
// the same self-declared name, no agencies, no tags.
func reRegister(t *testing.T, svc *Service, newID, name, ip string) {
	t.Helper()
	if _, err := svc.db.Exec(`
		INSERT INTO runners (id, name, status, os, capabilities, load, tags, last_client_ip,
		                     registered_at, created_at)
		VALUES (?, ?, 'online', 'Linux', '["ansible","bash"]', 0, '[]', ?,
		        '2026-08-26T01:00:00Z','2026-08-26T01:00:00Z')`, newID, name, nullStrOrNil(ip)); err != nil {
		t.Fatalf("re-register: %v", err)
	}
}

func TestSuggestionOfferedToAnUnboundRunner(t *testing.T) {
	svc := newTestService(t)
	seedPlacedRunner(t, svc, "old", "ansible-rh8")
	svc.deregisterRunner(context.Background(), "old", "ansible-rh8")
	reRegister(t, svc, "new", "ansible-rh8", "10.142.11.7") // same address

	got, err := suggestionsFor(context.Background(), svc.db, map[string]string{"new": "ansible-rh8"})
	if err != nil {
		t.Fatal(err)
	}
	s := got["new"]
	if s == nil {
		t.Fatal("expected a suggestion for the re-registered runner")
	}
	if len(s.Agencies) != 2 {
		t.Errorf("agencies = %v, want both", s.Agencies)
	}
	if !s.ClientIPMatches {
		t.Error("same observed address should register as a match")
	}
	if s.PreviousRunnerID != "old" {
		t.Errorf("previousRunnerId = %q", s.PreviousRunnerID)
	}
}

// DR-Q7: a differing address does NOT suppress the offer — a host rebuilt during
// recovery lands on a new lease — but it must be visible as a weaker match.
func TestSuggestionSurvivesAnAddressChangeButFlagsIt(t *testing.T) {
	svc := newTestService(t)
	seedPlacedRunner(t, svc, "old", "ansible-rh8")
	svc.deregisterRunner(context.Background(), "old", "ansible-rh8")
	reRegister(t, svc, "new", "ansible-rh8", "10.231.86.4") // rebuilt elsewhere

	got, _ := suggestionsFor(context.Background(), svc.db, map[string]string{"new": "ansible-rh8"})
	s := got["new"]
	if s == nil {
		t.Fatal("an address change must not suppress the offer")
	}
	if s.ClientIPMatches {
		t.Error("a different address must not read as a match")
	}
	if s.PreviousClientIP == nil || *s.PreviousClientIP != "10.142.11.7" {
		t.Errorf("the previous address must be shown for comparison: %v", s.PreviousClientIP)
	}
}

func TestNoSuggestionForAPlacedRunnerOrAnUnknownName(t *testing.T) {
	svc := newTestService(t)
	seedPlacedRunner(t, svc, "old", "ansible-rh8")
	svc.deregisterRunner(context.Background(), "old", "ansible-rh8")
	reRegister(t, svc, "stranger", "never-seen", "")

	got, _ := suggestionsFor(context.Background(), svc.db, map[string]string{"stranger": "never-seen"})
	if got["stranger"] != nil {
		t.Error("a name with no history must get no offer")
	}
	// A placed runner is simply not in the unbound set the caller passes.
	if len(got) != 0 {
		t.Errorf("unexpected offers: %v", got)
	}
}

func TestApplyPlacementRestoresAgenciesAndTags(t *testing.T) {
	svc := newTestService(t)
	seedPlacedRunner(t, svc, "old", "ansible-rh8")
	svc.deregisterRunner(context.Background(), "old", "ansible-rh8")
	reRegister(t, svc, "new", "ansible-rh8", "10.142.11.7")

	var histID int64
	if err := svc.db.QueryRow(`SELECT id FROM runner_placement_history WHERE runner_id='old'`).Scan(&histID); err != nil {
		t.Fatal(err)
	}
	allow := func(string) bool { return true }
	if err := svc.ApplyPlacement(context.Background(), "new", histID, "ops@example", allow); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var n int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM runner_agencies WHERE runner_id='new'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("restored %d memberships, want 2", n)
	}
	var tags string
	if err := svc.db.QueryRow(`SELECT tags FROM runners WHERE id='new'`).Scan(&tags); err != nil {
		t.Fatal(err)
	}
	if tags != `["rh8","prod"]` {
		t.Errorf("tags = %q", tags)
	}

	// Idempotence of a sort: the runner is now placed, so the same offer must not
	// apply twice.
	if err := svc.ApplyPlacement(context.Background(), "new", histID, "ops@example", allow); err != ErrPlacementGone {
		t.Errorf("second apply should be ErrPlacementGone, got %v", err)
	}
}

// DR-Q7's anti-amplification rule: EVERY agency in the snapshot must be one the
// caller could have granted by hand. One refusal fails the whole accept — a
// partial placement would be worse than none, because it looks placed.
func TestApplyPlacementRefusesAnAgencyTheCallerCannotGrant(t *testing.T) {
	svc := newTestService(t)
	seedPlacedRunner(t, svc, "old", "ansible-rh8")
	svc.deregisterRunner(context.Background(), "old", "ansible-rh8")
	reRegister(t, svc, "new", "ansible-rh8", "10.142.11.7")

	var histID int64
	if err := svc.db.QueryRow(`SELECT id FROM runner_placement_history WHERE runner_id='old'`).Scan(&histID); err != nil {
		t.Fatal(err)
	}
	// Carson yes, Reno no — a departmental operator.
	permits := func(agencyID string) bool { return agencyID == "ag-carson" }
	if err := svc.ApplyPlacement(context.Background(), "new", histID, "ops@example", permits); err != ErrPlacementForbidden {
		t.Fatalf("expected ErrPlacementForbidden, got %v", err)
	}
	var n int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM runner_agencies WHERE runner_id='new'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a refused accept must apply NOTHING, found %d membership(s)", n)
	}
}

// A pasted historyId belonging to a different runner name must not place this
// runner into an agency it was never associated with. The offer is honest; the
// endpoint has to be too.
func TestApplyPlacementRefusesASnapshotForAnotherName(t *testing.T) {
	svc := newTestService(t)
	seedPlacedRunner(t, svc, "old", "ansible-rh8")
	svc.deregisterRunner(context.Background(), "old", "ansible-rh8")
	reRegister(t, svc, "other", "totally-different", "")

	var histID int64
	if err := svc.db.QueryRow(`SELECT id FROM runner_placement_history WHERE runner_id='old'`).Scan(&histID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ApplyPlacement(context.Background(), "other", histID, "ops@example", func(string) bool { return true }); err != ErrPlacementGone {
		t.Fatalf("expected ErrPlacementGone for a mismatched name, got %v", err)
	}
}

// ── DRF-1..4: audit, tag union, dismiss ──────────────────────────────────────

func activityRows(t *testing.T, svc *Service, runnerName string) []string {
	t.Helper()
	rows, err := svc.db.Query(
		`SELECT actor || '|' || summary FROM activity WHERE runner_name = ? ORDER BY id`, runnerName)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func seedHistory(t *testing.T, svc *Service) int64 {
	t.Helper()
	seedPlacedRunner(t, svc, "old", "ansible-rh8")
	svc.deregisterRunner(context.Background(), "old", "ansible-rh8")
	var id int64
	if err := svc.db.QueryRow(`SELECT id FROM runner_placement_history WHERE runner_id='old'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// DRF-4 / DRF-Q5: both deletion paths leave a feed row, and the reaper's names
// the window so a timeout reads differently from a deliberate removal.
func TestDeregistrationIsAudited(t *testing.T) {
	svc := newTestService(t)
	seedPlacedRunner(t, svc, "r-op", "by-operator")
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/runners/r-op", nil)
	req.SetPathValue("id", "r-op")
	svc.HandleDeregisterRunner(httptest.NewRecorder(), req)

	got := activityRows(t, svc, "by-operator")
	if len(got) != 1 || !strings.HasSuffix(got[0], "|deregistered") {
		t.Errorf("operator deregister should write one 'deregistered' row, got %v", got)
	}

	seedPlacedRunner(t, svc, "r-reap", "by-reaper")
	svc.deregisterRunner(context.Background(), "r-reap", "by-reaper")
	got = activityRows(t, svc, "by-reaper")
	if len(got) != 1 || !strings.HasPrefix(got[0], "system|deregistered (offline past ") {
		t.Errorf("reaper deregister should write a system row naming the window, got %v", got)
	}
}

// DRF-1: an accept leaves a feed row carrying the session actor, and it is in
// the same transaction as the placement.
func TestAcceptIsAudited(t *testing.T) {
	svc := newTestService(t)
	histID := seedHistory(t, svc)
	reRegister(t, svc, "new", "ansible-rh8", "10.142.11.7")

	if err := svc.ApplyPlacement(context.Background(), "new", histID, "alice@example", func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	got := activityRows(t, svc, "ansible-rh8")
	// The reaper's deregistration row from seedHistory comes first.
	if len(got) != 2 || !strings.HasPrefix(got[1], "alice@example|placement restored: ") {
		t.Errorf("accept should append one row with the session actor, got %v", got)
	}
	if !strings.Contains(got[1], "Carson") || !strings.Contains(got[1], "Reno") {
		t.Errorf("the row should name the restored agencies, got %q", got[1])
	}
}

// DRF-2 / DRF-Q3: tags the operator set on the NEW runner survive; the restored
// ones are appended; duplicates collapse case-insensitively, first casing wins.
func TestAcceptUnionsTagsCurrentFirst(t *testing.T) {
	svc := newTestService(t)
	histID := seedHistory(t, svc) // snapshot tags: ["rh8","prod"]
	reRegister(t, svc, "new", "ansible-rh8", "10.142.11.7")
	if _, err := svc.db.Exec(`UPDATE runners SET tags = '["x","RH8"]' WHERE id='new'`); err != nil {
		t.Fatal(err)
	}

	if err := svc.ApplyPlacement(context.Background(), "new", histID, "ops@example", func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	var tags string
	if err := svc.db.QueryRow(`SELECT tags FROM runners WHERE id='new'`).Scan(&tags); err != nil {
		t.Fatal(err)
	}
	if tags != `["x","RH8","prod"]` {
		t.Errorf("tags = %s, want current first, restored appended, RH8/rh8 collapsed to the current casing", tags)
	}
}

// DRF-3 / DRF-Q4: a dismissal withdraws the snapshot from every runner, is
// audited, and a repeat is a clean refusal rather than a second row.
func TestDismissWithdrawsTheSnapshot(t *testing.T) {
	svc := newTestService(t)
	histID := seedHistory(t, svc)
	reRegister(t, svc, "new", "ansible-rh8", "10.142.11.7")

	if err := svc.DismissPlacement(context.Background(), "new", histID, "alice@example"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	got, _ := suggestionsFor(context.Background(), svc.db, map[string]string{"new": "ansible-rh8"})
	if got["new"] != nil {
		t.Error("a dismissed snapshot must not be offered")
	}
	// Nor accepted by a stale client that still holds the id.
	if err := svc.ApplyPlacement(context.Background(), "new", histID, "ops@example", func(string) bool { return true }); err != ErrPlacementGone {
		t.Errorf("accept of a dismissed snapshot should be ErrPlacementGone, got %v", err)
	}
	if err := svc.DismissPlacement(context.Background(), "new", histID, "alice@example"); err != ErrPlacementGone {
		t.Errorf("second dismiss should be ErrPlacementGone, got %v", err)
	}
	rows := activityRows(t, svc, "ansible-rh8")
	if len(rows) != 2 || rows[1] != "alice@example|placement suggestion dismissed" {
		t.Errorf("exactly one dismissal row expected, got %v", rows)
	}
}

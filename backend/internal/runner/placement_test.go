package runner

import (
	"context"
	"encoding/json"
	"errors"
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
	// A scope of each agency, bound to this runner: what a re-enrolled agent's
	// new id will not hold. (Scope ids are per runner so two seeded runners in
	// one test do not collide.)
	for _, sc := range []struct{ id, name, agency string }{
		{"sc-carson-" + id, "carson-web-" + id, "ag-carson"},
		{"sc-reno-" + id, "reno-web-" + id, "ag-reno"},
	} {
		if _, err := svc.db.Exec(
			`INSERT INTO scopes(id, name, source, created_at) VALUES(?, ?, 'cronomicon', '2026-08-26T00:00:00Z')`,
			sc.id, sc.name); err != nil {
			t.Fatalf("seed scope: %v", err)
		}
		if _, err := svc.db.Exec(
			`INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, sc.id, sc.agency); err != nil {
			t.Fatalf("seed scope agency: %v", err)
		}
		if _, err := svc.db.Exec(
			`INSERT INTO scope_runners(scope_id, runner_id, runner_name, bound_by, bound_at)
			 VALUES(?, ?, ?, 'test', '2026-08-26T00:00:00Z')`, sc.id, id, name); err != nil {
			t.Fatalf("seed binding: %v", err)
		}
	}
}

// servedBy lists the agencies a runner serves, sorted.
func servedBy(t *testing.T, svc *Service, runnerID string) []string {
	t.Helper()
	rows, err := svc.db.Query(`SELECT agency_id FROM runner_agencies WHERE runner_id = ? ORDER BY agency_id`, runnerID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// boundTo lists the scope ids bound to a runner id, sorted.
func boundTo(t *testing.T, svc *Service, runnerID string) []string {
	t.Helper()
	rows, err := svc.db.Query(`SELECT scope_id FROM scope_runners WHERE runner_id = ? ORDER BY scope_id`, runnerID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

type placementRow struct {
	name, tags, caps, via, by, owner string
	agencies                         []string
	lastIP                           *string
}

func readPlacement(t *testing.T, svc *Service, runnerID string) placementRow {
	t.Helper()
	var p placementRow
	var agencyJSON string
	if err := svc.db.QueryRow(`
		SELECT name, agency_ids, tags, capabilities, last_client_ip, deregistered_via, deregistered_by, owner_agency
		  FROM runner_placement_history WHERE runner_id = ?`, runnerID,
	).Scan(&p.name, &agencyJSON, &p.tags, &p.caps, &p.lastIP, &p.via, &p.by, &p.owner); err != nil {
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
	// The snapshot keeps who owned the runner (migration 1250): its record is
	// still that agency's after the row is gone.
	if p.owner != "global" {
		t.Errorf("owner_agency = %q, want global (a runner that served two agencies is Global's)", p.owner)
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

// ── DR-7 (c), MA-32: suggestions and acceptance ─────────────────────────────

// reRegister simulates what an agent does after losing its identity: a NEW id,
// the same self-declared name, no tags — enrolled with a token for `owner`, so
// it is owned by that agency and serves exactly it (the trigger writes the row).
func reRegister(t *testing.T, svc *Service, newID, name, ip string) {
	t.Helper()
	reRegisterFor(t, svc, newID, name, ip, "ag-carson")
}

func reRegisterFor(t *testing.T, svc *Service, newID, name, ip, owner string) {
	t.Helper()
	if _, err := svc.db.Exec(`
		INSERT INTO runners (id, name, status, os, capabilities, load, tags, last_client_ip,
		                     registered_at, created_at, owner_agency)
		VALUES (?, ?, 'online', 'Linux', '["ansible","bash"]', 0, '[]', ?,
		        '2026-08-26T01:00:00Z','2026-08-26T01:00:00Z', ?)`, newID, name, nullStrOrNil(ip), owner); err != nil {
		t.Fatalf("re-register: %v", err)
	}
}

func historyID(t *testing.T, svc *Service, runnerID string) int64 {
	t.Helper()
	var id int64
	if err := svc.db.QueryRow(`SELECT id FROM runner_placement_history WHERE runner_id = ?`, runnerID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSuggestionOfferedToAReEnrolledRunner(t *testing.T) {
	svc := newTestService(t)
	seedPlacedRunner(t, svc, "old", "ansible-rh8")
	svc.deregisterRunner(context.Background(), "old", "ansible-rh8")
	reRegister(t, svc, "new", "ansible-rh8", "10.142.11.7") // same address, Carson's agent

	got, err := suggestionsFor(context.Background(), svc.db)
	if err != nil {
		t.Fatal(err)
	}
	s := got["new"]
	if s == nil {
		t.Fatal("expected a suggestion for the re-registered runner")
	}
	// The offer is for the runner's own agency and the scopes of that agency the
	// old id still holds — not for everything the old runner served.
	if len(s.Agencies) != 1 || s.Agencies[0].ID != "ag-carson" || s.Agencies[0].Name != "Carson" {
		t.Errorf("agencies = %v, want exactly the runner's owner", s.Agencies)
	}
	if len(s.Scopes) != 1 || s.Scopes[0] != "carson-web-old" {
		t.Errorf("scopes = %v, want only Carson's bound scope", s.Scopes)
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

	got, _ := suggestionsFor(context.Background(), svc.db)
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

// No offer for a name with no history, and none for a same-named runner whose
// own agency has nothing bound to the old id: Global's agent claiming the name
// of a runner that served Carson and Reno is offered nothing of theirs.
func TestNoSuggestionForAnUnknownNameOrAnotherOwner(t *testing.T) {
	svc := newTestService(t)
	seedPlacedRunner(t, svc, "old", "ansible-rh8")
	svc.deregisterRunner(context.Background(), "old", "ansible-rh8")
	reRegisterFor(t, svc, "stranger", "never-seen", "", "ag-carson")
	reRegisterFor(t, svc, "globals", "ansible-rh8", "10.142.11.7", "global")

	got, err := suggestionsFor(context.Background(), svc.db)
	if err != nil {
		t.Fatal(err)
	}
	if got["stranger"] != nil {
		t.Error("a name with no history must get no offer")
	}
	if got["globals"] != nil {
		t.Errorf("a Global-owned runner was offered another agency's bindings: %+v", got["globals"])
	}
	if len(got) != 0 {
		t.Errorf("unexpected offers: %v", got)
	}
}

// MA-32: the accept re-points the bindings of the runner's own agency and
// merges the tags. It writes no serve row, and it leaves the bindings the old
// id holds on another agency's scopes exactly where they were.
func TestApplyPlacementRepointsOwnBindingsAndNeverWidens(t *testing.T) {
	svc := newTestService(t)
	seedPlacedRunner(t, svc, "old", "ansible-rh8")
	svc.deregisterRunner(context.Background(), "old", "ansible-rh8")
	reRegister(t, svc, "new", "ansible-rh8", "10.142.11.7")
	histID := historyID(t, svc, "old")

	if err := svc.ApplyPlacement(context.Background(), "new", histID, "ops@example"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := servedBy(t, svc, "new"); len(got) != 1 || got[0] != "ag-carson" {
		t.Errorf("after a restore the runner serves %v, want exactly its owner: a restore never adds a serve row", got)
	}
	if got := boundTo(t, svc, "new"); len(got) != 1 || got[0] != "sc-carson-old" {
		t.Errorf("bound to the new id: %v, want Carson's scope only", got)
	}
	if got := boundTo(t, svc, "old"); len(got) != 1 || got[0] != "sc-reno-old" {
		t.Errorf("left on the old id: %v, want Reno's binding untouched (its scope stays closed until Reno re-points it)", got)
	}
	var tags string
	if err := svc.db.QueryRow(`SELECT tags FROM runners WHERE id='new'`).Scan(&tags); err != nil {
		t.Fatal(err)
	}
	if tags != `["rh8","prod"]` {
		t.Errorf("tags = %q", tags)
	}

	// Nothing of Carson's is left to restore, so the same offer does not apply
	// twice and is no longer made.
	if err := svc.ApplyPlacement(context.Background(), "new", histID, "ops@example"); !errors.Is(err, ErrPlacementGone) {
		t.Errorf("second apply should be ErrPlacementGone, got %v", err)
	}
	if got, _ := suggestionsFor(context.Background(), svc.db); got["new"] != nil {
		t.Errorf("offer still made after it was accepted: %+v", got["new"])
	}
}

// The same snapshot serves each agency's own re-enrolled agent, each for its
// own scopes: Reno's agent takes Reno's binding and cannot reach Carson's.
func TestApplyPlacementGivesEachAgencyItsOwnBindings(t *testing.T) {
	svc := newTestService(t)
	seedPlacedRunner(t, svc, "old", "ansible-rh8")
	svc.deregisterRunner(context.Background(), "old", "ansible-rh8")
	reRegisterFor(t, svc, "reno-new", "ansible-rh8", "", "ag-reno")
	histID := historyID(t, svc, "old")

	got, err := suggestionsFor(context.Background(), svc.db)
	if err != nil {
		t.Fatal(err)
	}
	if s := got["reno-new"]; s == nil || len(s.Scopes) != 1 || s.Scopes[0] != "reno-web-old" {
		t.Fatalf("Reno's agent offer = %+v, want Reno's scope only", got["reno-new"])
	}
	if err := svc.ApplyPlacement(context.Background(), "reno-new", histID, "ops@example"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := boundTo(t, svc, "reno-new"); len(got) != 1 || got[0] != "sc-reno-old" {
		t.Errorf("Reno's agent is bound to %v, want Reno's scope only", got)
	}
	if got := boundTo(t, svc, "old"); len(got) != 1 || got[0] != "sc-carson-old" {
		t.Errorf("Carson's binding moved or vanished: old id holds %v", got)
	}
	if got := servedBy(t, svc, "reno-new"); len(got) != 1 || got[0] != "ag-reno" {
		t.Errorf("Reno's agent serves %v after the restore, want exactly ag-reno", got)
	}
}

// A runner that does not serve its owner (a legacy placement owned by Global
// that serves two agencies and not Global) is offered nothing and can accept
// nothing: a binding moved to it would name a runner that cannot claim the
// scope's runs.
func TestApplyPlacementSkipsARunnerThatDoesNotServeItsOwner(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	seedPlacedRunner(t, svc, "old", "ansible-rh8")
	if _, err := svc.db.Exec(`
		INSERT INTO scopes(id, name, source, created_at) VALUES('sc-global','global-web','cronomicon','2026-08-26T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	bindScope(t, svc, "sc-global", "old", "ansible-rh8")
	svc.deregisterRunner(ctx, "old", "ansible-rh8")
	seedPlacedRunner(t, svc, "legacy", "ansible-rh8") // Global's, serves Carson and Reno

	got, err := suggestionsFor(ctx, svc.db)
	if err != nil {
		t.Fatal(err)
	}
	if got["legacy"] != nil {
		t.Errorf("a legacy placement was offered a restore: %+v", got["legacy"])
	}
	if err := svc.ApplyPlacement(ctx, "legacy", historyID(t, svc, "old"), "ops@example"); !errors.Is(err, ErrPlacementGone) {
		t.Errorf("apply onto a legacy placement = %v, want ErrPlacementGone", err)
	}
}

// A pasted historyId belonging to a different runner name must not hand this
// runner another runner's bindings. The offer is honest; the endpoint has to be
// too.
func TestApplyPlacementRefusesASnapshotForAnotherName(t *testing.T) {
	svc := newTestService(t)
	seedPlacedRunner(t, svc, "old", "ansible-rh8")
	svc.deregisterRunner(context.Background(), "old", "ansible-rh8")
	reRegister(t, svc, "other", "totally-different", "")

	if err := svc.ApplyPlacement(context.Background(), "other", historyID(t, svc, "old"), "ops@example"); !errors.Is(err, ErrPlacementGone) {
		t.Fatalf("expected ErrPlacementGone for a mismatched name, got %v", err)
	}
	if got := boundTo(t, svc, "other"); len(got) != 0 {
		t.Errorf("a refused accept moved bindings: %v", got)
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

	if err := svc.ApplyPlacement(context.Background(), "new", histID, "alice@example"); err != nil {
		t.Fatal(err)
	}
	got := activityRows(t, svc, "ansible-rh8")
	// The reaper's deregistration row from seedHistory comes first.
	if len(got) != 2 || !strings.HasPrefix(got[1], "alice@example|placement restored: ") {
		t.Errorf("accept should append one row with the session actor, got %v", got)
	}
	if !strings.Contains(got[1], "carson-web-old") || strings.Contains(got[1], "reno-web-old") {
		t.Errorf("the row should name the scopes it re-pointed, and only those, got %q", got[1])
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

	if err := svc.ApplyPlacement(context.Background(), "new", histID, "ops@example"); err != nil {
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
	// Reno has re-pointed its own scope by hand, so what the snapshot still
	// holds is Carson's alone, and Carson's to dismiss.
	if _, err := svc.db.Exec(`DELETE FROM scope_runners WHERE scope_id = 'sc-reno-old'`); err != nil {
		t.Fatal(err)
	}

	if err := svc.DismissPlacement(context.Background(), "new", histID, "alice@example"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	got, _ := suggestionsFor(context.Background(), svc.db)
	if got["new"] != nil {
		t.Error("a dismissed snapshot must not be offered")
	}
	// Nor accepted by a stale client that still holds the id.
	if err := svc.ApplyPlacement(context.Background(), "new", histID, "ops@example"); !errors.Is(err, ErrPlacementGone) {
		t.Errorf("accept of a dismissed snapshot should be ErrPlacementGone, got %v", err)
	}
	if err := svc.DismissPlacement(context.Background(), "new", histID, "alice@example"); !errors.Is(err, ErrPlacementGone) {
		t.Errorf("second dismiss should be ErrPlacementGone, got %v", err)
	}
	rows := activityRows(t, svc, "ansible-rh8")
	if len(rows) != 2 || rows[1] != "alice@example|placement suggestion dismissed" {
		t.Errorf("exactly one dismissal row expected, got %v", rows)
	}
}

// A dismissal marks the SNAPSHOT, so it must not be something any runner of the
// same name can do: the name is the agent's own word, and any agency can enrol
// an agent under any name. It is accepted only for a snapshot on offer to this
// runner, and only when that offer is nobody else's as well.
func TestDismissIsOnlyForAnOfferThatIsThisRunnersAlone(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	histID := seedHistory(t, svc) // the old runner still holds a Carson and a Reno binding
	insertAgencyRow(t, svc, "ag-elko", "Elko")
	reRegisterFor(t, svc, "elko-same-name", "ansible-rh8", "", "ag-elko")
	reRegister(t, svc, "carson-new", "ansible-rh8", "10.142.11.7")

	dismissed := func() bool {
		t.Helper()
		var at *string
		if err := svc.db.QueryRow(`SELECT dismissed_at FROM runner_placement_history WHERE id = ?`, histID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at != nil
	}
	// Another agency's agent that merely carries the name: nothing of Elko's is
	// restorable from this snapshot, so it is not Elko's to dismiss.
	if err := svc.DismissPlacement(ctx, "elko-same-name", histID, "mallory@example"); !errors.Is(err, ErrPlacementGone) {
		t.Errorf("an unrelated agency's same-named agent dismissing the snapshot = %v, want ErrPlacementGone", err)
	}
	// Carson's own agent, while Reno's binding is still on the old id: the
	// offer is Reno's too.
	if err := svc.DismissPlacement(ctx, "carson-new", histID, "carol@example"); !errors.Is(err, ErrPlacementShared) {
		t.Errorf("dismissing an offer another agency shares = %v, want ErrPlacementShared", err)
	}
	if dismissed() {
		t.Fatal("a refused dismissal marked the snapshot")
	}
	if got, _ := suggestionsFor(ctx, svc.db); got["carson-new"] == nil {
		t.Error("a refused dismissal withdrew the offer")
	}
	// Carson accepts: its binding moves, the offer is gone for Carson, and
	// Reno's is untouched and still on offer to a Reno agent.
	if err := svc.ApplyPlacement(ctx, "carson-new", histID, "carol@example"); err != nil {
		t.Fatal(err)
	}
	reRegisterFor(t, svc, "reno-new", "ansible-rh8", "", "ag-reno")
	got, _ := suggestionsFor(ctx, svc.db)
	if got["carson-new"] != nil || got["reno-new"] == nil {
		t.Errorf("after Carson's accept: Carson offered %v, Reno offered %v; want none and one", got["carson-new"], got["reno-new"])
	}
	// What is left is Reno's alone, so Reno may dismiss it; Carson may not.
	if err := svc.DismissPlacement(ctx, "carson-new", histID, "carol@example"); !errors.Is(err, ErrPlacementGone) {
		t.Errorf("Carson dismissing what is now only Reno's = %v, want ErrPlacementGone", err)
	}
	if err := svc.DismissPlacement(ctx, "reno-new", histID, "rita@example"); err != nil {
		t.Errorf("Reno dismissing its own offer: %v", err)
	}
	if !dismissed() {
		t.Error("Reno's dismissal was not recorded")
	}
}

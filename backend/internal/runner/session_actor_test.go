package runner

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/auth"
)

// AA-4 — the three operator-initiated runner-lifecycle handlers used to write
// the LITERAL string "operator" as the activity actor while a session was in
// scope, so "who drained runner X?" could not be answered from the feed. The
// identity was available (auth.IdentityFrom) and thrown away; the host-key
// APPROVAL handler in the same file had always resolved it correctly, which is
// the code sessionActor() is lifted from.
//
// These tests assert the actor that reaches the ROW, not the helper — a fix
// that resolved the identity and then failed to pass it to WriteActivity would
// satisfy a unit test of sessionActor and still leave the feed anonymous.

const testOperator = "ops-lead@corp.example"

// withSession stamps an authenticated identity on the request the way the
// session middleware does before these handlers ever run.
func withSession(req *http.Request, email string) *http.Request {
	return req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{Email: email}))
}

// actorFor returns the actor of the newest activity row matching a summary.
func actorFor(t *testing.T, svc *Service, summaryLike string) string {
	t.Helper()
	var actor string
	err := svc.db.QueryRow(
		`SELECT actor FROM activity WHERE summary LIKE ? ORDER BY id DESC LIMIT 1`,
		summaryLike).Scan(&actor)
	if err != nil {
		t.Fatalf("no activity row matching %q: %v", summaryLike, err)
	}
	return actor
}

func TestDrainRecordsTheSessionActor(t *testing.T) {
	svc := newTestService(t)
	insertRunner(t, svc, "r1", "runner-east", "online", []string{"bash"})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/r1/drain", nil)
	req.SetPathValue("id", "r1")
	rec := httptest.NewRecorder()
	svc.HandleDrain(rec, withSession(req, testOperator))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("drain: got %d, want 202 (%s)", rec.Code, rec.Body.String())
	}

	if got := actorFor(t, svc, "drain initiated"); got != testOperator {
		t.Errorf("drain actor = %q, want %q", got, testOperator)
	}
}

func TestKeyscanRequestRecordsTheSessionActor(t *testing.T) {
	svc := newTestService(t)
	insertRunner(t, svc, "r1", "runner-east", "online", []string{"bash"})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/r1/keyscan",
		strings.NewReader(`{"hosts":["db-01"]}`))
	req.SetPathValue("id", "r1")
	rec := httptest.NewRecorder()
	svc.HandleKeyscan(rec, withSession(req, testOperator))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("keyscan: got %d, want 202 (%s)", rec.Code, rec.Body.String())
	}

	if got := actorFor(t, svc, "host-key scan requested%"); got != testOperator {
		t.Errorf("keyscan actor = %q, want %q", got, testOperator)
	}
}

func TestResyncRequestRecordsTheSessionActor(t *testing.T) {
	svc := newTestService(t)
	insertRunner(t, svc, "r1", "runner-east", "online", []string{"bash"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/r1/resync", nil)
	req.SetPathValue("id", "r1")
	svc.HandleResync(rec, withSession(req, testOperator))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("resync: got %d, want 202 (%s)", rec.Code, rec.Body.String())
	}

	if got := actorFor(t, svc, "resync requested"); got != testOperator {
		t.Errorf("resync actor = %q, want %q", got, testOperator)
	}
}

// The fallback is kept deliberately (these routes are session-gated at the mux,
// so it cannot fire in real traffic) — but a writer that cannot know the actor
// must say so rather than assert a name it guessed, and must never write an
// EMPTY actor: `activity.actor` is the column the AA band's filter keys on.
func TestSessionActorFallsBackWhenThereIsNoSession(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	if got := sessionActor(req); got != "operator" {
		t.Errorf("no session: sessionActor = %q, want %q", got, "operator")
	}
	// An identity carrying no email is not an attribution either.
	blank := req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{}))
	if got := sessionActor(blank); got != "operator" {
		t.Errorf("blank identity: sessionActor = %q, want %q", got, "operator")
	}
	if got := sessionActor(withSession(req, testOperator)); got != testOperator {
		t.Errorf("with session: sessionActor = %q, want %q", got, testOperator)
	}
}

// ── AA-1: runner_name on activity rows ───────────────────────────────────────
//
// The column exists so an operator can filter the feed by a runner they can
// NAME. `actor` carries `runner:<uuid>`, which is filterable but unreadable,
// and the id stops resolving the moment the runner is deregistered — so the
// name is snapshotted at write time rather than joined at read time.
//
// These assert the value on the ROW, like the AA-4 tests above and for the same
// reason: the point of the field is what lands in the database.

func runnerNameFor(t *testing.T, svc *Service, summaryLike string) (string, bool) {
	t.Helper()
	var n *string
	if err := svc.db.QueryRow(
		`SELECT runner_name FROM activity WHERE summary LIKE ? ORDER BY id DESC LIMIT 1`,
		summaryLike).Scan(&n); err != nil {
		t.Fatalf("no activity row matching %q: %v", summaryLike, err)
	}
	if n == nil {
		return "", false
	}
	return *n, true
}

// The three operator-initiated handlers hold the runner row already, so they
// pass the name straight through — no extra query.
func TestRunnerLifecycleRowsCarryTheRunnerName(t *testing.T) {
	for _, tc := range []struct {
		name, summary string
		call          func(t *testing.T, svc *Service)
	}{
		{"drain", "drain initiated", func(t *testing.T, svc *Service) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/r1/drain", nil)
			req.SetPathValue("id", "r1")
			svc.HandleDrain(httptest.NewRecorder(), withSession(req, testOperator))
		}},
		{"keyscan", "host-key scan requested%", func(t *testing.T, svc *Service) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/r1/keyscan",
				strings.NewReader(`{"hosts":["db-01"]}`))
			req.SetPathValue("id", "r1")
			svc.HandleKeyscan(httptest.NewRecorder(), withSession(req, testOperator))
		}},
		{"resync", "resync requested", func(t *testing.T, svc *Service) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/r1/resync", nil)
			req.SetPathValue("id", "r1")
			svc.HandleResync(httptest.NewRecorder(), withSession(req, testOperator))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			insertRunner(t, svc, "r1", "runner-east", "online", []string{"bash"})
			tc.call(t, svc)
			got, ok := runnerNameFor(t, svc, tc.summary)
			if !ok || got != "runner-east" {
				t.Errorf("%s: runner_name = %q (set=%v), want %q", tc.name, got, ok, "runner-east")
			}
		})
	}
}

// The re-admit notice already had the name in scope for its `target`; this pins
// that it reaches the new column too, and that the id-shaped `actor` is left
// alone (the audit stream and downstream log consumers read that value —
// renaming it would be a silent contract change, AA-Q6).
func TestReadmitNoticeCarriesNameAndKeepsTheIdActor(t *testing.T) {
	svc := newTestService(t)
	insertRunner(t, svc, "r1", "runner-east", "offline", []string{"bash"})

	if !svc.readmitRunner(t.Context(), "r1") {
		t.Fatal("readmitRunner returned false for an offline runner")
	}

	const summary = "runner re-admitted on poll (was offline)"
	if got, ok := runnerNameFor(t, svc, summary); !ok || got != "runner-east" {
		t.Errorf("runner_name = %q (set=%v), want %q", got, ok, "runner-east")
	}
	if got := actorFor(t, svc, summary); got != "runner:r1" {
		t.Errorf("actor = %q, want %q — actor keeps its id meaning (AA-Q6)", got, "runner:r1")
	}
}

// A runner deregistered before the lookup leaves the column NULL rather than
// acquiring a guess — the same rule migration 1100's backfill follows.
func TestRunnerNameOfIsEmptyForAnUnknownRunner(t *testing.T) {
	svc := newTestService(t)
	insertRunner(t, svc, "r1", "runner-east", "online", []string{"bash"})

	if got := svc.runnerNameOf(t.Context(), "r1"); got != "runner-east" {
		t.Errorf("known runner: runnerNameOf = %q, want %q", got, "runner-east")
	}
	if got := svc.runnerNameOf(t.Context(), "no-such-runner"); got != "" {
		t.Errorf("unknown runner: runnerNameOf = %q, want \"\" so the column stays NULL", got)
	}
}

// The two run-lifecycle sites hold only the runner ID, so they resolve the name
// through runnerNameOf. run-start is written by the claim; run-end by
// finalizeRun. Both must land the name, because "everything runner X ran" is
// the question the column exists to answer — and a run-start stamped while its
// run-end is not would answer it half way.
func TestRunStartAndRunEndCarryTheRunnerName(t *testing.T) {
	svc := newTestService(t)
	ctx := t.Context()
	insertRunner(t, svc, "r1", "runner-east", "online", []string{"bash"})

	trace := "run-aa1"
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, agencies_json, created_at)
		VALUES (?, 'nightly', 'bash', 'prod', 'queued', 'test', 'manual', 'runner', '[]', ?)`,
		trace, now()); err != nil {
		t.Fatalf("seed queued run: %v", err)
	}

	if _, err := svc.claimRun(ctx, "r1", []string{"bash"}, true); err != nil {
		t.Fatalf("claimRun: %v", err)
	}
	svc.finalizeRun(ctx, trace, "nightly", "prod", "r1", "success", nil, 1200, "")

	rows, err := svc.db.Query(
		`SELECT kind, COALESCE(runner_name,'') FROM activity WHERE trace_id = ? ORDER BY id`, trace)
	if err != nil {
		t.Fatalf("read activity: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[kind] = name
	}
	for _, kind := range []string{"run-start", "run-end"} {
		if got[kind] != "runner-east" {
			t.Errorf("%s: runner_name = %q, want %q", kind, got[kind], "runner-east")
		}
	}
}

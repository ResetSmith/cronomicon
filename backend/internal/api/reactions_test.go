package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	// Aliased: every test here binds a local `api` (the rxAPI client), which
	// would otherwise shadow the package.
	apipkg "github.com/ResetSmith/cronomicon/internal/api"
)

// RX-12 / RX-24 — the reaction authoring surface (Phase C).
//
// Phase B's engine reads the `reactions` table; nothing could write it but SQL.
// These tests fence the authoring rules that keep an authored reaction from
// being one that can never fire, or one that fires forever.

type rxAPI struct {
	t      *testing.T
	ts     string
	client *http.Client
	csrf   string
}

// doRaw returns the status plus the raw body, because several of these
// assertions are about the REFUSAL TEXT: an operator who is told "no" without
// being told which reaction, which upstream, or which cycle has to go read the
// database to find out.
func (c rxAPI) doRaw(method, path string, body any) (int, string) {
	c.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, c.ts+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", c.csrf)
	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func (c rxAPI) list(path string) []map[string]any {
	c.t.Helper()
	code, body := c.doRaw(http.MethodGet, path, nil)
	if code != http.StatusOK {
		c.t.Fatalf("GET %s = %d (%s)", path, code, body)
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		c.t.Fatalf("decode %s: %v (%s)", path, err, body)
	}
	return out
}

func newRxAPI(t *testing.T) (rxAPI, *sql.DB) {
	t.Helper()
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	return rxAPI{t: t, ts: ts.URL, client: client, csrf: csrf}, pool
}

func seedRxJob(t *testing.T, pool *sql.DB, name string) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO jobs (name, uid, source, run_type, enabled, synced_at) VALUES (?, 'uid-'||?, 'amadeus', 'bash', 1, 't')`,
		name, name); err != nil {
		t.Fatalf("seed job %s: %v", name, err)
	}
}

func seedRxWorkflow(t *testing.T, pool *sql.DB, name string) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO workflows (name, source, steps, enabled, synced_at) VALUES (?, 'amadeus', '[]', 1, 't')`,
		name); err != nil {
		t.Fatalf("seed workflow %s: %v", name, err)
	}
}

func rx(name, onKind, onName, outcome string) map[string]any {
	return map[string]any{
		"name": name, "onKind": onKind, "onName": onName,
		"onSource": "amadeus", "onOutcome": outcome,
	}
}

// ── RX-12: the happy path and the round trip ───────────────────────────────

func TestReactionCRUDRoundTrip(t *testing.T) {
	api, pool := newRxAPI(t)
	seedRxJob(t, pool, "extract")
	seedRxJob(t, pool, "load")

	code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/load", map[string]any{
		"reactions": []map[string]any{
			{"name": "after-extract", "onKind": "job", "onName": "extract",
				"onSource": "amadeus", "onOutcome": "success", "delaySeconds": 30,
				"minIntervalSeconds": 300, "includeWorkflowChildren": true},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", code, body)
	}

	got := api.list("/api/v1/reactions/job/load")
	if len(got) != 1 {
		t.Fatalf("got %d reactions, want 1", len(got))
	}
	// Every authored field must survive the round trip. A field that silently
	// fails to read back is the preservedInline hazard: the next save clears it.
	for key, want := range map[string]any{
		"name": "after-extract", "onKind": "job", "onName": "extract",
		"onOutcome": "success", "delaySeconds": float64(30),
		"minIntervalSeconds": float64(300), "includeWorkflowChildren": true,
		"enabled": true, "missing": false,
	} {
		if got[0][key] != want {
			t.Errorf("%s = %v, want %v", key, got[0][key], want)
		}
	}

	// The global edge list sees it too — that is the Reactions tab's query.
	if all := api.list("/api/v1/reactions"); len(all) != 1 {
		t.Errorf("global edge list has %d entries, want 1", len(all))
	}

	// Replace is wholesale: an empty list clears them.
	if code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/load",
		map[string]any{"reactions": []map[string]any{}}); code != http.StatusOK {
		t.Fatalf("clear = %d (%s)", code, body)
	}
	if got := api.list("/api/v1/reactions/job/load"); len(got) != 0 {
		t.Errorf("after clearing, got %d reactions, want 0", len(got))
	}
}

func TestDeleteOneReaction(t *testing.T) {
	api, pool := newRxAPI(t)
	seedRxJob(t, pool, "up")
	seedRxJob(t, pool, "down")
	if code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/down", map[string]any{
		"reactions": []map[string]any{rx("a", "job", "up", "success"), rx("b", "job", "up", "failure")},
	}); code != http.StatusOK {
		t.Fatalf("seed = %d (%s)", code, body)
	}

	if code, body := api.doRaw(http.MethodDelete, "/api/v1/reactions/job/down/a", nil); code != http.StatusNoContent {
		t.Fatalf("delete = %d (%s)", code, body)
	}
	got := api.list("/api/v1/reactions/job/down")
	if len(got) != 1 || got[0]["name"] != "b" {
		t.Errorf("after deleting 'a', got %+v", got)
	}
	if code, _ := api.doRaw(http.MethodDelete, "/api/v1/reactions/job/down/nope", nil); code != http.StatusNotFound {
		t.Errorf("deleting a missing reaction = %d, want 404", code)
	}
}

// A reaction whose upstream no longer exists is DANGLING, not an error: the Git
// prune can create this state and cannot be stopped from doing so. It must read
// back as missing rather than looking like a healthy edge.
func TestDanglingUpstreamIsRenderedMissing(t *testing.T) {
	api, pool := newRxAPI(t)
	seedRxJob(t, pool, "up")
	seedRxJob(t, pool, "down")
	if code, _ := api.doRaw(http.MethodPut, "/api/v1/reactions/job/down", map[string]any{
		"reactions": []map[string]any{rx("r", "job", "up", "success")},
	}); code != http.StatusOK {
		t.Fatal("seed failed")
	}
	// Simulate the sync prune: the upstream vanishes, the reaction survives.
	if _, err := pool.ExecContext(context.Background(), `DELETE FROM jobs WHERE name='up'`); err != nil {
		t.Fatal(err)
	}

	got := api.list("/api/v1/reactions/job/down")
	if len(got) != 1 {
		t.Fatalf("the reaction did not survive its upstream's deletion (%d rows) — cascading it "+
			"away would be a silent capability loss", len(got))
	}
	if got[0]["missing"] != true {
		t.Errorf("missing = %v, want true — a dangling reaction never fires and must not look healthy",
			got[0]["missing"])
	}
}

// ── RX-12: validation ──────────────────────────────────────────────────────

func TestReactionValidationRefusals(t *testing.T) {
	api, pool := newRxAPI(t)
	seedRxJob(t, pool, "up")
	seedRxJob(t, pool, "down")

	for _, tc := range []struct {
		name, wantIn string
		reactions    []map[string]any
	}{
		{
			name: "self reference", wantIn: "cannot react to itself",
			reactions: []map[string]any{rx("r", "job", "down", "success")},
		},
		{
			name: "unknown upstream", wantIn: "no such job",
			reactions: []map[string]any{rx("r", "job", "ghost", "success")},
		},
		{
			name: "bad outcome", wantIn: "onOutcome must be one of",
			reactions: []map[string]any{rx("r", "job", "up", "killed")},
		},
		{
			name: "bad kind", wantIn: "onKind must be",
			reactions: []map[string]any{rx("r", "script", "up", "success")},
		},
		{
			name: "missing name", wantIn: "needs a name",
			reactions: []map[string]any{rx("", "job", "up", "success")},
		},
		{
			name: "duplicate names", wantIn: "duplicate reaction name",
			reactions: []map[string]any{rx("r", "job", "up", "success"), rx("r", "job", "up", "failure")},
		},
		{
			name: "negative delay", wantIn: "cannot be negative",
			reactions: []map[string]any{{"name": "r", "onKind": "job", "onName": "up",
				"onSource": "amadeus", "onOutcome": "success", "delaySeconds": -1}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/down",
				map[string]any{"reactions": tc.reactions})
			if code != http.StatusUnprocessableEntity {
				t.Fatalf("%s = %d, want 422 (%s)", tc.name, code, body)
			}
			if !strings.Contains(body, tc.wantIn) {
				t.Errorf("%s: body %q should contain %q — a refusal that does not say WHICH rule "+
					"broke sends the author to the database to find out", tc.name, body, tc.wantIn)
			}
		})
	}

	// Nothing was written by any refusal.
	if got := api.list("/api/v1/reactions/job/down"); len(got) != 0 {
		t.Errorf("a rejected PUT wrote %d rows; validation must run before the replace", len(got))
	}
}

// `killed` and `warning` are deliberately NOT valid on_outcome values: they are
// runs.status values that normalisation has already mapped to stopped and
// success. Accepting either would let an author write a reaction that can never
// fire, which is the failure this whole validation layer exists to prevent.
func TestRunStatusValuesAreNotOutcomes(t *testing.T) {
	api, pool := newRxAPI(t)
	seedRxJob(t, pool, "up")
	seedRxJob(t, pool, "down")
	for _, bad := range []string{"killed", "warning", "danger", "skipped"} {
		code, _ := api.doRaw(http.MethodPut, "/api/v1/reactions/job/down",
			map[string]any{"reactions": []map[string]any{rx("r", "job", "up", bad)}})
		if code != http.StatusUnprocessableEntity {
			t.Errorf("onOutcome %q = %d, want 422", bad, code)
		}
	}
}

// §2.8 — a cycle is rejected at authoring time with the path named. The depth
// ceiling is a runtime backstop for cycles that close through a workflow's step
// graph; it should not be what catches an authored one.
func TestCycleIsRejectedWithThePathNamed(t *testing.T) {
	api, pool := newRxAPI(t)
	seedRxJob(t, pool, "a")
	seedRxJob(t, pool, "b")
	seedRxJob(t, pool, "c")

	// a → b (when a finishes, run b), b → c.
	if code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/b",
		map[string]any{"reactions": []map[string]any{rx("r", "job", "a", "success")}}); code != http.StatusOK {
		t.Fatalf("b: %d (%s)", code, body)
	}
	if code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/c",
		map[string]any{"reactions": []map[string]any{rx("r", "job", "b", "success")}}); code != http.StatusOK {
		t.Fatalf("c: %d (%s)", code, body)
	}

	// Closing the loop: c → a.
	code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/a",
		map[string]any{"reactions": []map[string]any{rx("r", "job", "c", "success")}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("closing a 3-node cycle = %d, want 422 (%s)", code, body)
	}
	if !strings.Contains(body, "cycle") {
		t.Errorf("refusal %q should say it is a cycle", body)
	}
	// The path must be named — "there is a cycle somewhere" is not actionable in
	// a graph the operator cannot see on one screen.
	for _, node := range []string{"a", "b", "c"} {
		if !strings.Contains(body, "job:amadeus/"+node) {
			t.Errorf("refusal %q should name every node in the cycle path (missing %q)", body, node)
		}
	}
}

// A DIAMOND is not a cycle. Two reactions on the same upstream, converging on a
// third definition, is a legitimate fan-out/fan-in shape and must be allowed —
// a cycle checker that rejects it makes the feature unusable for the exact
// pattern people want.
func TestDiamondIsNotACycle(t *testing.T) {
	api, pool := newRxAPI(t)
	for _, n := range []string{"root", "left", "right", "join"} {
		seedRxJob(t, pool, n)
	}
	for _, owner := range []string{"left", "right"} {
		if code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/"+owner,
			map[string]any{"reactions": []map[string]any{rx("r", "job", "root", "success")}}); code != http.StatusOK {
			t.Fatalf("%s: %d (%s)", owner, code, body)
		}
	}
	if code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/join", map[string]any{
		"reactions": []map[string]any{
			rx("from-left", "job", "left", "success"),
			rx("from-right", "job", "right", "success"),
		},
	}); code != http.StatusOK {
		t.Fatalf("diamond join = %d, want 200 (%s)", code, body)
	}
}

// All four quadrants are authorable — the 2×2 is one primitive.
func TestAllFourQuadrantsAreAuthorable(t *testing.T) {
	api, pool := newRxAPI(t)
	seedRxJob(t, pool, "j-up")
	seedRxJob(t, pool, "j-down")
	seedRxWorkflow(t, pool, "w-up")
	seedRxWorkflow(t, pool, "w-down")

	for _, tc := range []struct{ ownerKind, ownerName, onKind, onName string }{
		{"job", "j-down", "job", "j-up"},
		{"job", "j-down", "workflow", "w-up"},
		{"workflow", "w-down", "job", "j-up"},
		{"workflow", "w-down", "workflow", "w-up"},
	} {
		code, body := api.doRaw(http.MethodPut,
			"/api/v1/reactions/"+tc.ownerKind+"/"+tc.ownerName,
			map[string]any{"reactions": []map[string]any{rx("r", tc.onKind, tc.onName, "any")}})
		if code != http.StatusOK {
			t.Errorf("%s→%s = %d, want 200 (%s)", tc.onKind, tc.ownerKind, code, body)
		}
	}
}

// ── RX-24: the interactive delete guard ────────────────────────────────────

func TestDeleteWatchedDefinitionRequiresForce(t *testing.T) {
	api, pool := newRxAPI(t)
	seedRxJob(t, pool, "upstream")
	seedRxJob(t, pool, "downstream")
	if code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/downstream",
		map[string]any{"reactions": []map[string]any{rx("r", "job", "upstream", "success")}}); code != http.StatusOK {
		t.Fatalf("seed = %d (%s)", code, body)
	}
	var upstreamID int64
	if err := pool.QueryRowContext(context.Background(),
		`SELECT rowid FROM jobs WHERE name='upstream'`).Scan(&upstreamID); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/jobs/" + itoaRx(upstreamID)

	code, body := api.doRaw(http.MethodDelete, path, nil)
	if code != http.StatusConflict {
		t.Fatalf("deleting a watched job = %d, want 409 (%s)", code, body)
	}
	// The refusal has to name the reactions — "something depends on this" leaves
	// the operator hunting.
	if !strings.Contains(body, "job:downstream/r") {
		t.Errorf("refusal %q should list the watching reaction", body)
	}
	if !strings.Contains(body, "force=true") {
		t.Errorf("refusal %q should name the override", body)
	}
	// The refusal must be DISTINGUISHABLE from the route's other 409, the
	// git-source one. They are cleared by different things — this by
	// ?force=true, that by nothing — so a client keying on the status alone has
	// to guess, and the console guessed wrong for a release: every refusal here
	// rendered "Only amadeus-source jobs can be deleted in-app", which cannot be
	// true, since the git check has already passed by the time this fires.
	if !strings.Contains(body, `"code":"`+apipkg.ErrCodeReactionsWatching+`"`) {
		t.Errorf("refusal %q should carry code %q, not the generic conflict", body, apipkg.ErrCodeReactionsWatching)
	}

	// Still there — the refusal was not cosmetic.
	var n int
	_ = pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM jobs WHERE name='upstream'`).Scan(&n)
	if n != 1 {
		t.Fatal("the refused delete removed the job anyway")
	}

	// Forced: succeeds, and deliberately leaves the reaction dangling.
	if code, body := api.doRaw(http.MethodDelete, path+"?force=true", nil); code != http.StatusNoContent {
		t.Fatalf("forced delete = %d, want 204 (%s)", code, body)
	}
	got := api.list("/api/v1/reactions/job/downstream")
	if len(got) != 1 || got[0]["missing"] != true {
		t.Errorf("after a forced delete the reaction should survive as missing, got %+v", got)
	}
}

// The delete route's OTHER 409 — git-source — must not answer to ?force=true and
// must not carry the reactions code. This is the negative half of the pair: the
// codes exist so a client can offer "delete anyway" for exactly one of them, and
// a git-source row that started accepting force, or started reporting itself as
// a reactions refusal, would put a button in front of a delete that can never
// succeed.
func TestGitSourceDeleteIsADifferentConflict(t *testing.T) {
	api, pool := newRxAPI(t)
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO jobs (name, source, run_type, enabled, synced_at) VALUES ('gitjob','git','bash',1,'t')`); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := pool.QueryRowContext(context.Background(),
		`SELECT rowid FROM jobs WHERE name='gitjob'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/jobs/" + itoaRx(id)

	for _, q := range []string{"", "?force=true"} {
		code, body := api.doRaw(http.MethodDelete, path+q, nil)
		if code != http.StatusConflict {
			t.Fatalf("deleting a git-source job%s = %d, want 409 (%s)", q, code, body)
		}
		if strings.Contains(body, apipkg.ErrCodeReactionsWatching) {
			t.Errorf("git-source refusal%s must not carry the reactions code: %s", q, body)
		}
		if !strings.Contains(body, `"code":"conflict"`) {
			t.Errorf("git-source refusal%s should stay the generic conflict: %s", q, body)
		}
	}
	var n int
	_ = pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM jobs WHERE name='gitjob'`).Scan(&n)
	if n != 1 {
		t.Fatal("force deleted a git-source job — it is not a refusal force can clear")
	}
}

// Deleting the OWNER cascades its own reactions away — the opposite rule, and
// the one that needs no force, because a reaction on a definition that no
// longer exists could only ever fail at promotion.
func TestDeletingTheOwnerCascadesItsReactions(t *testing.T) {
	api, pool := newRxAPI(t)
	seedRxJob(t, pool, "upstream")
	seedRxJob(t, pool, "downstream")
	if code, _ := api.doRaw(http.MethodPut, "/api/v1/reactions/job/downstream",
		map[string]any{"reactions": []map[string]any{rx("r", "job", "upstream", "success")}}); code != http.StatusOK {
		t.Fatal("seed failed")
	}
	var downID int64
	if err := pool.QueryRowContext(context.Background(),
		`SELECT rowid FROM jobs WHERE name='downstream'`).Scan(&downID); err != nil {
		t.Fatal(err)
	}

	if code, body := api.doRaw(http.MethodDelete, "/api/v1/jobs/"+itoaRx(downID), nil); code != http.StatusNoContent {
		t.Fatalf("deleting the OWNER = %d, want 204 — nothing watches it (%s)", code, body)
	}
	// RH: a soft delete is an UPDATE, so the AFTER DELETE cascade does NOT fire
	// and the owner's reactions survive — deliberately, because restoring from
	// the recycle bin must bring the definition back whole. They are inert
	// meanwhile: the reactor's gates filter deleted_at.
	var n int
	_ = pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM reactions WHERE owner_name='downstream'`).Scan(&n)
	if n != 1 {
		t.Errorf("owner's reactions = %d after a soft delete, want 1 — an undelete must restore them", n)
	}

	// The PURGE is where the real DELETE happens, and with it the cascade.
	if code, body := api.doRaw(http.MethodDelete, "/api/v1/recycle-bin/job/downstream", nil); code != http.StatusNoContent {
		t.Fatalf("purge = %d, want 204 (%s)", code, body)
	}
	_ = pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM reactions WHERE owner_name='downstream'`).Scan(&n)
	if n != 0 {
		t.Errorf("owner's reactions survived the purge (%d rows); the cascade trigger should have taken them", n)
	}
}

func itoaRx(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Authoring a reaction onto a GIT-source definition is refused, because the
// write would appear to succeed and then be silently destroyed.
//
// writeDefinitionReactions does a SOURCE-SCOPED replace on every sync so that a
// sync can never wipe an operator's in-app rows. A reaction stored with
// owner_source='git' sits in exactly the rows that replace clears — so the
// operator would watch their reaction land, work, and vanish at the next sync
// with nothing anywhere saying why. A git definition's configuration belongs in
// git, which is what the YAML surface is for.
func TestCannotAuthorReactionsOnAGitSourceDefinition(t *testing.T) {
	api, pool := newRxAPI(t)
	seedRxJob(t, pool, "up")
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO jobs (name, source, run_type, enabled, synced_at) VALUES ('gitjob','git','bash',1,'t')`); err != nil {
		t.Fatal(err)
	}

	code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/gitjob",
		map[string]any{"reactions": []map[string]any{rx("r", "job", "up", "success")}})
	if code != http.StatusConflict {
		t.Fatalf("PUT on a git-source job = %d, want 409 (%s)", code, body)
	}
	if !strings.Contains(body, "spec.reactions") {
		t.Errorf("refusal %q should point at the Git surface that DOES work", body)
	}
	var n int
	_ = pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM reactions WHERE owner_name='gitjob'`).Scan(&n)
	if n != 0 {
		t.Errorf("the refused PUT wrote %d rows anyway", n)
	}
}

// When the SAME name exists in both sources, the in-app one wins — it is the
// one this API can own without a sync destroying the result.
func TestAmbiguousNamePrefersTheAmadeusDefinition(t *testing.T) {
	api, pool := newRxAPI(t)
	seedRxJob(t, pool, "up")
	seedRxJob(t, pool, "shared") // amadeus
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO jobs (name, source, run_type, enabled, synced_at) VALUES ('shared','git','bash',1,'t')`); err != nil {
		t.Fatal(err)
	}

	if code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/shared",
		map[string]any{"reactions": []map[string]any{rx("r", "job", "up", "success")}}); code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200 — the amadeus definition is writable (%s)", code, body)
	}
	var src string
	if err := pool.QueryRowContext(context.Background(),
		`SELECT owner_source FROM reactions WHERE owner_name='shared'`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != "amadeus" {
		t.Errorf("owner_source = %q, want amadeus — a 'git' row here would be wiped by the next sync", src)
	}
}

// A cycle cannot be constructed in two steps by routing around the checker with
// the enabled flag. Disabling an edge, closing the loop, then re-enabling would
// otherwise produce a live cycle whose final step — an innocuous toggle — has no
// validation of its own.
func TestCycleCannotBeBuiltThroughTheDisabledFlag(t *testing.T) {
	api, pool := newRxAPI(t)
	seedRxJob(t, pool, "a")
	seedRxJob(t, pool, "b")

	// a → b, but disabled.
	disabled := map[string]any{"name": "r", "onKind": "job", "onName": "a",
		"onSource": "amadeus", "onOutcome": "success", "enabled": false}
	if code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/b",
		map[string]any{"reactions": []map[string]any{disabled}}); code != http.StatusOK {
		t.Fatalf("seed disabled edge = %d (%s)", code, body)
	}

	// Closing the loop must still be refused, even though the other edge is off.
	code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/a",
		map[string]any{"reactions": []map[string]any{rx("r", "job", "b", "success")}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("closing a loop against a DISABLED edge = %d, want 422 — otherwise re-enabling "+
			"that edge silently creates a live cycle (%s)", code, body)
	}
}

// The per-definition read must be SOURCE-SCOPED, or a name present in both
// namespaces merges two definitions' reactions into one list — and since the
// write path resolves to a single source, a read-modify-write would copy the
// git definition's entries in as in-app rows. The next sync restores the git
// ones, leaving both, and one upstream completion produces two runs.
func TestPerDefinitionReadIsSourceScoped(t *testing.T) {
	api, pool := newRxAPI(t)
	seedRxJob(t, pool, "up")
	seedRxJob(t, pool, "shared") // amadeus
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO jobs (name, source, run_type, enabled, synced_at) VALUES ('shared','git','bash',1,'t')`); err != nil {
		t.Fatal(err)
	}
	// A git-authored reaction on the git twin, as a sync would have written it.
	if _, err := pool.ExecContext(context.Background(), `
		INSERT INTO reactions (owner_source, owner_kind, owner_name, name,
		                       on_source, on_kind, on_name, on_outcome)
		VALUES ('git','job','shared','from-git','amadeus','job','up','failure')`); err != nil {
		t.Fatal(err)
	}
	// And an in-app one on the amadeus twin.
	if code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/shared",
		map[string]any{"reactions": []map[string]any{rx("from-app", "job", "up", "success")}}); code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", code, body)
	}

	got := api.list("/api/v1/reactions/job/shared")
	if len(got) != 1 || got[0]["name"] != "from-app" {
		t.Fatalf("per-definition read returned %+v; it must show only the resolved source's "+
			"reactions, or a read-modify-write duplicates the git ones as in-app rows", got)
	}
	// The global edge list still shows both — that view is about the whole graph.
	if all := api.list("/api/v1/reactions"); len(all) != 2 {
		t.Errorf("global edge list = %d entries, want 2 (both sources)", len(all))
	}
}

// RX-21 — a min_interval longer than the retention window would silently
// disarm itself once its anchor delivery is pruned, so it is refused with the
// limit named.
func TestMinIntervalBeyondRetentionIsRefused(t *testing.T) {
	api, pool := newRxAPI(t)
	seedRxJob(t, pool, "up")
	seedRxJob(t, pool, "down")

	tooLong := map[string]any{"name": "r", "onKind": "job", "onName": "up",
		"onSource": "amadeus", "onOutcome": "success",
		"minIntervalSeconds": 400 * 24 * 60 * 60} // > the 90-day default
	code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/down",
		map[string]any{"reactions": []map[string]any{tooLong}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("a 400-day interval = %d, want 422 (%s)", code, body)
	}
	if !strings.Contains(body, "retention") {
		t.Errorf("refusal %q should name the limit that makes it impossible", body)
	}
}

// The API and YAML surfaces must agree on what a legal name is, or "move this
// reaction into Git" becomes a debugging session.
func TestReactionNameSlugMatchesTheYamlRule(t *testing.T) {
	api, pool := newRxAPI(t)
	seedRxJob(t, pool, "up")
	seedRxJob(t, pool, "down")
	for _, bad := range []string{"My Reaction!", "UPPER", "has space", "-leading"} {
		code, _ := api.doRaw(http.MethodPut, "/api/v1/reactions/job/down",
			map[string]any{"reactions": []map[string]any{rx(bad, "job", "up", "success")}})
		if code != http.StatusUnprocessableEntity {
			t.Errorf("name %q = %d, want 422 — the YAML path rejects it", bad, code)
		}
	}
	if code, body := api.doRaw(http.MethodPut, "/api/v1/reactions/job/down",
		map[string]any{"reactions": []map[string]any{rx("after_extract-2", "job", "up", "success")}}); code != http.StatusOK {
		t.Errorf("a legal slug = %d, want 200 (%s)", code, body)
	}
}

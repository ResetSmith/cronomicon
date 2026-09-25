package workflow

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// R2F-2 — a workflow step may reference a job by IDENTITY.
//
// Since R2-5 a name may belong to two departments' jobs, and resolveJobDef
// refuses an ambiguous name fail-closed. Correct, but it left a legal catalog
// state unbuildable: no workflow could name either twin. A step that pins the
// uid resolves in one lookup, with no A11 source-precedence walk to go wrong.

func identityPool(t *testing.T) *Engine {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "wf-step-identity.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return New(pool, discardLog())
}

func seedJob(t *testing.T, e *Engine, uid, name, source, runType, scope string) {
	t.Helper()
	if _, err := e.db.Exec(
		`INSERT INTO jobs (uid, name, source, run_type, command, concurrency_policy, enabled, scope, synced_at)
		 VALUES (?, ?, ?, ?, 'echo hi', 'Allow', 1, ?, 't')`, uid, name, source, runType, scope); err != nil {
		t.Fatalf("seed %s/%s: %v", source, name, err)
	}
}

// The point of the band: two twins, and a step can name either one.
func TestPinnedStepResolvesTheTwinItNames(t *testing.T) {
	e := identityPool(t)
	ctx := context.Background()
	seedJob(t, e, "uid-a", "deploy", "cronomicon", "bash", "fin-prod")
	seedJob(t, e, "uid-b", "deploy", "cronomicon", "ansible", "dss-prod")

	for _, tc := range []struct{ uid, wantType, wantScope string }{
		{"uid-a", "bash", "fin-prod"},
		{"uid-b", "ansible", "dss-prod"},
	} {
		src, jd, ok := e.resolveJobDef(ctx, StepRef{Name: "deploy", UID: tc.uid}, "cronomicon")
		if !ok {
			t.Fatalf("%s did not resolve", tc.uid)
		}
		if jd.unavailable != "" {
			t.Errorf("%s refused: %s — a pinned step is never ambiguous", tc.uid, jd.unavailable)
		}
		if jd.runType != tc.wantType || jd.scope != tc.wantScope || jd.uid != tc.uid {
			t.Errorf("%s = (%s, %s, %s), want (%s, %s, %s)", tc.uid, jd.runType, jd.scope, jd.uid, tc.wantType, tc.wantScope, tc.uid)
		}
		if src != "cronomicon" {
			t.Errorf("%s source = %q, want the row's own source", tc.uid, src)
		}
	}

	// The name-only step over the same catalog still refuses — unchanged, and the
	// reason the pinned form exists.
	_, jd, ok := e.resolveJobDef(ctx, StepRef{Name: "deploy"}, "cronomicon")
	if !ok || jd.unavailable == "" {
		t.Errorf("ambiguous name-only step = (%v, %q), want a refusal", ok, jd.unavailable)
	}
}

// An identity needs no fallback: it is answered by one indexed lookup, and the
// step's own jobSource does not steer it. A pinned step whose source override
// disagrees with where the job actually lives still resolves to the job.
func TestPinnedStepIgnoresSourcePrecedence(t *testing.T) {
	e := identityPool(t)
	ctx := context.Background()
	seedJob(t, e, "uid-git", "report", "git", "bash", "")
	seedJob(t, e, "uid-ama", "report", "cronomicon", "ansible", "")

	src, jd, ok := e.resolveJobDef(ctx, StepRef{Name: "report", Source: "cronomicon", UID: "uid-git"}, "cronomicon")
	if !ok {
		t.Fatal("pinned step did not resolve")
	}
	if jd.uid != "uid-git" || jd.runType != "bash" || src != "git" {
		t.Errorf("resolved (%s, %s, %s), want the git row the uid names — the override must not steer an identity", src, jd.uid, jd.runType)
	}
}

// A dangling uid REFUSES rather than falling back to a same-named job: the RX
// purge-recreate semantics, where a new identity is a new job on purpose.
// Falling back would silently substitute a different department's job for one
// the operator deliberately pinned.
func TestDanglingPinnedStepRefuses(t *testing.T) {
	e := identityPool(t)
	ctx := context.Background()
	seedJob(t, e, "uid-live", "deploy", "cronomicon", "bash", "")

	src, jd, ok := e.resolveJobDef(ctx, StepRef{Name: "deploy", UID: "uid-gone"}, "cronomicon")
	if !ok {
		t.Fatal("a dangling pin must resolve to a REFUSAL def, not to nothing — the step needs a terminal child run")
	}
	if jd.unavailable == "" {
		t.Error("dangling pin did not refuse — it must never fall back to the same-named job")
	}
	if jd.uid == "uid-live" {
		t.Error("dangling pin resolved the live same-named job: the substitution this band forbids")
	}
	if src == "" {
		t.Error("a refusing def still needs a legal source for its terminal child run")
	}
}

// Name-only resolution is byte-identical to pre-R2F-2 — the existing engine
// suite is the real regression test; this pins the precedence walk explicitly.
func TestNameOnlyStepKeepsA11Precedence(t *testing.T) {
	e := identityPool(t)
	ctx := context.Background()
	seedJob(t, e, "uid-git", "report", "git", "bash", "")
	seedJob(t, e, "uid-ama", "report", "cronomicon", "ansible", "")

	for _, tc := range []struct{ override, wfSource, wantUID string }{
		{"", "git", "uid-git"},           // the workflow's own source wins
		{"", "cronomicon", "uid-ama"},    // ditto, the other way
		{"git", "cronomicon", "uid-git"}, // an explicit override is absolute
		{"cronomicon", "git", "uid-ama"}, //
	} {
		_, jd, ok := e.resolveJobDef(ctx, StepRef{Name: "report", Source: tc.override}, tc.wfSource)
		if !ok || jd.uid != tc.wantUID {
			t.Errorf("(override=%q, wf=%q) resolved %q, want %q", tc.override, tc.wfSource, jd.uid, tc.wantUID)
		}
	}
}

// The defs map is keyed by REFERENCE, not by name: two steps pinning different
// twins of one name are two jobs. A name-keyed map collapsed them, resolving
// both to whichever the walk reached last.
func TestLookupJobDefsKeepsTwinsApart(t *testing.T) {
	e := identityPool(t)
	ctx := context.Background()
	seedJob(t, e, "uid-a", "deploy", "cronomicon", "bash", "fin-prod")
	seedJob(t, e, "uid-b", "deploy", "cronomicon", "ansible", "dss-prod")

	steps := []Step{
		{Type: "job", Name: "deploy", JobUID: "uid-a"},
		{Type: "job", Name: "deploy", JobUID: "uid-b"},
	}
	defs, err := e.lookupJobDefs(ctx, steps, "cronomicon")
	if err != nil {
		t.Fatalf("lookupJobDefs: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("defs = %d entries, want 2 — the twins collapsed", len(defs))
	}
	if got := defs[StepKey(steps[0])]; got.uid != "uid-a" {
		t.Errorf("step A resolved %q, want uid-a", got.uid)
	}
	if got := defs[StepKey(steps[1])]; got.uid != "uid-b" {
		t.Errorf("step B resolved %q, want uid-b", got.uid)
	}

	// JobScopes must therefore see BOTH departments' scopes: the authorization
	// guard reads this list, and a missing scope is a check that never runs.
	scopes, err := e.JobScopes(ctx, steps, "cronomicon")
	if err != nil {
		t.Fatalf("JobScopes: %v", err)
	}
	seen := map[string]bool{}
	for _, s := range scopes {
		seen[s] = true
	}
	if !seen["fin-prod"] || !seen["dss-prod"] {
		t.Errorf("JobScopes = %v, want both twins' scopes", scopes)
	}
}

// collectStepRefs is what the compose-time checks iterate, so its ORDER must be
// stable (first appearance) and its de-duplication must be by reference.
func TestCollectStepRefsOrderAndDedupe(t *testing.T) {
	steps := []Step{
		{Type: "job", Name: "b", JobUID: "uid-b"},
		{Type: "job", Name: "a"},
		{Type: "parallel", Jobs: []Step{
			{Type: "job", Name: "a", Source: "git"}, // same key as the bare "a": last value wins
			{Type: "job", Name: "b"},                // name-only "b" is a DIFFERENT reference from uid-b
		}},
	}
	refs := collectStepRefs(steps)
	var keys []string
	for _, r := range refs {
		keys = append(keys, r.Key())
	}
	want := []string{"uid:uid-b", "name:a", "name:b"}
	if len(keys) != len(want) {
		t.Fatalf("refs = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("refs = %v, want %v (first-appearance order)", keys, want)
		}
	}
	// The later jobSource wins the value, as the old map's last-write-wins did.
	for _, r := range refs {
		if r.Key() == "name:a" && r.Source != "git" {
			t.Errorf("ref a source = %q, want git (last write wins)", r.Source)
		}
	}
}

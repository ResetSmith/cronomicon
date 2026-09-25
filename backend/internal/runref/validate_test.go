package runref

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// unrestricted / scoped / zero-grant readers, mirroring auth.ScopeGrant.CanRead
// without importing auth (runref must stay a leaf — see CanReadScope).
func allScopes() CanReadScope { return func(string) bool { return true } }
func onlyScopes(allowed ...string) CanReadScope {
	return func(scope string) bool {
		if scope == "" {
			return true // a GLOBAL row is readable by everyone (A5)
		}
		return slices.Contains(allowed, scope)
	}
}

// TestValidateTruthTable is the specification for T1.1/AG-Q4: the authoring-time
// verdict over {in-scope, global, out-of-scope, missing} × {secret, variable, key},
// including the restricted-actor DEGRADATION that keeps the validator from being
// the cross-scope oracle M2 forbids on the run surface.
func TestValidateTruthTable(t *testing.T) {
	pool := openDB(t)
	cfg := testCfg(t)
	ctx := context.Background()
	sec := secrets.New(pool, cfg, nilLog())

	// DB_PASS: global + a prod-specific override. PROD_ONLY: prod only.
	if _, err := sec.Create(ctx, secrets.CreateInput{Key: "DB_PASS", Source: "stored", Value: "g"}, "alice"); err != nil {
		t.Fatalf("seed DB_PASS global: %v", err)
	}
	if _, err := sec.Create(ctx, secrets.CreateInput{Key: "DB_PASS", Source: "stored", Scope: new("prod"), Value: "p"}, "alice"); err != nil {
		t.Fatalf("seed DB_PASS prod: %v", err)
	}
	if _, err := sec.Create(ctx, secrets.CreateInput{Key: "PROD_ONLY", Source: "stored", Scope: new("prod"), Value: "p"}, "alice"); err != nil {
		t.Fatalf("seed PROD_ONLY: %v", err)
	}
	if _, err := settings.CreateEnvVar(ctx, pool, settings.EnvVarInput{Key: "REGION", Value: "us-east", Scope: new("prod")}, "alice"); err != nil {
		t.Fatalf("seed REGION: %v", err)
	}
	seedKey(t, pool, cfg, "deploy_key", "MATERIAL")

	tests := []struct {
		name           string
		kind           Kind
		ref            string
		scope          string
		canRead        CanReadScope
		wantOK         bool
		wantOutcome    Outcome
		wantResolved   *string  // nil ⇒ assert ResolvedScope is nil
		wantOthers     []string // nil ⇒ assert empty
		reasonContains string
	}{
		{
			name: "scope-exact row wins over the global fallback",
			kind: KindSecret, ref: "DB_PASS", scope: "prod", canRead: allScopes(),
			wantOK: true, wantOutcome: OutcomeResolved, wantResolved: new("prod"),
			reasonContains: `scope "prod"`,
		},
		{
			name: "global row resolves for a scope with no override",
			kind: KindSecret, ref: "DB_PASS", scope: "dev", canRead: allScopes(),
			wantOK: true, wantOutcome: OutcomeResolved, wantResolved: new(""),
			reasonContains: "global",
		},
		{
			name: "global row resolves for a global run",
			kind: KindSecret, ref: "DB_PASS", scope: "", canRead: allScopes(),
			wantOK: true, wantOutcome: OutcomeResolved, wantResolved: new(""),
		},
		{
			name: "out of scope names the scope it DOES live in",
			kind: KindSecret, ref: "PROD_ONLY", scope: "dev", canRead: allScopes(),
			wantOutcome: OutcomeOutOfScope, wantOthers: []string{"prod"},
			reasonContains: `only in scope "prod"`,
		},
		{
			name: "variables classify identically to secrets",
			kind: KindVar, ref: "REGION", scope: "dev", canRead: allScopes(),
			wantOutcome: OutcomeOutOfScope, wantOthers: []string{"prod"},
		},
		{
			name: "missing everywhere",
			kind: KindSecret, ref: "NOPE", scope: "dev", canRead: allScopes(),
			wantOutcome: OutcomeNotFound, reasonContains: "no secret",
		},
		{
			// AG-Q4's binding requirement. `alice` may read only dev, so the prod row
			// is invisible to her LIST and must be invisible here — the verdict
			// degrades from out_of_scope to the generic not_found.
			name: "restricted actor degrades out-of-scope to not_found",
			kind: KindSecret, ref: "PROD_ONLY", scope: "dev", canRead: onlyScopes("dev"),
			wantOutcome: OutcomeNotFound, reasonContains: "no secret",
		},
		{
			// The other half of the same rule: a restricted actor asking about a scope
			// they CAN read still gets the precise answer for rows they can see.
			name: "restricted actor keeps precision for readable scopes",
			kind: KindSecret, ref: "PROD_ONLY", scope: "dev", canRead: onlyScopes("dev", "prod"),
			wantOutcome: OutcomeOutOfScope, wantOthers: []string{"prod"},
		},
		{
			// A nil predicate is the fail-closed zero grant: GLOBAL only.
			name: "nil canRead reads as global-only",
			kind: KindSecret, ref: "PROD_ONLY", scope: "dev", canRead: nil,
			wantOutcome: OutcomeNotFound,
		},
		{
			name: "key resolves on existence — no scope axis",
			kind: KindKey, ref: "deploy_key", scope: "prod", canRead: allScopes(),
			wantOK: true, wantOutcome: OutcomeResolved,
			reasonContains: "not scope-filtered",
		},
		{
			name: "missing key",
			kind: KindKey, ref: "absent_key", scope: "prod", canRead: allScopes(),
			wantOutcome: OutcomeNotFound, reasonContains: "no SSH key named",
		},
		{
			name: "unknown kind is invalid, not missing",
			kind: Kind("bogus"), ref: "X", scope: "", canRead: allScopes(),
			wantOutcome: OutcomeInvalid,
		},
		{
			name: "empty name is invalid",
			kind: KindSecret, ref: "", scope: "", canRead: allScopes(),
			wantOutcome: OutcomeInvalid,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Validate(ctx, pool, tc.kind, tc.ref, tc.scope, tc.canRead)
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if v.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v (reason %q)", v.OK, tc.wantOK, v.Reason)
			}
			if v.Outcome != tc.wantOutcome {
				t.Errorf("Outcome = %q, want %q (reason %q)", v.Outcome, tc.wantOutcome, v.Reason)
			}
			switch {
			case tc.wantResolved == nil && v.ResolvedScope != nil:
				t.Errorf("ResolvedScope = %q, want nil", *v.ResolvedScope)
			case tc.wantResolved != nil && v.ResolvedScope == nil:
				t.Errorf("ResolvedScope = nil, want %q", *tc.wantResolved)
			case tc.wantResolved != nil && *v.ResolvedScope != *tc.wantResolved:
				t.Errorf("ResolvedScope = %q, want %q", *v.ResolvedScope, *tc.wantResolved)
			}
			if len(v.OtherScopes) != len(tc.wantOthers) {
				t.Errorf("OtherScopes = %v, want %v", v.OtherScopes, tc.wantOthers)
			} else {
				for i := range tc.wantOthers {
					if v.OtherScopes[i] != tc.wantOthers[i] {
						t.Errorf("OtherScopes = %v, want %v", v.OtherScopes, tc.wantOthers)
						break
					}
				}
			}
			if tc.reasonContains != "" && !strings.Contains(v.Reason, tc.reasonContains) {
				t.Errorf("Reason %q does not contain %q", v.Reason, tc.reasonContains)
			}
			// The invariant that makes any of this trustworthy: a verdict never
			// carries a value, only names.
			if strings.Contains(v.Reason, "us-east") || strings.Contains(v.Reason, "MATERIAL") {
				t.Errorf("verdict leaked a value: %q", v.Reason)
			}
		})
	}
}

// TestValidateAgreesWithDispatch is the anti-drift guard: whatever Validate says
// resolves, the dispatch resolver must actually inject — and vice versa. The two
// share lookupScoped precisely so this can never diverge silently.
func TestValidateAgreesWithDispatch(t *testing.T) {
	pool := openDB(t)
	cfg := testCfg(t)
	ctx := context.Background()
	sec := secrets.New(pool, cfg, nilLog())
	if _, err := sec.Create(ctx, secrets.CreateInput{Key: "GLOBAL_PW", Source: "stored", Value: "g"}, "alice"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := sec.Create(ctx, secrets.CreateInput{Key: "PROD_PW", Source: "stored", Scope: new("prod"), Value: "p"}, "alice"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := NewResolver(pool, cfg, sec, nilLog())

	for _, scope := range []string{"", "dev", "prod"} {
		for _, name := range []string{"GLOBAL_PW", "PROD_PW", "ABSENT"} {
			b := Binding{Kind: KindSecret, Name: name}
			v, err := Validate(ctx, pool, b.Kind, b.Name, scope, allScopes())
			if err != nil {
				t.Fatalf("Validate(%s,%s): %v", scope, name, err)
			}
			_, rerr := r.Resolve(ctx, nil, scope, nil, []Binding{b})
			dispatched := rerr == nil
			if v.OK != dispatched {
				t.Errorf("scope=%q name=%q: validator says ok=%v but dispatch %v (%v)",
					scope, name, v.OK, map[bool]string{true: "succeeded", false: "failed"}[dispatched], rerr)
			}
		}
	}
}

// TestValidateAllPreservesOrder — the chips render in the order they were sent.
func TestValidateAllPreservesOrder(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	in := []Binding{
		{Kind: KindVar, Name: "B"},
		{Kind: KindSecret, Name: "A"},
		{Kind: KindKey, Name: "C"},
	}
	out, err := ValidateAll(ctx, pool, in, "", allScopes())
	if err != nil {
		t.Fatalf("ValidateAll: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("want 3 verdicts, got %d", len(out))
	}
	for i := range in {
		if out[i].Kind != in[i].Kind || out[i].Name != in[i].Name {
			t.Fatalf("verdict %d is %s/%s, want %s/%s", i, out[i].Kind, out[i].Name, in[i].Kind, in[i].Name)
		}
		if out[i].Reference != in[i].Kind.Reference(in[i].Name) {
			t.Errorf("verdict %d reference = %q", i, out[i].Reference)
		}
	}
}

// TestAgencyPredicateAG_Q1b is the truth table for the Phase-3 resolution
// predicate. This table IS the specification: both clauses, agency AND scope, with
// an EMPTY membership set meaning "no agency restriction" rather than "reachable
// from nowhere" — the convention that keeps every global row global and made the
// migration-670 backfill behavior-preserving.
func TestAgencyPredicateAG_Q1b(t *testing.T) {
	pool := openDB(t)
	cfg := testCfg(t)
	ctx := context.Background()
	sec := secrets.New(pool, cfg, nilLog())
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	const now = "2026-01-01T00:00:00Z"
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-a','A',?)`, now)
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-b','B',?)`, now)

	mk := func(key, scope, agency string) {
		t.Helper()
		var sp *string
		if scope != "" {
			sp = &scope
		}
		s, err := sec.Create(ctx, secrets.CreateInput{Key: key, Source: "stored", Scope: sp, Value: "v"}, "seed")
		if err != nil {
			t.Fatalf("seed secret %s: %v", key, err)
		}
		if agency != "" {
			exec(`INSERT INTO secret_agencies(secret_id, agency_id) VALUES(?,?)`, s.ID, agency)
		}
	}
	mk("FREE", "", "")     // global, unrestricted
	mk("IN_A", "", "ag-a") // global, but only agency A
	mk("PROD_IN_A", "prod", "ag-a")

	r := NewResolver(pool, cfg, sec, nilLog())
	resolves := func(name, scope string, agencies []string) bool {
		t.Helper()
		_, err := r.Resolve(ctx, nil, scope, agencies, []Binding{{Kind: KindSecret, Name: name}})
		return err == nil
	}

	tests := []struct {
		name     string
		ref      string
		scope    string
		agencies []string
		want     bool
		why      string
	}{
		{"unrestricted row, general-pool run", "FREE", "prod", nil, true,
			"an empty membership set means NO restriction — this is what keeps global secrets global"},
		{"unrestricted row, agency run", "FREE", "prod", []string{"A"}, true, ""},
		{"agency row, matching run", "IN_A", "prod", []string{"A"}, true, ""},
		{"agency row, non-matching run", "IN_A", "prod", []string{"B"}, false,
			"the AG-Q1(b) agency clause"},
		{"agency row, general-pool run", "IN_A", "prod", nil, false,
			"an empty RUN set intersects nothing — the asymmetry with an empty ROW set is deliberate"},
		{"agency row, run in several agencies incl. a match", "IN_A", "prod", []string{"B", "A"}, true,
			"intersection, not equality"},
		{"scope clause still applies inside the right agency", "PROD_IN_A", "dev", []string{"A"}, false,
			"AG-Q1(b) is AND — nobody loses the scope control they had"},
		{"both clauses satisfied", "PROD_IN_A", "prod", []string{"A"}, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolves(tc.ref, tc.scope, tc.agencies); got != tc.want {
				t.Errorf("resolves(%s, scope=%q, agencies=%v) = %v, want %v — %s",
					tc.ref, tc.scope, tc.agencies, got, tc.want, tc.why)
			}
		})
	}
}

// TestKeyTighteningAG_Q5 is the change §7.1 flags as breaking bindings that work
// today: SSH keys gain an agency clause. The graded part is what keeps it from
// being an upgrade cliff — a key with NO membership stays reachable everywhere,
// which is the state migration 670 deliberately leaves every existing key in.
func TestKeyTighteningAG_Q5(t *testing.T) {
	pool := openDB(t)
	cfg := testCfg(t)
	ctx := context.Background()
	sec := secrets.New(pool, cfg, nilLog())
	seedKey(t, pool, cfg, "free_key", "MATERIAL")
	seedKey(t, pool, cfg, "a_key", "MATERIAL")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-a','A','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO ssh_credential_agencies(credential_id, agency_id)
	      SELECT id, 'ag-a' FROM ssh_credentials WHERE label='a_key'`)

	r := NewResolver(pool, cfg, sec, nilLog())
	resolves := func(label string, agencies []string) error {
		_, err := r.Resolve(ctx, nil, "prod", agencies, []Binding{{Kind: KindKey, Name: label}})
		return err
	}

	// The upgrade-safety property: an unassigned key is unrestricted, so nothing
	// breaks on deploy.
	if err := resolves("free_key", nil); err != nil {
		t.Errorf("a key with no membership must stay reachable: %v", err)
	}
	if err := resolves("free_key", []string{"A"}); err != nil {
		t.Errorf("a key with no membership must stay reachable from any agency: %v", err)
	}
	// The tightening itself.
	if err := resolves("a_key", []string{"A"}); err != nil {
		t.Errorf("a key in the run's agency must resolve: %v", err)
	}
	err := resolves("a_key", []string{"B"})
	if err == nil {
		t.Fatal("AG-Q5 not enforced: a job outside the key's agency still received it")
	}
	if !errors.Is(err, ErrOutOfScope) {
		t.Errorf("out-of-agency key error = %v, want ErrOutOfScope", err)
	}
	// M2 — the operator-facing message must stay generic, so a run cannot be used to
	// probe which key labels exist or which agencies they belong to.
	if msg := OperatorMessage(err); msg != "reference CRONOMICON_KEY_a_key is unavailable for this run" {
		t.Errorf("operator message = %q; the run surface must not name the cause", msg)
	}
	// A general-pool run intersects nothing, so an agency-bound key is out of reach.
	if err := resolves("a_key", nil); err == nil {
		t.Error("a general-pool run received an agency-bound key")
	}
}

package runref

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// Phase E (RA-15..RA-19, the runas-update plan) — per-agency same-name rows.
//
// The model: two departments may each hold a row under the SAME key in the SAME
// scope, and a run resolves its OWN department's row. What makes that safe rather
// than terrifying is the refusal at the end — when a run's agency snapshot spans
// BOTH owners there is no defensible pick, so nothing is injected at all.
//
// ⚠️ Every owned row in these tests is given membership as well as ownership,
// because that is the invariant the write path maintains (the owner is enrolled in
// its own row's visibility set, and SetAgencyMembership refuses to break the pair).
// A test that set ownership alone would be exercising a state the system does not
// produce — and would pass while the feature was broken, since the visibility
// clause would filter the row out before the owner tier ever ran.

type ownerFixture struct {
	pool *sql.DB
	cfg  *config.Config
	sec  *secrets.Service
	r    *Resolver
	exec func(string, ...any)
}

func newOwnerFixture(t *testing.T) *ownerFixture {
	t.Helper()
	pool := openDB(t)
	cfg := testCfg(t)
	sec := secrets.New(pool, cfg, nilLog())
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	const now = "2026-01-01T00:00:00Z"
	exec(`INSERT INTO agencies(id,name,created_at) VALUES('ag-a','TeamA',?)`, now)
	exec(`INSERT INTO agencies(id,name,created_at) VALUES('ag-b','TeamB',?)`, now)
	return &ownerFixture{pool: pool, cfg: cfg, sec: sec,
		r: NewResolver(pool, cfg, sec, nilLog()), exec: exec}
}

// ownedSecret creates a secret owned by (and visible to) one agency. owner "" makes
// it shared, with no membership — the pre-Phase-E shape.
func (f *ownerFixture) ownedSecret(t *testing.T, id, key, scope, owner, value string) {
	t.Helper()
	var sp *string
	if scope != "" {
		sp = &scope
	}
	sc, err := f.sec.Create(context.Background(), secrets.CreateInput{
		Key: key, Source: "stored", Scope: sp, Value: value, OwnerAgency: owner,
	}, "seed")
	if err != nil {
		t.Fatalf("create secret %s (owner %q): %v", key, owner, err)
	}
	if owner != "" {
		f.exec(`INSERT INTO secret_agencies(secret_id,agency_id) VALUES(?,?)`, sc.ID, owner)
	}
}

func (f *ownerFixture) resolveSecret(t *testing.T, name, scope string, agencies []string) (string, error) {
	t.Helper()
	out, err := f.r.Resolve(context.Background(), nil, scope, agencies,
		[]Binding{{Kind: KindSecret, Name: name}})
	if err != nil {
		return "", err
	}
	return out.Env["AMADEUS_SECRET_"+name], nil
}

// TestOwnedRowBeatsSharedPerDepartment is the headline: one key, one scope, two
// departments, each run fed its own row — with no alias and no naming convention.
func TestOwnedRowBeatsSharedPerDepartment(t *testing.T) {
	f := newOwnerFixture(t)
	f.ownedSecret(t, "s-a", "BECOME_PASSWORD", "prod", "ag-a", "teama-pw")
	f.ownedSecret(t, "s-b", "BECOME_PASSWORD", "prod", "ag-b", "teamb-pw")

	got, err := f.resolveSecret(t, "BECOME_PASSWORD", "prod", []string{"TeamA"})
	if err != nil {
		t.Fatalf("TeamA resolve: %v", err)
	}
	if got != "teama-pw" {
		t.Errorf("TeamA got %q, want teama-pw", got)
	}
	got, err = f.resolveSecret(t, "BECOME_PASSWORD", "prod", []string{"TeamB"})
	if err != nil {
		t.Fatalf("TeamB resolve: %v", err)
	}
	if got != "teamb-pw" {
		t.Errorf("TeamB got %q, want teamb-pw", got)
	}
}

// TestOwnedBeatsSharedWithinAScope — the tier order. A department that owns a row
// gets ITS row even when a shared row of the same key exists in the same scope.
func TestOwnedBeatsSharedWithinAScope(t *testing.T) {
	f := newOwnerFixture(t)
	f.ownedSecret(t, "s-shared", "TOKEN", "prod", "", "shared-tok")
	f.ownedSecret(t, "s-a", "TOKEN", "prod", "ag-a", "teama-tok")

	got, err := f.resolveSecret(t, "TOKEN", "prod", []string{"TeamA"})
	if err != nil {
		t.Fatalf("TeamA resolve: %v", err)
	}
	if got != "teama-tok" {
		t.Errorf("owned row did not win: got %q, want teama-tok", got)
	}
	// A department with no row of its own still falls through to the shared row —
	// today's behaviour, and the reason this is not a tightening.
	got, err = f.resolveSecret(t, "TOKEN", "prod", []string{"TeamB"})
	if err != nil {
		t.Fatalf("TeamB resolve: %v", err)
	}
	if got != "shared-tok" {
		t.Errorf("fallback to the shared row broke: got %q, want shared-tok", got)
	}
}

// TestTwoOwnersFailClosed is the refusal that makes the rest defensible. A run whose
// agency snapshot spans BOTH owners has no defensible winner: whichever row an
// ORDER BY happened to put first, the run would escalate with somebody's credential
// and the audit would record an intent that never existed.
func TestTwoOwnersFailClosed(t *testing.T) {
	f := newOwnerFixture(t)
	f.ownedSecret(t, "s-a", "BECOME_PASSWORD", "prod", "ag-a", "teama-pw")
	f.ownedSecret(t, "s-b", "BECOME_PASSWORD", "prod", "ag-b", "teamb-pw")

	val, err := f.resolveSecret(t, "BECOME_PASSWORD", "prod", []string{"TeamA", "TeamB"})
	if err == nil {
		t.Fatalf("a two-owner snapshot resolved to %q — one department's credential was picked silently", val)
	}
	if !errors.Is(err, ErrAmbiguousReference) {
		t.Errorf("error = %v, want ErrAmbiguousReference", err)
	}
	// The operator surface must say AMBIGUOUS, not "unavailable": sending someone to
	// hunt for a missing row when the problem is two rows is unrecoverable.
	msg := OperatorMessage(err)
	if !strings.Contains(msg, "ambiguous") {
		t.Errorf("operator message does not name the ambiguity: %q", msg)
	}
	if !strings.Contains(msg, "AMADEUS_SECRET_BECOME_PASSWORD") {
		t.Errorf("operator message does not name the reference: %q", msg)
	}
	// ...and it must NOT name the owning departments. That precision belongs to the
	// authoring-time validator, where it is bounded by the caller's own visibility.
	for _, leak := range []string{"TeamA", "TeamB"} {
		if strings.Contains(msg, leak) {
			t.Errorf("dispatch message leaks the owning department %q: %s", leak, msg)
		}
	}
}

// TestSharedCatalogueIsUnchanged is the regression fence for every installation that
// never uses ownership: an all-unowned catalogue must resolve exactly as it did
// before Phase E, including scope-exact-beats-global.
func TestSharedCatalogueIsUnchanged(t *testing.T) {
	f := newOwnerFixture(t)
	f.ownedSecret(t, "s-g", "TOKEN", "", "", "global-tok")
	f.ownedSecret(t, "s-p", "TOKEN", "prod", "", "prod-tok")

	for _, tc := range []struct{ scope, agencies, want string }{
		{"prod", "", "prod-tok"},
		{"dev", "", "global-tok"},
		{"", "", "global-tok"},
	} {
		var ag []string
		if tc.agencies != "" {
			ag = []string{tc.agencies}
		}
		got, err := f.resolveSecret(t, "TOKEN", tc.scope, ag)
		if err != nil {
			t.Fatalf("scope %q: %v", tc.scope, err)
		}
		if got != tc.want {
			t.Errorf("scope %q resolved %q, want %q", tc.scope, got, tc.want)
		}
	}
}

// TestScopeTiebreakAppliesWithinTheOwnerTier — ownership is the OUTER tier, scope
// the inner one. A department owning both a global and a prod row gets its prod row
// for a prod run, and neither is ambiguous: they are the same owner.
func TestScopeTiebreakAppliesWithinTheOwnerTier(t *testing.T) {
	f := newOwnerFixture(t)
	f.ownedSecret(t, "s-ag", "TOKEN", "", "ag-a", "teama-global")
	f.ownedSecret(t, "s-ap", "TOKEN", "prod", "ag-a", "teama-prod")

	got, err := f.resolveSecret(t, "TOKEN", "prod", []string{"TeamA"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "teama-prod" {
		t.Errorf("scope tiebreak inside the owner tier: got %q, want teama-prod", got)
	}
	got, err = f.resolveSecret(t, "TOKEN", "dev", []string{"TeamA"})
	if err != nil {
		t.Fatalf("resolve dev: %v", err)
	}
	if got != "teama-global" {
		t.Errorf("dev run should fall back to the owner's global row: got %q", got)
	}
}

// TestOwnershipCannotReachAnotherDepartment — the security direction. Ownership only
// ever NARROWS which of several visible rows wins; it can never make a row visible
// that membership excludes.
func TestOwnershipCannotReachAnotherDepartment(t *testing.T) {
	f := newOwnerFixture(t)
	f.ownedSecret(t, "s-b", "TEAMB_ONLY", "prod", "ag-b", "teamb-pw")

	if _, err := f.resolveSecret(t, "TEAMB_ONLY", "prod", []string{"TeamA"}); err == nil {
		t.Fatal("TeamA resolved TeamB's owned row")
	}
	// And an owned row whose membership has DRIFTED away from its owner is not
	// silently reachable either: pickOwned drops a candidate whose owner is not in
	// the run's snapshot, so the visibility clause is not the only thing standing
	// between the two departments.
	f.exec(`DELETE FROM secret_agencies WHERE secret_id='s-b'`)
	if _, err := f.resolveSecret(t, "TEAMB_ONLY", "prod", []string{"TeamA"}); err == nil {
		t.Error("a drifted owned row (owner set, membership gone) leaked to another department")
	}
}

// TestVariablesGetTheSameTiers — "it works for secrets" is exactly the assumption
// that let variables ship ungated in v0.56.6 (RF-3).
func TestVariablesGetTheSameTiers(t *testing.T) {
	f := newOwnerFixture(t)
	ctx := context.Background()
	scope := "prod"
	mk := func(id, value, owner string) {
		t.Helper()
		ev, err := settings.CreateEnvVar(ctx, f.pool, settings.EnvVarInput{
			Key: "REGION", Value: value, Scope: &scope, OwnerAgency: owner,
		}, "seed")
		if err != nil {
			t.Fatalf("create var (owner %q): %v", owner, err)
		}
		if owner != "" {
			f.exec(`INSERT INTO env_var_agencies(env_var_id,agency_id) VALUES(?,?)`, ev.ID, owner)
		}
	}
	mk("v-a", "eu-west", "ag-a")
	mk("v-b", "us-east", "ag-b")

	out, err := f.r.Resolve(ctx, nil, "prod", []string{"TeamA"}, []Binding{{Kind: KindVar, Name: "REGION"}})
	if err != nil {
		t.Fatalf("TeamA var resolve: %v", err)
	}
	if out.Env["AMADEUS_VAR_REGION"] != "eu-west" {
		t.Errorf("TeamA got %q, want eu-west", out.Env["AMADEUS_VAR_REGION"])
	}
	if _, err := f.r.Resolve(ctx, nil, "prod", []string{"TeamA", "TeamB"},
		[]Binding{{Kind: KindVar, Name: "REGION"}}); !errors.Is(err, ErrAmbiguousReference) {
		t.Errorf("two-owner variable did not fail closed: %v", err)
	}
}

// TestKeyLabelsGetTheSameTiers is RA-19, the part the product owner added over the draft's defer
// recommendation. Labels have no scope dimension, so the ambiguity is EASIER to hit
// here than for secrets: two departments owning `deploy_key` collide outright with
// no scope to separate them.
func TestKeyLabelsGetTheSameTiers(t *testing.T) {
	f := newOwnerFixture(t)
	ctx := context.Background()
	// Inserted explicitly rather than through seedKey + a follow-up UPDATE: two rows
	// share a label here, which is the whole point, and an id assigned by a helper
	// then patched into place is the kind of setup that passes for the wrong reason.
	mkKey := func(id, owner, material string) {
		t.Helper()
		sealed, err := secrets.NewSealer(f.cfg).Seal([]byte(material))
		if err != nil {
			t.Fatalf("seal %s: %v", id, err)
		}
		f.exec(`INSERT INTO ssh_credentials
			(id, label, source, ciphertext, nonce, wrapped_dek, kek_version, owner_agency,
			 created_by, created_at, last_modified_by, last_modified_at)
			VALUES (?, 'deploy_key', 'stored', ?, ?, ?, ?, ?, 'seed', '2026-01-01T00:00:00Z', 'seed', '2026-01-01T00:00:00Z')`,
			id, sealed.Ciphertext, sealed.Nonce, sealed.WrappedDEK, sealed.KEKVersion, owner)
		if owner != "" {
			f.exec(`INSERT INTO ssh_credential_agencies(credential_id,agency_id) VALUES(?,?)`, id, owner)
		}
	}
	mkKey("k-a", "ag-a", "PEM-TEAMA")
	mkKey("k-b", "ag-b", "PEM-TEAMB")

	out, err := f.r.Resolve(ctx, nil, "prod", []string{"TeamA"}, []Binding{{Kind: KindKey, Name: "deploy_key"}})
	if err != nil {
		t.Fatalf("TeamA key resolve: %v", err)
	}
	if len(out.Keys) != 1 || out.Keys[0].Material != "PEM-TEAMA" {
		t.Errorf("TeamA got %+v, want PEM-TEAMA", out.Keys)
	}
	out, err = f.r.Resolve(ctx, nil, "prod", []string{"TeamB"}, []Binding{{Kind: KindKey, Name: "deploy_key"}})
	if err != nil {
		t.Fatalf("TeamB key resolve: %v", err)
	}
	if len(out.Keys) != 1 || out.Keys[0].Material != "PEM-TEAMB" {
		t.Errorf("TeamB got %+v, want PEM-TEAMB", out.Keys)
	}
	// Both owners in one snapshot: refuse rather than ship one department's private
	// key to the other's run.
	if _, err := f.r.Resolve(ctx, nil, "prod", []string{"TeamA", "TeamB"},
		[]Binding{{Kind: KindKey, Name: "deploy_key"}}); !errors.Is(err, ErrAmbiguousReference) {
		t.Errorf("two same-labelled owned keys did not fail closed: %v", err)
	}
}

// TestFrozenSnapshotGovernsOwnership — AG-Q8 still holds. Ownership is resolved
// against the run's FROZEN agency snapshot, so re-homing a scope mid-flight cannot
// retarget which department's row an in-flight run receives.
func TestFrozenSnapshotGovernsOwnership(t *testing.T) {
	f := newOwnerFixture(t)
	f.ownedSecret(t, "s-a", "TOKEN", "prod", "ag-a", "teama-pw")
	f.ownedSecret(t, "s-b", "TOKEN", "prod", "ag-b", "teamb-pw")

	// The snapshot is the argument — there is no live-membership path into this
	// decision at all, which is the property worth pinning.
	got, err := f.resolveSecret(t, "TOKEN", "prod", []string{"TeamA"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "teama-pw" {
		t.Fatalf("got %q", got)
	}
	// Re-home the SCOPE to TeamB. An in-flight run still carries the old snapshot and
	// must still get TeamA's row.
	f.exec(`INSERT INTO scopes(id,name,source,created_at) VALUES('sc-p','prod','amadeus','2026-01-01T00:00:00Z')`)
	f.exec(`INSERT INTO scope_agencies(scope_id,agency_id) VALUES('sc-p','ag-b')`)
	got, err = f.resolveSecret(t, "TOKEN", "prod", []string{"TeamA"})
	if err != nil {
		t.Fatalf("resolve after re-home: %v", err)
	}
	if got != "teama-pw" {
		t.Errorf("live membership leaked into resolution: got %q, want teama-pw", got)
	}
}

// TestFileDeliveryKeepsTheValueOutOfEnv is RA-12's core invariant. A file-mode
// binding must NOT also land in Env: shipping both would leave the value in the
// environment, which is the entire exposure file delivery exists to close.
// Redaction is unaffected, because it is keyed by value rather than by name.
func TestFileDeliveryKeepsTheValueOutOfEnv(t *testing.T) {
	f := newOwnerFixture(t)
	f.ownedSecret(t, "s-b", "BECOME_PASSWORD", "", "", "sudo-pw")

	out, err := f.r.Resolve(context.Background(), nil, "", nil, []Binding{
		{Kind: KindSecret, Name: "BECOME_PASSWORD", File: true},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, present := out.Env["AMADEUS_SECRET_BECOME_PASSWORD"]; present {
		t.Error("a file-delivered secret ALSO landed in Env — the value is exposed in the " +
			"process environment, which is what file delivery exists to prevent")
	}
	if len(out.Files) != 1 {
		t.Fatalf("expected 1 file material, got %d", len(out.Files))
	}
	if out.Files[0].Value != "sudo-pw" || out.Files[0].Reference != "AMADEUS_SECRET_BECOME_PASSWORD" {
		t.Errorf("file material = %+v", out.Files[0])
	}
	// Redaction still covers it: a playbook that cats the file has its bytes masked.
	if !contains(out.Redact, "sudo-pw") {
		t.Errorf("file-delivered value missing from the redaction dictionary: %v", out.Redact)
	}
}

// TestFileDeliveryComposesWithAliasing — Phase A and Phase B are orthogonal. The
// alias picks the reference; File picks the delivery channel. Both at once must
// give a file exposed under the ALIASED reference.
func TestFileDeliveryComposesWithAliasing(t *testing.T) {
	f := newOwnerFixture(t)
	f.ownedSecret(t, "s-a", "TEAMA_SUDO", "", "", "teama-pw")

	out, err := f.r.Resolve(context.Background(), nil, "", nil, []Binding{
		{Kind: KindSecret, Name: "TEAMA_SUDO", As: "BECOME_PASSWORD", File: true},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(out.Files) != 1 || out.Files[0].Reference != "AMADEUS_SECRET_BECOME_PASSWORD" {
		t.Fatalf("alias not applied to file delivery: %+v", out.Files)
	}
	if out.Files[0].Name != "TEAMA_SUDO" {
		t.Errorf("Name should stay the ROW name for audit, got %q", out.Files[0].Name)
	}
}

// TestFileAndValueDeliveryAreDistinctBindings — DedupeKey includes the delivery
// mode. The same secret wanted both as a value and as a file is two bindings; a
// dedupe on kind+name+alias alone would silently drop one of them.
func TestFileAndValueDeliveryAreDistinctBindings(t *testing.T) {
	valueForm := Binding{Kind: KindSecret, Name: "P"}
	fileForm := Binding{Kind: KindSecret, Name: "P", File: true}
	if DedupeKey(valueForm) == DedupeKey(fileForm) {
		t.Fatal("value and file delivery of one secret collapse to the same dedupe key")
	}
	// ...and they DO collide when left on the same key. Both land on
	// AMADEUS_SECRET_P with different contents — the secret, and a path to the
	// secret — and whichever the agent writes last wins, so a job body would read a
	// path where it expected a password with nothing saying so.
	err := CheckAliasCollisions([]Binding{valueForm, fileForm})
	if err == nil {
		t.Fatal("value+file delivery on ONE key accepted — the agent would silently " +
			"overwrite the value with the file path")
	}
	if !strings.Contains(err.Error(), "AMADEUS_SECRET_P") {
		t.Errorf("collision error should name the contested key: %v", err)
	}
	// Aliasing one of them resolves it: two keys, two meanings, no ambiguity.
	if err := CheckAliasCollisions([]Binding{
		valueForm, {Kind: KindSecret, Name: "P", As: "P_FILE", File: true},
	}); err != nil {
		t.Errorf("aliasing the file form should resolve the collision: %v", err)
	}
}

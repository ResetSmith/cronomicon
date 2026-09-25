package runref

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/envref"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// Phase A (RA-1..RA-8, the runas-update plan) — the alias fence.
//
// The entire security argument for aliasing is §2.2: an alias is a DESTINATION
// applied after resolution, and it changes nothing about entitlement. These tests
// are the fence around that claim. An implementation that resolves BY alias, or
// lets an alias reach a row the binding could not, inverts the model — and would
// pass a naive "does it inject under the new name?" test while failing these.

// TestAliasInjectsUnderTheAliasNotTheRowName is RA-5/RA-6's core anchor: the value
// lands on the alias's derived key and the row's own key is ABSENT. The absence
// half matters as much as the presence half — injecting under both would keep the
// row name a working contract, and a job body written against it would silently
// keep working while the operator believed they had re-pointed it.
func TestAliasInjectsUnderTheAliasNotTheRowName(t *testing.T) {
	pool := openDB(t)
	cfg := testCfg(t)
	ctx := context.Background()
	sec := secrets.New(pool, cfg, nilLog())

	if _, err := sec.Create(ctx, secrets.CreateInput{Key: "TEAMA_SUDO", Source: "stored", Value: "a-pw"}, "seed"); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	if _, err := settings.CreateEnvVar(ctx, pool, settings.EnvVarInput{Key: "TEAMA_REGION", Value: "us-east"}, "seed"); err != nil {
		t.Fatalf("seed var: %v", err)
	}
	seedKey(t, pool, cfg, "teama_key", "PEM-A")

	r := NewResolver(pool, cfg, sec, nilLog())
	out, err := r.Resolve(ctx, nil, "", nil, []Binding{
		{Kind: KindSecret, Name: "TEAMA_SUDO", As: "BECOME_PASSWORD"},
		{Kind: KindVar, Name: "TEAMA_REGION", As: "REGION"},
		{Kind: KindKey, Name: "teama_key", As: "DEPLOY_KEY"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if got := out.Env["CRONOMICON_SECRET_BECOME_PASSWORD"]; got != "a-pw" {
		t.Errorf("secret did not land on the alias key: %q", got)
	}
	if _, present := out.Env["CRONOMICON_SECRET_TEAMA_SUDO"]; present {
		t.Error("aliased secret ALSO injected under its row name — the alias must REPLACE the key, not add one")
	}
	if got := out.Env["CRONOMICON_VAR_REGION"]; got != "us-east" {
		t.Errorf("variable did not land on the alias key: %q", got)
	}
	if _, present := out.Env["CRONOMICON_VAR_TEAMA_REGION"]; present {
		t.Error("aliased variable ALSO injected under its row name")
	}

	// RA-5 for keys: Reference (the env key a job body reads) takes the alias, while
	// Name stays the credential's own label — the agent keys its key-map by Name, so
	// aliasing that would break the runner's own signer lookup for a delivered key.
	if len(out.Keys) != 1 {
		t.Fatalf("expected 1 key material, got %d", len(out.Keys))
	}
	if out.Keys[0].Reference != "CRONOMICON_KEY_DEPLOY_KEY" {
		t.Errorf("key reference not aliased: %q", out.Keys[0].Reference)
	}
	if out.Keys[0].Name != "teama_key" {
		t.Errorf("key NAME must stay the credential label (the agent's key-map key), got %q", out.Keys[0].Name)
	}
}

// TestTwoAgenciesAliasToOneName is the whole point of Phase A: one shared job body
// reading a fixed CRONOMICON_SECRET_BECOME_PASSWORD, two departments, each run fed its
// OWN department's row. The two runs differ only in their agency snapshot.
func TestTwoAgenciesAliasToOneName(t *testing.T) {
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
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-a','TeamA',?)`, now)
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-b','TeamB',?)`, now)

	mk := func(key, agency, value string) {
		t.Helper()
		scope := "prod"
		s, err := sec.Create(ctx, secrets.CreateInput{Key: key, Source: "stored", Scope: &scope, Value: value}, "seed")
		if err != nil {
			t.Fatalf("seed secret %s: %v", key, err)
		}
		exec(`INSERT INTO secret_agencies(secret_id, agency_id) VALUES(?,?)`, s.ID, agency)
	}
	mk("TEAMA_SUDO", "ag-a", "teama-pw")
	mk("TEAMB_SUDO", "ag-b", "teamb-pw")

	r := NewResolver(pool, cfg, sec, nilLog())
	// Each department's run declares ITS row under the shared destination name. (This
	// is the naming-convention form Phase A delivers; Phase E later removes the need
	// for the convention by letting both rows be named BECOME_PASSWORD outright.)
	runA, err := r.Resolve(ctx, nil, "prod", []string{"TeamA"},
		[]Binding{{Kind: KindSecret, Name: "TEAMA_SUDO", As: "BECOME_PASSWORD"}})
	if err != nil {
		t.Fatalf("TeamA resolve: %v", err)
	}
	runB, err := r.Resolve(ctx, nil, "prod", []string{"TeamB"},
		[]Binding{{Kind: KindSecret, Name: "TEAMB_SUDO", As: "BECOME_PASSWORD"}})
	if err != nil {
		t.Fatalf("TeamB resolve: %v", err)
	}
	if runA.Env["CRONOMICON_SECRET_BECOME_PASSWORD"] != "teama-pw" {
		t.Errorf("TeamA got %q", runA.Env["CRONOMICON_SECRET_BECOME_PASSWORD"])
	}
	if runB.Env["CRONOMICON_SECRET_BECOME_PASSWORD"] != "teamb-pw" {
		t.Errorf("TeamB got %q", runB.Env["CRONOMICON_SECRET_BECOME_PASSWORD"])
	}
}

// TestAliasIsNotABypass is the §10 open risk, closed. An alias must not let a
// binding reach a row the run's agency snapshot excludes — not by naming the
// out-of-agency row and aliasing it to something innocuous, and not by aliasing an
// in-agency row TO the out-of-agency row's name. Both still fail closed / still
// resolve their own row.
func TestAliasIsNotABypass(t *testing.T) {
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
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-a','TeamA',?)`, now)
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-b','TeamB',?)`, now)

	secretB, err := sec.Create(ctx, secrets.CreateInput{Key: "TEAMB_SUDO", Source: "stored", Value: "teamb-pw"}, "seed")
	if err != nil {
		t.Fatalf("seed TeamB secret: %v", err)
	}
	exec(`INSERT INTO secret_agencies(secret_id, agency_id) VALUES(?,?)`, secretB.ID, "ag-b")
	secretA, err := sec.Create(ctx, secrets.CreateInput{Key: "TEAMA_SUDO", Source: "stored", Value: "teama-pw"}, "seed")
	if err != nil {
		t.Fatalf("seed TeamA secret: %v", err)
	}
	exec(`INSERT INTO secret_agencies(secret_id, agency_id) VALUES(?,?)`, secretA.ID, "ag-a")

	r := NewResolver(pool, cfg, sec, nilLog())

	// (a) Naming TeamB's row from a TeamA run still fails closed, alias or not — the
	// alias is applied AFTER resolution, so there is nothing for it to rescue.
	if _, err := r.Resolve(ctx, nil, "", []string{"TeamA"},
		[]Binding{{Kind: KindSecret, Name: "TEAMB_SUDO", As: "BECOME_PASSWORD"}}); err == nil {
		t.Fatal("a valid alias on an out-of-agency row resolved — the alias became a bypass")
	} else if !errors.Is(err, ErrOutOfScope) && !errors.Is(err, ErrMissingReference) {
		t.Errorf("unexpected error class: %v", err)
	}

	// (b) Aliasing TeamA's row TO TeamB's row name resolves TeamA's VALUE. The alias
	// names a key, never a row: if this ever returned "teamb-pw", resolution would be
	// running off the alias and the whole model would be inverted.
	out, err := r.Resolve(ctx, nil, "", []string{"TeamA"},
		[]Binding{{Kind: KindSecret, Name: "TEAMA_SUDO", As: "TEAMB_SUDO"}})
	if err != nil {
		t.Fatalf("aliasing to another row's name must be legal (it is only a key): %v", err)
	}
	if got := out.Env["CRONOMICON_SECRET_TEAMB_SUDO"]; got != "teama-pw" {
		t.Errorf("alias selected a row instead of naming a key: got %q, want teama-pw", got)
	}
}

// TestRedactionFollowsTheAlias is RA-8. The redaction dictionary is keyed by VALUE,
// not by name, so aliasing SHOULD be transparent to it — this test exists precisely
// so that stays true. Redaction has drifted between the two injection sites before
// (v0.56.6 gated secrets but not variables), and a miss here is a log leak.
func TestRedactionFollowsTheAlias(t *testing.T) {
	pool := openDB(t)
	cfg := testCfg(t)
	ctx := context.Background()
	sec := secrets.New(pool, cfg, nilLog())

	if _, err := sec.Create(ctx, secrets.CreateInput{Key: "TEAMA_SUDO", Source: "stored", Value: "s3cr3t-pw"}, "seed"); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	seedKey(t, pool, cfg, "teama_key", "PEM-SECRET-MATERIAL")

	r := NewResolver(pool, cfg, sec, nilLog())
	out, err := r.Resolve(ctx, nil, "", nil, []Binding{
		{Kind: KindSecret, Name: "TEAMA_SUDO", As: "BECOME_PASSWORD"},
		{Kind: KindKey, Name: "teama_key", As: "DEPLOY_KEY"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, want := range []string{"s3cr3t-pw", "PEM-SECRET-MATERIAL"} {
		if !contains(out.Redact, want) {
			t.Errorf("aliased value %q missing from the redaction dictionary: %v", want, out.Redact)
		}
	}
}

func contains(hay []string, needle string) bool {
	return slices.Contains(hay, needle)
}

// TestAliasCollisionFailsClosed is RA-Q2 at the authoritative seam. Dispatch is the
// only place the run's FULL binding set exists (job + script + per-run additions),
// so it is the only place a collision between a declared binding and a per-run one
// can be seen. No silent winner: whichever value the map write reached last would
// escalate with somebody's credential and the audit would imply an intent that
// never existed.
func TestAliasCollisionFailsClosed(t *testing.T) {
	pool := openDB(t)
	cfg := testCfg(t)
	ctx := context.Background()
	sec := secrets.New(pool, cfg, nilLog())

	for _, k := range []string{"TEAMA_SUDO", "TEAMB_SUDO", "BECOME_PASSWORD"} {
		if _, err := sec.Create(ctx, secrets.CreateInput{Key: k, Source: "stored", Value: k + "-pw"}, "seed"); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	r := NewResolver(pool, cfg, sec, nilLog())

	// Two DIFFERENT rows aliased to one destination.
	_, err := r.Resolve(ctx, nil, "", nil, []Binding{
		{Kind: KindSecret, Name: "TEAMA_SUDO", As: "BECOME_PASSWORD"},
		{Kind: KindSecret, Name: "TEAMB_SUDO", As: "BECOME_PASSWORD"},
	})
	if err == nil {
		t.Fatal("two rows aliased to one key resolved — one credential silently won")
	}
	if !strings.Contains(err.Error(), "CRONOMICON_SECRET_BECOME_PASSWORD") {
		t.Errorf("collision error should name the contested key, got: %v", err)
	}

	// An alias colliding with an UN-aliased binding's own name is the same clash.
	if _, err := r.Resolve(ctx, nil, "", nil, []Binding{
		{Kind: KindSecret, Name: "BECOME_PASSWORD"},
		{Kind: KindSecret, Name: "TEAMA_SUDO", As: "BECOME_PASSWORD"},
	}); err == nil {
		t.Error("an alias colliding with another binding's row name resolved")
	}

	// Cross-KIND is NOT a collision: the derived prefixes differ, so both land.
	if _, err := r.Resolve(ctx, nil, "", nil, []Binding{
		{Kind: KindSecret, Name: "TEAMA_SUDO", As: "SHARED"},
		{Kind: KindVar, Name: "TEAMB_SUDO", As: "SHARED"},
	}); err != nil && strings.Contains(err.Error(), "collision") {
		t.Errorf("secret and var aliased to the same BARE name are distinct keys: %v", err)
	}

	// One row bound bare AND `as` its own name is one binding, not a clash.
	if _, err := r.Resolve(ctx, nil, "", nil, []Binding{
		{Kind: KindSecret, Name: "TEAMA_SUDO"},
		{Kind: KindSecret, Name: "TEAMA_SUDO", As: "TEAMA_SUDO"},
	}); err != nil {
		t.Errorf("same row, same destination, two spellings must collapse: %v", err)
	}
}

// TestAliasValidationAndStorage covers RA-1's charset bar and RA-2's round-trip.
// An alias mints an env-var key exactly as a row name does, so a weaker bar here
// would let a binding produce a key no row could ever have been named.
func TestAliasValidationAndStorage(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	owner := Owner{Kind: "script", Name: "s"}

	bad := []struct {
		name string
		b    Binding
	}{
		{"cronomicon-prefixed alias", Binding{Kind: KindVar, Name: "OK", As: "CRONOMICON_FOO"}},
		{"non-posix alias", Binding{Kind: KindVar, Name: "OK", As: "has-dash"}},
		{"reserved KEK alias on a secret", Binding{Kind: KindSecret, Name: "OK", As: "KEK"}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			err := ReplaceBindings(ctx, pool, owner, []Binding{tc.b}, "alice")
			if err == nil {
				t.Fatal("expected an alias validation error")
			}
			if _, ok := errors.AsType[*envref.Error](err); !ok {
				t.Fatalf("expected *envref.Error (→422), got %T: %v", err, err)
			}
		})
	}

	// RA-2 round-trip, including the widened PK (migration 820): one row declared
	// under TWO destinations is two bindings, not a constraint violation.
	if err := ReplaceBindings(ctx, pool, owner, []Binding{
		{Kind: KindSecret, Name: "TEAMA_SUDO", As: "BECOME_PASSWORD"},
		{Kind: KindSecret, Name: "TEAMA_SUDO", As: "SUDO_PASS"},
		{Kind: KindVar, Name: "REGION"},
	}, "alice"); err != nil {
		t.Fatalf("ReplaceBindings with aliases: %v", err)
	}
	got, err := ListBindings(ctx, pool, owner)
	if err != nil {
		t.Fatalf("ListBindings: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 bindings (one row, two aliases, plus the var), got %d: %+v", len(got), got)
	}
	// Reference stays the row's OWN derived form — the binding's identity, which the
	// authoring surfaces label the chip with. The destination is InjectReference().
	for _, b := range got {
		if b.Kind != KindSecret {
			continue
		}
		if b.Reference != "CRONOMICON_SECRET_TEAMA_SUDO" {
			t.Errorf("Reference should stay the row's own form, got %q", b.Reference)
		}
		if b.InjectReference() != "CRONOMICON_SECRET_"+b.As {
			t.Errorf("InjectReference %q does not follow alias %q", b.InjectReference(), b.As)
		}
	}

	// A collision is refused on the AUTHORING write too, not only at dispatch.
	if err := ReplaceBindings(ctx, pool, owner, []Binding{
		{Kind: KindSecret, Name: "TEAMA_SUDO", As: "BECOME_PASSWORD"},
		{Kind: KindSecret, Name: "TEAMB_SUDO", As: "BECOME_PASSWORD"},
	}, "alice"); err == nil {
		t.Fatal("colliding aliases accepted on a binding write")
	}
}

// TestOverrideBindingsCarryAlias covers the envelope read half of RA-7, including
// the deliberate tolerance: a hand-edited envelope with a MALFORMED alias drops to
// the row's own name rather than dropping a binding the actor was authorized to
// attach.
func TestOverrideBindingsCarryAlias(t *testing.T) {
	got := OverrideBindings(`{"references":[
		{"kind":"secret","name":"TEAMA_SUDO","as":"BECOME_PASSWORD"},
		{"kind":"var","name":"REGION"},
		{"kind":"secret","name":"OTHER","as":"has-dash"}
	]}`)
	if len(got) != 3 {
		t.Fatalf("expected 3 bindings, got %d: %+v", len(got), got)
	}
	if got[0].As != "BECOME_PASSWORD" || got[0].InjectReference() != "CRONOMICON_SECRET_BECOME_PASSWORD" {
		t.Errorf("alias not carried: %+v", got[0])
	}
	if got[1].As != "" || got[1].InjectReference() != "CRONOMICON_VAR_REGION" {
		t.Errorf("un-aliased binding changed: %+v", got[1])
	}
	if got[2].As != "" {
		t.Errorf("malformed alias should degrade to the row name, got %q", got[2].As)
	}

	// RA-4: the same row under two aliases survives dedupe; an exact repeat does not.
	got = OverrideBindings(`{"references":[
		{"kind":"secret","name":"X","as":"A"},
		{"kind":"secret","name":"X","as":"B"},
		{"kind":"secret","name":"X","as":"A"}
	]}`)
	if len(got) != 2 {
		t.Fatalf("dedupe key must be kind+name+alias, got %d: %+v", len(got), got)
	}
}

// TestAuditRecordsBothNames is RA-7. An audit that recorded only the destination
// could not answer WHOSE credential a shared job body ran with — which is the one
// question aliasing makes it possible to ask.
func TestAuditRecordsBothNames(t *testing.T) {
	details := AuditDetails([]ResolvedRef{
		{Kind: KindSecret, Name: "TEAMA_SUDO", As: "BECOME_PASSWORD", Source: "stored"},
		{Kind: KindVar, Name: "REGION", Source: "stored"},
	}, "prod")

	for _, want := range []string{`"name":"TEAMA_SUDO"`, `"as":"BECOME_PASSWORD"`} {
		if !strings.Contains(details, want) {
			t.Errorf("audit payload missing %s: %s", want, details)
		}
	}
	// An un-aliased reference stays byte-compatible with the pre-Phase-A shape.
	if strings.Contains(details, `"name":"REGION","as"`) {
		t.Errorf("un-aliased reference should omit `as`: %s", details)
	}

	back := ParseAuditReferences(details)
	if len(back) != 2 {
		t.Fatalf("round-trip lost references: %+v", back)
	}
	var found bool
	for _, b := range back {
		if b.Name == "TEAMA_SUDO" && b.As == "BECOME_PASSWORD" {
			found = true
		}
	}
	if !found {
		t.Errorf("alias lost in the audit round-trip: %+v", back)
	}
}

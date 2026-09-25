package runref

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/envref"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

func testCfg(t *testing.T) *config.Config {
	t.Helper()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	return &config.Config{
		SecretKEKEnv:            base64.StdEncoding.EncodeToString(kek),
		SecretsInjectionEnabled: true,
	}
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func nilLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

//go:fix inline

// seedKey inserts a stored ssh_credentials row directly (sealed) so the key path
// resolves without needing a parseable PEM — the resolver only round-trips the
// decrypted material, it doesn't parse it.
func seedKey(t *testing.T, pool *sql.DB, cfg *config.Config, label, material string) {
	t.Helper()
	sealed, err := secrets.NewSealer(cfg).Seal([]byte(material))
	if err != nil {
		t.Fatalf("seal key %q: %v", label, err)
	}
	_, err = pool.Exec(
		`INSERT INTO ssh_credentials (id, label, source, ciphertext, nonce, wrapped_dek, kek_version,
		                              created_by, created_at, last_modified_by, last_modified_at)
		 VALUES (?, ?, 'stored', ?, ?, ?, ?, 'tester', '2026-01-01T00:00:00Z', 'tester', '2026-01-01T00:00:00Z')`,
		db.NewID(), label, sealed.Ciphertext, sealed.Nonce, sealed.WrappedDEK, sealed.KEKVersion)
	if err != nil {
		t.Fatalf("insert ssh credential %q: %v", label, err)
	}
}

func TestReplaceAndListBindings(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	owner := Owner{Kind: "job", Source: "cronomicon", Name: "deploy"}

	// Duplicate entries dedupe; result is sorted (kind, name).
	err := ReplaceBindings(ctx, pool, owner, []Binding{
		{Kind: KindVar, Name: "REGION"},
		{Kind: KindSecret, Name: "DB_PASS"},
		{Kind: KindSecret, Name: "DB_PASS"}, // dup
		{Kind: KindKey, Name: "deploy_key"},
	}, "alice")
	if err != nil {
		t.Fatalf("ReplaceBindings: %v", err)
	}
	got, err := ListBindings(ctx, pool, owner)
	if err != nil {
		t.Fatalf("ListBindings: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 bindings after dedupe, got %d: %+v", len(got), got)
	}
	// Sorted by (kind, name): key, secret, var.
	if got[0].Kind != KindKey || got[1].Kind != KindSecret || got[2].Kind != KindVar {
		t.Fatalf("unexpected order: %+v", got)
	}
	if got[1].Reference != "CRONOMICON_SECRET_DB_PASS" {
		t.Fatalf("derived reference wrong: %q", got[1].Reference)
	}

	// Full-replace semantics: a second replace overwrites, not appends.
	if err := ReplaceBindings(ctx, pool, owner, []Binding{{Kind: KindVar, Name: "REGION"}}, "alice"); err != nil {
		t.Fatalf("second ReplaceBindings: %v", err)
	}
	got, _ = ListBindings(ctx, pool, owner)
	if len(got) != 1 || got[0].Name != "REGION" {
		t.Fatalf("replace did not overwrite: %+v", got)
	}

	// Empty replace clears.
	if err := ReplaceBindings(ctx, pool, owner, nil, "alice"); err != nil {
		t.Fatalf("clear ReplaceBindings: %v", err)
	}
	if got, _ = ListBindings(ctx, pool, owner); len(got) != 0 {
		t.Fatalf("expected cleared, got %+v", got)
	}
}

func TestBindingsClearedOnOwnerDelete(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	now := "2026-01-01T00:00:00Z"

	// A composed job + a script, each with a binding. Deleting the owner must
	// cascade-clear its bindings (migration 590 triggers) so a later same-named
	// owner cannot silently inherit the grant.
	if _, err := pool.Exec(`INSERT INTO jobs(uid, name, source, run_type, command, content_hash, synced_at)
		VALUES('uid-deploy','deploy','cronomicon','bash','echo','sha256:x',?)`, now); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO scripts(name, run_type, command, content_hash, synced_at)
		VALUES('build','bash','echo','sha256:y',?)`, now); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	jobOwner := Owner{Kind: "job", Source: "cronomicon", Name: "deploy"}
	scriptOwner := Owner{Kind: "script", Name: "build"}
	if err := ReplaceBindings(ctx, pool, jobOwner, []Binding{{Kind: KindSecret, Name: "DB_PASS"}}, "a"); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceBindings(ctx, pool, scriptOwner, []Binding{{Kind: KindVar, Name: "REGION"}}, "a"); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(`DELETE FROM jobs WHERE source='cronomicon' AND name='deploy'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`DELETE FROM scripts WHERE name='build'`); err != nil {
		t.Fatal(err)
	}

	if got, _ := ListBindings(ctx, pool, jobOwner); len(got) != 0 {
		t.Fatalf("job bindings not cascade-cleared: %+v", got)
	}
	if got, _ := ListBindings(ctx, pool, scriptOwner); len(got) != 0 {
		t.Fatalf("script bindings not cascade-cleared: %+v", got)
	}
}

func TestReplaceBindingsValidation(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	owner := Owner{Kind: "script", Source: "", Name: "s"}

	cases := []struct {
		name string
		b    Binding
	}{
		{"invalid kind", Binding{Kind: "bogus", Name: "X"}},
		{"cronomicon-prefixed name", Binding{Kind: KindVar, Name: "CRONOMICON_FOO"}},
		{"non-posix name", Binding{Kind: KindVar, Name: "has-dash"}},
		{"reserved KEK secret", Binding{Kind: KindSecret, Name: "KEK"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ReplaceBindings(ctx, pool, owner, []Binding{tc.b}, "alice")
			if err == nil {
				t.Fatal("expected validation error")
			}
			if _, ok := errors.AsType[*envref.Error](err); !ok {
				t.Fatalf("expected *envref.Error (→422), got %T: %v", err, err)
			}
		})
	}
	// A rejected replace must not have partially written.
	if got, _ := ListBindings(ctx, pool, owner); len(got) != 0 {
		t.Fatalf("failed replace left rows: %+v", got)
	}
}

func TestScanBodyAndNames(t *testing.T) {
	body := `#!/bin/bash
echo "$CRONOMICON_SECRET_DB_PASS"
curl -H "token: ${CRONOMICON_VAR_REGION}" https://x
ssh -i "$CRONOMICON_KEY_deploy_key" host
# a run-context token is not a binding:
echo "$CRONOMICON_RUN_TRACE_ID"
# duplicate secret ref:
echo "$CRONOMICON_SECRET_DB_PASS"
`
	got := ScanBody(body)
	want := []Binding{
		{Kind: KindKey, Name: "deploy_key", Reference: "CRONOMICON_KEY_deploy_key"},
		{Kind: KindSecret, Name: "DB_PASS", Reference: "CRONOMICON_SECRET_DB_PASS"},
		{Kind: KindVar, Name: "REGION", Reference: "CRONOMICON_VAR_REGION"},
	}
	if !slices.EqualFunc(got, want, func(a, b Binding) bool { return a == b }) {
		t.Fatalf("ScanBody mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestLintBareNames(t *testing.T) {
	known := map[string]Kind{"DB_PASS": KindSecret, "REGION": KindVar}
	body := `echo $DB_PASS; echo $REGION; echo $CRONOMICON_SECRET_DB_PASS; echo $UNKNOWN`
	got := LintBareNames(body, known)
	// CRONOMICON_SECRET_DB_PASS is already migrated (namespaced) → not flagged; UNKNOWN
	// is not a known row → not flagged. DB_PASS + REGION bare sites are flagged.
	if len(got) != 2 {
		t.Fatalf("expected 2 bare-name hits, got %+v", got)
	}
	if got[0].Kind != KindSecret || got[0].Name != "DB_PASS" || got[1].Name != "REGION" {
		t.Fatalf("unexpected lint result: %+v", got)
	}
}

func TestResolveHappyPath(t *testing.T) {
	pool := openDB(t)
	cfg := testCfg(t)
	ctx := context.Background()
	sec := secrets.New(pool, cfg, nilLog())

	scGlobal, err := sec.Create(ctx, secrets.CreateInput{Key: "DB_PASS", Source: "stored", Value: "global-pw"}, "alice")
	if err != nil {
		t.Fatalf("create global secret: %v", err)
	}
	_ = scGlobal
	// A prod-scoped secret with the SAME key must win over the global one for a prod run.
	if _, err := sec.Create(ctx, secrets.CreateInput{Key: "DB_PASS", Source: "stored", Scope: new("prod"), Value: "prod-pw"}, "alice"); err != nil {
		t.Fatalf("create prod secret: %v", err)
	}
	if _, err := settings.CreateEnvVar(ctx, pool, settings.EnvVarInput{Key: "REGION", Value: "us-east", Scope: new("prod")}, "alice"); err != nil {
		t.Fatalf("create var: %v", err)
	}
	seedKey(t, pool, cfg, "deploy_key", "PEM-KEY-MATERIAL")

	r := NewResolver(pool, cfg, sec, nilLog())
	out, err := r.Resolve(ctx, []string{"prod"}, "prod", nil, []Binding{
		{Kind: KindSecret, Name: "DB_PASS"},
		{Kind: KindVar, Name: "REGION"},
		{Kind: KindKey, Name: "deploy_key"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if out.Env["CRONOMICON_SECRET_DB_PASS"] != "prod-pw" {
		t.Fatalf("scope preference failed: got %q, want prod-pw", out.Env["CRONOMICON_SECRET_DB_PASS"])
	}
	if out.Env["CRONOMICON_VAR_REGION"] != "us-east" {
		t.Fatalf("var resolve failed: %q", out.Env["CRONOMICON_VAR_REGION"])
	}
	if len(out.Keys) != 1 || out.Keys[0].Material != "PEM-KEY-MATERIAL" || out.Keys[0].Reference != "CRONOMICON_KEY_deploy_key" {
		t.Fatalf("key resolve failed: %+v", out.Keys)
	}
	// Redaction: secret value + key material present; log-safe var value absent.
	if !slices.Contains(out.Redact, "prod-pw") || !slices.Contains(out.Redact, "PEM-KEY-MATERIAL") {
		t.Fatalf("redact missing sensitive values: %+v", out.Redact)
	}
	if slices.Contains(out.Redact, "us-east") {
		t.Fatalf("variable value must not be redacted (log-safe): %+v", out.Redact)
	}
	// Audit metadata (P1.6/D8): the injected secret + variable + KEY are all recorded
	// with their source (keys are audited in lockstep with D8 delivery). No values.
	if len(out.Refs) != 3 {
		t.Fatalf("expected 3 audit refs (secret+var+key), got %+v", out.Refs)
	}
	var sawSecret, sawVar, sawKey bool
	for _, ref := range out.Refs {
		switch {
		case ref.Kind == KindSecret && ref.Name == "DB_PASS" && ref.Source == "stored":
			sawSecret = true
		case ref.Kind == KindVar && ref.Name == "REGION" && ref.Source == "stored":
			sawVar = true
		case ref.Kind == KindKey && ref.Name == "deploy_key":
			sawKey = true
		}
	}
	if !sawSecret || !sawVar || !sawKey {
		t.Fatalf("audit refs missing expected entries: %+v", out.Refs)
	}
}

func TestResolveGlobalFallback(t *testing.T) {
	pool := openDB(t)
	cfg := testCfg(t)
	ctx := context.Background()
	sec := secrets.New(pool, cfg, nilLog())
	// Only a GLOBAL secret exists; a scoped run still gets it.
	if _, err := sec.Create(ctx, secrets.CreateInput{Key: "TOKEN", Source: "stored", Value: "g"}, "alice"); err != nil {
		t.Fatal(err)
	}
	r := NewResolver(pool, cfg, sec, nilLog())
	out, err := r.Resolve(ctx, nil, "prod", nil, []Binding{{Kind: KindSecret, Name: "TOKEN"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if out.Env["CRONOMICON_SECRET_TOKEN"] != "g" {
		t.Fatalf("global fallback failed: %q", out.Env["CRONOMICON_SECRET_TOKEN"])
	}
}

func TestResolveOutOfScope(t *testing.T) {
	pool := openDB(t)
	cfg := testCfg(t)
	ctx := context.Background()
	sec := secrets.New(pool, cfg, nilLog())
	// Secret exists ONLY in staging; a prod run must fail closed with ErrOutOfScope.
	if _, err := sec.Create(ctx, secrets.CreateInput{Key: "STG", Source: "stored", Scope: new("staging"), Value: "s"}, "alice"); err != nil {
		t.Fatal(err)
	}
	r := NewResolver(pool, cfg, sec, nilLog())
	_, err := r.Resolve(ctx, nil, "prod", nil, []Binding{{Kind: KindSecret, Name: "STG"}})
	if !errors.Is(err, ErrOutOfScope) {
		t.Fatalf("expected ErrOutOfScope, got %v", err)
	}
}

func TestResolveMissing(t *testing.T) {
	pool := openDB(t)
	cfg := testCfg(t)
	ctx := context.Background()
	r := NewResolver(pool, cfg, secrets.New(pool, cfg, nilLog()), nilLog())
	_, err := r.Resolve(ctx, nil, "prod", nil, []Binding{{Kind: KindVar, Name: "NOPE"}})
	if !errors.Is(err, ErrMissingReference) {
		t.Fatalf("expected ErrMissingReference, got %v", err)
	}
}

// TestOperatorMessageIsNonOracle (M2): probing the SAME reference name must yield
// IDENTICAL operator-visible text whether it exists only in another scope or not
// at all (no cross-scope existence oracle), while the precise server-log Error()
// text still distinguishes the two for diagnostics.
func TestOperatorMessageIsNonOracle(t *testing.T) {
	cfg := testCfg(t)
	ctx := context.Background()
	binding := []Binding{{Kind: KindSecret, Name: "PROBE"}}

	// DB #1: PROBE exists only in staging → out-of-scope for a prod run.
	poolA := openDB(t)
	secA := secrets.New(poolA, cfg, nilLog())
	if _, err := secA.Create(ctx, secrets.CreateInput{Key: "PROBE", Source: "stored", Scope: new("staging"), Value: "s"}, "alice"); err != nil {
		t.Fatal(err)
	}
	_, outOfScopeErr := NewResolver(poolA, cfg, secA, nilLog()).Resolve(ctx, nil, "prod", nil, binding)

	// DB #2: PROBE exists nowhere → missing.
	poolB := openDB(t)
	_, missingErr := NewResolver(poolB, cfg, secrets.New(poolB, cfg, nilLog()), nilLog()).Resolve(ctx, nil, "prod", nil, binding)

	if outOfScopeErr == nil || missingErr == nil {
		t.Fatalf("both resolves must fail closed: outOfScope=%v missing=%v", outOfScopeErr, missingErr)
	}

	// Same reference name → operator text must be byte-identical (no oracle).
	if OperatorMessage(outOfScopeErr) != OperatorMessage(missingErr) {
		t.Errorf("operator message leaks existence: out-of-scope=%q missing=%q",
			OperatorMessage(outOfScopeErr), OperatorMessage(missingErr))
	}
	if strings.Contains(OperatorMessage(outOfScopeErr), "outside run scope") {
		t.Errorf("operator message leaks scope oracle: %q", OperatorMessage(outOfScopeErr))
	}

	// The server-log Error() text still distinguishes them (precise diagnostics).
	if outOfScopeErr.Error() == missingErr.Error() {
		t.Errorf("server-log text should stay precise, but both were %q", outOfScopeErr.Error())
	}
	// errors.Is classification is preserved through the ReferenceError wrapper.
	if !errors.Is(outOfScopeErr, ErrOutOfScope) {
		t.Errorf("out-of-scope no longer classifies as ErrOutOfScope: %v", outOfScopeErr)
	}
	if !errors.Is(missingErr, ErrMissingReference) {
		t.Errorf("missing no longer classifies as ErrMissingReference: %v", missingErr)
	}
}

func TestResolveActorScopeGate(t *testing.T) {
	pool := openDB(t)
	cfg := testCfg(t)
	ctx := context.Background()
	r := NewResolver(pool, cfg, secrets.New(pool, cfg, nilLog()), nilLog())
	// Restricted actor (dev only) cannot inject into a prod run.
	_, err := r.Resolve(ctx, []string{"dev"}, "prod", nil, nil)
	if !errors.Is(err, ErrOutOfScope) {
		t.Fatalf("expected ErrOutOfScope from actor gate, got %v", err)
	}
	// Unrestricted actor (empty scopes) may run anywhere.
	if _, err := r.Resolve(ctx, nil, "prod", nil, nil); err != nil {
		t.Fatalf("unrestricted actor rejected: %v", err)
	}
}

func TestResolveKillSwitch(t *testing.T) {
	pool := openDB(t)
	cfg := testCfg(t)
	cfg.SecretsInjectionEnabled = false
	ctx := context.Background()
	sec := secrets.New(pool, cfg, nilLog())
	if _, err := sec.Create(ctx, secrets.CreateInput{Key: "X", Source: "stored", Value: "v"}, "alice"); err != nil {
		t.Fatal(err)
	}
	r := NewResolver(pool, cfg, sec, nilLog())
	out, err := r.Resolve(ctx, nil, "", nil, []Binding{{Kind: KindSecret, Name: "X"}})
	if err != nil {
		t.Fatalf("Resolve with kill-switch off: %v", err)
	}
	if len(out.Env) != 0 || len(out.Keys) != 0 || len(out.Redact) != 0 {
		t.Fatalf("kill-switch off must inject nothing, got %+v", out)
	}
}

// fakeVaultClient is an in-memory secrets.VaultClient: vault_ref → value. It lets
// the resolver tests exercise the vault-source path (Reveal → VaultClient.Fetch)
// without standing up an HTTP fake — the injection seam is source-transparent, so
// this is exactly what a real Vault delivers to Resolve.
type fakeVaultClient map[string]string

func (f fakeVaultClient) Fetch(ref string) (string, error) {
	if v, ok := f[ref]; ok {
		return v, nil
	}
	return "", fmt.Errorf("vault: no value at %q", ref)
}
func (f fakeVaultClient) Write(ref, val string) error { f[ref] = val; return nil }

// downVaultClient models a reachable-but-failing Vault (outage / permission).
type downVaultClient struct{}

func (downVaultClient) Fetch(string) (string, error) { return "", errors.New("vault unavailable") }
func (downVaultClient) Write(string, string) error   { return errors.New("vault unavailable") }

// TestResolveMixedStoredAndVault is the P2.2 core: a run binding both a stored and
// a vault-source secret (plus a variable) resolves all three through the SAME path,
// with the vault value injected and — critically — added to the redaction slice
// even though it is absent from the stored-secret global dictionary.
func TestResolveMixedStoredAndVault(t *testing.T) {
	pool := openDB(t)
	cfg := testCfg(t)
	ctx := context.Background()
	sec := secrets.New(pool, cfg, nilLog())
	sec.WithVaultClient(fakeVaultClient{"secret/data/app#API_TOKEN": "vault-tok"})

	if _, err := sec.Create(ctx, secrets.CreateInput{Key: "DB_PASS", Source: "stored", Value: "stored-pw"}, "alice"); err != nil {
		t.Fatalf("create stored secret: %v", err)
	}
	if _, err := sec.Create(ctx, secrets.CreateInput{Key: "API_TOKEN", Source: "vault", VaultPath: "secret/data/app#API_TOKEN"}, "alice"); err != nil {
		t.Fatalf("create vault secret: %v", err)
	}
	if _, err := settings.CreateEnvVar(ctx, pool, settings.EnvVarInput{Key: "REGION", Value: "us-east"}, "alice"); err != nil {
		t.Fatalf("create var: %v", err)
	}

	r := NewResolver(pool, cfg, sec, nilLog())
	out, err := r.Resolve(ctx, nil, "", nil, []Binding{
		{Kind: KindSecret, Name: "DB_PASS"},
		{Kind: KindSecret, Name: "API_TOKEN"},
		{Kind: KindVar, Name: "REGION"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if out.Env["CRONOMICON_SECRET_DB_PASS"] != "stored-pw" || out.Env["CRONOMICON_SECRET_API_TOKEN"] != "vault-tok" {
		t.Fatalf("mixed resolve failed: %+v", out.Env)
	}
	// Redaction must cover the vault value (it is NOT in the stored global dictionary).
	if !slices.Contains(out.Redact, "stored-pw") || !slices.Contains(out.Redact, "vault-tok") {
		t.Fatalf("redact missing a secret value: %+v", out.Redact)
	}
	if slices.Contains(out.Redact, "us-east") {
		t.Fatalf("variable value must not be redacted (log-safe): %+v", out.Redact)
	}
	// Audit provenance: the vault-source secret is recorded as source "vault".
	var vaultSourced bool
	for _, ref := range out.Refs {
		if ref.Kind == KindSecret && ref.Name == "API_TOKEN" {
			if ref.Source != "vault" {
				t.Fatalf("vault secret recorded as source %q, want vault", ref.Source)
			}
			vaultSourced = true
		}
	}
	if !vaultSourced {
		t.Fatalf("audit refs missing vault secret: %+v", out.Refs)
	}
}

// TestResolveVaultUnavailableFailsClosed: when Vault cannot serve a bound vault
// secret, Resolve must fail (no partial injection, no silent skip). Covers both a
// reachable-but-failing Vault and the default stub (no client wired).
func TestResolveVaultUnavailableFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire func(*secrets.Service)
	}{
		{"vault-down", func(s *secrets.Service) { s.WithVaultClient(downVaultClient{}) }},
		{"no-client-stub", func(*secrets.Service) {}}, // default stub ⇒ unavailable
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := openDB(t)
			cfg := testCfg(t)
			ctx := context.Background()
			sec := secrets.New(pool, cfg, nilLog())
			tc.wire(sec)
			if _, err := sec.Create(ctx, secrets.CreateInput{Key: "API_TOKEN", Source: "vault", VaultPath: "secret/data/app#API_TOKEN"}, "alice"); err != nil {
				t.Fatalf("create vault secret: %v", err)
			}
			r := NewResolver(pool, cfg, sec, nilLog())
			if _, err := r.Resolve(ctx, nil, "", nil, []Binding{{Kind: KindSecret, Name: "API_TOKEN"}}); err == nil {
				t.Fatal("expected fail-closed error when Vault cannot serve a bound secret")
			}
		})
	}
}

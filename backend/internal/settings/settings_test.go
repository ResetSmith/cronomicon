package settings

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// openTestPool opens a migrated SQLite DB for a test.
func openTestPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "settings_test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

// ── Env vars (LB2: description persistence + no field clobber) ────────────────

func TestEnvVarDescriptionRoundTrip(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	scope := "Production"
	desc := "How long nightly backups are kept."

	created, err := CreateEnvVar(ctx, pool, EnvVarInput{
		Key: "BACKUP_RETENTION_DAYS", Value: "30", Scope: &scope, Description: &desc,
	}, "tester")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Description == nil || *created.Description != desc {
		t.Fatalf("description not persisted on create: %+v", created.Description)
	}

	// List and Get both surface the description.
	list, err := ListEnvVars(ctx, pool)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if list[0].Description == nil || *list[0].Description != desc {
		t.Fatalf("description missing from list: %+v", list[0].Description)
	}

	// Editing ONLY the description must not clobber value/scope/key (the LB2
	// side-effect). Caller round-trips the existing value (as the SPA form does).
	newDesc := "Updated retention note."
	updated, err := UpdateEnvVar(ctx, pool, created.ID, EnvVarInput{
		Key: "BACKUP_RETENTION_DAYS", Value: "30", Scope: &scope, Description: &newDesc,
	}, "tester")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Description == nil || *updated.Description != newDesc {
		t.Fatalf("description not updated: %+v", updated.Description)
	}
	if updated.Value != "30" {
		t.Fatalf("value clobbered on description edit: %q", updated.Value)
	}
	if updated.Scope == nil || *updated.Scope != scope {
		t.Fatalf("scope clobbered on description edit: %+v", updated.Scope)
	}
	if updated.Key != "BACKUP_RETENTION_DAYS" {
		t.Fatalf("key clobbered on description edit: %q", updated.Key)
	}
}

// ── Notification config ──────────────────────────────────────────────────────

func TestNotificationConfigApprise(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek)}

	// Configure Apprise with targets + an SMTP password.
	_, err := UpdateNotificationConfig(ctx, pool, cfg, NotificationConfig{
		Provider: "apprise",
		SMTP:     &SMTPConfig{Host: "mail.example.com", Port: 587, Encryption: "starttls", Password: "p4ss"},
		Apprise: &AppriseConfig{Enabled: true, APIURL: "http://apprise:8000", Targets: []AppriseTarget{
			{Label: "Ops Slack", Service: "slack", URL: "slack://x", Enabled: true},
			{Label: "Ops Mail", Service: "email", URL: "mailto://y", Enabled: false},
		}},
	}, "tester")
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := GetNotificationConfig(ctx, pool)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Apprise == nil || len(got.Apprise.Targets) != 2 {
		t.Fatalf("apprise targets not round-tripped: %+v", got.Apprise)
	}
	// Rich fields survive the round-trip and ids are server-assigned (1..N).
	if t0 := got.Apprise.Targets[0]; t0.URL != "slack://x" || t0.Label != "Ops Slack" || !t0.Enabled || t0.ID != 1 {
		t.Fatalf("first target wrong: %+v", t0)
	}
	if t1 := got.Apprise.Targets[1]; t1.URL != "mailto://y" || t1.Enabled || t1.ID != 2 {
		t.Fatalf("second target wrong (enabled flag must persist): %+v", t1)
	}
	// Password is write-only: stored (PasswordSet) but never returned.
	if got.SMTP == nil || !got.SMTP.PasswordSet || got.SMTP.Password != "" {
		t.Fatalf("smtp password handling wrong: %+v", got.SMTP)
	}

	// An SMTP-only update (no Apprise block) must preserve the targets.
	_, err = UpdateNotificationConfig(ctx, pool, cfg, NotificationConfig{
		SMTP: &SMTPConfig{Host: "mail2.example.com", Port: 25, Encryption: "none"},
	}, "tester")
	if err != nil {
		t.Fatalf("smtp-only update: %v", err)
	}
	got, _ = GetNotificationConfig(ctx, pool)
	if got.Apprise == nil || len(got.Apprise.Targets) != 2 {
		t.Fatalf("apprise targets wiped by SMTP-only update: %+v", got.Apprise)
	}
	// And the password survives a blank-password update.
	if got.SMTP == nil || !got.SMTP.PasswordSet {
		t.Fatalf("smtp password lost on blank update: %+v", got.SMTP)
	}
}

func TestParseAppriseTargets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []AppriseTarget
	}{
		{"empty", "", []AppriseTarget{}},
		{"blank", "   ", []AppriseTarget{}},
		{"invalid json", "not-json", []AppriseTarget{}},
		{"bare strings are skipped (the pre-rich form, dropped in v1.5.41)", `["slack://x","mailto://y"]`, []AppriseTarget{}},
		{"objects", `[{"label":"L","service":"slack","url":"slack://x","enabled":true},{"url":"mailto://y","enabled":false}]`, []AppriseTarget{
			{ID: 1, Label: "L", Service: "slack", URL: "slack://x", Enabled: true},
			{ID: 2, URL: "mailto://y", Enabled: false},
		}},
		{"client-echoed id is overwritten", `[{"id":99,"url":"slack://x","enabled":true}]`, []AppriseTarget{
			{ID: 1, URL: "slack://x", Enabled: true},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseAppriseTargets(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (%+v)", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("target %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ── Env Vars ─────────────────────────────────────────────────────────────────

func TestEnvVarCRUD(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	// List empty.
	list, err := ListEnvVars(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 env vars, got %d", len(list))
	}

	// Create.
	scope := "production"
	ev, err := CreateEnvVar(ctx, pool, EnvVarInput{
		Key: "API_URL", Value: "https://api.example.com", Scope: &scope,
	}, "alice@example.com")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ev.Key != "API_URL" || ev.Value != "https://api.example.com" {
		t.Fatalf("unexpected env var: %+v", ev)
	}

	// List finds it.
	list, _ = ListEnvVars(ctx, pool)
	if len(list) != 1 {
		t.Fatalf("expected 1, got %d", len(list))
	}

	// Update.
	updated, err := UpdateEnvVar(ctx, pool, ev.ID, EnvVarInput{
		Key: "API_URL", Value: "https://v2.api.example.com", Scope: &scope,
	}, "alice@example.com")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Value != "https://v2.api.example.com" {
		t.Fatalf("update did not persist: %+v", updated)
	}

	// Delete.
	found, err := DeleteEnvVar(ctx, pool, ev.ID, "alice@example.com")
	if err != nil || !found {
		t.Fatalf("Delete: found=%v err=%v", found, err)
	}
	list, _ = ListEnvVars(ctx, pool)
	if len(list) != 0 {
		t.Fatalf("expected 0 after delete, got %d", len(list))
	}

	// Delete non-existent.
	found, err = DeleteEnvVar(ctx, pool, ev.ID, "alice@example.com")
	if err != nil || found {
		t.Fatalf("double delete: found=%v err=%v", found, err)
	}
}

// ── Scopes ────────────────────────────────────────────────────────────────────

func TestScopeCRUD(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	list, _ := ListScopes(ctx, pool, "")
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %d", len(list))
	}

	desc := "production environment"
	sc, err := CreateScope(ctx, pool, LocalScopeInput{
		Scope:          "prod",
		Description:    &desc,
		Hosts:          []string{"host1.example.com", "host2.example.com"},
		SupportedTypes: []string{"bash", "ansible"},
	}, "alice@example.com")
	if err != nil {
		t.Fatalf("CreateScope: %v", err)
	}
	if sc.Scope != "prod" || len(sc.Hosts) != 2 {
		t.Fatalf("unexpected scope: %+v", sc)
	}
	// bash floor should always be present.
	hasBash := false
	for _, ty := range sc.Capability.Types {
		if ty == "bash" {
			hasBash = true
		}
	}
	if !hasBash {
		t.Error("bash floor should be in supported types")
	}

	// Get by ID.
	got, err := GetScope(ctx, pool, sc.ID)
	if err != nil || got == nil {
		t.Fatalf("GetScope: %v, %v", got, err)
	}

	// List.
	list, _ = ListScopes(ctx, pool, "")
	if len(list) != 1 {
		t.Fatalf("expected 1 scope, got %d", len(list))
	}

	// Delete.
	found, err := DeleteScope(ctx, pool, sc.ID, "alice@example.com")
	if err != nil || !found {
		t.Fatalf("DeleteScope: found=%v err=%v", found, err)
	}
	list, _ = ListScopes(ctx, pool, "")
	if len(list) != 0 {
		t.Fatalf("expected 0 after delete, got %d", len(list))
	}
}

func TestScopeRenameKeepsGrantsAndFlagsBrokenRefs(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	// Create a scope.
	sc, err := CreateScope(ctx, pool, LocalScopeInput{
		Scope: "old-name",
		Hosts: []string{"host.example.com"},
	}, "alice@example.com")
	if err != nil {
		t.Fatalf("CreateScope: %v", err)
	}

	// An access grant reaching this scope, via the agency that contains it. The
	// point of asserting on it after the rename is that grants key on agency_id and
	// scope_agencies keys on scope_id — neither stores the scope NAME — so a rename
	// cannot orphan a grant. That is why the scope_restrictions cascade this test
	// used to check no longer exists to check (RB-19, v0.57.8): the table it
	// cascaded is gone, and the model that replaced it never needed the cascade.
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO agencies(id, name, created_at) VALUES ('a1', 'Ops', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("insert agency: %v", err)
	}
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO scope_agencies(scope_id, agency_id) VALUES (?, 'a1')`, sc.ID,
	); err != nil {
		t.Fatalf("insert scope_agencies: %v", err)
	}
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO access_grants(id, ad_group, role, agency_id, all_scopes, created_by, created_at)
		 VALUES ('g1', 'sg-ops', 'operator', 'a1', 0, 'alice@example.com', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("insert grant: %v", err)
	}

	// Insert an env_var that references the old scope name (to generate a broken ref).
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO env_vars(id, key, value, scope, created_by, created_at, last_modified_by, last_modified_at)
		 VALUES ('ev1', 'MY_KEY', 'val', 'old-name', 'alice@example.com', '2026-01-01T00:00:00Z', 'alice@example.com', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("insert env_var: %v", err)
	}

	// Rename.
	updated, broken, err := UpdateScope(ctx, pool, sc.ID, LocalScopeInput{
		Scope: "new-name",
		Hosts: []string{"host.example.com"},
	}, "alice@example.com")
	if err != nil {
		t.Fatalf("UpdateScope rename: %v", err)
	}
	if updated.Scope != "new-name" {
		t.Fatalf("expected scope name to be new-name, got %s", updated.Scope)
	}

	// The grant still reaches the renamed scope, with nothing having been cascaded:
	// membership is by id, and the rename did not touch an id.
	var reach int
	if err := pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM access_grants g
		JOIN scope_agencies sa ON sa.agency_id = g.agency_id
		JOIN scopes s          ON s.id = sa.scope_id
		WHERE g.id = 'g1' AND s.name = 'new-name'`).Scan(&reach); err != nil {
		t.Fatalf("query grant reach after rename: %v", err)
	}
	if reach != 1 {
		t.Errorf("the grant stopped reaching its scope after a rename (%d rows); membership "+
			"is keyed on ids precisely so a rename cannot break it", reach)
	}

	// env_var still references old-name → should appear in brokenReferences.
	if len(broken) == 0 {
		t.Error("expected at least one broken reference (env var still references old scope name)")
	}
	foundEnvVarRef := false
	for _, b := range broken {
		if b.Entity == "envVars" && b.Name == "MY_KEY" {
			foundEnvVarRef = true
		}
	}
	if !foundEnvVarRef {
		t.Errorf("expected MY_KEY in broken references, got: %+v", broken)
	}
}

func TestScopeBashFloorEnforcement(t *testing.T) {
	// Empty types → bash added.
	result := enforceBashFloor([]string{})
	if len(result) != 1 || result[0] != "bash" {
		t.Errorf("empty types: expected [bash], got %v", result)
	}
	// Types without bash → bash prepended.
	result = enforceBashFloor([]string{"ansible", "terraform"})
	if result[0] != "bash" {
		t.Errorf("expected bash first, got %v", result)
	}
	// Types that already have bash → unchanged.
	orig := []string{"bash", "ansible"}
	result = enforceBashFloor(orig)
	if len(result) != 2 {
		t.Errorf("expected 2, got %v", result)
	}
}

// ── Global Settings ───────────────────────────────────────────────────────────

func TestGlobalSettingsGetUpdate(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	// Get defaults (empty table).
	gs, err := GetGlobalSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetGlobalSettings: %v", err)
	}
	if gs.AppName != "Cronomicon" {
		t.Fatalf("expected default appName=Cronomicon, got %q", gs.AppName)
	}
	// defaultExecutor defaults to ssh (R5.1).
	if gs.DefaultExecutor != "ssh" {
		t.Fatalf("expected default defaultExecutor=ssh, got %q", gs.DefaultExecutor)
	}

	// Update.
	inp := GlobalSettings{
		AppName:           "My Cronomicon",
		Timezone:          "America/New_York",
		MaxConcurrent:     5,
		JobTimeoutSeconds: 1800,
		DefaultExecutor:   "runner",
	}
	updated, err := UpdateGlobalSettings(ctx, pool, inp, "alice@example.com")
	if err != nil {
		t.Fatalf("UpdateGlobalSettings: %v", err)
	}
	if updated.AppName != "My Cronomicon" {
		t.Errorf("appName not persisted: %q", updated.AppName)
	}
	if updated.MaxConcurrent != 5 {
		t.Errorf("maxConcurrent not persisted: %d", updated.MaxConcurrent)
	}
	if updated.DefaultExecutor != "runner" {
		t.Errorf("defaultExecutor not persisted: %q", updated.DefaultExecutor)
	}

	// Invalid defaultExecutor is rejected (R5.1) and flagged as a validation error.
	if _, err := UpdateGlobalSettings(ctx, pool, GlobalSettings{DefaultExecutor: "bogus"}, "alice@example.com"); err == nil {
		t.Error("expected error for invalid defaultExecutor, got nil")
	} else if !errors.Is(err, ErrValidation) {
		t.Errorf("invalid defaultExecutor: want ErrValidation, got %v", err)
	}

	// Read back independently.
	readBack, err := GetGlobalSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetGlobalSettings after update: %v", err)
	}
	if readBack.AppName != "My Cronomicon" {
		t.Errorf("appName not durable: %q", readBack.AppName)
	}
}

// TestResolveEffectiveTimezone covers the single zone resolver: a valid stored
// zone wins; unset/invalid degrade to time.Local without panicking (§3, §8).
func TestResolveEffectiveTimezone(t *testing.T) {
	// Set + valid → that zone.
	if got := ResolveEffectiveTimezone(&GlobalSettings{Timezone: "America/New_York"}); got.String() != "America/New_York" {
		t.Errorf("valid zone: got %q, want America/New_York", got.String())
	}
	// UTC stored → UTC.
	if got := ResolveEffectiveTimezone(&GlobalSettings{Timezone: "UTC"}); got.String() != "UTC" {
		t.Errorf("UTC: got %q, want UTC", got.String())
	}
	// Unset → time.Local (the TZ fallback).
	if got := ResolveEffectiveTimezone(&GlobalSettings{Timezone: ""}); got != time.Local {
		t.Errorf("empty: got %q, want time.Local (%q)", got.String(), time.Local.String())
	}
	// nil settings → time.Local, no panic.
	if got := ResolveEffectiveTimezone(nil); got != time.Local {
		t.Errorf("nil: got %q, want time.Local", got.String())
	}
	// Invalid stored value → time.Local, no panic (defense-in-depth; validate-on-
	// save normally prevents this from ever being stored).
	if got := ResolveEffectiveTimezone(&GlobalSettings{Timezone: "Mars/Olympus_Mons"}); got != time.Local {
		t.Errorf("invalid: got %q, want time.Local fallback", got.String())
	}
}

// TestUpdateGlobalSettingsRejectsBadTimezone confirms the save-time gate keeps a
// bad zone out of the scheduler/display path, flagged as a validation error (§6).
func TestUpdateGlobalSettingsRejectsBadTimezone(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	_, err := UpdateGlobalSettings(ctx, pool, GlobalSettings{Timezone: "Not/AZone"}, "alice@example.com")
	if err == nil {
		t.Fatal("expected error for invalid timezone, got nil")
	}
	if !errors.Is(err, ErrValidation) {
		t.Errorf("invalid timezone: want ErrValidation, got %v", err)
	}

	// A valid IANA zone is accepted and persisted.
	if _, err := UpdateGlobalSettings(ctx, pool, GlobalSettings{Timezone: "Europe/Berlin"}, "alice@example.com"); err != nil {
		t.Fatalf("valid timezone rejected: %v", err)
	}
	gs, err := GetGlobalSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetGlobalSettings: %v", err)
	}
	if gs.Timezone != "Europe/Berlin" {
		t.Errorf("timezone not persisted: %q", gs.Timezone)
	}
}

// ── Alerts ────────────────────────────────────────────────────────────────────

func TestAlertRuleCRUD(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	list, err := ListAlertRules(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 alert rules, got %d", len(list))
	}

	ar, err := CreateAlertRule(ctx, pool, AlertRuleInput{
		TargetMode: "all",
		Trigger:    "failure",
		Channels:   []string{"email", "in-app"},
		Enabled:    true,
	}, "alice@example.com")
	if err != nil {
		t.Fatalf("CreateAlertRule: %v", err)
	}
	if ar.Trigger != "failure" || !ar.Enabled {
		t.Fatalf("unexpected rule: %+v", ar)
	}

	// Update.
	updated, err := UpdateAlertRule(ctx, pool, ar.ID, AlertRuleInput{
		TargetMode: "job",
		Trigger:    "success",
		Channels:   []string{"slack"},
		Enabled:    false,
	}, "alice@example.com")
	if err != nil {
		t.Fatalf("UpdateAlertRule: %v", err)
	}
	if updated.Trigger != "success" || updated.Enabled {
		t.Fatalf("update not applied: %+v", updated)
	}

	// Delete.
	found, err := DeleteAlertRule(ctx, pool, ar.ID, "alice@example.com")
	if err != nil || !found {
		t.Fatalf("DeleteAlertRule: found=%v err=%v", found, err)
	}
	list, _ = ListAlertRules(ctx, pool)
	if len(list) != 0 {
		t.Fatalf("expected 0 after delete, got %d", len(list))
	}
}

// ── Change Log audit ──────────────────────────────────────────────────────────

func TestAuditWritesChangeLog(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	if err := writeChangeLog(ctx, pool, "alice@example.com", "Env Vars", "created", "API_KEY", ""); err != nil {
		t.Fatalf("writeChangeLog: %v", err)
	}

	var count int
	if err := pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM change_log WHERE actor='alice@example.com' AND category='Env Vars'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 change_log row, got %d", count)
	}
}

// ── SSH Hosts + Bastions ──────────────────────────────────────────────────────

func TestSshHostCRUD(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	addr := "192.168.1.10"
	osVal := "Linux"
	h, err := CreateSshHost(ctx, pool, SshHostInput{
		Hostname: "prod-host-1", Address: &addr, Port: 22, OS: &osVal,
	}, "alice@example.com")
	if err != nil {
		t.Fatalf("CreateSshHost: %v", err)
	}
	if h.Hostname != "prod-host-1" {
		t.Fatalf("unexpected hostname: %s", h.Hostname)
	}

	list, err := ListSshHosts(ctx, pool)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListSshHosts: %v %v", list, err)
	}

	found, err := DeleteSshHost(ctx, pool, h.ID, "alice@example.com")
	if err != nil || !found {
		t.Fatalf("DeleteSshHost: %v %v", found, err)
	}
}

func TestBastionCRUD(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	b, err := CreateBastion(ctx, pool, SshBastionInput{
		Name: "bastion-us-east", Address: "10.0.0.1", Port: 22,
	}, "alice@example.com")
	if err != nil {
		t.Fatalf("CreateBastion: %v", err)
	}
	if b.Name != "bastion-us-east" {
		t.Fatalf("unexpected name: %s", b.Name)
	}

	list, err := ListBastions(ctx, pool)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListBastions: %v %v", list, err)
	}

	found, err := DeleteBastion(ctx, pool, b.ID, "alice@example.com")
	if err != nil || !found {
		t.Fatalf("DeleteBastion: %v %v", found, err)
	}
}

func TestGitlabConfig(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek)}

	// Initial default check
	initial, err := GetGitlabConfig(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if initial.BotName != "cronomicon-bot" || initial.WriteBranch != "main" {
		t.Fatalf("unexpected defaults: %+v", initial)
	}

	// Update gitlab config
	_, err = UpdateGitlabConfig(ctx, pool, cfg, GitlabConfig{
		Pat:         "my-secret-pat",
		BotName:     "custom-bot",
		BotEmail:    "custom@example.com",
		WriteBranch: "develop",
		RepoUrl:     "https://gitlab.example.com/org/repo.git",
	}, "tester")
	if err != nil {
		t.Fatal(err)
	}

	got, err := GetGitlabConfig(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.BotName != "custom-bot" || got.WriteBranch != "develop" || got.RepoUrl != "https://gitlab.example.com/org/repo.git" {
		t.Fatalf("unexpected values: %+v", got)
	}
	if !got.PatSet || got.Pat != "••••-pat" {
		t.Fatalf("pat masking wrong: %+v", got)
	}
}

func TestVaultConfig(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek)}

	// Initial check
	initial, err := GetVaultConfig(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Status != "unconfigured" {
		t.Fatalf("expected unconfigured, got %s", initial.Status)
	}

	// Update vault config
	ns := "my-namespace"
	_, err = UpdateVaultConfig(ctx, pool, cfg, VaultConfig{
		Addr:       "http://vault.example.com:8200",
		AuthMethod: "approle",
		RoleId:     "my-role-id",
		SecretId:   "my-secret-id",
		Namespace:  &ns,
	}, "tester")
	if err != nil {
		t.Fatal(err)
	}

	got, err := GetVaultConfig(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Addr != "http://vault.example.com:8200" || *got.Namespace != "my-namespace" {
		t.Fatalf("unexpected values: %+v", got)
	}
	if !got.RoleIdSet || got.RoleId != "my-role-id" {
		t.Fatalf("role id set/masked wrong: %+v", got)
	}
	if !got.SecretIdSet || got.SecretId != "" {
		t.Fatalf("secret id set/masked wrong: %+v", got)
	}
}

func TestLogStorageConfig(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek)}

	// Get initial defaults — spec shape: backend, local.path, readOnly stats.
	initial, err := GetLogStorageConfig(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Backend != "local" || initial.Local == nil || initial.Local.Path == "" {
		t.Fatalf("unexpected defaults: %+v", initial)
	}
	if initial.Stats == nil {
		t.Fatal("stats missing from GET (readOnly field the SPA renders)")
	}

	// Round-trip a custom local path + S3 connection fields (stored for later;
	// the s3 *backend* itself is gated).
	updated, err := UpdateLogStorageConfig(ctx, pool, cfg, LogStorageConfig{
		Backend: "local",
		Local:   &LocalLogConfig{Path: "/var/lib/cronomicon/custom-logs"},
		S3: &S3LogConfig{
			Bucket: "my-bucket", Endpoint: "http://s3.example.com", Region: "us-east-1",
			AccessKey: "key", SecretKey: "secret", Prefix: "cronomicon/",
		},
	}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Local.Path != "/var/lib/cronomicon/custom-logs" {
		t.Fatalf("local.path not persisted: %+v", updated.Local)
	}
	if updated.S3 == nil || updated.S3.AccessKey != "key" || updated.S3.Prefix != "cronomicon/" {
		t.Fatalf("s3 fields not persisted: %+v", updated.S3)
	}
	if updated.S3.SecretKey != "" {
		t.Fatal("s3.secretKey is write-only and must never be returned")
	}

	// backend=s3 with no connection block is a validation error (SL-1 replaced
	// the blanket "not yet supported" gate with real validation + a bucket probe;
	// see log_storage_test.go for the matrix).
	_, err = UpdateLogStorageConfig(ctx, pool, cfg, LogStorageConfig{Backend: "s3"}, "tester")
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError for backend=s3 without s3 block, got: %v", err)
	}
	if ve.Field != "s3" {
		t.Fatalf("unexpected field: %v (%s)", ve.Field, ve.Message)
	}

	// Bad enum and relative path are 422-class validation errors too.
	if _, err = UpdateLogStorageConfig(ctx, pool, cfg, LogStorageConfig{Backend: "tape"}, "tester"); !errors.As(err, &ve) {
		t.Fatalf("want ValidationError for bad backend, got: %v", err)
	}
	if _, err = UpdateLogStorageConfig(ctx, pool, cfg, LogStorageConfig{
		Backend: "local", Local: &LocalLogConfig{Path: "relative/path"},
	}, "tester"); !errors.As(err, &ve) {
		t.Fatalf("want ValidationError for relative path, got: %v", err)
	}
}

func TestObservabilityConfig(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	cfg := &config.Config{SecretKEKEnv: base64.StdEncoding.EncodeToString(kek)}

	// Default config check
	initial, err := GetObservabilityConfig(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !initial.Enabled || initial.Path != "/metrics" || initial.AuthType != "none" {
		t.Fatalf("unexpected defaults: %+v", initial)
	}

	// Update observability config
	_, err = UpdateObservabilityConfig(ctx, pool, cfg, ObservabilityConfig{
		Enabled:     true,
		Path:        "/prometheus-metrics",
		AuthType:    "bearer",
		BearerToken: "my-bearer-token",
		Metrics:     map[string]bool{"http_requests_total": true},
	}, "tester")
	if err != nil {
		t.Fatal(err)
	}

	got, err := GetObservabilityConfig(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/prometheus-metrics" || got.AuthType != "bearer" || !got.Metrics["http_requests_total"] {
		t.Fatalf("unexpected values: %+v", got)
	}
	if !got.BearerTokenSet || got.BearerToken != "••••oken" {
		t.Fatalf("bearer token mask wrong: %+v", got)
	}
}

func TestGitOpsScopeSyncAndList(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	// 1. Insert gitlab config to resolve GitLabURL.
	_, err := pool.ExecContext(ctx, `
		INSERT INTO gitlab_config (id, repo_url, write_branch, webhook_secret, last_modified_by, last_modified_at)
		VALUES (1, ?, ?, ?, ?, ?)`,
		"https://gitlab.example.com/org/repo.git", "main", "secret", "tester", "2026-06-12T16:11:17Z")
	if err != nil {
		t.Fatalf("failed to insert gitlab config: %v", err)
	}

	// 2. Insert a local scope.
	descLocal := "Local environment"
	localScope, err := CreateScope(ctx, pool, LocalScopeInput{
		Scope:          "local-env",
		Description:    &descLocal,
		Hosts:          []string{"127.0.0.1"},
		SupportedTypes: []string{"bash"},
	}, "alice@example.com")
	if err != nil {
		t.Fatalf("failed to create local scope: %v", err)
	}

	// 3. Insert a git-source scope (simulating gitlab sync).
	gitScopeID := db.NewID()
	capJSON := `{"types":["bash","ansible"],"origin":"git","owner":"plat-eng","sidecarPath":"inventory/dev.cronomicon.yaml","errors":[{"file":"inventory/dev.cronomicon.yaml","line":5,"field":"owner","message":"owner not found"}]}`
	_, err = pool.ExecContext(ctx, `
		INSERT INTO scopes (id, name, source, description, supported_types, created_by, created_at, last_modified_by, last_modified_at, source_path, capability_types, capability_json, synced_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		gitScopeID, "dev-env", "git", "Dev inventory", `["bash","ansible"]`, "gitlab", "2026-06-12T16:00:00Z", "gitlab", "2026-06-12T16:00:00Z", "inventory/dev-env.ini", `["bash","ansible"]`, capJSON, "2026-06-12T16:05:00Z")
	if err != nil {
		t.Fatalf("failed to insert git scope: %v", err)
	}

	// Insert hosts for git scope.
	_, err = pool.ExecContext(ctx, `INSERT INTO scope_hosts (scope_id, host) VALUES (?, ?)`, gitScopeID, "dev-host.example.com")
	if err != nil {
		t.Fatalf("failed to insert git scope hosts: %v", err)
	}

	// 4. Test ListScopes (unified, unfiltered).
	allList, err := ListScopes(ctx, pool, "")
	if err != nil {
		t.Fatalf("ListScopes failed: %v", err)
	}
	if len(allList) != 2 {
		t.Fatalf("expected 2 scopes, got %d", len(allList))
	}

	// Check sorting and details.
	// source DESC, name: "local-env" is "cronomicon" so it comes before "git", wait.
	// Wait, "local-env" source is "cronomicon", "dev-env" source is "git".
	// 'local' vs 'git'. If sorting is source DESC, "local" (starts with l) comes before "git" (starts with g). Wait, no: "local" > "git" alphabetically so DESC puts "local" first.
	var gitScope, cronomiconScope Scope
	for _, sc := range allList {
		if sc.Source == "git" {
			gitScope = sc
		} else if sc.Source == "cronomicon" {
			cronomiconScope = sc
		}
	}

	if cronomiconScope.ID != localScope.ID {
		t.Errorf("cronomicon scope ID mismatch")
	}

	if gitScope.ID != gitScopeID {
		t.Errorf("git scope ID mismatch: want %q, got %q", gitScopeID, gitScope.ID)
	}
	if gitScope.Scope != "dev-env" {
		t.Errorf("git scope Name mismatch: want %q, got %q", "dev-env", gitScope.Scope)
	}
	if gitScope.Description == nil || *gitScope.Description != "Dev inventory" {
		t.Errorf("git scope description mismatch: %+v", gitScope.Description)
	}
	if gitScope.Name == nil || *gitScope.Name != "dev-env.ini" {
		t.Errorf("git scope Name pointer mismatch: %+v", gitScope.Name)
	}
	if gitScope.GitLabURL == nil || *gitScope.GitLabURL != "https://gitlab.example.com/org/repo/-/blob/main/inventory/dev-env.ini" {
		t.Errorf("git scope GitLabURL mismatch: %+v", gitScope.GitLabURL)
	}
	if gitScope.SidecarPath == nil || *gitScope.SidecarPath != "inventory/dev.cronomicon.yaml" {
		t.Errorf("git scope SidecarPath mismatch: %+v", gitScope.SidecarPath)
	}
	if len(gitScope.Hosts) != 1 || gitScope.Hosts[0] != "dev-host.example.com" {
		t.Errorf("git scope hosts mismatch: %v", gitScope.Hosts)
	}
	if gitScope.Capability.Origin != "git" {
		t.Errorf("git scope capability origin mismatch: %q", gitScope.Capability.Origin)
	}
	if gitScope.Capability.Owner == nil || *gitScope.Capability.Owner != "plat-eng" {
		t.Errorf("git scope capability owner mismatch: %+v", gitScope.Capability.Owner)
	}
	if len(gitScope.Capability.Errors) != 1 {
		t.Errorf("git scope capability errors count mismatch: %d", len(gitScope.Capability.Errors))
	} else {
		le := gitScope.Capability.Errors[0]
		if le.File != "inventory/dev.cronomicon.yaml" || le.Line != 5 || le.Field != "owner" || le.Message != "owner not found" {
			t.Errorf("git scope capability line error mismatch: %+v", le)
		}
	}
	if gitScope.LastChangedAt == nil || *gitScope.LastChangedAt != "2026-06-12T16:05:00Z" {
		t.Errorf("git scope LastChangedAt mismatch: %+v", gitScope.LastChangedAt)
	}

	// 5. Test ListScopes (filtered by git).
	gitOnly, err := ListScopes(ctx, pool, "git")
	if err != nil {
		t.Fatalf("ListScopes(git) failed: %v", err)
	}
	if len(gitOnly) != 1 || gitOnly[0].ID != gitScopeID {
		t.Fatalf("expected 1 git scope, got %d", len(gitOnly))
	}

	// 6. Test ListScopes (filtered by cronomicon).
	localOnly, err := ListScopes(ctx, pool, "cronomicon")
	if err != nil {
		t.Fatalf("ListScopes(cronomicon) failed: %v", err)
	}
	if len(localOnly) != 1 || localOnly[0].ID != localScope.ID {
		t.Fatalf("expected 1 cronomicon scope, got %d", len(localOnly))
	}

	// 7. Test GetScope.
	gotGit, err := GetScope(ctx, pool, gitScopeID)
	if err != nil {
		t.Fatalf("GetScope failed: %v", err)
	}
	if gotGit == nil || gotGit.ID != gitScopeID {
		t.Fatalf("expected git scope, got %+v", gotGit)
	}
	if gotGit.GitLabURL == nil || *gotGit.GitLabURL != "https://gitlab.example.com/org/repo/-/blob/main/inventory/dev-env.ini" {
		t.Errorf("GetScope GitLabURL mismatch: %+v", gotGit.GitLabURL)
	}
}

// ── Log-storage stats classifier (LU-11) ──────────────────────────────────────
//
// Why these tests matter. The Settings panel's job is to answer "what is filling
// my disk", and before LU-11 it could not: the walk counted every file it found
// as a run log, so the process log, its rotated generations and the per-folder
// _meta.json sidecars were all folded into a number labelled "log files". Two
// concrete misreadings followed, and both are the kind an operator acts on:
//
//   - FileCount read as "roughly how many runs are on disk" was inflated by
//     files that have nothing to do with runs.
//   - OldestLogAt was pinned to whichever file was oldest — in practice
//     cronomicon.log, which is created once at first boot and appended to forever.
//     So the "oldest log" never moved, exactly when an operator was trying to
//     judge whether retention was working.
//
// The fixtures below therefore make the PROCESS log the oldest file on purpose:
// that is the bug, and a classifier that regresses to counting everything will
// report that timestamp again.

// logTreeFixture builds a log directory containing one of each class the
// classifier must distinguish, with controlled sizes and mtimes, and returns the
// directory plus the total byte count of everything in it.
func logTreeFixture(t *testing.T) (dir string, totalBytes int64) {
	t.Helper()
	dir = t.TempDir()
	const code = "a3f2c1d0"
	if err := os.MkdirAll(filepath.Join(dir, code), 0o750); err != nil {
		t.Fatalf("mkdir entity folder: %v", err)
	}

	// Distinct sizes so a misattributed file shows up in the per-class byte
	// totals, not just the counts.
	files := []struct {
		rel  string
		size int
		age  time.Duration
	}{
		{filepath.Join(code, "trace-foldered.log"), 100, 2 * time.Hour}, // run log (foldered)
		{"trace-flat.log", 200, 3 * time.Hour},                          // run log (flat, pre-710)
		{"cronomicon.log", 400, 100 * time.Hour},                        // process log — OLDEST FILE IN THE TREE
		{"cronomicon.log.1", 800, 90 * time.Hour},                       // rotated process log
		{"audit.log", 1600, 4 * time.Hour},                              // audit stream
		{filepath.Join(code, "_meta.json"), 3200, 5 * time.Hour},        // folder sidecar
	}
	now := time.Now()
	for _, f := range files {
		p := filepath.Join(dir, f.rel)
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), f.size), 0o640); err != nil {
			t.Fatalf("write %s: %v", f.rel, err)
		}
		mt := now.Add(-f.age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatalf("chtimes %s: %v", f.rel, err)
		}
		totalBytes += int64(f.size)
	}
	return dir, totalBytes
}

// TestLogStatsCountOnlyRunLogsButSizeEverything pins the split that makes the
// panel readable: FileCount answers "how many runs", TotalSizeBytes answers "how
// much disk". Conflating them is what the old walk did.
func TestLogStatsCountOnlyRunLogsButSizeEverything(t *testing.T) {
	dir, total := logTreeFixture(t)
	stats := computeLogStats(dir)

	if stats.FileCount != 2 {
		t.Errorf("FileCount = %d, want 2 (the two run logs only) — the process log, its rotation and the sidecar are being counted as runs", stats.FileCount)
	}
	if stats.TotalSizeBytes != total {
		t.Errorf("TotalSizeBytes = %d, want %d (every file under the tree) — the disk-usage answer must span all classes", stats.TotalSizeBytes, total)
	}
}

// TestLogStatsOldestIgnoresTheProcessLog is the LU-11 bug itself. cronomicon.log is
// the oldest file in the fixture by a wide margin and is rewritten continuously,
// so letting it set OldestLogAt makes the value permanently stale — the operator
// sees an ancient timestamp no matter how aggressively retention runs.
func TestLogStatsOldestIgnoresTheProcessLog(t *testing.T) {
	dir, _ := logTreeFixture(t)
	stats := computeLogStats(dir)

	if stats.OldestLogAt == nil {
		t.Fatal("OldestLogAt is nil though two run logs exist")
	}
	got, err := time.Parse(time.RFC3339, *stats.OldestLogAt)
	if err != nil {
		t.Fatalf("OldestLogAt %q is not RFC3339: %v", *stats.OldestLogAt, err)
	}
	// The oldest RUN log is the flat one at -3h; the process log sits at -100h.
	flatInfo, err := os.Stat(filepath.Join(dir, "trace-flat.log"))
	if err != nil {
		t.Fatalf("stat flat run log: %v", err)
	}
	if diff := got.Sub(flatInfo.ModTime().UTC()); diff > time.Second || diff < -time.Second {
		procInfo, _ := os.Stat(filepath.Join(dir, "cronomicon.log"))
		t.Errorf("OldestLogAt = %v, want the oldest RUN log %v (the process log at %v must not set it)",
			got, flatInfo.ModTime().UTC(), procInfo.ModTime().UTC())
	}
}

// TestLogStatsClassifyEachFileKind checks every bucket individually, because the
// classifier is a switch and a single mis-ordered case silently moves a whole
// class. The sidecar landing in `other` is asserted explicitly: it is neither a
// run log (the reaper must not delete it) nor process output, and reporting it
// rather than dropping it is what keeps the classes honest.
func TestLogStatsClassifyEachFileKind(t *testing.T) {
	dir, total := logTreeFixture(t)
	stats := computeLogStats(dir)

	checks := []struct {
		name  string
		got   LogClassStat
		count int
		size  int64
	}{
		{"runLogs", stats.Classes.RunLogs, 2, 100 + 200},
		{"processLog", stats.Classes.ProcessLog, 2, 400 + 800}, // cronomicon.log + cronomicon.log.1
		{"auditLog", stats.Classes.AuditLog, 1, 1600},
		{"other", stats.Classes.Other, 1, 3200}, // the _meta.json sidecar
	}
	for _, c := range checks {
		if c.got.FileCount != c.count {
			t.Errorf("classes.%s.fileCount = %d, want %d", c.name, c.got.FileCount, c.count)
		}
		if c.got.TotalSizeBytes != c.size {
			t.Errorf("classes.%s.totalSizeBytes = %d, want %d", c.name, c.got.TotalSizeBytes, c.size)
		}
	}
	// The run-log class and the headline FileCount must be the same number, or the
	// panel contradicts itself.
	if stats.Classes.RunLogs.FileCount != stats.FileCount {
		t.Errorf("classes.runLogs.fileCount = %d but FileCount = %d", stats.Classes.RunLogs.FileCount, stats.FileCount)
	}
	// And nothing is dropped: a silently discarded remainder would make the
	// breakdown disagree with the total for no visible reason.
	sum := stats.Classes.RunLogs.TotalSizeBytes + stats.Classes.ProcessLog.TotalSizeBytes +
		stats.Classes.AuditLog.TotalSizeBytes + stats.Classes.Other.TotalSizeBytes
	if sum != total || sum != stats.TotalSizeBytes {
		t.Errorf("class sizes sum to %d, want %d (= TotalSizeBytes %d) — some file is unaccounted for",
			sum, total, stats.TotalSizeBytes)
	}
	sumCount := stats.Classes.RunLogs.FileCount + stats.Classes.ProcessLog.FileCount +
		stats.Classes.AuditLog.FileCount + stats.Classes.Other.FileCount
	if sumCount != 6 {
		t.Errorf("class counts sum to %d, want 6 (every file in the fixture)", sumCount)
	}
}

// TestLogStatsOnAnEmptyOrMissingTree pins the zero case: the log directory is
// created lazily on the first run, so a fresh install must render zeros rather
// than an error or a nil-deref in the panel.
func TestLogStatsOnAnEmptyOrMissingTree(t *testing.T) {
	for _, dir := range []string{t.TempDir(), filepath.Join(t.TempDir(), "does-not-exist")} {
		stats := computeLogStats(dir)
		if stats == nil {
			t.Fatalf("computeLogStats(%q) = nil", dir)
		}
		if stats.FileCount != 0 || stats.TotalSizeBytes != 0 || stats.OldestLogAt != nil {
			t.Errorf("computeLogStats(%q) = %+v, want zero stats", dir, stats)
		}
		if stats.Classes != (LogClasses{}) {
			t.Errorf("computeLogStats(%q) classes = %+v, want zero", dir, stats.Classes)
		}
	}
}

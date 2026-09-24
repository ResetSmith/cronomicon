package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/envref"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/notify"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"github.com/ResetSmith/cronomicon/internal/sshexec"
	"github.com/ResetSmith/cronomicon/internal/sshkeys"
	"github.com/ResetSmith/cronomicon/internal/tagutil"
)

// mountSettings is owned by B6 (config + settings APIs + secrets).
// All operator routes: s.auth.RequireSession + RequireCSRF on state-changing.
func (s *Server) mountSettings(mux *http.ServeMux) {
	// SL-1: seed the archive store from the stored log-storage settings, the way
	// mountRunners seeds the log dir. Pushed to every consumer on save thereafter.
	s.applyLogArchive(context.Background())

	sec := secrets.New(s.db, s.cfg, s.log)
	keys := sshkeys.New(s.db, s.cfg, s.log)

	// Vault (C.3): wire the real AppRole/KV-v2 client only when configured —
	// env first, DB-backed settings otherwise (E.3/E.5); resolved once at
	// startup, so settings changes take effect on restart. Unconfigured keeps
	// the stub and vault-source secrets report "unavailable" exactly as before
	// (decision #8 — built, disabled by default).
	settings.WireVaultClient(context.Background(), s.db, s.cfg, sec, s.log)

	// ── Server capabilities (feature gating for the SPA) ───────────────────────
	mux.Handle("GET /api/v1/capabilities", s.auth.RequireSession(http.HandlerFunc(s.handleCapabilities)))

	// ── Env Vars (writes: ManageEnvVars, PP-B1) ─────────────────────────────────
	mux.Handle("GET /api/v1/env-vars", s.auth.RequireSession(http.HandlerFunc(s.handleListEnvVars)))
	mux.Handle("POST /api/v1/env-vars", s.requirePerm("manageEnvVars", permManageEnvVars)(http.HandlerFunc(s.handleCreateEnvVar)))
	mux.Handle("PUT /api/v1/env-vars/{envVarId}", s.requirePerm("manageEnvVars", permManageEnvVars)(http.HandlerFunc(s.handleUpdateEnvVar)))
	mux.Handle("DELETE /api/v1/env-vars/{envVarId}", s.requirePerm("manageEnvVars", permManageEnvVars)(http.HandlerFunc(s.handleDeleteEnvVar)))
	// Tags (migration 470): operator-authored, SQLite-only labels on a variable —
	// same ManageEnvVars gate as the other env-var writes (settings_tags_mount.go).
	mux.Handle("PUT /api/v1/env-var-tags/{envVarId}", s.requirePerm("manageEnvVars", permManageEnvVars)(http.HandlerFunc(s.updateEnvVarTags)))

	// ── Secrets ───────────────────────────────────────────────────────────────
	// P1.7/D3: secret metadata (keys, scopes, vault refs) is scope-filtered so a
	// scope-restricted session never sees out-of-scope secrets. It stays
	// RequireSession (NOT ManageEnvVars): non-manager operators legitimately read
	// secret-KEY metadata — the Job Composer and Scripts views cross-reference
	// declared references against defined secret keys. Values stay protected (reveal
	// needs ManageEnvVars + scope). A manager-only keys/metadata split is a P1.8
	// follow-up if in-scope key-name visibility must narrow further.
	mux.Handle("GET /api/v1/env-secrets", s.auth.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// L4: fail CLOSED on a missing identity. RequireSession should guarantee one;
		// the read path must not fail open if it doesn't. (Under the A5 fix a zero
		// Identity's grant reads as GLOBAL-only, not unrestricted — but the explicit
		// 401 keeps the contract crisp.)
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		list, err := sec.List(r.Context(), id.Grant().CanRead)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if list == nil {
			list = []secrets.Secret{}
		}
		if tagFilter := tagutil.ParseQuery(r.URL.Query()); !tagFilter.Empty() {
			kept := make([]secrets.Secret, 0, len(list))
			for _, sc := range list {
				if tagFilter.Match(sc.Tags) {
					kept = append(kept, sc)
				}
			}
			list = kept
		}
		httpx.JSON(w, http.StatusOK, list)
	})))
	mux.Handle("POST /api/v1/env-secrets", s.requirePerm("manageEnvVars", permManageEnvVars)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		var inp struct {
			Key         string   `json:"key"`
			Source      string   `json:"source"`
			Scope       *string  `json:"scope"`
			Description *string  `json:"description"`
			Value       string   `json:"value"`
			VaultPath   string   `json:"vaultPath"`
			AgencyIDs   []string `json:"agencyIds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
			httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
			return
		}
		if inp.Key == "" || inp.Source == "" {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "key and source required")
			return
		}
		// D3: a scope-restricted manager may not create a secret outside their grants,
		// NOR in the global scope (a global secret injects into every scope). 403 (not
		// 404) — the actor supplied the scope, so there is no existence to leak.
		if !auth.ScopeWritable(id, scopeStrOf(inp.Scope)) {
			s.denyScope(w, r, scopeStrOf(inp.Scope), "cannot create a secret in a scope outside your access")
			return
		}
		// RF-Q2(a): a restricted creator names at least one held agency, so the new
		// secret is born owned — an unmembered secret is RB-Q14 unrestricted-only,
		// which would lock the creator out of the row they just made. RA-9: when they
		// name none and hold the verb on exactly one department, it is inherited
		// rather than refused, so the safe outcome is the default.
		agencyIDs, ok := s.requireCreationAgencies(w, r, id, auth.PermManageEnvVars, inp.AgencyIDs, "secret")
		if !ok {
			return
		}
		sc, err := sec.Create(r.Context(), secrets.CreateInput{
			Key: inp.Key, Source: inp.Source, Scope: inp.Scope,
			Description: inp.Description, Value: inp.Value, VaultPath: inp.VaultPath,
			// RA-15: born owned by the creator's department when there is exactly one,
			// which is the same set RA-9 inherited. Set at INSERT so the row never
			// exists in a state that could collide with a shared row of the same key.
			OwnerAgency: ownerAgencyFor(agencyIDs),
		}, id.Email)
		if err != nil {
			if refErr, ok := errors.AsType[*envref.Error](err); ok {
				httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", refErr.Error())
				return
			}
			if errors.Is(err, secrets.ErrKeyConflict) {
				httpx.Fail(w, http.StatusConflict, "conflict", "an env var already uses this key in this scope (A13)")
				return
			}
			if strings.Contains(err.Error(), "UNIQUE") {
				httpx.Fail(w, http.StatusConflict, "conflict", "key already exists for this scope")
				return
			}
			httpx.Fail500(w, s.log, "create_failed", err)
			return
		}
		if len(agencyIDs) > 0 {
			// Membership rides the create (RF-Q2(a)); on failure the secret is rolled
			// back — see the SSH-key site for why an orphaned unmembered row is the
			// UNSAFE state rather than the safe one. agencyIDs is the EFFECTIVE set:
			// what the caller sent, or what RA-9 inherited on their behalf.
			if err := settings.SetAgencyMembership(r.Context(), s.db, settings.MemberSecret,
				[]settings.AgencyMembership{{ID: sc.ID, AgencyIDs: agencyIDs}}, id.Email); err != nil {
				if _, derr := sec.Delete(r.Context(), sc.ID); derr != nil {
					s.log.Error("orphaned secret: membership failed and rollback failed",
						"secretId", sc.ID, "membershipError", err, "rollbackError", derr)
				}
				httpx.Fail500(w, s.log, "membership_failed", err)
				return
			}
		}
		s.mountWriteChangeLog(r, "Secrets", "created", inp.Key, id.Email)
		httpx.JSON(w, http.StatusCreated, sc)
	})))
	mux.Handle("PUT /api/v1/env-secrets/{secretId}", s.requirePerm("manageEnvVars", permManageEnvVars)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		sid := r.PathValue("secretId")
		// RB-32: manageEnvVars is DEPARTMENTAL now (RB-Q2). The route gate above
		// proves the caller holds the verb somewhere; this proves they hold it on the
		// agency that owns THIS secret. Secret material is the highest-consequence
		// case — reveal is gated on manageEnvVars, so a global verb meant anyone
		// trusted with any department's variables could read every department's
		// credentials.
		if !s.requireEntityAgency(w, r, id, auth.PermManageEnvVars, "secret_agencies", "secret_id", sid, "secret") {
			return
		}
		var inp struct {
			Key         string  `json:"key"`
			Source      string  `json:"source"`
			Scope       *string `json:"scope"`
			Description *string `json:"description"`
			Value       string  `json:"value"`
			VaultPath   string  `json:"vaultPath"`
		}
		if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
			httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
			return
		}
		// D3/H3: the actor must be able to WRITE the EXISTING secret (404 out-of-scope,
		// 403 on a global row a restricted actor may not rewrite/capture) AND may not
		// move it into a scope outside their grants (403 on the target).
		if _, ok := s.loadSecretWritable(w, r, sec, sid, id); !ok {
			return
		}
		// The target scope must be writable too (blocks moving it out of reach OR
		// widening a scoped secret to global). A restricted PUT that omits scope would
		// globalize the row — ScopeWritable("") rejects that for a restricted actor.
		if !auth.ScopeWritable(id, scopeStrOf(inp.Scope)) {
			s.denyScope(w, r, scopeStrOf(inp.Scope), "cannot move a secret into a scope outside your access")
			return
		}
		sc, err := sec.Update(r.Context(), sid, secrets.UpdateInput{
			Key: inp.Key, Source: inp.Source, Scope: inp.Scope,
			Description: inp.Description, Value: inp.Value, VaultPath: inp.VaultPath,
		}, id.Email)
		if err != nil {
			if refErr, ok := errors.AsType[*envref.Error](err); ok {
				httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", refErr.Error())
				return
			}
			if errors.Is(err, secrets.ErrKeyConflict) {
				httpx.Fail(w, http.StatusConflict, "conflict", "an env var already uses this key in this scope (A13)")
				return
			}
			httpx.Fail500(w, s.log, "update_failed", err)
			return
		}
		if sc == nil {
			httpx.Fail(w, http.StatusNotFound, "not_found", "secret not found")
			return
		}
		s.mountWriteChangeLog(r, "Secrets", "updated", inp.Key, id.Email)
		httpx.JSON(w, http.StatusOK, sc)
	})))
	mux.Handle("DELETE /api/v1/env-secrets/{secretId}", s.requirePerm("manageEnvVars", permManageEnvVars)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		sid := r.PathValue("secretId")
		// D3/H3: enforce write-scope before deleting (404 out-of-scope; 403 on a global
		// row — a restricted manager must not delete a secret every scope binds).
		if _, ok := s.loadSecretWritable(w, r, sec, sid, id); !ok {
			return
		}
		// RB-32: and the agency that owns it (RB-Q2/RB-Q14).
		if !s.requireEntityAgency(w, r, id, auth.PermManageEnvVars, "secret_agencies", "secret_id", sid, "secret") {
			return
		}
		found, err := sec.Delete(r.Context(), sid)
		if err != nil {
			httpx.Fail500(w, s.log, "delete_failed", err)
			return
		}
		if !found {
			httpx.Fail(w, http.StatusNotFound, "not_found", "secret not found")
			return
		}
		s.mountWriteChangeLog(r, "Secrets", "deleted", sid, id.Email)
		w.WriteHeader(http.StatusNoContent)
	})))
	mux.Handle("POST /api/v1/env-secrets/{secretId}/reveal", s.requirePerm("manageEnvVars", permManageEnvVars)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		sid := r.PathValue("secretId")
		sc, ok := s.loadSecretInScope(w, r, sec, sid, id)
		if !ok {
			return
		}
		// RB-32: manageEnvVars is DEPARTMENTAL now (RB-Q2). The route gate above
		// proves the caller holds the verb somewhere; this proves they hold it on the
		// agency that owns THIS secret. Secret material is the highest-consequence
		// case — reveal is gated on manageEnvVars, so a global verb meant anyone
		// trusted with any department's variables could read every department's
		// credentials.
		if !s.requireEntityAgency(w, r, id, auth.PermManageEnvVars, "secret_agencies", "secret_id", sid, "secret") {
			return
		}
		if sc.Source == "vault" {
			httpx.Fail(w, http.StatusConflict, "vault_source", "vault-source secrets cannot be revealed through Cronomicon")
			return
		}
		value, err := sec.Reveal(r.Context(), sid)
		if err != nil {
			// L3: the row was deleted between loadSecretInScope and Reveal (TOCTOU) —
			// 404, not a 500 (and never the old 200 {"value":""}).
			if errors.Is(err, secrets.ErrSecretNotFound) {
				httpx.Fail(w, http.StatusNotFound, "not_found", "secret not found")
				return
			}
			httpx.Fail500(w, s.log, "reveal_failed", err)
			return
		}
		// S1 / PP-B4b: the reveal MUST be audited. Write the change_log row BEFORE
		// returning the plaintext and fail closed (500, no value) if the audit
		// write errors — never hand back a secret with no audit trail.
		if auditErr := settings.WriteChangeLog(r.Context(), s.db, id.Email, "Secrets", "revealed", sc.Key, ""); auditErr != nil {
			s.log.Error("secret reveal audit write failed — withholding value", "key", sc.Key, "error", auditErr)
			httpx.Fail(w, http.StatusInternalServerError, "audit_failed", "could not record the reveal; value withheld")
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"value": value})
	})))
	mux.Handle("POST /api/v1/env-secrets/{secretId}/migrate-to-vault", s.requirePerm("manageEnvVars", permManageEnvVars)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		sid := r.PathValue("secretId")
		var inp struct {
			VaultPath string `json:"vaultPath"`
		}
		if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
			httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
			return
		}
		// D3/H3: enforce write-scope before migrating (404 out-of-scope; 403 on a global
		// row — re-scoping/capturing a global secret's storage is a restricted-actor no).
		if _, ok := s.loadSecretWritable(w, r, sec, sid, id); !ok {
			return
		}
		// RB-32 (RF-5): migrating rewrites where the secret's MATERIAL lives — a
		// write in every sense — so it takes the same departmental gate as PUT.
		if !s.requireEntityAgency(w, r, id, auth.PermManageEnvVars, "secret_agencies", "secret_id", sid, "secret") {
			return
		}
		sc, err := sec.MigrateToVault(r.Context(), sid, inp.VaultPath, id.Email)
		if err != nil {
			if strings.Contains(err.Error(), "already vault-source") {
				httpx.Fail(w, http.StatusConflict, "already_vault", err.Error())
				return
			}
			if strings.Contains(err.Error(), "vault unavailable") {
				httpx.Fail(w, http.StatusServiceUnavailable, "vault_unreachable", err.Error())
				return
			}
			httpx.Fail500(w, s.log, "migrate_failed", err)
			return
		}
		if sc == nil {
			httpx.Fail(w, http.StatusNotFound, "not_found", "secret not found")
			return
		}
		s.mountWriteChangeLog(r, "Secrets", "migrated-to-vault", sc.Key, id.Email)
		httpx.JSON(w, http.StatusOK, sc)
	})))
	// Tags (migration 470): operator-authored, SQLite-only labels on a secret —
	// plaintext metadata, never the encrypted envelope; same ManageEnvVars gate.
	mux.Handle("PUT /api/v1/env-secret-tags/{secretId}", s.requirePerm("manageEnvVars", permManageEnvVars)(http.HandlerFunc(s.updateSecretTags)))

	// ── Scopes (writes: ConfigureApp, PP-B1; GET stays session — scope lists
	// feed compose/job dropdowns for all operators) ─────────────────────────────
	mux.Handle("GET /api/v1/scopes", s.auth.RequireSession(http.HandlerFunc(s.handleListScopes)))
	mux.Handle("POST /api/v1/scopes", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleCreateScope)))
	mux.Handle("GET /api/v1/scopes/{scopeId}", s.auth.RequireSession(http.HandlerFunc(s.handleGetScope)))
	// Inventory detail (raw + parsed host/group vars) is ConfigureApp-gated, not
	// plain session: a git inventory can carry sensitive non-connection vars under
	// arbitrary keys (outside the narrow secret reject-set), so the read surface is
	// kept no broader than the privilege that guards scope mutation / hides git raw.
	mux.Handle("GET /api/v1/scopes/{scopeId}/inventory", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleGetScopeInventory)))
	mux.Handle("PUT /api/v1/scopes/{scopeId}/inventory", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handlePutScopeInventory)))
	mux.Handle("POST /api/v1/scopes/{scopeId}/inventory/import-hosts", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleImportScopeHosts)))
	mux.Handle("PATCH /api/v1/scopes/{scopeId}", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleUpdateScope)))
	mux.Handle("DELETE /api/v1/scopes/{scopeId}", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleDeleteScope)))

	// ── Alerts (writes: ConfigureApp, PP-B1) ────────────────────────────────────
	mux.Handle("GET /api/v1/alerts", s.auth.RequireSession(http.HandlerFunc(s.handleListAlerts)))
	mux.Handle("POST /api/v1/alerts", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleCreateAlert)))
	mux.Handle("PUT /api/v1/alerts/{alertId}", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleUpdateAlert)))
	mux.Handle("DELETE /api/v1/alerts/{alertId}", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleDeleteAlert)))

	// ── SSH Hosts (writes + test: ConfigureApp, PP-B1) ──────────────────────────
	mux.Handle("GET /api/v1/ssh/hosts", s.auth.RequireSession(http.HandlerFunc(s.handleListSshHosts)))
	mux.Handle("POST /api/v1/ssh/hosts", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleCreateSshHost)))
	mux.Handle("PUT /api/v1/ssh/hosts/{hostId}", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleUpdateSshHost)))
	mux.Handle("DELETE /api/v1/ssh/hosts/{hostId}", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleDeleteSshHost)))
	mux.Handle("POST /api/v1/ssh/hosts/{hostId}/test", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleTestSshHost)))
	mux.Handle("DELETE /api/v1/ssh/hosts/{hostId}/host-key", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleClearSshHostKey)))

	// ── Bastions (writes + test: ConfigureApp, PP-B1) ───────────────────────────
	mux.Handle("GET /api/v1/ssh/bastions", s.auth.RequireSession(http.HandlerFunc(s.handleListBastions)))
	mux.Handle("POST /api/v1/ssh/bastions", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleCreateBastion)))
	mux.Handle("PUT /api/v1/ssh/bastions/{bastionId}", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleUpdateBastion)))
	mux.Handle("DELETE /api/v1/ssh/bastions/{bastionId}", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleDeleteBastion)))
	mux.Handle("POST /api/v1/ssh/bastions/{bastionId}/test", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleTestBastion)))
	mux.Handle("DELETE /api/v1/ssh/bastions/{bastionId}/host-key", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleClearBastionHostKey)))

	// ── SSH Key Credentials (writes: ConfigureApp, SK-D6; first-class system SSH
	// keys — ssh-keys-update.md SK.8). Private key material is never returned. ────
	mux.Handle("GET /api/v1/ssh/credentials", s.auth.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		list, err := keys.List(r.Context())
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if list == nil {
			list = []sshkeys.Credential{}
		}
		if tagFilter := tagutil.ParseQuery(r.URL.Query()); !tagFilter.Empty() {
			kept := make([]sshkeys.Credential, 0, len(list))
			for _, c := range list {
				if tagFilter.Match(c.Tags) {
					kept = append(kept, c)
				}
			}
			list = kept
		}
		httpx.JSON(w, http.StatusOK, list)
	})))
	mux.Handle("POST /api/v1/ssh/credentials", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		inp, agencyIDs, err := decodeCredentialInput(r)
		if err != nil {
			httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
			return
		}
		// RF-1/RF-Q2(a) (the RBAC-fixes plan): the entity gate that sat
		// here in v0.56.6 could only ever evaluate the empty-membership branch —
		// there is no {credentialId} on a create — so it demanded an unrestricted
		// actor for EVERY creation with a misleading denial. Creation is instead
		// governed by the RF-Q2(a) rule: a restricted creator names at least one
		// held agency, so the new key is born owned rather than born global —
		// inherited per RA-9 when they hold configureApp on exactly one department.
		agencyIDs, ok = s.requireCreationAgencies(w, r, id, auth.PermConfigureApp, agencyIDs, "SSH key")
		if !ok {
			return
		}
		inp.OwnerAgency = ownerAgencyFor(agencyIDs) // RA-19 — see the secret create site
		c, err := keys.Create(r.Context(), *inp, id.Email)
		if err != nil {
			if sshkeys.IsValidationError(err) {
				httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
				return
			}
			if strings.Contains(err.Error(), "UNIQUE") {
				// RA-19: uniqueness is per-OWNER now, so the message has to say where the
				// collision is. "already exists" would send a department hunting through a
				// catalogue for a label that is in fact their own department's.
				httpx.Fail(w, http.StatusConflict, "conflict",
					"an SSH credential with this label already exists in this department")
				return
			}
			httpx.Fail500(w, s.log, "create_failed", err)
			return
		}
		if len(agencyIDs) > 0 {
			// Membership rides the create (RF-Q2(a)), and a failure here must ROLL THE
			// ENTITY BACK rather than leave it behind.
			//
			// An orphaned unmembered row is NOT the safe state, despite being closed on
			// the write axis: AG-Q1(b) makes "no membership" mean "no agency
			// restriction", so runref's dispatch resolver treats it as shared
			// infrastructure and EVERY department's runs can consume it. Leaving one
			// behind would turn a transient DB fault into a cross-department secret
			// with no owner and no way for its creator to fix it (the retry collides
			// on UNIQUE(key, scope)).
			if err := settings.SetAgencyMembership(r.Context(), s.db, settings.MemberSSHCredential,
				[]settings.AgencyMembership{{ID: c.ID, AgencyIDs: agencyIDs}}, id.Email); err != nil {
				if _, derr := keys.Delete(r.Context(), c.ID, true); derr != nil {
					s.log.Error("orphaned SSH key: membership failed and rollback failed",
						"credentialId", c.ID, "membershipError", err, "rollbackError", derr)
				}
				httpx.Fail500(w, s.log, "membership_failed", err)
				return
			}
		}
		s.mountWriteChangeLog(r, "SSH Keys", "created", c.Label, id.Email)
		httpx.JSON(w, http.StatusCreated, c)
	})))
	mux.Handle("GET /api/v1/ssh/credentials/{credentialId}", s.auth.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := keys.Get(r.Context(), r.PathValue("credentialId"))
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if c == nil {
			httpx.Fail(w, http.StatusNotFound, "not_found", "ssh credential not found")
			return
		}
		httpx.JSON(w, http.StatusOK, c)
	})))
	mux.Handle("PUT /api/v1/ssh/credentials/{credentialId}", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		// RB-16 (fixed in RF-1): this is the route that REWRITES key material — the
		// exact "Tax admin edits Finance's credentials" hole the plan called the
		// highest-consequence gap. v0.56.6 put the gate on the create route (where
		// {credentialId} is always empty) and left this one open; the misplacement
		// shipped precisely because no test touched ssh_credential_agencies here.
		// An unmembered key is shared infrastructure and is unrestricted-only (RB-Q14).
		if !s.requireEntityAgency(w, r, id, auth.PermConfigureApp, "ssh_credential_agencies", "credential_id",
			r.PathValue("credentialId"), "SSH key") {
			return
		}
		inp, _, err := decodeCredentialInput(r)
		if err != nil {
			httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
			return
		}
		c, err := keys.Update(r.Context(), r.PathValue("credentialId"), *inp, id.Email)
		if err != nil {
			if sshkeys.IsValidationError(err) {
				httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
				return
			}
			httpx.Fail500(w, s.log, "update_failed", err)
			return
		}
		if c == nil {
			httpx.Fail(w, http.StatusNotFound, "not_found", "ssh credential not found")
			return
		}
		s.mountWriteChangeLog(r, "SSH Keys", "updated", c.Label, id.Email)
		httpx.JSON(w, http.StatusOK, c)
	})))
	mux.Handle("DELETE /api/v1/ssh/credentials/{credentialId}", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		// RB-16: SSH keys have NO scope column at all — isolation is pure agency
		// intersection (AG-Q5). Without this, a Tax admin could manage Finance's
		// credential MATERIAL, which the plan calls the highest-consequence gap in
		// the whole program. An unmembered key is shared infrastructure and is
		// unrestricted-only (RB-Q14).
		if !s.requireEntityAgency(w, r, id, auth.PermConfigureApp, "ssh_credential_agencies", "credential_id",
			r.PathValue("credentialId"), "SSH key") {
			return
		}
		cid := r.PathValue("credentialId")
		force := r.URL.Query().Get("force") == "true"
		found, err := keys.Delete(r.Context(), cid, force)
		if err != nil {
			if errors.Is(err, sshkeys.ErrCredentialInUse) {
				// 409 with the usage list so the operator can see what blocks it.
				usage, uerr := keys.Usage(r.Context(), cid)
				if uerr != nil {
					httpx.Fail500(w, s.log, "db_error", uerr)
					return
				}
				httpx.JSON(w, http.StatusConflict, usage)
				return
			}
			httpx.Fail500(w, s.log, "delete_failed", err)
			return
		}
		if !found {
			httpx.Fail(w, http.StatusNotFound, "not_found", "ssh credential not found")
			return
		}
		s.mountWriteChangeLog(r, "SSH Keys", "deleted", cid, id.Email)
		w.WriteHeader(http.StatusNoContent)
	})))
	mux.Handle("GET /api/v1/ssh/credentials/{credentialId}/usage", s.auth.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cid := r.PathValue("credentialId")
		c, err := keys.Get(r.Context(), cid)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if c == nil {
			httpx.Fail(w, http.StatusNotFound, "not_found", "ssh credential not found")
			return
		}
		usage, err := keys.Usage(r.Context(), cid)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		httpx.JSON(w, http.StatusOK, usage)
	})))
	// Tags (migration 470): operator-authored, SQLite-only labels on an SSH key
	// credential — plaintext metadata beside the sealed key; ConfigureApp gate to
	// match the other credential writes (settings_tags_mount.go).
	mux.Handle("PUT /api/v1/ssh-credential-tags/{credentialId}", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.updateCredentialTags)))

	// ── Settings (writes: ConfigureApp, PP-B1) ──────────────────────────────────
	// GET /settings/general stays session (the SPA TZ-mismatch banner reads
	// serverTimezone for all operators). GET gitlab/vault are gated (Q2 — they
	// expose connection config); other GETs stay session.
	mux.Handle("GET /api/v1/settings/general", s.auth.RequireSession(http.HandlerFunc(s.handleGetGeneralSettings)))
	mux.Handle("PUT /api/v1/settings/general", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleUpdateGeneralSettings)))
	mux.Handle("GET /api/v1/settings/notifications", s.auth.RequireSession(http.HandlerFunc(s.handleGetNotifications)))
	mux.Handle("PUT /api/v1/settings/notifications", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleUpdateNotifications)))
	// K-5: a test send. ConfigureApp because it delivers to real people using the
	// stored SMTP/Apprise config — the same permission that set that config.
	mux.Handle("POST /api/v1/settings/notifications/test", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleTestNotification)))
	mux.Handle("GET /api/v1/settings/audit-compliance", s.auth.RequireSession(http.HandlerFunc(s.handleGetAuditCompliance)))
	mux.Handle("PUT /api/v1/settings/audit-compliance", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleUpdateAuditCompliance)))
	mux.Handle("GET /api/v1/settings/gitlab", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleGetGitlabSettings)))
	mux.Handle("PUT /api/v1/settings/gitlab", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleUpdateGitlabSettings)))
	mux.Handle("POST /api/v1/settings/gitlab/webhook-secret/rotate", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleRotateGitlabWebhookSecret)))
	mux.Handle("GET /api/v1/settings/vault", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleGetVaultSettings)))
	mux.Handle("PUT /api/v1/settings/vault", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleUpdateVaultSettings)))
	mux.Handle("GET /api/v1/settings/log-storage", s.auth.RequireSession(http.HandlerFunc(s.handleGetLogStorageSettings)))
	mux.Handle("PUT /api/v1/settings/log-storage", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleUpdateLogStorageSettings)))
	// SL-2 / SL-Q8: Sync now is configureApp, the same permission that set the
	// bucket. 202 with the archive status; 409 while a tick runs; 422 on local.
	mux.Handle("POST /api/v1/settings/log-storage/sync", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleLogStorageSyncNow)))
	mux.Handle("GET /api/v1/settings/observability", s.auth.RequireSession(http.HandlerFunc(s.handleGetObservabilitySettings)))
	mux.Handle("PUT /api/v1/settings/observability", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleUpdateObservabilitySettings)))

	// ── Audit Export (ConfigureApp — full change/audit history, Q2/PP-B1) ────────
	mux.Handle("GET /api/v1/audit/export", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleAuditExport)))
}

// mountWriteChangeLog writes a change_log row for mutations audited at the API layer
// (secret reveals and vault migrations, which have no settings-package equivalent).
func (s *Server) mountWriteChangeLog(r *http.Request, category, action, target, actor string) {
	_ = settings.WriteChangeLog(r.Context(), s.db, actor, category, action, target, "")
}

// loadSecretInScope loads a secret by id and enforces the actor's scope grants
// (P1.7 / D3). On a DB error it writes 500; on missing OR out-of-scope it writes
// 404 and returns false — deliberately NOT 403, so a scope-restricted operator can
// never use this endpoint as an oracle for the existence of secrets they can't
// reach. Callers use the returned secret only after ok==true.
func (s *Server) loadSecretInScope(w http.ResponseWriter, r *http.Request, sec *secrets.Service, sid string, actor auth.Identity) (*secrets.Secret, bool) {
	sc, err := sec.Get(r.Context(), sid)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return nil, false
	}
	scopeStr := ""
	if sc != nil && sc.Scope != nil {
		scopeStr = *sc.Scope
	}
	if sc == nil || !auth.ScopeReadable(actor, scopeStr) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "secret not found")
		return nil, false
	}
	return sc, true
}

// loadSecretWritable loads a secret AND enforces that the actor may WRITE its
// EXISTING scope — the guard the delete/update/migrate/tag paths need on top of
// loadSecretInScope (H3). loadSecretInScope only checks the READ predicate
// (auth.ScopeReadable), which returns true for a GLOBAL (scope "") row, so a
// scope-restricted manager could otherwise delete, re-scope (capture), migrate, or
// label a global secret that injects into every scope. This adds
// auth.ScopeWritable(existing.Scope): a restricted actor is refused on a global
// row. Ordering matters — loadSecretInScope has already returned 404 for a
// genuinely out-of-scope non-global row (no existence oracle), so the only row
// that reaches the writability gate and fails it is a global one, for which 403 is
// correct (global existence is not scope-secret; it mirrors the create-side 403).
func (s *Server) loadSecretWritable(w http.ResponseWriter, r *http.Request, sec *secrets.Service, sid string, actor auth.Identity) (*secrets.Secret, bool) {
	sc, ok := s.loadSecretInScope(w, r, sec, sid, actor)
	if !ok {
		return nil, false
	}
	if !auth.ScopeWritable(actor, scopeStrOf(sc.Scope)) {
		s.denyScope(w, r, scopeStrOf(sc.Scope), "cannot modify a secret in a scope outside your access")
		return nil, false
	}
	return sc, true
}

// loadEnvVarWritable loads an env var and enforces that the actor may WRITE its
// EXISTING scope (M4) — the variable analogue of loadSecretWritable. Env-var
// writes were wholly unscoped, which the resolver's scope-exact-beats-global
// ordering turns into an INTEGRITY hole (a restricted manager POSTing
// {key:"DB_HOST", scope:"prod"} makes a prod run's CRONOMICON_VAR_DB_HOST resolve the
// attacker's value over the global row). 404 on a row out of read scope (no
// existence oracle), 403 on a global row a restricted actor may not rewrite.
func (s *Server) loadEnvVarWritable(w http.ResponseWriter, r *http.Request, evID string, actor auth.Identity) (*settings.EnvVar, bool) {
	ev, err := settings.GetEnvVar(r.Context(), s.db, evID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return nil, false
	}
	scopeStr := ""
	if ev != nil && ev.Scope != nil {
		scopeStr = *ev.Scope
	}
	if ev == nil || !auth.ScopeReadable(actor, scopeStr) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "env var not found")
		return nil, false
	}
	if !auth.ScopeWritable(actor, scopeStr) {
		s.denyScope(w, r, scopeStr, "cannot modify an env var in a scope outside your access")
		return nil, false
	}
	return ev, true
}

// scopeStrOf coalesces an optional scope pointer to the "" global convention.
func scopeStrOf(scope *string) string {
	if scope == nil {
		return ""
	}
	return *scope
}

// ── Env Var handlers ────────────────────────────────────────────────────────

func (s *Server) handleListEnvVars(w http.ResponseWriter, r *http.Request) {
	// L4: fail CLOSED on a missing identity — a zero Identity ⇒ nil AllowedScopes ⇒
	// the scope filter below reads it as unrestricted (leaks every scope's values).
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	list, err := settings.ListEnvVars(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	// D3 (P1.7): variables carry PLAINTEXT values, so a cross-scope leak here is a
	// value leak, not just metadata. Scope-filter to the actor's grants (global
	// always visible; empty grants ⇒ admin sees all). Endpoint stays RequireSession
	// — variables are lower-risk than secrets, whose LIST additionally needs
	// ManageEnvVars; management-scope enforcement on variable writes is a follow-up.
	tagFilter := tagutil.ParseQuery(r.URL.Query())
	filtered := make([]settings.EnvVar, 0, len(list))
	for _, ev := range list {
		if auth.ScopeReadable(id, scopeStrOf(ev.Scope)) && tagFilter.Match(ev.Tags) {
			filtered = append(filtered, ev)
		}
	}
	httpx.JSON(w, http.StatusOK, filtered)
}

func (s *Server) handleCreateEnvVar(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var inp struct {
		Key         string   `json:"key"`
		Value       string   `json:"value"`
		Scope       *string  `json:"scope"`
		Description *string  `json:"description"`
		AgencyIDs   []string `json:"agencyIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	if inp.Key == "" || inp.Value == "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "key and value required")
		return
	}
	// M4: a restricted manager may not create a variable outside their grants, NOR in
	// the global scope (a global var injects into every scope — same rationale as
	// secrets). 403 (not 404): the actor supplied the scope, so there is nothing to leak.
	if !auth.ScopeWritable(id, scopeStrOf(inp.Scope)) {
		s.denyScope(w, r, scopeStrOf(inp.Scope), "cannot create an env var in a scope outside your access")
		return
	}
	// RF-Q2(a): same creation rule as secrets — the new variable is born owned by
	// at least one of the restricted creator's agencies (RB-Q14 otherwise makes it
	// unrestricted-only the moment it exists), inherited per RA-9 when unambiguous.
	agencyIDs, ok := s.requireCreationAgencies(w, r, id, auth.PermManageEnvVars, inp.AgencyIDs, "variable")
	if !ok {
		return
	}
	ev, err := settings.CreateEnvVar(r.Context(), s.db, settings.EnvVarInput{
		Key: inp.Key, Value: inp.Value, Scope: inp.Scope, Description: inp.Description,
		OwnerAgency: ownerAgencyFor(agencyIDs), // RA-15 — see the secret create site
	}, id.Email)
	if err != nil {
		if refErr, ok := errors.AsType[*envref.Error](err); ok {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", refErr.Error())
			return
		}
		if errors.Is(err, settings.ErrKeyConflict) {
			httpx.Fail(w, http.StatusConflict, "conflict", "a secret already uses this key in this scope (A13)")
			return
		}
		if strings.Contains(err.Error(), "UNIQUE") {
			httpx.Fail(w, http.StatusConflict, "conflict", "key already exists for this scope")
			return
		}
		httpx.Fail500(w, s.log, "create_failed", err)
		return
	}
	if len(agencyIDs) > 0 {
		// Membership rides the create (RF-Q2(a)); on failure the variable is rolled
		// back — an orphaned unmembered row is consumable by every department's
		// runs (AG-Q1(b)), which is the opposite of a safe failure.
		if err := settings.SetAgencyMembership(r.Context(), s.db, settings.MemberEnvVar,
			[]settings.AgencyMembership{{ID: ev.ID, AgencyIDs: agencyIDs}}, id.Email); err != nil {
			if _, derr := settings.DeleteEnvVar(r.Context(), s.db, ev.ID, id.Email); derr != nil {
				s.log.Error("orphaned variable: membership failed and rollback failed",
					"envVarId", ev.ID, "membershipError", err, "rollbackError", derr)
			}
			httpx.Fail500(w, s.log, "membership_failed", err)
			return
		}
	}
	httpx.JSON(w, http.StatusCreated, ev)
}

func (s *Server) handleUpdateEnvVar(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	evID := r.PathValue("envVarId")
	var inp struct {
		Key         string  `json:"key"`
		Value       string  `json:"value"`
		Scope       *string `json:"scope"`
		Description *string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	// M4: the actor must be able to WRITE the EXISTING variable (404 out-of-scope,
	// 403 on a global row) AND may not move it into a scope outside their grants.
	if _, ok := s.loadEnvVarWritable(w, r, evID, id); !ok {
		return
	}
	// RB-32 (fixed in RF-3): variables were named alongside secrets in the plan's
	// "manageEnvVars becomes departmental" resolution but never got the agency
	// gate. Same rule as the secret PUT above: hold the verb on the agency that
	// owns THIS variable; an unmembered variable is unrestricted-only (RB-Q14).
	if !s.requireEntityAgency(w, r, id, auth.PermManageEnvVars, "env_var_agencies", "env_var_id", evID, "variable") {
		return
	}
	if !auth.ScopeWritable(id, scopeStrOf(inp.Scope)) {
		s.denyScope(w, r, scopeStrOf(inp.Scope), "cannot move an env var into a scope outside your access")
		return
	}
	ev, err := settings.UpdateEnvVar(r.Context(), s.db, evID, settings.EnvVarInput{
		Key: inp.Key, Value: inp.Value, Scope: inp.Scope, Description: inp.Description,
	}, id.Email)
	if err != nil {
		if refErr, ok := errors.AsType[*envref.Error](err); ok {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", refErr.Error())
			return
		}
		if errors.Is(err, settings.ErrKeyConflict) {
			httpx.Fail(w, http.StatusConflict, "conflict", "a secret already uses this key in this scope (A13)")
			return
		}
		if strings.Contains(err.Error(), "UNIQUE") {
			httpx.Fail(w, http.StatusConflict, "conflict", "key already exists for this scope")
			return
		}
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	if ev == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "env var not found")
		return
	}
	httpx.JSON(w, http.StatusOK, ev)
}

func (s *Server) handleDeleteEnvVar(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	evID := r.PathValue("envVarId")
	// M4: enforce write-scope before deleting (404 out-of-scope; 403 on a global row —
	// deleting a global var fail-closes/alters every scope's runs that bind it).
	if _, ok := s.loadEnvVarWritable(w, r, evID, id); !ok {
		return
	}
	// RB-32 (RF-3): departmental ownership — see handleUpdateEnvVar.
	if !s.requireEntityAgency(w, r, id, auth.PermManageEnvVars, "env_var_agencies", "env_var_id", evID, "variable") {
		return
	}
	found, err := settings.DeleteEnvVar(r.Context(), s.db, evID, id.Email)
	if err != nil {
		httpx.Fail500(w, s.log, "delete_failed", err)
		return
	}
	if !found {
		httpx.Fail(w, http.StatusNotFound, "not_found", "env var not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Scope handlers ──────────────────────────────────────────────────────────

func (s *Server) handleListScopes(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("source")
	list, err := settings.ListScopes(r.Context(), s.db, src)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if list == nil {
		list = []settings.Scope{}
	}
	httpx.JSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateScope(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var inp struct {
		Scope          string   `json:"scope"`
		Description    *string  `json:"description"`
		Hosts          []string `json:"hosts"`
		SupportedTypes []string `json:"supportedTypes"`
		RawInventory   *string  `json:"rawInventory"`
	}
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	if inp.Scope == "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "scope name is required")
		return
	}
	sc, err := settings.CreateScope(r.Context(), s.db, settings.LocalScopeInput{
		Scope: inp.Scope, Description: inp.Description, Hosts: inp.Hosts, SupportedTypes: inp.SupportedTypes, RawInventory: inp.RawInventory,
	}, id.Email)
	if err != nil {
		if validationErr, ok := errors.AsType[settings.InventoryValidationError](err); ok {
			httpx.JSON(w, http.StatusUnprocessableEntity, struct {
				Code    string               `json:"code"`
				Message string               `json:"message"`
				Errors  []settings.LineError `json:"errors"`
			}{"inventory_secret_rejected", "the inventory contains inline secret values; use env-var-NAME indirection", validationErr.Errors})
			return
		}
		if strings.Contains(err.Error(), "UNIQUE") {
			httpx.Fail(w, http.StatusConflict, "conflict", "scope name already exists")
			return
		}
		httpx.Fail500(w, s.log, "create_failed", err)
		return
	}
	httpx.JSON(w, http.StatusCreated, sc)
}

func (s *Server) handleGetScope(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("scopeId")
	sc, err := settings.GetScope(r.Context(), s.db, sid)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if sc == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "scope not found")
		return
	}
	httpx.JSON(w, http.StatusOK, sc)
}

func (s *Server) handleGetScopeInventory(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("scopeId")
	doc, err := settings.GetScopeInventory(r.Context(), s.db, sid)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if doc == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "scope not found")
		return
	}
	httpx.JSON(w, http.StatusOK, doc)
}

func (s *Server) handlePutScopeInventory(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	sid := r.PathValue("scopeId")
	var inp struct {
		Raw    string `json:"raw"`
		Format string `json:"format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	doc, lineErrs, err := settings.PutScopeInventory(r.Context(), s.db, sid, inp.Raw, inp.Format, id.Email)
	if err != nil {
		switch {
		case errors.Is(err, settings.ErrInventoryGitReadOnly):
			httpx.Fail(w, http.StatusConflict, "conflict", err.Error())
		case errors.Is(err, settings.ErrInventoryUnsupportedFormat):
			httpx.Fail(w, http.StatusUnprocessableEntity, "unsupported_format", err.Error())
		default:
			httpx.Fail500(w, s.log, "inventory_write_failed", err)
		}
		return
	}
	if len(lineErrs) > 0 {
		// Path A reject — line-numbered secret findings (the scope is unchanged).
		httpx.JSON(w, http.StatusUnprocessableEntity, struct {
			Code    string               `json:"code"`
			Message string               `json:"message"`
			Errors  []settings.LineError `json:"errors"`
		}{"inventory_secret_rejected", "the inventory contains inline secret values; use env-var-NAME indirection", lineErrs})
		return
	}
	if doc == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "scope not found")
		return
	}
	httpx.JSON(w, http.StatusOK, doc)
}

func (s *Server) handleImportScopeHosts(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	sid := r.PathValue("scopeId")
	var inp struct {
		Hosts     []string `json:"hosts"`
		Overwrite bool     `json:"overwrite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil && err != io.EOF {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	res, err := settings.ImportScopeHosts(r.Context(), s.db, sid, inp.Hosts, inp.Overwrite, id.Email)
	if err != nil {
		switch {
		case errors.Is(err, settings.ErrInventoryGitReadOnly):
			httpx.Fail(w, http.StatusConflict, "conflict", err.Error())
		case errors.Is(err, settings.ErrNoInventory), errors.Is(err, settings.ErrProjectionDegraded):
			httpx.Fail(w, http.StatusConflict, "conflict", err.Error())
		default:
			httpx.Fail500(w, s.log, "import_failed", err)
		}
		return
	}
	if res == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "scope not found")
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

func (s *Server) handleUpdateScope(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	sid := r.PathValue("scopeId")
	// SU-5: a scope RENAME cascades scope_restrictions (settings.UpdateScope), so it
	// is an RBAC change — capture the old name to bump the session epoch iff it changes.
	var oldScopeName string
	_ = s.db.QueryRowContext(r.Context(), `SELECT name FROM scopes WHERE id=?`, sid).Scan(&oldScopeName)
	var inp struct {
		Scope          string   `json:"scope"`
		Description    *string  `json:"description"`
		Hosts          []string `json:"hosts"`
		SupportedTypes []string `json:"supportedTypes"`
		RawInventory   *string  `json:"rawInventory"`
	}
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	sc, broken, err := settings.UpdateScope(r.Context(), s.db, sid, settings.LocalScopeInput{
		Scope: inp.Scope, Description: inp.Description, Hosts: inp.Hosts, SupportedTypes: inp.SupportedTypes, RawInventory: inp.RawInventory,
	}, id.Email)
	if err != nil {
		if validationErr, ok := errors.AsType[settings.InventoryValidationError](err); ok {
			httpx.JSON(w, http.StatusUnprocessableEntity, struct {
				Code    string               `json:"code"`
				Message string               `json:"message"`
				Errors  []settings.LineError `json:"errors"`
			}{"inventory_secret_rejected", "the inventory contains inline secret values; use env-var-NAME indirection", validationErr.Errors})
			return
		}
		if strings.Contains(err.Error(), "only amadeus-source") {
			httpx.Fail(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		if strings.Contains(err.Error(), "UNIQUE") {
			httpx.Fail(w, http.StatusConflict, "conflict", "scope name already exists")
			return
		}
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	if sc == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "scope not found")
		return
	}
	// Merge brokenReferences into response (S9).
	type updateResp struct {
		settings.Scope
		BrokenReferences []settings.BrokenReference `json:"brokenReferences"`
	}
	if broken == nil {
		broken = []settings.BrokenReference{}
	}
	// SU-5: on a rename, the scope name in every session's frozen AllowedScopes is now
	// stale — revoke other sessions so they re-resolve (closes the name-reuse vector).
	if oldScopeName != "" && sc.Scope != oldScopeName {
		s.auth.RevokeOtherSessions(w, r)
	}
	httpx.JSON(w, http.StatusOK, updateResp{Scope: *sc, BrokenReferences: broken})
}

func (s *Server) handleDeleteScope(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	sid := r.PathValue("scopeId")
	found, err := settings.DeleteScope(r.Context(), s.db, sid, id.Email)
	if err != nil {
		if strings.Contains(err.Error(), "only amadeus-source") || strings.Contains(err.Error(), "referenced by") {
			httpx.Fail(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		httpx.Fail500(w, s.log, "delete_failed", err)
		return
	}
	if !found {
		httpx.Fail(w, http.StatusNotFound, "not_found", "scope not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Alert handlers ──────────────────────────────────────────────────────────

func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	list, err := settings.ListAlertRules(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if list == nil {
		list = []settings.AlertRule{}
	}
	httpx.JSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateAlert(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	inp, err := decodeAlertRuleInput(r)
	if err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	if code, msg := validateAlertRuleInput(*inp); code != "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, code, msg)
		return
	}
	ar, err := settings.CreateAlertRule(r.Context(), s.db, *inp, id.Email)
	if err != nil {
		httpx.Fail500(w, s.log, "create_failed", err)
		return
	}
	httpx.JSON(w, http.StatusCreated, ar)
}

func (s *Server) handleUpdateAlert(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	aid := r.PathValue("alertId")
	inp, err := decodeAlertRuleInput(r)
	if err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	if code, msg := validateAlertRuleInput(*inp); code != "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, code, msg)
		return
	}
	ar, err := settings.UpdateAlertRule(r.Context(), s.db, aid, *inp, id.Email)
	if err != nil {
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	if ar == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "alert rule not found")
		return
	}
	httpx.JSON(w, http.StatusOK, ar)
}

func (s *Server) handleDeleteAlert(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	aid := r.PathValue("alertId")
	found, err := settings.DeleteAlertRule(r.Context(), s.db, aid, id.Email)
	if err != nil {
		httpx.Fail500(w, s.log, "delete_failed", err)
		return
	}
	if !found {
		httpx.Fail(w, http.StatusNotFound, "not_found", "alert rule not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeAlertRuleInput(r *http.Request) (*settings.AlertRuleInput, error) {
	var body struct {
		TargetMode string   `json:"targetMode"`
		JobName    *string  `json:"jobName"`
		Trigger    string   `json:"trigger"`
		Channels   []string `json:"channels"`
		Recipients *string  `json:"recipients"`
		Owner      *string  `json:"owner"`
		Enabled    bool     `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	return &settings.AlertRuleInput{
		TargetMode: body.TargetMode, JobName: body.JobName,
		Trigger: body.Trigger, Channels: body.Channels, Recipients: body.Recipients,
		Owner: body.Owner, Enabled: body.Enabled,
	}, nil
}

// validateAlertRuleInput refuses a rule that could never fire (VF-15 / J-3).
// Until v0.52.23 the API accepted `tag` targeting, an `n-failures-window`
// trigger, and slack/webhook/in-app channels — all of which the dispatcher has
// no branch for, so the rule persisted, displayed, and did nothing. The
// vocabulary lives in internal/notify beside the code that acts on it; see
// TestAlertEnumsAreHonoured for the guard that keeps the two in step.
//
// Returns an httpx error code and a message naming the field, or "" if valid.
func validateAlertRuleInput(inp settings.AlertRuleInput) (code, msg string) {
	if !notify.TargetModeHonoured(inp.TargetMode) {
		return "invalid_target_mode", fmt.Sprintf(
			"targetMode %q is not supported — use \"job\" or \"all\". A rule with any other target matches no run.", inp.TargetMode)
	}
	if !notify.TriggerHonoured(inp.Trigger) {
		return "invalid_trigger", fmt.Sprintf(
			"trigger %q is not supported — it is matched against the run's terminal status (success, failure, warning, killed, skipped) or \"any\".", inp.Trigger)
	}
	for _, ch := range inp.Channels {
		if !notify.ChannelHonoured(ch) {
			return "invalid_channel", fmt.Sprintf(
				"channel %q has no delivery transport — use \"email\" or \"apprise\". Apprise fans out to Slack, Discord and webhooks.", ch)
		}
	}
	return "", ""
}

// ── SSH Host handlers ────────────────────────────────────────────────────────

func (s *Server) handleListSshHosts(w http.ResponseWriter, r *http.Request) {
	list, err := settings.ListSshHosts(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if list == nil {
		list = []settings.SshHost{}
	}
	httpx.JSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateSshHost(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	inp, err := decodeSshHostInput(r)
	if err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	h, err := settings.CreateSshHost(r.Context(), s.db, *inp, id.Email)
	if err != nil {
		httpx.Fail500(w, s.log, "create_failed", err)
		return
	}
	httpx.JSON(w, http.StatusCreated, h)
}

func (s *Server) handleUpdateSshHost(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	hid := r.PathValue("hostId")
	inp, err := decodeSshHostInput(r)
	if err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	h, err := settings.UpdateSshHost(r.Context(), s.db, hid, *inp, id.Email)
	if err != nil {
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	if h == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "host not found")
		return
	}
	httpx.JSON(w, http.StatusOK, h)
}

func (s *Server) handleDeleteSshHost(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	hid := r.PathValue("hostId")
	found, err := settings.DeleteSshHost(r.Context(), s.db, hid, id.Email)
	if err != nil {
		httpx.Fail500(w, s.log, "delete_failed", err)
		return
	}
	if !found {
		httpx.Fail(w, http.StatusNotFound, "not_found", "host not found")
		return
	}
	_ = id
	w.WriteHeader(http.StatusNoContent)
}

func decodeSshHostInput(r *http.Request) (*settings.SshHostInput, error) {
	var body struct {
		Hostname         string  `json:"hostname"`
		Address          *string `json:"address"`
		Port             int     `json:"port"`
		OS               *string `json:"os"`
		Via              *string `json:"via"`
		AuthKeyEnvVar    *string `json:"authKeyEnvVar"`
		AuthCredentialID *string `json:"authCredentialId"`
		User             *string `json:"user"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	return &settings.SshHostInput{
		Hostname: body.Hostname, Address: body.Address, Port: body.Port,
		OS: body.OS, Via: body.Via, AuthKeyEnvVar: body.AuthKeyEnvVar,
		AuthCredentialID: body.AuthCredentialID, User: body.User,
	}, nil
}

// handleTestSshHost runs a synchronous SSH connection test against a host,
// persists the outcome, audits it, and returns the result (ssh-update.md TC.4).
// A probe that ran but failed to authenticate/connect is still HTTP 200 — the
// test ran and its outcome is data; 4xx/5xx is reserved for "couldn't run it".
func (s *Server) handleTestSshHost(w http.ResponseWriter, r *http.Request) {
	idn, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	hid := r.PathValue("hostId")
	res, err := s.sshProbe.ProbeHost(r.Context(), hid)
	if err != nil {
		failProbe(w, s.log, err, "host")
		return
	}
	if err := settings.SetSshHostStatus(r.Context(), s.db, hid, res.Status, res.CheckedAt); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	name := hid
	out := sshTestResult{ProbeResult: res}
	if h, _ := settings.GetSshHost(r.Context(), s.db, hid); h != nil {
		name = h.Hostname
		// Surface the fingerprint the probe just pinned (FU-1) so the UI can show
		// "here's what we saw" for out-of-band comparison.
		out.HostKeyFingerprint = h.HostKeyFingerprint
		out.HostKeyType = h.HostKeyType
	}
	_ = settings.WriteChangeLog(r.Context(), s.db, idn.Email, "SSH Hosts", "tested", name, res.Status)
	// Additive (V1.1-2): also record the test as a History/Executions run with the
	// probe output as its Log Output. Best-effort — never fails the test request.
	s.recordSSHTestRun(r.Context(), name, res, idn.Email)
	s.log.Info("ssh host connection test", "host", name, "status", res.Status, "latencyMs", res.LatencyMs)
	httpx.JSON(w, http.StatusOK, out)
}

// sshTestResult is the "Test connection" wire response: the probe outcome plus
// the (non-secret) fingerprint of the host key pinned by the test (FU-1). The
// embedded ProbeResult flattens status/message/latencyMs/checkedAt.
type sshTestResult struct {
	sshexec.ProbeResult
	HostKeyFingerprint string `json:"hostKeyFingerprint,omitempty"`
	HostKeyType        string `json:"hostKeyType,omitempty"`
}

// handleClearSshHostKey clears a target's pinned host key (FU-1 re-key).
func (s *Server) handleClearSshHostKey(w http.ResponseWriter, r *http.Request) {
	idn, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	ok, err := settings.ClearSshHostKey(r.Context(), s.db, idn.Email, r.PathValue("hostId"))
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if !ok {
		httpx.Fail(w, http.StatusNotFound, "not_found", "host not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// failProbe maps the "couldn't run the test" sentinels to HTTP status codes.
func failProbe(w http.ResponseWriter, log *slog.Logger, err error, kind string) {
	switch {
	case errors.Is(err, sshexec.ErrProbeNotFound):
		httpx.Fail(w, http.StatusNotFound, "not_found", kind+" not found")
	case errors.Is(err, sshexec.ErrProbeInFlight):
		httpx.Fail(w, http.StatusConflict, "conflict", "a connection test is already in progress")
	default:
		httpx.Fail500(w, log, "probe_failed", err)
	}
}

// ── Bastion handlers ─────────────────────────────────────────────────────────

func (s *Server) handleListBastions(w http.ResponseWriter, r *http.Request) {
	list, err := settings.ListBastions(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if list == nil {
		list = []settings.SshBastion{}
	}
	httpx.JSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateBastion(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	inp, err := decodeBastionInput(r)
	if err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	b, err := settings.CreateBastion(r.Context(), s.db, *inp, id.Email)
	if err != nil {
		httpx.Fail500(w, s.log, "create_failed", err)
		return
	}
	httpx.JSON(w, http.StatusCreated, b)
}

func (s *Server) handleUpdateBastion(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	bid := r.PathValue("bastionId")
	inp, err := decodeBastionInput(r)
	if err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	b, err := settings.UpdateBastion(r.Context(), s.db, bid, *inp, id.Email)
	if err != nil {
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	if b == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "bastion not found")
		return
	}
	httpx.JSON(w, http.StatusOK, b)
}

func (s *Server) handleDeleteBastion(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	bid := r.PathValue("bastionId")
	found, err := settings.DeleteBastion(r.Context(), s.db, bid, id.Email)
	if err != nil {
		httpx.Fail500(w, s.log, "delete_failed", err)
		return
	}
	if !found {
		httpx.Fail(w, http.StatusNotFound, "not_found", "bastion not found")
		return
	}
	_ = id
	w.WriteHeader(http.StatusNoContent)
}

func decodeBastionInput(r *http.Request) (*settings.SshBastionInput, error) {
	var body struct {
		Name             string  `json:"name"`
		Address          string  `json:"address"`
		Port             int     `json:"port"`
		Username         *string `json:"username"`
		AuthKeyEnvVar    *string `json:"authKeyEnvVar"`
		AuthCredentialID *string `json:"authCredentialId"`
		Zone             *string `json:"zone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	return &settings.SshBastionInput{
		Name: body.Name, Address: body.Address, Port: body.Port,
		Username: body.Username, AuthKeyEnvVar: body.AuthKeyEnvVar,
		AuthCredentialID: body.AuthCredentialID, Zone: body.Zone,
	}, nil
}

// decodeCredentialInput parses an SSH key credential create/update body. material
// (the private key) is write-only and never echoed back.
// decodeCredentialInput also returns the agencyIds the caller asked to place a
// NEW credential in (RF-Q2(a)); the update route ignores them — membership edits
// stay on PUT /ssh-credential-agencies.
func decodeCredentialInput(r *http.Request) (*sshkeys.CreateInput, []string, error) {
	var body struct {
		Label       string   `json:"label"`
		Description *string  `json:"description"`
		Source      string   `json:"source"`
		Material    string   `json:"material"`
		VaultRef    string   `json:"vaultRef"`
		AgencyIDs   []string `json:"agencyIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, nil, err
	}
	if body.Source == "" {
		body.Source = "stored"
	}
	return &sshkeys.CreateInput{
		Label: body.Label, Description: body.Description, Source: body.Source,
		Material: body.Material, VaultRef: body.VaultRef,
	}, body.AgencyIDs, nil
}

// handleTestBastion runs a synchronous SSH connection test against a bastion,
// authenticating with the bastion's own username + key (ssh-update.md TC.4).
func (s *Server) handleTestBastion(w http.ResponseWriter, r *http.Request) {
	idn, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	bid := r.PathValue("bastionId")
	res, err := s.sshProbe.ProbeBastion(r.Context(), bid)
	if err != nil {
		failProbe(w, s.log, err, "bastion")
		return
	}
	if err := settings.SetBastionStatus(r.Context(), s.db, bid, res.Status, res.CheckedAt); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	name := bid
	out := sshTestResult{ProbeResult: res}
	if b, _ := settings.GetBastion(r.Context(), s.db, bid); b != nil {
		name = b.Name
		out.HostKeyFingerprint = b.HostKeyFingerprint
		out.HostKeyType = b.HostKeyType
	}
	_ = settings.WriteChangeLog(r.Context(), s.db, idn.Email, "Bastions", "tested", name, res.Status)
	// Additive (V1.1-2): also record the test as a History/Executions run with the
	// probe output as its Log Output. Best-effort — never fails the test request.
	s.recordSSHTestRun(r.Context(), name, res, idn.Email)
	s.log.Info("ssh bastion connection test", "bastion", name, "status", res.Status, "latencyMs", res.LatencyMs)
	httpx.JSON(w, http.StatusOK, out)
}

// handleClearBastionHostKey clears a bastion's pinned host key (FU-1 re-key).
func (s *Server) handleClearBastionHostKey(w http.ResponseWriter, r *http.Request) {
	idn, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	ok, err := settings.ClearBastionHostKey(r.Context(), s.db, idn.Email, r.PathValue("bastionId"))
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if !ok {
		httpx.Fail(w, http.StatusNotFound, "not_found", "bastion not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Global Settings handlers ─────────────────────────────────────────────────

func (s *Server) handleGetGeneralSettings(w http.ResponseWriter, r *http.Request) {
	gs, err := settings.GetGlobalSettings(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	// Surface the server's OS timezone — where cron schedules actually evaluate
	// (time.Local) — so the Settings view can warn when the operator's browser
	// zone differs (review decision 3). This is distinct from gs.Timezone, which
	// is an advisory display preference and does not affect scheduling.
	zoneName, offsetSec := time.Now().Zone()
	tz := time.Local.String()
	if tz == "" || tz == "Local" {
		tz = zoneName
	}
	// appTimezone is the resolved, validated zone the app actually schedules AND
	// displays in (gs.Timezone if loadable, else time.Local — the TZ fallback).
	// It is what the SPA formats timestamps in (timezone-update §5.1).
	httpx.JSON(w, http.StatusOK, struct {
		*settings.GlobalSettings
		ServerTimezone         string `json:"serverTimezone"`
		ServerUTCOffsetMinutes int    `json:"serverUtcOffsetMinutes"`
		AppTimezone            string `json:"appTimezone"`
	}{
		GlobalSettings:         gs,
		ServerTimezone:         tz,
		ServerUTCOffsetMinutes: offsetSec / 60,
		AppTimezone:            settings.ResolveEffectiveTimezone(gs).String(),
	})
}

func (s *Server) handleUpdateGeneralSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var inp settings.GlobalSettings
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	gs, err := settings.UpdateGlobalSettings(r.Context(), s.db, inp, id.Email)
	if err != nil {
		// A bad input value (invalid timezone / defaultExecutor) is a 422, not a
		// 500 — the operator can fix it (timezone-update §6).
		if errors.Is(err, settings.ErrValidation) {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
			return
		}
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	// If the effective app zone changed, rebuild the cron engine in the new zone
	// (robfig/cron can't re-zone in place) AND refresh the cached zone the schedule
	// projections read — keeping scheduling and display in agreement (timezone-update
	// §4.2/§4.4). Order matters: rebuild the scheduler FIRST so it adopts the new
	// zone, THEN point the display cache at the same zone. The scheduler adopts the
	// zone unconditionally (the engine is rebuilt + started in it; entries self-heal
	// via the backstop if the immediate reload errored), so the cache is updated even
	// when the reschedule reports an error — never leaving display in a zone the
	// scheduler isn't using.
	newLoc := settings.ResolveEffectiveTimezone(gs)
	if newLoc.String() != s.appLocation().String() {
		if s.scheduleTimezoneReload != nil {
			if rerr := s.scheduleTimezoneReload(r.Context(), newLoc); rerr != nil {
				// The save succeeded and the engine adopted the zone; a failed entry
				// reload is logged, not fatal — the backstop converges it.
				s.log.Error("settings: timezone reschedule reload failed", "zone", newLoc.String(), "err", rerr)
			}
		}
		s.appLoc.Store(newLoc)
	}
	httpx.JSON(w, http.StatusOK, gs)
}

// handleCapabilities reports which optional integrations are configured AND the
// caller's effective authz permissions so the SPA can gate features without a
// failed write (PP-B1). Session-gated; leaks no secrets, only booleans.
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	// P2.1: reflect ResolveVaultRuntime (env-OR-DB), not env-only. L7: report vault
	// capable only when a REAL client is actually WIRED, not merely when config is
	// present — a bad CA bundle leaves the stub, and reporting vault=true then would
	// have the SPA offer a vault option whose every reveal fails "unavailable". Wire a
	// client (process-singleton, cheap) and ask whether it is a real one.
	capSec := secrets.New(s.db, s.cfg, s.log)
	settings.WireVaultClient(r.Context(), s.db, s.cfg, capSec, s.log)
	vaultOK := capSec.VaultConfigured()
	// compose = the caller may author amadeus-source jobs and workflows (A11). Was
	// HasRole("admin") until AF-2 made it a grantable, agency-bound permission; it
	// is now a flat-union flag like its siblings — "may you compose SOMEWHERE" —
	// and like them it is for nav gating only. Per-object truth is the server's
	// requireComposeScope, which a departmental composer will still fail for a job
	// outside their agencies.
	var perms rolePermissions
	var unrestricted bool
	var composeUnbound bool
	if id, ok := auth.IdentityFrom(r.Context()); ok {
		perms = permsForRoles(id.Roles)
		unrestricted = id.Unrestricted()
		// AF-2 — may this caller author an ALL-scoped (unscoped) definition? Only
		// an unrestricted compose grant may, because a scheduled fire of an unbound
		// job runs scope-unchecked (RB-30). The composer reads this to withhold the
		// "All agencies (global)" option rather than teaching the rule by 403.
		composeUnbound = id.CanUnbound(auth.PermCompose)
	}
	compose := perms.Compose
	// triggerJobs/killJobs are FLAT UNION semantics — "may this actor do this
	// SOMEWHERE" (RB-3). They are for coarse nav gating only and must never gate a
	// route: an operator on Finance reports triggerJobs=true and still cannot
	// trigger a Tax job. Per-row truth is RB-24 (canRun/canKill on list rows).
	//
	// Deliberately NOT emitted: viewDashboard and editSchedules, which RB-Q9/RB-Q13
	// delete outright in Phase 1 — a permission that cannot be enforced should not
	// be advertised as if it were.
	httpx.JSON(w, http.StatusOK, map[string]bool{
		"vault":           vaultOK,
		"apprise":         s.cfg.AppriseURL != "",
		"compose":         compose,
		"manageRoles":     perms.ManageRoles,
		"configureApp":    perms.ConfigureApp,
		"manageEnvVars":   perms.ManageEnvVars,
		"publishSchedule": perms.PublishSchedule,
		"triggerJobs":     perms.TriggerJobs,
		"killJobs":        perms.KillJobs,
		"composeUnbound":  composeUnbound,
		// RB-29: not a permission — a fact about scope REACH. The Run dialog needs it
		// to offer a compliance path for RB-26: an unscoped job must be bound to a
		// scope by a restricted caller, while an unrestricted one may deliberately run
		// it unbound (the system-global case). Without this the UI cannot tell those
		// two callers apart and would have to let both discover the rule via a 403.
		"unrestricted": unrestricted,
	})
}

func (s *Server) handleGetNotifications(w http.ResponseWriter, r *http.Request) {
	nc, err := settings.GetNotificationConfig(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, nc)
}

func (s *Server) handleGetAuditCompliance(w http.ResponseWriter, r *http.Request) {
	ac, err := settings.GetAuditCompliance(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, ac)
}

func (s *Server) handleUpdateAuditCompliance(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var inp settings.AuditCompliance
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	ac, err := settings.UpdateAuditCompliance(r.Context(), s.db, inp, id.Email)
	if err != nil {
		// These knobs drive the nightly sweep since LU-2, so a bad value is now
		// destructive rather than merely stored — surface the rejection as a 422
		// the form can render instead of an opaque 500.
		if ve, ok := errors.AsType[*settings.ValidationError](err); ok {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", ve.Error())
			return
		}
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	// The sweeper re-reads this blob at the top of each run (LU-2), so the change
	// lands on the next nightly sweep with no restart.
	s.log.Info("retention settings updated", "actor", id.Email,
		"runs_days", ac.RetentionDays.Runs, "log_files_days", ac.RetentionDays.LogFiles)
	httpx.JSON(w, http.StatusOK, ac)
}

func (s *Server) handleUpdateNotifications(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var inp settings.NotificationConfig
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	nc, err := settings.UpdateNotificationConfig(r.Context(), s.db, s.cfg, inp, id.Email)
	if err != nil {
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	httpx.JSON(w, http.StatusOK, nc)
}

// handleTestNotification sends a test message over every configured transport
// (K-5). It closes the gap Phase K exposed: the per-transport "last sent" line
// only filled in after a real run failed, so an operator configuring
// notifications could not answer "can this reach anyone?" without waiting for
// something to break.
//
// Takes no body on purpose — every address, gateway and target comes from stored
// config, so this cannot be used to mail an arbitrary recipient.
//
// Always 200 when the send was attempted, with per-transport outcomes in the
// body: a refused SMTP connection is a true answer to the operator's question,
// not a server error. Only a failure to READ the config is a 500.
func (s *Server) handleTestNotification(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	report, err := notify.New(s.db, s.cfg, s.log).SendTest(r.Context())
	if err != nil {
		httpx.Fail500(w, s.log, "test_failed", err)
		return
	}
	// Audited because it delivers to real inboxes: "who sent the 3am test mail"
	// is a question someone will ask.
	outcome := "no transport sent"
	if report.AnySent() {
		outcome = "sent"
	}
	settings.Audit(r.Context(), s.db, id.Email, "Notifications", "test", outcome, detailForTest(report))
	httpx.JSON(w, http.StatusOK, report)
}

// detailForTest renders the per-transport outcomes for the audit row.
func detailForTest(report notify.TestReport) string {
	parts := make([]string, 0, len(report.Results))
	for _, res := range report.Results {
		state := "failed"
		switch {
		case res.Sent:
			state = "sent"
		case res.Skipped:
			state = "skipped"
		}
		parts = append(parts, fmt.Sprintf("%s: %s (%s)", res.Transport, state, res.Detail))
	}
	return strings.Join(parts, "; ")
}

// ── Audit Export handler ─────────────────────────────────────────────────────

func (s *Server) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from := q.Get("from")
	to := q.Get("to")
	format := q.Get("format")
	if format == "" {
		format = "json"
	}
	if format != "csv" && format != "json" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "format must be csv or json")
		return
	}
	var eventTypes []string
	if raw := q.Get("eventTypes"); raw != "" {
		eventTypes = strings.Split(raw, ",")
	}

	contentType := "application/json"
	if format == "csv" {
		contentType = "text/csv"
	}
	filename := fmt.Sprintf("amadeus-audit.%s", format)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	w.WriteHeader(http.StatusOK)

	// Streamed row-by-row (CC.10) so a full-table export stays flat on memory.
	// Headers + 200 are already sent, so a mid-stream failure can only be logged;
	// the truncated body signals it to the operator.
	if err := settings.ExportAudit(r.Context(), s.db, w, from, to, eventTypes, format); err != nil {
		s.log.Error("audit export stream failed", "error", err)
	}
}

func (s *Server) handleGetGitlabSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := settings.GetGitlabConfig(r.Context(), s.db, s.cfg)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, cfg)
}

func (s *Server) handleUpdateGitlabSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var inp settings.GitlabConfig
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	cfg, err := settings.UpdateGitlabConfig(r.Context(), s.db, s.cfg, inp, id.Email)
	if err != nil {
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	// The sync service resolves repo URL/PAT once at startup (E.5).
	s.log.Warn("gitlab settings updated — repo URL/PAT changes take effect on restart", "actor", id.Email)
	httpx.JSON(w, http.StatusOK, cfg)
}

func (s *Server) handleRotateGitlabWebhookSecret(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}

	var inp struct {
		UpdateGitlab   *bool `json:"updateGitlab"`
		OverlapMinutes *int  `json:"overlapMinutes"`
	}

	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
			httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
			return
		}
	}

	updateGitlab := true
	if inp.UpdateGitlab != nil {
		updateGitlab = *inp.UpdateGitlab
	}

	overlapMinutes := 5
	if inp.OverlapMinutes != nil {
		overlapMinutes = *inp.OverlapMinutes
		if overlapMinutes < 5 {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "overlapMinutes must be at least 5")
			return
		}
	}

	secret, gitlabUpdated, overlapUntil, err := settings.RotateWebhookSecret(r.Context(), s.db, s.cfg, updateGitlab, overlapMinutes, id.Email)
	if err != nil {
		switch {
		case errors.Is(err, settings.ErrWebhookSecretEnvPinned):
			httpx.Fail(w, http.StatusConflict, "env_pinned", err.Error())
		case errors.Is(err, settings.ErrRotateNotConfigured):
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		case errors.Is(err, settings.ErrGitlabUnreachable):
			httpx.Fail(w, http.StatusServiceUnavailable, "service_unavailable", err.Error())
		default:
			httpx.Fail500(w, s.log, "rotation_failed", err)
		}
		return
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"secret":        secret,
		"gitlabUpdated": gitlabUpdated,
		"overlapUntil":  overlapUntil.Format(time.RFC3339),
	})
}

func (s *Server) handleGetVaultSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := settings.GetVaultConfig(r.Context(), s.db, s.cfg)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, cfg)
}

func (s *Server) handleUpdateVaultSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var inp settings.VaultConfig
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	cfg, err := settings.UpdateVaultConfig(r.Context(), s.db, s.cfg, inp, id.Email)
	if err != nil {
		if ve, ok := errors.AsType[*settings.ValidationError](err); ok {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", ve.Error())
			return
		}
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	// The vault client is wired once at startup (E.5).
	s.log.Warn("vault settings updated — connection changes take effect on restart", "actor", id.Email)
	httpx.JSON(w, http.StatusOK, cfg)
}

func (s *Server) handleGetLogStorageSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := settings.GetLogStorageConfig(r.Context(), s.db, s.cfg)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	s.overlayArchiveProgress(cfg)
	httpx.JSON(w, http.StatusOK, cfg)
}

// overlayArchiveProgress fills Archive.InProgress from the live sweep — the
// one archive-status field that is process state rather than a stored column.
func (s *Server) overlayArchiveProgress(cfg *settings.LogStorageConfig) {
	if cfg != nil && cfg.Archive != nil && s.logArchiveSync != nil {
		cfg.Archive.InProgress = s.logArchiveSync.InProgress()
	}
}

// handleLogStorageSyncNow runs one archive tick on demand (SL-2, SL-Q8).
// POST /settings/log-storage/sync[?reconcile=1]
//
// The tick runs in the background under the sweep's single-flight lock and the
// response is 202 with the status as of now — a tick may take up to its budget
// (10 min), far past any sensible request timeout. Refreshing the settings card
// shows progress via lastSync*/pending.
func (s *Server) handleLogStorageSyncNow(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	if s.logArchiveSync == nil {
		httpx.Fail(w, http.StatusServiceUnavailable, "log_archive_unavailable", "the log archive sync is not running in this process")
		return
	}
	if s.LogArchive() == nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "select the S3 archive backend and save first")
		return
	}
	if s.logArchiveSync.InProgress() {
		httpx.Fail(w, http.StatusConflict, "sync_in_progress", "a log archive sync is already running")
		return
	}
	reconcile := r.URL.Query().Get("reconcile") == "1" || r.URL.Query().Get("reconcile") == "true"
	actor := id.Email
	s.log.Info("log archive: sync requested", "actor", actor, "reconcile", reconcile)
	if s.shutdownWG != nil {
		s.shutdownWG.Add(1)
	}
	go func() {
		if s.shutdownWG != nil {
			defer s.shutdownWG.Done()
		}
		// Detached from the request: the tick must outlive the response.
		if err := s.logArchiveSync.Sync(s.runCtx, reconcile, actor); err != nil {
			s.log.Warn("log archive: on-demand sync ended with error", "actor", actor, "error", err)
		}
	}()
	cfg, err := settings.GetLogStorageConfig(r.Context(), s.db, s.cfg)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if cfg.Archive == nil {
		cfg.Archive = &settings.LogArchiveStatus{}
	}
	cfg.Archive.InProgress = true
	httpx.JSON(w, http.StatusAccepted, cfg)
}

func (s *Server) handleUpdateLogStorageSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var inp settings.LogStorageConfig
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	cfg, err := settings.UpdateLogStorageConfig(r.Context(), s.db, s.cfg, inp, id.Email)
	if err != nil {
		if ve, ok := errors.AsType[*settings.ValidationError](err); ok {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", ve.Error())
			return
		}
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	// LU-5: re-point every run-log writer now rather than warning that a restart
	// is needed. Fired after the successful save, so the in-process value can
	// never get ahead of what's stored.
	if cfg.Local != nil && cfg.Local.Path != "" {
		s.applyLogDir(r.Context(), cfg.Local.Path)
		s.log.Info("log storage settings updated", "actor", id.Email, "log_dir", cfg.Local.Path, "backend", cfg.Backend)
	}
	// SL-1: same contract for the archive tier — the client the sweep and the
	// reader use is the one just saved, no restart.
	s.applyLogArchive(r.Context())
	s.overlayArchiveProgress(cfg)
	httpx.JSON(w, http.StatusOK, cfg)
}

func (s *Server) handleGetObservabilitySettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := settings.GetObservabilityConfig(r.Context(), s.db, s.cfg)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, cfg)
}

func (s *Server) handleUpdateObservabilitySettings(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var inp settings.ObservabilityConfig
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	cfg, err := settings.UpdateObservabilityConfig(r.Context(), s.db, s.cfg, inp, id.Email)
	if err != nil {
		if ve, ok := errors.AsType[*settings.ValidationError](err); ok {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", ve.Error())
			return
		}
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	httpx.JSON(w, http.StatusOK, cfg)
}

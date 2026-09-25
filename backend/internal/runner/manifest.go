package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/envref"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/gitlab"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/inventory"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// HandleGetManifest returns the execution manifest for a claimed run (R1.1/R1.2,
// D2). GET /api/v1/runs/{traceId}/manifest  (runner bearer auth)
//
// The runner fetches this right after it claims a run via /poll. The manifest
// carries everything the agent needs to execute — resolved command + env +
// host references — resolved through internal/execspec, the same source of
// truth the in-app SSH executor uses (§3), so the two paths can't drift.
//
// Credential model b (D1): host references ONLY for SSH key material —
// AuthKeyEnvVar is the NAME of the env var the agent looks up locally; the
// manifest never carries KEK-decrypted private-key bytes for host auth.
//
// EXCEPTION (P1.4, D1 = 1C): declared reference bindings (Secrets/Variables) ARE
// resolved here and shipped as VALUES in the sensitive Secrets block — the runner
// path deliberately relaxes the "never ship secret bytes" invariant. This is
// gated three ways: the operator's per-runner allow_secret_injection flag (only
// such a runner claims a binding-bearing run), a defense-in-depth refusal to ship
// secrets to a non-flagged runner, and a protocol v6 minimum. Injected values
// never touch the run tree (the agent injects them into the process env).
//
// Authorization (R1.4): the caller must own the run (run.runner_id ==
// caller.runnerId); otherwise 404 (do not leak the run's existence).
func (s *Service) HandleGetManifest(w http.ResponseWriter, r *http.Request) {
	traceID := r.PathValue("traceId")

	// Look up the run. 404 if absent.
	var jobName, runType, status, executor string
	var scope, targetHost, envJSON, runnerID, jobSource, overrideJSON sql.NullString
	var checkoutSHA, checkoutEntry, requiresJSON, scriptRef, triggeredBy, sshUser, sshCred, jobUID sql.NullString
	err := s.db.QueryRowContext(r.Context(), `
		SELECT job_name, run_type, status, executor,
		       scope, target_host, env_json, runner_id, job_source, override_json,
		       checkout_sha, checkout_entry, requires_json, script_ref, triggered_by,
		       ssh_user, ssh_credential, job_uid
		FROM runs WHERE id = ?`, traceID).
		Scan(&jobName, &runType, &status, &executor,
			&scope, &targetHost, &envJSON, &runnerID, &jobSource, &overrideJSON,
			&checkoutSHA, &checkoutEntry, &requiresJSON, &scriptRef, &triggeredBy, &sshUser, &sshCred, &jobUID)
	if err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "run not found")
		return
	}

	// Must be a claimed runner run. Mirror HandleIngestLog's 409 for status.
	if executor != "runner" {
		httpx.Fail(w, http.StatusConflict, "conflict",
			fmt.Sprintf("run executor is %q, not runner", executor))
		return
	}
	if status != "running" {
		httpx.Fail(w, http.StatusConflict, "conflict",
			fmt.Sprintf("run is in status %q, not running", status))
		return
	}

	// Ownership (R1.4): the calling runner must own this run. Respond 404 (not
	// 403) so a non-owning runner cannot probe for the existence of others'
	// runs, consistent with treating cross-run access as "not found for you".
	callerRunnerID, ok := auth.RunnerIDFrom(r.Context())
	if !ok || callerRunnerID != runnerID.String {
		httpx.Fail(w, http.StatusNotFound, "not_found", "run not found")
		return
	}

	// Resolve the command via the shared execspec path (same as sshexec). Git
	// cache dir resolves identically to runners_mount.go (gitlab.DefaultCloneDir).
	interp, body, err := execspec.ResolveCommand(
		r.Context(), s.db, gitlab.DefaultCloneDir(), jobName, jobSource.String, runType)
	if err != nil {
		s.log.Error("manifest resolve command", "trace_id", traceID, "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "command resolution failed")
		return
	}

	// Look up the owning runner's inventory mode (D8) and injection flag.
	inventoryMode := "cronomicon"
	var allowSecretInjection bool
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT inventory, allow_secret_injection FROM runners WHERE id = ?`, runnerID.String).
		Scan(&inventoryMode, &allowSecretInjection); err != nil {
		s.log.Error("manifest read runner inventory", "runner_id", runnerID.String, "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "inventory lookup failed")
		return
	}

	// M3 — the ansible `--limit` is computed for BOTH inventory modes: it carries
	// only host/group NAMES (no secret, no server-side projection dependency in
	// local mode — it derives solely from operator trigger input), so it is
	// PATH-A-safe even for local-mode runners. A raw ansibleLimit passthrough
	// (OD-15) takes precedence over the structured host∪group limit.
	//
	// This site stays standalone rather than reading ResolveRun's returned limit:
	// local-mode runners never call ResolveRun, and for them --limit is the ONLY
	// targeting mechanism shipped. Both routes go through execspec.RunLimit so the
	// two cannot disagree (TG-3).
	limit := execspec.RunLimit(overrideJSON.String, targetHost.String)

	// TG-4 — a pin that fails ValidName is silently DROPPED by AnsibleLimit, which
	// would leave this run with no --limit at all: a pinned job running the entire
	// inventory. Unlike the trigger-boundary host subset (422'd at
	// execution_mount.go), jobs.target_host has no authoring boundary on the Git
	// path, so this is the backstop for git-synced and pre-existing rows. Refuse in
	// the tone of the no_inventory refusal below: not running beats running
	// wide.
	//
	// Scoped to the branch where the pin is actually USED: with a raw ansibleLimit
	// passthrough the operator has taken manual control and RunLimit never folds the
	// pin in, so there is no widening to prevent and refusing would break the
	// documented escape hatch.
	if runType == "ansible" && targetHost.String != "" && !execspec.ValidName(targetHost.String) &&
		execspec.OverrideAnsibleLimit(overrideJSON.String) == "" {
		httpx.Fail(w, http.StatusConflict, "invalid_target_host",
			fmt.Sprintf("job target host %q contains an ansible pattern character, so it cannot be expressed as a --limit; the run would target the full inventory instead of the pinned host. Fix the job's target host (use a plain name), or use ansibleLimit for raw --limit patterns", targetHost.String))
		return
	}

	// CA (the ssh-user plan) — a run carrying a "connect as" identity
	// override cannot reach a LOCAL-inventory runner's targets: the agent resolves
	// hosts (user + key) from its own inventory, so the override would be silently
	// ignored — running as the wrong identity. Refuse in the tone of the
	// no_inventory refusal: not running beats running wrong.
	if inventoryMode != "cronomicon" && (sshUser.String != "" || sshCred.String != "") {
		httpx.Fail(w, http.StatusConflict, "conflict",
			"this run carries a per-run SSH identity override, which a local-inventory runner cannot honor (the agent resolves users/keys from its own inventory); re-run without the override or route to an cronomicon-inventory runner")
		return
	}

	// RP-8/RP-Q3 — an ansible run's identity override travels in the explicit
	// manifest fields below (protocol v7), because the agent builds an argv
	// rather than dialing the targets itself; ssh-family overrides ride
	// ManifestTarget.User. (The v7 agent_too_old gate that once sat here went
	// with the protocol floor — every registered agent speaks v12+.)

	// Phase 3 (RP-17) — the per-run advanced ansible options, read from the run's
	// envelope (there are no columns; these are trigger-time only).
	ansOpts := execspec.AnsibleOptions(overrideJSON.String)
	if ansOpts.Any() {
		if runType != "ansible" {
			// Defence in depth: the trigger boundary 422s this, so reaching here
			// means an envelope was written by something else. Refuse rather than
			// ship options no toolchain will read.
			httpx.Fail(w, http.StatusConflict, "conflict",
				"this run carries advanced ansible options but is not an ansible run")
			return
		}
	}

	// Resolve targets only in 'cronomicon' mode. In 'local' mode the agent resolves
	// hosts against its own inventory (T-b), so we ship only the scope name.
	var targets []runnerproto.ManifestTarget
	// RP-8 — the ansible identity carriage (set below, shipped on the response).
	var manifestSSHUser, manifestSSHKeyRef string
	if inventoryMode == "cronomicon" {
		// F2 host subset + M3 group expansion (override_json.hosts/groups): enforced
		// here for cronomicon-inventory runners since the server resolves their targets.
		// Local-inventory runners resolve hosts agent-side and honor --limit instead.
		resolved, _, err := execspec.ResolveRun(r.Context(), s.db, scope.String, targetHost.String,
			execspec.OverrideHosts(overrideJSON.String), execspec.OverrideGroups(overrideJSON.String))
		if err != nil {
			s.log.Error("manifest resolve targets", "trace_id", traceID, "error", err)
			httpx.Fail(w, http.StatusInternalServerError, "internal", "target resolution failed")
			return
		}
		// CA-3b/RP-8 — apply the run's frozen identity. The credential rides as the
		// derived CRONOMICON_KEY_<label> reference (names only, D1): the same label's
		// material is delivered through the D8 key channel (the implicit KindKey
		// binding in collectReferenceBindings), and the agent's resolveKeyPath
		// prefers a delivered key file for exactly this name.
		//
		// RP-8 makes the branch EXPLICIT per run type. It used to be un-gated and
		// merely unreachable for ansible because every producer zeroed the columns
		// — defence-in-depth by luck, which stops holding the moment the producers
		// admit ansible (RP-7). The channel differs because the consumer differs:
		//
		//   ssh-family — the agent dials the targets with its own SSH client, so
		//                rewriting the resolved targets IS the override.
		//   ansible    — ansible-playbook dials, and the agent never reads
		//                Target.User. The override must be named as an override in
		//                its own fields so the agent can emit it as extra-vars
		//                (RP-Q1), which beat inventory-authored identity. Targets
		//                are deliberately left ALONE: overlaying AuthKeyEnvVar here
		//                would make privateKeyArgs auto-wire --private-key, a
		//                LOWER-precedence channel that the inventory would then
		//                beat per host — the silent half-application RP-Q1 rejected.
		//   terraform  — never; it authenticates through its providers, and the
		//                producers never freeze identity onto it (RP-7).
		if sshUser.String != "" || sshCred.String != "" {
			keyRef := ""
			if sshCred.String != "" {
				keyRef = envref.KeyReference(sshCred.String)
			}
			switch {
			case execspec.SupportedRunType(runType):
				resolved = execspec.ApplyIdentityOverride(resolved, sshUser.String, "", keyRef)
			case runType == "ansible":
				manifestSSHUser, manifestSSHKeyRef = sshUser.String, keyRef
			}
		}
		targets = make([]runnerproto.ManifestTarget, 0, len(resolved))
		for _, t := range resolved {
			// References only (D1): carry AuthKeyEnvVar as the var NAME, never
			// resolve it to key bytes.
			targets = append(targets, runnerproto.ManifestTarget{
				Name:          t.Name,
				Address:       t.Address,
				Port:          t.Port,
				User:          t.User,
				Via:           t.Via,
				AuthKeyEnvVar: t.AuthKeyEnvVar,
				ResolveErr:    t.ResolveErr,
			})
		}
	}

	// Ansible inventory attach (§7.3, M1). Only for cronomicon-mode ANSIBLE runs:
	// terraform runner runs never carry inventory, and local-mode runners hold
	// their own inventory (we must never ship them the host list — the no-leak
	// property). The Raw shipped here is secret-free because secret-bearing
	// inventory is rejected at ingest (internal/inventory.ValidateSecrets); the
	// manifest path never calls secrets.Reveal (D1).
	var inv *runnerproto.ManifestInventory
	if inventoryMode == "cronomicon" && runType == "ansible" && scope.String != "" {
		var raw, format sql.NullString
		// Sequential query (not nested inside an open rows cursor) — safe under the
		// SQLite pool rule.
		qerr := s.db.QueryRowContext(r.Context(),
			`SELECT raw_inventory, inventory_format FROM scopes WHERE name = ?`, scope.String).
			Scan(&raw, &format)
		hasInventory := qerr == nil && raw.Valid && raw.String != ""
		if !hasInventory {
			// No managed inventory for this scope. Running ansible-playbook with no
			// -i targets implicit-localhost — i.e. silently UNSCOPED — which is the
			// §7.2 safety hazard, not just a missing feature. It is reachable when a
			// scope predates this feature (raw NULL until re-synced) or was rejected
			// at ingest for carrying a secret (raw stays NULL). Hard-fail rather than
			// run unscoped (§7.3/§15, OD-7).
			httpx.Fail(w, http.StatusConflict, "no_inventory",
				fmt.Sprintf("ansible run references scope %q which has no managed inventory; it cannot run unscoped — sync an inventory/*.ini for the scope (or fix one rejected for carrying a secret)", scope.String))
			return
		}
		inv = &runnerproto.ManifestInventory{Raw: raw.String, Format: format.String}
	}

	// Job-level execution knobs (jobs.timeout_seconds, jobs.env_passthrough),
	// source-qualified (A9) so an cronomicon job's knobs aren't read off a
	// same-named git job.
	js := jobSource.String
	if js == "" {
		js = "git"
	}
	var timeoutSeconds sql.NullInt64
	var envPassthroughJSON, projectRoot sql.NullString
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT timeout_seconds, env_passthrough, project_root FROM jobs WHERE name = ? AND source = ?`, jobName, js).
		Scan(&timeoutSeconds, &envPassthroughJSON, &projectRoot); err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.log.Error("manifest read job knobs", "job", jobName, "error", err)
	}

	// RX.9 (ansible-update.md, Phase 1) — env-passthrough allowlist for
	// local-toolchain run-types (ansible/terraform execute ON the runner host
	// with a child environment; SSH run-types build their env from the manifest
	// alone). NAMES only, never values (D1): the union of the job's declared
	// env_passthrough list, every {{ lookup('env', NAME) }} the shipped
	// inventory references, and each target's authKeyEnvVar. Always non-nil for
	// local-toolchain runs — even an empty list must ship, because its PRESENCE
	// is what flips a current agent from legacy full-env inheritance to the
	// scoped child env. A local-mode runner's own inventory is invisible here;
	// names it needs beyond this list are the operator's -env-base-extra.
	var envPassthrough []string
	if runType == "ansible" || runType == "terraform" {
		seen := map[string]bool{}
		if envPassthroughJSON.Valid && envPassthroughJSON.String != "" {
			var jobNames []string
			_ = json.Unmarshal([]byte(envPassthroughJSON.String), &jobNames)
			for _, n := range jobNames {
				if n != "" {
					seen[n] = true
				}
			}
		}
		if inv != nil {
			for _, n := range inventory.EnvLookupNames(inv.Raw) {
				seen[n] = true
			}
		}
		for _, t := range targets {
			if t.AuthKeyEnvVar != "" {
				seen[t.AuthKeyEnvVar] = true
			}
		}
		envPassthrough = make([]string, 0, len(seen))
		for n := range seen {
			envPassthrough = append(envPassthrough, n)
		}
		sort.Strings(envPassthrough)
	}

	// Checkout mode (§7, RX.1/RX.2): a run whose job is a project (project_root
	// set) was pinned at enqueue to a commit SHA + entry playbook. Ship a
	// ManifestCheckout instead of relying on the body-only /dev/stdin path. Body
	// stays populated (the entry file's content, resolved above) for display
	// parity; the agent ignores it in checkout mode.
	var checkout *runnerproto.ManifestCheckout
	if checkoutSHA.Valid && checkoutSHA.String != "" {
		// Defense-in-depth (RX.2): the pinned value must be a full commit SHA,
		// never a ref. It is snapshotted from git_sync_state.last_sha, so this
		// only fires on a corrupt/never-synced state.
		if !isFullCommitSHA(checkoutSHA.String) {
			httpx.Fail(w, http.StatusConflict, "no_checkout_sha",
				"checkout run has no valid pinned commit SHA (the repo may not have synced yet)")
			return
		}
		repoURL, _ := settings.ResolveGitlabRuntime(r.Context(), s.db, s.cfg)
		if repoURL == "" {
			httpx.Fail(w, http.StatusConflict, "no_checkout_repo",
				"checkout run cannot resolve the source repository URL; configure the GitLab integration")
			return
		}
		checkout = &runnerproto.ManifestCheckout{
			Repo:  repoURL,
			SHA:   checkoutSHA.String,
			Entry: checkoutEntry.String,
		}
		// ReqPath is a CANDIDATE (<project_root>/requirements.yml); the agent
		// installs it only if it exists in the pinned tree (RX.4). Derived from
		// the job's denormalized project_root.
		if projectRoot.Valid && projectRoot.String != "" {
			checkout.ReqPath = projectRoot.String + "/requirements.yml"
		}
		// Vault opt-in (RX.13): the run's `vault` requirement token sets UsesVault
		// so the agent passes --vault-password-file from ITS OWN config. The
		// password never travels (D1).
		if requiresContains(requiresJSON.String, "vault") {
			checkout.UsesVault = true
		}
	}

	// Dispatch-time reference injection (P1.4, D1 = 1C). Resolve the run's declared
	// Secrets + Variables bindings and ship their VALUES in the sensitive Secrets
	// block — the runner path deliberately relaxes the "never ship secret bytes"
	// invariant, gated below. CRONOMICON_RUN_* run context (log-safe) merges into the
	// plaintext Env. Gated by the injection kill-switch.
	resolved := &runref.Resolved{Env: map[string]string{}}
	if s.cfg.SecretsInjectionEnabled {
		var rerr error
		resolved, rerr = s.resolveManifestReferences(r.Context(), traceID, jobName, jobSource.String, scriptRef.String, scope.String)
		if rerr != nil {
			// Fail-closed: a missing / out-of-scope / un-revealable binding must not
			// yield a run executed without the secret it declared. The error names
			// references only (never values).
			s.log.Error("manifest resolve references", "trace_id", traceID, "error", rerr)
			httpx.Fail(w, http.StatusConflict, "reference_injection_failed",
				// M2: the precise error (server log above) distinguishes out-of-scope
				// from missing; the operator-facing 409 body must NOT — a cross-scope
				// existence oracle. runref.OperatorMessage collapses both.
				"reference injection failed: "+runref.OperatorMessage(rerr))
			return
		}
	}
	// D8: key MATERIAL is as sensitive as a secret value, so the same gates cover it —
	// a run that ships secret values OR key material must go only to a flagged, v6+
	// runner.
	hasSecrets := len(resolved.Env) > 0 || len(resolved.Keys) > 0 || len(resolved.Files) > 0

	// Defense-in-depth: never ship resolved secret values / key material to a runner
	// the operator has NOT flagged for secret injection. The claim gate (poll.go)
	// should already keep a binding-bearing run away from such a runner; this refuses
	// to leak even if that gate were somehow bypassed.
	if hasSecrets && !allowSecretInjection {
		s.log.Error("manifest secret injection refused: runner not flagged",
			"trace_id", traceID, "runner_id", runnerID.String)
		httpx.Fail(w, http.StatusConflict, "secret_injection_not_allowed",
			"this run injects secrets but the assigned runner is not permitted to receive them")
		return
	}
	// (The v6/v9 agent_too_old backstops — an old agent dropping Secrets/Keys or
	// SecretFiles/BecomePasswordRef — went with the protocol floor; the
	// `become-file` requirement token (RA-20a) remains the claim-time control.)

	// Dispatch audit (P1.6): record WHICH references this run received — names +
	// source, never values — to change_log, exactly once per run. Fail-closed: a
	// manifest that injects references is withheld if the audit cannot be recorded,
	// mirroring the reveal handler (never hand out secret material with no trail).
	if len(resolved.Refs) > 0 {
		if err := s.auditInjection(r.Context(), traceID, triggeredBy.String, scope.String, resolved); err != nil {
			s.log.Error("manifest injection audit failed — withholding manifest", "trace_id", traceID, "error", err)
			httpx.Fail(w, http.StatusInternalServerError, "audit_failed",
				"could not record the reference injection; manifest withheld")
			return
		}
	}

	// M5: persist the dispatch-time "this run injected a secret VALUE" decision so
	// the log-ingest fail-closed rule is authoritative to DISPATCH, not to live
	// bindings (which drift when a binding is deleted/edited or its job is pruned
	// mid-run — a clean-empty enumeration that previously flipped a secret-bearing
	// run to the lenient path). resolved.Redact holds exactly the sensitive values
	// (secret/vault; key material once delivered) — var/context values are log-safe
	// and excluded. Set-once. Fail-closed on error: never ship secret values without
	// the flag that arms the ingest redactor's fail-closed guard for the run's life.
	if len(resolved.Redact) > 0 {
		if _, err := s.db.ExecContext(r.Context(),
			`UPDATE runs SET injects_secret = 1 WHERE id = ? AND injects_secret = 0`, traceID); err != nil {
			s.log.Error("mark run injects_secret — withholding manifest", "trace_id", traceID, "error", err)
			httpx.Fail(w, http.StatusInternalServerError, "internal",
				"could not arm the run's redaction guard; manifest withheld")
			return
		}
	}

	// Env = the plaintext env_json snapshot overlaid with the log-safe CRONOMICON_RUN_*
	// run context (never redacted). Reference VALUES go in Secrets, not here.
	runEnv := parseEnvSnapshot(envJSON.String)
	if runEnv == nil {
		runEnv = map[string]string{}
	}
	maps.Copy(runEnv, (runref.RunContext{
		ID:          traceID,
		Job:         jobName,
		JobSource:   jobSource.String,
		Scope:       scope.String,
		Type:        runType,
		TriggeredBy: triggeredBy.String,
		Executor:    "runner",
	}).Env())

	// RA-12 — the derived reference whose materialized file path the agent passes to
	// --become-password-file. A reference rather than a path because the server has
	// no idea where the agent will write it (the same names-only discipline as
	// SSHKeyRef); the agent resolves it against the files it just materialized and
	// fails the run if it cannot.
	becomePasswordRef := ""
	if bs := becomeSecretFor(r.Context(), s.db, jobName, jobSourceOrGit(jobSource.String), jobUID.String); bs != "" && s.cfg.SecretsInjectionEnabled {
		becomePasswordRef = envref.SecretReference(bs)
	}

	manifest := runnerproto.ManifestResponse{
		TraceID:           traceID,
		JobName:           jobName,
		RunType:           runType,
		Executor:          executor,
		Interp:            interp,
		Body:              body,
		Env:               runEnv,
		Secrets:           resolved.Env,
		Keys:              manifestKeys(resolved.Keys),         // D8: delivered SSH-key material
		SecretFiles:       manifestSecretFiles(resolved.Files), // RA-12: file-delivered secrets
		BecomePasswordRef: becomePasswordRef,
		InventoryMode:     inventoryMode,
		Scope:             scope.String,
		Targets:           targets,
		TimeoutSeconds:    int(timeoutSeconds.Int64),
		Inventory:         inv,
		SSHUser:           manifestSSHUser,                 // RP-8 — ansible identity override (names only)
		SSHKeyRef:         manifestSSHKeyRef,               // RP-8 — derived CRONOMICON_KEY_<label>, resolved agent-side
		AnsibleOptions:    manifestAnsibleOptions(ansOpts), // RP-17 — advanced ansible flags (nil when none)
		Limit:             limit,
		EnvPassthrough:    envPassthrough,
		Checkout:          checkout,
	}
	httpx.JSON(w, http.StatusOK, manifest)
}

// resolveManifestReferences resolves the run's declared reference bindings (from
// its job and, when it references one, its script) into injectable values for the
// runner manifest (P1.4). It injects Secrets + Variables; an CRONOMICON_KEY_
// reference is warned and skipped here — runner key-MATERIAL delivery (D8) is a
// follow-up, mirroring the SSH executor's remote-key deferral. Fails closed on the
// first out-of-scope / missing / un-revealable binding.
func (s *Service) resolveManifestReferences(ctx context.Context, runID, jobName, jobSource, scriptRef, scope string) (*runref.Resolved, error) {
	resolved, _, err := s.resolveInjectableReferences(ctx, runID, jobName, jobSource, scriptRef, scope, true)
	return resolved, err
}

// manifestSecretFiles maps the resolver's file-delivered secrets to the wire shape
// (RA-12). nil when there are none, keeping a file-free manifest wire-identical to
// a pre-v9 one.
func manifestSecretFiles(files []runref.FileMaterial) []runnerproto.ManifestSecretFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]runnerproto.ManifestSecretFile, 0, len(files))
	for _, f := range files {
		out = append(out, runnerproto.ManifestSecretFile{Name: f.Name, Reference: f.Reference, Value: f.Value})
	}
	return out
}

// manifestKeys maps the resolver's key materials to the wire ManifestKey shape
// (D8). nil when there are none, keeping a no-keys manifest wire-identical to v6.
func manifestKeys(keys []runref.KeyMaterial) []runnerproto.ManifestKey {
	if len(keys) == 0 {
		return nil
	}
	out := make([]runnerproto.ManifestKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, runnerproto.ManifestKey{Name: k.Name, Reference: k.Reference, Material: k.Material})
	}
	return out
}

// resolveInjectableReferences is the shared resolve pass behind both manifest
// injection (P1.4/D8) and ingest-log redaction (P1.5). It collects the run's
// declared bindings and resolves them. D8: CRONOMICON_KEY_ references are NO LONGER
// dropped — their material is resolved into resolved.Keys (delivered to the runner)
// and resolved.Redact (masked in logs), in lockstep so delivered key bytes cannot
// land in logs un-masked. It reports whether the run injects any SENSITIVE value
// (secret OR key), which arms the M5 dispatch flag / ingest fail-closed rule.
// actorScopes is nil: the run's scope was validated at enqueue and the resolver
// still enforces per-row scope against it. Fails closed on the first out-of-scope /
// missing / un-revealable binding. (warnKeys is retained for signature stability;
// keys are no longer warn-skipped.)
func (s *Service) resolveInjectableReferences(ctx context.Context, runID, jobName, jobSource, scriptRef, scope string, warnKeys bool) (resolved *runref.Resolved, injectsSecret bool, err error) {
	bindings, err := s.collectReferenceBindings(ctx, runID, jobName, jobSource, scriptRef)
	if err != nil {
		return nil, false, err
	}
	for _, b := range bindings {
		if b.Kind == runref.KindSecret || b.Kind == runref.KindKey {
			injectsSecret = true // secret VALUES and key MATERIAL both need fail-closed masking
		}
	}
	// AG-Q8 — the run's FROZEN agency snapshot, never live membership.
	runAgencies, err := runref.RunAgencies(ctx, s.db, runID)
	if err != nil {
		return nil, injectsSecret, err
	}
	resolved, err = s.resolver.Resolve(ctx, nil, scope, runAgencies, bindings)
	return resolved, injectsSecret, err
}

// auditInjection records a run's dispatch-time reference injection to change_log
// (P1.6), exactly once per run (guarded by runs.injection_audited so an agent's
// manifest re-fetch does not duplicate the row). It records reference names + source
// only, NEVER values. Returns the WriteChangeLog error unmasked so the caller can
// fail closed. actor is the run's triggered_by (→ "system" when unattributed).
func (s *Service) auditInjection(ctx context.Context, traceID, actor, scope string, resolved *runref.Resolved) error {
	return runref.AuditInjectionOnce(ctx, s.db, s.log, traceID, actor, scope, resolved)
}

// collectReferenceBindings loads and de-duplicates (by kind+name) the reference
// bindings declared by a run's job and, when it references one, its script — plus
// the operator's per-run additions from the run's override envelope (V2-11;
// manual runs only, every other trigger writes no "references" entry). Additions
// go through the same resolve/redact path as declared bindings, so a per-run
// secret or key arms the fail-closed masking exactly like a declared one.
func (s *Service) collectReferenceBindings(ctx context.Context, runID, jobName, jobSource, scriptRef string) ([]runref.Binding, error) {
	js := jobSource
	if js == "" {
		js = "git"
	}
	// The run row is read FIRST because it carries job_uid (R2-1) — the identity
	// the owner set below is keyed on (R2F-1). Read after building the owners, the
	// bindings would be the union of every same-named job's, and a run would be
	// injected with a sibling department's credentials. ErrNoRows is tolerated
	// (callers pass an empty runID on the pure-enumeration paths): the uid is then
	// empty and the legacy name arm applies, exactly as before this band.
	var overrideJSON, sshCred, jobUID sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT override_json, ssh_credential, job_uid FROM runs WHERE id = ?`, runID).Scan(&overrideJSON, &sshCred, &jobUID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		// Fail like a binding read: the caller cannot prove the run is secret-free
		// without knowing whether additions exist.
		return nil, fmt.Errorf("load run overrides: %w", err)
	}
	owners := []runref.Owner{{Kind: "job", Source: js, Name: jobName, UID: jobUID.String}}
	if scriptRef != "" {
		owners = append(owners, runref.Owner{Kind: "script", Name: scriptRef})
	}
	sets := make([][]runref.Binding, 0, len(owners)+1)
	for _, o := range owners {
		bs, err := runref.ListBindings(ctx, s.db, o)
		if err != nil {
			return nil, fmt.Errorf("load bindings: %w", err)
		}
		sets = append(sets, bs)
	}
	// RA-12 — a job's become password is an implicit FILE-delivered secret binding.
	// Going through the normal binding path is what buys it everything: the agency
	// and scope predicate, the fail-closed missing/ambiguous errors, injectsSecret
	// arming (M5) and — because this is the SHARED collect pass used by the log
	// redactor too — lockstep masking of the value if a playbook ever echoes it.
	// Exactly the CA-3b reasoning for the connect-as key.
	if bs := becomeSecretFor(ctx, s.db, jobName, js, jobUID.String); bs != "" {
		sets = append(sets, []runref.Binding{{Kind: runref.KindSecret, Name: bs, File: true}})
	}
	sets = append(sets, runref.OverrideBindings(overrideJSON.String))
	// CA-3b — a per-run "connect as" credential is an implicit key binding: going
	// through the normal binding path buys the agency check, the fail-closed
	// missing-label error, injectsSecret arming (M5) and lockstep redaction. This
	// is the SHARED collect pass (manifest injection + ingest redaction), so the
	// redactor sees the delivered material too.
	if sshCred.String != "" {
		sets = append(sets, []runref.Binding{{Kind: runref.KindKey, Name: sshCred.String}})
	}
	seen := map[string]bool{}
	var bindings []runref.Binding
	for _, bs := range sets {
		for _, b := range bs {
			// RA-4: kind+name+ALIAS. Deduping on kind+name alone would silently drop
			// a second destination for the same row — the job's own binding would
			// swallow the operator's differently-aliased per-run addition, and the run
			// would come up missing a key it was told it would get.
			key := runref.DedupeKey(b)
			if seen[key] {
				continue
			}
			seen[key] = true
			bindings = append(bindings, b)
		}
	}
	return bindings, nil
}

// requiresContains reports whether the run's requires_json snapshot (a JSON
// array of tokens) contains token. A NULL/blank/'[]' snapshot contains nothing.
func requiresContains(requiresJSON, token string) bool {
	if requiresJSON == "" || requiresJSON == "[]" {
		return false
	}
	var tokens []string
	if err := json.Unmarshal([]byte(requiresJSON), &tokens); err != nil {
		return false
	}
	return slices.Contains(tokens, token)
}

// isFullCommitSHA reports whether s is a full git commit SHA (40 hex chars for
// sha1, 64 for sha256) — never a ref/branch/tag (RX.2: a ref must never travel).
func isFullCommitSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// parseEnvSnapshot unmarshals the run's env_json snapshot into a map. These are
// plain config values (the scheduler env snapshot), not secrets — the same
// snapshot the SSH executor injects (sshexec parseEnv). Returns nil on
// blank/invalid input. Crucially this is NOT a secret-decryption path (D1).
func parseEnvSnapshot(envJSON string) map[string]string {
	if envJSON == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(envJSON), &m); err != nil {
		return nil
	}
	return m
}

// manifestAnsibleOptions converts the envelope-read option set to its wire form,
// returning nil when the operator set none — so a plain run's manifest stays
// byte-identical to a pre-v8 one rather than growing an empty object.
func manifestAnsibleOptions(o execspec.AnsibleOpts) *runnerproto.ManifestAnsibleOptions {
	if !o.Any() {
		return nil
	}
	return &runnerproto.ManifestAnsibleOptions{
		Check:      o.Check,
		Diff:       o.Diff,
		Tags:       o.Tags,
		SkipTags:   o.SkipTags,
		Verbosity:  o.Verbosity,
		Become:     o.Become,
		BecomeUser: o.BecomeUser,
		ExtraVars:  o.ExtraVars,
	}
}

// becomeSecretFor reads a job's declared become-password secret name (RA-12), or ""
// when it declares none — which is every job that predates Phase B.
//
// Read from the JOB rather than from a run snapshot, matching how the run's body and
// interpreter are resolved (execspec.ResolveCommand): the run carries WHAT to run by
// reference, not by copy, and the become secret is part of that same job definition.
// R2F-1: jobUID identifies WHICH job when a name belongs to more than one. The
// become password is an implicit secret binding, so picking the wrong twin here
// injects the wrong department's credential — read by identity when the run
// carries one, by name only when it does not.
func becomeSecretFor(ctx context.Context, database *sql.DB, jobName, jobSource, jobUID string) string {
	var name sql.NullString
	if err := database.QueryRowContext(ctx,
		`SELECT become_password_secret FROM jobs
		  WHERE CASE WHEN ? != '' THEN uid = ? ELSE source = ? AND name = ? END`,
		jobUID, jobUID, jobSource, jobName).Scan(&name); err != nil {
		return ""
	}
	return name.String
}

// jobSourceOrGit normalizes a run's job_source the way every owner lookup in this
// file does — an empty source means the git catalog.
func jobSourceOrGit(src string) string {
	if src == "" {
		return "git"
	}
	return src
}

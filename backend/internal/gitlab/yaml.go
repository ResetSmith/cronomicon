// Package gitlab implements the GitLab integration slice (B3):
//   - Clone + cache the job-definitions repo (go-git).
//   - Parse jobs/*.yaml, workflows/*.yaml, inventory/*.ini (pragma + sidecar).
//   - Validate apiVersion (T10) and the cronomicon:v1 inventory pragma (S10).
//   - Upsert jobs/workflows/scopes into the shared DB tables on sync.
//   - Implement the schedule write-path (POST /schedules/publish) with A2 If-Match OCC.
//   - Expose ValidateFile for the `amadeus validate` CLI.
package gitlab

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/inventory"
	"gopkg.in/yaml.v3"

	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/reaction"

	"github.com/ResetSmith/cronomicon/internal/watchspec"
)

// ──────────────────────────────────────────────────────────────────────────────
// API-version validation (T10)
// ──────────────────────────────────────────────────────────────────────────────

const requiredAPIVersion = "cronomicon.io/v1"

// validKinds is the set of YAML kinds Cronomicon understands.
var validKinds = map[string]bool{
	"Job":              true,
	"Script":           true,
	"Schedule":         true,
	"Playbook":         true,
	"Terraform":        true,
	"Workflow":         true,
	"InventorySidecar": true,
}

// amadeusHeader is the top-level shape every Cronomicon YAML file must have.
type amadeusHeader struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
}

// ValidationError is a structured, line-numbered parse/validate error (S10/T10).
type ValidationError struct {
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"` // 1-based; 0 if line not applicable
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

func (e ValidationError) Error() string {
	if e.File != "" && e.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", e.File, e.Line, e.Message)
	}
	if e.File != "" {
		return fmt.Sprintf("%s: %s", e.File, e.Message)
	}
	return e.Message
}

// ──────────────────────────────────────────────────────────────────────────────
// Job YAML shape
// ──────────────────────────────────────────────────────────────────────────────

// ScheduleEntry is one named cron entry in a Job/Workflow spec's `schedules:`
// list. Env is an optional plaintext map injected when this entry fires.
type ScheduleEntry struct {
	Name string            `yaml:"name" json:"name"`
	Cron string            `yaml:"cron" json:"cron"`
	Env  map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	// StartAt/EndAt are the optional activation window (AW-8): RFC3339 bounds on
	// when this entry's cron may fire. Both empty is the pre-window behavior —
	// active immediately, never expires.
	StartAt string `yaml:"startAt,omitempty" json:"startAt,omitempty"`
	EndAt   string `yaml:"endAt,omitempty" json:"endAt,omitempty"`
	// Interval is the anchored-interval mode ("7d", "36h"), mutually exclusive
	// with Cron and requiring StartAt as its phase anchor. Cron empty + Interval
	// empty + StartAt set is the one-shot mode.
	Interval string `yaml:"interval,omitempty" json:"interval,omitempty"`
	// SkipCalendars / OnlyCalendars bind working calendars to this entry (CAL-9).
	// The calendars themselves are NEVER Git-authored (CAL-Q2) — they are
	// operator-authored reference data. What rides in the repo is the BINDING: a
	// git-source job naming an operator-authored calendar. Without these keys the
	// whole feature would work only for in-app definitions, which are a minority
	// of the fleet and not the ones under change control.
	SkipCalendars []string `yaml:"skipCalendars,omitempty" json:"skipCalendars,omitempty"`
	OnlyCalendars []string `yaml:"onlyCalendars,omitempty" json:"onlyCalendars,omitempty"`
	// SourceRef is the first-class schedule name this entry was expanded from
	// (D1c — schedule-builder.md); empty for an inline entry. Internal-only: it is
	// neither parsed from YAML nor serialized; mergeScheduleRefs sets it so
	// writeDefinitionSchedules can record definition_schedules.source_ref.
	SourceRef string `yaml:"-" json:"-"`
}

// scheduleNameRe constrains schedule entry names to a slug form.
var scheduleNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ReactionEntry is one named entry in a Job/Workflow spec's `reactions:` list
// (RX-13): "when the named upstream finishes with onOutcome, run me".
//
// Reactions are DEFINITION-level, not schedule-entry-level, which is the one
// structural difference from calendar bindings: a reaction has no clock, so
// there is no entry for it to hang off. There is also no standalone `Reaction`
// kind — a first-class schedule has no owner, so it could not own one.
//
// # Why this is Git-authorable at all (RX-Q2)
//
// A reaction is a per-definition binding, so it follows `skipCalendars` (which
// IS in YAML) rather than calendars themselves (which CAL-Q2 deliberately kept
// out of Git). Air-gapped installs author everything through the repo; a
// trigger kind that only worked in-app would be unavailable to exactly the
// deployments under the strictest change control.
type ReactionEntry struct {
	Name string `yaml:"name" json:"name"`
	// OnKind/OnName identify the watched definition. OnSource must be settable:
	// defaulting it to 'git' silently makes a Git-authored reaction unable to
	// watch an in-app definition, and cross-plane edges are exactly what an
	// installation running both sources will want.
	OnKind    string `yaml:"onKind" json:"onKind"`
	OnName    string `yaml:"onName" json:"onName"`
	OnSource  string `yaml:"onSource,omitempty" json:"onSource,omitempty"`
	OnOutcome string `yaml:"onOutcome" json:"onOutcome"`

	DelaySeconds            int   `yaml:"delaySeconds,omitempty" json:"delaySeconds,omitempty"`
	MinIntervalSeconds      int   `yaml:"minIntervalSeconds,omitempty" json:"minIntervalSeconds,omitempty"`
	IncludeWorkflowChildren bool  `yaml:"includeWorkflowChildren,omitempty" json:"includeWorkflowChildren,omitempty"`
	Enabled                 *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// EnabledOrDefault reports the entry's enabled flag, defaulting to true.
func (r ReactionEntry) EnabledOrDefault() bool { return r.Enabled == nil || *r.Enabled }

// OnSourceOrDefault defaults the watched definition's source to 'git', matching
// the column default.
func (r ReactionEntry) OnSourceOrDefault() string {
	if r.OnSource == "" {
		return "git"
	}
	return r.OnSource
}

// NormalizeReactions validates a definition's reaction list SHAPE — everything
// checkable without a database, so `amadeus validate` can catch it at MR time
// rather than at sync. Cross-reference checks (does the upstream exist, does it
// close a cycle) need the DB and live in sync.go.
//
// Returns the entries with defaults applied, plus any structural errors.
func NormalizeReactions(in []ReactionEntry) ([]ReactionEntry, []ValidationError) {
	var errs []ValidationError
	out := make([]ReactionEntry, 0, len(in))
	seen := map[string]bool{}
	for i, e := range in {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("spec.reactions[%d].name", i),
				Message: "reaction name is required",
			})
			continue
		}
		if !scheduleNameRe.MatchString(name) {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("spec.reactions[%d].name", i),
				Message: fmt.Sprintf("reaction name %q must be a slug: lowercase letters, digits, '-' or '_'", name),
			})
			continue
		}
		if seen[name] {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("spec.reactions[%d].name", i),
				Message: fmt.Sprintf("duplicate reaction name %q", name),
			})
			continue
		}
		seen[name] = true

		if e.OnKind != "job" && e.OnKind != "workflow" {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("spec.reactions[%s].onKind", name),
				Message: "onKind must be job or workflow",
			})
			continue
		}
		if strings.TrimSpace(e.OnName) == "" {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("spec.reactions[%s].onName", name),
				Message: "onName is required",
			})
			continue
		}
		if src := e.OnSourceOrDefault(); src != "git" && src != "amadeus" {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("spec.reactions[%s].onSource", name),
				Message: fmt.Sprintf("onSource %q must be git or amadeus", src),
			})
			continue
		}
		if !reaction.ValidOnOutcome(e.OnOutcome) {
			// `killed` and `warning` are the near-misses worth naming: they are
			// runs.status values that normalisation has already folded into
			// stopped and success, so accepting them would let an author write a
			// reaction that can never fire.
			errs = append(errs, ValidationError{
				Field: fmt.Sprintf("spec.reactions[%s].onOutcome", name),
				Message: fmt.Sprintf("onOutcome %q must be one of success, failure, stopped, any",
					e.OnOutcome),
			})
			continue
		}
		if e.DelaySeconds < 0 || e.MinIntervalSeconds < 0 {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("spec.reactions[%s].delaySeconds", name),
				Message: "delaySeconds and minIntervalSeconds cannot be negative",
			})
			continue
		}

		e.Name = name
		e.OnSource = e.OnSourceOrDefault()
		out = append(out, e)
	}
	return out, errs
}

// JobYAML is the parsed representation of a jobs/*.yaml file.
type JobYAML struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Description    string          `yaml:"description"`
		RunType        string          `yaml:"run_type"`     // bash|ansible|terraform|powershell|perl|python
		Schedule       string          `yaml:"schedule"`     // legacy single cron expression; empty = manual
		Schedules      []ScheduleEntry `yaml:"schedules"`    // multi-schedule list (mutually exclusive with schedule)
		ScheduleRefs   []string        `yaml:"scheduleRefs"` // A10a — references to first-class schedules/<name>; resolved into the runtime schedule expansion at sync
		Reactions      []ReactionEntry `yaml:"reactions"`    // RX-13 — run this job when another definition finishes
		Scope          string          `yaml:"scope"`
		TargetHost     string          `yaml:"target_host"`
		Tags           []string        `yaml:"tags"`
		Enabled        *bool           `yaml:"enabled"` // nil → default true
		TimeoutSeconds int             `yaml:"timeout_seconds"`
		Retries        int             `yaml:"retries"`
		Requestable    bool            `yaml:"requestable"`
		// SL — soft deadlines. Distinct from TimeoutSeconds, which KILLS: these
		// only warn, because a job running long is often healthy and merely slow.
		WarnAfterSeconds int    `yaml:"warn_after_seconds"`
		MustFinishBy     string `yaml:"must_finish_by"` // 'HH:MM' wall-clock in the app zone
		// ET-D — file-arrival triggers. Declaring a watch IS the opt-in for this
		// job being started by a file (decision, 2026-08-11): `requestable` gates
		// the token API, a different surface, and requiring both would mean two
		// flags for one intent with silence as the failure mode.
		Watch             []watchspec.Watch `yaml:"watch"`
		ConcurrencyPolicy string            `yaml:"concurrency_policy"` // Allow|Forbid|Queue (940; Replace is coerced to Allow)
		ConcurrencyKey    string            `yaml:"concurrency_key"`
		// JR-Q5 — per-job run-input enforcement: "warn" (default, the UDV4 behavior) or
		// "block" (a declared required input with no value rejects the run, 422). Empty
		// or unrecognized ⇒ "warn", so a typo can never silently harden a job. Persisted
		// as jobs.prompt_enforcement (migration 660).
		PromptEnforcement string `yaml:"prompt_enforcement"`
		// sensitive_logging (removed, PP-M9): redaction is unconditional; the key is
		// silently ignored if still present in a job's Git YAML.
		// B-Git — a job references a reusable Script by name instead of carrying an
		// inline body. Mutually exclusive with command/script/scriptPath. During the
		// transition a job may still carry an inline body (legacy); the Phase 3
		// migration rewrites all jobs to script_ref and the legacy path is removed.
		ScriptRef string `yaml:"script_ref"` // scripts/<name>; sync resolves run_type+body+executor from it
		// EX.1 — executable source (legacy/inline path). Exactly one of command/
		// script/scriptPath. run_type selects the interpreter when executed.
		Command    string `yaml:"command"`    // inline one-liner
		Script     string `yaml:"script"`     // inline multi-line body
		ScriptPath string `yaml:"scriptPath"` // repo-relative file, read from the clone
		// EX.3 — per-job executor default; empty → resolve from run_type.
		Executor string `yaml:"executor"` // runner|ssh
		// RT-2 — the DECLARED runner pin: this job's runs may be claimed only by a
		// runner carrying this tag (the runner-targeting plan). A sibling
		// of executor/target_host/ssh_credential in every sense — it is part of how
		// the job runs, it belongs in review, and sync OVERWRITES it. For a
		// git-source job this is the ONLY durable pin: the operator override that
		// could mask it from inside the app was retired in v1.3.5 (mig. 1090), so a
		// lasting change to where such a job runs is a change to this repository.
		// A single run still escapes via the Run dialog's per-run pin.
		RunnerTag string `yaml:"runner_tag"`
		// CA Phase B (the ssh-user plan) — declarative "connect as"
		// identity: the SSH login and/or stored-credential LABEL every run of
		// this job uses in place of each target host's configured identity
		// (names only, CA-Q2 — never key material). Folded onto the run at
		// enqueue on every producer; the Run dialog's per-run override wins per
		// field. ssh-family run types only; ignored with a sync warning on
		// ansible/terraform, whose identity comes from inventory/toolchain.
		SSHUser       string `yaml:"ssh_user"`
		SSHCredential string `yaml:"ssh_credential"`
		// UDV1 — declared prompt variables (user-defined-vars.md). The operator is
		// asked to fill these in the ad-hoc Run dialog; the answers ride the per-run
		// env override path (UDV2). Advisory: never blocks a sync. Read-only for git
		// jobs (this field is the source of truth for them).
		Prompts []PromptSpec `yaml:"prompts"`
		// RX.9 (ansible-update.md, Phase 1) — env-var NAMES a local-toolchain run
		// (ansible/terraform on a runner) needs forwarded from the runner's own
		// environment under the scoped child env. Names only, never values (D1);
		// the agent resolves them locally (secrets.env). Unioned into the manifest
		// EnvPassthrough with inventory env-lookups and target authKeyEnvVars.
		EnvPassthrough []string `yaml:"env_passthrough"`
		// §5/RX.13 (Phase 3) — requirement tokens a run declares it needs (e.g.
		// `vault`). Snapshotted onto the run at enqueue; drives Checkout.UsesVault
		// now, and claim-gating in Phase 4. Persisted as jobs.requires_json.
		Requires []string `yaml:"requires"`
		// RA-12 (Phase B) — the bare NAME of a Secrets row supplying this job's
		// Ansible become password. A NAME, never a value: the password lives in the
		// Secrets catalogue and is resolved at dispatch through the same reference
		// path as any other secret, then delivered to the runner as a 0600 file the
		// agent passes to `--become-password-file`.
		//
		// Passwordless sudo remains the preferred arrangement for both run types;
		// this is the exception path, not an endorsement.
		BecomePasswordSecret string `yaml:"become_password_secret"`
	} `yaml:"spec"`
	SourcePath string `yaml:"-"` // repo-relative path, set during recursive discovery (folder support)
}

// PromptSpec is one declared prompt variable on a Job (user-defined-vars.md UDV1).
// It surfaces as a fillable field in the ad-hoc Run dialog; the operator's answer
// is submitted as env[Name]=value and merged into runs.env_json via the existing
// per-run override path (UDV2). Persisted (as a JSON array) on jobs.prompts_json
// for both git (sync) and amadeus (composer) jobs.
type PromptSpec struct {
	Name     string   `yaml:"name" json:"name"`                             // env var key the answer binds to (required, unique within a job)
	Label    string   `yaml:"label,omitempty" json:"label,omitempty"`       // human-facing prompt text; defaults to Name in the UI
	Required bool     `yaml:"required,omitempty" json:"required,omitempty"` // empty-at-run produces a warn-only note (UDV4/UDV6)
	Default  *string  `yaml:"default,omitempty" json:"default,omitempty"`   // pre-fill; auto-submitted even if untouched (UDV8). *string distinguishes unset from ""
	Options  []string `yaml:"options,omitempty" json:"options,omitempty"`   // optional dropdown; UI-convenience only, not enforced server-side (UDV7)
}

// ScriptYAML is the parsed representation of a scripts/*.yaml file (B-Git).
//
// A Script is the reusable executable unit referenced by jobs via script_ref.
// run_type, the body (command/script/scriptPath), and the default executor are
// properties of the CODE, so they live here rather than on the Job. The struct is
// shaped to allow a future `parameters:` block without a migration (Decision 2).
type ScriptYAML struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Description string `yaml:"description"`
		RunType     string `yaml:"run_type"` // bash|ansible|terraform|powershell|perl|python
		// Executable source — exactly one of command/script/scriptPath.
		Command    string `yaml:"command"`    // inline one-liner
		Script     string `yaml:"script"`     // inline multi-line body
		ScriptPath string `yaml:"scriptPath"` // repo-relative file, read from the clone
		Executor   string `yaml:"executor"`   // optional default executor: runner|ssh
		// Checkout project (ansible-update.md §7, RX.1). ProjectRoot is a
		// repo-relative directory; Entry is the repo-relative playbook run from
		// the checked-out tree. When ProjectRoot is set, the whole directory is
		// ONE script (its members are not auto-synthesized into standalone
		// scripts) and jobs referencing it get pinned-checkout manifests. Entry
		// is mirrored onto ScriptPath at discovery so catalog display, body lint,
		// and content_hash keep working on the entry file.
		ProjectRoot string `yaml:"project_root"`
		Entry       string `yaml:"entry"`
		// JR-Q6 — run inputs the script DECLARES (same PromptSpec shape a Job carries
		// in spec.prompts). The Job Composer seeds a new job's Run inputs from these,
		// so the required set is correct by default rather than depending on an admin
		// remembering the import button. Distinct from the heuristic `variables`
		// extracted from the body (UDV5): declared beats inferred (UDV1). Advisory —
		// never blocks a sync. Persisted as scripts.prompts_json (migration 651).
		Prompts []PromptSpec `yaml:"prompts"`
	} `yaml:"spec"`
	SourcePath string `yaml:"-"` // Not in YAML, set dynamically during parsing/discovery
}

// WorkflowYAML is the parsed representation of a workflows/*.yaml file.
type WorkflowYAML struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Description  string          `yaml:"description"`
		Schedule     string          `yaml:"schedule"`
		Schedules    []ScheduleEntry `yaml:"schedules"`
		ScheduleRefs []string        `yaml:"scheduleRefs"` // A10a — references to first-class schedules/<name>
		Reactions    []ReactionEntry `yaml:"reactions"`    // RX-13 — run this workflow when another definition finishes
		Enabled      *bool           `yaml:"enabled"`
		Steps        []any           `yaml:"steps"` // stored as raw JSON
	} `yaml:"spec"`
	SourcePath string `yaml:"-"` // repo-relative path, set during recursive discovery (folder support)
}

// ScheduleYAML is the parsed representation of a schedules/*.yaml file (A10a).
//
// A first-class Schedule is a standalone named cron (+ optional plaintext env)
// that N jobs/workflows reference via `scheduleRefs`. It is the schedule analog of
// a Script: the reusable scheduling primitive lifted out of the owner definition.
type ScheduleYAML struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Description string            `yaml:"description"`
		Cron        string            `yaml:"cron"`
		Env         map[string]string `yaml:"env,omitempty"` // optional plaintext env injected when this schedule fires
		// Optional activation window (AW-8), RFC3339. Copied onto every entry
		// expanded from this schedule so the runtime rows stay self-contained.
		StartAt string `yaml:"startAt,omitempty"`
		EndAt   string `yaml:"endAt,omitempty"`
		// Anchored-interval mode ("7d", "36h"), mutually exclusive with Cron and
		// requiring StartAt as its phase anchor (Phase 2).
		Interval string `yaml:"interval,omitempty"`
		// Working-calendar bindings (CAL-9), copied onto every entry expanded
		// from this schedule exactly like cron/env/window — which is what makes a
		// first-class schedule a complete, reusable policy object (§2.5).
		SkipCalendars []string `yaml:"skipCalendars,omitempty"`
		OnlyCalendars []string `yaml:"onlyCalendars,omitempty"`
	} `yaml:"spec"`
	SourcePath string `yaml:"-"` // repo-relative path, set during recursive discovery (folder support)
}

// NormalizeSchedules resolves the legacy `schedule:` string and the `schedules:`
// list into a canonical entry list, returning any line-less validation errors.
//
// Rules:
//   - legacy and list both set        → error (mutually exclusive)
//   - legacy "Manual" (any case)/empty → no entries
//   - legacy "<cron>"                  → one entry named "default"
//   - list                            → entries as-is, validated
//
// Entry names must match scheduleNameRe and be unique (case-insensitive) within
// the definition; each cron must parse.
func NormalizeSchedules(legacy string, list []ScheduleEntry) ([]ScheduleEntry, []ValidationError) {
	var errs []ValidationError
	legacyTrim := strings.TrimSpace(legacy)
	hasLegacy := legacyTrim != "" && !strings.EqualFold(legacyTrim, "Manual")

	if hasLegacy && len(list) > 0 {
		errs = append(errs, ValidationError{Field: "spec.schedules",
			Message: "spec.schedule and spec.schedules are mutually exclusive"})
		return nil, errs
	}

	if len(list) == 0 {
		if !hasLegacy {
			return nil, nil
		}
		if !cronutil.Valid(legacyTrim) {
			errs = append(errs, ValidationError{Field: "spec.schedule",
				Message: fmt.Sprintf("invalid cron expression %q", legacyTrim)})
			return nil, errs
		}
		return []ScheduleEntry{{Name: "default", Cron: legacyTrim}}, nil
	}

	seen := map[string]bool{}
	out := make([]ScheduleEntry, 0, len(list))
	for i, e := range list {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			errs = append(errs, ValidationError{Field: fmt.Sprintf("spec.schedules[%d].name", i),
				Message: "schedule entry name is required"})
			continue
		}
		if !scheduleNameRe.MatchString(name) {
			errs = append(errs, ValidationError{Field: fmt.Sprintf("spec.schedules[%d].name", i),
				Message: fmt.Sprintf("invalid schedule name %q (want %s)", name, scheduleNameRe.String())})
			continue
		}
		if seen[strings.ToLower(name)] {
			errs = append(errs, ValidationError{Field: fmt.Sprintf("spec.schedules[%d].name", i),
				Message: fmt.Sprintf("duplicate schedule name %q", name)})
			continue
		}
		seen[strings.ToLower(name)] = true
		cronExpr := strings.TrimSpace(e.Cron)
		startAt, endAt, werr := NormalizeWindow(e.StartAt, e.EndAt)
		if werr != nil {
			errs = append(errs, ValidationError{Field: fmt.Sprintf("spec.schedules[%d]", i),
				Message: werr.Error()})
			continue
		}
		// One mode check for cron / interval / once, shared with the API so a
		// Git-authored entry is held to exactly the same contract.
		interval := strings.TrimSpace(e.Interval)
		spec := cronutil.Spec{Cron: cronExpr, Interval: interval,
			Window: cronutil.NewWindow(parseRFC3339Ptr(startAt), parseRFC3339Ptr(endAt))}
		if serr := spec.Validate(); serr != nil {
			errs = append(errs, ValidationError{Field: fmt.Sprintf("spec.schedules[%d]", i),
				Message: serr.Error()})
			continue
		}
		// ⚠️ This rebuilds the entry FIELD BY FIELD, so every field added to
		// ScheduleEntry must be copied here or it is silently dropped between the
		// YAML and the runtime row — the same hazard as WorkflowEditor's
		// preservedInline, and it bit the calendar bindings exactly this way.
		out = append(out, ScheduleEntry{Name: name, Cron: cronExpr, Env: e.Env,
			StartAt: startAt, EndAt: endAt, Interval: interval,
			SkipCalendars: e.SkipCalendars, OnlyCalendars: e.OnlyCalendars})
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// YAML file validation entry point (T11 / ValidateFile)
// ──────────────────────────────────────────────────────────────────────────────

// ValidateFile parses and validates the YAML file at path, returning line-numbered
// errors. The path is used only for error attribution; it is acceptable to call
// this on a temp file. This is the entry point wired into `amadeus validate`.
func ValidateFile(path string) ([]ValidationError, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".ini" {
		// Inventory file — validate pragma only.
		_, errs := parseCronomiconPragma(string(data))
		var out []ValidationError
		for _, e := range errs {
			out = append(out, ValidationError{File: path, Line: e.Line, Message: e.Message})
		}
		return out, nil
	}
	return validateYAMLBytes(path, data)
}

func validateYAMLBytes(file string, data []byte) ([]ValidationError, error) {
	// First pass: extract header with line numbers via yaml.Node.
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return []ValidationError{{File: file, Line: 1, Message: "YAML parse error: " + err.Error()}}, nil
	}
	if root.Kind == 0 {
		return []ValidationError{{File: file, Message: "empty YAML document"}}, nil
	}

	// Find apiVersion and kind nodes for line attribution.
	doc := &root
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}
	apiVersionLine, kindLine := 0, 0
	apiVersionVal, kindVal := "", ""
	if doc.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(doc.Content); i += 2 {
			key := doc.Content[i]
			val := doc.Content[i+1]
			switch key.Value {
			case "apiVersion":
				apiVersionLine = key.Line
				apiVersionVal = val.Value
			case "kind":
				kindLine = key.Line
				kindVal = val.Value
			}
		}
	}

	var errs []ValidationError

	if apiVersionVal == "" {
		l := 1
		if apiVersionLine > 0 {
			l = apiVersionLine
		}
		errs = append(errs, ValidationError{File: file, Line: l, Field: "apiVersion", Message: "missing required field apiVersion"})
	} else if apiVersionVal != requiredAPIVersion {
		l := apiVersionLine
		if l == 0 {
			l = 1
		}
		errs = append(errs, ValidationError{File: file, Line: l, Field: "apiVersion",
			Message: fmt.Sprintf("unsupported apiVersion %q (want %q)", apiVersionVal, requiredAPIVersion)})
	}

	if kindVal == "" {
		l := 1
		if kindLine > 0 {
			l = kindLine
		}
		errs = append(errs, ValidationError{File: file, Line: l, Field: "kind", Message: "missing required field kind"})
	} else if !validKinds[kindVal] {
		l := kindLine
		if l == 0 {
			l = 1
		}
		errs = append(errs, ValidationError{File: file, Line: l, Field: "kind",
			Message: fmt.Sprintf("unknown kind %q", kindVal)})
	}

	// EX.1 / B-Git — Job source validation: a job carries EITHER a script_ref
	// (new) OR exactly one inline body (command/script/scriptPath, legacy), not
	// both and not neither. scriptPath confined to the repo, executor enum.
	if kindVal == "Job" {
		var j JobYAML
		if err := yaml.Unmarshal(data, &j); err == nil {
			n := 0
			if j.Spec.Command != "" {
				n++
			}
			if j.Spec.Script != "" {
				n++
			}
			if j.Spec.ScriptPath != "" {
				n++
			}
			hasRef := j.Spec.ScriptRef != ""
			switch {
			case hasRef && n > 0:
				errs = append(errs, ValidationError{File: file, Field: "spec",
					Message: "a Job must define either script_ref or an inline body (command/script/scriptPath), not both"})
			case !hasRef && n == 0:
				errs = append(errs, ValidationError{File: file, Field: "spec",
					Message: "a Job must define a script_ref or exactly one of command, script, or scriptPath"})
			case !hasRef && n > 1:
				errs = append(errs, ValidationError{File: file, Field: "spec",
					Message: "a Job must define only one of command, script, or scriptPath"})
			}
			if j.Spec.ScriptPath != "" && !safeScriptPath(j.Spec.ScriptPath) {
				errs = append(errs, ValidationError{File: file, Field: "spec.scriptPath",
					Message: "scriptPath must be a repo-relative path with no '..' escape"})
			}
			if j.Spec.Executor != "" && j.Spec.Executor != "runner" && j.Spec.Executor != "ssh" {
				errs = append(errs, ValidationError{File: file, Field: "spec.executor",
					Message: fmt.Sprintf("unknown executor %q (want runner|ssh)", j.Spec.Executor)})
			}
			// CA-8 — the "connect as" username becomes an SSH auth string verbatim,
			// so a bad charset is a hard error (unlike the credential label, whose
			// existence is a sync-time WARNING — the repo may sync before the
			// credential is created, and dispatch fails loudly anyway, CA-3).
			if j.Spec.SSHUser != "" && !execspec.ValidSSHUser(j.Spec.SSHUser) {
				errs = append(errs, ValidationError{File: file, Field: "spec.ssh_user",
					Message: fmt.Sprintf("invalid ssh_user %q: want letters, digits, '.', '-', '_', starting with a letter or '_', at most 32 chars", j.Spec.SSHUser)})
			}
			if _, schedErrs := NormalizeSchedules(j.Spec.Schedule, j.Spec.Schedules); len(schedErrs) > 0 {
				for _, e := range schedErrs {
					e.File = file
					errs = append(errs, e)
				}
			}
			// RX-13 — the SHAPE of a reaction is checkable without a database, so
			// `amadeus validate` catches a bad outcome or a malformed name at MR
			// time. This is strictly better than the calendar-binding precedent,
			// which could check nothing offline because a binding names a row.
			if _, rxErrs := NormalizeReactions(j.Spec.Reactions); len(rxErrs) > 0 {
				for _, e := range rxErrs {
					e.File = file
					errs = append(errs, e)
				}
			}
		}
	}

	// B-Git — Script source validation: exactly one of command/script/scriptPath,
	// scriptPath confined to the repo, a valid run_type (it moves off Job onto
	// Script), and the executor enum. Cross-reference resolution (does any job's
	// script_ref name a missing script) is repo-wide and lives in ValidateRepo /
	// sync, not here — validateYAMLBytes only sees one file.
	if kindVal == "Script" {
		var sc ScriptYAML
		if err := yaml.Unmarshal(data, &sc); err == nil {
			// A checkout PROJECT (§7) supplies its executable via project_root +
			// entry instead of command/script/scriptPath. Entry is mirrored onto
			// script_path at discovery, so the body-source count is skipped here
			// for a project wrapper; project-specific checks (entry set, entry
			// under root, root in-repo) live in discoverScripts/ValidateRepo.
			isProject := strings.TrimSpace(sc.Spec.ProjectRoot) != ""
			n := 0
			if sc.Spec.Command != "" {
				n++
			}
			if sc.Spec.Script != "" {
				n++
			}
			if sc.Spec.ScriptPath != "" {
				n++
			}
			switch {
			case isProject && n > 0:
				errs = append(errs, ValidationError{File: file, Field: "spec",
					Message: "a project Script (project_root/entry) must not also define command/script/scriptPath"})
			case !isProject && n == 0:
				errs = append(errs, ValidationError{File: file, Field: "spec",
					Message: "a Script must define exactly one of command, script, or scriptPath (or project_root + entry)"})
			case n > 1:
				errs = append(errs, ValidationError{File: file, Field: "spec",
					Message: "a Script must define only one of command, script, or scriptPath"})
			}
			if isProject && strings.TrimSpace(sc.Spec.Entry) == "" {
				errs = append(errs, ValidationError{File: file, Field: "spec.entry",
					Message: "a project Script (project_root set) must define spec.entry"})
			}
			if sc.Spec.ScriptPath != "" && !safeScriptPath(sc.Spec.ScriptPath) {
				errs = append(errs, ValidationError{File: file, Field: "spec.scriptPath",
					Message: "scriptPath must be a repo-relative path with no '..' escape"})
			}
			if sc.Spec.RunType == "" {
				errs = append(errs, ValidationError{File: file, Field: "spec.run_type",
					Message: "a Script must define spec.run_type"})
			} else if !validRunTypes[sc.Spec.RunType] {
				errs = append(errs, ValidationError{File: file, Field: "spec.run_type",
					Message: fmt.Sprintf("unknown run_type %q", sc.Spec.RunType)})
			}
			if sc.Spec.Executor != "" && sc.Spec.Executor != "runner" && sc.Spec.Executor != "ssh" {
				errs = append(errs, ValidationError{File: file, Field: "spec.executor",
					Message: fmt.Sprintf("unknown executor %q (want runner|ssh)", sc.Spec.Executor)})
			}
		}
	}

	// A10a — first-class Schedule validation: a name slug, exactly one valid cron.
	if kindVal == "Schedule" {
		var sd ScheduleYAML
		if err := yaml.Unmarshal(data, &sd); err == nil {
			if name := strings.TrimSpace(sd.Metadata.Name); name == "" {
				errs = append(errs, ValidationError{File: file, Field: "metadata.name",
					Message: "a Schedule must define metadata.name"})
			} else if !scheduleNameRe.MatchString(name) {
				errs = append(errs, ValidationError{File: file, Field: "metadata.name",
					Message: fmt.Sprintf("invalid schedule name %q (want %s)", name, scheduleNameRe.String())})
			}
			cron := strings.TrimSpace(sd.Spec.Cron)
			if cron == "" {
				errs = append(errs, ValidationError{File: file, Field: "spec.cron",
					Message: "a Schedule must define spec.cron"})
			} else if !cronutil.Valid(cron) {
				errs = append(errs, ValidationError{File: file, Field: "spec.cron",
					Message: fmt.Sprintf("invalid cron expression %q", cron)})
			}
		}
	}

	// Workflow schedule validation (same multi-schedule rules as Job).
	if kindVal == "Workflow" {
		var wf WorkflowYAML
		if err := yaml.Unmarshal(data, &wf); err == nil {
			if _, schedErrs := NormalizeSchedules(wf.Spec.Schedule, wf.Spec.Schedules); len(schedErrs) > 0 {
				for _, e := range schedErrs {
					e.File = file
					errs = append(errs, e)
				}
			}
			if _, rxErrs := NormalizeReactions(wf.Spec.Reactions); len(rxErrs) > 0 {
				for _, e := range rxErrs {
					e.File = file
					errs = append(errs, e)
				}
			}
		}
	}
	return errs, nil
}

// safeScriptPath rejects absolute paths and any '..' traversal so a job's
// scriptPath can only resolve to a file inside the synced clone.
func safeScriptPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	cleaned := filepath.Clean(p)
	return cleaned != ".." && !strings.HasPrefix(cleaned, "../") && !strings.Contains(cleaned, "/../")
}

// publishableDirs are the only top-level directories POST /schedules/publish may
// write to (PP-B3 / Q4). scripts/, inventory/, and anything outside the clone
// are rejected.
var publishableDirs = []string{"jobs/", "schedules/", "workflows/"}

// validatePublishPath guards the operator-supplied publish target against path
// traversal and arbitrary-file-write (PP-B3). The path must be repo-relative
// with no '..' escape (safeScriptPath), end in .yaml/.yml, and live under an
// allowed definition directory. Returns a non-nil error describing the violation.
func validatePublishPath(p string) error {
	if !safeScriptPath(p) {
		return fmt.Errorf("filePath must be a relative path inside the repo with no '..' escape")
	}
	cleaned := filepath.Clean(p)
	if !strings.HasSuffix(cleaned, ".yaml") && !strings.HasSuffix(cleaned, ".yml") {
		return fmt.Errorf("filePath must end in .yaml or .yml")
	}
	for _, d := range publishableDirs {
		if strings.HasPrefix(cleaned, d) {
			return nil
		}
	}
	return fmt.Errorf("filePath must be under one of: jobs/, schedules/, workflows/")
}

// ContentHash returns the Decision-8 content hash of a script body: a
// 'sha256:'-prefixed lowercase hex digest. Callers pass the resolved body — the
// inline command/script string, or the bytes read from a scriptPath file.
func ContentHash(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ──────────────────────────────────────────────────────────────────────────────
// Repo-wide validation (T11 / ValidateRepo) — cross-file script_ref resolution
// ──────────────────────────────────────────────────────────────────────────────

// ValidateRepo validates a whole job-definitions checkout rooted at dir. Unlike
// ValidateFile (one file, no cross-file context) it parses scripts/ first, then
// validates jobs/ INCLUDING script_ref resolution — a job whose script_ref names
// no scripts/<name>.yaml is a hard, line-attributed error (risk #1: caught at MR
// time, not run time). Orphan scripts (referenced by zero jobs) are returned
// separately as non-fatal warnings. A missing scripts/ or jobs/ dir is not an
// error (a repo may have only one).
func ValidateRepo(dir string) (errs []ValidationError, warnings []ValidationError, err error) {
	isYAML := func(name string) bool {
		return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
	}

	// 1. scripts/ — collect declared names (and flag duplicates).
	scriptNames := map[string]bool{}
	scripts, sErrs := discoverScripts(filepath.Join(dir, "scripts"))
	for _, se := range sErrs {
		if ve, ok := se.(ValidationError); ok {
			errs = append(errs, ve)
		} else if ve, ok := se.(*ValidationError); ok && ve != nil {
			errs = append(errs, *ve)
		} else {
			errs = append(errs, ValidationError{Message: se.Error()})
		}
	}
	for _, sc := range scripts {
		name := sc.Metadata.Name
		if name != "" {
			if scriptNames[name] {
				errs = append(errs, ValidationError{
					File:    filepath.Join(dir, sc.SourcePath),
					Field:   "metadata.name",
					Message: fmt.Sprintf("duplicate script name %q", name),
				})
			}
			scriptNames[name] = true
		}
		// Project checks (§7): a project script (project_root set; discovery has
		// already validated the root is in-repo and the entry is under it) must
		// point at an entry playbook that actually exists in the tree. (Role /
		// requirements.yml coverage checks land with Phase 3.)
		if strings.TrimSpace(sc.Spec.ProjectRoot) != "" {
			entryAbs := filepath.Join(dir, filepath.FromSlash(sc.Spec.ScriptPath))
			if st, statErr := os.Stat(entryAbs); statErr != nil || st.IsDir() {
				errs = append(errs, ValidationError{
					File:    filepath.Join(dir, sc.SourcePath),
					Field:   "spec.entry",
					Message: fmt.Sprintf("entry %q does not exist in the repository", sc.Spec.ScriptPath),
				})
			}
			// Phase 3 (RX.5): pinning lint on the project's requirements.yml, and
			// a tree secret-scan (§6.2). At validate/CI time these are ERRORS (CI
			// fails); at sync time they warn (never blocking) — see LintProject.
			for _, f := range LintProject(dir, sc.Spec.ProjectRoot) {
				errs = append(errs, f)
			}
		}
	}

	// 1b. schedules/ — collect declared first-class schedule names (A10a).
	scheduleNames := map[string]bool{}
	schedEntries, schErr := os.ReadDir(filepath.Join(dir, "schedules"))
	if schErr != nil && !os.IsNotExist(schErr) {
		return nil, nil, fmt.Errorf("read schedules dir: %w", schErr)
	}
	for _, e := range schedEntries {
		if e.IsDir() || !isYAML(e.Name()) {
			continue
		}
		path := filepath.Join(dir, "schedules", e.Name())
		data, rErr := os.ReadFile(path)
		if rErr != nil {
			errs = append(errs, ValidationError{File: path, Message: rErr.Error()})
			continue
		}
		ve, _ := validateYAMLBytes(path, data)
		errs = append(errs, ve...)
		var sd ScheduleYAML
		if yaml.Unmarshal(data, &sd) == nil && sd.Metadata.Name != "" {
			if scheduleNames[sd.Metadata.Name] {
				errs = append(errs, ValidationError{File: path, Field: "metadata.name",
					Message: fmt.Sprintf("duplicate schedule name %q", sd.Metadata.Name)})
			}
			scheduleNames[sd.Metadata.Name] = true
		}
	}
	referencedSchedules := map[string]bool{}

	// resolveScheduleRefs validates each scheduleRef of an owner against the
	// collected schedule set (dangling = hard error), marking each referenced.
	resolveScheduleRefs := func(path string, refs []string) {
		for _, ref := range refs {
			referencedSchedules[ref] = true
			if !scheduleNames[ref] {
				errs = append(errs, ValidationError{File: path, Field: "spec.scheduleRefs",
					Message: fmt.Sprintf("scheduleRef %q does not resolve to any schedules/*.yaml", ref)})
			}
		}
	}

	// scriptRunTypes lets a job's effective run type be resolved when the job
	// delegates it to a script_ref (TG-4's advisory check needs to know whether a
	// job is an ansible job).
	scriptRunTypes := map[string]string{}
	for _, sc := range scripts {
		if sc.Metadata.Name != "" {
			scriptRunTypes[sc.Metadata.Name] = sc.Spec.RunType
		}
	}

	// 2. jobs/ — validate each, then resolve script_ref against the script set.
	// R2-5 — refuse a duplicate job/workflow name across the repo's files. The
	// upsert map is keyed by name, so before this guard a second file with the
	// same metadata.name silently WON (last file iterated replaced the first),
	// which was survivable-by-luck while the DB's PK also enforced uniqueness
	// and is a data-loss hazard now that only this validator stands in the way.
	seenJobNames := map[string]string{}
	referenced := map[string]bool{}
	jobEntries, jErr := os.ReadDir(filepath.Join(dir, "jobs"))
	if jErr != nil && !os.IsNotExist(jErr) {
		return nil, nil, fmt.Errorf("read jobs dir: %w", jErr)
	}
	for _, e := range jobEntries {
		if e.IsDir() || !isYAML(e.Name()) {
			continue
		}
		path := filepath.Join(dir, "jobs", e.Name())
		data, rErr := os.ReadFile(path)
		if rErr != nil {
			errs = append(errs, ValidationError{File: path, Message: rErr.Error()})
			continue
		}
		ve, _ := validateYAMLBytes(path, data)
		errs = append(errs, ve...)
		var j JobYAML
		if yaml.Unmarshal(data, &j) == nil {
			if n := j.Metadata.Name; n != "" {
				if prev, dup := seenJobNames[n]; dup {
					errs = append(errs, ValidationError{File: path, Field: "metadata.name",
						Message: fmt.Sprintf("duplicate job name %q (also defined in %s)", n, prev)})
				}
				seenJobNames[n] = path
			}
			if j.Spec.ScriptRef != "" {
				referenced[j.Spec.ScriptRef] = true
				if !scriptNames[j.Spec.ScriptRef] {
					errs = append(errs, ValidationError{File: path, Field: "spec.script_ref",
						Message: fmt.Sprintf("script_ref %q does not resolve to any script in scripts/", j.Spec.ScriptRef)})
				}
			}
			// TG-4 — an ansible job's target_host is passed verbatim to `ansible
			// --limit`, which refuses any name carrying a pattern metacharacter. A
			// refused pin means NO --limit, so the run would widen to the full
			// inventory instead of the pinned host. A warning (not an error) so
			// `amadeus validate` flags it pre-merge without failing CI on it; the
			// sync path warns in the same terms, and the manifest hard-fails such a
			// run with a 409.
			//
			// Run-type resolution mirrors upsertJobs exactly: with a script_ref the
			// SCRIPT's run type is what gets stored, overriding whatever the job
			// declares — so a job saying `run_type: bash` over an ansible script is
			// an ansible job, and must be checked as one.
			runType := j.Spec.RunType
			if j.Spec.ScriptRef != "" {
				if rt, ok := scriptRunTypes[j.Spec.ScriptRef]; ok {
					runType = rt
				}
			}
			if runType == "ansible" && j.Spec.TargetHost != "" && !inventory.ValidName(j.Spec.TargetHost) {
				warnings = append(warnings, ValidationError{File: path, Field: "spec.target_host",
					Message: fmt.Sprintf("target_host %q contains an ansible pattern character; it cannot be expressed as a --limit, so runs of this job will be REFUSED (409) rather than silently targeting the full inventory", j.Spec.TargetHost)})
			}
			resolveScheduleRefs(path, j.Spec.ScheduleRefs)
		}
	}

	seenWorkflowNames := map[string]string{}
	// 2b. workflows/ — validate each + resolve scheduleRefs (A10a).
	wfEntries, wErr := os.ReadDir(filepath.Join(dir, "workflows"))
	if wErr != nil && !os.IsNotExist(wErr) {
		return nil, nil, fmt.Errorf("read workflows dir: %w", wErr)
	}
	for _, e := range wfEntries {
		if e.IsDir() || !isYAML(e.Name()) {
			continue
		}
		path := filepath.Join(dir, "workflows", e.Name())
		data, rErr := os.ReadFile(path)
		if rErr != nil {
			errs = append(errs, ValidationError{File: path, Message: rErr.Error()})
			continue
		}
		ve, _ := validateYAMLBytes(path, data)
		errs = append(errs, ve...)
		var wf WorkflowYAML
		if yaml.Unmarshal(data, &wf) == nil {
			if n := wf.Metadata.Name; n != "" {
				if prev, dup := seenWorkflowNames[n]; dup {
					errs = append(errs, ValidationError{File: path, Field: "metadata.name",
						Message: fmt.Sprintf("duplicate workflow name %q (also defined in %s)", n, prev)})
				}
				seenWorkflowNames[n] = path
			}
			// R2F-2/R2F-Q3 — names are the law in git. A step MAY carry `jobUid` in
			// the API (the composer writes it, so a step can name one of two
			// same-named jobs), but the git pool is name-unique by constraint, so an
			// identity in a hand-edited file buys nothing and rots the first time the
			// repo is replayed into a different installation. Warn and ignore rather
			// than refuse: ignore-unknown is this parser's standing posture, and a
			// hard error would break a repo exported from an API-composed workflow.
			if step := firstStepWithJobUID(wf.Spec.Steps); step != "" {
				warnings = append(warnings, ValidationError{File: path, Field: "spec.steps",
					Message: fmt.Sprintf("step %q sets jobUid, which is ignored in git-sourced workflows — "+
						"names identify jobs in a repository (one name per repo), and a stored identity would not survive replay into another installation", step)})
			}
			resolveScheduleRefs(path, wf.Spec.ScheduleRefs)
		}
	}

	// 3. Orphan scripts — referenced by no job. Non-fatal (a script may be added
	// before the jobs that use it, or kept as a building block).
	for name := range scriptNames {
		if !referenced[name] {
			warnings = append(warnings, ValidationError{Field: "metadata.name",
				Message: fmt.Sprintf("script %q is referenced by no job (orphan)", name)})
		}
	}
	// 3b. Orphan schedules — referenced by no job/workflow. Non-fatal.
	for name := range scheduleNames {
		if !referencedSchedules[name] {
			warnings = append(warnings, ValidationError{Field: "metadata.name",
				Message: fmt.Sprintf("schedule %q is referenced by no job or workflow (orphan)", name)})
		}
	}
	return errs, warnings, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Inventory pragma parsing (S10)
// ──────────────────────────────────────────────────────────────────────────────

// validRunTypes is the allowed set of run-type strings (mirrors VALID_RUN_TYPES in prototype).
var validRunTypes = map[string]bool{
	"bash":       true,
	"ansible":    true,
	"terraform":  true,
	"powershell": true,
	"perl":       true,
	"python":     true,
}

// PragmaDirectives holds the decoded cronomicon:v1 pragma values from an inventory file.
type PragmaDirectives struct {
	Types       []string // declared run types (validated)
	Owner       string
	Description string
}

// pragmaError is a line-numbered error from pragma parsing.
type pragmaError struct {
	Line    int
	Message string
}

// pragmaRe matches `# cronomicon:v<N> key=value` comment lines.
var pragmaRe = regexp.MustCompile(`^[#;]\s*cronomicon:v(\d+)\s+(.+)$`)
var kvRe = regexp.MustCompile(`^(\w+)=(.+)$`)

// parseCronomiconPragma parses `# cronomicon:v1 key=value` directives from the top
// of an inventory file. Parsing is strict per S10: unknown directives, unsupported
// versions, malformed lines, and unknown run-type values all produce line-numbered
// errors. Errors do not stop parsing of subsequent lines.
func parseCronomiconPragma(content string) (PragmaDirectives, []pragmaError) {
	var out PragmaDirectives
	var errs []pragmaError

	scanner := bufio.NewScanner(strings.NewReader(content))
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		raw := scanner.Text()
		line := strings.TrimSpace(raw)

		// Top-of-file only: stop at the first non-comment, non-blank line.
		if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, ";") {
			break
		}
		m := pragmaRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		version := m[1]
		if version != "1" {
			errs = append(errs, pragmaError{Line: lineNum, Message: fmt.Sprintf("unsupported pragma version: v%s", version)})
			continue
		}
		rest := strings.TrimSpace(m[2])
		kv := kvRe.FindStringSubmatch(rest)
		if kv == nil {
			errs = append(errs, pragmaError{Line: lineNum, Message: fmt.Sprintf("malformed directive: %q", rest)})
			continue
		}
		key, rawVal := kv[1], kv[2]
		switch key {
		case "types":
			requested := strings.Split(rawVal, ",")
			var invalid []string
			var valid []string
			for _, t := range requested {
				t = strings.TrimSpace(t)
				if t == "" {
					continue
				}
				if !validRunTypes[t] {
					invalid = append(invalid, t)
				} else {
					valid = append(valid, t)
				}
			}
			if len(invalid) > 0 {
				errs = append(errs, pragmaError{Line: lineNum, Message: fmt.Sprintf("unknown run type(s): %s", strings.Join(invalid, ", "))})
			}
			out.Types = valid
		case "owner":
			out.Owner = strings.TrimSpace(rawVal)
		case "description":
			out.Description = strings.TrimSpace(rawVal)
		default:
			errs = append(errs, pragmaError{Line: lineNum, Message: fmt.Sprintf("unknown directive: %q", key)})
		}
	}
	return out, errs
}

// ──────────────────────────────────────────────────────────────────────────────
// Sidecar YAML shape (inventory/<name>.cronomicon.yaml)
// ──────────────────────────────────────────────────────────────────────────────

type sidecarYAML struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Types       []string `yaml:"types"`
		Owner       string   `yaml:"owner"`
		Description string   `yaml:"description"`
		// AuthKeyEnvVar (M4 / §9.2) is the per-scope default env-var NAME the in-app
		// SSH executor resolves to a key when importing this inventory's hosts into
		// ssh_hosts. A NAME only (never a secret value, D1); per-host
		// `cronomicon_auth_key_env_var` overrides it.
		AuthKeyEnvVar string `yaml:"authKeyEnvVar"`
	} `yaml:"spec"`
}

// ──────────────────────────────────────────────────────────────────────────────
// Capability resolution (resolveScopeCapability semantics from prototype)
// ──────────────────────────────────────────────────────────────────────────────

// CapabilityOrigin identifies how a scope's run-type capability was derived.
type CapabilityOrigin string

const (
	OriginLocal    CapabilityOrigin = "local"
	OriginSidecar  CapabilityOrigin = "sidecar"
	OriginPragma   CapabilityOrigin = "pragma"
	OriginInferred CapabilityOrigin = "inferred"
)

// ScopeCapability is the resolved declared run-type capability for a scope.
type ScopeCapability struct {
	Types       []string
	Origin      CapabilityOrigin
	Owner       string
	SidecarPath string
	Errors      []ValidationError
}

// resolveInventoryCapability mirrors the prototype's resolveScopeCapability for git-source scopes.
// content is the raw .ini text; sidecar is the parsed sidecar (nil if absent); sidecarPath
// is used in error attribution.
func resolveInventoryCapability(content string, sidecar *sidecarYAML, sidecarPath string) ScopeCapability {
	directives, pragmaErrs := parseCronomiconPragma(content)

	var valErrs []ValidationError
	for _, e := range pragmaErrs {
		valErrs = append(valErrs, ValidationError{File: sidecarPath, Line: e.Line, Message: e.Message})
	}

	// Sidecar types win over pragma.
	if sidecar != nil && len(sidecar.Spec.Types) > 0 {
		return ScopeCapability{
			Types:       sidecar.Spec.Types,
			Origin:      OriginSidecar,
			Owner:       sidecar.Spec.Owner,
			SidecarPath: sidecarPath,
			Errors:      valErrs,
		}
	}
	if len(directives.Types) > 0 {
		return ScopeCapability{
			Types:  directives.Types,
			Origin: OriginPragma,
			Owner:  directives.Owner,
			Errors: valErrs,
		}
	}
	// Inferred fallback.
	return ScopeCapability{
		Types:  inferScopeTypes(content),
		Origin: OriginInferred,
		Errors: valErrs,
	}
}

// inferScopeTypes is a conservative heuristic fallback when no pragma/sidecar
// declares types. Delegates to inventory.InferTypes so git sync and the in-app
// upload handler (M5) share ONE heuristic.
func inferScopeTypes(content string) []string {
	return inventory.InferTypes(content)
}

// projectClaim is a project_root directory claimed by a kind:Script wrapper
// (ansible-update.md §7). root is repo-relative and cleaned (e.g.
// "scripts/vmware-patch"); wrapper is the claiming wrapper's repo-relative path.
type projectClaim struct {
	root    string
	wrapper string
}

// discoverScripts walks scripts/ and returns one ScriptYAML per catalog entry.
// It is TWO-PASS (§7 / review R1): a single WalkDir cannot both auto-synthesize
// bare files and honor project grouping, because WalkDir visits lexically — a
// project's member file (e.g. roles/x/tasks/main.yml) can be reached BEFORE the
// wrapper that claims its tree. Pass 1 collects every project_root claim; pass 2
// synthesizes and filters, skipping files inside a claimed root. Correctness no
// longer depends on visit order.
func discoverScripts(dir string) ([]ScriptYAML, []error) {
	var scripts []ScriptYAML
	var errs []error

	// ── Pass 1: collect project_root claims from kind:Script wrappers. ──────────
	var claims []projectClaim
	claimWalkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if ext := strings.ToLower(filepath.Ext(rel)); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil // pass 2 reports read errors
		}
		var hdr amadeusHeader
		_ = yaml.Unmarshal(data, &hdr)
		if hdr.APIVersion != requiredAPIVersion || hdr.Kind != "Script" {
			return nil
		}
		var sc ScriptYAML
		if yaml.Unmarshal(data, &sc) != nil {
			return nil
		}
		if strings.TrimSpace(sc.Spec.ProjectRoot) == "" {
			return nil
		}
		root, verr := cleanProjectRoot(sc.Spec.ProjectRoot)
		if verr != nil {
			errs = append(errs, ValidationError{File: path, Field: "spec.project_root", Message: verr.Error()})
			return nil
		}
		if strings.TrimSpace(sc.Spec.Entry) == "" {
			errs = append(errs, ValidationError{File: path, Field: "spec.entry",
				Message: "a project wrapper (spec.project_root set) must declare spec.entry"})
			return nil
		}
		// Overlapping / nested roots: first wins, warn on the later one.
		overlaps := false
		for _, c := range claims {
			if c.root == root || pathWithin(root, c.root) || pathWithin(c.root, root) {
				errs = append(errs, ValidationError{File: path, Field: "spec.project_root",
					Message: fmt.Sprintf("project_root %q overlaps already-claimed root %q (first wins)", root, c.root)})
				overlaps = true
				break
			}
		}
		if !overlaps {
			claims = append(claims, projectClaim{root: root, wrapper: filepath.ToSlash(filepath.Join("scripts", rel))})
		}
		return nil
	})
	if claimWalkErr != nil && !os.IsNotExist(claimWalkErr) {
		errs = append(errs, fmt.Errorf("walk scripts dir (claims): %w", claimWalkErr))
	}

	// ── Pass 2: synthesize + filter. ───────────────────────────────────────────
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		repoRel := filepath.ToSlash(filepath.Join("scripts", rel))
		ext := strings.ToLower(filepath.Ext(rel))

		// 1. YAML files: Cronomicon Script wrappers, or raw Ansible playbooks.
		if ext == ".yaml" || ext == ".yml" {
			data, err := os.ReadFile(path)
			if err != nil {
				errs = append(errs, fmt.Errorf("read %s: %w", rel, err))
				return nil
			}
			var hdr amadeusHeader
			_ = yaml.Unmarshal(data, &hdr)
			if hdr.APIVersion == requiredAPIVersion && hdr.Kind == "Script" {
				// Wrappers are ALWAYS processed — a wrapper is never filtered by a
				// claimed root (§7: "wrapper files themselves excepted").
				valErrs, _ := validateYAMLBytes(path, data)
				for _, ve := range valErrs {
					errs = append(errs, ve)
				}
				var sc ScriptYAML
				if err := yaml.Unmarshal(data, &sc); err != nil {
					errs = append(errs, ValidationError{File: path, Message: err.Error()})
					return nil
				}
				if strings.TrimSpace(sc.Spec.ProjectRoot) != "" {
					// Project wrapper → exactly ONE script; script_path = entry.
					root, verr := cleanProjectRoot(sc.Spec.ProjectRoot)
					entry := strings.TrimSpace(sc.Spec.Entry)
					if verr != nil || entry == "" {
						return nil // already reported in pass 1
					}
					if !pathWithin(entry, root) && entry != root {
						errs = append(errs, ValidationError{File: path, Field: "spec.entry",
							Message: fmt.Sprintf("entry %q must live under project_root %q", entry, root)})
						return nil
					}
					sc.Spec.ProjectRoot = root
					sc.Spec.ScriptPath = entry
					sc.SourcePath = repoRel
					scripts = append(scripts, sc)
					return nil
				}
				if sc.Spec.ScriptPath == "" {
					sc.Spec.ScriptPath = repoRel
				}
				sc.SourcePath = repoRel
				scripts = append(scripts, sc)
				return nil
			}
			// Raw Ansible playbook — filtered if inside a claimed project tree.
			if insideClaim(repoRel, claims) {
				return nil
			}
			scripts = append(scripts, synthesizeRawScript(rel, "ansible"))
			return nil
		}

		// 2. Other raw scripts.
		var runType string
		switch ext {
		case ".sh":
			runType = "bash"
		case ".tf":
			runType = "terraform"
		case ".ps1":
			runType = "powershell"
		case ".pl":
			runType = "perl"
		case ".py":
			runType = "python"
		default:
			return nil // Skip unsupported formats
		}
		if insideClaim(repoRel, claims) {
			return nil
		}
		scripts = append(scripts, synthesizeRawScript(rel, runType))
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		errs = append(errs, fmt.Errorf("walk scripts dir: %w", walkErr))
	}

	return scripts, errs
}

// cleanProjectRoot validates and normalizes a wrapper's spec.project_root: it
// must be a clean, repo-relative directory under scripts/ that does not escape
// the repo. Returns the slash-normalized path (e.g. "scripts/vmware-patch").
func cleanProjectRoot(raw string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(raw)))
	if clean == "" || clean == "." {
		return "", fmt.Errorf("project_root is empty")
	}
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("project_root %q must be repo-relative and must not escape the repo", raw)
	}
	if clean != "scripts" && !strings.HasPrefix(clean, "scripts/") {
		return "", fmt.Errorf("project_root %q must be under scripts/", raw)
	}
	return clean, nil
}

// pathWithin reports whether repo-relative path a is strictly inside directory b
// (a == b/... ), segment-aware so "scripts/ab" is NOT within "scripts/a".
func pathWithin(a, b string) bool {
	a = filepath.ToSlash(filepath.Clean(a))
	b = filepath.ToSlash(filepath.Clean(b))
	return strings.HasPrefix(a, b+"/")
}

// insideClaim reports whether repo-relative file path is inside any claimed
// project root (used to filter auto-synthesis of project member files).
func insideClaim(repoRel string, claims []projectClaim) bool {
	for _, c := range claims {
		if repoRel == c.root || pathWithin(repoRel, c.root) {
			return true
		}
	}
	return false
}

func synthesizeRawScript(relPath string, runType string) ScriptYAML {
	sc := ScriptYAML{}
	sc.APIVersion = requiredAPIVersion
	sc.Kind = "Script"
	sc.Metadata.Name = relPath // keep file extension
	sc.Spec.RunType = runType
	sc.Spec.ScriptPath = filepath.Join("scripts", relPath)
	sc.Spec.Description = fmt.Sprintf("Auto-discovered %s script", runType)
	sc.SourcePath = filepath.Join("scripts", relPath)
	return sc
}

// stripStepJobUIDs removes `jobUid` from every step in a raw YAML step graph,
// in place, walking the same containers firstStepWithJobUID does (R2F-2). Called
// on the sync path so a git-sourced graph reaches the engine name-only, which is
// what ValidateRepo's warning promises.
func stripStepJobUIDs(steps []any) {
	for _, raw := range steps {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		delete(m, "jobUid")
		for _, key := range []string{"jobs", "steps"} {
			if nested, ok := m[key].([]any); ok {
				stripStepJobUIDs(nested)
			}
		}
		for _, key := range []string{"pass", "fail"} {
			arm, ok := m[key].(map[string]any)
			if !ok {
				continue
			}
			if nested, ok := arm["steps"].([]any); ok {
				stripStepJobUIDs(nested)
			}
		}
	}
}

// firstStepWithJobUID reports the name of the first step in a raw YAML step
// graph that sets `jobUid`, or "" when none does (R2F-2). It walks the nesting
// containers the workflow engine walks — parallel arms, sequence chains and both
// branch arms — because a hand-edited identity is just as inert three levels
// down as at the top, and warning about only the top level would read as
// approval of the rest.
func firstStepWithJobUID(steps []any) string {
	for _, raw := range steps {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if uid, _ := m["jobUid"].(string); uid != "" {
			name, _ := m["name"].(string)
			if name == "" {
				name = uid
			}
			return name
		}
		for _, key := range []string{"jobs", "steps"} {
			if nested, ok := m[key].([]any); ok {
				if hit := firstStepWithJobUID(nested); hit != "" {
					return hit
				}
			}
		}
		for _, key := range []string{"pass", "fail"} {
			arm, ok := m[key].(map[string]any)
			if !ok {
				continue
			}
			if nested, ok := arm["steps"].([]any); ok {
				if hit := firstStepWithJobUID(nested); hit != "" {
					return hit
				}
			}
		}
	}
	return ""
}

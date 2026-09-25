# Cronomicon — Script Orchestrator

<p align="center">
  <img src="assets/cronomicon-logo.png" alt="Cronomicon" width="480">
</p>

<p align="center"><em>Ancient rites of scheduling, made safe.</em></p>

[![Version](https://img.shields.io/badge/version-1.5.45-blue)](CHANGELOG.md)
[![Status](https://img.shields.io/badge/status-stable-brightgreen)](CHANGELOG.md#100---2026-08-11)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

Cronomicon is a **centralized script orchestrator** for scheduling and executing Bash, Ansible, Terraform, PowerShell, Perl, and Python jobs with a modern web UI, departmental access control, and distributed execution via autonomous runner agents.

The product is built around **composable primitives** — reusable **Scripts** and **Schedules** combine with **Env Vars** into **Jobs**, and ordered Jobs (with branching logic and Env Vars passed between them) compose into **Workflows**. Each definition can live in **either Git or Cronomicon** (the *dual-source* model): browse and version everything in a GitLab repository, **or** compose Jobs, Workflows, and Schedules **in-app** (including first-class Schedule authoring via the Schedule Builder) with no Git round-trip. The Workflow Editor is an interactive **graph canvas** (React Flow) for visual composition — drag job steps, compose parallel groups, author **nested** branch arms, and wire inter-job data passing as edges — alongside advanced run controls (per-step retries, inputs, soft-cancellation). Catalogs are browsable as **folder trees** with client-side **Tag filtering**, and synced scripts are scanned for the **environment variables they reference** so operators see what to define before running.

A run can start six ways — a **schedule**, a **click**, a **Git push**, an **API token**, another run finishing (a **reaction**), or a **file arriving** — and every definition belongs to an **agency** (a department), which decides who may see it, run it, author it, and which credentials it runs under.

## Quick Start

### Prerequisites

- Go 1.26+ (CGO enabled — SQLite is compiled in)
- Node.js 20+ (frontend build only)

### Local Development

```bash
# Build the frontend (embedded into the binary) and the backend
cd frontend && npm ci && npm run build && cd ..
cd backend && make build

# Run with dev auth and demo data
CRONOMICON_DEV_AUTH=true CRONOMICON_DEV_SEED=true CRONOMICON_COOKIE_SECURE=false \
  CRONOMICON_DB_PATH=/tmp/cronomicon-dev.db ./bin/cronomicon

# Visit http://localhost:8080 → click "Developer login"
```

### Production Deployment

```bash
# From the repo root — the Dockerfile copies frontend/ and backend/
docker build -f backend/Dockerfile -t cronomicon:latest .
docker run -d -p 8080:8080 -v /var/lib/cronomicon:/var/lib/cronomicon \
  --env-file cronomicon.env cronomicon:latest
```

`docker-compose.yml` at the repo root is the reference stack (Cronomicon behind a reverse proxy with an OIDC provider). See [backend/README.md](backend/README.md) for every configuration option and [backend/deploy/](backend/deploy/) for the deployment guide.

## Architecture

Cronomicon is deployed as **a single static Go binary** (`T1/T3`) — one container, one process, one SQLite database (`/var/lib/cronomicon/cronomicon.db`). The frontend is embedded in the binary and served as static assets from `web/dist/`.

### Core Surfaces

| Path | Component | Status |
|------|-----------|--------|
| [`backend/`](backend/) | Go API server + embedded frontend | v2.0.0 — Stable (schema v1150, runner protocol v13) |
| [`frontend/`](frontend/) | React + TypeScript + Vite | v2.0.0 — 15 routed views |

### Key Features

#### Job Execution

- **Multi-executor support**: SSH (direct + bastion), Ansible, Terraform, Bash, PowerShell, Perl, Python
- **Distributed runners**: Autonomous `cronomicon-runner` agents with capability-based job routing
- **Self-service runner provisioning (v0.47.x)**: The server hosts the installer and distributes the agent binary; a one-click **Add Runner** flow issues **single-use registration tokens**, and runners self-register. Server-managed runner settings with **config-drift detection** and a one-click **Resync**, host-key scan-and-approve, runner **tags** + inline group membership, and a unified auth model (**one key + one trust store** for both Bash and Ansible, with an optional **Vault Agent sidecar** for runner-local secrets)
- **Ansible checkout projects (v0.46.0–v0.46.6)**: Runners execute full Ansible playbook **projects** (roles, `vars_files`, `.j2` templates) by checking out the playbooks repo at a **server-pinned commit SHA** — opt-in per job and per runner (`-allow-checkout` + a repo allowlist). Per-run `requirements.yml`/galaxy installs, Ansible Vault, requirement-token **claim gating** (`vault`, `collection:<fqcn>` route a run only to runners that satisfy it), and a per-run `systemd-run` **sandbox** with scoped child env. Body-only playbook runs are unchanged.
- **Scope-aware execution**: Jobs bound to named scopes (prod/staging/dev); since v0.56.4 both the **scope and the verb** are enforced on every execution route — a viewer cannot trigger or kill, and an operator holds the verbs only where their grant says
- **Target-host pinning honored everywhere (v0.53.0)**: A job pinned to a single `target_host` runs on that host on **every** path — manual trigger, cron fire, workflow step, and ansible (`--limit`) — where previously only manual triggers applied the pin. A pin that cannot be expressed as an ansible limit is refused loudly rather than silently widened
- **Declared run inputs (v0.38, reworked v0.51.4–.7)**: A Job or Script declares the values an operator supplies at run time; the Run dialog leads with them (readiness meter, provenance chips), and a job can enforce **Warn or Block** on a missing required input
- **Agency-based runner isolation**: Runners group into **agencies** (network-isolation zones) — since v0.52.1–.4 the **single membership axis** for scopes, secrets, variables, SSH keys, and runners; a job dispatches only to a runner that can reach its network (hard, disjoint, operator-governed)
- **Runner targeting (v1.3.0–v1.3.5)**: Within an agency, a job — or a single ad-hoc run — can be **pinned to runners carrying a given tag**, so work that must reach a particular network segment goes to a runner that sits on it instead of to whichever runner is free. Set in Git YAML (`spec.runner_tag`), in the Composer's **Run on** field, or per run in the Run dialog
- **Run-as agency credentials (v0.57.0–v0.57.6)**: One job body runs under **each department's own credential**. A binding declares the **name a reference arrives under**, so two agencies can each hold `BECOME_PASSWORD` in `prod` and a run resolves its own; the Ansible **become password** is supplied from a named Secret and is never an environment variable
- **Concurrency control**: Custom `concurrency_key` grouping of unrelated jobs under a shared concurrency gate (defaulting to a source-aware `source/name` key), with three policies — `Allow` (overlap), `Forbid` (lose the fire, recorded as skipped), and `Queue` (park and promote when the gate clears, capped at 3 per key). A per-trigger **priority** reorders the claim. `Replace` was removed in v0.57.29 because no code ever honoured it
- **Service-level expectations (v0.57.28)**: A job can declare `warn_after_seconds` ("this normally finishes in 20 minutes") and `must_finish_by` ("done before the 06:00 batch"), and Cronomicon notices a run that is **late** or that **never ran at all** — distinct from `timeout_seconds`, which kills it
- **Live log tailing (v1.4.16–v1.5.0)**: A running job's log follows itself in History — the SSH executor writes line-by-line, and runner agents flush pending lines every two seconds (wire protocol **v12**) instead of delivering the whole log at the end
- **Key-bound runs refuse the SSH executor (v1.5.35)**: A job that binds an SSH key credential only runs on a runner agent; if the run resolves to the in-app SSH executor it is refused with a stated reason rather than silently run without the key
- **Run history & tracing**: Full audit trail with trace IDs, timing, and outcome logging

#### Ways a run starts

Six producers can start a run, and each one records what it was:

| Trigger | What starts the run | Since |
|---|---|---|
| **Schedule** | A cron schedule fires — subject to activation windows, interval/once modes, and **working calendars** | core |
| **Manual** | An operator clicks **Run**, optionally deferring it to a chosen time | core |
| **Git push** | A GitLab webhook syncs definitions (it publishes changes, it does not run jobs) | core |
| **API token** | A machine principal (**service account**) calls the API with `Authorization: Bearer <token>` — no browser session required | v0.57.26 |
| **Reaction** | Another job or workflow **finished**, and something is watching for that | v0.57.13–.18 |
| **File arrival** | A file matching a watch pattern **appeared** on a target host | v0.57.31 |

- **Working calendars (v0.57.10–.12)**: A calendar (holidays, maintenance freezes, business days) **vetoes** a scheduled fire — it can only suppress a run, never cause one — and the suppression is recorded, so you can prove a job did not run on Christmas Day and say why
- **Reactions (v0.57.13–.18)**: Jobs and workflows react to each other's completions in all four pairings, without a workflow wrapping them. Stopping a run records a **disposition** (what you meant by stopping it) that reactors consume; chains carry a depth ceiling that holds across workflow step graphs, and every fire decision — including "decided to fire, could not" — lands in a delivery log
- **Service accounts (v0.57.26)**: Machine principals with their own tokens, permissions and audit identity. The plaintext token is shown once at creation and only its hash is stored — a lost token is reissued, never recovered
- **File-arrival triggers (v0.57.31)**: A job declares the paths it watches; a runner reports sightings, and a durable sighting record answers "the file landed — why did nothing happen?" when a gate refused the trigger
- **Deferred ad-hoc runs (v0.55.20)**: "Run this at 22:00 tonight" parks the run and promotes it when due, re-judging the concurrency gate and caps at promotion time

#### Workflow Orchestration

- **Step graphs**: Chain jobs serially, run groups in parallel, branch on a prior step's status/output (`type: branch`), nest a serial chain inside a parallel arm (`type: sequence`), or call another workflow as a step (`type: workflow`, v0.57.30) — all authorable directly in-app
- **Error resilience & retries (v0.35.0)**: Configure step-level retry limits, backoff periods, and continue-on-error behavior overrides
- **Soft-cancel (v0.35.0)**: Graceful cancellation of running workflow runs, skipping unstarted steps while allowing active jobs to finish cleanly
- **Stable run linkage (v0.35.0)**: Runs are indexed to workflow definitions by stable name + source rather than DB row ID, preserving links across workflow re-creations
- **Inter-job env passing (A12)**: a job emits `::cronomicon-output name=KEY::value` on stdout; a downstream step consumes it as injected env via an explicit `{fromStep, fromOutput}` reference
- **Sub-workflows (v0.57.30)**: a common sequence ("quiesce, snapshot, verify") is authored once and referenced as a step by every workflow that needs it, instead of copied into each
- **Unambiguous step targets (v1.2.2)**: since two departments may own a job of the same name, a step can name **which** one it means (by agency) rather than refusing to guess

#### Composable primitives & dual-source (v20)

- **First-class Scripts & Schedules**: reusable executable units (`scripts/`) and reusable named crons (`schedules/`) that many Jobs/Workflows reference (`script_ref` / `scheduleRefs`); each carries a blast-radius "used by" index
- **Git OR Cronomicon**: Jobs, Workflows, Schedules, and Scopes may live in Git **or** be authored in the DB in-app; Scripts stay Git-only; Env Vars live in Cronomicon or Vault. The runtime keys every definition by `(source, name)`, so a Git `backup` and an in-app `backup` never collide
- **In-app composition (A11)**: build a Job from a Script × Schedule(s) × Scope × env, or assemble a Workflow from job steps, entirely in the UI — no Git round-trip
- **Compose RBAC**: in-app authoring is gated by the `compose` permission — since v1.0.14 a **grantable, agency-bound** permission ("FIN composer — authors FIN's jobs and nothing else") rather than admin-only, checked **per job** against the scope it is leaving and the one it is arriving at. Global (no-agency) definitions and shared surfaces — reusable schedules, calendars, reactions, revision history and the recycle bin — remain admin-only by design. Git authoring is unaffected
- **Job-level env (v0.29.0)**: a Job declares an env map merged into **every** run, with precedence job-env → schedule env → per-run override
- **Operator-owned tags (v0.36.1–v0.36.5)**: User-authored tags on Scripts, Jobs, Workflows, and Schedules are SQLite-only, preserved during Git resyncs, and inline-editable by any logged-in user
- **Operator annotations (v1.2.4–v1.2.7)**: A job or workflow carries free-text **notes**, a **critical** flag, and a **contact** — who to call when it breaks. Like tags they live only in Cronomicon, survive Git syncs, and never overwrite the Git-owned `description`. **Critical** is a sortable column of its own (v1.4.0), and a failure notification names the contact and says whether the definition is critical
- **Revision history & recycle bin (v0.57.27)**: A Git-source definition has history, blame and undelete through Git; cronomicon-source Jobs, Workflows and Schedules now have the same — every edit records a revision you can view and **restore**, and a delete is recoverable from a recycle bin instead of gone

#### Catalog & script intelligence

- **Folder browsing (v0.30.0)**: Scripts, Jobs, Schedules, and Workflows nest into sub-folders of any depth and render as a navigable file tree; searching flattens to path-labelled results
- **Script lint + variable detection (v0.26.0 / v0.31.0)**: each synced script is scanned for body-lint warnings (CRLF, missing shebang, …) and the **environment variables it references** — surfaced as required/optional/already-provided chips in the Scripts catalog, the Run dialog, and the Job Composer so operators know what to define
- **On-demand body**: a script's resolved body is read from the synced clone on expand (path-traversal-guarded, 1 MiB display cap), never stored
- **Unified tables & tag filtering (v0.36.0–v0.36.4)**: Client-side Tag filter dropdown on all catalogs, resizable columns, and uniform row heights with "+N" tag overflow

#### Secrets & Access Control

- **Reference-based secret injection (v0.49.x)**: A job binds **reference names** — an env var, a stored secret, or a Vault path — instead of embedding plaintext. At dispatch time the resolver injects only the declared references into the run's environment (on both the SSH and runner paths), redacts them from logs, and records a one-time audit entry of exactly what was injected
- **HashiCorp Vault as a secret source (v0.49.x)**: Env vars and SSH credentials can resolve from Vault instead of the local encrypted store, configured under **Settings → Integrations → Vault** — authenticating via **AppRole or a static token** (token auth wired end-to-end in v0.52.29; switching methods requires re-entering the credential)
- **Security remediation (v0.51.x)**: Scope-scoped reads (out-of-scope rows 404/drop from listings), 8-hour sessions with **revocation on RBAC edits**, bastion host-key pinning, an SSRF egress guard on credentialed outbound calls, git-token argv-leak fix, and secret zeroization
- **KEK rotation you can finish (v1.5.30)**: `cronomicon rewrap-secrets` re-encrypts all three encrypted stores (secrets, SSH credentials, encrypted settings) under the current key; `--dry-run` counts rows per key version without decrypting. A world-readable `CRONOMICON_KEK_FILE` **refuses to boot**
- **The audit stream is masked (v1.5.34)**: One process-wide redaction dictionary — stored secrets, SSH credentials, encrypted settings, multi-line variables — is applied to every compliance audit row before it is written, and the same dictionary feeds the per-run log redactor so the two cannot drift
- **Env-var namespace references (v0.48.x)**: One env var or secret can derive its value from another through reserved reference prefixes, resolved when a run's environment is built
- **Departmental access control (v0.56.x, superseding the A5 matrix of v0.50.x)**: Access is decided by **Access Grants** — each grant pairs an AD group with a role and the **agency** (department) it applies to, or "All scopes" for unrestricted reach — so "Operator, but only for Tax" is one row. Roles are **editable data** (seven permissions; custom roles are rows, not code), a permission and a scope must come from the **same grant** (holding Operator in Finance grants nothing in Tax), and secrets, variables, SSH keys and runners are **departmentally owned**: writes and secret reveal require the verb on the owning agency, and entities in no agency are shared infrastructure only unrestricted operators may change. **No grants means no access**; recovery from a lockout is the offline `cronomicon grant-admin` subcommand. The legacy two-axis tables were dropped in v0.57.8 (schema 850)
- **Agency-first ownership (v1.0.13–v1.0.15)**: Every job now **states** its agency, and "All agencies (global)" is a deliberate, visible choice rather than what an unfilled field silently meant. Authoring became a grantable departmental permission (above), and a department can **administer its own access** — deciding who does its work — without a global admin
- **Two departments, one name (v1.1.0–v1.2.3)**: Jobs, workflows and schedules carry a **permanent id** independent of their name, so name uniqueness relaxes to per-agency: Finance and Tax may each own a `monthly-close`. Everything that used to identify a definition by name — run history, schedule entries, pauses, parked runs, reactions, credential bindings, concurrency gates, log-folder codes and alert targets — follows the id instead, and any screen where a name is ambiguous says which department it means. Runner protocol reached **v11**

#### Configuration & GitOps

- **Definitions in Git**: scripts, schedules, jobs, and workflows version-controlled in a GitLab repo, synced via webhook (Git wins on read for Git-source rows)
- **Pragma system**: inline metadata (executor, scope, types) in inventory files
- **Manual scope inventory**: host lists and capabilities defined in the Cronomicon UI
- **Git-synced scopes**: inventory synchronized from `inventory/*.ini` pragmas + sidecars

#### Scheduling

- **Cron builder**: Visual cron expression editor with human-readable output
- **Schedule → Git**: Publish cron jobs back to GitLab as commits
- **Concurrent-edit detection**: `baseShaRef` ensures safe collaborative edits
- **Scheduler integration**: Built-in daemon fires scheduled jobs on time, with fingerprint-aware DB checks for instant reloads on Cronomicon-managed changes
- **Activation windows & modes (v0.55.18–.19)**: A schedule can carry a start and end date, and run on a fixed **interval** or exactly **once** instead of a cron expression
- **Working calendars (v0.57.10–.12)**: Authored under **Schedules → Calendars**; a calendar vetoes fires on the days it names, and the veto is recorded on the job's timeline rather than passing silently
- **Upcoming**: a forward projection of what will fire and when, including **queued** runs waiting on a concurrency gate (shown as "waiting for the gate" — nobody knows when it will fire) and deferred ad-hoc runs, each cancellable from the list

#### Observability & Compliance

- **The Dashboard leads with its answer (v1.5.3–v1.5.5)**: A **verdict strip** at the top says whether anything needs attention (dismissible per attention set — it returns the moment a new failure appears), then the **Score** timeline — 24h back / 12h ahead, filled marks for runs, hollow for scheduled, coincident runs stacked as **chords**, every stat tile a link — then **Up next** (the next three fires) and **Recent errors** (folded per job, deep-linking into that job's History)
- **Activity feed (server-searched since v1.5.24)**: Real-time stream of runs, config changes, pushes, and syncs — searched and paged on the server, with a Kind filter, an **Actor** picker (users / runners / system), a **Range** select with custom From/To, and a runner name on every card that has one (v1.5.24–v1.5.29)
- **Audit trail**: Config change log **plus a compliance audit stream with real auth events** (login/logout/denial/CSRF, v0.52.11), exportable, with per-window retention that genuinely reaps files (v0.52.8)
- **Structured logging (v0.52.8–.12)**: A process log on disk (`cronomicon.log`) with a live re-pointable log directory, and run logs grouped into **per-entity folders** keyed by a stable entity code
- **S3 log archive (v1.5.33)**: Local disk stays the only write target during a run; a scheduled sweep copies sealed logs to any S3-compatible bucket (verify-by-size, budgeted, with an optional expiry window), History reads an archived log through the server when the local file is gone, and the local reaper never removes an unarchived log while the tier is on
- **Runner placement survives re-registration (v1.5.30)**: A runner that loses its identity re-registers into the general pool; its previous agency placement is snapshotted before the old row is deleted and the Runners view offers **Restore placement** — never automatic, because the name is self-declared
- **Sensitive data redaction**: **Unconditional** — injected references, stored secrets, and per-run overrides are masked in logs; there is no toggle
- **Metrics & alerting**: Prometheus scrape endpoint plus notification routing over **Email (SMTP) and Apprise** (which reaches Slack, Discord, webhooks, and more), with per-transport delivery status and a **Send test** button — configured under **Settings → Notifications**
- **Alerts that say who to call (v1.2.7)**: A failure notification for an annotated job or workflow names its **contact** and says so when the definition is **critical**, so the message reaching a pager carries the ownership the catalog already knows
- **Late and missed runs (v0.57.28)**: Separate from a kill — a run past its `warn_after_seconds`, a run that will miss `must_finish_by`, and a scheduled run that never happened are each noticed and reported

## How It Works — Composable Primitives & Dual-Source

The app flow is built from five primitives that compose:

```text
   BROWSE                       COMPOSE                       ORCHESTRATE
 ┌─────────┐  ┌───────────┐
 │ Scripts │  │ Schedules │ ─┐
 └─────────┘  └───────────┘  ├──►  ┌──────┐  ─ order + logic ─►  ┌───────────┐
 ┌──────────┐                ├──►  │ Jobs │  ─ pass Env Vars ─►  │ Workflows │
 │ Env Vars │ ───────────────┘     └──────┘                      └───────────┘
 └──────────┘

 Script   = the executable body (code)      Job      = Script + Schedule(s) + Env Vars + Scope
 Schedule = a named cron (+ optional env)    Workflow = ordered Jobs with branching logic
 Env Var  = a name/value (secret or not)                and Env Vars passed between Jobs
```

**Two authoring paths, one runtime.** Every Job/Workflow/Schedule carries a `source` and the
runtime keys it by `(source, name)`:

| Primitive | Git (GitLab) | Cronomicon (in-app) | Vault | How you author it |
|---|:--:|:--:|:--:|---|
| **Scripts** | ✅ only | — | — | Git MR (read-only catalog in the UI) |
| **Schedules** | ✅ | ✅ | — | Git YAML, or the in-app composer |
| **Jobs** | ✅ | ✅ | — | Git YAML, or **Compose** (Script × Schedule × Scope × env) |
| **Workflows** | ✅ | ✅ | — | Git YAML, or the **Workflow Editor** (ordered job steps) |
| **Scopes** | ✅ | ✅ | — | Git inventory, or the **Scopes** page |
| **Env Vars** | — | ✅ | ✅ | Settings (DB) or a Vault reference |

- **Git-source** definitions are parsed from the GitLab clone on sync and are **read-only** over the
  API — edits round-trip through the Git publish flow (Git wins on read).
- **Cronomicon-source** definitions are operator-composed in-app and edited via the write API; they are
  their own source of truth and never touched by Git sync.
- The two namespaces are **disjoint** — a Git `backup` and an in-app `backup` are different
  definitions, each badged by source in the UI.
- **Operator-owned tags** on all four definition types (Scripts, Jobs, Schedules, Workflows) bypass the Git-read-only rule: they are authored directly in SQLite via inline editor controls and are preserved across Git resyncs.

### UI surfaces (15 views)

The sidebar groups the primitives by what you're doing:

| View | Path | What it does |
|---|---|---|
| **Scripts** | `/scripts` | Browse the Git script catalog + "used by" index (read-only) |
| **Schedules** | `/schedules` | Browse first-class reusable cron schedules + "used by" index, plus the **Calendars**, **Reactions**, and **Upcoming** tabs |
| **Schedule Builder** | `/schedule-builder` | Author an in-app **cronomicon-source Schedule** with a visual cron helper and env variables |
| **Compose** | `/compose` | Build an **cronomicon-source Job** from a Script + Schedule(s) + Scope + env |
| **Workflow Editor** | `/workflow-editor` | Assemble an **cronomicon-source Workflow** on a graph canvas |
| **Jobs** / **Workflows** | `/jobs` · `/workflows` | List, run, pause/resume, and trigger (both Git- and Cronomicon-source), with the **Run dialog**, operator annotations, revision history and the recycle bin |
| **Scopes** | `/scopes` | Manage execution scopes (hosts/inventory, run-type capabilities, git-vs-cronomicon source) + the **Agencies** catalog (network-isolation zones) |
| **Publish to GitLab** | `Jobs → Publish` | The publish builder that commits a job's schedule back to GitLab (the old standalone `/schedule` hub is dissolved; the route redirects to `/schedules`) |
| Dashboard · Activity · History · Env Vars · Runners · Settings | — | The **Score** timeline + Current status, audit, run history, variables/secrets/SSH keys, runner fleet, config (incl. **Service Accounts** and **Users & Access**) |

Two conveniences apply across the catalogs:

- **Columns menu (v1.4.0–v1.4.5)**: every catalog table lets you reorder and hide columns, remembered per table — including **Critical**, the annotation column added in v1.4.0. The two History tables stay server-sorted; only order and visibility are client-side.
- **The Run dialog (v0.55.5, reworked through v1.5.32)**: five top-level sections (Inputs, Targets, Method, Timing, Advanced), a one-line recap of what the run will do, and — at 1200px and wider — a fixed **"This run"** rail that states *every* answer and setting the run will use, highlighting the ones you changed and naming the default each replaced. Every ad-hoc run passes through a confirmation window that restates the declared inputs and your deviations.
- **Refresh on every expanded panel (v1.4.12–v1.4.14)**: each expanded row on Jobs, Workflows, Runners, Scopes, Git Sync and the Env Vars tabs carries a Refresh button that re-fetches everything the panel shows without collapsing it; the top-bar control is now "Reload page".

> [!NOTE]
> **Compose** and the **Workflow Editor** are gated by the `compose` permission, which since v1.0.14 can be granted to a
> department rather than only to a global admin — the holder authors within the agencies their grant covers. The
> **Schedule Builder**, **Calendars**, **Reactions**, revision history and the recycle bin act on objects belonging to no
> single agency and remain admin-only. Users without the permission see a notice instead of the form.

## Walkthrough — from a Git script to a Job to a Workflow

New to Cronomicon? Start here. This is the guided, end-to-end tour of the most common task:
**take a script that lives in your Git config repo, turn it into a scheduled Job, then chain
several Jobs into a Workflow.** The two reference sections that follow it
([GitOps YAML](#authoring-path-1--gitops-yaml) and [in-app Compose](#authoring-path-2--in-app-compose))
document every field — this section just shows the path through them.

**The mental model.** You build up in three layers, each one wrapping the one below:

```text
 Script  ──►  Job  ──►  Workflow
 what runs    where + when it runs    the order several Jobs run in
```

- A **Script** is the code (a Bash/Ansible/Terraform/PowerShell/Perl/Python body). **Scripts always live in Git.**
- A **Job** binds one Script to a **Scope** (where) and one or more **Schedules** (when), plus options.
- A **Workflow** runs several Jobs in a defined order, with optional parallel groups, branches, and data passed between steps.

Jobs and Workflows can live in **Git** *or* be built **in the app** (the dual-source model). We'll
follow the Git path the whole way — "from the Git folder" — and call out the in-app shortcut at
each step.

> [!NOTE]
> **What you need:** a clone of the Cronomicon **config repo** (the GitLab repo Cronomicon syncs — the one
> with the `scripts/`, `jobs/`, `workflows/`, `schedules/`, `inventory/` folders); the `cronomicon`
> binary on your `PATH` for local validation; and a login. The **in-app** shortcuts additionally
> need the **compose** permission (grantable per department since v1.0.14, or held globally by an admin).

### Step 1 — Drop your script into `scripts/`

You have two ways to add a script, and you can mix them freely in the same repo:

**a) Raw file (simplest).** Just commit the script itself — Cronomicon auto-discovers it on the next sync,
infers the run type from the file extension, and registers it. Supported extensions:

| File you commit | Becomes run type |
|---|---|
| `scripts/backup-db.sh` | `bash` |
| `scripts/plan.tf` | `terraform` |
| `scripts/rotate.ps1` | `powershell` |
| `scripts/report.pl` | `perl` |
| `scripts/site.yml` *(a plain playbook, no `kind: Script` header)* | `ansible` |

> [!IMPORTANT]
> A raw script's **registered name keeps its extension** — `scripts/backup-db.sh` registers as
> `backup-db.sh`, not `backup-db`. That is deliberate: it lets `backup.sh` and `backup.tf` coexist
> without colliding. Whatever the name ends up being is exactly what a Job's `script_ref` must say
> (see Step 4). Sub-folders are fine too — `scripts/db/backup.sh` registers as `db/backup.sh`.

**b) YAML wrapper (explicit control).** Write a `kind: Script` file when you want to pin the executor,
use an inline multi-line body, or point at a file elsewhere in the repo. A wrapper's name is its
`metadata.name` (no extension):

```yaml
# scripts/backup-db.yaml  → registers as "backup-db"
apiVersion: cronomicon.io/v1
kind: Script
metadata:
  name: backup-db
spec:
  run_type: bash
  executor: runner
  command: pg_dump -h db-01 -U postgres app_db > /backups/app_db.sql
```

See [Scripts (`scripts/`)](#1-scripts-scripts) below for inline-body and file-reference variants.

### Step 2 — Validate locally before you push

Catch mistakes on your laptop instead of at sync time:

```bash
cronomicon validate /path/to/config-repo
```

This parses every YAML file, confirms every cross-reference resolves (a `script_ref` or `scheduleRefs`
that points at nothing is a **hard error**), and **warns** on orphan scripts — a script registered but
not yet used by any Job, which is fine while you're still wiring things up. Wire `cronomicon validate`
into your CI so a bad definition can never merge.

### Step 3 — Commit, push, and let Cronomicon sync

Commit and push (open a merge request if your repo protects `main`). Cronomicon picks the change up one
of three ways:

- a **GitLab webhook** fires automatically on push (`POST /api/v1/webhooks/gitlab`), **or**
- an operator clicks **Resync** in **Settings → GitLab**, **or**
- you call `POST /api/v1/git/sync`.

For anything defined in Git, **Git is the source of truth** ("Git wins on read"). Confirm the result:
open the **Scripts** view in the sidebar — your script is now in the read-only catalog with its run
type and a *used-by* count of `0`.

### Step 4 — Wrap the script in a Job

A Job answers **where** the script runs (a **Scope**, e.g. `Production`, or a single `target_host`)
and **when** (one or more Schedules), plus options like timeout, retries, and concurrency.

**Git path** — add a job file and reference the script by its **exact registered name**:

```yaml
# jobs/nightly-db-backup.yaml
apiVersion: cronomicon.io/v1
kind: Job
metadata:
  name: nightly-db-backup
spec:
  script_ref: backup-db.sh     # ← exact script name, incl. extension for a raw script
  scope: Production            # where it runs
  schedules:
    - name: nightly
      cron: "0 2 * * *"        # when it runs
  timeout_seconds: 3600
  retries: 2
```

Run `cronomicon validate` again, push, and sync. The Job appears in the **Jobs** view.

**In-app shortcut (needs `compose`)** — click **Jobs → + Create** to open **Compose**: pick the Script from a
dropdown, choose a Scope, add Schedule(s) (either inline or by referencing a first-class schedule
authored in Git or in-app via the **Schedule Builder**), set options, and save. It goes live immediately with no Git
round-trip and is badged *cronomicon*-source. (Compose **reuses** a Git Script — it never authors the
script itself; scripts are always Git.)

> [!NOTE]
> **Newcomer traps in this step:**
>
> - `script_ref` must match the registered name **character-for-character** — including the `.sh`/`.tf`
>   extension for a raw script. A typo'd or dangling `script_ref` drops the whole Job at sync (hence
>   Step 2).
> - Pick a **real Scope**. Scopes come from your Git `inventory/` or from the **Scopes** page.
>   The scope is **not** checked when you save — a bad one only fails at *run* time.
> - A composed job can reference a first-class Schedule. If you edit that Schedule in the **Schedule Builder**, the changes automatically propagate to all referencing Jobs/Workflows, reloading the scheduler immediately.

### Step 5 — Run it once to confirm

Open **Jobs**, find your job, and hit **Run** for a manual trigger (`POST /api/v1/jobs/{jobId}/run`) —
or just wait for its schedule to fire. Watch it land in **History** and **Activity** with a trace ID,
timing, and full log output.

### Step 6 — Chain Jobs into a Workflow

A Workflow runs Jobs in a defined order. Steps run in sequence by default; a `type: parallel` step runs
a group at once, and a `type: branch` step takes a different path based on a prior step's status or
output.

**Git path** — list the jobs as ordered steps (reference each Job by name):

```yaml
# workflows/prod-release.yaml
apiVersion: cronomicon.io/v1
kind: Workflow
metadata:
  name: prod-release
spec:
  enabled: true
  steps:
    - type: job
      name: terraform-plan-prod
      label: "Plan"
    - type: job
      name: terraform-apply-prod
      label: "Apply"
```

**In-app shortcut (needs `compose`)** — **Workflows → + Create** opens the **Workflow Editor**: add jobs as
steps (the picker badges each job *git* or *cronomicon*), optionally attach schedules, and save.
The editor supports building complex graph structures, including parallel groups, branching logic based on step statuses or outputs, and per-step configurations like retries, backoff, and continue-on-error.

**Passing data between steps (optional).** A job prints a marker on stdout, and a later step consumes
it as an injected env var:

```bash
# inside the upstream job's script:
echo "::cronomicon-output name=ARTIFACT::app-1.2.3"
```

```yaml
steps:
  - type: job
    name: build              # emits ARTIFACT
  - type: job
    name: deploy
    inputs:
      DEPLOY_ART:            # becomes $DEPLOY_ART in the deploy job
        fromStep: build
        fromOutput: ARTIFACT
```

> Output names must be **env-var style** — letters, digits, and underscores only (no hyphens). A
> missing or failed upstream output resolves to an empty value, deterministically. Full details in
> [Workflows (`workflows/`)](#4-workflows-workflows) below.

### Step 7 — Trigger the workflow

Open **Workflows** and hit **Trigger** (`POST /api/v1/workflows/{workflowId}/trigger`), or attach a
schedule so it fires on cron. The run gets its own workflow trace ID, and each child job run links back
to it in **History**.
While a workflow run is executing, you can watch its status update live, inspect individual step logs or redacted resolved contexts, and cancel the run if needed using the **Cancel** button.

### Which path should I use?

| | **GitOps (the Git folder)** | **In-app (Compose / editors)** |
|---|---|---|
| **Good for** | reviewed, versioned, auditable definitions — the canonical path | quick changes with no Git round-trip |
| **Covers** | Scripts, Jobs, Workflows, Schedules, Scopes | Jobs, Workflows, Schedules, Scopes — **Scripts are always Git** |
| **How you change it** | edit YAML → `cronomicon validate` → MR → sync | fill in a form → save (live immediately) |
| **Who** | anyone who can push to the repo | anyone holding **`compose`** for that department (admins everywhere) |
| **Editing the other source** | — | a Git-defined row is **read-only** in the app (the API returns `409`); edit it in Git |

Both paths run through the exact same scheduler and executor — only where the definition is *stored*
differs. A Git `backup` and an in-app `backup` are two distinct definitions (keyed by `source` + name)
and never collide.

## Authoring path 1 — GitOps (YAML)

Cronomicon supports a declarative GitOps model: scripts, schedules, jobs, and workflows are defined in
a Git repository (like GitLab) and synchronized automatically by the Cronomicon daemon. This is the
canonical path for reviewed, version-controlled definitions. (The in-app path is below.)

### Repository Structure

Your configuration repository should organize files into the following folders:

```text
.
├── scripts/       # Reusable script and command definitions (kind: Script)
│   └── *.yaml
├── schedules/     # Reusable named cron schedules (kind: Schedule, A10a)
│   └── *.yaml
├── jobs/          # Job targets (scope, script_ref, schedules / scheduleRefs)
│   └── *.yaml
├── workflows/     # Ordered/parallel/branching job pipelines
│   └── *.yaml
└── inventory/     # Scope inventories (*.ini + optional .cronomicon.yaml sidecars)
```

---

### 1. Scripts (`scripts/`)

Scripts are first-class reusable code primitives. They define *what* code to execute and *how* to run it, but do not contain scheduling or targeting (scope) information. On sync each script body is scanned for body-lint **warnings** and the **environment variables it references** (advisory; surfaced in the catalog and the run/compose env editors) — see *Catalog & script intelligence* above.

Example script definition (`scripts/backup-db.yaml`):

```yaml
apiVersion: cronomicon.io/v1
kind: Script
metadata:
  name: backup-db
spec:
  # Supported run types: bash, ansible, terraform, powershell, perl, python
  run_type: bash
  # The default executor for this script (ssh or runner)
  executor: runner
  # Provide one of: command (string), script (multi-line inline script), or scriptPath
  command: pg_dump -h db-01 -U postgres app_db > /backups/app_db.sql
```

Other ways to define execution bodies in scripts:

- **Inline script**:

    ```yaml
    spec:
      run_type: bash
      script: |
        echo "Starting maintenance..."
        service nginx stop
        # do updates...
        service nginx start
    ```

- **File reference** (relative path to a script file in the repo):

    ```yaml
    spec:
      run_type: perl
      scriptPath: scripts/bin/report_generator.pl
    ```

---

### 2. Schedules (`schedules/`)

Schedules are first-class, reusable named crons (A10a). A job or workflow references one or more by
name via `scheduleRefs`, so a single schedule (e.g. `nightly`) can drive many definitions — the
schedule analog of a Script. Inline `schedules:` on a job/workflow still work; `scheduleRefs` is the
reusable alternative.

Example schedule definition (`schedules/nightly.yaml`):

```yaml
apiVersion: cronomicon.io/v1
kind: Schedule
metadata:
  name: nightly
spec:
  cron: "0 2 * * *"
  # Optional plaintext env injected when this schedule fires.
  env:
    TIER: prod
```

A job then references it instead of defining the cron inline:

```yaml
spec:
  script_ref: backup-db
  scope: Production
  scheduleRefs: [nightly]      # resolved into the runtime schedule at sync
```

---

### 3. Jobs (`jobs/`)

Jobs bind a specific script reference (`script_ref`) to a runtime target environment (`scope` or individual `target_host`), scheduling rules (inline `schedules:` and/or `scheduleRefs`), and environmental configurations.

Example job definition (`jobs/nightly-db-backup.yaml`):

```yaml
apiVersion: cronomicon.io/v1
kind: Job
metadata:
  name: nightly-db-backup
spec:
  # References the script metadata.name
  script_ref: backup-db
  
  # Target group of hosts configured in Cronomicon (e.g. Production, Staging)
  scope: Production
  
  # (Optional) Pin execution to a single host in the scope.
  # Honored on EVERY run path since v0.53.0 — manual, scheduled, workflow, ansible.
  # (Before v0.53.0 only manual triggers applied it; the rest fanned out.)
  target_host: db-01
  
  # Configure multiple independent schedules
  schedules:
    - name: nightly
      cron: "0 2 * * *"
    - name: verify-4h
      cron: "0 */4 * * *"
      # Schedule-specific environment overrides
      env:
        VERIFY: "true"
        STAGE: "prod"
        
  # (Optional) Send this job only to runners carrying this tag (RT, v1.3.1).
  # A per-run pin in the Run dialog overrides it for that run.
  runner_tag: dmz

  # Optional timeout and retry options
  timeout_seconds: 3600
  retries: 2

  # (Optional) Service-level expectations (v0.57.28). These NOTICE, they do not kill —
  # timeout_seconds is what kills. warn_after_seconds flags a run still going after
  # 20 minutes; must_finish_by is wall-clock 'HH:MM' in the application timezone.
  warn_after_seconds: 1200
  must_finish_by: "06:00"

  # (Optional) Start this job when a file arrives (v0.57.31). stable_seconds waits
  # for the file to stop growing before firing, so a partial upload does not trigger.
  watch:
    - path: /incoming/ledger-*.csv
      stable_seconds: 30

  # (Optional) Concurrency policy (Allow, Forbid, Queue; defaults to Allow).
  #   Allow  — overlap freely
  #   Forbid — lose the fire, recorded as a skipped run
  #   Queue  — park the fire and promote it when the gate clears (max 3 per key)
  concurrency_policy: Queue
  # (Optional) Custom concurrency key for grouping jobs (defaults to source/name)
  concurrency_key: database-operations
```

> [!NOTE]
> Backward compatibility is maintained for single schedule strings using: `schedule: "0 2 * * *"`. However, using the `schedules:` list allows you to specify named schedules and environment variables that are injected only during that schedule's run.

> [!IMPORTANT]
> `Replace` is no longer a valid `concurrency_policy` — it was parsed and stored but never honoured by any code path, so a
> job set to the strictest-sounding value overlapped freely. It was removed in v0.57.29 and existing rows were coerced to
> `Allow` (which is how they had always actually behaved). An unrecognised policy is now a hard error (422) rather than a
> silent downgrade.

---

### 4. Workflows (`workflows/`)

Workflows string together multiple jobs into serial, parallel, and/or branching pipelines, and can pass data between steps (A12).

There are five step types:

| `type:` | What it does |
|---|---|
| `job` | Run one job |
| `parallel` | Run a group of steps at once |
| `branch` | Take a different path based on a prior step's status or output |
| `sequence` | A serial chain **inside** a parallel arm (v0.55.17) |
| `workflow` | Call another workflow as a step (v0.57.30) — author a common sequence once and reuse it |

Example workflow definition (`workflows/prod-release.yaml`):

```yaml
apiVersion: cronomicon.io/v1
kind: Workflow
metadata:
  name: prod-release
spec:
  description: "Plan, apply, and verify the production release"
  enabled: true
  
  # Workflows can be triggered manually or scheduled
  schedules:
    - name: daily-midnight
      cron: "0 0 * * *"
      
  # Steps run in sequence. Use type: parallel to run steps simultaneously.
  steps:
    - type: job
      name: terraform-plan-prod
      label: "Plan"
    - type: job
      name: terraform-apply-prod
      label: "Apply"
    - type: parallel
      jobs:
        - type: job
          name: disk-usage-audit
          label: "Verify Disk"
        - type: job
          name: cert-renewal
          label: "Renew Certs"
```

**Passing data between steps (A12).** A job emits an output on stdout with a marker line, and a
later step consumes it as an injected env var via an explicit `{fromStep, fromOutput}` reference:

```bash
# inside the 'build' job's script:
echo "::cronomicon-output name=ARTIFACT::app-1.2.3"
```

```yaml
steps:
  - type: job
    name: build              # emits ARTIFACT
  - type: job
    name: deploy
    inputs:                  # consume it as $DEPLOY_ART
      DEPLOY_ART:
        fromStep: build
        fromOutput: ARTIFACT
```

Outputs are captured per step (a missing/failed upstream resolves to an empty value,
deterministically) and merged over the workflow env with inputs taking precedence. A branch step's
`condition` can also match on a prior step's output (`type: output_match`).

---

### Repo Validation and Sync

Before committing and pushing changes to Git, you should validate your repository structure locally using the Cronomicon CLI. This catches configuration errors, syntax issues, and dangling references before they hit production.

Run validation:

```bash
cronomicon validate /path/to/your/repo
```

This checks:

1. **Schema Validation**: Ensures all YAML structures conform to the API specification.
2. **Dangling Script References**: If a job references a `script_ref` that does not exist in the `scripts/` directory, validation returns a hard error.
3. **Orphaned Scripts**: If a script exists but is not referenced by any jobs, validation returns a warning (though it is not a fatal error).

## Authoring path 2 — In-app (Compose)

Operators holding the **`compose`** permission for the target department can create definitions directly in the
UI — no Git round-trip. These land as **cronomicon-source** rows that run through the exact same
scheduler/executor seam as Git-defined ones; only the origin differs.

- **Compose** (`/compose` / `JobComposer.tsx`) → builds an cronomicon-source **Job** by binding a Git **Script**
  (`scriptRef`) to a **Scope**, one or more **Schedules** (refs or inline), and execution options.
  The referenced script's run-type/body/executor are denormalized onto the job at write time.
- **Workflow Editor** (`/workflow-editor` / `WorkflowEditor.tsx`) → assembles an cronomicon-source **Workflow** on an
  interactive **graph canvas** (React Flow): ordered job steps, parallel groups, and **nested** branch arms, with
  A12 inter-job data passing drawn as edges (steps may target Git- or Cronomicon-source jobs via the A11 source
  precedence). A **Simple ｜ Advanced** toggle keeps the linear list editor for flat chains; hand-arranged node
  positions persist per workflow.
- **Schedule Builder** (`/schedule-builder` / `ScheduleBuilder.tsx`) → authors an cronomicon-source **Schedule**
  (reusable named cron + optional env variables) directly in the UI. When an cronomicon-source schedule is edited, the changes automatically propagate to all referencing Jobs/Workflows, reloading the scheduler immediately.
- **Schedules** (`/schedules` / `Schedules.tsx`) → browse the reusable schedule catalog (Git + cronomicon) and the
  jobs/workflows that reference each. Admins can click **+ New schedule** to launch the Schedule Builder, or edit/delete cronomicon-source rows directly.

Mechanics:

- Backed by `POST/PUT/DELETE /api/v1/jobs`, `/api/v1/workflows`, and `/api/v1/schedule-defs` (session + CSRF + admin role).
  Mutating a **Git-source** definition through these returns **409** — Git rows are read-only in-app.
- Writes validate references (unknown `scriptRef`/step job → `422`; deleting a schedule referenced by jobs/workflows returns `409` unless `?force=true` is supplied to cascade and reload), de-duplicate names within the
  cronomicon namespace (`409`), write a Change Log + activity entry, and immediately reload the
  scheduler so a composed schedule fires without waiting for the next sync.
- The SPA shows/hides the composer via `GET /api/v1/capabilities` (`compose` flag).

> [!NOTE]
> v20 shipped the core in-app authoring loop. The **visual workflow canvas** (v0.44.0) closes the largest follow-on:
> branch/parallel/**nested** authoring and drawn A12 data edges now ship in the Workflow Editor (it is
> no longer a linear-only list). Remaining conveniences: History rendering of captured step outputs
> and a unified Env Var picker.

## Documentation

### For Operators & Developers

- **[User Manual](documentation/user-manual.html)** — the full per-view reference and task recipes for operators and authors, served in-app from the header **Help** button
- **[Administrator Manual](documentation/administrator-manual.html)** — service operations: bootstrap, execution model, GitOps, secrets, runner fleet, deployment & day-2 (reconciled to v1.5.45)
- **Training courses (v1.5.7–v1.5.11)** — an operator course (`/training-operator.html`, ten modules, framed for teams migrating from a legacy job scheduler) and an administrator course (`/training-admin.html`, seven modules); both ship in the binary, open in a slide **deck mode** by default, and degrade to a scrolling page without JavaScript
- **Usage guides** — per-run-type guides for Bash, Ansible, PowerShell and Python, plus the runner **Install**, **Manage** and **Security** guides; the header **Help** menu and contextual links in each screen open the relevant page (v1.5.13–v1.5.15)
- **[Deployment Guide](backend/deploy/deployment-guide.md)** — Reverse proxy + OIDC stack, environment configuration, health checks, storage layout, backup & restore
- **[Security Hardening](backend/deploy/)** — OIDC / trusted-header SSO, CSRF, runner auth, secret management
- **[Changelog](CHANGELOG.md)** — Full version history and feature release notes

### For Contributors

- **[AGENTS.md](AGENTS.md)** — Agent context & development guide: repository map, current state, backend/frontend structure, key contracts, and dev workflow (the canonical entry point for AI agents and new contributors)
- **Architecture decisions** — the ratified decisions (A1–A13, T1–T13, v20, and each feature band's decision record) are summarised in the **Key Contracts** section of [`AGENTS.md`](AGENTS.md); the changelog records the reasoning behind each release.
- **Package boundaries and views** — the backend package table and the frontend views table in [`AGENTS.md`](AGENTS.md)

### For Designers

- **[Frontend README](frontend/README.md)** — Build setup, Vite config, component organization
- **[Design System](#design-system)** — Color tokens, typography, spacing

## API & Specification

The **canonical API contract** lives in [`openapi.yaml`](openapi.yaml):

```bash
# Generate the TypeScript client from the spec (frontend)
cd frontend && npm run gen
```

> Backend handlers are hand-written against `openapi.yaml` with conformance tests; there is no Go
> codegen step. The two spec copies (`backend/openapi.yaml` + repo-root `openapi.yaml`) must
> stay byte-identical — `TestOpenAPIMirrorByteIdentical` enforces it.

### Key Endpoints

All under the `/api/v1` base prefix. 🆕 = added in v20.

Requests authenticate either with a **browser session + CSRF token**, or — since v0.57.26 — with a
**service-account token** (`Authorization: Bearer <token>`), which carries its own permissions and
audit identity and needs no CSRF header.

| Method & Path | Purpose | Notes |
|---|---|---|
| `GET /jobs` · `GET /jobs/{jobId}` | List / get jobs (both sources) | session |
| `POST /jobs/{jobId}/run` | Trigger a manual run | session + CSRF |
| `POST /jobs/{jobId}/pause` · `/resume` · `/kill` | Pause / resume schedule, kill a run | session + CSRF |
| `PUT /job-tags/{jobId}` | Full-replace a job's tags | session + CSRF |
| 🆕 `POST /jobs` · `PUT /jobs/{jobId}` · `DELETE /jobs/{jobId}` | Compose / edit / delete an **cronomicon-source** job | `compose`, checked against the job's scope; `409` on git rows |
| `GET /workflows` · `POST /workflows/{workflowId}/trigger` | List / trigger workflows | session (+CSRF on trigger) |
| `PUT /workflow-tags/{workflowId}` | Full-replace a workflow's tags | session + CSRF |
| `POST /workflows/runs/{traceId}/cancel` | Cancel a running workflow | session + CSRF |
| `POST /workflows/validate` | Dry-run validation of workflow steps | session + CSRF |
| `PATCH /workflows/{workflowId}` | Pause / resume a workflow | session + CSRF + scope guard (v0.52.37) |
| 🆕 `POST /workflows` · `PUT /workflows/{workflowId}` · `DELETE /workflows/{workflowId}` | Compose / edit / delete an **cronomicon-source** workflow | `compose` on every job in the graph; `409` on git rows |
| `GET /scripts` · `GET /scripts/{name}` | Read-only Git script catalog + `usedBy` | session |
| `PUT /schedule-tags/{name}` | Full-replace a schedule's tags | session + CSRF; has `?source` parameter |
| 🆕 `GET /schedule-defs` · `GET /schedule-defs/{name}` | First-class schedule catalog + `usedBy` | session |
| `GET /schedules` · `GET /schedules/upcoming` | Per-binding schedule inventory + upcoming fires | session |
| `GET /env-vars` · `GET /env-secrets` (+ write ops) | Env vars / secrets (values never returned) | writes session + CSRF |
| `PUT /job-annotation/{jobId}` · `PUT /workflow-annotation/{workflowId}` | Set notes / criticality / contact | session + CSRF (any authenticated user) |
| `GET /reactions` · `PUT/DELETE /reactions/{ownerKind}/{ownerName}[/{name}]` | Read / author what reacts to what | reads session; writes admin (shared surface) |
| `GET /calendars` · `POST /calendars` · `PUT/DELETE /calendars/{name}` · `PUT …/days` | Working calendars and the dates they name | reads session; writes admin (shared surface) |
| `GET /definitions/{kind}/{name}/revisions` · `POST …/{no}/restore` | Revision history for cronomicon-source definitions | admin |
| `GET /recycle-bin` · `POST /recycle-bin/{kind}/{name}/restore` · `DELETE /recycle-bin/{kind}/{name}` | Restore or purge a deleted definition | admin |
| `GET/POST /service-accounts` · `DELETE /service-accounts/{id}` | List / mint / revoke machine principals; the token plaintext is returned **once**, on create | `manageRoles` |
| `POST /trigger/jobs/{name}` · `POST /trigger/workflows/{name}` | Start a run as a machine principal | service-account token (Bearer), no session |
| `PUT /runner-tags/{runnerId}` | Set a runner's tags (what a job's `runner_tag` matches) | session + CSRF |
| `GET /runs/{traceId}/log?offset=` | Read a run log, or tail it from an offset while the run is active (`X-Log-Offset`) | session |
| `GET /activity` · `GET /activity/actors` | Server-searched activity feed (`q`, `kind`, `actor`, `runner`, `from`, `to`) and its actor list | session |
| `GET /capabilities` | The caller's permission flags, incl. `compose` | drives SPA gating (with per-row authority flags) |
| `POST /git/sync` · `POST /webhooks/gitlab` | Trigger sync / receive GitLab push | webhook uses `X-Gitlab-Token` |
| `GET /runs/{traceId}/manifest` · `POST /runners/{id}/poll` | Runner manifest / poll for work | Runner-only (Bearer) |

See [`openapi.yaml`](openapi.yaml) for the full v1 surface. Note: `GET /capabilities` is mounted in
code (not in the spec); contract changes must update both `openapi.yaml` copies (guarded by
`TestOpenAPIMirrorByteIdentical`).

## Design System

Rebuilt in the eleven-phase visual update (v0.52.13–v0.52.25). All tokens live in [`frontend/src/theme.ts`](frontend/src/theme.ts) — components read the mutable `c.*` object at render time (never freeze tokens in module-level consts, or the Light/Dark toggle silently breaks).

### Colors

- **Dark theme**: `#0a1119` (bg), `#e8eff7` (text), `#4da3d9` (primary), `#c9a227` (accent — **gold is the brand**)
- **Light theme**: `#f4f6fa` (bg), white panels, with darker counterpart tokens
- **Status**: `#45b26b` (success), `#e05a5a` (danger), `#d98a2b` (**warning is orange** — gold no longer means warn)
- **Contrast is a gate**: every text/background and control-boundary pair is held to WCAG AA (4.5:1 body, 3:1 controls); `c.onSolid` exists because white-on-solid fails in dark mode

### Typography

- **Faces**: IBM Plex Sans (body/titles), Plex Sans Condensed (uppercase structural type), Plex Mono with `tabular-nums` (cron, trace IDs, timestamps) — **self-hosted** under `frontend/public/fonts/`, never a font CDN
- **Six-token scale**: `fontDisplay` 36 · `fontTitle` 24 · `fontHead` 16 · `fontBody` 14 · `fontSm` 13 · `fontXs` 11 — no literals, and 10px is deliberately gone

### Shape & spacing

- **Three radii, only three**: `radiusChip` 4 (chips, buttons, inputs) · `radiusSurface` 8 (cards, panels, modals) · `radiusPill` 999 (**status pills only**); a test fails on any numeric `borderRadius` literal
- **A surface gets a border OR a shadow, never both** (floating overlays excepted)
- **Grid**: 8px base unit

## State Management (Frontend)

The production frontend uses **no global store or external state library** (no Redux/Zustand/React Query). State is split into two layers:

**App-wide concerns → three React contexts** (provided in `main.tsx` / `App.tsx`):

| Context | Provider · hook | Holds |
|---|---|---|
| `auth.tsx` | `AuthProvider` · `useAuth()` | Current user (`Me`), loading state. Capability gating reads the server's `/capabilities` flags plus per-row `canRun`/`canKill` (RB-24); the old client-side `canTriggerJobs(me)` helper is deprecated — a hardcoded role list is wrong the moment custom roles exist |
| `theme-context.tsx` | `ThemeProvider` | Light/dark mode |
| `timezone-context.tsx` | `TimezoneProvider` | Application timezone for rendering timestamps |

**Server data → fetched per-view via shared hooks** (`hooks.ts`), against the typed `openapi-fetch` client (`api/client.ts`, generated from `openapi.yaml`, CSRF double-submit + session cookie):

```typescript
// A view fetches its own data with useGet; useLiveGet polls only while work is in flight.
const jobsQ = useGet<Job[]>(
  () => api.GET("/jobs", { params: { query: { page: 1, pageSize: 200 } } }),
  [bump],                 // refetch when a mutation bumps this dep
);
```

- `useGet<T>(fetcher, deps, intervalMs?)` — typed fetch with `{ data, error, loading }`; keeps the last-good data on a background-poll error rather than blanking the view.
- `useLiveGet<T>(...)` — wraps `useGet` to poll (`activeMs`) **only while `isActive(data)`** is true (e.g. a run is still queued/running) and stops once everything is terminal.
- Other shared hooks: `useTableColumns` (order/visibility behind the Columns menu, `components/table.tsx`), `useColumnWidths` (localStorage-persisted widths), `useTableSort` (paired with the backend's `sortparam` package), `useClientPager`, `useDebounced`, `useInlineTags`, `useInlineAnnotation`, `useToast`.

Local UI state (active tab, search, expanded rows, pagination) is plain `useState` inside each view — no prop drilling, no shared mutable store.

## Running Tests

```bash
# Backend
make test

# Frontend
cd frontend && npm test

# Integration (real agent ↔ real server)
go test ./internal/runner -v
```

## Deployment

### Single Container (Production)

```bash
docker run -d \
  -p 8080:8080 \
  -v /var/lib/cronomicon:/var/lib/cronomicon \
  -v /run/secrets:/run/secrets:ro \
  -e CRONOMICON_OIDC_ISSUER=https://auth.example.com \
  -e CRONOMICON_OIDC_CLIENT_ID=cronomicon \
  -e CRONOMICON_OIDC_CLIENT_SECRET=... \
  -e CRONOMICON_OIDC_REDIRECT_URL=https://cronomicon.example.com/api/v1/auth/callback \
  -e CRONOMICON_KEK_FILE=/run/secrets/cronomicon-kek \
  -e CRONOMICON_BACKUP_S3_BUCKET=cronomicon-backups \
  cronomicon:2.0.0
```

The full variable matrix is in [`backend/deploy/env-matrix.md`](backend/deploy/env-matrix.md) and the
annotated template in [`backend/deploy/cronomicon.env.example`](backend/deploy/cronomicon.env.example).
The KEK file must not be world-readable — the server refuses to start if it is (v1.5.30).

### Runner Agents (Distributed Execution)

Runners are provisioned **from the app**: open the **Runners** view → **Add Runner**, and follow the
generated one-line install command, which is backed by a single-use registration token:

```bash
curl -fsSL https://cronomicon.example.com/install/<token> | sudo bash
# or, with the script and binary downloaded from the server:
sudo ./runner-install.sh -s https://cronomicon.example.com -t <token> -n runner-dc1-01 -c bash,ansible
```

The installer writes a systemd unit and `/etc/cronomicon/cronomicon-runner.env` (template:
[`backend/deploy/cronomicon-runner.env.example`](backend/deploy/cronomicon-runner.env.example)). Every deployed
runner must speak wire protocol **v12** — registration is refused below the floor (v1.5.40). See
[backend/deploy/](backend/deploy/) for hardened units and security guides.

The **Runners** view also links to **Install Guide**, **Config Guide**, and **Security
Guide** — the full install/configure, day-2 management, and credential-model walk-throughs.
They open in a new tab as standalone pages served at `/runner-install.html`,
`/runner-manage.html`, and `/runner-security.html`, generated at build time from the
single-source fragments in `documentation/` (editing those updates the pages).

## Known Limitations

- **No multi-server HA**: Single-process, single-DB (V2 roadmap)
- **WinRM execution (R6)**: Windows remote execution over WinRM is shelved by decision. Runners execute over SSH (direct + bastion) or locally — PowerShell scripts run that way, not via native Windows remoting
- **No bundled metrics dashboard**: A Prometheus scrape endpoint (`/metrics`, toggled under **Settings → Observability**) and alert routing ship today, but a built-in metrics dashboard UI does not — point your own Grafana/Prometheus stack at the endpoint
- **Queue depth is fixed at 3 per key**: The `Queue` concurrency policy is a safety backstop, not a tuning knob; beyond the cap a fire falls back to `Forbid` behaviour and says so in the run's reason
- **Workflows cannot queue**: Only jobs carry a concurrency policy
- **Shared surfaces stay admin-only**: Reusable schedules, working calendars, reactions, revision history and the recycle bin act on objects belonging to no single agency, so they are not delegable to a department
- **SSH key credentials are runner-only**: A key-bound job runs on a runner agent; the in-app SSH executor refuses it rather than running without the key (v1.5.35)
- **One wire protocol**: Runner agents older than protocol v12 are refused at registration; upgrade agents with the server

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for how to report issues and submit pull requests, and [AGENTS.md](AGENTS.md) for code patterns, state management, and conventions. New features should:

1. Update `openapi.yaml` (API contract) — both copies (backend + repo root) byte-identical
2. Regenerate the typed client (`npm run gen` in frontend); hand-mount the backend handler
3. Implement backend handler + tests (land routes with their spec ops so conformance stays green)
4. Implement frontend view + client call
5. Update [CHANGELOG.md](CHANGELOG.md) with a new version section

## License

Copyright 2026 ResetSmith. Licensed under the [Apache License, Version 2.0](LICENSE).
Third-party components and trademarks are listed in [NOTICE](NOTICE).

## Support & Troubleshooting

- **Logs**: `docker logs <container>`, the process log at `<log dir>/cronomicon.log`, and run logs grouped in per-entity folders under the configured log directory (Settings → Execution → Log Storage; local path changes apply live, v0.52.9–.10)
- **Database health**: `GET /readyz` (checks DB migrations, OIDC identity)
- **Runner offline**: Check `CRONOMICON_RUNNER_OFFLINE_AFTER` (default 5m); a runner with no heartbeat for over 2 minutes shows **degraded**, stale runners are marked offline, and rows silent for `CRONOMICON_RUNNER_DEREGISTER_AFTER` (default 14d) are removed with their placement snapshotted for restore
- **SSH execution fails**: Verify bastion ProxyJump config and host keys in `~/.ssh/known_hosts`

See the [Administrator Manual](documentation/administrator-manual.html) (deployment & day-2 operations) for detailed troubleshooting steps.

---

**Last updated**: Sep 23, 2026  
**Current version**: v1.5.45 (schema v1150, runner protocol v12) — see [CHANGELOG.md](CHANGELOG.md)  
**Status**: Stable. v1.0.0 (Aug 11, 2026) was the first stable release; the 1.x line since then has added
production features (event triggers, SLA monitoring, revision history and the recycle bin, the Queue
concurrency policy, sub-workflows, file-arrival triggers), **agency-first RBAC** (every job states its
agency, authoring is a grantable departmental permission, a department administers its own access),
**per-agency identity** (two departments may own the same job name, on permanent ids), **operator
annotations** (notes, critical, contact — and failure alerts that name the contact), **runner
targeting**, the **Columns** menu, and the Run dialog's **"This run"** summary rail. The 1.5 series
added **live log tailing** (protocol v12), the Dashboard **verdict strip**, per-panel **Refresh**, the
in-app **training courses** and usage guides, a server-searched **Activity** feed, the **DR band**
(finishable KEK rotation, runner placement restore, a KEK-permission boot check), the **S3 log archive**,
a **masked audit stream**, Go 1.26 modernisation with a gating linter, and a dead-code sweep that
retired every compatibility shim the private history had needed.

<details>
<summary>Programme history before v1.0.0</summary>

Composable-primitives + Git-OR-Cronomicon dual-source + Schedule Builder + ad-hoc run controls + job-level env + catalog folder browsing + script variable scanning + visual workflow canvas + Python run-type + operator-owned tags + declared run inputs (Warn/Block enforcement) + Ansible inventory support (M1–M5) + agency-based runner isolation & single membership axis + first-class SSH key credentials + Ansible runner-checkout hardening (RX Phases 1–6) + self-service runner provisioning & auth unification (v0.47.x) + env-var namespace references (v0.48.x) + reference-based secret injection & Vault, AppRole or token (v0.49.x, v0.52.29) + A5 scope model (v0.50.x) + security update (v0.51.x) + logging update: process log, per-entity run logs, audit stream (v0.52.8–.12) + the visual update: IBM Plex, gold brand, true light mode, the Score (v0.52.13–.25) + fixes pass incl. workflow-pause authz + derived `degraded` runner status (v0.52.30–.38) + target-host pin on every run path + Dashboard Current status (v0.53.x) + per-run/per-job SSH identity (v0.54–0.55.0) + SSH↔Ansible run parity (v0.55.1–.3) + table sorting + schedule windows/modes + deferred ad-hoc runs (v0.55.x) + **departmental RBAC: grants, roles-as-data, enforced execution verbs, departmental secrets/keys/runners, break-glass `grant-admin`** (v0.56.0–0.56.10), then run-as agency credentials, working calendars,
reactions and the Run-dialog runtime update (v0.57.x) — shipped as the A5→RB/RF, A9–A13, F1–F4 and
JC/RX/FX/TG/EV/LG/CS/CA/RP/AW/AR/RA/CAL/RU series.

</details>

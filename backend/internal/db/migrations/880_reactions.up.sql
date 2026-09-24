-- 880 Reactions — a definition runs when another definition finishes
-- (the reactions-update plan, RX-1 — Phase B).
--
-- Job-to-job coupling existed only INSIDE a workflow: the engine walks its own
-- step graph and reacts to runs IT started. If a job fired from its own cron, or
-- the Run dialog, or another workflow, nothing downstream noticed. There was no
-- depends_on, no on_success, no trigger source anywhere whose event is
-- "something finished".
--
-- A reaction is a fourth trigger kind, alongside cron, interval and one-shot:
--
--     when <job|workflow> X finishes with <success|failure|stopped|any>,
--     run <job|workflow> Y  (optionally after a delay)
--
-- It is EDGE-TRIGGERED. There is no persistent "prereq satisfied" state
-- anywhere: a reaction is not a latch, does not accumulate, and cannot be
-- inspected as "currently waiting", because it never waits. That is what makes
-- manual prereq satisfaction unnecessary — there is nothing to satisfy — and it
-- is why a newly created reaction fires on the NEXT matching completion and
-- never retroactively on the most recent historical one.
--
-- ── why the 2×2 is one primitive, not four ─────────────────────────────────
-- Both halves are already modelled. The OWNER side reuses definition_schedules'
-- tuple exactly — (owner_source, owner_kind, owner_name, name) with
-- owner_kind IN ('job','workflow'), cascade triggers included. The TARGET side
-- is pending_runs.kind IN ('job','workflow'), with firers for both already
-- wired. So a reaction resolves into a pending_runs row and adds a PRODUCER,
-- not a dispatch path: the concurrency cap, the Forbid policy and the
-- definition-existence check are then judged once, at promotion, by code that
-- already runs.

CREATE TABLE reactions (
    -- The reacting definition — the one that RUNS. Mirrors definition_schedules'
    -- primary key shape exactly, including `name` as the entry name, so a
    -- definition can carry several reactions the way it carries several
    -- schedule entries.
    owner_source  TEXT NOT NULL DEFAULT 'git' CHECK (owner_source IN ('git','amadeus')),
    owner_kind    TEXT NOT NULL CHECK (owner_kind IN ('job','workflow')),
    owner_name    TEXT NOT NULL,
    name          TEXT NOT NULL,

    -- The watched definition — the one that FINISHES. Deliberately NOT a foreign
    -- key: see the cascade asymmetry below.
    on_source     TEXT NOT NULL DEFAULT 'git' CHECK (on_source IN ('git','amadeus')),
    on_kind       TEXT NOT NULL CHECK (on_kind IN ('job','workflow')),
    on_name       TEXT NOT NULL,

    -- The normalised outcome to match. Jobs have five terminal states and
    -- workflows are binary, so both are projected onto {success, failure,
    -- stopped} before matching (internal/reaction).
    --
    -- `stopped` is reaction-side vocabulary ONLY and is never a runs.status
    -- value. It means an operator ended the upstream and did not say what it
    -- meant — an unclassified kill, or a cancelled workflow. It exists because
    -- both flat alternatives fail loudly: folding it into failure fires the
    -- rollback automation while a human is already hands-on, and folding it into
    -- nothing silently skips the cleanup that releases a lock. `any` matches all
    -- three, and never a run that did not happen.
    on_outcome    TEXT NOT NULL CHECK (on_outcome IN ('success','failure','stopped','any')),

    -- Deferral. 0 ⇒ as soon as the reactor observes the completion.
    delay_seconds INTEGER NOT NULL DEFAULT 0 CHECK (delay_seconds >= 0),

    -- RX-21 storm brake. A job on a one-minute cron with a reaction attached is
    -- 1440 downstream runs a day, and the Forbid policy only helps jobs that set
    -- it (and does not exist for workflows at all). Judged against the last
    -- FIRED delivery; a blocked event is dropped, not deferred, because unlike a
    -- cap deferral the event has already been consumed. 0 ⇒ off.
    min_interval_seconds INTEGER NOT NULL DEFAULT 0 CHECK (min_interval_seconds >= 0),

    -- §2.5. A run inside a workflow emits no job reaction by default: if job
    -- `extract` is step 3 of workflow `nightly`, a reaction watching `extract`
    -- gives `nightly` fan-out that appears nowhere in its step graph. Workflow
    -- completion is the right altitude for cross-definition coupling.
    include_workflow_children INTEGER NOT NULL DEFAULT 0 CHECK (include_workflow_children IN (0,1)),

    -- RX-Q8. A granularity definition_schedules does NOT have — an individual
    -- schedule entry cannot be disabled today; the only lever is paused_jobs
    -- against the whole definition. Shipped anyway, because the alternative is
    -- worse: the only way to stop a reaction firing would be to delete it or
    -- pause the owning definition, and pausing also takes down that definition's
    -- own schedules. "Stop reacting to the nightly for a week while we debug"
    -- must not require taking the job's cron down with it. The inconsistency is
    -- a known finding and an argument for entry-level enable on schedules later.
    enabled       INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),

    position      INTEGER NOT NULL DEFAULT 0,

    PRIMARY KEY (owner_source, owner_kind, owner_name, name)
);

-- The reactor's hot path: "what reacts to THIS definition finishing?", asked
-- once per terminal row observed. enabled is in the index so disabled rows are
-- filtered without a table lookup.
CREATE INDEX idx_reactions_on ON reactions(on_kind, on_source, on_name, enabled);
-- The read path for a definition's own detail view and the delete guard.
CREATE INDEX idx_reactions_owner ON reactions(owner_kind, owner_source, owner_name);

-- ── cascade asymmetry, deliberate (RX-Q5) ──────────────────────────────────
-- Deleting the OWNER takes its reactions with it — the same rule as
-- definition_schedules and pending_runs, and a reaction on a job that no longer
-- exists could only fail at promotion.
--
-- Deleting the WATCHED definition never cascades, because a reaction that
-- vanishes when somebody deletes an unrelated upstream is a silent capability
-- loss. Dangling is therefore a SUPPORTED state: the interactive delete path
-- 409s unless forced (Phase C, RX-24), but the Git sync prune has no request to
-- fail and no operator to ask, so it always succeeds and leaves the reaction
-- dangling. A dangling reaction never fires and is rendered as missing rather
-- than being quietly inert.
CREATE TRIGGER trg_reactions_job_delete AFTER DELETE ON jobs
BEGIN
    DELETE FROM reactions
     WHERE owner_kind = 'job' AND owner_source = OLD.source AND owner_name = OLD.name;
END;

CREATE TRIGGER trg_reactions_workflow_delete AFTER DELETE ON workflows
BEGIN
    DELETE FROM reactions
     WHERE owner_kind = 'workflow' AND owner_source = OLD.source AND owner_name = OLD.name;
END;

-- ── the delivery log ───────────────────────────────────────────────────────
-- One row per (reaction, source run) pair, written BEFORE the verdict and never
-- after it. That ordering is the whole point: the record of "we considered this
-- event" must survive a crash mid-decision, and a suppressed delivery is a
-- DECISION THAT WAS MADE rather than an event that was lost. CAL-8 had to fix
-- exactly that distinction retroactively for suppressed fires.
--
-- The primary key is also the idempotency key. The reactor scans with a lookback
-- window rather than a strict watermark, because completed_at is written by
-- several uncoordinated writers and is NOT monotonic across concurrent runs — a
-- strict cursor would silently drop events that landed with an earlier stamp
-- after the cursor moved past. Overlap + INSERT OR IGNORE on this key is correct
-- under overlap, restart and clock skew alike.
CREATE TABLE reaction_deliveries (
    owner_source TEXT NOT NULL,
    owner_kind   TEXT NOT NULL,
    owner_name   TEXT NOT NULL,
    name         TEXT NOT NULL,

    -- The upstream run that was observed. src_kind is in the key even though
    -- both run tables use UUIDv7 ids and cannot collide in practice: it costs
    -- nothing and makes the key state the true identity of an event rather than
    -- leaning on an id format the schema does not enforce.
    src_kind     TEXT NOT NULL CHECK (src_kind IN ('job','workflow')),
    src_run_id   TEXT NOT NULL,

    outcome      TEXT NOT NULL,

    -- What we decided, and why. Every suppression writes a row: a suppressed
    -- reaction is never silent.
    --
    -- 'pending' is the CLAIM value, written by the insert below before any
    -- verdict exists, and it has to be its own value rather than a provisional
    -- 'fired' for two reasons. First, honesty: a crash between the claim and the
    -- verdict would otherwise leave a row asserting that we fired when we did
    -- not, and a delivery log that lies about the one case it exists to survive
    -- is worse than no log. Second, correctness: the rate brake (RX-21) anchors
    -- on the last 'fired' delivery for the reaction, so a provisional 'fired'
    -- would be found by the very evaluation that wrote it and every reaction
    -- with a min_interval would suppress itself on its first event.
    result       TEXT NOT NULL CHECK (result IN (
                     'pending',
                     'fired',
                     'expired',              -- past the catch-up grace
                     'suppressed_disabled',  -- the reaction, or its owner definition
                     'suppressed_paused',
                     'suppressed_calendar',  -- global tier only (RX-20)
                     'suppressed_depth',
                     'suppressed_rate'
                 )),
    -- Free text for the verdict: the suppressing calendar's name and day label,
    -- the depth reached, the interval that blocked. So "the cascade did not run
    -- on Veterans Day, deliberately" is queryable rather than a log line.
    detail       TEXT,

    -- Set only on 'fired': the pending_runs row this became.
    pending_run_id TEXT,
    delivered_at TEXT NOT NULL,

    PRIMARY KEY (owner_source, owner_kind, owner_name, name, src_kind, src_run_id)
);

-- "What did this run trigger?" — the reverse of the reaction edge, used by the
-- History because-of link and by the rate brake's last-fired lookup.
CREATE INDEX idx_reaction_deliveries_src ON reaction_deliveries(src_run_id);
-- The rate brake asks for the newest 'fired' delivery of one reaction.
CREATE INDEX idx_reaction_deliveries_fired
    ON reaction_deliveries(owner_source, owner_kind, owner_name, name, delivered_at)
    WHERE result = 'fired';

-- ── provenance and depth on the run tables ─────────────────────────────────
-- reaction_depth is the runaway backstop: a downstream run inherits
-- upstream + 1, and at the ceiling (a Go constant, not a setting) the delivery
-- is recorded suppressed_depth and nothing fires. This catches cycles that
-- static detection cannot see — one closing through a workflow's step graph
-- rather than through reaction edges.
--
-- reacted_to_run_id is the DURABLE because-of link both History directions
-- render. It must live on the run row: the only other place the upstream run id
-- exists is reaction_deliveries, which is retention-pruned alongside runs, and a
-- provenance link that evaporates with the delivery log would contradict
-- History owning the permanent trail.
ALTER TABLE runs           ADD COLUMN reaction_depth INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs           ADD COLUMN reacted_to_run_id TEXT;
ALTER TABLE workflow_runs  ADD COLUMN reaction_depth INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_runs  ADD COLUMN reacted_to_run_id TEXT;

CREATE INDEX idx_runs_reacted_to
    ON runs(reacted_to_run_id) WHERE reacted_to_run_id IS NOT NULL;
CREATE INDEX idx_workflow_runs_reacted_to
    ON workflow_runs(reacted_to_run_id) WHERE reacted_to_run_id IS NOT NULL;

-- ── the reaction context a pending run must carry ──────────────────────────
-- For a JOB target the frozen params_json could hold these, but for a WORKFLOW
-- target params_json is NULL — and the delivery and the promotion are separated
-- by delay_seconds plus possibly a restart, so nothing may ride in memory or in
-- the retention-pruned delivery row. All four are persisted here and read back
-- at promotion, which is what makes a delayed workflow reaction survive a
-- restart with its env stamp and depth intact.
ALTER TABLE pending_runs ADD COLUMN origin_kind     TEXT;    -- 'reaction' when a reactor produced this row
ALTER TABLE pending_runs ADD COLUMN origin_ref      TEXT;    -- the upstream run id
ALTER TABLE pending_runs ADD COLUMN reaction_depth  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pending_runs ADD COLUMN origin_env_json TEXT;    -- the AMADEUS_REACTED_TO_* stamp

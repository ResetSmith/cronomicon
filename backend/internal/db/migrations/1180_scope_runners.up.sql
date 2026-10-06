-- 1180_scope_runners — a scope names the runners allowed to serve it (the
-- scope-bound-runners plan, SB-1).
--
-- "Only this runner reaches this VLAN" is a fact about where a scope's hosts
-- are. Until now it was recorded per job, as a runner-tag pin (1070) matched
-- against the runner_tags projection — the one place a tag decided anything.
-- This table records it on the scope instead. The pin and its projection are
-- still present here; they are retired in a later migration of this band, once
-- every reader has moved.
--
-- A scope is RESTRICTED iff it has at least one row. An unrestricted scope
-- dispatches exactly as before; a restricted one is claimable only by a runner
-- named here, in addition to the agency, capability and injection rules — the
-- binding narrows and never widens (the RT-Q2 rule, carried over).
--
-- runner_id deliberately has NO foreign key to runners. A runner row is deleted
-- by an operator or by the reaper once it has been offline past the deregister
-- window, and a cascade would turn that into "the scope is unrestricted again":
-- jobs confined to one network segment would silently spread to every runner in
-- the agency. The row outlives its runner, the scope stays closed, and
-- runner_name (snapshotted at bind time) is what lets the UI and the
-- unclaimable reason name a runner that no longer exists. Only an operator
-- unbinds; restoring a placement re-points the row at the re-enrolled runner.
--
-- Like the agency binding and the tags, this is an operator-owned overlay: it is
-- never parsed from Git and the git scope upsert does not touch it. scope_id
-- DOES cascade — a pruned or deleted scope has nothing left to restrict.
CREATE TABLE scope_runners (
    scope_id    TEXT NOT NULL REFERENCES scopes(id) ON DELETE CASCADE,
    runner_id   TEXT NOT NULL,
    runner_name TEXT NOT NULL,
    bound_by    TEXT,
    bound_at    TEXT NOT NULL,
    PRIMARY KEY (scope_id, runner_id)
);

-- "Which scopes does this runner serve" (the Runners view, replace-runner, and
-- the re-point on a restored placement).
CREATE INDEX idx_scope_runners_runner ON scope_runners (runner_id);

-- Pins that could not be carried over. The pin column is dropped later in this
-- band, after which nothing else records that a job was once confined; the
-- Scopes view lists these until an operator resolves or dismisses each one.
--
--   mixed_pins          the scope's jobs pin different tags
--   partial_pins        some of the scope's jobs are pinned and some are not
--   no_scope            the job has no scope, so there is nothing to bind
--   unknown_scope       the job names a scope that has no scopes row
--   no_eligible_runner  every job agrees, but no runner both carries the tag
--                       and is eligible for the scope's agency
--   binned_job          the job is in the recycle bin, so it took no part in
--                       deciding its scope's binding; restoring it brings back a
--                       definition whose pin no longer confines it
--   leftover_git_key    written by git sync, not by this migration: a job's YAML
--                       still carries runner_tag on a scope with no binding
CREATE TABLE retired_runner_pins (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    job_uid      TEXT,
    job_name     TEXT NOT NULL,
    job_source   TEXT NOT NULL,
    scope        TEXT NOT NULL DEFAULT '',
    runner_tag   TEXT NOT NULL,
    reason       TEXT NOT NULL CHECK (reason IN (
                     'mixed_pins', 'partial_pins', 'no_scope', 'unknown_scope',
                     'no_eligible_runner', 'binned_job', 'leftover_git_key')),
    recorded_at  TEXT NOT NULL,
    dismissed_at TEXT,
    dismissed_by TEXT
);

CREATE INDEX idx_retired_runner_pins_scope ON retired_runner_pins (scope);

-- Convert what converts cleanly: a scope whose every live job is pinned, all to
-- the same tag, is bound to the runners that carry that tag AND are eligible for
-- the scope's agency. Eligibility is claimRun's disjoint rule — a scope with
-- agencies takes a member runner, a scope with none takes a general-pool runner —
-- so a converted binding never names a runner the claim query would refuse.
--
-- "Live" is deleted_at IS NULL, enabled or not: a disabled job is one click from
-- running. A binned job has no say in whether its scope is bound — it is not
-- running anything — but its pin is not forgotten either: see the notices below.
--
-- rt.tag is on the LEFT of the comparison on purpose: runner_tags.tag is
-- COLLATE NOCASE (1080) and the left operand's collation decides, which is what
-- makes "Prod" on the job match "prod" on the runner, as it did at claim time.
INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_by, bound_at)
SELECT sc.id, rn.id, rn.name, 'migration:1180', strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
  FROM scopes sc
  JOIN (
        SELECT j.scope AS scope_name, MIN(j.runner_tag) AS tag
          FROM jobs j
         WHERE j.deleted_at IS NULL AND COALESCE(j.scope, '') <> ''
         GROUP BY j.scope
        HAVING COUNT(*) = SUM(CASE WHEN COALESCE(j.runner_tag, '') <> '' THEN 1 ELSE 0 END)
           AND COUNT(DISTINCT lower(j.runner_tag)) = 1
       ) u ON u.scope_name = sc.name
  JOIN runner_tags rt ON rt.tag = u.tag
  JOIN runners rn ON rn.id = rt.runner_id
 WHERE (EXISTS (SELECT 1 FROM scope_agencies sa WHERE sa.scope_id = sc.id)
        AND EXISTS (SELECT 1 FROM scope_agencies sa
                      JOIN runner_agencies ra ON ra.agency_id = sa.agency_id
                     WHERE sa.scope_id = sc.id AND ra.runner_id = rn.id))
    OR (NOT EXISTS (SELECT 1 FROM scope_agencies sa WHERE sa.scope_id = sc.id)
        AND NOT EXISTS (SELECT 1 FROM runner_agencies ra WHERE ra.runner_id = rn.id));

-- Everything else: every pinned job whose scope did not just get a binding.
-- The CASE is ordered from the job's own situation outwards, so each row carries
-- the first reason an operator would have to fix.
--
-- That includes BINNED jobs. A restore only clears deleted_at, so a binned job
-- comes back exactly as it was — and once the pin column is dropped that means
-- unconfined, on a scope nobody bound, with nothing left to say it ever was. A
-- binned job on a scope that DID get a binding needs no notice: restored, it is
-- served by that scope's bound runners like every other job there.
INSERT INTO retired_runner_pins (job_uid, job_name, job_source, scope, runner_tag, reason, recorded_at)
SELECT j.uid, j.name, j.source, COALESCE(j.scope, ''), j.runner_tag,
       CASE
         WHEN j.deleted_at IS NOT NULL THEN 'binned_job'
         WHEN COALESCE(j.scope, '') = '' THEN 'no_scope'
         WHEN NOT EXISTS (SELECT 1 FROM scopes sc WHERE sc.name = j.scope) THEN 'unknown_scope'
         WHEN (SELECT COUNT(DISTINCT lower(j2.runner_tag)) FROM jobs j2
                WHERE j2.deleted_at IS NULL AND j2.scope = j.scope
                  AND COALESCE(j2.runner_tag, '') <> '') > 1 THEN 'mixed_pins'
         WHEN EXISTS (SELECT 1 FROM jobs j2
                       WHERE j2.deleted_at IS NULL AND j2.scope = j.scope
                         AND COALESCE(j2.runner_tag, '') = '') THEN 'partial_pins'
         ELSE 'no_eligible_runner'
       END,
       strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
  FROM jobs j
 WHERE COALESCE(j.runner_tag, '') <> ''
   AND NOT EXISTS (SELECT 1 FROM scope_runners sr
                     JOIN scopes sc ON sc.id = sr.scope_id
                    WHERE sc.name = j.scope);

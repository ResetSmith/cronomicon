-- 1070_runner_targeting — RT-1: pin a job or an ad-hoc run to a TAGGED runner
-- (the runner-targeting plan).
--
-- WHY THIS EXISTS. The fleet had two ways to say where work runs, and neither
-- says "on a runner that can reach the right network":
--
--   · agency membership is coarse — every runner in an agency is interchangeable;
--   · `requires` tokens have the right shape but the wrong ownership. They match
--     against runners.capabilities, which the AGENT self-declares at registration
--     (register.go), so granting one means editing config on the runner host. An
--     admin cannot do it from inside Cronomicon at all.
--
-- Tags (migration 580) are the one runner property the application can already
-- set — operator-authored, edited from the runner's expanded row, full-replaced
-- via PUT /runner-tags/{runnerId}. Until now nothing in dispatch read them. This
-- migration makes them load-bearing.
--
-- BY TAG, NOT BY RUNNER ID (RT-Q1). A runner id is a REGISTRATION identity:
-- register.go mints a fresh one per enrollment, so re-enrolling a host dangles
-- every pin that named it, silently, and the pinned runs just queue forever. A
-- tag survives re-enrollment, and gives N-of-M redundancy for free — "any runner
-- tagged vlan-dmz" is what the operator meant anyway, since VLAN reachability is
-- a property of network position rather than of one box.
--
-- OPTIONAL EVERYWHERE. Both columns are nullable and unset is the default. The
-- claim predicate short-circuits on NULL/'' so an unpinned run's dispatch is
-- byte-identical to its pre-1070 behaviour — that no-regression case is the most
-- important test in the band (TestUnpinnedRunClaimableByAnyRunner).
--
-- THE PIN NARROWS, NEVER WIDENS (RT-Q2). The new predicate is ANDed with the
-- agency intersection in claimRun, never ORed and never substituted for it. A pin
-- naming a tag that only an out-of-agency runner carries leaves the run queued,
-- which is correct: pinning is not, and must never become, a route to a runner
-- agency isolation would otherwise deny. Any future patch that makes the tag
-- branch reachable while the agency branch is false is a defect.
ALTER TABLE jobs ADD COLUMN runner_tag TEXT;
ALTER TABLE runs ADD COLUMN runner_tag TEXT;

-- ── The projection table ────────────────────────────────────────────────────
--
-- runners.tags stays AUTHORITATIVE. This table is derived from it, written in the
-- same transaction (settings_tags_mount.go), and re-derivable at any time by
-- SyncRunnerTags. If the two ever disagree, the JSON column is right and this is
-- a stale index.
--
-- It exists for exactly the reason run_agencies (690) exists, and the numbers
-- there are the precedent: json_each cannot be indexed, so probing tags via
-- `json_each(rn.tags)` inside claimRun would re-parse a JSON array for every
-- candidate row on every poll — the same shape that measured +350% when the
-- agency predicate was written that way. 690's header records that benchmark and
-- poll_claim_bench_test.go keeps it reproducible. The claim query's shape is
-- load-bearing and documented as such in poll.go; a correlated JSON parse there
-- is a real regression, not a style preference.
--
-- Keyed (runner_id, tag) so the claim's probe — "does THIS runner carry the run's
-- tag" — is a seek on the composite PK's leading column, the same access pattern
-- run_agencies serves for the agency probe.
--
-- THE FK IS DELIBERATE and is the cascade. Deregistration (register.go:560) and
-- the offline reaper (reaper.go:273) both DELETE the runners row; with
-- _foreign_keys=on (db.go:43) that reaps these rows too, so a re-enrolled host
-- cannot inherit a dead registration's tags. Unlike run_agencies' snapshot
-- discipline there is nothing to preserve here — a deregistered runner's tags are
-- not history anyone reads.
CREATE TABLE runner_tags (
  runner_id TEXT NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
  tag       TEXT NOT NULL,
  PRIMARY KEY (runner_id, tag)
);

-- The reverse question — "which runners carry vlan-dmz?" — is what the RT-3
-- picker's online-count and UnclaimableReason's new arm ask. Not served by the
-- PK, so it gets its own index.
CREATE INDEX idx_runner_tags_tag ON runner_tags (tag);

-- Backfill from the authoritative column. json_each over the whole runners table
-- once at migration time is fine; per poll is precisely what this replaces.
-- Empty-string tags are skipped — tagutil.Normalize cannot produce one, but a
-- pre-580 hand-edited row could, and an empty tag would collide with the
-- NULL/'' short-circuit that means "unpinned".
INSERT OR IGNORE INTO runner_tags (runner_id, tag)
SELECT rn.id, je.value
FROM runners rn, json_each(COALESCE(rn.tags, '[]')) je
WHERE je.value IS NOT NULL AND je.value <> '';

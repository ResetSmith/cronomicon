-- 511 Checkout project grouping + pinned-checkout run snapshot
-- (ansible-update.md Phase 2 — checkout-mode core, RX.1/RX.2/§7).
--
-- A `kind: Script` wrapper may declare spec.project_root (a repo-relative
-- directory) + spec.entry (the repo-relative entry playbook). Such a script is
-- a "project": it is delivered to the runner as a pinned checkout of the repo
-- rather than a body-only playbook string. project_root is the marker
-- (non-empty ⇒ project); entry is stored on the existing script_path column
-- (§7: "script_path = entry"), so no separate entry column is needed on
-- scripts/jobs.
--
--   scripts.project_root — synced from the wrapper (nullable; NULL/'' ⇒ plain).
--   jobs.project_root    — denormalized from the referenced script (the
--                          content_hash/script_ref precedent), so enqueue and
--                          the read APIs can tell a checkout job from a body job
--                          without a join.
--   runs.checkout_sha    — the commit pinned at ENQUEUE (git_sync_state.last_sha
--                          snapshotted so a mid-run sync can't retarget it,
--                          RX.2). Non-NULL ⇒ this run is a checkout run.
--   runs.checkout_entry  — the entry playbook path pinned at enqueue (the job's
--                          script_path at enqueue time), so a mid-run redefinition
--                          can't move the entry under a pinned SHA.
--
-- All plain ADD COLUMN (nullable, no rebuild): body-only jobs/runs read NULL
-- and behave exactly as before.
ALTER TABLE scripts ADD COLUMN project_root TEXT;
ALTER TABLE jobs    ADD COLUMN project_root TEXT;
ALTER TABLE runs    ADD COLUMN checkout_sha   TEXT;
ALTER TABLE runs    ADD COLUMN checkout_entry TEXT;

-- 730_webhook_flags_backfill — make the GitLab webhook toggles safe to enforce
-- (the visual-update-followups plan, F2-3). Data-only: no schema
-- change, no rebuild.
--
-- WHY THIS EXISTS. `webhook_enabled` and the three `webhook_events_*` columns
-- (migration 060) have been written by Settings → Integrations since they were
-- added, and read by nothing. The webhook handler validated its token and
-- synced, whatever the flags said. F2-3 wires them up, which turns a stored
-- value that never mattered into one that decides whether a delivery is acted
-- on — and every one of those columns defaults to 0.
--
-- So wiring them without this line would silently break every working webhook
-- on upgrade: the operator never had a reason to switch on a toggle that did
-- nothing, and their repo would simply stop syncing on push, with a 403 visible
-- only in GitLab's delivery log. That is a worse outcome than the inert control
-- F2 set out to fix.
--
-- The back-fill therefore encodes the policy every install has effectively been
-- running: enabled, all three event types accepted. What changes after it is
-- only that an operator can now turn those off and have it mean something.
-- settings.GetWebhookPolicy fails open for the same reason (absent row, absent
-- table, read error → enabled), so a fresh install behaves identically.
UPDATE gitlab_config
   SET webhook_enabled     = 1,
       webhook_events_push = 1,
       webhook_events_mr   = 1,
       webhook_events_tag  = 1
 WHERE id = 1;

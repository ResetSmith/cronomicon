-- K-2 (VF-16) — re-home the "last sent" signal onto the transports.
--
-- Until v0.52.24 this lived on alert_destinations.last_fired_at, stamped by
-- notify.markDestinations on EVERY enabled destination whose type matched the
-- transport that sent. So a row reading "last fired 3 minutes ago" meant only
-- "something delivered over this transport" — never "this destination
-- delivered", because no destination was ever a delivery target: routing came
-- from notification_config plus each rule's channels. The Destinations section
-- is gone (K-1), and the signal becomes what it always was — two facts per
-- transport, on the singleton that actually owns delivery.
--
-- No backfill. The old per-destination stamps cannot be mapped onto a transport
-- without inventing which destination "won" a send, and inventing history is
-- worse than an empty field that fills in on the next dispatch.
ALTER TABLE notification_config ADD COLUMN last_email_at TEXT;
ALTER TABLE notification_config ADD COLUMN last_email_status TEXT;
ALTER TABLE notification_config ADD COLUMN last_apprise_at TEXT;
ALTER TABLE notification_config ADD COLUMN last_apprise_status TEXT;

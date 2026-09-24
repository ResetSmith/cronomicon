-- Reverse 720. Drops the auth audit trail.
--
-- LOSSY AND UNRECOVERABLE, unlike most reversals here: these rows exist nowhere
-- else. change_log and activity never carried auth events (that absence is the
-- reason this table was added), and `recent_logins` keeps only the most recent
-- login per user by UPSERT. Rolling back destroys the record outright.
--
-- If the rows matter — and for an audit trail they generally do — export them
-- first: the audit.log stream carries the same events as JSON Lines, and
-- GET /audit/export produces CSV/JSON over a date range.
DROP INDEX IF EXISTS idx_auth_events_actor;
DROP INDEX IF EXISTS idx_auth_events_at;
DROP TABLE IF EXISTS auth_events;

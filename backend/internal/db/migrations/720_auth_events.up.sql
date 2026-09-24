-- 720_auth_events — an audit trail for authentication and authorization
-- (the logging-update plan LU-9, decision LU-Q2(a)). Additive: one new
-- table, so no rebuild.
--
-- WHAT DID NOT EXIST BEFORE THIS. Zero auth events were audited anywhere. No
-- login success, no login failure, no logout, no session creation, no RBAC
-- denial, no CSRF failure — to either audit table. `recent_logins` is not a
-- substitute and is documented as such: it is an UPSERT keyed on the user, so
-- login #2 destroys the record of login #1, and its write error is swallowed.
--
-- WHY A SEPARATE TABLE rather than reusing the existing pair:
--
--   activity.kind has a CHECK limited to seven values with no 'auth', and
--   widening a CHECK means rebuilding the table — which this plan avoids
--   everywhere because rebuilds silently drop triggers and reassign rowids.
--
--   change_log.actor is NOT NULL, but the events that matter most here are the
--   ones with no known identity: a failed login, a CSRF rejection before the
--   session resolves. Forcing a sentinel actor onto those would corrupt the one
--   column an auditor filters on.
--
-- A purpose-built table sidesteps both, and lets the columns say what auth
-- events actually need to say.
--
-- TWO ADDRESS COLUMNS, DELIBERATELY. Behind a reverse proxy r.RemoteAddr is the
-- proxy's address on every request — knowing that in advance is exactly why one
-- column would be uninformative. Recording both makes the question empirical: if
-- client_ip tracks real operator addresses while remote_addr stays constant, the
-- forwarded-header walk is working; if the two always match, the proxy is not
-- setting the header and the deployment needs a proxy-side change. The operator
-- asked to observe what this collects in practice before investing further, and
-- one column would have answered nothing.
--
-- client_ip is derived by a right-most-untrusted walk over X-Forwarded-For, never
-- by taking XFF[0] — the left-most entry is attacker-supplied, and an audit trail
-- carrying an address the attacker chose is worse than one carrying no address.
CREATE TABLE auth_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    at          TEXT NOT NULL,        -- RFC3339 UTC
    kind        TEXT NOT NULL,        -- login | logout | login-failed | denied | csrf-failed | dev-auth | bootstrap-admin | session-revoked
    outcome     TEXT NOT NULL,        -- success | failure
    actor       TEXT,                 -- NULL is legal and expected: pre-identity failures
    reason      TEXT,                 -- exchange_failed | bad_nonce | insufficient_scope | ...
    target      TEXT,                 -- the denied resource / scope / required role
    remote_addr TEXT,                 -- r.RemoteAddr verbatim (the proxy, behind one)
    client_ip   TEXT,                 -- right-most-untrusted XFF walk; NULL when undeterminable
    user_agent  TEXT,
    details     TEXT,
    created_at  TEXT NOT NULL
);

-- No CHECK on `kind` or `outcome`, for the same reason migration 710 omits one:
-- a CHECK can only be widened by rebuilding the table, and the set of auditable
-- auth events will grow. internal/auditlog's constants are the writers.

-- `at` serves the export's date-range scan; `actor` serves "everything this user
-- did", which is the first question asked of an auth trail during an incident.
CREATE INDEX idx_auth_events_at ON auth_events(at);
CREATE INDEX idx_auth_events_actor ON auth_events(actor);

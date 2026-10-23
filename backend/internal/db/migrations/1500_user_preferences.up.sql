-- 1500_user_preferences — a person's own choices (2.4.4 S4, the agency
-- switcher). The first is where a visit starts: default_agency, an agency's
-- id, 'global', or 'overview'; NULL for the rule (GET /me/context). Keyed by
-- the sign-in's email, as recent_logins is: there is no users table.
CREATE TABLE user_preferences (
    email          TEXT PRIMARY KEY,
    default_agency TEXT,
    updated_at     TEXT NOT NULL
);

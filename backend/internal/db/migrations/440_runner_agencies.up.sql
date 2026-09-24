-- 440_runner_agencies — runner ↔ agency membership (agency-support.md M2, §3.3).
-- Many-to-many: a runner may belong to several agencies (a multi-NIC runner can
-- serve more than one VLAN). Operator-assigned only — never self-declared by the
-- agent (§2.1). ON DELETE CASCADE on runner_id cleans membership when a runner is
-- deregistered; the agency_id cascade is a backstop (agency deletion is blocked
-- with 409 while any runner or scope references it).
CREATE TABLE runner_agencies (
  runner_id TEXT NOT NULL REFERENCES runners(id)  ON DELETE CASCADE,
  agency_id TEXT NOT NULL REFERENCES agencies(id) ON DELETE CASCADE,
  PRIMARY KEY (runner_id, agency_id)
);

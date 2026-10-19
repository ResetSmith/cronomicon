-- 1570_runner_processes — the agent processes seen on a runner's identity
-- (2.5.0 C6a; present defect 70, question 67).
--
-- The identity file names the runner, not the process: a cloned VM, a
-- snapshot taken after enrolment or two containers on one volume are one
-- runner row and two processes, and 2.3.2's started=1 made each copy's start
-- close the other's live runs as runner_lost. An agent of this release sends a
-- value of its own on every poll (runnerproto.PollParamProcess). One row per
-- value seen; seq orders them by first sight, which a timestamp cannot (two
-- copies started in the same second). A restarted process never polls again;
-- an older process that polls after a newer one started is a copy.
-- Rows unseen for the offline window are pruned.
CREATE TABLE runner_processes (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    runner_id     TEXT NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
    process       TEXT NOT NULL,
    client_ip     TEXT,
    first_seen_at TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    UNIQUE (runner_id, process)
);

-- identity_shared_at: when an older process last polled after a newer one
-- started (the runner_identity_shared notice is open while it is set).
-- lost_runs_before: a start that could not yet tell a restart from a copy.
-- An older process was seen within the hold window, so its runs were not
-- closed; they are closed (those started before this time) once every older
-- process has stayed silent for the window, and never if one polls again.
ALTER TABLE runners ADD COLUMN identity_shared_at TEXT;
ALTER TABLE runners ADD COLUMN lost_runs_before TEXT;

-- Reverse 690. run_agencies is a pure lookup structure derived from
-- runs.agencies_json, so dropping it loses no fact — only the speed. A server
-- running the pre-690 predicate reads agencies_json directly and is correct, just
-- slower (see the numbers in the up migration).
DROP TABLE run_agencies;

-- Reverse 070.
ALTER TABLE ssh_hosts DROP COLUMN host_key;
ALTER TABLE runs DROP COLUMN executor;
ALTER TABLE jobs DROP COLUMN executor;
ALTER TABLE jobs DROP COLUMN script_path;
ALTER TABLE jobs DROP COLUMN script;
ALTER TABLE jobs DROP COLUMN command;

-- Reverse 600. Drop the per-runner secret-injection trust flag.
ALTER TABLE runners DROP COLUMN allow_secret_injection;

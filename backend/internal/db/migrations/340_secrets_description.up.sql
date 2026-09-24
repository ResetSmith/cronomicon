-- 340 secrets description. The column was promised by the OpenAPI EnvSecret
-- schema and the SPA "New Secret" form but never existed in the table, so
-- secret descriptions were silently dropped on write and always read back NULL
-- (the exact bug 100 fixed for env_vars — secrets were missed).
ALTER TABLE secrets ADD COLUMN description TEXT;

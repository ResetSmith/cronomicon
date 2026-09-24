-- 100 env_vars description (LB2). The column was promised by the OpenAPI EnvVar
-- schema and the SPA form but never existed in the table, so descriptions were
-- silently dropped on write and always read back NULL.
ALTER TABLE env_vars ADD COLUMN description TEXT;

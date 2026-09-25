# Migration-on-boot policy & runbook (B.8)

## Policy: forward-only

Cronomicon runs golang-migrate **on every boot** (`internal/db/migrate.go`,
`db.Migrate` called from `main.go`). The deploy policy is **forward-only**:

- Each release ships `NNN_*.up.sql` migrations that are applied automatically.
- `*.down.sql` files exist for local development and tests **only**. Production
  recovery is **restore-from-backup**, not `migrate down` — down-migrations can
  drop columns/tables and lose data.
- Never edit an already-released migration. Fix-forward with a new numbered one.

`/readyz` reports schema state via the `database` check: it fails (503) while the
schema version is behind or **dirty** (a half-applied migration), so an
orchestrator holds traffic until the schema is clean. `cronomicon healthcheck -ready`
(the container HEALTHCHECK) wraps the same probe.

## Normal upgrade

1. Pull/build the new image and `docker compose up -d`.
2. On boot, pending `up` migrations apply; `/readyz` flips to ready once clean.
3. If `/readyz` stays `not_ready` with `database: error`, see below.

## Failed / dirty migration

A migration that errors mid-way leaves the schema **dirty**. `/readyz` then
reports `database: error: schema is dirty (failed migration)` and the container
stays unhealthy (traffic held). Do **not** try to hand-patch the live DB.

1. **Capture the logs** — the failing migration number + SQL error is logged at
   boot (`migrate db: ...`). Note the version.
2. **Stop** the container so nothing holds the DB open.
3. **Restore the last good snapshot** following `backup-restore.md` (stop → swap
   the DB file → clear `*.db-wal`/`*.db-shm` → start). The restored DB is at the
   previous schema version and is clean.
4. **Roll back the image** to the previous release so boot doesn't re-apply the
   same broken migration. Pin the prior tag in compose and `up -d`.
5. **Fix forward**: correct the migration in a new release, test against a copy of
   the restored DB, then upgrade again.

> Single-writer, single-node SQLite (decision: no HA, no PITR for v1) — the
> snapshot granularity is the nightly backup, so a dirty migration loses at most
> the day's changes. For a risky migration, take a manual snapshot first:
> `cp /var/lib/amadeus/amadeus.db /var/lib/amadeus/backups/pre-upgrade-$(date +%F).db`
> (do this while stopped, or via `VACUUM INTO`).

## Inspecting schema state

```
# version + dirty flag are surfaced at /readyz (see the database check)
curl -s https://amadeus.example.com/readyz | jq .

# or, against a stopped DB on the volume:
sqlite3 /var/lib/amadeus/amadeus.db 'SELECT * FROM schema_migrations;'
```

# On-call runbook (Phase D.5/D.6)

Operational procedures for the Cronomicon stack. Pairs with `migrations-runbook.md`
(schema), `backup-restore.md` (data), and `security-review.md` (controls).

## Degradation model (T12) — what breaks when a dependency is down

Cronomicon is a control plane fronted by a proxy + Authelia. Expected behavior when
something upstream/adjacent fails:

| Failure | Effect | Action |
|---|---|---|
| **Authelia / proxy down** | No one can reach the app (the proxy fronts it). The app itself keeps running; `/readyz` stays healthy (DB up). In **oidc mode**, `/readyz` fails until the issuer is reachable. | Fix the proxy/Authelia. The app needs no restart; sessions are stateless (trusted-header mode). |
| **DB unavailable / dirty schema** | `/readyz` returns 503 (`database` check); orchestrator holds traffic. | See `migrations-runbook.md` (dirty) / `backup-restore.md` (corruption). |
| **No runner registered** | Runs queue but never execute (decision #1 — control plane only). | Expected until a runner agent registers. Not an outage. |
| **SMTP / Apprise down** | Alerts fail to send; the failure is **logged, not swallowed**; the run path is unaffected (async dispatch). | Check logs (`notification dispatch failed`); fix the mail/Apprise endpoint. |
| **Vault down** (if enabled) | Vault-source secret reveals return "unavailable"; stored (local-KEK) secrets unaffected. | Fix Vault; or the secret can be re-stored locally. |

## Rollback plan

1. **App rollback:** pin the previous image tag in `docker-compose.yml`
   (`amadeus` service) and `docker compose up -d amadeus`. The binary is
   forward-only on schema (see below), so only roll back to a tag whose schema
   version ≤ the current DB version, unless you also restore a snapshot.
2. **Data rollback:** restore the last good snapshot per `backup-restore.md`
   (stop → swap DB file → clear WAL/SHM → start). Re-supply the KEK out-of-band.
3. **Verify:** `/readyz` healthy, `verify-deployment.sh` clean.

## Failed / dirty migration

See `migrations-runbook.md`. Summary: stop → restore last good snapshot →
roll the image back to the prior tag → fix-forward in a new migration. Never
hand-patch the live schema; never run `migrate down` in production.

## Restore drill (D.2)

Run periodically, not just at go-live:
1. Follow `backup-restore.md` against a recent snapshot in a scratch environment.
2. Confirm the **KEK is available** and stored-source secrets reveal post-restore
   (a restore without the KEK leaves secrets unrecoverable — that's the test).
3. Confirm `/readyz` healthy and a spot-check query returns expected rows.
4. If S3 upload is configured, confirm a fresh nightly object actually lands in
   the bucket (`aws s3 ls` / `mc ls`).

## Bootstrap-admin re-enable (lockout recovery)

If all admin mappings are lost (e.g. AD group renamed) and no one can administer:
1. Set `AMADEUS_BOOTSTRAP_ADMIN_GROUP=<a group you control>` in `amadeus.env`.
2. `docker compose up -d amadeus`. A loud warning logs while it's active.
3. Log in (you're now admin), fix `ad_group_mappings` in Settings.
4. **Remove the var** and `docker compose up -d amadeus` again. Confirm the
   warning stops logging.

## Shutdown drain

The app drains for **20s** on SIGTERM (in-flight long-polls / log-ingest finish).
Ensure the orchestrator's termination grace period is **≥ 20s** (compose
`stop_grace_period`, k8s `terminationGracePeriodSeconds`) so drains aren't cut
short. Verify under load (D.4): SIGTERM mid-soak, confirm clean exit.

## Logging & retention

- slog → stdout; set `AMADEUS_LOG_LEVEL` / `AMADEUS_LOG_FORMAT=json`; ship stdout
  to your collector. Health-probe lines are at debug to keep the access log clean.
- Retention sweep + nightly `VACUUM INTO` (+ optional S3 upload) run every 24h
  (`internal/backup`); windows via `AMADEUS_RETENTION_*`. Confirm they run on
  schedule (look for the periodic sweep log line).

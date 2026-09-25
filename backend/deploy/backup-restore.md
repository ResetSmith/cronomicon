# Backup & restore runbook

Cronomicon keeps all durable state in one SQLite file on the mounted volume
(`/var/lib/cronomicon/cronomicon.db`). Backups are a nightly consistent snapshot
shipped offsite to S3-compatible storage.

## How the nightly backup works

The retention worker (`internal/db/retention.go`), started at boot, runs the
sweep at a fixed **wall-clock time** (default **02:00 UTC**, `CRONOMICON_BACKUP_AT`)
and re-arms a timer for the next occurrence each day. It also runs one
**boot catch-up** sweep at startup *iff* the last successful backup is overdue
(older than ~24h or never) — gated on the persisted last-success time so a
crash-loop or frequent redeploy backs up at most ~once/day rather than on every
boot. Anchoring to wall-clock time rather than a 24h-from-boot ticker matters in
environments that restart more often than daily, where such a ticker would never
fire and the only durable backup could silently never run. The sweep:

1. Prunes aged rows per the retention policy (90d runs/activity/workflow_runs,
   1yr change_log/schedule_pushes — configurable).
2. `VACUUM INTO /var/lib/cronomicon/backups/cronomicon-YYYYMMDD.db` — a consistent
   snapshot taken without locking out live traffic (WAL). The write is
   **idempotent**: a same-UTC-day re-run removes the prior snapshot first,
   so a second sweep can't abort on SQLite's overwrite refusal *after*
   the prune already ran.
3. Uploads the snapshot to `s3://<bucket>/cronomicon-backups/cronomicon-YYYYMMDD.db`
   when S3 is configured (`internal/backup`). If unconfigured, the snapshot is
   kept locally only (the step is skipped — the S3 upload is optional).
4. On success, records the last-success time (flat-KV `settings.backupLastSuccessAt`)
   and sets the `cronomicon_backup_last_success_timestamp_seconds` gauge; on failure
   increments `cronomicon_backup_failures_total`.

### Monitoring

Two Prometheus series are exported at `/metrics`:

- `cronomicon_backup_last_success_timestamp_seconds` (gauge) — Unix time of the last
  successful backup.
- `cronomicon_backup_failures_total` (counter) — sweep failures.

Recommended alert (a backup is overdue / has been silently failing):

```
time() - cronomicon_backup_last_success_timestamp_seconds > 129600   # 36h
```

## Configuration (environment)

| Var | Notes |
|---|---|
| `CRONOMICON_BACKUP_S3_BUCKET` | target bucket; **empty disables upload** |
| `CRONOMICON_BACKUP_S3_ENDPOINT` | host:port for S3-compatible (MinIO/Ceph/R2); empty ⇒ AWS `s3.<region>.amazonaws.com` |
| `CRONOMICON_BACKUP_S3_REGION` | default `us-east-1` |
| `CRONOMICON_BACKUP_S3_ACCESS_KEY` / `_SECRET_KEY` | static credentials (env, not the DB). **Omit both to use the ambient IAM credential chain** — see below |
| `CRONOMICON_BACKUP_S3_USE_SSL` | default `true` |
| `CRONOMICON_BACKUP_AT` | daily sweep time, `HH:MM` UTC; default `02:00` |

> **Credential resolution.** When both `_ACCESS_KEY` and
> `_SECRET_KEY` are set, those static keys are used. When
> either is omitted, the uploader falls back to the standard AWS credential
> chain: `AWS_*` env vars → shared credentials file → IAM (EC2 instance
> profile / ECS task role / EKS IRSA web-identity token). In a cloud
> orchestrator, leave the static keys unset and grant the container/instance
> role `s3:PutObject` (+ `s3:GetObject` for restore) on the bucket. The init
> log line `s3 backup client initialized creds=...` reports which path was
> chosen (`static` vs `ambient chain (env/file/IAM)`); no key bytes are logged.

> **KEK invariant:** the stored-secret KEK must **not** live in this bucket. A
> bucket compromise must not yield both the encrypted DB and the key that
> decrypts its secrets. Back the KEK up separately.

## Restore

SQLite restore is a file swap — no import step.

### Tooling: `cronomicon restore`

The binary bundles a restore subcommand that scripts the download + swap + verify
steps below, reading the same `CRONOMICON_BACKUP_S3_*` / `CRONOMICON_DB_PATH` env the
server uses. **Stop the server first** — the swap replaces the live `.db` and its
`-wal`/`-shm` sidecars.

```
cronomicon restore --list                          # show available snapshots (newest first)
cronomicon restore                                 # restore the LATEST snapshot over CRONOMICON_DB_PATH
cronomicon restore --from cronomicon-20260722.db      # restore a specific snapshot
cronomicon restore --db /var/lib/cronomicon/cronomicon.db --yes   # non-interactive
```

It refuses an obviously-active DB (a best-effort write-lock probe — not a
substitute for stopping the service), prompts for confirmation (skip with
`--yes`), removes the stale WAL/SHM sidecars, installs the snapshot, and runs
`PRAGMA integrity_check` plus a row-count sanity check. Then start the server to
apply migrations and re-supply the KEK/OIDC keys (step 5 below).

### Manual restore (equivalent steps)

1. **Stop** the container/process (so nothing holds the DB open).
2. Fetch the snapshot:
   `aws s3 cp s3://<bucket>/cronomicon-backups/cronomicon-YYYYMMDD.db ./restore.db`
   (or `mc cp` for MinIO).
3. Replace the live DB and clear stale WAL/SHM sidecars:
   ```
   rm -f /var/lib/cronomicon/cronomicon.db /var/lib/cronomicon/cronomicon.db-wal /var/lib/cronomicon/cronomicon.db-shm
   cp ./restore.db /var/lib/cronomicon/cronomicon.db
   ```
4. **Start** the process. On boot it applies any pending migrations and
   `/readyz` reports `database: ok` once schema state is clean.
5. Re-supply out-of-band material that does **not** live in the DB: the secret
   KEK (`CRONOMICON_KEK*`) and
   OIDC/session keys. Without the original KEK,
   stored secrets cannot be decrypted (vault-source secrets are unaffected).

> **Rehearse before relying on it.** This procedure has not yet been exercised
> end-to-end against a live stack. Run it on staging first: snapshot → fresh binary
> on an empty volume → restore → confirm `/readyz` `database: ok`,
> `integrity_check`, and that a stored secret decrypts with the re-supplied KEK
> (and fails without it). Record timings and fold any corrections back here.

## After a restore: runners

A restore does not end when `/readyz` goes green. The runner fleet reconciles
itself against the database you just restored, and one of the outcomes is a
runner that looks healthy and does no work. Work through this before declaring
the restore complete.

**Runners whose rows predate the restore point** resume on their own — their API
keys still validate against the restored `runners` table. Nothing to do.

**Runners enrolled after the restore point** are absent from the restored
database. On their next poll they get a 404 (row gone) or a 401 (token
rejected), discard their stored identity, and attempt to register again. What
happens next depends on which token they hold:

| The runner presents | Outcome |
|---|---|
| The server's `CRONOMICON_RUNNER_BOOTSTRAP_TOKEN` | Re-registration completes unattended. Nothing to do. |
| A single-use `crn_reg_*` from its original install | Re-registration **fails** `token_used`. The runner is offline until an operator mints a fresh token, places it on the host, and restarts the unit. |

Note that the agent discards its identity *before* it attempts to register, so a
runner in the second case cannot fall back to its old key — it stays down until
attended to. Per host.

> **A long outage widens this.** A runner that stays offline past
> `CRONOMICON_RUNNER_DEREGISTER_AFTER` (default **14 days**) is reaped, which
> deletes its row. After a multi-week incident the second case is the normal
> case, not the edge case — plan for re-enrollment across the fleet rather than
> for a handful of stragglers.

### Re-registered runners come back unbound

**This is the step most likely to be missed.** A runner that re-registers gets a
**new id**, so it has no agency membership and no tags — those are keyed to the
id that was just deleted, and they are operator-owned, never self-declared by
the agent. The runner will:

- report itself **online**,
- re-detect and advertise its capabilities correctly,
- show zero load,
- and **silently change which work it is eligible for**.

That last point is the one to understand, because "unbound" is not "idle". The
claim predicate is a **disjoint** two-branch rule: an agency-tagged run goes only
to a member of one of its agencies, and an **untagged run goes ONLY to a runner
with no agencies at all**. So a runner that lost its placement has not gone
quiet — it has moved out of its department's pool and **into the shared general
pool**, where it can now claim untagged work it was previously excluded from,
while no longer claiming the departmental runs it existed to serve.

That is an isolation change, not just an availability one. Green status is not
evidence that the right work can be dispatched. Before declaring the restore
complete, open **Runners** and check the per-agency coverage, then review every
runner that came back unplaced.

**Placement history makes the re-bind a confirmation, not a reconstruction.**
Before a runner row is deleted, by an operator deregistering it or by the reaper
sweeping it past `CRONOMICON_RUNNER_DEREGISTER_AFTER`, the server snapshots its
agency membership and tags into `runner_placement_history`. When a runner
re-registers unplaced and a snapshot matches, its row in **Runners** shows
**Previous placement found**; expand the row to review the prior agencies and
tags, then **Restore placement** to re-apply them in one step, or dismiss the
suggestion. The match is offered for an operator to confirm, never applied
automatically: the runner name is self-declared by the agent, so healing by name
alone would let any agent inherit another runner's agency by claiming its name.
The observed client IP of the runner's last contact is recorded beside the
snapshot as the one signal the agent cannot assert. Accept and dismiss carry the
same agency gate as manual placement (an unrestricted operator for the general
pool).

> The snapshot only exists in the database you restored: a runner reaped or
> deregistered *after* the restore point has no history row, and comes back with
> no suggestion. Those runners are re-bound by hand (agency, then tags). Either
> way the check does not go away: an unreviewed runner is an unplaced runner.

### Verifying from the runner side

The agent logs to journald. On each host:

```
journalctl -u cronomicon-runner -f
```

Three strings tell you which path a runner took:

| Log line | Meaning |
|---|---|
| `resumed runner identity` | Its row survived the restore. Healthy, no action. |
| `re-registering` | Its row was absent; it is re-enrolling. Expect it to reappear with a new id — **and unbound**. |
| `re-register failed` | Re-enrollment was refused (usually `token_used`). This runner needs a fresh token and a unit restart. |

Filtering for just the relevant lines:

```
journalctl -u cronomicon-runner --since '30 min ago' | grep -Ei 'register|401|404'
```

## Verifying a backup

`VACUUM INTO` output is a complete, openable database. To spot-check a snapshot:
`sqlite3 cronomicon-YYYYMMDD.db 'PRAGMA integrity_check; SELECT count(*) FROM runs;'`
(`cronomicon restore` runs this check automatically after installing a snapshot.)

> Backups are configured **only** via `CRONOMICON_BACKUP_S3_*` env (see Configuration
> above) — there is no DB-stored backup setting.

## Load / soak testing

The two endurance paths to exercise before going to production (not part of CI):
- **Runner long-poll** (`GET /runners/{id}/poll`): many idle runners holding
  30s long-polls — verify goroutine/connection headroom under the single-process
  model.
- **Log ingest** (`POST /runs/{traceId}/log`): sustained chunked streaming with
  mid-stream reconnects (`X-Resume-Offset`) — verify append throughput and that
  redaction keeps up at ingest.
A simple `vegeta`/`k6` script against a seeded instance covers both.

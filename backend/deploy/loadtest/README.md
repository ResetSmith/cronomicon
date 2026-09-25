# Load / soak tests (Phase D.4)

The two endurance paths to exercise before go-live (the carry-over open item from
`todos.md` B8). Run against a **seeded staging/ops instance — never production**.

| Script | Path under test | Why it's the risk |
|---|---|---|
| `poll.js` | `GET /runners/{id}/poll` (long-poll, A6.2) | Many idle runners hold 30s polls — goroutine/connection headroom under the single-process model (T3). |
| `log-ingest.js` | `POST /runs/{traceId}/log` (chunked, T6) | Sustained streaming + mid-stream reconnect (`X-Resume-Offset`); append throughput while redaction runs at ingest (T7/S7). |

## Running (k6)

```sh
# Long-poll soak: 200 virtual runners for 10 minutes.
BASE=https://cronomicon.staging.example.com RUNNER_TOKEN=crn_run_xxx \
  k6 run --vus 200 --duration 10m poll.js

# Log-ingest throughput: 50 streams for 5 minutes against pre-claimed runs.
BASE=https://cronomicon.staging.example.com RUNNER_TOKEN=crn_run_xxx \
  TRACE_IDS=run-1,run-2,run-3 \
  k6 run --vus 50 --duration 5m log-ingest.js
```

Pre-seed: register the load runner(s) and capture bearer tokens; for ingest,
queue and claim runs (a mock runner) so the trace ids are in `running` state.

A `vegeta` equivalent works too — point it at the same endpoints with the bearer
header; k6 is preferred for the long-poll case (it holds connections cleanly).

## What to watch (D.4 acceptance)

- **SQLite under concurrency:** WAL mode active, `busy_timeout` set, and the
  connection-pool **size invariant** (the pool must stay > 1 — a pool of 1
  deadlocks on nested queries). Watch for `database is locked` / `busy` errors.
- **Memory:** steady-state RSS over the soak window (no leak from held polls).
- **Goroutines / FDs:** `go_goroutines` and process FD count on `/metrics` should
  plateau, not climb, with N idle long-polls.
- **Graceful shutdown:** send SIGTERM mid-load and confirm the 20s drain completes
  within the orchestrator's termination grace period (no dropped in-flight work).

Capture `/metrics` before/during/after (`cronomicon_http_request_duration_seconds`,
`cronomicon_log_ingest_*`, `go_goroutines`, `process_resident_memory_bytes`).

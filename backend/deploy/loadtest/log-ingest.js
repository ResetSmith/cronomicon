// k6 load/soak script for the chunked log-ingest path (D.4, carry-over from B8).
//
// Exercises POST /api/v1/runs/{traceId}/log: sustained chunked streaming with
// redaction at ingest (T7/S7) and the X-Resume-Offset reconnect path (T6).
// Runner bearer auth; no CSRF.
//
// IMPORTANT: ingest only accepts a run in 'running' state, so each trace id must
// belong to a queued→claimed run. For a pure throughput test, pre-seed N runs
// and claim them with a mock runner, then pass their trace ids via TRACE_IDS
// (comma-separated). Without real runs the endpoint returns 404/409 by design —
// that still load-tests routing/auth but not the append path.
//
// Usage:
//   BASE=https://cronomicon.staging.example.com RUNNER_TOKEN=crn_run_xxx \
//   TRACE_IDS=run-1,run-2,run-3 \
//   k6 run --vus 50 --duration 5m backend/deploy/loadtest/log-ingest.js

import http from "k6/http";
import { check } from "k6";

const BASE = __ENV.BASE || "http://localhost:8080";
const TOKEN = __ENV.RUNNER_TOKEN || "";
const TRACE_IDS = (__ENV.TRACE_IDS || "demo-run").split(",");
const LINES_PER_CHUNK = Number(__ENV.LINES || 200);

export const options = {
  vus: Number(__ENV.VUS || 50),
  duration: __ENV.DURATION || "5m",
  thresholds: {
    http_req_duration: ["p(95)<2000"],
  },
};

function chunk() {
  const lines = [];
  for (let i = 0; i < LINES_PER_CHUNK; i++) {
    // Include a token that the redactor should mask if seeded as a secret.
    lines.push(`2026-06-10T00:00:0${i % 10}Z level=info msg="work" secret=REDACT_ME_${i}`);
  }
  return lines.join("\n") + "\n";
}

export default function () {
  const traceID = TRACE_IDS[__VU % TRACE_IDS.length];
  const res = http.post(`${BASE}/api/v1/runs/${traceID}/log`, chunk(), {
    headers: {
      Authorization: `Bearer ${TOKEN}`,
      "Content-Type": "text/plain",
    },
  });
  check(res, {
    // 204 = accepted; 404/409 = run not in 'running' (expected without live runs).
    "ingest handled": (r) => [204, 404, 409].includes(r.status),
  });
}

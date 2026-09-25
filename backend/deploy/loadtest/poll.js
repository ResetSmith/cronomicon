// k6 load/soak script for the runner long-poll path (D.4, carry-over from B8).
//
// Simulates many idle runners holding 30s long-polls against
// GET /api/v1/runners/{id}/poll — the endurance concern under the single-process
// model (T3): goroutine + connection headroom while most polls return 204.
//
// Runners authenticate with a per-runner bearer token (T9), NOT the operator
// session — so this exercises the RequireRunner path with no CSRF.
//
// Usage (against a seeded ops/staging instance — never production):
//   BASE=https://amadeus.staging.example.com \
//   RUNNER_ID=ldtest-1 RUNNER_TOKEN=crn_run_xxx \
//   k6 run --vus 200 --duration 10m backend/deploy/loadtest/poll.js
//
// Pre-seed: register the runner(s) and capture their bearer tokens first. Vary
// RUNNER_ID per VU if you want distinct runners (see __VU below).

import http from "k6/http";
import { check } from "k6";

const BASE = __ENV.BASE || "http://localhost:8080";
const TOKEN = __ENV.RUNNER_TOKEN || "";
const RUNNER_BASE = __ENV.RUNNER_ID || "ldtest";

export const options = {
  // Override on the CLI with --vus/--duration. These are sane soak defaults.
  vus: Number(__ENV.VUS || 200),
  duration: __ENV.DURATION || "10m",
  thresholds: {
    // Long-poll returns within ~30s server-side; allow margin. 204/200 only.
    http_req_failed: ["rate<0.01"],
  },
};

export default function () {
  // Each VU acts as its own runner id so heartbeats/last_seen spread out.
  const runnerID = `${RUNNER_BASE}-${__VU}`;
  const res = http.get(`${BASE}/api/v1/runners/${runnerID}/poll`, {
    headers: { Authorization: `Bearer ${TOKEN}` },
    timeout: "35s", // > server pollTimeout (30s)
  });
  check(res, {
    "poll status ok": (r) => r.status === 200 || r.status === 204,
  });
}

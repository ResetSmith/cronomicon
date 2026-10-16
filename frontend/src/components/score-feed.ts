import type { Mark } from "./score-model";

// The Score's two feeds made into one set of marks (shared by the Dashboard
// and, since 2.4.4 F4, the Operations overview, so that the two cannot draw
// the same runs differently). The feeds are GET /runs and
// GET /schedules/upcoming; the caller fetches them, filters suppressed fires
// (excludeSuppressed) and says how each mark is named.

/** The fields of a run row the Score reads. */
export interface ScoreRun {
  traceId?: string;
  jobName?: string;
  jobUid?: string | null;
  agencies?: string[];
  /** Whose the run is (2.4.4). */
  ownerAgency?: string | null;
  type?: string | null;
  status?: string;
  startedAt?: string | null;
  durationMs?: number | null;
  triggeredBy?: string | null;
  kind?: string | null;
  triggerKind?: string | null;
  scheduleName?: string | null;
  executor?: string | null;
  runnerId?: string | null;
  runnerName?: string | null;
}

/** The fields of a projected fire the Score reads. */
export interface ScoreUpcoming {
  ownerKind?: string;
  ownerName?: string;
  ownerUid?: string;
  ownerAgencies?: string[];
  at?: string | null;
}

export interface ScoreNamer {
  run: (r: ScoreRun) => string;
  upcoming: (u: ScoreUpcoming) => string;
  /** Where the Score spans agencies: each mark's agency, for the readout's Agency column. */
  runAgency?: (r: ScoreRun) => string;
  upcomingAgency?: (u: ScoreUpcoming) => string;
}

/**
 * buildScoreMarks — past runs and future fires as one tense-split set of
 * marks. A run is plotted at its START (the API filters on enqueue time, so a
 * run with no startedAt has not begun and has no place on the timeline);
 * ssh-test probes are not jobs and are left out.
 */
export function buildScoreMarks(
  runs: ScoreRun[],
  upcoming: ScoreUpcoming[],
  runnerNames: Map<string, string>,
  name: ScoreNamer,
): Mark[] {
  const out: Mark[] = [];
  for (const r of runs) {
    const at = r.startedAt ? Date.parse(r.startedAt) : NaN;
    if (Number.isNaN(at) || r.kind === "ssh-test") continue;
    out.push({
      at,
      tense: "past",
      jobName: name.run(r) || "—",
      status: r.status,
      durationMs: r.durationMs ?? null,
      traceId: r.traceId,
      type: r.type,
      triggerKind: r.triggerKind,
      triggeredBy: r.triggeredBy,
      scheduleName: r.scheduleName,
      executor: r.executor,
      runnerName: r.runnerName ?? (r.runnerId ? (runnerNames.get(r.runnerId) ?? null) : null),
      ...(name.runAgency ? { agency: name.runAgency(r) } : {}),
    });
  }
  for (const u of upcoming) {
    const at = u.at ? Date.parse(u.at) : NaN;
    if (Number.isNaN(at)) continue;
    out.push({
      at,
      tense: "future",
      jobName: name.upcoming(u) || "—",
      ownerKind: u.ownerKind,
      ...(name.upcomingAgency ? { agency: name.upcomingAgency(u) } : {}),
    });
  }
  return out;
}

/**
 * scoreTruncatedAt — D-2b. The projection gives no truncation signal (both
 * caps are silent), so a full 500 is the only inference available; read from
 * the RAW projection, before suppressed fires are filtered out.
 */
export function scoreTruncatedAt(upcomingAll: { at?: string | null }[]): number | null {
  const last = upcomingAll[upcomingAll.length - 1];
  return upcomingAll.length >= 500 && last?.at ? Date.parse(last.at) : null;
}

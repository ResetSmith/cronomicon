// The Score's maths, with no React and no DOM in it.
//
// Everything here is a pure function over plain data: the window, the time↔x
// scale, the coincident-run grouping, the stem heights, and pointer→nearest-mark
// resolution. Score.tsx is then only rendering and event plumbing.
//
// The split is not tidiness for its own sake. The defect this component exists
// to avoid — a failed run hidden behind a successful one (VU-21) — is a property
// of the grouping function, and a property is worth testing directly rather than
// through a rendered SVG.

/** A run that happened, or a fire that is projected to happen. */
export interface Mark {
  /** Epoch ms. For a run, when it started; for a projection, when it will fire. */
  at: number;
  /** Filled noteheads have run; hollow ones are scheduled. The tense split. */
  tense: "past" | "future";
  jobName: string;
  /** Canonical run status (success | warning | danger | running | queued | …). Absent on projections. */
  status?: string | null;
  durationMs?: number | null;
  traceId?: string | null;
  /** "job" | "workflow" — projections carry it; runs infer it. */
  ownerKind?: string | null;
  /** Run type (bash/ansible/…), for the full score's grouping. */
  type?: string | null;
  // SR-1 — the readout's per-run detail (runs only; absent on projections).
  /** How the run was caused: manual | scheduled | webhook | reaction | workflow. */
  triggerKind?: string | null;
  /** Who pressed Run, for a manual run. */
  triggeredBy?: string | null;
  /** The schedule entry that fired it, for a scheduled run. */
  scheduleName?: string | null;
  /** "ssh" | "runner" — frozen on the run at trigger. */
  executor?: string | null;
  /** The runner that claimed it, when the executor was a runner and it is still registered. */
  runnerName?: string | null;
}

/**
 * SR-1 — one phrase for how a run came to be. Reads the trigger kind first,
 * because `triggeredBy` is a person only for manual runs, and falls back to
 * the raw kind rather than inventing a label for one it has never seen.
 */
export function triggerLabel(m: Pick<Mark, "triggerKind" | "triggeredBy" | "scheduleName">): string {
  const kind = (m.triggerKind ?? "").toLowerCase();
  if (kind === "manual" || (!kind && m.triggeredBy)) return m.triggeredBy ? `by ${m.triggeredBy}` : "manual";
  if (kind === "scheduled" || kind === "cron") return m.scheduleName ? `schedule ${m.scheduleName}` : "scheduled";
  if (kind === "reaction") return "reaction";
  if (kind === "webhook") return "token or file";
  if (kind === "workflow") return "workflow step";
  return kind || "";
}

/** SR-1 — "SSH" or "Runner · name"; the executor alone when the runner is unknown. */
export function executorLabel(m: Pick<Mark, "executor" | "runnerName">): string {
  if (!m.executor) return "";
  if (m.executor === "ssh") return "SSH";
  return m.runnerName ? `Runner · ${m.runnerName}` : "Runner";
}

/** Marks that share an exact instant, collapsed into one drawable notehead. */
export interface MarkGroup {
  at: number;
  tense: "past" | "future";
  members: Mark[];
  /** The group's outcome — the WORST of its members. Never a majority vote. */
  status?: string | null;
  /** Longest member run; the stem is sized from this. */
  durationMs?: number | null;
}

// ── Window ───────────────────────────────────────────────────────────────────
// Fixed, and deliberately not a range picker (VU-Q8): the picker the old
// histogram carried existed to make a bar chart useful at several scales, and a
// timeline that shows both tenses does not have that problem. 36h also keeps the
// forward half inside /schedules/upcoming's projection caps, which a 7d window
// did not — at 7d a */30 schedule projects 336 fires against a per-entry cap of
// 200 and is silently truncated.

export const HOURS_BACK = 24;
export const HOURS_AHEAD = 12;
const SPAN_MS = (HOURS_BACK + HOURS_AHEAD) * 3600_000;

export interface Window {
  start: number;
  now: number;
  end: number;
}

/** `now` is injected rather than read from the clock so every test is deterministic. */
export function scoreWindow(now: number): Window {
  return { start: now - HOURS_BACK * 3600_000, now, end: now + HOURS_AHEAD * 3600_000 };
}

/** Fraction of the window, 0..1. Values outside the window are NOT clamped —
 *  callers filter first, and a silent clamp would stack off-window marks on the
 *  edge as if they were real events there. */
export function frac(w: Window, at: number): number {
  return (at - w.start) / (w.end - w.start);
}

export function inWindow(w: Window, at: number): boolean {
  return at >= w.start && at <= w.end;
}

// ── Grouping (VU-21) ─────────────────────────────────────────────────────────
// Marks sharing an x-position paint in array order, so the last one drawn covers
// the others. Measured on the prototype: ~a quarter of marks share an instant
// with another — `0 6 * * *` fires two jobs, and */30 collides with every
// top-of-hour schedule. That is not a crowding artefact and a wider window does
// not help.
//
// The fix is to collapse them into ONE notehead carrying a count, coloured by the
// worst outcome present. Not by the first, not by the most common: if any member
// failed, the group reads as a failure. A component whose main job is surfacing
// failures must not be able to hide one.
//
// Deliberately NOT solved by nudging marks apart (beeswarm fanning): this is a
// time axis, and moving a mark off its true time trades one inaccuracy for
// another.

/** Worst-first. Anything unrecognised sorts below a known-bad status but above success. */
const SEVERITY: Record<string, number> = {
  danger: 5,
  failed: 5,
  failure: 5,
  killed: 5,
  warning: 4,
  warn: 4,
  running: 3,
  queued: 3,
  success: 1,
  ok: 1,
  skipped: 0,
  cancelled: 0,
};

export function severity(status?: string | null): number {
  if (!status) return 2;
  return SEVERITY[status] ?? 2;
}

/** The worst status among the members — the group's colour. */
export function worstStatus(members: Mark[]): string | null | undefined {
  if (!members.length) return undefined;
  return members.reduce((a, b) => (severity(b.status) > severity(a.status) ? b : a)).status;
}

// ── Clustering (FX-5) ────────────────────────────────────────────────────────
// Exact-instant grouping was too strict to do the job it was written for. It
// merges `0 6 * * *` firing two jobs, because those share a millisecond — but two
// runs launched seconds or minutes apart get distinct noteheads at
// indistinguishable x, and pile up exactly as if nothing grouped them.
//
// The bucket is derived from the geometry, not chosen: at the plot's typical
// width a notehead is ~12px including its gap, and 12px of a 36-hour window is
// ~18 minutes. Marks closer together than that ALREADY overlap on screen; the
// only question was whether they overlapped as one honest mark carrying a count
// or as a smear of unreadable ones. So the merge never moves a mark further than
// the width of the mark itself — which is why this does not reopen the recorded
// "no jitter/beeswarm" decision below. That decision rejected moving marks off
// their true time to make room; this changes nothing about where a mark is drawn
// beyond sub-notehead distance, and it removes the overlap rather than arranging
// it.
//
// A cluster anchors on its EARLIEST member rather than its mean. Two reasons:
// the anchor is stable across the Dashboard's 4-second poll (a mean shifts every
// time a member joins, and `${tense}@${at}` is the identity the hover and the
// keyboard cursor are held by, so a shifting anchor would drop the selection),
// and it keeps a lone mark exactly where it has always been.

/** Typical rendered plot width in CSS px. The plot is fluid, so this is the
 *  assumption the bucket is sized from rather than a measurement — a container
 *  ref would make the grouping depend on layout, which is what keeps this module
 *  free of the DOM. Being wrong by a few hundred px moves the bucket by minutes,
 *  and the whole point is that minutes at this scale are invisible. */
const PLOT_WIDTH_PX = 1400;
/** One notehead (2 × NOTE_R) plus the gap that would separate two of them. */
const CLUSTER_PX = 12;
/** ≈ 18.5 minutes on the fixed 36-hour window. */
export const CLUSTER_MS = Math.round((SPAN_MS * CLUSTER_PX) / PLOT_WIDTH_PX);

/**
 * Group marks that would overlap on screen into one drawable notehead. Past and
 * future never merge: a run that happened and a fire that is projected are
 * different tenses and must stay visually distinct even when they land on the
 * same millisecond.
 *
 * `clusterMs` is the merge window, defaulting to one notehead's worth of time.
 * Pass 0 for exact-instant grouping.
 */
export function groupMarks(marks: Mark[], clusterMs: number = CLUSTER_MS): MarkGroup[] {
  const byTense = new Map<Mark["tense"], Mark[]>();
  for (const m of marks) {
    const bucket = byTense.get(m.tense);
    if (bucket) bucket.push(m);
    else byTense.set(m.tense, [m]);
  }

  const out: MarkGroup[] = [];
  for (const [tense, inTense] of byTense) {
    // Sorted, then swept: each cluster admits marks within `clusterMs` of its
    // ANCHOR, never of the previous member — otherwise a dense enough run of
    // marks would chain into one arbitrarily wide cluster, and the bound on how
    // far a mark can be drawn from its true time would be lost.
    const sorted = [...inTense].sort((a, b) => a.at - b.at);
    let members: Mark[] = [];
    const flush = () => {
      if (!members.length) return;
      // The anchor is the EARLIEST member, captured before the severity sort
      // below — the sort must not move the group's time (or its identity key,
      // which the hover and keyboard cursor are held by).
      const at = members[0].at;
      // Members are handed to the renderer worst-first: the chord stack draws
      // them bottom-up from the staff line, so the worst outcome sits ON the
      // line where the eye tracks, and an overflow can only ever fold away the
      // mildest members. Stable sort, so equal severities keep time order —
      // which is also what keeps the readout's enumeration deterministic.
      const bySeverity = [...members].sort((a, b) => severity(b.status) - severity(a.status));
      out.push({
        at,
        tense,
        members: bySeverity,
        status: worstStatus(members),
        // The longest run in the group drives the stem: a group is at least as
        // long as its longest member, and showing the shortest would understate it.
        durationMs: members.reduce<number | null>((max, m) => (m.durationMs != null && (max == null || m.durationMs > max) ? m.durationMs : max), null),
      });
      members = [];
    };
    for (const m of sorted) {
      if (members.length && m.at - members[0].at > clusterMs) flush();
      members.push(m);
    }
    flush();
  }
  return out.sort((a, b) => a.at - b.at);
}

// ── Stems ────────────────────────────────────────────────────────────────────
// Duration on a LOG scale. Linear is useless here: real durations cluster at
// 2–3 minutes while disk-usage-audit is ~14 seconds, so on a linear scale every
// stem but the outlier is the same height.

export const STEM_MIN = 7;
export const STEM_MAX = 56;

export function stemHeight(durationMs?: number | null): number {
  if (durationMs == null || durationMs <= 0) return STEM_MIN;
  // 1s → floor, ~2h → ceiling; log10 over that range.
  const lo = Math.log10(1000);
  const hi = Math.log10(2 * 3600_000);
  const t = (Math.log10(durationMs) - lo) / (hi - lo);
  return STEM_MIN + Math.max(0, Math.min(1, t)) * (STEM_MAX - STEM_MIN);
}

// ── Hover (D-3) ──────────────────────────────────────────────────────────────
// Resolve pointer → time → NEAREST mark, rather than giving each mark a hit box.
// The prototype proved why: at ~14px wide, marks minutes apart overlap and the
// last one painted swallows its neighbours — three of six seeded runs were
// literally unhoverable. Resolving on the track means every mark is reachable,
// including one sitting underneath another.

/** Within 3% of the window span — about 65 minutes on a 36h window. */
export const NEAREST_TOLERANCE = 0.03;

export function nearestGroup(groups: MarkGroup[], w: Window, at: number): MarkGroup | null {
  if (!groups.length) return null;
  let best: MarkGroup | null = null;
  let bestDist = Infinity;
  for (const g of groups) {
    const d = Math.abs(g.at - at);
    if (d < bestDist) {
      bestDist = d;
      best = g;
    }
  }
  return best && bestDist <= NEAREST_TOLERANCE * (w.end - w.start) ? best : null;
}

// ── Axis ─────────────────────────────────────────────────────────────────────

export interface Tick {
  at: number;
  /** Every 6th hour gets a bar line and a label; the rest are hour ticks. */
  major: boolean;
  /** Midnight in the application timezone — the day boundary. */
  dayBoundary: boolean;
}

/**
 * Hour ticks across the window. `hourInZone` maps an instant to its hour-of-day
 * in the APPLICATION timezone — the same zone the scheduler fires in — so the
 * 6-hour bar lines and the midnight rule land where an operator expects rather
 * than where the browser's locale happens to put them.
 */
export function ticks(w: Window, hourInZone: (at: number) => number): Tick[] {
  const out: Tick[] = [];
  const first = Math.ceil(w.start / 3600_000) * 3600_000;
  for (let at = first; at <= w.end; at += 3600_000) {
    const h = hourInZone(at);
    out.push({ at, major: h % 6 === 0, dayBoundary: h === 0 });
  }
  return out;
}

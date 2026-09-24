// Cron helpers for the Schedule builder — kept dependency-free on purpose
// (no cron lib; the prototype's 5-field subset is all we need).

import { appZoneParts, fmtInAppZone } from "../../utils/datetime";

export interface CronPreset {
  label: string;
  expr: string;
}

export const CRON_PRESETS: CronPreset[] = [
  { label: "Every 15 min", expr: "*/15 * * * *" },
  { label: "Hourly", expr: "0 * * * *" },
  { label: "Daily 07:00", expr: "0 7 * * *" },
  { label: "Nightly 23:00", expr: "0 23 * * *" },
  { label: "Weekdays 09:00", expr: "0 9 * * 1-5" },
  { label: "Weekly Sun 02:00", expr: "0 2 * * 0" },
  { label: "Monthly 1st 00:00", expr: "0 0 1 * *" },
];

// Validates the classic 5-field cron subset: * | */n | a-b | n | comma lists.
export function validateCron(expr: string): boolean {
  const parts = expr.trim().split(/\s+/);
  if (parts.length !== 5) return false;
  const ok = (field: string, lo: number, hi: number): boolean =>
    field.split(",").every((f) => {
      if (f === "*") return true;
      if (/^\*\/\d+$/.test(f)) {
        const n = parseInt(f.slice(2), 10);
        return n >= 1 && n <= hi;
      }
      if (/^\d+-\d+$/.test(f)) {
        const [a, b] = f.split("-").map(Number);
        return a >= lo && b <= hi && a < b;
      }
      if (/^\d+$/.test(f)) {
        const n = parseInt(f, 10);
        return n >= lo && n <= hi;
      }
      return false;
    });
  const [min, hr, dom, month, dow] = parts;
  return ok(min, 0, 59) && ok(hr, 0, 23) && ok(dom, 1, 31) && ok(month, 1, 12) && ok(dow, 0, 6);
}

const DOW = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];

const dowName = (f: string): string | null => {
  if (/^\d+$/.test(f)) return DOW[parseInt(f, 10) % 7];
  if (/^\d+-\d+$/.test(f)) {
    const [a, b] = f.split("-").map(Number);
    if (a <= 6 && b <= 6) return `${DOW[a]}–${DOW[b]}`;
  }
  if (/^(\d+,)+\d+$/.test(f)) return f.split(",").map((d) => DOW[parseInt(d, 10) % 7]).join(", ");
  return null;
};

// Best-effort human summary for the common shapes; falls back to the raw expr.
export function cronToHuman(expr: string): string {
  const p = expr.trim().split(/\s+/);
  if (p.length !== 5) return "Invalid expression — expected 5 fields (min hr dom mon dow)";
  const [min, hr, dom, mon, dow] = p;
  const pad = (v: string) => v.padStart(2, "0");
  const time = /^\d+$/.test(min) && /^\d+$/.test(hr) ? `${pad(hr)}:${pad(min)}` : null;
  const day = dowName(dow);

  if (min === "*" && hr === "*" && dom === "*" && mon === "*" && dow === "*") return "Every minute";
  if (/^\*\/\d+$/.test(min) && hr === "*" && dom === "*" && mon === "*" && dow === "*")
    return `Every ${min.slice(2)} minutes`;
  if (/^\*\/\d+$/.test(hr) && /^\d+$/.test(min) && dom === "*" && dow === "*")
    return `Every ${hr.slice(2)} hours at minute ${min}`;
  if (/^\d+$/.test(min) && hr === "*" && dom === "*" && mon === "*" && dow === "*")
    return `Hourly at minute ${min}`;
  if (time && dom === "*" && mon === "*" && dow === "*") return `Daily at ${time}`;
  if (time && dom === "*" && mon === "*" && day) return `Weekly on ${day} at ${time}`;
  if (time && /^\d+$/.test(dom) && mon === "*" && dow === "*") return `Monthly on day ${dom} at ${time}`;
  return `cron: ${expr}`;
}

// Projects a canonical 6-field cron (sec min hr dom mon dow) onto the classic
// 5-field form the helpers above understand, dropping the seconds field (previewed
// at minute resolution). 5-field (or any other length) expressions pass through
// unchanged. Returns the projected expr plus whether projection happened, so a UI
// can note "seconds shown at minute resolution".
function projectTo5Field(expr: string): { expr5: string; sixField: boolean } {
  const t = expr.trim();
  const n = t ? t.split(/\s+/).length : 0;
  if (n === 6) return { expr5: t.split(/\s+/).slice(1).join(" "), sixField: true };
  return { expr5: t, sixField: false };
}

export interface CronPreview {
  raw: string;
  sixField: boolean;
  previewExpr: string;
  valid: boolean;
  human: string;
  nexts: string[];
  // nextsTruncated: the stepper ran out of budget before finding `count` fires
  // (FX2-E). A short — or empty — list with this set means "not found within
  // the preview horizon", which is a different statement from "fires this
  // often" / "never fires", and the surfaces must render the difference.
  nextsTruncated: boolean;
}

// An activation window (AW): optional ISO instants bounding when a schedule may
// fire. Either side may be absent, meaning unbounded on that side.
export interface ActivationWindow {
  startAt?: string | null;
  endAt?: string | null;
}

export type WindowState = "pending" | "active" | "expired" | null;

// Classifies a window the same way the backend does, so a badge rendered here
// matches the windowState the server reports.
export function windowState(win: ActivationWindow | undefined, now = Date.now()): WindowState {
  const start = win?.startAt ? Date.parse(win.startAt) : NaN;
  const end = win?.endAt ? Date.parse(win.endAt) : NaN;
  if (Number.isNaN(start) && Number.isNaN(end)) return null;
  if (!Number.isNaN(start) && now < start) return "pending";
  if (!Number.isNaN(end) && now > end) return "expired";
  return "active";
}

// Validates a window pair for the editors: both bounds optional, but a window
// that closes before it opens can never fire and is rejected (mirrors the
// backend's 422).
export function validateWindow(win: ActivationWindow | undefined): string | null {
  const rawStart = win?.startAt?.trim();
  const rawEnd = win?.endAt?.trim();
  const start = rawStart ? Date.parse(rawStart) : NaN;
  const end = rawEnd ? Date.parse(rawEnd) : NaN;
  if (rawStart && Number.isNaN(start)) return "Invalid start date";
  if (rawEnd && Number.isNaN(end)) return "Invalid end date";
  if (!Number.isNaN(start) && !Number.isNaN(end) && start >= end) return "Start must be before end";
  return null;
}

// One-call preview tolerant of both 5- and 6-field crons (the backend accepts
// both). `human` is always set (falls back to the raw expr); `nexts` is empty
// when the expression is too complex for the bounded stepper. Use this instead of
// calling validateCron/cronToHuman/nextCronTimes directly so every surface
// previews 6-field crons identically (JC6) rather than rendering "Invalid".
export function cronPreview(raw: string, count = 3, win?: ActivationWindow): CronPreview {
  const { expr5, sixField } = projectTo5Field(raw);
  const valid = validateCron(expr5);
  const detail = valid ? nextCronTimesDetailed(expr5, count, win) : { times: [], truncated: false };
  return {
    raw: raw.trim(),
    sixField,
    previewExpr: expr5,
    valid,
    human: expr5 ? cronToHuman(expr5) : "",
    nexts: detail.times,
    nextsTruncated: detail.truncated,
  };
}

// Next-fire preview by minute-stepping (bounded), matching the prototype.
// Supports * | */n | n on minute/hour/dow; anything fancier returns [].
//
// The match is evaluated against each candidate instant's wall clock IN THE APP
// ZONE (not the browser's) and the result is rendered in the app zone, so the
// live editor preview agrees with the zone the server fires in (timezone-update
// §5.3, OD-TZ2). It is a convenience approximation — imprecise at DST edges; the
// authoritative next-run/upcoming come from the server. Surfaces label this
// "(app zone)".
// An optional activation window clamps the preview the same way it clamps real
// fires (AW-12): stepping starts at the window opening and stops at its close,
// so the editor never previews a run the scheduler would not perform.
export function nextCronTimes(expr: string, count = 3, win?: ActivationWindow): string[] {
  return nextCronTimesDetailed(expr, count, win).times;
}

// nextCronTimesDetailed is nextCronTimes plus the truncation fact (FX2-E): when
// the iteration budget runs out before `count` fires are found, the short list
// is a HORIZON statement, not a frequency one — and without the flag a sparse
// expression (leap day; an impossible date like Feb 31) previews as "fires
// less often than asked" or "never fires" while the engine disagrees.
export function nextCronTimesDetailed(
  expr: string,
  count = 3,
  win?: ActivationWindow,
): { times: string[]; truncated: boolean } {
  const none = { times: [], truncated: false };
  const parts = expr.trim().split(/\s+/);
  if (parts.length !== 5) return none;
  // FX-F2 — ALL FIVE fields. This matcher destructured [minF, hrF, , , dowF]
  // and silently ignored day-of-month and month, while validateCron accepted
  // them and CRON_PRESETS shipped "Monthly 1st 00:00" — which therefore
  // previewed as DAILY at 00:00. The server's projection was always right; the
  // live preview in the editor was the only surface lying, which is the worst
  // one, because it lies at the moment the operator is deciding.
  const [minF, hrF, domF, monF, dowF] = parts;
  const simple = (f: string) => f === "*" || /^\*\/\d+$/.test(f) || /^\d+$/.test(f);
  if (![minF, hrF, domF, monF, dowF].every(simple)) return none;
  // base handles */N on 1-based fields: cron steps count from the field's
  // minimum, so dom */5 means 1,6,11,… — (val-1)%5===0 — not val%5===0.
  const match = (field: string, val: number, base = 0): boolean => {
    if (field === "*") return true;
    if (field.startsWith("*/")) return (val - base) % parseInt(field.slice(2), 10) === 0;
    return parseInt(field, 10) === val;
  };
  // Vixie/robfig day rule, mirrored from the library the engine fires with
  // (robfig/cron v3): when BOTH dom and dow are restricted, a day matches if
  // EITHER does; a `*` on one side leaves the other in sole charge.
  const dayMatches = (dom: number, dow: number): boolean => {
    // */1 counts as a star: robfig's parser retains the star bit for step 1
    // ("if step > 1 { extra = 0 }"), so `0 0 */1 * 1` fires Mondays only —
    // treating */1 as "restricted" here would flip the day rule to OR and
    // preview every day.
    const isStar = (f: string) => f === "*" || f === "*/1";
    const domStar = isStar(domF);
    const dowStar = isStar(dowF);
    if (domStar && dowStar) return true;
    if (domStar) return match(dowF, dow);
    if (dowStar) return match(domF, dom, 1);
    return match(domF, dom, 1) || match(dowF, dow);
  };
  const times: string[] = [];
  const startMs = win?.startAt ? Date.parse(win.startAt) : NaN;
  const endMs = win?.endAt ? Date.parse(win.endAt) : NaN;
  // Step absolute instants (zone-agnostic); derive each probe's app-zone wall
  // clock to test against the fields. Granularity-aware skips replace the old
  // flat minute-step: a monthly fire sits ~44,000 minutes out, far past any
  // sane flat-step budget — which is WHY the old matcher ignored dom/month.
  //
  // The day-jump is TWO-STAGE: first to one hour before the computed midnight,
  // then the remainder. A single full-length jump assumes a 24-hour day, and a
  // spring-forward day is 23 — jumping across one lands at 01:00 of the NEXT
  // day, past its 00:00 fire, and since every jump moves forward the fire is
  // skipped permanently (in EU zones that made the shipped Monthly preset
  // preview a month late whenever March 31 was the transition Sunday). The
  // short second hop cannot overshoot by more than the transition amount, and
  // a time that lands where the wall clock skipped simply matches the engine,
  // which never fires nonexistent local times either.
  let from = Date.now() + 60000;
  if (!Number.isNaN(startMs) && startMs > from) from = startMs;
  const cur = new Date(from);
  cur.setSeconds(0, 0);
  let iters = 0;
  // FX2-E — 15000, restored from a 5000 cut. The day-skip costs ~2 iterations
  // per non-matching day, so 5000 could not enumerate three fires of a
  // leap-day cron (~2,900 iterations per 4-year gap) and exited early with a
  // silently short list. The per-iteration cost that motivated the cut was
  // already fixed by caching appZoneParts' formatter per zone, and the
  // month-skip below collapses the sparse cases to a handful of probes anyway.
  const budget = 15000;
  while (times.length < count && iters < budget) {
    iters++;
    if (!Number.isNaN(endMs) && cur.getTime() > endMs) break;
    const { minute, hour, dow, dom, month } = appZoneParts(cur);
    const monthOk = match(monF, month, 1);
    // FX2-E — month-level skip: when the MONTH alone rules this probe out,
    // walking it day-by-day spends ~60 iterations saying "still the wrong
    // month". Hop a whole-day multiple that is guaranteed to stay inside or
    // just reach the month's tail (every month has ≥ 28 days, so from day D
    // the jump lands no later than day 28); the day-level logic walks the
    // actual boundary, which keeps the DST discipline in ONE place — a
    // whole-day hop can drift an hour across a transition, and mid-month
    // that drift is harmless because the next probe re-derives the wall
    // clock from the instant.
    if (!monthOk && dom < 28) {
      cur.setTime(cur.getTime() + (28 - dom) * 86400000);
      continue;
    }
    if (!monthOk || !dayMatches(dom, dow)) {
      const toMidnight = ((24 - hour) * 60 - minute) * 60000;
      const hop = toMidnight > 3600000 ? toMidnight - 3600000 : toMidnight;
      cur.setTime(cur.getTime() + Math.max(hop, 60000));
      continue;
    }
    if (!match(hrF, hour)) {
      cur.setTime(cur.getTime() + Math.max((60 - minute) * 60000, 60000));
      continue;
    }
    if (match(minF, minute)) {
      times.push(
        fmtInAppZone(cur.toISOString(), { weekday: "short", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }),
      );
    }
    cur.setTime(cur.getTime() + 60000);
  }
  return { times, truncated: times.length < count && iters >= budget };
}

// ── Schedule modes (Phase 2) ────────────────────────────────────────────────
//
// A schedule entry fires by one of three rules. Cron is calendar-positional and
// cannot express "every 10 days" (a repeating interval needs a phase anchor);
// the activation window's start IS that anchor, so interval mode is expressible
// once the two combine. `once` is the degenerate case: an anchor and nothing to
// repeat.
export type ScheduleMode = "cron" | "interval" | "once";

export interface ScheduleSpec extends ActivationWindow {
  cron?: string | null;
  interval?: string | null;
}

// modeOf derives a spec's mode the same way the backend does, so the UI never
// has to infer it from which field happens to be populated.
export function modeOf(spec: ScheduleSpec): ScheduleMode {
  if (spec.cron?.trim()) return "cron";
  if (spec.interval?.trim()) return "interval";
  return "once";
}

const DAY_SHORTHAND = /^(\d+)\s*d$/;
const GO_DURATION = /^(\d+(?:\.\d+)?(?:ns|us|ms|s|m|h))+$/;
const MIN_INTERVAL_MS = 60_000;

// parseIntervalMs mirrors the backend's ParseInterval: a Go duration ("36h",
// "90m") or a day shorthand ("7d"). Returns null when unparseable.
function parseIntervalMs(raw?: string | null): number | null {
  const v = (raw ?? "").trim().toLowerCase();
  if (!v) return null;
  const day = DAY_SHORTHAND.exec(v);
  if (day) {
    const n = parseInt(day[1], 10);
    return n > 0 ? n * 86_400_000 : null;
  }
  if (!GO_DURATION.test(v)) return null;
  const units: Record<string, number> = { ns: 1e-6, us: 1e-3, ms: 1, s: 1000, m: 60_000, h: 3_600_000 };
  let total = 0;
  for (const [, num, unit] of v.matchAll(/(\d+(?:\.\d+)?)(ns|us|ms|s|m|h)/g)) {
    total += parseFloat(num) * units[unit];
  }
  return total > 0 ? total : null;
}

// validateInterval returns an operator-facing message, or null when valid.
export function validateInterval(raw?: string | null): string | null {
  const v = (raw ?? "").trim();
  if (!v) return "Interval is required";
  const ms = parseIntervalMs(v);
  if (ms === null) return 'Use a duration like 36h or 90m, or a day count like 7d';
  if (ms < MIN_INTERVAL_MS) return "Interval must be at least 1 minute";
  return null;
}

// intervalToHuman renders an interval as a readable phrase ("every 7 days").
export function intervalToHuman(raw?: string | null): string {
  const ms = parseIntervalMs(raw);
  if (ms === null) return "";
  const plural = (n: number, word: string) => `every ${n} ${word}${n === 1 ? "" : "s"}`;
  if (ms % 86_400_000 === 0) return plural(ms / 86_400_000, "day");
  if (ms % 3_600_000 === 0) return plural(ms / 3_600_000, "hour");
  if (ms % 60_000 === 0) return plural(ms / 60_000, "minute");
  return `every ${(raw ?? "").trim()}`;
}

// validateSpec enforces mode exclusivity and each mode's requirements — the
// client-side mirror of the backend's Spec.Validate, so the form catches what
// the API would 422 on.
export function validateSpec(spec: ScheduleSpec): string | null {
  const hasCron = !!spec.cron?.trim();
  const hasInterval = !!spec.interval?.trim();
  if (hasCron && hasInterval) return "Set a cron expression or an interval, not both";
  const windowErr = validateWindow(spec);
  if (windowErr) return windowErr;
  if (hasInterval) {
    const err = validateInterval(spec.interval);
    if (err) return err;
    if (!spec.startAt?.trim()) return "An interval schedule needs a start date to anchor it";
    return null;
  }
  if (hasCron) return cronPreview(spec.cron ?? "").valid ? null : "Invalid cron expression";
  // Neither: a one-shot, which needs an instant to fire at.
  if (!spec.startAt?.trim()) return "Set a cron expression, an interval, or a start date to run once";
  return null;
}

// nextSpecTimes projects upcoming fires for ANY mode, so every preview surface
// shows what will really happen rather than only handling cron.
export function nextSpecTimes(spec: ScheduleSpec, count = 3): string[] {
  const mode = modeOf(spec);
  if (mode === "cron") return nextCronTimes(projectTo5Field(spec.cron ?? "").expr5, count, spec);
  const startMs = spec.startAt ? Date.parse(spec.startAt) : NaN;
  if (Number.isNaN(startMs)) return [];
  const endMs = spec.endAt ? Date.parse(spec.endAt) : NaN;
  const fmt = (ms: number) =>
    fmtInAppZone(new Date(ms).toISOString(), {
      weekday: "short",
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
  if (mode === "once") {
    return startMs > Date.now() && !(!Number.isNaN(endMs) && startMs > endMs) ? [fmt(startMs)] : [];
  }
  const every = parseIntervalMs(spec.interval);
  if (every === null || every <= 0) return [];
  const now = Date.now();
  // Skip to the first fire strictly AFTER now, matching the backend's
  // intervalSchedule.Next (spec.go), which is strictly-after: a fire landing
  // exactly on `now` has already happened. ceil alone previews that
  // already-fired instant — one tick of drift, but drift between the preview
  // and the engine is this file's whole reason to be careful.
  let at = startMs;
  if (at <= now) {
    at = startMs + Math.ceil((now - startMs) / every) * every;
    if (at <= now) at += every;
  }
  const out: string[] = [];
  while (out.length < count) {
    if (!Number.isNaN(endMs) && at > endMs) break;
    out.push(fmt(at));
    at += every;
  }
  return out;
}

// describeSpec renders a spec in one phrase for summaries and table cells.
export function describeSpec(spec: ScheduleSpec): string {
  switch (modeOf(spec)) {
    case "cron": {
      const p = cronPreview(spec.cron ?? "");
      return p.valid ? p.human : `cron: ${spec.cron}`;
    }
    case "interval":
      return intervalToHuman(spec.interval);
    default:
      return "Runs once";
  }
}

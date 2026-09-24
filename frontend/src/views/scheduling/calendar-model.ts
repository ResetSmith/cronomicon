// The working-calendar maths, with no React and no API client in it.
//
// Everything here is a pure function over plain data: the day rule, the
// suppression precedence, the expiry check, the per-definition roll-up, and the
// two import parsers. calendars.ts is then only the hooks that fetch.
//
// The split is not tidiness for its own sake (score-model.ts made the same one,
// for the same reason). The defects this feature exists to prevent are silent
// ones — a calendar that quietly stopped suppressing, an entry whose policy
// disagrees with its siblings, a pasted list that dropped half its dates — and
// those are properties of these functions, worth testing directly rather than
// through a rendered picker.
//
// The day rule mirrors the server's calendar gate: a calendar day is a
// wall-clock date matched in the EFFECTIVE APP TIMEZONE, never UTC. Skip is a
// veto that wins over only (veto-before-only), and global calendars union into
// every entry's skip set whether or not the entry names them.

import type { components } from "../../api/schema";
import { appZone } from "../../utils/datetime";

export type Calendar = components["schemas"]["Calendar"];
export type CalendarDay = components["schemas"]["CalendarDay"];

// The server ships its threshold in the list envelope so the UI and server can
// never drift to two numbers; this is only the render fallback before it loads.
export const DEFAULT_EXPIRY_WARNING_DAYS = 60;

// dayInAppZone — the YYYY-MM-DD a given instant falls on in the app zone (the
// zone the scheduler matches calendar days in). en-CA is the one locale whose
// date format IS ISO YYYY-MM-DD.
export function dayInAppZone(at: number | string | Date = Date.now()): string {
  const d = typeof at === "string" ? new Date(Date.parse(at)) : new Date(at);
  if (Number.isNaN(d.getTime())) return "";
  const zone = appZone();
  try {
    return new Intl.DateTimeFormat("en-CA", {
      ...(zone ? { timeZone: zone } : {}),
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
    }).format(d);
  } catch {
    // Unknown zone id — fall back to the browser's zone rather than rendering nothing.
    return new Intl.DateTimeFormat("en-CA", { year: "numeric", month: "2-digit", day: "2-digit" }).format(d);
  }
}

// ── Expiry (CAL-16) ──────────────────────────────────────────────────────────
// The silent-failure mode this exists to catch: a skip calendar that ran out of
// days simply stops suppressing, and holiday runs resume with nothing anywhere
// saying so. Empty counts as expired — it has never suppressed anything.

export type ExpiryState = "expired" | "expiring" | null;

export function calendarExpiry(cal: Pick<Calendar, "lastDay" | "daysRemaining">, warnDays: number): ExpiryState {
  if (cal.lastDay == null) return "expired";
  const rem = cal.daysRemaining;
  if (rem == null) return null;
  if (rem < 0) return "expired";
  if (rem <= warnDays) return "expiring";
  return null;
}

// ── Suppression preview ──────────────────────────────────────────────────────

export type DaysByCalendar = Record<string, CalendarDay[]>;

export interface Suppression {
  /** Calendar responsible (or the joined only-set when the day is outside it). */
  by: string;
  /** The matched day's label, when the calendar gave it one. */
  label?: string;
  reason: "skip" | "global" | "only";
}

const dayLabel = (list: CalendarDay[] | undefined, day: string): { hit: boolean; label?: string } => {
  const d = (list ?? []).find((x) => x.day === day);
  return d ? { hit: true, label: d.label || undefined } : { hit: false };
};

/**
 * Would a fire landing on `day` (YYYY-MM-DD, app zone) be suppressed?
 * Mirrors the server gate's precedence: entry skip → global skip → only.
 * Calendars whose days are not present in `daysByCal` are treated as empty —
 * callers fetch what they can and the result is a preview, not the gate.
 */
export function suppressionFor(
  day: string,
  skipCalendars: string[],
  onlyCalendars: string[],
  daysByCal: DaysByCalendar,
  globalCalendars: string[] = [],
): Suppression | null {
  if (!day) return null;
  for (const name of skipCalendars) {
    const { hit, label } = dayLabel(daysByCal[name], day);
    if (hit) return { by: name, label, reason: "skip" };
  }
  for (const name of globalCalendars) {
    if (skipCalendars.includes(name)) continue;
    const { hit, label } = dayLabel(daysByCal[name], day);
    if (hit) return { by: name, label, reason: "global" };
  }
  if (onlyCalendars.length > 0) {
    const inSet = onlyCalendars.some((name) => dayLabel(daysByCal[name], day).hit);
    if (!inSet) return { by: onlyCalendars.join(", "), reason: "only" };
  }
  return null;
}

// excludeSuppressed (CAL-11) — /schedules/upcoming ANNOTATES suppressed instants
// rather than dropping them, because the Upcoming tab is the one place a holiday
// gap is worth seeing. Every other consumer answers "what will run next" and must
// drop them: a suppressed fire is not an upcoming run. Named and shared so that
// obligation is one call rather than a rule each surface has to remember.
export function excludeSuppressed<T extends { suppressed?: boolean }>(items: T[]): T[] {
  return items.filter((i) => !i.suppressed);
}

// ── Per-definition roll-up (CAL-12) ──────────────────────────────────────────
// "Does job X run on holidays?" costs one field instead of a walk over every
// entry. The server computes this for the schedules inventory (and SU-2 filters
// it); this is the same rendering rule for surfaces that already hold a
// definition's entries and would otherwise fetch the whole inventory to ask.
//
// Entries with NO binding still count toward the denominator: an entry that
// skips nothing is exactly the entry an auditor is looking for. Disagreement
// renders "federal-holidays (2 of 3 entries)" rather than collapsing — a
// definition-level answer would make it unrepresentable instead of visible,
// which is the wrong trade in a compliance context.
export function calendarRollup(entries: { skipCalendars?: string[]; onlyCalendars?: string[] }[]): string {
  if (entries.length === 0) return "";
  const counts = new Map<string, number>();
  for (const e of entries) {
    // One entry naming a calendar twice is still one entry.
    for (const n of new Set([...(e.skipCalendars ?? []), ...(e.onlyCalendars ?? [])])) {
      counts.set(n, (counts.get(n) ?? 0) + 1);
    }
  }
  if (counts.size === 0) return "";
  return [...counts.keys()]
    .sort()
    .map((n) => (counts.get(n) === entries.length ? n : `${n} (${counts.get(n)} of ${entries.length} entries)`))
    .join(", ");
}

export const rollupKey = (kind: string, source: string | undefined, name: string) => `${kind}:${source ?? "git"}:${name}`;

// ── Day-list parsing (CAL-19 bulk entry) ─────────────────────────────────────
// Bulk paste and CSV upload share one line grammar: `YYYY-MM-DD[,label]`.
// iCal upload reads DTSTART/SUMMARY pairs. All of it is client-side file/text
// parsing — NEVER a network fetch of a published feed (air-gapped deployments
// would fail closed in exactly the environment that needs this most, CAL-Q8).

const DAY_RE = /^\d{4}-\d{2}-\d{2}$/;

// isRealDate — strict calendar validity (rejects 2026-02-30), matching the
// server's write-time validation so a paste fails here rather than on save.
export function isRealDate(day: string): boolean {
  if (!DAY_RE.test(day)) return false;
  const [y, m, d] = day.split("-").map(Number);
  if (m < 1 || m > 12 || d < 1) return false;
  const dt = new Date(Date.UTC(y, m - 1, d));
  return dt.getUTCFullYear() === y && dt.getUTCMonth() === m - 1 && dt.getUTCDate() === d;
}

export interface ParsedDays {
  days: CalendarDay[];
  /** Human-readable per-line problems; parsing is best-effort, not all-or-nothing. */
  errors: string[];
}

// Dedupe on the day (first label wins), sorted — matching the server's
// write-time normalization, so what the editor shows is what will be stored.
const toDayList = (days: Map<string, string>): CalendarDay[] =>
  [...days.entries()]
    .sort(([a], [b]) => (a < b ? -1 : 1))
    .map(([day, label]) => (label ? { day, label } : { day }));

// parseDayLines — `YYYY-MM-DD[,label]` per line; blank lines and #comments are
// skipped; a CSV header line ("date,label") is tolerated.
export function parseDayLines(text: string): ParsedDays {
  const days = new Map<string, string>();
  const errors: string[] = [];
  const lines = text.split(/\r?\n/);
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].trim();
    if (!line || line.startsWith("#")) continue;
    const comma = line.indexOf(",");
    const day = (comma === -1 ? line : line.slice(0, comma)).trim();
    const label = comma === -1 ? "" : line.slice(comma + 1).trim().replace(/^"|"$/g, "");
    if (!isRealDate(day)) {
      // Tolerate a CSV header row without flagging it.
      if (i === 0 && /^date\b/i.test(day)) continue;
      errors.push(`line ${i + 1}: "${day || line}" is not a real YYYY-MM-DD date`);
      continue;
    }
    if (!days.has(day)) days.set(day, label);
  }
  return { days: toDayList(days), errors };
}

// parseICS — minimal VEVENT reader: DTSTART (date or date-time, the date part is
// what a working calendar stores) + SUMMARY as the label. Unfolds continuation
// lines per RFC 5545 before scanning.
export function parseICS(text: string): ParsedDays {
  const unfolded = text.replace(/\r?\n[ \t]/g, "");
  const days = new Map<string, string>();
  const errors: string[] = [];
  let events = 0;
  let cur: { day?: string; label?: string } | null = null;
  for (const raw of unfolded.split(/\r?\n/)) {
    const line = raw.trim();
    if (/^BEGIN:VEVENT$/i.test(line)) {
      cur = {};
      events++;
    } else if (/^END:VEVENT$/i.test(line)) {
      if (cur?.day) {
        if (!days.has(cur.day)) days.set(cur.day, cur.label ?? "");
      } else if (cur) {
        errors.push("an event had no parseable DTSTART date");
      }
      cur = null;
    } else if (cur) {
      const m = /^DTSTART[^:]*:(\d{4})(\d{2})(\d{2})/i.exec(line);
      if (m) {
        const day = `${m[1]}-${m[2]}-${m[3]}`;
        if (isRealDate(day)) cur.day = day;
      } else if (/^SUMMARY[^:]*:/i.test(line)) {
        cur.label = line.slice(line.indexOf(":") + 1).trim();
      }
    }
  }
  if (events === 0) errors.push("no VEVENT entries found — is this an iCalendar (.ics) file?");
  return { days: toDayList(days), errors };
}

// Working-calendar data access (CAL, the calendar-update plan): the
// hooks that fetch, and nothing else. The maths they feed — the day rule,
// suppression precedence, expiry, the roll-up, the import parsers — live in
// calendar-model.ts, which has no React and no API client in it and is tested
// directly. This module re-exports them so a component has one import.

import { useEffect, useRef, useState } from "react";
import { api } from "../../api/client";
import { useGet, rows } from "../../hooks";
import { DEFAULT_EXPIRY_WARNING_DAYS, type Calendar, type DaysByCalendar } from "./calendar-model";

export * from "./calendar-model";

// ── List hook ────────────────────────────────────────────────────────────────

export interface CalendarsList {
  calendars: Calendar[];
  expiryWarningDays: number;
  loading: boolean;
  error: string | null;
}

export function useCalendars(refresh = 0): CalendarsList {
  const { data, error, loading } = useGet<unknown>(() => api.GET("/calendars"), [refresh]);
  const env = data as { items?: Calendar[]; expiryWarningDays?: number } | undefined;
  return {
    calendars: rows<Calendar>(data),
    // The server's threshold, so the UI and server can never drift to two
    // numbers; the constant is only the fallback before the list lands.
    expiryWarningDays: env?.expiryWarningDays ?? DEFAULT_EXPIRY_WARNING_DAYS,
    loading,
    error,
  };
}

// ── Per-calendar day sets ────────────────────────────────────────────────────
// The list response deliberately omits days; the handful of calendars a preview
// needs (an entry's bindings + the globals) are fetched individually and cached
// for the component's lifetime. Failed fetches cache as [] — a preview is
// advisory, and the server re-evaluates for real at fire time.

// calendarDaysKey is the key useCalendarDays files a calendar's days under:
// its name, or, given its owner, the owner and the name (2.4.1). Two agencies
// may each have a calendar of a name (2.4.0, I2), and by the name alone the
// read answered 409 ambiguous_calendar or another agency's days.
export const calendarDaysKey = (name: string, owner?: string | null) => (owner ? `${owner}/${name}` : name);

/** A calendar to read: its name, or its name and owning agency. */
export type CalendarRef = string | { name: string; owner?: string | null };

export function useCalendarDays(refs: CalendarRef[]): { days: DaysByCalendar; loading: boolean } {
  const [days, setDays] = useState<DaysByCalendar>({});
  const inFlight = useRef<Set<string>>(new Set());
  // Keys carry no spaces (a name is a slug, an agency id has none), so the
  // joined string is a stable dependency.
  const want = [
    ...new Set(
      refs
        .map((r) => (typeof r === "string" ? r : r.name ? calendarDaysKey(r.name, r.owner) : ""))
        .filter(Boolean),
    ),
  ]
    .sort()
    .join(" ");
  useEffect(() => {
    let cancelled = false;
    for (const key of want ? want.split(" ") : []) {
      if (key in days || inFlight.current.has(key)) continue;
      inFlight.current.add(key);
      const slash = key.indexOf("/");
      const name = slash < 0 ? key : key.slice(slash + 1);
      const owner = slash < 0 ? undefined : key.slice(0, slash);
      api
        .GET("/calendars/{name}", { params: { path: { name }, query: owner ? { agencyId: owner } : {} } } as never)
        .then(({ data }) => {
          if (cancelled) return;
          const cal = data as Calendar | undefined;
          setDays((cur) => ({ ...cur, [key]: cal?.days ?? [] }));
        })
        .catch(() => {
          if (!cancelled) setDays((cur) => ({ ...cur, [key]: [] }));
        })
        .finally(() => inFlight.current.delete(key));
    }
    return () => {
      cancelled = true;
    };
    // `days` is deliberately omitted: it only ever grows with fetch results, and
    // depending on it would refetch-loop.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [want]);
  const loading = (want ? want.split(" ") : []).some((n) => !(n in days));
  return { days, loading };
}

// useCalendarRollupMap — the server-computed roll-up for every definition, keyed
// "kind:source:name" (rollupKey) and already restricted to what the caller may
// read (SU-2). For a surface that holds a definition's own schedule entries,
// calendarRollup() is cheaper; this is for the ones that do not — a Workflows
// catalog row carries crons, not entries.
export function useCalendarRollupMap(): Record<string, string> {
  const { data } = useGet<unknown>(() => api.GET("/schedules"), []);
  return (data as { calendarRollup?: Record<string, string> } | undefined)?.calendarRollup ?? {};
}

// Shared timestamp formatting in the application timezone (timezone-update §5).
//
// Every absolute timestamp in the UI renders in the *app* zone — the same zone
// the scheduler fires in — so "2 a.m." means the same thing in the schedule
// editor, the Upcoming list, run History, and reality. The app zone is held in a
// module-level variable (NOT a captured const — it is read at call time) set once
// at startup by TimezoneProvider from GET /settings/general.appTimezone. Plain
// formatter functions can't use React context, so the holder is the bridge.
//
// `undefined` means "no app zone resolved yet / fall back to the browser zone" —
// toLocaleString/Intl treat `timeZone: undefined` as the runtime default, so the
// UI degrades gracefully to browser-local before the fetch completes.

let _appZone: string | undefined;

// setAppZone installs the effective app zone (an IANA name). Empty/nullish clears
// it back to the browser-default behavior.
export function setAppZone(z?: string | null): void {
  _appZone = z && z.trim() ? z : undefined;
}

// appZone returns the current app zone, or undefined when unset (browser default).
export function appZone(): string | undefined {
  return _appZone;
}

// fmtInAppZone formats an ISO instant in the app zone. The shared core every
// absolute-timestamp formatter routes through. Returns "—" for nullish input and
// the raw string for an unparseable one (matching the prior per-view formatters).
export function fmtInAppZone(iso?: string | null, opts?: Intl.DateTimeFormatOptions): string {
  if (iso == null || iso === "") return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return String(iso);
  return d.toLocaleString(undefined, { ...(opts ?? {}), timeZone: _appZone });
}

// fmtDuration renders a run duration compactly: "45s", "3m 08s" (seconds padded),
// or "1h 05m" (drops seconds past an hour). One formatter across History,
// Workflows, Dashboard and Jobs (CC.19), superseding four drifted copies.
// Returns "—" for nullish input.
export function fmtDuration(ms?: number | null): string {
  if (ms == null) return "—";
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, "0")}s`;
  return `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, "0")}m`;
}

const WEEKDAY_IDX: Record<string, number> = {
  Sun: 0,
  Mon: 1,
  Tue: 2,
  Wed: 3,
  Thu: 4,
  Fri: 5,
  Sat: 6,
};

// appZoneParts returns the wall-clock minute/hour/day-of-week of an absolute
// instant AS SEEN IN THE APP ZONE — so the client-side cron preview matches the
// zone the server fires in, not the browser's (timezone-update §5.3, OD-TZ2).
// The parts formatter is cached per zone: constructing Intl.DateTimeFormat is
// ~100× the cost of formatToParts, and the cron preview probes thousands of
// instants per keystroke through this function.
let _partsFmt: Intl.DateTimeFormat | undefined;
let _partsFmtZone: string | undefined;

export function appZoneParts(d: Date): { minute: number; hour: number; dow: number; dom: number; month: number } {
  if (!_partsFmt || _partsFmtZone !== _appZone) {
    _partsFmt = new Intl.DateTimeFormat("en-US", {
      timeZone: _appZone,
      hour: "2-digit",
      minute: "2-digit",
      day: "numeric",
      month: "numeric",
      weekday: "short",
      hour12: false,
    });
    _partsFmtZone = _appZone;
  }
  const parts = _partsFmt.formatToParts(d);
  let minute = 0;
  let hour = 0;
  let dow = 0;
  // dom/month (FX-F2): the cron preview matched only minute/hour/weekday, so
  // the shipped "Monthly 1st 00:00" preset previewed as DAILY at 00:00 — the
  // server's projection was right and the editor's live preview lied.
  let dom = 1;
  let month = 1;
  for (const p of parts) {
    if (p.type === "minute") minute = parseInt(p.value, 10);
    else if (p.type === "hour") hour = parseInt(p.value, 10) % 24; // "24:xx" midnight → 0
    else if (p.type === "weekday") dow = WEEKDAY_IDX[p.value] ?? 0;
    else if (p.type === "day") dom = parseInt(p.value, 10);
    else if (p.type === "month") month = parseInt(p.value, 10);
  }
  return { minute, hour, dow, dom, month };
}

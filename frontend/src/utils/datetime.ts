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

// appZoneName is the zone times are entered and shown in: the app zone, else
// the browser's (as fmtInAppZone falls back).
export function appZoneName(): string {
  return _appZone ?? Intl.DateTimeFormat().resolvedOptions().timeZone;
}

// zoneOffsetMs is the app zone's wall clock minus UTC at an instant, in ms.
function zoneOffsetMs(at: number): number {
  const parts = new Intl.DateTimeFormat("en-US", {
    timeZone: _appZone,
    hourCycle: "h23",
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  }).formatToParts(new Date(at));
  const get = (type: string) => Number(parts.find((p) => p.type === type)?.value ?? 0);
  const asUTC = Date.UTC(get("year"), get("month") - 1, get("day"), get("hour"), get("minute"), get("second"));
  return asUTC - Math.floor(at / 1000) * 1000;
}

// appZoneLocalToISO turns a wall-clock time typed IN THE APP ZONE (a
// datetime-local value, "YYYY-MM-DDTHH:mm") into the UTC instant it names
// (2.5.2 FZ: a datetime-local input speaks the browser's clock, so it is read
// through the app zone instead, and the label says which). Null for a value
// that is not one.
export function appZoneLocalToISO(local: string): string | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})$/.exec(local.trim());
  if (!m) return null;
  const [y, mo, d, h, mi] = m.slice(1).map(Number);
  const wall = Date.UTC(y, mo - 1, d, h, mi);
  let t = wall;
  for (let i = 0; i < 3; i++) t = wall - zoneOffsetMs(t);
  return new Date(t).toISOString().replace(/\.\d{3}Z$/, "Z");
}

// isoToAppZoneLocal is the reverse, for prefilling a datetime-local input.
export function isoToAppZoneLocal(iso: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "";
  return new Date(t + zoneOffsetMs(t)).toISOString().slice(0, 16);
}

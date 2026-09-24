import { c } from "../../theme";
import { appZone, fmtInAppZone } from "../../utils/datetime";
import { validateWindow, type ActivationWindow } from "./cron";

// Activation-window editor (AW-10/AW-11) — shared by the Schedule Builder and the
// Job Composer's inline-schedule rows so both author the window identically.
//
// The two inputs are `datetime-local`, which has no timezone: the browser shows
// and collects the viewer's local wall clock. We convert to an absolute UTC
// instant on change (that is what the API stores) and render the resolved app
// zone beside it — the zone the scheduler actually fires in — so an operator in a
// different timezone can see what "5pm" resolves to before saving.

// toLocalInput converts a stored ISO instant to the `datetime-local` value
// format (YYYY-MM-DDTHH:mm) in the BROWSER's zone.
function toLocalInput(iso?: string | null): string {
  if (!iso) return "";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "";
  const d = new Date(t);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// fromLocalInput converts a `datetime-local` value (browser wall clock) to an
// absolute ISO instant, or null when cleared.
function fromLocalInput(v: string): string | null {
  if (!v.trim()) return null;
  const d = new Date(v);
  if (Number.isNaN(d.getTime())) return null;
  return d.toISOString();
}

// appZoneHint renders what the chosen instant looks like in the app zone, but
// only when that zone differs from the browser's — otherwise it is noise.
//
// FX-F3: `always` forces it regardless. Once-mode is the caller that needs
// that: there the window start IS the fire instant, and this datetime-local
// input collects the BROWSER's wall clock — the one place in the app where an
// operator types a time that is not in the app zone. When the zones agree the
// hint is still harmless confirmation; when they differ it is the difference
// between firing at 17:00 and firing at 17:00 five and a half hours away.
function appZoneHint(iso?: string | null, always = false): string | null {
  if (!iso) return null;
  const zone = appZone();
  if (!zone) return null;
  try {
    if (!always && zone === Intl.DateTimeFormat().resolvedOptions().timeZone) return null;
  } catch {
    return null;
  }
  const shown = fmtInAppZone(iso, {
    weekday: "short",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
  return `= ${shown} (${zone})`;
}

export interface WindowEditorProps {
  value: ActivationWindow;
  onChange: (next: ActivationWindow) => void;
  /** Compact mode drops the explanatory copy — used inside inline-schedule rows. */
  compact?: boolean;
  /** Once-mode: the start IS the fire instant, so the app-zone hint always renders (FX-F3). */
  startIsFireInstant?: boolean;
}

export function WindowEditor({ value, onChange, compact, startIsFireInstant }: WindowEditorProps) {
  const err = validateWindow(value);
  const startHint = appZoneHint(value.startAt, startIsFireInstant);
  const endHint = appZoneHint(value.endAt);

  const inputStyle: React.CSSProperties = {
    background: c.bg,
    color: c.text,
    border: `1px solid ${c.border}`,
    borderRadius: c.radiusChip,
    padding: "6px 8px",
    fontSize: c.fontSm,
    fontFamily: "inherit",
  };
  const labelStyle: React.CSSProperties = { color: c.textSec, fontSize: c.fontXs, marginBottom: 4 };

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      {!compact && (
        <div style={{ color: c.textSec, fontSize: c.fontSm }}>
          Optional. Leave blank to start firing immediately and never expire. A start date
          defers the first run without changing the cron; an end date stops it after that
          instant. Scheduled runs only — manual runs ignore the window.
        </div>
      )}
      <div style={{ display: "flex", gap: 12, flexWrap: "wrap" }}>
        <div style={{ display: "flex", flexDirection: "column", minWidth: 210 }}>
          <label style={labelStyle} htmlFor="window-start">
            Starts
          </label>
          <input
            id="window-start"
            type="datetime-local"
            value={toLocalInput(value.startAt)}
            onChange={(e) => onChange({ ...value, startAt: fromLocalInput(e.target.value) })}
            style={inputStyle}
          />
          {startHint && <span style={{ color: c.textSec, fontSize: c.fontXs, marginTop: 3 }}>{startHint}</span>}
        </div>
        <div style={{ display: "flex", flexDirection: "column", minWidth: 210 }}>
          <label style={labelStyle} htmlFor="window-end">
            Ends
          </label>
          <input
            id="window-end"
            type="datetime-local"
            value={toLocalInput(value.endAt)}
            onChange={(e) => onChange({ ...value, endAt: fromLocalInput(e.target.value) })}
            style={inputStyle}
          />
          {endHint && <span style={{ color: c.textSec, fontSize: c.fontXs, marginTop: 3 }}>{endHint}</span>}
        </div>
      </div>
      {err && <div style={{ color: c.danger, fontSize: c.fontXs }}>{err}</div>}
    </div>
  );
}

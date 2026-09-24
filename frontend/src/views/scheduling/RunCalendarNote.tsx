// CAL-14 / CAL-15 — the Run dialog's calendar note.
//
// Working calendars gate SCHEDULED fires only. A manual run — now or deferred as
// an ad-hoc pending run — is never blocked by one, and that is deliberate: an
// operator running a job by hand on a holiday is usually doing so precisely
// because it is a holiday. So this is a warning, never a block.
//
// It exists because the surprise runs the other way too: someone who bound a
// run-day (only) calendar to a job expects "this runs on business days", and
// discovering at 09:00 on a Sunday that the manual run went through anyway is
// worse learned from the run log than from the dialog.
import { c } from "../../theme";
import { dayInAppZone, suppressionFor, useCalendarDays, useCalendars } from "./calendars";

export interface EntryBinding {
  scheduleName?: string;
  skipCalendars?: string[];
  onlyCalendars?: string[];
}

/**
 * `at` is the instant the run would happen: null for "now". `entries` are the
 * job's (or workflow's) schedule entries — the note speaks for whichever of them
 * carries a binding, since a manual run has no entry of its own.
 */
export function RunCalendarNote({ entries, at }: { entries: EntryBinding[]; at: string | null }) {
  const { calendars } = useCalendars();
  const globals = calendars.filter((cal) => cal.global).map((cal) => cal.name ?? "");
  const bound = entries.flatMap((e) => [...(e.skipCalendars ?? []), ...(e.onlyCalendars ?? [])]);
  const { days } = useCalendarDays([...bound, ...globals]);

  if (bound.length === 0 && globals.length === 0) return null;
  const day = dayInAppZone(at ?? Date.now());

  // One line per entry whose policy disagrees with this instant; entries that
  // would have fired normally say nothing.
  const notes: string[] = [];
  for (const e of entries) {
    const sup = suppressionFor(day, e.skipCalendars ?? [], e.onlyCalendars ?? [], days, globals);
    if (!sup) continue;
    const which = e.scheduleName ? `Schedule "${e.scheduleName}"` : "This job's schedule";
    notes.push(
      sup.reason === "only"
        ? `${which} only runs on days in ${sup.by}, and ${at ? "that instant" : "today"} is not one of them.`
        : `${which} would be suppressed ${at ? "then" : "today"} by ${sup.label ? `${sup.label} (${sup.by})` : sup.by}.`,
    );
  }
  if (notes.length === 0) return null;

  return (
    <div
      style={{
        marginTop: 10,
        padding: "8px 10px",
        borderRadius: c.radiusSurface,
        border: `1px solid ${c.warning}30`,
        background: c.warningBg,
        color: c.textSec,
        fontSize: c.fontXs,
        lineHeight: 1.5,
      }}
    >
      {notes.map((n, i) => (
        <div key={i}>{n}</div>
      ))}
      <div style={{ marginTop: 4 }}>
        Working calendars gate scheduled fires only — this run will still happen{at ? " at the instant you picked" : ""}.
      </div>
    </div>
  );
}

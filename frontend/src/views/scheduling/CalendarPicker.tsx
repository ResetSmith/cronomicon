// CalendarPicker (CAL-13) — binds working calendars to one schedule entry in two
// polarities: skip ("never on these days") and only ("no day but these"). Shared
// by the Schedule Builder, the Job Composer's inline rows, and the Workflow
// Editor's preserved inline entries, so all three author the binding identically.
//
// Skip is a veto and wins over only, so the same name in both roles is an
// authoring error surfaced inline (the API 422s it too). Global calendars apply
// to every entry automatically and may not be bound as run-day (only) calendars.
//
// 2.5.3 (CA-30): the screens call the two polarities Forbid and Allow, at both
// levels; the wire keeps skipCalendars/onlyCalendars for an entry and takes
// forbidCalendars/allowCalendars for a job or a workflow, whose own calendars
// this picker also binds (`level="definition"`).
import { c } from "../../theme";
import { modeOf, type ScheduleSpec } from "./cron";
import {
  dayInAppZone,
  suppressionFor,
  useCalendars,
  useAgencyCalendars,
  type Calendar,
} from "./calendars";

export interface CalendarBinding {
  skipCalendars: string[];
  onlyCalendars: string[];
}

export const EMPTY_BINDING: CalendarBinding = { skipCalendars: [], onlyCalendars: [] };

// bindingError — the client-side mirror of the API's mutual-exclusion 422, so
// the form catches it before save.
export function bindingError(v: CalendarBinding, of = "entry"): string | null {
  const both = v.skipCalendars.filter((n) => v.onlyCalendars.includes(n));
  if (both.length > 0) return `"${both[0]}" cannot be both a Forbid and an Allow calendar on one ${of}.`;
  return null;
}

/** What a definition's own calendars are said to be of, in the picker's copy. */
type Of = "job" | "workflow";

export function CalendarPicker({
  value,
  onChange,
  compact,
  spec,
  agency,
  level = "entry",
  of = "job",
}: {
  value: CalendarBinding;
  onChange: (next: CalendarBinding) => void;
  /** Compact mode drops the explanatory copy — used inside inline-schedule rows. */
  compact?: boolean;
  /** The entry's firing spec, when the caller has one — enables the once-mode
   *  suppression warning (a suppressed one-shot never runs, not "not today"). */
  spec?: ScheduleSpec;
  /** The agency of the definition the entry is of (2.4.1): its calendars are
   *  the ones offered and judged, as the scheduler judges them. Unknown: every
   *  calendar, as before. */
  agency?: string;
  /** "definition" binds a job's or a workflow's own calendars (CA-30), which
   *  gate its every automatic start (CA-32); "entry" (the default) one entry's. */
  level?: "entry" | "definition";
  of?: Of;
}) {
  const { calendars: all } = useCalendars();
  const bound = [...value.skipCalendars, ...value.onlyCalendars];
  // Day sets for the once-mode preview only: the bound calendars plus the
  // globals that would veto the instant anyway (the agency's own).
  const isOnce = !!spec && modeOf(spec) === "once" && !!spec.startAt?.trim();
  const { relevant: calendars, globals, daysByName: days } = useAgencyCalendars(all, agency, bound, isOnce);

  const err = bindingError(value, level === "definition" ? of : "entry");
  // Dangling names (CAL-16): a force-deleted calendar leaves its bindings in
  // place by design; the badge is how the author learns the policy is dead.
  const known = new Set(calendars.map((cal) => cal.name ?? ""));
  const dangling = bound.filter((n) => !known.has(n));

  // Once-mode flag (CAL-13): with no defer polarity, suppression of a one-shot
  // means the run NEVER happens — the one case worth an authoring-time warning.
  let onceWarning: string | null = null;
  if (isOnce && !err) {
    const day = dayInAppZone(spec!.startAt!);
    const sup = suppressionFor(day, value.skipCalendars, value.onlyCalendars, days, globals);
    if (sup) {
      const why =
        sup.reason === "only"
          ? `is not a run day in ${sup.by}`
          : `falls on ${sup.label ? `${sup.label} (${sup.by})` : sup.by}`;
      onceWarning = `This one-shot's instant ${why} — a suppressed one-shot never runs at all, not "not today".`;
    }
  }

  if (calendars.length === 0 && bound.length === 0) {
    // Nothing to bind and nothing bound: stay quiet in compact rows; in the full
    // form say where calendars come from rather than rendering two empty selects.
    return compact ? null : (
      <div style={{ fontSize: c.fontXs, color: c.textMuted }}>
        No working calendars yet — author them under Schedules → Calendars, then bind them here.
      </div>
    );
  }

  const toggle = (role: "skipCalendars" | "onlyCalendars", name: string) => {
    const cur = value[role];
    onChange({ ...value, [role]: cur.includes(name) ? cur.filter((x) => x !== name) : [...cur, name] });
  };

  const roleBlock = (role: "skipCalendars" | "onlyCalendars") => {
    const isSkip = role === "skipCalendars";
    const selected = value[role];
    // Offer every known calendar (globals excluded from the only role — the API
    // refuses them there) plus any dangling bound name, which must stay visible
    // to be removable.
    const options: { name: string; cal?: Calendar; disabled?: boolean }[] = [
      ...calendars
        .map((cal) => ({ name: cal.name ?? "", cal, disabled: !isSkip && !!cal.global }))
        .filter((o) => o.name),
      ...dangling.filter((n) => selected.includes(n)).map((name) => ({ name })),
    ];
    return (
      <div style={{ minWidth: 200, flex: 1 }}>
        <div style={{ color: c.textSec, fontSize: c.fontXs, marginBottom: 4 }}>
          {isSkip ? "Forbid — never on days in" : "Allow — only on days in"}
        </div>
        <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
          {options.map(({ name, cal, disabled }) => {
            const on = selected.includes(name);
            const missing = !cal;
            return (
              <button
                key={name}
                type="button"
                disabled={disabled && !on}
                onClick={() => toggle(role, name)}
                title={
                  disabled
                    ? "A freeze calendar already applies to every start of its agency and cannot be an Allow calendar."
                    : missing
                      ? "This calendar no longer exists — the binding does nothing. Click to remove it."
                      : cal?.description || undefined
                }
                style={{
                  padding: "3px 9px",
                  fontSize: c.fontXs,
                  fontFamily: c.mono,
                  borderRadius: c.radiusChip,
                  cursor: disabled && !on ? "not-allowed" : "pointer",
                  border: `1px solid ${missing ? c.warning : on ? c.primary : c.border}`,
                  background: on ? (missing ? `${c.warning}20` : c.primary) : "transparent",
                  color: missing ? c.warning : on ? c.onSolid : c.textSec,
                  opacity: disabled && !on ? 0.45 : 1,
                }}
              >
                {name}
                {cal?.global ? " ◦ freeze" : ""}
                {missing ? " — missing" : ""}
              </button>
            );
          })}
          {options.length === 0 && <span style={{ fontSize: c.fontXs, color: c.textMuted }}>none available</span>}
        </div>
      </div>
    );
  };

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      {!compact && level === "entry" && (
        <div style={{ color: c.textSec, fontSize: c.fontSm }}>
          Optional. Forbid calendars veto a fire landing on one of their days; Allow calendars make their
          days the only days this entry may fire. Forbid wins when both apply. Suppressed fires appear in
          History as skipped, with the calendar named.
        </div>
      )}
      {!compact && level === "definition" && (
        <div style={{ color: c.textSec, fontSize: c.fontSm }}>
          Optional. The {of} never starts on a day in a Forbid calendar, and starts only on days in one of its Allow
          calendars. They gate every automatic start: its schedules, a service account's trigger, a reaction, a file
          arrival. A start on a forbidden day is dropped, not postponed, and recorded as skipped with the calendar named.
          A person's Run is not stopped; the Run dialog says when today is forbidden. Forbid wins when both list a day,
          and an entry's own Forbid and Allow apply on top.
          {of === "workflow" && " A step's job calendars do not apply inside the workflow: a workflow never stops half-way."}
        </div>
      )}
      <div style={{ display: "flex", gap: 16, flexWrap: "wrap" }}>
        {roleBlock("skipCalendars")}
        {roleBlock("onlyCalendars")}
      </div>
      {err && <div style={{ color: c.danger, fontSize: c.fontXs }}>{err}</div>}
      {dangling.length > 0 && !err && (
        <div style={{ color: c.warning, fontSize: c.fontXs }}>
          {dangling.length === 1 ? `Calendar "${dangling[0]}" no longer exists` : `${dangling.length} bound calendars no longer exist`} — the binding has no effect until it is removed or the calendar is re-created.
        </div>
      )}
      {onceWarning && <div style={{ color: c.warning, fontSize: c.fontXs }}>{onceWarning}</div>}
    </div>
  );
}

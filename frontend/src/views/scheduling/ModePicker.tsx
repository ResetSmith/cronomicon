import { c } from "../../theme";
import { intervalToHuman, modeOf, validateInterval, type ScheduleMode, type ScheduleSpec } from "./cron";

// Schedule-mode picker (Phase 2) — the Cron ⟷ Interval ⟷ Once switch shared by
// the Schedule Builder and the Composer's inline rows.
//
// Switching mode clears the field the other mode owns, because the backend
// treats cron and interval as mutually exclusive: leaving a stale cron behind
// while the operator types an interval would 422 on save with an error about a
// field they can no longer see.

const MODES: { mode: ScheduleMode; label: string; hint: string }[] = [
  { mode: "cron", label: "On a schedule", hint: "Calendar-based — every Wednesday at 17:00, weekdays at 09:00." },
  { mode: "interval", label: "Every N", hint: "A fixed gap measured from the start date — every 7 days, every 36 hours." },
  { mode: "once", label: "Once", hint: "A single run at the start date, then never again." },
];

const INTERVAL_PRESETS = ["12h", "36h", "3d", "7d", "14d", "30d"];

export function ModePicker({
  value,
  onChange,
}: {
  value: ScheduleSpec;
  onChange: (next: Partial<ScheduleSpec>) => void;
}) {
  const mode = modeOf(value);

  // Clearing the other mode's field is what makes the switch safe — see above.
  const select = (next: ScheduleMode) => {
    if (next === mode) return;
    if (next === "cron") onChange({ cron: "0 7 * * *", interval: null });
    else if (next === "interval") onChange({ cron: "", interval: "7d" });
    else onChange({ cron: "", interval: null });
  };

  const active = MODES.find((m) => m.mode === mode);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
      <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
        {MODES.map((m) => {
          const on = m.mode === mode;
          return (
            <button
              key={m.mode}
              type="button"
              onClick={() => select(m.mode)}
              style={{
                padding: "5px 12px",
                fontSize: c.fontSm,
                borderRadius: c.radiusChip,
                cursor: "pointer",
                border: `1px solid ${on ? c.primary : c.border}`,
                background: on ? c.primary : "transparent",
                color: on ? c.onSolid : c.textSec,
                fontWeight: on ? 600 : 400,
              }}
            >
              {m.label}
            </button>
          );
        })}
      </div>
      {active && <div style={{ color: c.textMuted, fontSize: c.fontXs }}>{active.hint}</div>}
    </div>
  );
}

// IntervalField is the interval-mode input: presets plus a free-text duration,
// with the anchor requirement stated where the operator will hit it.
export function IntervalField({
  value,
  onChange,
  hasAnchor,
}: {
  value?: string | null;
  onChange: (v: string) => void;
  hasAnchor: boolean;
}) {
  const raw = (value ?? "").trim();
  const err = raw ? validateInterval(raw) : null;
  const human = intervalToHuman(raw);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
        {INTERVAL_PRESETS.map((p) => {
          const on = p === raw;
          return (
            <button
              key={p}
              type="button"
              onClick={() => onChange(p)}
              style={{
                padding: "4px 8px",
                fontSize: c.fontXs,
                borderRadius: c.radiusChip,
                cursor: "pointer",
                border: `1px solid ${on ? c.primary : c.border}`,
                background: on ? c.primary : "transparent",
                color: on ? c.onSolid : c.textSec,
              }}
            >
              {p}
            </button>
          );
        })}
      </div>
      <input
        value={value ?? ""}
        placeholder="7d"
        onChange={(e) => onChange(e.target.value)}
        style={{
          background: c.bg,
          color: c.text,
          border: `1px solid ${err ? c.danger : c.border}`,
          borderRadius: c.radiusChip,
          padding: "6px 8px",
          fontSize: c.fontSm,
          fontFamily: c.mono,
        }}
      />
      <div style={{ fontSize: c.fontXs, color: err ? c.danger : c.textMuted }}>
        {err ?? (human ? `Runs ${human}, counted from the start date.` : "A duration like 36h or 90m, or a day count like 7d.")}
      </div>
      {!hasAnchor && (
        <div style={{ fontSize: c.fontXs, color: c.warning }}>
          Set a <strong>start date</strong> below — an interval is measured from it, so it cannot run without one.
        </div>
      )}
      <div style={{ fontSize: c.fontXs, color: c.textMuted }}>
        Intervals count elapsed time, not calendar days: across a daylight-saving change the
        wall-clock time shifts by an hour. For “every day at 5&nbsp;p.m.”, use{" "}
        <strong>On a schedule</strong> instead.
      </div>
    </div>
  );
}

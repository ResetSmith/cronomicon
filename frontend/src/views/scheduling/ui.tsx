// Shared presentational helpers for the Schedule hub tabs (Inventory / Upcoming
// / Builder). Token-bearing styles are functions so they re-read the mutable `c`
// palette on each render (theme toggle); see theme.ts.
import { c } from "../../theme";
import { fmtInAppZone } from "../../utils/datetime";

// fmtWhen renders an absolute RFC3339 instant in the application timezone — the
// same zone the scheduler fires in — so a schedule's next/last run reads as the
// app-zone wall clock (timezone-update §5.2).
export function fmtWhen(iso?: string | null): string {
  return fmtInAppZone(iso, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

// WindowNote renders a schedule's activation window (AW-13) as a short
// supporting line — "starts Aug 5, 17:00" / "ended Sep 1, 00:00" — beneath the
// state chip, so an operator can tell WHEN a pending entry begins without
// opening the editor. Returns null for an unbounded entry.
export function WindowNote({ startAt, endAt }: { startAt?: string | null; endAt?: string | null }) {
  if (!startAt && !endAt) return null;
  const now = Date.now();
  const parts: string[] = [];
  if (startAt) {
    const started = Date.parse(startAt) <= now;
    parts.push(`${started ? "since" : "starts"} ${fmtWhen(startAt)}`);
  }
  if (endAt) {
    const ended = Date.parse(endAt) < now;
    parts.push(`${ended ? "ended" : "until"} ${fmtWhen(endAt)}`);
  }
  return <span style={{ fontSize: c.fontXs, color: c.textMuted }}>{parts.join(" · ")}</span>;
}

// OwnerBadge tags a schedule row as belonging to a job or a workflow. Kind, not
// state, so it takes the tag shape — the pill is reserved for status (VU-17).
export function OwnerBadge({ kind }: { kind?: string }) {
  const isWf = kind === "workflow";
  const tone = isWf ? c.info : c.primary;
  return (
    <span
      style={{
        fontSize: c.fontXs,
        fontWeight: 600,
        padding: "2px 7px",
        borderRadius: c.radiusChip,
        background: `${tone}1a`,
        color: tone,
        border: `1px solid ${tone}30`,
        textTransform: "uppercase",
        letterSpacing: 0.3,
      }}
    >
      {isWf ? "workflow" : "job"}
    </span>
  );
}

// StateBadge renders enabled/paused/disabled state from the inventory flags. This
// is the row's status, so it is the pill — the one shape allowed to pop (VU-17),
// which is also what keeps it distinct from the OwnerBadge tag beside it.
// StateBadge collapses a schedule entry's runtime state into one chip.
//
// Definition-level state wins: a paused or disabled definition silences every
// entry regardless of its activation window, so those labels take precedence.
// Only when the definition is live does the window (AW-13) get to speak —
// "pending" before it opens, "expired" after it closes.
export function StateBadge({
  enabled,
  paused,
  windowState,
}: {
  enabled?: boolean;
  paused?: boolean;
  windowState?: string | null;
}) {
  const [label, tone] = paused
    ? ["paused", c.warning]
    : !enabled
      ? ["disabled", c.textMuted]
      : windowState === "pending"
        ? ["pending", c.info]
        : windowState === "expired"
          ? ["expired", c.textMuted]
          : ["active", c.success];
  return (
    <span
      style={{
        fontSize: c.fontXs,
        fontWeight: 600,
        padding: "2px 8px",
        borderRadius: c.radiusPill,
        background: `${tone}18`,
        color: tone,
        border: `1px solid ${tone}40`,
      }}
    >
      {label}
    </span>
  );
}

export const panel = (): React.CSSProperties => ({
  background: c.panel,
  border: `1px solid ${c.border}`,
  borderRadius: c.radiusSurface,
  padding: 16,
});
export const h2 = (): React.CSSProperties => ({ fontSize: c.fontHead, fontWeight: 600, color: c.text, marginTop: 0, marginBottom: 12 });
export const label = (): React.CSSProperties => ({
  display: "block",
  fontSize: c.fontXs,
  fontFamily: c.sansCond,
  fontWeight: 600,
  color: c.textMuted,
  textTransform: "uppercase",
  letterSpacing: 0.7,
  marginBottom: 6,
});
export const input = (): React.CSSProperties => ({
  width: "100%",
  boxSizing: "border-box",
  border: `1px solid ${c.borderStrong}`,
  borderRadius: c.radiusChip,
  padding: "8px 12px",
  fontSize: c.fontSm,
  background: c.panelInput,
  color: c.text,
  fontFamily: "inherit",
  outline: "none",
  marginBottom: 14,
});
// The one style here that is a plain object rather than a function, so it cannot
// read `c` — a module-level const would freeze the load-time palette, so this is
// a function like its siblings below. It was the last radius literal in the app
// (VU-6) precisely because it was the last plain object.
export const btn = (): React.CSSProperties => ({
  padding: "7px 14px",
  fontSize: c.fontSm,
  fontWeight: 600,
  border: "none",
  borderRadius: c.radiusChip,
  cursor: "pointer",
  fontFamily: "inherit",
});
export const chipBtn = (): React.CSSProperties => ({
  padding: "4px 10px",
  fontSize: c.fontXs,
  fontWeight: 500,
  border: `1px solid ${c.border}`,
  borderRadius: c.radiusChip,
  cursor: "pointer",
  fontFamily: "inherit",
});
export const pre = (): React.CSSProperties => ({
  margin: 0,
  padding: 12,
  background: c.bg,
  border: `1px solid ${c.border}`,
  borderRadius: c.radiusSurface,
  fontSize: c.fontSm,
  fontFamily: c.mono,
  color: c.textSec,
  lineHeight: 1.6,
  overflow: "auto",
});
export const banner = (color: string): React.CSSProperties => ({
  padding: "10px 14px",
  borderRadius: c.radiusSurface,
  border: `1px solid ${color}50`,
  background: `${color}14`,
  color,
  fontSize: c.fontSm,
  lineHeight: 1.5,
});
export const th = (): React.CSSProperties => ({
  padding: "10px 14px",
  fontFamily: c.sansCond,
  fontWeight: 600,
  fontSize: c.fontXs,
  textTransform: "uppercase",
  letterSpacing: 0.7,
  textAlign: "left",
  whiteSpace: "nowrap",
});
export const td: React.CSSProperties = { padding: "10px 14px" };

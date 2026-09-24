import { type CSSProperties, type ReactNode } from "react";
import { c } from "../../theme";
import { CopyText, Field as UIField, InlineLoading, statusLabel, statusTone } from "../../components/ui";
import { fmtInAppZone } from "../../utils/datetime";

// ── formatting ──────────────────────────────────────────────────────────────

/** ISO timestamp → "Jun 9, 2026, 14:22" in the application timezone (§5.2). */
export function fmtTime(iso?: string | null): string {
  return fmtInAppZone(iso, {
    month: "short",
    day: "numeric",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

/** durationMs → "3m 24s" / "12s". */
// fmtDuration re-exported from the shared datetime utils (CC.19).
export { fmtDuration } from "../../utils/datetime";

/** Distinct non-empty values, prefixed with "All" — for filter selects. */
export function uniq(vals: (string | undefined | null)[]): string[] {
  return ["All", ...Array.from(new Set(vals.filter((v): v is string => !!v))).sort()];
}

// ── table styles ─────────────────────────────────────────────────────────────
// th/mono are FUNCTIONS so they re-read the mutable `c` palette on theme toggle
// (theme.ts). th is converged onto the canonical thStyle — condensed uppercase at
// fontXs (VU-7), which is what buys the characters-per-pixel this table's nine
// columns need. `td` captures no tokens, so it stays a const.

export const th = (): CSSProperties => ({
  textAlign: "left",
  padding: "12px 16px",
  fontFamily: c.sansCond,
  fontWeight: 600,
  fontSize: c.fontXs,
  textTransform: "uppercase",
  letterSpacing: 0.7,
  color: c.textMuted,
  borderBottom: `1px solid ${c.border}`,
});

export const td: CSSProperties = { padding: "10px 16px", verticalAlign: "top" };

export const mono = (): CSSProperties => ({
  fontFamily: c.mono,
  fontSize: c.fontXs,
  color: c.textSec,
  // JetBrains Mono is already fixed-pitch, but trace IDs and durations also pass
  // through fallbacks (ui-monospace/monospace) on a cold cache — pin the figures
  // so a live-refreshing column never re-flows (VU-4).
  fontVariantNumeric: "tabular-nums",
});

// shortTrace renders a UUIDv7 trace id as a compact, *visually distinct* token
// (LB6). A UUIDv7 packs the 48-bit millisecond timestamp into its leading bytes,
// so two runs minted in the same ~minute share an identical leading block — the
// old `traceId.slice(0, 8)` showed only that shared timestamp prefix, making
// distinct runs look like they share a trace id. Instead we drop the high
// timestamp block (already conveyed by the adjacent time column) and show the
// discriminating low-timestamp/sequence head plus the random tail, middle-ellipsis
// style: `019ec1a6-3c4d-74f7-8bbc-86dfd09d5474` → `3c4d-74f7…d5474`. Callers keep
// the full id in the cell's `title=` tooltip. Falls back to a raw middle-ellipsis
// for any non-UUID id (e.g. legacy/manual ids).
export function shortTrace(id?: string | null): string {
  if (!id) return "—";
  const p = id.split("-");
  if (p.length === 5 && p[4].length >= 5) return `${p[1]}-${p[2]}…${p[4].slice(-5)}`;
  return id.length > 14 ? `${id.slice(0, 6)}…${id.slice(-5)}` : id;
}

// TraceId renders shortTrace on a single non-wrapping line with a click-to-copy
// affordance (VC.9). The row it sits in is usually clickable, so copy stops
// propagation; the full id lives in the tooltip and the copied value. `color`
// lets callers keep their accent (Jobs uses c.accent; History uses textSec).
export function TraceId({ id, color }: { id?: string | null; color?: string }) {
  if (!id) return <span style={{ color: c.textMuted }}>—</span>;
  // FX-13 — was a bare <span onClick>: no tab stop, no role, no name. CopyText
  // keeps the shortened display and the full id on the clipboard, and makes it a
  // real button. stopPropagation because the row behind it is clickable.
  return (
    <CopyText
      text={id}
      display={shortTrace(id)}
      stopPropagation
      style={{ fontFamily: c.mono, fontSize: c.fontXs, color: color ?? c.textSec, whiteSpace: "nowrap" }}
    />
  );
}

// ── small components ─────────────────────────────────────────────────────────

export function Card({ children }: { children: ReactNode }) {
  return (
    <div style={{ background: c.panel, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, overflow: "hidden" }}>
      {children}
    </div>
  );
}

// History result/status badge — renders the canonical six-state vocabulary
// (V1.1-4): Success · Failed · Warn · Skipped · Running · Paused. Colour + label
// come from the single shared source (components/ui) so every status surface
// reads the same. `queued` shows as Running; `running` keeps the pulsing dot.
export function StatusBadge({ status }: { status?: string | null }) {
  const { color, bg } = statusTone(status);
  return (
    <span
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 5,
        padding: "3px 10px",
        borderRadius: c.radiusPill,
        fontSize: c.fontXs,
        fontWeight: 600,
        color,
        background: bg === "transparent" ? `${color}1a` : bg,
        whiteSpace: "nowrap",
      }}
    >
      <span
        style={{
          width: 6,
          height: 6,
          borderRadius: "50%",
          background: color,
          animation: status === "running" ? "pulse 2s infinite" : undefined,
        }}
      />
      {status ? statusLabel(status) : "—"}
    </span>
  );
}

// Executor badge — surfaces how a run was executed (R5.3). EX.10 introduced the
// `ssh` variant as the extension point; `runner` is the new sibling. Subtle, pill-
// shaped, tinted to match the existing chip vocabulary (cf. the workflow/Test chips).
export function ExecutorBadge({ executor }: { executor?: string | null }) {
  if (executor !== "ssh" && executor !== "runner") return null;
  const isRunner = executor === "runner";
  const color = isRunner ? c.primary : c.info;
  return (
    <span
      title={isRunner ? "Executed by a runner agent" : "Executed by the in-app SSH executor"}
      style={{
        display: "inline-flex",
        alignItems: "center",
        padding: "2px 7px",
        // Chip, not pill: the executor is a *kind*, not a status (B-3/VU-17).
        borderRadius: c.radiusChip,
        fontSize: c.fontXs,
        fontWeight: 600,
        background: `${color}1a`,
        color,
        border: `1px solid ${color}30`,
        whiteSpace: "nowrap",
        letterSpacing: 0.3,
      }}
    >
      {isRunner ? "Runner" : "SSH"}
    </span>
  );
}

// "Lost" indicator (D6) — a runner-loss failure (status=failure,
// statusReason=runner_lost; the reaper writes queued_reason='runner_lost', which
// the API surfaces as statusReason). Distinguishes a runner-loss failure from a
// normal failure without inventing a new status colour. Renders next to the
// status badge; subtle, consistent with the six-state status vocabulary.
export function LostBadge() {
  return (
    <span
      title="Runner was lost mid-run — this run was reaped and marked failed"
      style={{
        display: "inline-flex",
        alignItems: "center",
        padding: "2px 7px",
        // Chip, not pill: "Lost" qualifies *why* a run failed and is deliberately
        // outside the six-state status vocabulary, so it must not take the status
        // shape sitting next to it (B-3/VU-17).
        borderRadius: c.radiusChip,
        fontSize: c.fontXs,
        fontWeight: 600,
        background: `${c.warning}1a`,
        color: c.warning,
        border: `1px solid ${c.warning}40`,
        whiteSpace: "nowrap",
        letterSpacing: 0.3,
      }}
    >
      Lost
    </span>
  );
}

/** True when a run is a runner-loss failure (D6). */
export function isRunnerLost(status?: string | null, statusReason?: string | null): boolean {
  return (status === "failure" || status === "danger" || status === "killed") && statusReason === "runner_lost";
}

// "Stopped" indicator (RX-23) — a human ended this run, whatever status it now
// carries. This is not polish. Kill dispositions (RX-4) create a genuinely new
// state: status='success' with killedBy set, which correctly appears under the
// Success filter — so without a marker, "successful runs" silently starts
// including runs an operator stopped, and History gets LESS trustworthy than it
// was before dispositions existed. The badge is what keeps the status filters
// honest once status no longer implies how the run ended.
//
// Chip, not pill, for LostBadge's reason: "stopped by a human" qualifies *how* a
// run reached its status and is deliberately outside the status vocabulary, so
// it must not take the status shape sitting next to it (B-3/VU-17).
export function StoppedBadge({ by }: { by?: string | null }) {
  return (
    <span
      title={by ? `Stopped by ${by}` : "Stopped by an operator"}
      style={{
        display: "inline-flex",
        alignItems: "center",
        padding: "2px 7px",
        borderRadius: c.radiusChip,
        fontSize: c.fontXs,
        fontWeight: 600,
        background: `${c.textSec}1a`,
        color: c.textSec,
        border: `1px solid ${c.textSec}40`,
        whiteSpace: "nowrap",
        letterSpacing: 0.3,
      }}
    >
      Stopped
    </span>
  );
}

export function SearchInput({ value, onChange, placeholder }: { value: string; onChange: (v: string) => void; placeholder: string }) {
  return (
    <div
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 8,
        flex: 1,
        minWidth: 220,
        padding: "7px 12px",
        borderRadius: c.radiusChip,
        border: `1px solid ${c.borderStrong}`,
        background: c.panelInput,
        boxSizing: "border-box",
      }}
    >
      <span style={{ color: c.textMuted, display: "inline-flex", flexShrink: 0 }} aria-hidden>
        <svg width={14} height={14} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round">
          <circle cx="11" cy="11" r="7" />
          <line x1="21" y1="21" x2="16.65" y2="16.65" />
        </svg>
      </span>
      <input
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        style={{
          flex: 1,
          minWidth: 0,
          border: "none",
          outline: "none",
          background: "transparent",
          color: c.text,
          fontSize: c.fontSm,
          fontFamily: c.sans,
        }}
      />
    </div>
  );
}

// FilterSelect now lives in the canonical kit (CC.16); re-exported for the
// history/list views that import it from here.
export { FilterSelect } from "../../components/ui";

export function Loading() {
  return <InlineLoading style={{ padding: "12px 2px" }} />;
}

export function ErrorMsg({ msg }: { msg: string }) {
  return <div style={{ color: c.danger, padding: "12px 2px" }}>Error: {msg}</div>;
}

// VU-14 — `action` carries the empty state's next step (clear the filter, run the
// sync, go to the page that creates the thing). It is optional because some empty
// tables genuinely have no next step: an audit trail with nothing in it is waiting
// on the operator to do something elsewhere, not on a control here.
export function EmptyRow({ colSpan, title, hint, action }: { colSpan: number; title: string; hint?: string; action?: ReactNode }) {
  return (
    <tr>
      <td colSpan={colSpan} style={{ padding: 36, textAlign: "center" }}>
        <div style={{ fontWeight: 600, fontSize: c.fontBody, color: c.text }}>{title}</div>
        {hint && <div style={{ color: c.textSec, fontSize: c.fontSm, marginTop: 4 }}>{hint}</div>}
        {action && <div style={{ marginTop: 12, display: "flex", justifyContent: "center" }}>{action}</div>}
      </td>
    </tr>
  );
}

// Field delegates to the shared components/ui Field (EV-1) rather than being a
// second implementation of it. The adapter keeps History's own call shape
// (`children`, `isMono`) so its ~14 call sites did not have to churn, and passes
// breakAll because these fields carry trace ids and hashes, which must wrap
// anywhere rather than at word boundaries.
export function Field({ label, isMono, children }: { label: string; isMono?: boolean; children: ReactNode }) {
  return <UIField label={label} value={children} mono={isMono} breakAll />;
}

// ── pagination (per-tab state — fixes prototype X3) ──────────────────────────
// Relocated to components/ui (PP) so catalog pages don't reach into the history/
// namespace for a pager. Re-exported here unchanged so the History/Workflows tabs'
// existing `from "./shared"` imports keep compiling with no edits.
export { usePager, Pager } from "../../components/ui";
export type { PagerState } from "../../components/ui";

export const filterBar: CSSProperties = {
  display: "flex",
  gap: 10,
  marginBottom: 12,
  alignItems: "center",
  flexWrap: "wrap",
};

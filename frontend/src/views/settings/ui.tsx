import { useEffect, useState } from "react";
import { c } from "../../theme";
import { useGet } from "../../hooks";
import { Btn as KitBtn, InlineLoading } from "../../components/ui";
import { errMsg as toErrMsg } from "../../api/client";
import { fmtInAppZone } from "../../utils/datetime";

// csrfHeader (a type-satisfying placeholder; the client middleware injects the
// real double-submit token) and errMsg now live in api/client.ts — re-exported
// here so the settings tab views that import them from this kit keep working (CC.21).
export { csrfHeader, errMsg } from "../../api/client";

// The shared field + table-cell styles and the Btn/Card primitives now come from
// the canonical kit (CC.16); re-exported here so the settings tab views converge
// onto the canonical styling. lblStyle stays local (no canonical equivalent yet).
export { inputStyle, thStyle, tdStyle, Card, Btn } from "../../components/ui";

export const lblStyle = (): React.CSSProperties => ({
  fontSize: c.fontXs,
  fontFamily: c.sansCond,
  fontWeight: 600,
  color: c.textMuted,
  textTransform: "uppercase",
  letterSpacing: 0.7,
  marginBottom: 5,
});

// ── Toggle switch ──
export function Toggle(props: { on: boolean; onChange: () => void; onColor?: string }) {
  return (
    <div
      onClick={props.onChange}
      style={{
        width: 36,
        height: 20,
        // The one non-status use of the pill token (B-2/VU-17). A switch track is
        // a capsule by *affordance* — at chip radius a 36×20 track with a round
        // knob reads as a broken button, not a switch. It never competes with a
        // status pill for attention because the two never share a row.
        borderRadius: c.radiusPill,
        background: props.on ? (props.onColor ?? c.success) : c.border,
        padding: 2,
        cursor: "pointer",
        transition: "background 0.2s",
        flexShrink: 0,
      }}
    >
      <div
        style={{
          width: 16,
          height: 16,
          borderRadius: "50%",
          background: "#fff",
          transform: `translateX(${props.on ? 16 : 0}px)`,
          transition: "transform 0.2s",
        }}
      />
    </div>
  );
}

// ── Pill chip (for multi-select filters) ──
export function Chip(props: { label: string; on: boolean; onClick: () => void }) {
  return (
    <span
      onClick={props.onClick}
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 7,
        padding: "6px 12px",
        // A filter toggle, not a status — chip shape (B-3/VU-17). The comment
        // above still says "pill" because that is what the component was called.
        borderRadius: c.radiusChip,
        cursor: "pointer",
        userSelect: "none",
        fontSize: c.fontXs,
        fontWeight: 500,
        border: `1px solid ${props.on ? c.primary : c.border}`,
        background: props.on ? `${c.primary}26` : "transparent",
        color: props.on ? c.primary : c.textSec,
      }}
    >
      <span style={{ width: 7, height: 7, borderRadius: "50%", background: props.on ? c.primary : c.textSec }} />
      {props.label}
    </span>
  );
}

// ── Status badge (SSH host/bastion verification) ──
// Colors are resolved at render time (not module load) because theme tokens
// (`c`) mutate in place on toggle — see the inline-styles note above.
const STATUS_LABELS: Record<string, string> = {
  verified: "Verified",
  reachable: "Reachable",
  cred_error: "Cred Error",
  conn_error: "Conn Error",
  unverified: "Unverified",
};
// reachable (keyless tier: endpoint answered + host key pinned, auth untested)
// carries the same success green as verified — both are their tier's good
// outcome; the label alone marks the difference.
const statusColor = (s: string): string =>
  s === "verified" || s === "reachable" ? c.success : s === "cred_error" || s === "conn_error" ? c.danger : c.textSec;

export function StatusBadge(props: { status?: string | null }) {
  const s = props.status && STATUS_LABELS[props.status] ? props.status : "unverified";
  const color = statusColor(s);
  const label = STATUS_LABELS[s];
  return (
    <span
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 5,
        padding: "3px 9px",
        borderRadius: c.radiusPill,
        fontSize: c.fontXs,
        fontWeight: 600,
        background: `${color}26`,
        color,
      }}
    >
      <span style={{ width: 6, height: 6, borderRadius: "50%", background: color }} />
      {label}
    </span>
  );
}

// ── Settings row (label left, control right) ──
export function SettingRow(props: { label: string; hint?: string; last?: boolean; children: React.ReactNode }) {
  return (
    <div
      style={{
        display: "flex",
        justifyContent: "space-between",
        alignItems: "center",
        gap: 16,
        padding: "12px 0",
        borderBottom: props.last ? "none" : `1px solid ${c.border}`,
      }}
    >
      <div>
        <span style={{ color: c.textSec, fontSize: c.fontSm }}>{props.label}</span>
        {props.hint && <div style={{ fontSize: c.fontXs, color: c.textSec, opacity: 0.7, marginTop: 2 }}>{props.hint}</div>}
      </div>
      {props.children}
    </div>
  );
}

// ── Load → edit → save plumbing shared by the config panels ──────────────────
// Hoisted here from Integrations.tsx when the retention card (LU-15) became the
// third consumer. Every settings panel that reads a config blob and PUTs it back
// has the same five pieces of state (bump/form/saving/saved/error); keeping one
// copy is what stops them drifting into three slightly different save semantics.
export function useSettingForm<T extends object>(
  get: () => Promise<{ data?: unknown; error?: unknown }>,
  put: (body: T) => Promise<{ error?: unknown }>,
) {
  const [bump, setBump] = useState(0);
  const { data, error, loading } = useGet<T>(get, [bump]);
  const [form, setForm] = useState<T | null>(null);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [saveErr, setSaveErr] = useState<string | null>(null);

  useEffect(() => {
    if (data) setForm(data);
  }, [data]);

  const patch = (p: Partial<T>) => setForm((f) => (f ? { ...f, ...p } : f));

  async function save(body?: T) {
    const payload = body ?? form;
    if (!payload) return;
    setSaving(true);
    setSaveErr(null);
    const { error: err } = await put(payload);
    setSaving(false);
    if (err) setSaveErr(toErrMsg(err));
    else {
      setSaved(true);
      setTimeout(() => setSaved(false), 2200);
      // Refetch rather than trusting the local form: the server may normalise or
      // enrich what it stored (read-only stats, derived fields).
      setBump((b) => b + 1);
    }
  }

  // `data` is the last thing the SERVER returned, untouched by patch — for the
  // one control that must gate on stored state rather than unsaved edits
  // (Sync now, SL-5: a backend switched in the dropdown has no store yet).
  return { form, setForm, patch, save, saving, saved, saveErr, error, loading, data, refetch: () => setBump((b) => b + 1) };
}

// SaveBtn is the panels' only success affordance — a transient green flip, no
// toast. Kept that way deliberately so the settings views stay consistent.
export function SaveBtn({ onClick, saving, saved, disabled }: { onClick: () => void; saving: boolean; saved: boolean; disabled?: boolean }) {
  return (
    <KitBtn primary onClick={onClick} disabled={disabled || saving} style={saved ? { background: c.success, borderColor: c.success } : undefined}>
      {saved ? "✓ Saved" : saving ? "Saving…" : "Save Changes"}
    </KitBtn>
  );
}

export function Status({ loading, error, saveErr }: { loading: boolean; error: string | null; saveErr: string | null }) {
  return (
    <>
      {loading && <InlineLoading />}
      {error && <div style={{ color: c.danger }}>Error: {error}</div>}
      {saveErr && <div style={{ color: c.danger, marginBottom: 10, fontSize: c.fontSm }}>Save failed: {saveErr}</div>}
    </>
  );
}

// App-zone datetime for the settings views (timezone-update §5.2). A thin domain
// wrapper over the shared fmtInAppZone core (utils/datetime) — the same core every
// view's formatter routes through, including the like-named thin wrappers in
// Jobs/Workflows, so all absolute timestamps render in the application timezone.
export function fmtDateTime(iso?: string | null): string {
  return fmtInAppZone(iso);
}

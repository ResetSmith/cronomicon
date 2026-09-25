import { c } from "../../theme";
import { fmtInAppZone } from "../../utils/datetime";

// ── Local row interfaces (generated list types are loose) ──

export interface WfStep {
  type?: string; // job | parallel | branch | sequence | workflow (empty ⇒ job)
  name?: string;
  label?: string;
  id?: string; // child-run trace id for a job node in a run graph (WB-O3 drill-in)
  status?: string | null;
  durationMs?: number | null;
  inputs?: Record<string, string>;
  outputs?: Record<string, string>;
  jobs?: WfStep[]; // present when type=parallel (or a flat branch list) — arms may be sequences (PS-1)
  steps?: WfStep[]; // present when type=sequence — the ordered serial chain (PS-1)
  workflow?: string; // present when type=workflow — the sub-workflow this step runs (SW)
  // Run graphs carry both the pre-rendered label (StepChain) and the structured
  // fields (WC-R1, for the canvas); raw definition steps carry only the latter.
  condition?: { label?: string; type?: string; jobRef?: string; field?: string; operator?: string; value?: string } | null;
  pass?: WfStep[]; // branch: jobs taken when the condition passes (green arm)
  fail?: WfStep[]; // branch: jobs taken when the condition fails (red arm)
}

export interface Workflow {
  id: number;
  // AF-4a — the definition's permanent identity; what R2F-3 keys name
  // disambiguation on when two departments own a workflow of one name.
  uid?: string;
  name: string;
  source?: string; // git | cronomicon — gates the in-app Edit affordance (cronomicon only)
  sourcePath?: string | null; // repo-relative file path, for folder browsing
  description?: string;
  tags?: string[];
  status?: string;
  schedule?: string;
  disabled?: boolean;
  lastRunAt?: string | null;
  nextRunAt?: string | null;
  createdAt?: string | null;
  lastModifiedAt?: string | null;
  // RB-23 (RF-17): DERIVED, display-only — the union of this workflow's
  // constituent jobs' scope→agency memberships. A workflow has no scope of its
  // own, so one spanning two departments carries both. Never authored.
  agencies?: string[];
  // AN — the operator annotation (migration 1060). Operator-owned and
  // sync-preserved, unlike `description`, which Git owns and overwrites on every
  // sync; the two coexist. List rows carry critical + contact, the detail adds
  // notes and its attribution.
  critical?: boolean;
  contact?: string;
  notes?: string;
  notesBy?: string;
  notesAt?: string;
  steps?: WfStep[];
}

export interface WfRun {
  traceId: string;
  workflowId?: number;
  workflowName?: string;
  status?: string;
  scope?: string;
  triggeredBy?: string | null;
  startedAt?: string | null;
  completedAt?: string | null;
  durationMs?: number | null;
  jobTraceIds?: string[];
}

export interface WfRunStep {
  id?: string;
  stepIndex?: number;
  stepType?: string; // system | job
  stepName?: string;
  status?: string;
  jobType?: string;
  startedAt?: string | null;
  finishedAt?: string | null;
  durationMs?: number | null;
  detail?: string | null;
  logFilePath?: string | null;
  contextSnapshot?: Record<string, string> | null;
}

// Status color/label now come from the canonical statusTone/statusLabel
// (components/ui) — the single app-wide vocabulary (CC.19/CC-D3). The workflow
// run tables render "Success" (was "OK") and queued/skipped take the canonical
// tones. badgeStyle below still paints a badge given a color.

// ── Formatting ──

// Compact app-zone time (no year) for workflow run tables (§5.2).
export function fmtTime(iso?: string | null): string {
  return fmtInAppZone(iso, { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
}

// fmtDuration re-exported from the shared datetime utils (CC.19).
export { fmtDuration } from "../../utils/datetime";

// ── Tiny shared atoms ──

// Pill badge matching the prototype (cronomicon-core.jsx Badge): rounded, tinted
// fill, no border. Pass the status color from runStatusColor/outcomeColor.
export function badgeStyle(color: string): React.CSSProperties {
  return {
    display: "inline-flex",
    alignItems: "center",
    gap: 5,
    fontSize: c.fontXs,
    fontWeight: 600,
    color,
    background: `${color}24`,
    // A run status — the one thing that gets the fully-round pill (VU-17).
    borderRadius: c.radiusPill,
    padding: "3px 10px",
    whiteSpace: "nowrap",
  };
}

// FUNCTION so it re-reads the mutable `c` palette on theme toggle (theme.ts).
export const btnStyle = (): React.CSSProperties => ({
  padding: "5px 12px",
  border: `1px solid ${c.border}`,
  borderRadius: c.radiusChip,
  background: c.panel2,
  color: c.text,
  fontSize: c.fontSm,
  fontWeight: 600,
  cursor: "pointer",
  fontFamily: "inherit",
  whiteSpace: "nowrap",
});

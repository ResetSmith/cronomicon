import { Suspense, lazy, useMemo, useState } from "react";
import { api } from "../../api/client";
import { useGet } from "../../hooks";
import { c } from "../../theme";
import { DetailPanel, SectionLabel, statusLabel, statusTone } from "../../components/ui";
import {
  badgeStyle,
  fmtDuration,
  fmtTime,
  type WfRunStep,
  type WfStep,
} from "./shared";
import { StepChain } from "./StepChain";
import { normalizeGraph } from "./graphView";
import { RunLog } from "../history/RunLog";
import { shortTrace } from "../history/shared";

// WC-R1: the dagre canvas loads on demand (xyflow + dagre share the editor's chunk).
const WorkflowCanvasLazy = lazy(() => import("./WorkflowCanvas").then((m) => ({ default: m.WorkflowCanvas })));

interface RunDetailResp {
  status?: string;
  steps?: WfRunStep[];
  graph?: WfStep[];
}

// Shared workflow-run drill-down (WB-O1/O3/O4/O5): a grouped step-graph view
// (StepChain over the per-run snapshot with live status, WB-O3) plus a flat
// execution timeline where each non-system step expands to its redacted resolved
// context and job-run log (WB-O1/O4 — step.id IS the child-run trace id). Consumed
// by both the Workflows page (the expanded row's compact runs table) and History
// (WorkflowRunsTab) so the two surfaces never drift.
export function WorkflowRunDetail({ traceId, live }: { traceId: string; live?: boolean }) {
  // While the run is in flight, poll so step status/duration advance live; stop
  // once terminal (WB-O2).
  const { data, error, loading } = useGet<RunDetailResp>(
    () => api.GET("/workflow-runs/{traceId}", { params: { path: { traceId } } }),
    [traceId],
    live ? 4000 : undefined,
  );
  const steps = data?.steps ?? [];
  const graph = data?.graph ?? [];
  // WC-R1: chain-vs-canvas per the same rule the Workflows page uses, plus the
  // per-node status map so the canvas tints each job by its child run's outcome.
  const g = useMemo(() => normalizeGraph(graph), [graph]);

  if (loading) return <div style={muted()}>Loading steps…</div>;
  if (error) return <div style={{ ...muted(), color: c.danger }}>Error loading run: {error}</div>;
  if (steps.length === 0) {
    // WB-O3: a still-starting run reads differently from one that recorded none.
    const running = data?.status === "running" || data?.status === "queued";
    return <div style={muted()}>{running ? "Steps are starting…" : "No step-level detail recorded for this run."}</div>;
  }

  return (
    // EP-4 Shape A — the workflow-run fetch plus every step's log viewer inside.
    <DetailPanel style={{ display: "flex", flexDirection: "column", gap: 18 }}>
      {graph.length > 0 && (
        <div>
          <SectionLabel>Step Graph</SectionLabel>
          {g.useCanvas ? (
            <Suspense fallback={<div style={muted()}>Loading graph…</div>}>
              <WorkflowCanvasLazy steps={g.def} statusById={g.statusById} height={380} />
            </Suspense>
          ) : (
            <div style={{ overflowX: "auto", paddingBottom: 4 }}>
              <StepChain steps={graph} />
            </div>
          )}
        </div>
      )}
      <div>
        <SectionLabel>Execution Timeline</SectionLabel>
        <div style={{ display: "flex", flexDirection: "column" }}>
          {steps.map((step, i) => (
            <StepRow key={step.id ?? i} step={step} isLast={i === steps.length - 1} />
          ))}
        </div>
      </div>
    </DetailPanel>
  );
}

function StepRow({ step, isLast }: { step: WfRunStep; isLast: boolean }) {
  const [open, setOpen] = useState(false);
  const isSystem = step.stepType === "system";
  const sColor = statusTone(step.status).color;
  const isSkipped = step.status === "skipped";
  const ctx = step.contextSnapshot ?? null;
  const hasCtx = !!ctx && Object.keys(ctx).length > 0;
  // A skipped (untaken-arm) step never ran, so it has no log to drill into.
  const canDrill = !isSystem && !!step.id && !isSkipped;

  return (
    <div style={{ display: "flex" }}>
      {/* timeline rail */}
      <div style={{ width: 26, display: "flex", flexDirection: "column", alignItems: "center", flexShrink: 0 }}>
        <div
          style={{
            width: isSystem ? 9 : 13,
            height: isSystem ? 9 : 13,
            borderRadius: "50%",
            background: isSkipped ? "transparent" : sColor,
            border: isSkipped ? `2px dashed ${c.textSec}` : `2px solid ${sColor}`,
            flexShrink: 0,
            marginTop: isSystem ? 6 : 4,
          }}
        />
        {!isLast && <div style={{ width: 2, flex: 1, background: c.border, minHeight: 10 }} />}
      </div>

      {/* step content */}
      <div style={{ flex: 1, paddingBottom: isLast ? 0 : 6, minWidth: 0 }}>
        {isSystem ? (
          <div style={{ padding: "4px 10px", borderRadius: c.radiusSurface, background: c.panel2, marginBottom: 4 }}>
            <div style={{ display: "flex", alignItems: "center", gap: 6, fontSize: c.fontSm }}>
              <span style={{ color: c.textSec, fontSize: c.fontXs }}>⚙</span>
              <span style={{ fontWeight: 500, color: c.textSec }}>{step.stepName}</span>
              <span style={{ color: c.textSec, fontSize: c.fontXs, marginLeft: "auto", flexShrink: 0 }}>{fmtDuration(step.durationMs)}</span>
            </div>
            {step.detail && (
              <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 3, paddingLeft: 16, fontFamily: c.mono }}>{step.detail}</div>
            )}
          </div>
        ) : (
          <div
            style={{
              padding: "8px 12px",
              borderRadius: c.radiusSurface,
              border: `1px solid ${step.status === "danger" ? `${c.danger}50` : c.border}`,
              background: step.status === "danger" ? `${c.danger}10` : c.panel,
              marginBottom: 4,
              opacity: isSkipped ? 0.6 : 1,
            }}
          >
            <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
              {step.stepType && step.stepType !== "job" && (
                <span
                  title={`${step.stepType} step`}
                  style={{ fontSize: c.fontXs, fontWeight: 700, color: c.accent, border: `1px solid ${c.accent}40`, borderRadius: c.radiusChip, padding: "1px 5px", textTransform: "uppercase", letterSpacing: 0.4 }}
                >
                  {step.stepType}
                </span>
              )}
              {step.jobType && (
                <span style={{ fontSize: c.fontXs, color: c.textSec, fontFamily: c.mono, border: `1px solid ${c.border}`, borderRadius: c.radiusChip, padding: "1px 5px" }}>
                  {step.jobType}
                </span>
              )}
              <span style={{ fontWeight: 600, color: c.text, fontSize: c.fontSm, textDecoration: isSkipped ? "line-through" : "none" }}>
                {step.stepName}
              </span>
              <span style={badgeStyle(sColor)}>{statusLabel(step.status ?? "") || step.status || "—"}</span>
              {step.id && (
                <code title={step.id} style={{ fontFamily: c.mono, fontSize: c.fontXs, color: c.accent }}>
                  {shortTrace(step.id)}
                </code>
              )}
              <span style={{ marginLeft: "auto", display: "flex", alignItems: "center", gap: 10, flexShrink: 0 }}>
                <span style={{ color: c.textSec, fontSize: c.fontXs, fontFamily: c.mono }}>{fmtDuration(step.durationMs)}</span>
                {(canDrill || hasCtx) && (
                  <button onClick={() => setOpen((o) => !o)} style={miniBtn()}>
                    {open ? "Hide" : "Details"}
                  </button>
                )}
              </span>
            </div>
            {step.detail && <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4, fontFamily: c.mono }}>{step.detail}</div>}
            {step.startedAt && (
              <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4 }}>
                {fmtTime(step.startedAt)} → {step.finishedAt ? fmtTime(step.finishedAt) : "…"}
              </div>
            )}
            {open && (
              <div style={{ marginTop: 12, display: "flex", flexDirection: "column", gap: 14 }}>
                {hasCtx && <ContextTable ctx={ctx!} />}
                {/* EP-8b — a step's own status decides whether its log tails;
                    an earlier finished step in the same run must not poll. */}
                {canDrill && <RunLog traceId={step.id!} active={step.status === "running" || step.status === "queued"} />}
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

function ContextTable({ ctx }: { ctx: Record<string, string> }) {
  const entries = Object.entries(ctx);
  return (
    <div>
      <SectionLabel>Resolved context (env + outputs; secrets redacted)</SectionLabel>
      <div style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, overflow: "hidden" }}>
        <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm }}>
          <tbody>
            {entries.map(([k, v], i) => (
              <tr key={k} style={{ borderTop: i === 0 ? "none" : `1px solid ${c.border}` }}>
                <td style={{ padding: "5px 10px", fontFamily: c.mono, color: c.textSec, whiteSpace: "nowrap", verticalAlign: "top", width: 1 }}>{k}</td>
                <td style={{ padding: "5px 10px", fontFamily: c.mono, color: c.text, wordBreak: "break-all" }}>{v}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// Functions, not module-level consts, so a theme toggle re-reads the active
// palette. Capturing c.* tokens at module load freezes the load-time theme
// (default: dark). See theme.ts: "never capture token values in module-level
// constants."
const muted = (): React.CSSProperties => ({ color: c.textSec, fontSize: c.fontSm, padding: "8px 0" });
const miniBtn = (): React.CSSProperties => ({
  padding: "2px 9px",
  background: c.panel2,
  color: c.textSec,
  border: `1px solid ${c.border}`,
  borderRadius: c.radiusChip,
  fontSize: c.fontXs,
  fontWeight: 600,
  cursor: "pointer",
  fontFamily: "inherit",
  whiteSpace: "nowrap",
});

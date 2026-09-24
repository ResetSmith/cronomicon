import { c } from "../../theme";
import { statusTone } from "../../components/ui";
import { fmtDuration, type WfStep } from "./shared";

// Horizontal step chain for a workflow's definition (job → parallel → branch).
// Simplified from the prototype's StepsChain: sequential chips with arrows,
// parallel steps in a dashed group, branch steps as a condition badge with
// nested jobs (the full branch editor is out of scope here).
//
// Radius follows the diagram's own hierarchy (VU-6): the step/condition chips are
// chips, and the dashed parallel group that contains them is the surface — so the
// nesting stays legible instead of collapsing to one uniform corner.

function Dot({ status }: { status?: string | null }) {
  return (
    <span
      style={{
        width: 6,
        height: 6,
        borderRadius: "50%",
        background: statusTone(status).color,
        display: "inline-block",
        flexShrink: 0,
      }}
    />
  );
}

function JobChip({ step }: { step: WfStep }) {
  const clr = statusTone(step.status).color;
  return (
    <div
      style={{
        padding: "5px 11px",
        background: `${clr}14`,
        border: `1px solid ${clr}30`,
        borderRadius: c.radiusChip,
        fontSize: c.fontSm,
        fontWeight: 500,
        color: clr,
        display: "inline-flex",
        alignItems: "center",
        gap: 6,
        whiteSpace: "nowrap",
        opacity: step.status === "skipped" ? 0.55 : 1,
      }}
    >
      <Dot status={step.status} /> {step.name || step.label || "step"}
    </div>
  );
}

function Arrow() {
  return <span style={{ color: c.textSec, alignSelf: "center", fontSize: c.fontSm, flexShrink: 0 }}>→</span>;
}

function StepNum({ n }: { n: number }) {
  return (
    <span
      style={{
        fontSize: c.fontXs,
        fontWeight: 700,
        color: c.textSec,
        background: c.panel2,
        border: `1px solid ${c.border}`,
        borderRadius: c.radiusChip,
        padding: "1px 5px",
      }}
    >
      {n}
    </span>
  );
}

// A branch outcome arm (Pass = green, Fail = red), each a colored rail of chips.
function BranchArm({ label, color, jobs }: { label: string; color: string; jobs: WfStep[] }) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4, paddingLeft: 10, borderLeft: `2px solid ${color}` }}>
      <span style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 700, color, textTransform: "uppercase", letterSpacing: 0.7 }}>{label}</span>
      {jobs.map((j, k) => (
        <JobChip key={k} step={j} />
      ))}
    </div>
  );
}

function StepBody({ step }: { step: WfStep }) {
  if (step.type === "parallel") {
    return (
      <div
        style={{
          display: "flex",
          flexDirection: "column",
          gap: 6,
          border: `1.5px dashed ${c.border}`,
          borderRadius: c.radiusSurface,
          padding: "8px 10px 8px",
          minWidth: 100,
          background: `${c.border}08`,
        }}
      >
        <div
          style={{
            fontSize: c.fontXs,
            fontFamily: c.sansCond,
            fontWeight: 700,
            color: c.textSec,
            letterSpacing: 0.7,
            textTransform: "uppercase",
            borderBottom: `1px dashed ${c.border}`,
            paddingBottom: 4,
            marginBottom: 2,
            textAlign: "center",
            whiteSpace: "nowrap",
          }}
        >
          ∥ {step.label || "parallel"}
        </div>
        {(step.jobs || []).length === 0 ? (
          <span style={{ fontSize: c.fontXs, color: c.textMuted, fontStyle: "italic", textAlign: "center", padding: "4px 0" }}>
            empty
          </span>
        ) : (
          (step.jobs || []).map((j, k) => (
            <div key={k} style={{ display: "flex", flexDirection: "column", gap: 2 }}>
              <JobChip step={j} />
              {j.durationMs != null && (
                <span style={{ fontSize: c.fontXs, color: c.textSec, paddingLeft: 2 }}>{fmtDuration(j.durationMs)}</span>
              )}
            </div>
          ))
        )}
      </div>
    );
  }
  if (step.type === "branch") {
    const hasArms = (step.pass?.length ?? 0) > 0 || (step.fail?.length ?? 0) > 0;
    return (
      <div style={{ display: "flex", flexDirection: "column", gap: 6, alignItems: "flex-start" }}>
        <div
          style={{
            fontSize: c.fontXs,
            fontWeight: 700,
            color: c.warning,
            background: `${c.warning}14`,
            border: `1px solid ${c.warning}50`,
            borderRadius: c.radiusChip,
            padding: "4px 10px",
            whiteSpace: "nowrap",
            alignSelf: "center",
          }}
        >
          ◆ {step.condition?.label || step.label || "condition"}
        </div>
        {hasArms ? (
          <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
            {(step.pass?.length ?? 0) > 0 && <BranchArm label="Pass" color={c.success} jobs={step.pass!} />}
            {(step.fail?.length ?? 0) > 0 && <BranchArm label="Fail" color={c.danger} jobs={step.fail!} />}
          </div>
        ) : (
          (step.jobs || []).length > 0 && (
            <div style={{ display: "flex", flexDirection: "column", gap: 4, paddingLeft: 10, borderLeft: `2px solid ${c.border}` }}>
              {(step.jobs || []).map((j, k) => (
                <JobChip key={k} step={j} />
              ))}
            </div>
          )
        )}
      </div>
    );
  }
  if (step.type === "workflow") {
    // SW: one collapsed chip, deliberately — the child has its own run detail to
    // drill into, and inlining its steps would grow this chain with someone
    // else's graph.
    const clr = statusTone(step.status).color;
    return (
      <div style={{ display: "flex", flexDirection: "column", alignItems: "center", gap: 3 }}>
        <div
          style={{
            padding: "5px 11px",
            background: `${clr}14`,
            border: `1px dashed ${clr}55`,
            borderRadius: c.radiusChip,
            fontSize: c.fontSm,
            fontWeight: 500,
            color: clr,
            display: "inline-flex",
            alignItems: "center",
            gap: 6,
            whiteSpace: "nowrap",
          }}
        >
          <Dot status={step.status} /> ⧉ {step.workflow || step.name || "workflow"}
        </div>
        {step.durationMs != null && <span style={{ fontSize: c.fontXs, color: c.textSec }}>{fmtDuration(step.durationMs)}</span>}
      </div>
    );
  }
  if (step.type === "sequence") {
    // PS-1: a serial chain. graphView routes a sequence INSIDE an arm to the
    // canvas (it sets `nested`), but a TOP-LEVEL sequence under the node
    // threshold still arrives here — and without this case it fell through to
    // the job default below, rendering the whole chain as one chip labelled
    // "step" with every job in it invisible.
    const chain = step.steps || [];
    return (
      <div
        style={{
          display: "flex",
          flexDirection: "column",
          gap: 6,
          border: `1.5px solid ${c.border}`,
          borderRadius: c.radiusSurface,
          padding: "8px 10px 8px",
          minWidth: 100,
          background: `${c.border}08`,
        }}
      >
        <div
          style={{
            fontSize: c.fontXs,
            fontFamily: c.sansCond,
            fontWeight: 700,
            color: c.textSec,
            letterSpacing: 0.7,
            textTransform: "uppercase",
            borderBottom: `1px solid ${c.border}`,
            paddingBottom: 4,
            marginBottom: 2,
            textAlign: "center",
            whiteSpace: "nowrap",
          }}
        >
          → {step.label || "sequence"}
        </div>
        {chain.length === 0 ? (
          <span style={{ fontSize: c.fontXs, color: c.textMuted, fontStyle: "italic", textAlign: "center", padding: "4px 0" }}>
            empty
          </span>
        ) : (
          chain.map((s, k) => (
            <div key={k} style={{ display: "flex", flexDirection: "column", gap: 2 }}>
              <StepBody step={s} />
            </div>
          ))
        )}
      </div>
    );
  }
  // type=job (default)
  return (
    <div style={{ display: "flex", flexDirection: "column", alignItems: "center", gap: 3 }}>
      <JobChip step={step} />
      {step.durationMs != null && <span style={{ fontSize: c.fontXs, color: c.textSec }}>{fmtDuration(step.durationMs)}</span>}
    </div>
  );
}

export function StepChain({ steps }: { steps: WfStep[] }) {
  if (steps.length === 0) {
    return <div style={{ color: c.textSec, fontSize: c.fontSm }}>No steps defined for this workflow.</div>;
  }
  return (
    <div
      style={{
        display: "flex",
        alignItems: "flex-start",
        gap: 12,
        flexWrap: "nowrap",
        overflowX: "auto",
        width: "100%",
        paddingBottom: 8,
      }}
    >
      {steps.map((step, i) => (
        <div key={i} style={{ display: "flex", alignItems: "flex-start", gap: 12, flexShrink: 0 }}>
          <div style={{ display: "flex", flexDirection: "column", alignItems: "center", gap: 4 }}>
            <StepNum n={i + 1} />
            <StepBody step={step} />
          </div>
          {i < steps.length - 1 && <div style={{ marginTop: 22 }}><Arrow /></div>}
        </div>
      ))}
    </div>
  );
}

// graphView.ts — WC-R1: normalize the two step shapes the API serves into the
// canvas wire shape (DefStep[]), plus a per-node run-status map keyed by the
// deterministic canvas path ids toCanvasTree mints (`s0`, `s1.j0`, `s2.pass.0` …).
// Pure — no React, no dagre — unit-tested in graphView.test.ts.
//
// Two sources, one target:
//   • Workflow.steps (GET /workflows) — RAW workflow.Step JSON: branch arms are
//     `{steps:[…]}` objects and the condition is structured. Already DefStep-
//     shaped, but normalized here anyway because the loose WfStep TS type says
//     arms are flat arrays and the runtime disagrees.
//   • A run's graph (GET /workflow-runs/{id} → .graph) — UI-shaped: arms ARE
//     flat arrays, the condition carries {label, type, jobRef, …}, and job
//     nodes carry live status. Statuses land in statusById under the node's
//     canvas path id so WorkflowCanvas can tint without knowing the source.
//
// The chain-vs-canvas decision also lives here: StepChain flattens any branch
// arm that contains a container (`nested`), and a long graph turns its single
// horizontal row into a blind sideways scroll (`nodeCount`). Either condition
// says the dagre canvas is the honest renderer.

import { type DefStep, type DefBranch, type DefCondition } from "./canvasModel";
import { type WfStep } from "./shared";

export interface NormalizedGraph {
  def: DefStep[];
  statusById: Record<string, string>;
  nodeCount: number; // containers + leaves, all depths
  nested: boolean; // any branch arm containing a non-job step
  useCanvas: boolean; // nested, or too long for the horizontal chain
}

// Above this many nodes the numbered chain stops being a diagram and becomes a
// scrollbar; hand the graph to the canvas (pan/zoom/minimap).
const CANVAS_NODE_THRESHOLD = 10;

// A branch arm as either wire shape: raw `{steps:[…]}` or the run graph's flat list.
type ArmShape = WfStep[] | { steps?: WfStep[] } | null | undefined;

const armSteps = (arm: ArmShape): WfStep[] => {
  if (!arm) return [];
  if (Array.isArray(arm)) return arm;
  return arm.steps ?? [];
};

export function normalizeGraph(steps: WfStep[]): NormalizedGraph {
  const statusById: Record<string, string> = {};
  let nodeCount = 0;
  let nested = false;

  const walkSeq = (seq: WfStep[], prefix: (i: number) => string, inArm: boolean): DefStep[] =>
    seq.map((s, i) => walk(s, prefix(i), inArm));

  function walk(s: WfStep, id: string, inArm: boolean): DefStep {
    nodeCount++;
    const type =
      s.type === "parallel" || s.type === "branch" || s.type === "sequence" || s.type === "workflow" ? s.type : "job";
    if (inArm && type !== "job") nested = true;
    if (type === "parallel") {
      return {
        type,
        label: s.label,
        // PS-1: an arm may be a sequence — a shape the chain cannot draw, so it
        // counts as nesting (the `inArm` flag doubles as "container where the
        // chain expects a leaf").
        jobs: (s.jobs ?? []).map((j, k) => walk(j, `${id}.j${k}`, true)),
      };
    }
    if (type === "workflow") {
      // SW: a collapsed node — one node, whatever the child contains. That is
      // also why it does NOT set `nested`: the parent's graph does not grow with
      // the child's, so the chain renderer can still draw it.
      if (s.status) statusById[id] = s.status;
      return { type, name: s.name, label: s.label, workflow: s.workflow };
    }
    if (type === "sequence") {
      return {
        type,
        label: s.label,
        steps: (s.steps ?? []).map((ss, k) => walk(ss, `${id}.q${k}`, false)),
      };
    }
    if (type === "branch") {
      const pass: DefBranch = { steps: walkSeq(armSteps(s.pass as ArmShape), (k) => `${id}.pass.${k}`, true) };
      const fail: DefBranch = { steps: walkSeq(armSteps(s.fail as ArmShape), (k) => `${id}.fail.${k}`, true) };
      return { type, label: s.label, condition: normCondition(s.condition), pass, fail };
    }
    if (s.status) statusById[id] = s.status;
    return { type: "job", name: s.name, label: s.label };
  }

  const def = walkSeq(steps, (i) => `s${i}`, false);
  return { def, statusById, nodeCount, nested, useCanvas: nested || nodeCount > CANVAS_NODE_THRESHOLD };
}

// The run graph's condition may be label-only (older wire); the canvas builds its
// diamond text with conditionLabel(), whose default case renders jobRef verbatim —
// so a bare label rides through as {type:"label", jobRef:label}.
function normCondition(c: WfStep["condition"]): DefCondition {
  const anyC = (c ?? {}) as { label?: string; type?: string; jobRef?: string; field?: string; operator?: string; value?: string };
  if (anyC.type) {
    return { type: anyC.type, jobRef: anyC.jobRef ?? "", field: anyC.field, operator: anyC.operator, value: anyC.value };
  }
  return { type: "label", jobRef: anyC.label ?? "condition" };
}

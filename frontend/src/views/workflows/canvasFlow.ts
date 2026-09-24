// canvasFlow.ts — derive a flat React Flow node/edge graph from the canvas tree for
// a top-down, dagre-laid READ render (WC-P2). Mirrors the Option B edge-derivation
// spec (`workflow-update-2.md` §4): a single START source + END sink, parallel
// fork/join anchors, a branch condition diamond with green/red Pass/Fail edges, and
// A12 data-dependency overlay edges. Pure — no React, no dagre — unit-tested in
// canvasFlow.test.ts. The renderer (WorkflowCanvas.tsx) lays this out with dagre and
// excludes the `data` overlay edges from ranking so the left-to-right flow stays put.

import { type CanvasNode, conditionLabel } from "./canvasModel";

export type FlowNodeKind = "start" | "end" | "job" | "fork" | "join" | "condition" | "subworkflow";
export type FlowEdgeKind = "sequence" | "fanOut" | "fanIn" | "pass" | "fail" | "data";

export interface FlowNode {
  id: string;
  kind: FlowNodeKind;
  label: string;
  sublabel?: string; // e.g. a parallel block's `∥ label`
  canvasId?: string; // the source CanvasNode id (job / branch) for later drill-in
}
export interface FlowEdge {
  id: string;
  source: string;
  target: string;
  kind: FlowEdgeKind;
  label?: string;
}
export interface FlowGraph {
  nodes: FlowNode[];
  edges: FlowEdge[];
}

export const START_ID = "__start";
export const END_ID = "__end";

/** Whether an edge kind participates in dagre ranking (structural flow) vs. is a
 *  post-layout overlay spline (A12 data deps — WG7). */
export const isStructuralEdge = (kind: FlowEdgeKind): boolean => kind !== "data";

export function treeToFlow(tree: CanvasNode[]): FlowGraph {
  const nodes: FlowNode[] = [];
  const edges: FlowEdge[] = [];
  const producerOf = new Map<string, string>(); // job name → flow node id (first producer wins)
  const a12: Array<{ consumer: string; envKey: string; fromStep: string; fromOutput: string }> = [];
  let edgeSeq = 0;

  const addEdge = (source: string, target: string, kind: FlowEdgeKind, label?: string) =>
    edges.push({ id: `e${edgeSeq++}`, source, target, kind, label });

  nodes.push({ id: START_ID, kind: "start", label: "Start" });
  nodes.push({ id: END_ID, kind: "end", label: "End" });

  const recordProducer = (name: string | undefined, nodeId: string) => {
    if (name && !producerOf.has(name)) producerOf.set(name, nodeId);
  };
  const recordInputs = (node: CanvasNode) => {
    for (const r of node.inputs ?? []) {
      if (r.envKey && r.fromStep) a12.push({ consumer: node.id, envKey: r.envKey, fromStep: r.fromStep, fromOutput: r.fromOutput });
    }
  };

  // emitStep returns the entry node id(s) and exit node id(s) of one step; emitSeq
  // chains a sub-sequence (sequence edge from each prior exit to each next entry).
  function emitStep(node: CanvasNode): { entry: string[]; exit: string[] } {
    if (node.kind === "job") {
      nodes.push({ id: node.id, kind: "job", label: node.name || node.label || "job", canvasId: node.id });
      recordProducer(node.name, node.id);
      recordInputs(node);
      return { entry: [node.id], exit: [node.id] };
    }
    if (node.kind === "workflow") {
      // SW: drawn COLLAPSED — one node, not the child's graph inlined. The child
      // has its own canvas to drill into, and inlining would grow this one
      // without bound with someone else's steps (and re-grow it whenever they
      // edited theirs).
      nodes.push({
        id: node.id,
        kind: "subworkflow",
        label: node.name || node.label || "workflow",
        sublabel: node.workflow ? `⧉ ${node.workflow}` : "⧉",
        canvasId: node.id,
      });
      recordProducer(node.name, node.id);
      recordInputs(node);
      return { entry: [node.id], exit: [node.id] };
    }
    if (node.kind === "sequence") {
      // PS-1: an inline serial chain — drawn as its steps, chained by emitSeq.
      // (Standalone it is indistinguishable from writing the steps at this
      // level; inside a parallel it becomes one concurrent lane.)
      return emitSeq(node.steps ?? []);
    }
    if (node.kind === "parallel") {
      const fork = `${node.id}.fork`;
      const join = `${node.id}.join`;
      nodes.push({ id: fork, kind: "fork", label: "", sublabel: node.label ? `∥ ${node.label}` : "∥" });
      nodes.push({ id: join, kind: "join", label: "" });
      for (const child of node.jobs ?? []) {
        // PS-1: an arm is a leaf job or a sequence lane; either way its entry
        // hangs off the fork and its exit feeds the join.
        const arm = emitStep(child);
        for (const en of arm.entry) addEdge(fork, en, "fanOut");
        for (const ex of arm.exit) addEdge(ex, join, "fanIn");
        // An empty sequence arm (authoring intermediate) still keeps flow sane.
        if (arm.entry.length === 0) addEdge(fork, join, "fanOut");
      }
      return { entry: [fork], exit: [join] };
    }
    // branch — a condition diamond with two arms; each arm's tail merges into the
    // successor via the sequence edges emitSeq draws.
    const cond = `${node.id}.cond`;
    nodes.push({ id: cond, kind: "condition", label: conditionLabel(node.condition), canvasId: node.id });
    const pass = emitSeq(node.pass ?? []);
    const fail = emitSeq(node.fail ?? []);
    for (const e of pass.entry) addEdge(cond, e, "pass", "Pass");
    for (const e of fail.entry) addEdge(cond, e, "fail", "Fail");
    const exit = [...(pass.exit.length ? pass.exit : [cond]), ...(fail.exit.length ? fail.exit : [cond])];
    return { entry: [cond], exit };
  }

  function emitSeq(seq: CanvasNode[]): { entry: string[]; exit: string[] } {
    let prevExit: string[] | null = null;
    let firstEntry: string[] | null = null;
    for (const node of seq) {
      const { entry, exit } = emitStep(node);
      if (firstEntry === null) firstEntry = entry;
      if (prevExit) for (const p of prevExit) for (const q of entry) addEdge(p, q, "sequence");
      prevExit = exit;
    }
    return { entry: firstEntry ?? [], exit: prevExit ?? [] };
  }

  const root = emitSeq(tree);
  for (const e of root.entry) addEdge(START_ID, e, "sequence");
  for (const x of root.exit) addEdge(x, END_ID, "sequence");
  if (root.entry.length === 0) addEdge(START_ID, END_ID, "sequence"); // empty workflow

  // A12 data-dependency overlay edges (producer → consumer).
  for (const dep of a12) {
    const producer = producerOf.get(dep.fromStep);
    if (producer) addEdge(producer, dep.consumer, "data", `${dep.envKey} ← ${dep.fromOutput || dep.fromStep}`);
  }

  return { nodes, edges };
}

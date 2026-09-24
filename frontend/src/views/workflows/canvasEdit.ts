// canvasEdit.ts — WC-P3: pure, immutable mutations over the canvas authoring tree
// (CanvasNode[], from canvasModel.ts), plus node factories with stable client ids.
// The editor UI (WorkflowCanvasEditor.tsx) is a thin shell over these — every edit
// returns a NEW tree, so React state updates are trivial and undo/redo (a future
// fast-follow) is just a history of trees. No React, no network — unit-tested.
//
// A "lane" is one ordered sub-sequence: the root, a parallel block's arms, a
// sequence's steps, or a branch arm (pass/fail). Inserts/moves are addressed by
// (containerId, which); removes/updates/moves by node id (unique across the tree).

import { type CanvasNode, type CanvasKind } from "./canvasModel";

export type LaneWhich = "root" | "jobs" | "steps" | "pass" | "fail";
export interface LaneRef {
  containerId: string | null; // null ⇒ the root lane
  which: LaneWhich;
}

// ── Stable client ids ─────────────────────────────────────────────────────────────

let idSeq = 0;
/** A fresh, process-unique node id. Stable across edits (unlike the path-based ids
 *  `toCanvasTree` assigns), so React keys and selection survive reorders/inserts. */
export const freshId = (): string => `cn${++idSeq}`;
/** Test hook: reset the id counter for deterministic ids. */
export const __resetIds = (n = 0): void => {
  idSeq = n;
};

// ── Node factories ─────────────────────────────────────────────────────────────────

export function newJobNode(name = "", jobSource?: string): CanvasNode {
  return { id: freshId(), kind: "job", name, jobSource, inputs: [], retries: null, backoffSeconds: null, continueOnError: null };
}
export function newWorkflowNode(name = "", workflow = ""): CanvasNode {
  return { id: freshId(), kind: "workflow", name, workflow, inputs: [], retries: null, backoffSeconds: null, continueOnError: null };
}
export function newParallelNode(label = ""): CanvasNode {
  return { id: freshId(), kind: "parallel", label, jobs: [] };
}
export function newSequenceNode(label = ""): CanvasNode {
  return { id: freshId(), kind: "sequence", label, steps: [] };
}
export function newBranchNode(): CanvasNode {
  return {
    id: freshId(),
    kind: "branch",
    condition: { type: "job_status", jobRef: "", field: "", operator: "==", value: "" },
    pass: [],
    fail: [],
  };
}
export function newNode(kind: CanvasKind): CanvasNode {
  return kind === "parallel"
    ? newParallelNode()
    : kind === "branch"
      ? newBranchNode()
      : kind === "sequence"
        ? newSequenceNode()
        : kind === "workflow"
          ? newWorkflowNode()
          : newJobNode();
}

// ── Internal lookups ─────────────────────────────────────────────────────────────────

const clamp = (i: number, lo: number, hi: number) => Math.max(lo, Math.min(i, hi));

/** Mutating lookup of a node by id within a (cloned) tree. */
function findById(tree: CanvasNode[], id: string): CanvasNode | null {
  for (const n of tree) {
    if (n.id === id) return n;
    if (n.kind === "parallel") {
      const hit = findById(n.jobs ?? [], id);
      if (hit) return hit;
    } else if (n.kind === "sequence") {
      const hit = findById(n.steps ?? [], id);
      if (hit) return hit;
    } else if (n.kind === "branch") {
      const hit = findById(n.pass ?? [], id) ?? findById(n.fail ?? [], id);
      if (hit) return hit;
    }
  }
  return null;
}

/** Mutating resolve of a lane's array within a (cloned) tree, initializing arms/jobs. */
function resolveLane(tree: CanvasNode[], lane: LaneRef): CanvasNode[] | null {
  if (lane.which === "root") return tree;
  const container = lane.containerId ? findById(tree, lane.containerId) : null;
  if (!container) return null;
  if (lane.which === "jobs" && container.kind === "parallel") return (container.jobs ??= []);
  if (lane.which === "steps" && container.kind === "sequence") return (container.steps ??= []);
  if (lane.which === "pass" && container.kind === "branch") return (container.pass ??= []);
  if (lane.which === "fail" && container.kind === "branch") return (container.fail ??= []);
  return null;
}

// ── Mutations (all return a NEW tree; the input is never mutated) ───────────────────

/** Insert `node` into `lane` at `index` (clamped). Returns the input unchanged if the
 *  lane can't be resolved. */
export function insertNode(tree: CanvasNode[], lane: LaneRef, index: number, node: CanvasNode): CanvasNode[] {
  const next = structuredClone(tree) as CanvasNode[];
  const arr = resolveLane(next, lane);
  if (!arr) return tree;
  arr.splice(clamp(index, 0, arr.length), 0, node);
  return next;
}

/** Append `node` to the end of `lane`. */
export function appendNode(tree: CanvasNode[], lane: LaneRef, node: CanvasNode): CanvasNode[] {
  return insertNode(tree, lane, Number.MAX_SAFE_INTEGER, node);
}

/** Remove the node with `id` from wherever it lives — cascading its whole subtree. */
export function removeNode(tree: CanvasNode[], id: string): CanvasNode[] {
  const strip = (arr: CanvasNode[]): CanvasNode[] =>
    arr
      .filter((n) => n.id !== id)
      .map((n) =>
        n.kind === "parallel"
          ? { ...n, jobs: strip(n.jobs ?? []) }
          : n.kind === "sequence"
            ? { ...n, steps: strip(n.steps ?? []) }
            : n.kind === "branch"
              ? { ...n, pass: strip(n.pass ?? []), fail: strip(n.fail ?? []) }
              : n,
      );
  return strip(tree);
}

/** Shallow-merge `patch` into the node with `id`. */
export function updateNode(tree: CanvasNode[], id: string, patch: Partial<CanvasNode>): CanvasNode[] {
  const walk = (arr: CanvasNode[]): CanvasNode[] =>
    arr.map((n) => {
      let m = n.id === id ? ({ ...n, ...patch } as CanvasNode) : n;
      if (m.kind === "parallel") m = { ...m, jobs: walk(m.jobs ?? []) };
      else if (m.kind === "sequence") m = { ...m, steps: walk(m.steps ?? []) };
      else if (m.kind === "branch") m = { ...m, pass: walk(m.pass ?? []), fail: walk(m.fail ?? []) };
      return m;
    });
  return walk(tree);
}

/** Swap the node with `id` with its neighbour (`dir` = -1 up / +1 down) within its
 *  own lane. No-op at a lane boundary. */
export function moveNode(tree: CanvasNode[], id: string, dir: -1 | 1): CanvasNode[] {
  const swap = (arr: CanvasNode[]): CanvasNode[] => {
    const i = arr.findIndex((n) => n.id === id);
    if (i >= 0) {
      const j = i + dir;
      if (j < 0 || j >= arr.length) return arr;
      const copy = arr.slice();
      [copy[i], copy[j]] = [copy[j], copy[i]];
      return copy;
    }
    return arr.map((n) =>
      n.kind === "parallel"
        ? { ...n, jobs: swap(n.jobs ?? []) }
        : n.kind === "sequence"
          ? { ...n, steps: swap(n.steps ?? []) }
          : n.kind === "branch"
            ? { ...n, pass: swap(n.pass ?? []), fail: swap(n.fail ?? []) }
            : n,
    );
  };
  return swap(tree);
}

/** A sensible default A12 env key derived from the producer's job name (UPPER_SNAKE),
 *  made unique against the consumer's existing keys. Used when a canvas drag wires a
 *  producer → consumer so the new input is immediately **valid** — it draws its data
 *  edge and survives save — instead of a blank-keyed placeholder that renders nothing
 *  and is dropped on serialize. The user renames it (and sets `fromOutput`) in the
 *  inspector. */
export function defaultInputKey(producerName: string, existingKeys: string[]): string {
  const base =
    (producerName || "input")
      .toUpperCase()
      .replace(/[^A-Z0-9]+/g, "_")
      .replace(/^_+|_+$/g, "") || "INPUT";
  const taken = new Set(existingKeys);
  if (!taken.has(base)) return base;
  for (let i = 2; ; i++) {
    const candidate = `${base}_${i}`;
    if (!taken.has(candidate)) return candidate;
  }
}

/** Re-assign fresh stable ids across a tree (used when loading a definition, whose
 *  `toCanvasTree` ids are path-based and would shift on edit). */
export function withFreshIds(tree: CanvasNode[]): CanvasNode[] {
  return tree.map((n) => {
    if (n.kind === "parallel") return { ...n, id: freshId(), jobs: withFreshIds(n.jobs ?? []) };
    if (n.kind === "sequence") return { ...n, id: freshId(), steps: withFreshIds(n.steps ?? []) };
    if (n.kind === "branch") return { ...n, id: freshId(), pass: withFreshIds(n.pass ?? []), fail: withFreshIds(n.fail ?? []) };
    return { ...n, id: freshId() };
  });
}

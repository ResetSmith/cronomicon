import { describe, expect, it } from "vitest";
import { toCanvasTree, toSteps, validateTree, upstreamProducers, type DefStep } from "./canvasModel";
import { treeToFlow } from "./canvasFlow";
import { newSequenceNode, newJobNode, appendNode, withFreshIds } from "./canvasEdit";
import { normalizeGraph } from "./graphView";
import { type WfStep } from "./shared";

// PS-1: parallel arms may be sequences. These lock the frontend model to the
// engine's rules: round-trip fidelity, arm validation, A12 scoping, flow-graph
// edges, and the chain-vs-canvas decision.

const suite: DefStep[] = [
  { type: "job", name: "seed" },
  {
    type: "parallel",
    label: "chains",
    jobs: [
      { type: "sequence", label: "db", steps: [{ type: "job", name: "c1" }, { type: "job", name: "c2" }] },
      { type: "job", name: "solo" },
    ],
  },
  { type: "job", name: "after" },
];

describe("PS-1 sequence arms", () => {
  it("round-trips a sequence arm through the canvas tree", () => {
    const tree = toCanvasTree(suite);
    expect(tree[1].jobs?.[0]?.kind).toBe("sequence");
    expect(tree[1].jobs?.[0]?.steps?.map((s) => s.name)).toEqual(["c1", "c2"]);
    expect(tree[1].jobs?.[0]?.steps?.[1]?.id).toBe("s1.j0.q1");
    const out = toSteps(tree);
    expect(out).toEqual([
      { type: "job", name: "seed" },
      {
        type: "parallel",
        label: "chains",
        jobs: [
          { type: "sequence", label: "db", steps: [{ type: "job", name: "c1" }, { type: "job", name: "c2" }] },
          { type: "job", name: "solo" },
        ],
      },
      { type: "job", name: "after" },
    ]);
  });

  it("validates arm rules like the engine", () => {
    const ok = toCanvasTree(suite);
    expect(validateTree(ok)).toEqual([]);
    const emptySeq = toCanvasTree([{ type: "parallel", jobs: [{ type: "sequence", steps: [] }] }]);
    expect(validateTree(emptySeq).some((e) => e.message.includes("sequence arm requires"))).toBe(true);
    const branchArm = toCanvasTree([
      { type: "parallel", jobs: [{ type: "branch", condition: { type: "job_status", jobRef: "x" }, pass: { steps: [] }, fail: { steps: [] } }] },
    ]);
    expect(validateTree(branchArm).some((e) => e.message.includes("plain job or a sequence"))).toBe(true);
  });

  it("scopes upstream producers per arm (siblings invisible, chain serial)", () => {
    const tree = toCanvasTree(suite);
    const c2 = tree[1].jobs![0].steps![1]; // second step of the sequence arm
    expect(upstreamProducers(tree, c2.id).sort()).toEqual(["c1", "seed"]); // NOT solo
    const solo = tree[1].jobs![1];
    expect(upstreamProducers(tree, solo.id)).toEqual(["seed"]);
    const after = tree[2];
    expect(upstreamProducers(tree, after.id).sort()).toEqual(["c1", "c2", "seed", "solo"]);
  });

  it("draws a sequence arm as a chained lane between fork and join", () => {
    const { edges } = treeToFlow(toCanvasTree(suite));
    const byKind = (k: string) => edges.filter((e) => e.kind === k);
    // fork fans out to c1 (chain entry) and solo; c2 and solo fan into the join.
    expect(byKind("fanOut").map((e) => e.target).sort()).toEqual(["s1.j0.q0", "s1.j1"]);
    expect(byKind("fanIn").map((e) => e.source).sort()).toEqual(["s1.j0.q1", "s1.j1"]);
    // c1 → c2 is a sequence edge inside the arm.
    expect(edges.some((e) => e.kind === "sequence" && e.source === "s1.j0.q0" && e.target === "s1.j0.q1")).toBe(true);
  });

  it("canvasEdit builds and re-ids sequence nodes", () => {
    let tree = toCanvasTree([{ type: "parallel", jobs: [] }]);
    tree = appendNode(tree, { containerId: tree[0].id, which: "jobs" }, newSequenceNode("lane"));
    const seq = tree[0].jobs![0];
    tree = appendNode(tree, { containerId: seq.id, which: "steps" }, newJobNode("inner"));
    expect(tree[0].jobs![0].steps![0].name).toBe("inner");
    const fresh = withFreshIds(tree);
    expect(fresh[0].jobs![0].steps![0].name).toBe("inner");
    expect(fresh[0].jobs![0].steps![0].id).not.toBe(tree[0].jobs![0].steps![0].id);
  });

  it("graphView maps sequence statuses and flips to the canvas", () => {
    const graph: WfStep[] = [
      {
        type: "parallel",
        jobs: [
          { type: "sequence", steps: [{ type: "job", name: "c1", status: "success" }, { type: "job", name: "c2", status: "danger" }] },
          { type: "job", name: "solo", status: "success" },
        ],
      },
    ];
    const g = normalizeGraph(graph);
    expect(g.nested).toBe(true);
    expect(g.useCanvas).toBe(true);
    expect(g.statusById["s0.j0.q0"]).toBe("success");
    expect(g.statusById["s0.j0.q1"]).toBe("danger");
    expect(g.statusById["s0.j1"]).toBe("success");
  });
});

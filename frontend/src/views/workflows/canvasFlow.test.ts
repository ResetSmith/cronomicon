import { describe, it, expect } from "vitest";
import { toCanvasTree, type DefStep } from "./canvasModel";
import { treeToFlow, isStructuralEdge, START_ID, END_ID, type FlowEdgeKind } from "./canvasFlow";

const workedExample: DefStep[] = [
  { type: "job", name: "build" },
  { type: "parallel", label: "tests", jobs: [{ type: "job", name: "unit" }, { type: "job", name: "integration" }] },
  {
    type: "branch",
    condition: { type: "job_status", jobRef: "unit" },
    pass: { steps: [{ type: "job", name: "deploy" }] },
    fail: { steps: [{ type: "job", name: "notify" }] },
  },
  { type: "job", name: "cleanup", inputs: { ARTIFACT: { fromStep: "build", fromOutput: "artifactUrl" } } },
];

const flow = treeToFlow(toCanvasTree(workedExample));
const hasEdge = (source: string, target: string, kind: FlowEdgeKind) => flow.edges.some((e) => e.source === source && e.target === target && e.kind === kind);

describe("treeToFlow — render graph derivation", () => {
  it("emits START/END anchors plus a node per job / fork / join / condition", () => {
    expect(flow.nodes.map((n) => n.id).sort()).toEqual(
      ["__end", "__start", "s0", "s1.fork", "s1.j0", "s1.j1", "s1.join", "s2.cond", "s2.fail.0", "s2.pass.0", "s3"].sort(),
    );
    expect(flow.nodes.find((n) => n.id === "s1.fork")?.kind).toBe("fork");
    expect(flow.nodes.find((n) => n.id === "s1.join")?.kind).toBe("join");
    expect(flow.nodes.find((n) => n.id === "s2.cond")?.kind).toBe("condition");
    expect(flow.nodes.find((n) => n.id === "s2.cond")?.label).toBe("unit succeeded");
    expect(flow.nodes.find((n) => n.id === "s0")?.label).toBe("build");
    // job nodes carry their CanvasNode id for drill-in.
    expect(flow.nodes.find((n) => n.id === "s3")?.canvasId).toBe("s3");
  });

  it("wires the sequence: START → build → ∥ → branch → cleanup → END", () => {
    expect(hasEdge(START_ID, "s0", "sequence")).toBe(true);
    expect(hasEdge("s0", "s1.fork", "sequence")).toBe(true);
    expect(hasEdge("s1.join", "s2.cond", "sequence")).toBe(true);
    expect(hasEdge("s3", END_ID, "sequence")).toBe(true);
  });

  it("fans a parallel out and back in", () => {
    expect(hasEdge("s1.fork", "s1.j0", "fanOut")).toBe(true);
    expect(hasEdge("s1.fork", "s1.j1", "fanOut")).toBe(true);
    expect(hasEdge("s1.j0", "s1.join", "fanIn")).toBe(true);
    expect(hasEdge("s1.j1", "s1.join", "fanIn")).toBe(true);
  });

  it("draws Pass/Fail edges and merges both arms into the successor", () => {
    expect(hasEdge("s2.cond", "s2.pass.0", "pass")).toBe(true);
    expect(hasEdge("s2.cond", "s2.fail.0", "fail")).toBe(true);
    expect(hasEdge("s2.pass.0", "s3", "sequence")).toBe(true); // pass-arm tail merges
    expect(hasEdge("s2.fail.0", "s3", "sequence")).toBe(true); // fail-arm tail merges
  });

  it("overlays the A12 data dependency build → cleanup", () => {
    const dataEdge = flow.edges.find((e) => e.kind === "data");
    expect(dataEdge).toBeTruthy();
    expect(dataEdge!.source).toBe("s0"); // producer "build"
    expect(dataEdge!.target).toBe("s3"); // consumer "cleanup"
    expect(dataEdge!.label).toBe("ARTIFACT ← artifactUrl");
    expect(isStructuralEdge("data")).toBe(false);
    expect(isStructuralEdge("sequence")).toBe(true);
  });

  it("handles a flat chain", () => {
    const f = treeToFlow(toCanvasTree([{ type: "job", name: "a" }, { type: "job", name: "b" }]));
    expect(f.nodes.map((n) => n.id).sort()).toEqual(["__end", "__start", "s0", "s1"].sort());
    expect(f.edges.some((e) => e.source === START_ID && e.target === "s0")).toBe(true);
    expect(f.edges.some((e) => e.source === "s0" && e.target === "s1")).toBe(true);
    expect(f.edges.some((e) => e.source === "s1" && e.target === END_ID)).toBe(true);
  });

  it("renders an empty workflow as just START → END", () => {
    const f = treeToFlow([]);
    expect(f.nodes.map((n) => n.id).sort()).toEqual([END_ID, START_ID].sort());
    expect(f.edges.some((e) => e.source === START_ID && e.target === END_ID)).toBe(true);
  });
});

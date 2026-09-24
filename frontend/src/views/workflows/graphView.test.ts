import { describe, expect, it } from "vitest";
import { normalizeGraph } from "./graphView";
import { type WfStep } from "./shared";

// The two wire shapes normalizeGraph must unify: the raw definition (arms as
// {steps:[…]}, structured condition) and the run graph (flat arms, label +
// structured condition, per-node status).

describe("normalizeGraph", () => {
  it("passes a raw definition through, arms unwrapped from {steps}", () => {
    const raw = [
      { type: "job", name: "build" },
      {
        type: "branch",
        condition: { type: "job_status", jobRef: "build" },
        pass: { steps: [{ type: "job", name: "deploy" }] },
        fail: { steps: [{ type: "job", name: "rollback" }] },
      },
    ] as unknown as WfStep[];
    const g = normalizeGraph(raw);
    expect(g.def).toHaveLength(2);
    expect(g.def[1].pass?.steps?.[0]?.name).toBe("deploy");
    expect(g.def[1].fail?.steps?.[0]?.name).toBe("rollback");
    expect(g.def[1].condition?.type).toBe("job_status");
    expect(g.nested).toBe(false);
    expect(g.useCanvas).toBe(false); // 4 nodes, flat — StepChain's territory
  });

  it("maps a run graph's statuses to canvas path ids", () => {
    const graph: WfStep[] = [
      { type: "job", name: "build", status: "success" },
      {
        type: "parallel",
        jobs: [
          { type: "job", name: "a", status: "success" },
          { type: "job", name: "b", status: "danger" },
        ],
      },
      {
        type: "branch",
        condition: { label: "build succeeded", type: "job_status", jobRef: "build" },
        pass: [{ type: "job", name: "deploy", status: "success" }],
        fail: [{ type: "job", name: "rollback", status: "skipped" }],
      },
    ];
    const g = normalizeGraph(graph);
    expect(g.statusById["s0"]).toBe("success");
    expect(g.statusById["s1.j0"]).toBe("success");
    expect(g.statusById["s1.j1"]).toBe("danger");
    expect(g.statusById["s2.pass.0"]).toBe("success");
    expect(g.statusById["s2.fail.0"]).toBe("skipped");
  });

  it("flags nested containers in arms and flips to the canvas", () => {
    const graph: WfStep[] = [
      {
        type: "branch",
        condition: { type: "job_status", jobRef: "x" },
        pass: [{ type: "parallel", jobs: [{ type: "job", name: "p1" }] }],
        fail: [],
      },
    ];
    const g = normalizeGraph(graph);
    expect(g.nested).toBe(true);
    expect(g.useCanvas).toBe(true);
    expect(g.def[0].pass?.steps?.[0]?.type).toBe("parallel");
    expect(g.statusById).toEqual({});
  });

  it("flips to the canvas past the node threshold even when flat", () => {
    const graph: WfStep[] = Array.from({ length: 11 }, (_, i) => ({ type: "job", name: `j${i}` }));
    expect(normalizeGraph(graph).useCanvas).toBe(true);
    expect(normalizeGraph(graph.slice(0, 10)).useCanvas).toBe(false);
  });

  it("falls back to a label-only condition without inventing a predicate", () => {
    const graph: WfStep[] = [
      { type: "branch", condition: { label: "custom check" }, pass: [], fail: [] },
    ];
    const cond = normalizeGraph(graph).def[0].condition;
    expect(cond?.type).toBe("label");
    expect(cond?.jobRef).toBe("custom check");
  });
});

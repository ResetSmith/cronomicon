import { describe, it, expect } from "vitest";
import { toCanvasTree, toSteps, type DefStep } from "./canvasModel";

// R2F-2 — a step may pin the job it means by identity, and the round trip must
// not invent or drop one.
//
// The preservation rule is the load-bearing half: opening a stored graph and
// saving it must leave every reference the operator did not touch exactly as it
// was. A silent mass-migration of name-only steps to uids on open+save would
// rewrite references the operator never reviewed — including, in a git-synced
// graph, ones that are name-only by law.

describe("step identity round-trip", () => {
  it("preserves a pinned step's uid", () => {
    const graph: DefStep[] = [{ type: "job", name: "deploy", jobUid: "uid-fin" }];
    expect(toSteps(toCanvasTree(graph))).toEqual(graph);
  });

  it("leaves a legacy name-only step name-only", () => {
    const graph: DefStep[] = [
      { type: "job", name: "build" },
      { type: "job", name: "deploy", jobSource: "git" },
    ];
    const out = toSteps(toCanvasTree(graph));
    expect(out).toEqual(graph);
    expect(out.every((s) => !("jobUid" in s))).toBe(true);
  });

  it("carries identity through nesting (parallel arms, sequences, branch arms)", () => {
    const graph: DefStep[] = [
      {
        type: "parallel",
        jobs: [
          { type: "job", name: "unit", jobUid: "uid-unit" },
          { type: "sequence", steps: [{ type: "job", name: "lint", jobUid: "uid-lint" }] },
        ],
      },
      {
        type: "branch",
        condition: { type: "job_status", jobRef: "unit" },
        pass: { steps: [{ type: "job", name: "deploy", jobUid: "uid-deploy" }] },
        fail: { steps: [{ type: "job", name: "notify" }] },
      },
    ];
    expect(toSteps(toCanvasTree(graph))).toEqual(graph);
  });

  it("keeps two same-named steps apart when they pin different jobs", () => {
    const graph: DefStep[] = [
      { type: "job", name: "deploy", jobUid: "uid-fin" },
      { type: "job", name: "deploy", jobUid: "uid-dss" },
    ];
    const out = toSteps(toCanvasTree(graph));
    expect(out.map((s) => s.jobUid)).toEqual(["uid-fin", "uid-dss"]);
  });
});

// Present defect 54 (2.4.2): a sub-workflow step's pinned identity survives an
// open-and-save. It was dropped, so the server re-resolved the child by name:
// refused when two agencies hold the name, or re-pinned to another workflow.
describe("a sub-workflow step keeps its workflowUid", () => {
  it("round-trips the pin", () => {
    const steps = [{ type: "workflow", name: "go", workflow: "nightly", workflowUid: "wf-uid-1" }];
    expect(toSteps(toCanvasTree(steps))).toEqual([{ type: "workflow", name: "go", workflow: "nightly", workflowUid: "wf-uid-1" }]);
  });
  it("emits none for a step that had none", () => {
    const steps = [{ type: "workflow", name: "go", workflow: "nightly" }];
    expect(toSteps(toCanvasTree(steps))[0]).not.toHaveProperty("workflowUid");
  });
});

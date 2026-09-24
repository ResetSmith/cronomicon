import { describe, it, expect } from "vitest";
import {
  toCanvasTree,
  toSteps,
  upstreamProducers,
  validateTree,
  findNode,
  conditionLabel,
  parseLayout,
  type DefStep,
} from "./canvasModel";

// Fixtures are written in *canonical emitted form* (exactly what toSteps produces),
// so `toSteps(toCanvasTree(fixture))` is an identity. The worked example mirrors
// the workflow-update-2b plan §4.4 / the WG §4.4 graph.

const flatChain: DefStep[] = [
  { type: "job", name: "build" },
  { type: "job", name: "test" },
  { type: "job", name: "deploy" },
];

const parallelWf: DefStep[] = [
  { type: "job", name: "build" },
  { type: "parallel", label: "tests", jobs: [{ type: "job", name: "unit" }, { type: "job", name: "integration" }] },
  { type: "job", name: "cleanup" },
];

// Worked example: build → ∥{unit,integration} → branch(unit){deploy|notify} → cleanup(ARTIFACT←build)
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

const outputMatchBranch: DefStep[] = [
  { type: "job", name: "build" },
  {
    type: "branch",
    condition: { type: "output_match", jobRef: "build", field: "rc", operator: "==", value: "0" },
    pass: { steps: [{ type: "job", name: "ship" }] },
    fail: { steps: [{ type: "job", name: "rollback" }] },
  },
];

// branch-in-branch (nested): only reachable via git YAML; the canvas must round-trip it.
const nestedBranch: DefStep[] = [
  { type: "job", name: "a" },
  {
    type: "branch",
    condition: { type: "job_status", jobRef: "a" },
    pass: {
      steps: [
        {
          type: "branch",
          condition: { type: "job_status", jobRef: "a" },
          pass: { steps: [{ type: "job", name: "p_inner" }] },
          fail: { steps: [{ type: "job", name: "f_inner" }] },
        },
      ],
    },
    fail: { steps: [{ type: "job", name: "b" }] },
  },
];

// parallel-in-branch: a parallel block inside a branch arm.
const parallelInBranch: DefStep[] = [
  { type: "job", name: "a" },
  {
    type: "branch",
    condition: { type: "job_status", jobRef: "a" },
    pass: { steps: [{ type: "parallel", jobs: [{ type: "job", name: "x" }, { type: "job", name: "y" }] }] },
    fail: { steps: [{ type: "job", name: "z" }] },
  },
];

const advancedJob: DefStep[] = [
  { type: "job", name: "build" },
  {
    type: "job",
    name: "deploy",
    label: "Ship it",
    jobSource: "git",
    inputs: { ARTIFACT: { fromStep: "build", fromOutput: "url" } },
    retries: 3,
    backoffSeconds: 10,
    continueOnError: true,
  },
];

const allFixtures: Array<[string, DefStep[]]> = [
  ["flat chain", flatChain],
  ["parallel", parallelWf],
  ["worked example (branch + A12)", workedExample],
  ["output_match branch", outputMatchBranch],
  ["nested branch-in-branch", nestedBranch],
  ["parallel-in-branch", parallelInBranch],
  ["advanced job (inputs/retries/jobSource/label)", advancedJob],
];

describe("canvasModel round-trip serializer", () => {
  it.each(allFixtures)("toSteps(toCanvasTree(%s)) is identity", (_label, steps) => {
    expect(toSteps(toCanvasTree(steps))).toEqual(steps);
  });

  it.each(allFixtures)("tree is stable across a round-trip (%s)", (_label, steps) => {
    const tree = toCanvasTree(steps);
    expect(toCanvasTree(toSteps(tree))).toEqual(tree);
  });

  it("drops unset fields and never emits id/status (job)", () => {
    const tree = toCanvasTree([{ type: "job", name: "x" }]);
    expect(toSteps(tree)).toEqual([{ type: "job", name: "x" }]);
  });

  it("emits job_status conditions without field/operator/value", () => {
    const out = toSteps(toCanvasTree(workedExample));
    expect(out[2].condition).toEqual({ type: "job_status", jobRef: "unit" });
  });

  it("assigns deterministic path-based ids", () => {
    const tree = toCanvasTree(workedExample);
    expect(tree.map((n) => n.id)).toEqual(["s0", "s1", "s2", "s3"]);
    expect(findNode(tree, "s1.j1")?.name).toBe("integration");
    expect(findNode(tree, "s2.pass.0")?.name).toBe("deploy");
    expect(findNode(tree, "s2.fail.0")?.name).toBe("notify");
  });
});

describe("validateTree — mirror of engine ValidateSteps", () => {
  it("accepts the worked example with no errors", () => {
    expect(validateTree(toCanvasTree(workedExample))).toEqual([]);
  });

  it.each(allFixtures)("accepts valid fixture %s", (_label, steps) => {
    expect(validateTree(toCanvasTree(steps))).toEqual([]);
  });

  it("flags a job with no name", () => {
    const errs = validateTree(toCanvasTree([{ type: "job", name: "" }]));
    expect(errs).toContainEqual({ nodeId: "s0", field: "name", message: "job step requires a name" });
  });

  it("flags an empty parallel block", () => {
    const errs = validateTree(toCanvasTree([{ type: "parallel", jobs: [] }]));
    expect(errs.some((e) => e.field === "jobs")).toBe(true);
  });

  it("flags a parallel arm that is neither a job nor a sequence (PS-1 rule)", () => {
    // Hand-build an invalid tree (a parallel whose arm is a branch).
    const tree = toCanvasTree([{ type: "parallel", jobs: [{ type: "job", name: "ok" }] }]);
    tree[0].jobs!.push({
      id: "s0.j1",
      kind: "branch",
      condition: { type: "job_status", jobRef: "ok", field: "", operator: "==", value: "" },
      pass: [],
      fail: [],
    });
    const errs = validateTree(tree);
    expect(errs).toContainEqual({ nodeId: "s0.j1", field: "type", message: "a parallel arm must be a plain job or a sequence" });
  });

  it("flags a branch missing its jobRef", () => {
    const errs = validateTree(
      toCanvasTree([
        { type: "branch", condition: { type: "job_status", jobRef: "" }, pass: { steps: [] }, fail: { steps: [] } },
      ]),
    );
    expect(errs).toContainEqual({ nodeId: "s0", field: "condition.jobRef", message: "condition requires a jobRef" });
  });

  it("flags an output_match branch with a bad operator and missing field", () => {
    const tree = toCanvasTree([
      { type: "branch", condition: { type: "output_match", jobRef: "a", field: "", operator: "~=", value: "1" }, pass: { steps: [] }, fail: { steps: [] } },
    ]);
    // Prepend a producer so jobRef resolution isn't the issue under test.
    const errs = validateTree(tree);
    expect(errs.some((e) => e.field === "condition.field")).toBe(true);
    expect(errs.some((e) => e.field === "condition.operator")).toBe(true);
  });

  it("flags an A12 input whose fromStep is not upstream (forward / sibling ref)", () => {
    // cleanup references "later" — a job that runs after it.
    const errs = validateTree(
      toCanvasTree([
        { type: "job", name: "cleanup", inputs: { X: { fromStep: "later", fromOutput: "o" } } },
        { type: "job", name: "later" },
      ]),
    );
    expect(errs).toContainEqual({ nodeId: "s0", field: "inputs.X", message: "input references unknown upstream step: later" });
  });

  it("flags an A12 input with a blank fromStep", () => {
    const errs = validateTree(toCanvasTree([{ type: "job", name: "a", inputs: { X: { fromStep: "", fromOutput: "" } } }]));
    expect(errs).toContainEqual({ nodeId: "s0", field: "inputs.X", message: "input must name an upstream step (fromStep)" });
  });

  it("flags negative retries", () => {
    const tree = toCanvasTree([{ type: "job", name: "a", retries: -1 }]);
    expect(validateTree(tree)).toContainEqual({ nodeId: "s0", field: "retries", message: "retries must be ≥ 0" });
  });

  it("flags a duplicate job name on one path", () => {
    const errs = validateTree(toCanvasTree([{ type: "job", name: "a" }, { type: "job", name: "a" }]));
    expect(errs).toContainEqual({ nodeId: "s1", field: "name", message: 'job name "a" appears more than once on one execution path' });
  });

  it("ALLOWS the same name once per mutually-exclusive branch arm", () => {
    const errs = validateTree(
      toCanvasTree([
        { type: "job", name: "a" },
        {
          type: "branch",
          condition: { type: "job_status", jobRef: "a" },
          pass: { steps: [{ type: "job", name: "shared" }] },
          fail: { steps: [{ type: "job", name: "shared" }] },
        },
      ]),
    );
    expect(errs).toEqual([]);
  });

  it("honors an optional job-existence check", () => {
    const known = new Set(["build"]);
    const errs = validateTree(toCanvasTree([{ type: "job", name: "ghost" }]), { jobExists: (n) => known.has(n) });
    expect(errs).toContainEqual({ nodeId: "s0", field: "name", message: "step references unknown job: ghost" });
  });
});

describe("upstreamProducers — scoped visibility (mirrors validateStepSeq)", () => {
  const tree = toCanvasTree(workedExample);

  it("a later step sees all earlier producers (incl. both branch arms)", () => {
    expect(new Set(upstreamProducers(tree, "s3"))).toEqual(new Set(["build", "unit", "integration", "deploy", "notify"]));
  });

  it("the branch condition sees only pre-branch producers", () => {
    expect(new Set(upstreamProducers(tree, "s2"))).toEqual(new Set(["build", "unit", "integration"]));
  });

  it("a parallel child does NOT see its sibling (only pre-block producers)", () => {
    expect(upstreamProducers(tree, "s1.j0")).toEqual(["build"]); // unit cannot see integration
    expect(upstreamProducers(tree, "s1.j1")).toEqual(["build"]); // integration cannot see unit
  });

  it("a first job sees nothing upstream", () => {
    expect(upstreamProducers(tree, "s0")).toEqual([]);
  });

  it("the fail arm additionally sees the pass arm's producers (engine merge order)", () => {
    const t = toCanvasTree([
      { type: "job", name: "a" },
      {
        type: "branch",
        condition: { type: "job_status", jobRef: "a" },
        pass: { steps: [{ type: "job", name: "p1" }] },
        fail: { steps: [{ type: "job", name: "f1" }] },
      },
    ]);
    // p1 (pass arm) sees only the pre-branch producer.
    expect(new Set(upstreamProducers(t, "s1.pass.0"))).toEqual(new Set(["a"]));
    // f1 (fail arm) sees the pre-branch producer AND the pass arm's producer.
    expect(new Set(upstreamProducers(t, "s1.fail.0"))).toEqual(new Set(["a", "p1"]));
  });

  it("returns [] for an unknown node id", () => {
    expect(upstreamProducers(tree, "nope")).toEqual([]);
  });
});

describe("conditionLabel", () => {
  it("renders job_status and output_match", () => {
    expect(conditionLabel({ type: "job_status", jobRef: "unit", field: "", operator: "==", value: "" })).toBe("unit succeeded");
    expect(conditionLabel({ type: "output_match", jobRef: "build", field: "rc", operator: "==", value: "0" })).toBe("build.rc == 0");
  });
});

// WC-P7: the advisory layout blob is server/user data, so parseLayout must be total —
// tolerating null, non-objects, and malformed entries without throwing, and keeping
// only finite {x,y} pairs (the keys are flow node ids; see canvasFlow).
describe("parseLayout", () => {
  it("keeps well-formed {x,y} entries", () => {
    expect(parseLayout({ s0: { x: 10, y: 20 }, "s1.fork": { x: -4, y: 0 } })).toEqual({
      s0: { x: 10, y: 20 },
      "s1.fork": { x: -4, y: 0 },
    });
  });

  it("coerces null / undefined / non-object input to {}", () => {
    expect(parseLayout(null)).toEqual({});
    expect(parseLayout(undefined)).toEqual({});
    expect(parseLayout("nope")).toEqual({});
    expect(parseLayout(42)).toEqual({});
  });

  it("drops malformed entries (missing/non-finite/non-number coords)", () => {
    expect(
      parseLayout({
        good: { x: 1, y: 2 },
        missingY: { x: 3 },
        nanX: { x: NaN, y: 5 },
        infY: { x: 5, y: Infinity },
        stringXY: { x: "1", y: "2" },
        nullEntry: null,
      }),
    ).toEqual({ good: { x: 1, y: 2 } });
  });
});

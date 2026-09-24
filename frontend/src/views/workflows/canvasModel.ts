// canvasModel.ts — WC-P1: the workflow-canvas authoring model and its round-trip
// serializer (the Option C correctness spine — the workflow-update-2b plan §8).
//
// The engine's workflow definition is a *nested, ordered, path-based* []Step tree
// (`backend/internal/workflow/engine.go`). This module is the pure, framework-free
// core that a React Flow canvas (WC-P2+) renders and edits:
//
//   • CanvasNode[]      — the editable, ordered nested tree (the source of truth;
//                         React Flow runs controlled, deriving its nodes from this —
//                         array order lives here, never in node x/y — WC12).
//   • DefStep[]         — the raw compose wire shape (`GET`/`POST`/`PUT /workflows`):
//                         structured `inputs`/`condition`, branch arms as `{steps}`.
//   • toCanvasTree()    — DefStep[] → CanvasNode[]  (arbitrary nesting; generalizes
//                         WorkflowEditor.toEditorStep, subsuming the one-level
//                         isEditableGraph limit — RT-2).
//   • toSteps()         — CanvasNode[] → DefStep[]  (the 12 definition fields,
//                         omit-when-unset; generalizes WorkflowEditor.emitStep).
//   • upstreamProducers — the legal A12 `fromStep` / branch `jobRef` candidates
//                         visible at a node, mirroring engine validateStepSeq scoping.
//   • validateTree()    — a path/tree-aware mirror of engine ValidateSteps +
//                         firstSamePathDuplicate, returning per-node errors.
//
// No React, no dagre, no network — fully unit-tested (canvasModel.test.ts). The
// serializer is the only thing standing between the canvas and a 422; it must stay
// byte-faithful to `workflow.Step` (engine.go:36-64) and never emit id/status/etc.

// ── 1 · Raw compose wire shape (the "definition-shaped" type — §5) ────────────────

export interface DefInputRef {
  fromStep: string;
  fromOutput: string;
}
export interface DefCondition {
  type: string; // job_status | output_match
  jobRef: string;
  field?: string;
  operator?: string;
  value?: string;
}
export interface DefBranch {
  steps: DefStep[];
}
/** One raw `workflow.Step` as served by `GET /workflows` and accepted by the compose
 *  endpoints. `id` (engine-minted NodeID) appears only on run snapshots and is never
 *  emitted by the canvas. */
export interface DefStep {
  type?: string; // job | parallel | branch | sequence | workflow (empty ⇒ job)
  name?: string;
  label?: string;
  jobSource?: string;
  jobUid?: string; // R2F-2 — the step's pinned job identity, when it has one
  inputs?: Record<string, DefInputRef>;
  jobs?: DefStep[]; // parallel: the concurrent arms (leaf jobs or sequences, PS-1)
  steps?: DefStep[]; // sequence: the ordered serial chain (PS-1)
  workflow?: string; // workflow: the sub-workflow this step runs (SW)
  pass?: DefBranch | null;
  fail?: DefBranch | null;
  condition?: DefCondition | null;
  retries?: number;
  backoffSeconds?: number;
  continueOnError?: boolean;
  id?: string;
}

// ── 2 · The canvas authoring tree (ordered, nested — the source of truth) ─────────

export type CanvasKind = "job" | "parallel" | "branch" | "sequence" | "workflow";

export interface CanvasInput {
  envKey: string;
  fromStep: string;
  fromOutput: string;
}
export interface CanvasCondition {
  type: string;
  jobRef: string;
  field: string;
  operator: string;
  value: string;
}
export interface CanvasNode {
  /** Stable client id (React Flow node id / inspector target). NOT the engine name,
   *  and never serialized — `toSteps` drops it. */
  id: string;
  kind: CanvasKind;
  label?: string;
  // job
  name?: string;
  jobSource?: string;
  jobUid?: string; // R2F-2 — see DefStep.jobUid
  inputs?: CanvasInput[];
  retries?: number | null; // null ⇒ unset (inherit job default)
  backoffSeconds?: number | null;
  continueOnError?: boolean | null;
  // parallel — the concurrent arms: leaf jobs or sequences (PS-1; validateTree enforces)
  jobs?: CanvasNode[];
  // sequence — an ordered serial chain (PS-1); primarily used as a parallel arm
  steps?: CanvasNode[];
  // workflow — the sub-workflow this step runs (SW). Distinct from `name`, which
  // is the step's position in the parent's results map.
  workflow?: string;
  // branch
  condition?: CanvasCondition;
  pass?: CanvasNode[]; // ordered sub-sequence (may nest)
  fail?: CanvasNode[];
}

const normKind = (t?: string): CanvasKind =>
  t === "parallel" || t === "branch" || t === "sequence" || t === "workflow" ? t : "job";

// ── 3 · DefStep[] → CanvasNode[] (parse; handles arbitrary nesting) ───────────────

/** Parse the raw compose `steps` into the editable canvas tree. Path-based,
 *  deterministic node ids (`s0`, `s1.j0`, `s2.pass.0`, …) so the result is stable
 *  for a given input; the editor may re-id on mutation (WC-P3). */
export function toCanvasTree(steps: DefStep[]): CanvasNode[] {
  return steps.map((s, i) => parseNode(s, `s${i}`));
}

function parseNode(s: DefStep, id: string): CanvasNode {
  const kind = normKind(s.type);
  if (kind === "parallel") {
    return { id, kind, label: s.label, jobs: (s.jobs ?? []).map((j, k) => parseNode(j, `${id}.j${k}`)) };
  }
  if (kind === "sequence") {
    return { id, kind, label: s.label, steps: (s.steps ?? []).map((ss, k) => parseNode(ss, `${id}.q${k}`)) };
  }
  if (kind === "workflow") {
    return {
      id,
      kind,
      name: s.name ?? "",
      workflow: s.workflow ?? "",
      label: s.label,
      inputs: Object.entries(s.inputs ?? {}).map(([envKey, ref]) => ({
        envKey,
        fromStep: ref?.fromStep ?? "",
        fromOutput: ref?.fromOutput ?? "",
      })),
      retries: s.retries ?? null,
      backoffSeconds: s.backoffSeconds ?? null,
      continueOnError: s.continueOnError ?? null,
    };
  }
  if (kind === "branch") {
    const c = s.condition ?? { type: "job_status", jobRef: "" };
    return {
      id,
      kind,
      label: s.label,
      condition: {
        type: c.type || "job_status",
        jobRef: c.jobRef ?? "",
        field: c.field ?? "",
        operator: c.operator ?? "==",
        value: c.value ?? "",
      },
      pass: (s.pass?.steps ?? []).map((p, k) => parseNode(p, `${id}.pass.${k}`)),
      fail: (s.fail?.steps ?? []).map((p, k) => parseNode(p, `${id}.fail.${k}`)),
    };
  }
  return {
    id,
    kind: "job",
    name: s.name ?? "",
    jobSource: s.jobSource,
    jobUid: s.jobUid,
    label: s.label,
    inputs: Object.entries(s.inputs ?? {}).map(([envKey, ref]) => ({
      envKey,
      fromStep: ref?.fromStep ?? "",
      fromOutput: ref?.fromOutput ?? "",
    })),
    retries: s.retries ?? null,
    backoffSeconds: s.backoffSeconds ?? null,
    continueOnError: s.continueOnError ?? null,
  };
}

// ── 4 · CanvasNode[] → DefStep[] (serialize; the 12 definition fields) ────────────

/** Serialize the canvas tree to compose `steps` — exactly the engine `workflow.Step`
 *  definition fields, omitting unset ones (mirrors WorkflowEditor.emitStep, lifted to
 *  arbitrary nesting). Never emits id/status/durationMs/outputs. */
export function toSteps(tree: CanvasNode[]): DefStep[] {
  return tree.map(emit);
}

function emit(n: CanvasNode): DefStep {
  if (n.kind === "parallel") {
    return { type: "parallel", ...(n.label?.trim() ? { label: n.label.trim() } : {}), jobs: (n.jobs ?? []).map(emit) };
  }
  if (n.kind === "sequence") {
    return { type: "sequence", ...(n.label?.trim() ? { label: n.label.trim() } : {}), steps: (n.steps ?? []).map(emit) };
  }
  if (n.kind === "workflow") {
    const wfInputs: Record<string, DefInputRef> = {};
    for (const r of n.inputs ?? []) {
      const key = r.envKey.trim();
      if (key && r.fromStep) wfInputs[key] = { fromStep: r.fromStep, fromOutput: r.fromOutput.trim() };
    }
    return {
      type: "workflow",
      name: n.name ?? "",
      workflow: n.workflow ?? "",
      ...(n.label?.trim() ? { label: n.label.trim() } : {}),
      ...(Object.keys(wfInputs).length ? { inputs: wfInputs } : {}),
      ...(typeof n.retries === "number" ? { retries: n.retries } : {}),
      ...(typeof n.backoffSeconds === "number" ? { backoffSeconds: n.backoffSeconds } : {}),
      ...(typeof n.continueOnError === "boolean" ? { continueOnError: n.continueOnError } : {}),
    };
  }
  if (n.kind === "branch") {
    const c = n.condition ?? { type: "job_status", jobRef: "", field: "", operator: "==", value: "" };
    return {
      type: "branch",
      ...(n.label?.trim() ? { label: n.label.trim() } : {}),
      condition: {
        type: c.type,
        jobRef: c.jobRef,
        ...(c.type === "output_match" ? { field: c.field, operator: c.operator, value: c.value } : {}),
      },
      pass: { steps: (n.pass ?? []).map(emit) },
      fail: { steps: (n.fail ?? []).map(emit) },
    };
  }
  // job
  const inputs: Record<string, DefInputRef> = {};
  for (const r of n.inputs ?? []) {
    const key = r.envKey.trim();
    if (key && r.fromStep) inputs[key] = { fromStep: r.fromStep, fromOutput: r.fromOutput.trim() };
  }
  return {
    type: "job",
    name: n.name ?? "",
    ...(n.jobSource ? { jobSource: n.jobSource } : {}),
    // R2F-2: emitted only when the node CARRIES one — a legacy name-only step
    // survives an open-and-save untouched.
    ...(n.jobUid ? { jobUid: n.jobUid } : {}),
    ...(n.label?.trim() ? { label: n.label.trim() } : {}),
    ...(Object.keys(inputs).length ? { inputs } : {}),
    ...(typeof n.retries === "number" ? { retries: n.retries } : {}),
    ...(typeof n.backoffSeconds === "number" ? { backoffSeconds: n.backoffSeconds } : {}),
    ...(typeof n.continueOnError === "boolean" ? { continueOnError: n.continueOnError } : {}),
  };
}

// ── 5 · Scoped-upstream walk (legal A12 fromStep / branch jobRef candidates) ──────

/** All producer names a branch arm contributes downstream — recursively, both nested
 *  arms — matching the engine's `mergeBoolSet(upstream, armUp)` after each arm. */
function armProducers(seq: CanvasNode[]): string[] {
  const out: string[] = [];
  for (const node of seq) {
    if (node.kind === "job") {
      if (node.name) out.push(node.name);
    } else if (node.kind === "workflow") {
      // SW: a sub-workflow step produces a result under its own name, exactly
      // like a job step — that is what makes {fromStep} work across the boundary.
      if (node.name) out.push(node.name);
    } else if (node.kind === "parallel") {
      for (const j of node.jobs ?? []) {
        if (j.kind === "sequence") out.push(...armProducers(j.steps ?? []));
        else if (j.name) out.push(j.name);
      }
    } else if (node.kind === "sequence") {
      out.push(...armProducers(node.steps ?? []));
    } else {
      out.push(...armProducers(node.pass ?? []), ...armProducers(node.fail ?? []));
    }
  }
  return out;
}

/** The set of producer names visible upstream of node `targetId`, mirroring the
 *  engine's validateStepSeq scoping exactly (engine.go:1042-1114): parallel siblings
 *  are invisible to each other; a branch's condition + arms see the pre-branch
 *  producers; and — matching the engine — the fail arm additionally sees the pass
 *  arm's producers (pass is merged into `upstream` before the fail copy). Returns []
 *  if `targetId` is not in the tree. */
export function upstreamProducers(tree: CanvasNode[], targetId: string): string[] {
  const hit = visibleUpstream(tree, new Set<string>(), targetId);
  return hit ? [...hit].filter(Boolean) : [];
}

function visibleUpstream(seq: CanvasNode[], upstreamIn: ReadonlySet<string>, targetId: string): Set<string> | null {
  const upstream = new Set(upstreamIn);
  for (const node of seq) {
    if (node.id === targetId) return new Set(upstream);
    if (node.kind === "job") {
      if (node.name) upstream.add(node.name);
    } else if (node.kind === "parallel") {
      // Siblings see the pre-block upstream; their names enter only after the
      // block. A sequence arm's own steps see earlier steps of the SAME arm
      // (serial), so each arm recurses from the pre-block set (PS-1).
      for (const child of node.jobs ?? []) {
        if (child.id === targetId) return new Set(upstream);
        if (child.kind === "sequence") {
          const hit = visibleUpstream(child.steps ?? [], upstream, targetId);
          if (hit) return hit;
        }
      }
      for (const child of node.jobs ?? []) {
        if (child.kind === "sequence") for (const n of armProducers(child.steps ?? [])) upstream.add(n);
        else if (child.name) upstream.add(child.name);
      }
    } else if (node.kind === "sequence") {
      // Inline serial chain: walked in place, exactly like steps at this level.
      const hit = visibleUpstream(node.steps ?? [], upstream, targetId);
      if (hit) return hit;
      for (const n of armProducers(node.steps ?? [])) upstream.add(n);
    } else {
      const passHit = visibleUpstream(node.pass ?? [], upstream, targetId);
      if (passHit) return passHit;
      for (const n of armProducers(node.pass ?? [])) upstream.add(n);
      const failHit = visibleUpstream(node.fail ?? [], upstream, targetId);
      if (failHit) return failHit;
      for (const n of armProducers(node.fail ?? [])) upstream.add(n);
    }
  }
  return null;
}

// ── 6 · Validation oracle — a path/tree-aware mirror of engine ValidateSteps ──────

export interface CanvasError {
  nodeId: string;
  field: string;
  message: string;
}
export interface ValidateOpts {
  /** Optional job-existence check (server-side in the engine via the jobs table,
   *  workflow_compose_mount.go:261-272). When provided, a job node whose name is
   *  unknown yields an error. */
  jobExists?: (name: string) => boolean;
  /** Optional sub-workflow existence check (SW). Mirrors validateWorkflowRefs on
   *  the server; the server is still authoritative — this only saves a round-trip
   *  and lands the error on the right node. */
  workflowExists?: (name: string) => boolean;
  /** The workflow being edited, so a self-reference is caught client-side too. */
  selfName?: string;
}

const push = (errs: CanvasError[], nodeId: string, field: string, message: string) => errs.push({ nodeId, field, message });

/** Mirror of `workflow.ValidateSteps` + the same-path duplicate-name guard
 *  (firstSamePathDuplicate), keyed by canvas node id so errors land on the right
 *  node. Job-existence is checked only when `opts.jobExists` is supplied. */
export function validateTree(tree: CanvasNode[], opts: ValidateOpts = {}): CanvasError[] {
  const errs: CanvasError[] = [];
  validateSeq(tree, new Set<string>(), errs, opts);
  const dup = firstDuplicateName(tree, new Set<string>());
  if (dup) push(errs, dup.nodeId, "name", `job name "${dup.name}" appears more than once on one execution path`);
  return errs;
}

function validateSeq(seq: CanvasNode[], upstream: Set<string>, errs: CanvasError[], opts: ValidateOpts): void {
  for (const node of seq) {
    if (node.kind === "job") {
      if (!node.name) push(errs, node.id, "name", "job step requires a name");
      else if (opts.jobExists && !opts.jobExists(node.name)) push(errs, node.id, "name", `step references unknown job: ${node.name}`);
      validateInputs(node, upstream, errs);
      validateRetry(node, errs);
      if (node.name) upstream.add(node.name);
    } else if (node.kind === "workflow") {
      // SW — mirrors the server's validateWorkflowRefs. The server stays
      // authoritative (it can also see cycles through workflows this editor has
      // never loaded); this only saves a round-trip and lands the error on the
      // right node.
      if (!node.name) push(errs, node.id, "name", "workflow step requires a name");
      if (!node.workflow) push(errs, node.id, "workflow", "workflow step requires a workflow to run");
      else if (opts.selfName && node.workflow === opts.selfName)
        push(errs, node.id, "workflow", `a workflow cannot run itself: ${node.workflow}`);
      else if (opts.workflowExists && !opts.workflowExists(node.workflow))
        push(errs, node.id, "workflow", `step references unknown workflow: ${node.workflow}`);
      validateInputs(node, upstream, errs);
      validateRetry(node, errs);
      if (node.name) upstream.add(node.name);
    } else if (node.kind === "parallel") {
      if ((node.jobs ?? []).length === 0) push(errs, node.id, "jobs", "parallel step requires at least one arm");
      // Mirror engine validateStepSeq (PS-1): an arm is a plain job or a
      // sequence; a sequence arm validates against its own copy of the
      // pre-block upstream, and every arm's producers merge in only after.
      const produced: string[] = [];
      for (const j of node.jobs ?? []) {
        if (j.kind === "sequence") {
          if ((j.steps ?? []).length === 0) push(errs, j.id, "steps", "sequence arm requires at least one step");
          const armUp = new Set(upstream);
          validateSeq(j.steps ?? [], armUp, errs, opts);
          for (const n of armUp) if (!upstream.has(n)) produced.push(n);
          continue;
        }
        if (j.kind !== "job") push(errs, j.id, "type", "a parallel arm must be a plain job or a sequence");
        if (!j.name) push(errs, j.id, "name", "parallel job requires a name");
        else if (opts.jobExists && !opts.jobExists(j.name)) push(errs, j.id, "name", `step references unknown job: ${j.name}`);
        validateInputs(j, upstream, errs);
        validateRetry(j, errs);
        if (j.name) produced.push(j.name);
      }
      for (const n of produced) upstream.add(n);
    } else if (node.kind === "sequence") {
      if ((node.steps ?? []).length === 0) push(errs, node.id, "steps", "sequence step requires at least one step");
      validateSeq(node.steps ?? [], upstream, errs, opts);
    } else {
      validateBranch(node, errs);
      const passUp = new Set(upstream);
      validateSeq(node.pass ?? [], passUp, errs, opts);
      for (const n of passUp) upstream.add(n);
      const failUp = new Set(upstream);
      validateSeq(node.fail ?? [], failUp, errs, opts);
      for (const n of failUp) upstream.add(n);
    }
  }
}

function validateBranch(node: CanvasNode, errs: CanvasError[]): void {
  const c = node.condition;
  if (!c) {
    push(errs, node.id, "condition", "branch step requires a condition");
  } else {
    if (c.type === "job_status") {
      // jobRef required (checked below); no field/operator/value.
    } else if (c.type === "output_match") {
      if (!c.field) push(errs, node.id, "condition.field", "output_match condition requires a field");
      if (!["==", "!=", "contains"].includes(c.operator)) push(errs, node.id, "condition.operator", "output_match operator must be one of ==, !=, contains");
    } else if (!c.type) {
      push(errs, node.id, "condition.type", "condition requires a type (job_status|output_match)");
    } else {
      push(errs, node.id, "condition.type", `unknown condition type "${c.type}" (want job_status|output_match)`);
    }
    if (!c.jobRef) push(errs, node.id, "condition.jobRef", "condition requires a jobRef");
  }
  // The engine guards against a missing arm pointer; the canvas always builds both
  // arms (an empty arm — `[]` — is valid: "if fail, nothing runs → continue").
  if (node.pass == null) push(errs, node.id, "pass", "branch step requires a pass arm");
  if (node.fail == null) push(errs, node.id, "fail", "branch step requires a fail arm");
}

function validateInputs(node: CanvasNode, upstream: ReadonlySet<string>, errs: CanvasError[]): void {
  for (const r of node.inputs ?? []) {
    const key = r.envKey.trim();
    if (!key) continue; // blank row — not emitted; the UI surfaces incompleteness separately
    if (!r.fromStep) {
      push(errs, node.id, `inputs.${key}`, "input must name an upstream step (fromStep)");
      continue;
    }
    if (!upstream.has(r.fromStep)) push(errs, node.id, `inputs.${key}`, `input references unknown upstream step: ${r.fromStep}`);
  }
}

function validateRetry(node: CanvasNode, errs: CanvasError[]): void {
  if (typeof node.retries === "number" && node.retries < 0) push(errs, node.id, "retries", "retries must be ≥ 0");
  if (typeof node.backoffSeconds === "number" && node.backoffSeconds < 0) push(errs, node.id, "backoffSeconds", "backoffSeconds must be ≥ 0");
}

/** First job name that repeats on a single execution path (engine
 *  firstSamePathDuplicate, workflow_compose_mount.go:281-318). A branch's Pass and
 *  Fail arms are mutually exclusive, so each is walked with its own copy of `seen`;
 *  a name may appear once per arm. */
function firstDuplicateName(seq: CanvasNode[], seen: Set<string>): { nodeId: string; name: string } | null {
  for (const node of seq) {
    if (node.kind === "job") {
      if (node.name && seen.has(node.name)) return { nodeId: node.id, name: node.name };
      if (node.name) seen.add(node.name);
    } else if (node.kind === "parallel") {
      // PS-1: every arm runs, so all arms share this path's seen-set; a
      // sequence arm recurses its chain into the same set.
      for (const j of node.jobs ?? []) {
        if (j.kind === "sequence") {
          const d = firstDuplicateName(j.steps ?? [], seen);
          if (d) return d;
          continue;
        }
        if (j.name && seen.has(j.name)) return { nodeId: j.id, name: j.name };
        if (j.name) seen.add(j.name);
      }
    } else if (node.kind === "sequence") {
      const d = firstDuplicateName(node.steps ?? [], seen);
      if (d) return d;
    } else {
      const p = firstDuplicateName(node.pass ?? [], new Set(seen));
      if (p) return p;
      const f = firstDuplicateName(node.fail ?? [], new Set(seen));
      if (f) return f;
    }
  }
  return null;
}

// ── 7 · Small helpers reused by the canvas UI (WC-P2/P3) ──────────────────────────

/** Human-readable branch condition label (mirrors WorkflowEditor.conditionLabel). */
export function conditionLabel(c?: CanvasCondition): string {
  if (!c) return "condition";
  if (c.type === "output_match") return `${c.jobRef || "?"}.${c.field || "?"} ${c.operator} ${c.value}`.trim();
  return `${c.jobRef || "?"} succeeded`;
}

/** Depth-first lookup of a node by its id (job nodes, parallel children, and arm
 *  steps all included). */
export function findNode(tree: CanvasNode[], id: string): CanvasNode | null {
  for (const node of tree) {
    if (node.id === id) return node;
    const child =
      node.kind === "parallel"
        ? findNode(node.jobs ?? [], id)
        : node.kind === "sequence"
          ? findNode(node.steps ?? [], id)
          : node.kind === "branch"
            ? findNode(node.pass ?? [], id) ?? findNode(node.fail ?? [], id)
            : null;
    if (child) return child;
  }
  return null;
}

// ── 8 · Advisory canvas layout (WC-P7) ────────────────────────────────────────────

/** One hand-arranged node position (React Flow top-left coords). */
export interface XY {
  x: number;
  y: number;
}
/** Persisted node-position overrides, keyed by the *flow* node id (deterministic from
 *  the serialized steps — see canvasFlow.treeToFlow). Advisory only: it never affects
 *  step order or steps_hash. Absent ids fall back to dagre auto-layout. Serves as both
 *  the `workflows.layout_json` payload and the in-editor drag state. */
export type LayoutMap = Record<string, XY>;

/** Coerce a persisted/loaded layout blob (the API `Workflow.layout` field, which may
 *  be null/undefined/partly malformed) into a clean LayoutMap — dropping any entry
 *  whose x/y is not a finite number. Never throws. */
export function parseLayout(raw: unknown): LayoutMap {
  const out: LayoutMap = {};
  if (!raw || typeof raw !== "object") return out;
  for (const [id, v] of Object.entries(raw as Record<string, unknown>)) {
    if (v && typeof v === "object") {
      const { x, y } = v as { x?: unknown; y?: unknown };
      if (typeof x === "number" && Number.isFinite(x) && typeof y === "number" && Number.isFinite(y)) {
        out[id] = { x, y };
      }
    }
  }
  return out;
}

// WorkflowCanvasEditor.tsx — WC-P3: a structured, nestable editor over the canvas
// authoring tree (CanvasNode[]). Controlled (value/onChange); every edit goes through
// the pure, tested canvasEdit mutations. Generalizes the linear WorkflowEditor's
// StepCard list to ARBITRARY NESTING (branch-in-branch, parallel-in-branch — the
// graphs the linear editor punts to a read-only screen, RT-2), and gives every node a
// full inspector (label, jobSource, retries/backoff/continueOnError, A12 inputs —
// closing the JobLeaf fidelity gap, RT-1). The "from step" / branch "jobRef" pickers
// are driven by the engine-faithful scoped-upstream walk (canvasModel). A live,
// read-only WorkflowCanvas + validateTree mirror sit below. Editing only — save wires
// through the host editor's compose path. Theme-reactive: c.* read at render.
import { useState, type CSSProperties } from "react";
import { c } from "../../theme";
import { ExpandChevron } from "../../components/ui";
import {
  toSteps,
  validateTree,
  upstreamProducers,
  findNode,
  type CanvasNode,
  type CanvasKind,
  type LayoutMap,
} from "./canvasModel";
import {
  type LaneRef,
  appendNode,
  removeNode,
  updateNode,
  moveNode,
  newJobNode,
  newSequenceNode,
  newParallelNode,
  newBranchNode,
  newWorkflowNode,
  defaultInputKey,
} from "./canvasEdit";
import { WorkflowCanvas } from "./WorkflowCanvas";
import { ambiguousNames, disambiguate, type NamedRef } from "../../utils/disambiguate";

export interface JobOption {
  name: string;
  source?: string;
  uid?: string;      // R2F-2 — the identity a picked option pins
  agencies?: string[]; // AF-1 derived agencies, for disambiguating a shared name
}

// ── styles (functions so c.* is read at render — never a frozen module const) ───────
const inp = (): CSSProperties => ({ padding: "4px 7px", border: `1px solid ${c.borderStrong}`, borderRadius: c.radiusChip, background: c.panel2, color: c.text, fontSize: c.fontSm, fontFamily: "inherit" });
const inpNum = (): CSSProperties => ({ ...inp(), width: 64 });
const addBtn = (): CSSProperties => ({ padding: "3px 9px", border: `1px dashed ${c.border}`, borderRadius: c.radiusChip, background: "transparent", color: c.textSec, fontSize: c.fontXs, fontWeight: 600, cursor: "pointer" });
const iconBtn = (): CSSProperties => ({ width: 22, height: 22, border: `1px solid ${c.border}`, borderRadius: c.radiusChip, background: c.panel2, color: c.textSec, fontSize: c.fontXs, cursor: "pointer", lineHeight: 1, padding: 0 });
const KIND_COLOR = (kind: CanvasKind): string =>
  kind === "parallel"
    ? c.accent
    : kind === "branch"
      ? c.warning
      : kind === "sequence"
        ? c.success
        : kind === "workflow"
          ? c.info
          : c.primary;

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label style={{ display: "flex", alignItems: "center", gap: 8, fontSize: c.fontXs, color: c.textSec }}>
      <span style={{ width: 96, flexShrink: 0 }}>{label}</span>
      {children}
    </label>
  );
}

// R2F-2 — one option per JOB, valued by identity. R2-5 already stopped folding
// two same-named jobs into one option, but the value was still the bare name, so
// the two options were indistinguishable AND the step it wrote resolved
// ambiguously (the engine refused it fail-closed at run time — correct, but it
// made a legal catalog state unbuildable). Now picking says which job, and a
// duplicated name carries its agency so the operator can tell them apart.
const NAME_VALUE = "name:";

function JobPicker({
  name,
  jobUid,
  jobs,
  onPick,
}: {
  name: string;
  jobUid?: string;
  jobs: JobOption[];
  onPick: (pick: { name: string; jobSource?: string; jobUid?: string }) => void;
}) {
  const dupes = ambiguousNames(jobs as NamedRef[]);
  // The node's current selection. A pinned node is its uid; a legacy name-only
  // node displays against the job its name resolves to when that is unambiguous,
  // WITHOUT being rewritten — a save still emits it name-only.
  const matches = jobs.filter((j) => j.name === name);
  const value = jobUid || (matches.length === 1 && matches[0].uid ? matches[0].uid : name ? `${NAME_VALUE}${name}` : "");
  const unlisted = value.startsWith(NAME_VALUE);
  return (
    <select
      value={value}
      onChange={(e) => {
        const v = e.target.value;
        if (v.startsWith(NAME_VALUE)) return onPick({ name: v.slice(NAME_VALUE.length) });
        const j = jobs.find((x) => x.uid === v);
        onPick(j ? { name: j.name, jobSource: j.source, jobUid: j.uid } : { name: "" });
      }}
      style={{ ...inp(), flex: 1 }}
    >
      <option value="">— select a job —</option>
      {unlisted && <option value={value}>{name} (by name)</option>}
      {jobs.map((j) => (
        <option key={j.uid || `${j.source}:${j.name}`} value={j.uid || `${NAME_VALUE}${j.name}`}>
          {disambiguate(j as NamedRef, dupes)}
        </option>
      ))}
    </select>
  );
}

function JobInspector({ node, tree, set }: { node: CanvasNode; tree: CanvasNode[]; set: (p: Partial<CanvasNode>) => void }) {
  const upstream = upstreamProducers(tree, node.id);
  const inputs = node.inputs ?? [];
  const setInputs = (next: typeof inputs) => set({ inputs: next });
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6, marginTop: 6, paddingTop: 6, borderTop: `1px solid ${c.border}` }}>
      <Row label="Label">
        <input value={node.label ?? ""} placeholder="(optional)" onChange={(e) => set({ label: e.target.value })} style={{ ...inp(), flex: 1 }} />
      </Row>
      <Row label="Source (A11)">
        <select value={node.jobSource ?? ""} onChange={(e) => set({ jobSource: e.target.value || undefined })} style={inp()}>
          <option value="">auto</option>
          <option value="git">git</option>
          <option value="cronomicon">cronomicon</option>
        </select>
      </Row>
      <Row label="Retries">
        <input type="number" min={0} value={node.retries ?? ""} onChange={(e) => set({ retries: e.target.value === "" ? null : Math.max(0, Number(e.target.value)) })} style={inpNum()} />
      </Row>
      <Row label="Backoff (s)">
        <input type="number" min={0} value={node.backoffSeconds ?? ""} onChange={(e) => set({ backoffSeconds: e.target.value === "" ? null : Math.max(0, Number(e.target.value)) })} style={inpNum()} />
      </Row>
      <Row label="On error">
        <select value={node.continueOnError == null ? "" : String(node.continueOnError)} onChange={(e) => set({ continueOnError: e.target.value === "" ? null : e.target.value === "true" })} style={inp()}>
          <option value="">halt (default)</option>
          <option value="true">continue</option>
          <option value="false">halt</option>
        </select>
      </Row>
      <div>
        <div style={{ fontSize: c.fontXs, color: c.textSec, fontWeight: 600, marginBottom: 3 }}>Inputs (A12 — env ← upstream output)</div>
        {inputs.map((r, i) => (
          <div key={i} style={{ display: "flex", gap: 4, alignItems: "center", marginBottom: 3 }}>
            <input placeholder="ENV_KEY" value={r.envKey} onChange={(e) => setInputs(inputs.map((x, k) => (k === i ? { ...x, envKey: e.target.value } : x)))} style={{ ...inp(), width: 110 }} />
            <span style={{ color: c.textSec }}>←</span>
            <select value={r.fromStep} onChange={(e) => setInputs(inputs.map((x, k) => (k === i ? { ...x, fromStep: e.target.value } : x)))} style={inp()}>
              <option value="">— from step —</option>
              {upstream.map((u) => (
                <option key={u} value={u}>
                  {u}
                </option>
              ))}
            </select>
            <input placeholder="output" value={r.fromOutput} onChange={(e) => setInputs(inputs.map((x, k) => (k === i ? { ...x, fromOutput: e.target.value } : x)))} style={{ ...inp(), width: 90 }} />
            <button style={iconBtn()} onClick={() => setInputs(inputs.filter((_, k) => k !== i))} title="Remove input" aria-label="Remove input">
              ✕
            </button>
          </div>
        ))}
        <button style={addBtn()} disabled={upstream.length === 0} onClick={() => setInputs([...inputs, { envKey: "", fromStep: "", fromOutput: "" }])}>
          + input
        </button>
      </div>
    </div>
  );
}

function ConditionEditor({ node, tree, set }: { node: CanvasNode; tree: CanvasNode[]; set: (p: Partial<CanvasNode>) => void }) {
  const cond = node.condition ?? { type: "job_status", jobRef: "", field: "", operator: "==", value: "" };
  const upstream = upstreamProducers(tree, node.id);
  const setCond = (patch: Partial<typeof cond>) => set({ condition: { ...cond, ...patch } });
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      <Row label="If job">
        <select value={cond.jobRef} onChange={(e) => setCond({ jobRef: e.target.value })} style={{ ...inp(), flex: 1 }}>
          <option value="">— job —</option>
          {upstream.map((u) => (
            <option key={u} value={u}>
              {u}
            </option>
          ))}
        </select>
      </Row>
      <Row label="Check">
        <select value={cond.type} onChange={(e) => setCond({ type: e.target.value })} style={inp()}>
          <option value="job_status">succeeded</option>
          <option value="output_match">output matches…</option>
        </select>
      </Row>
      {cond.type === "output_match" && (
        <Row label="Output">
          <input placeholder="field" value={cond.field} onChange={(e) => setCond({ field: e.target.value })} style={{ ...inp(), width: 90 }} />
          <select value={cond.operator} onChange={(e) => setCond({ operator: e.target.value })} style={inp()}>
            <option value="==">==</option>
            <option value="!=">!=</option>
            <option value="contains">contains</option>
          </select>
          <input placeholder="value" value={cond.value} onChange={(e) => setCond({ value: e.target.value })} style={{ ...inp(), width: 90 }} />
        </Row>
      )}
    </div>
  );
}

function NodeRow({
  node,
  index,
  count,
  tree,
  onChange,
  jobs,
  errByNode,
}: {
  node: CanvasNode;
  index: number;
  count: number;
  tree: CanvasNode[];
  onChange: (t: CanvasNode[]) => void;
  jobs: JobOption[];
  errByNode: Map<string, string[]>;
}) {
  const [open, setOpen] = useState(false);
  const errs = errByNode.get(node.id) ?? [];
  const set = (patch: Partial<CanvasNode>) => onChange(updateNode(tree, node.id, patch));
  const clr = KIND_COLOR(node.kind);
  return (
    // The border stays here (unlike the linear editor's StepCard): lanes nest
    // recursively, so the box IS the depth cue for a branch arm inside a parallel
    // group, and this row is not sitting inside another bordered surface (VU-5).
    <div style={{ border: `1px solid ${errs.length ? c.danger : c.border}`, borderLeft: `3px solid ${clr}`, borderRadius: c.radiusSurface, background: c.panel, padding: "7px 9px", display: "flex", flexDirection: "column", gap: 6 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
        <span style={{ fontSize: c.fontXs, fontWeight: 700, color: clr, textTransform: "uppercase", letterSpacing: 0.4, width: 56, flexShrink: 0 }}>{node.kind}</span>
        {node.kind === "job" ? (
          <JobPicker name={node.name ?? ""} jobUid={node.jobUid} jobs={jobs} onPick={(pick) => set(pick)} />
        ) : (
          <span style={{ flex: 1, fontSize: c.fontSm, color: c.textSec }}>
            {node.kind === "parallel"
              ? `∥ ${node.label || "parallel group"}`
              : node.kind === "sequence"
                ? `⇣ ${node.label || "sequence"}`
                : node.kind === "workflow"
                  ? `⧉ ${node.workflow || "(pick a workflow)"}`
                  : `◆ branch`}
          </span>
        )}
        <button style={iconBtn()} disabled={index === 0} onClick={() => onChange(moveNode(tree, node.id, -1))} title="Move up" aria-label={`Move ${node.name || node.kind} up`}>↑</button>
        <button style={iconBtn()} disabled={index === count - 1} onClick={() => onChange(moveNode(tree, node.id, 1))} title="Move down" aria-label={`Move ${node.name || node.kind} down`}>↓</button>
        {node.kind === "job" && (
          <button style={iconBtn()} onClick={() => setOpen((o) => !o)} title="Advanced" aria-label={`${open ? "Hide" : "Show"} advanced fields for ${node.name || "job"}`} aria-expanded={open}><ExpandChevron open={open} /></button>
        )}
        <button style={iconBtn()} onClick={() => onChange(removeNode(tree, node.id))} title="Delete step" aria-label={`Delete ${node.name || node.kind} step`}>✕</button>
      </div>

      {errs.length > 0 && <div style={{ fontSize: c.fontXs, color: c.danger }}>{errs.join(" · ")}</div>}

      {node.kind === "job" && open && <JobInspector node={node} tree={tree} set={set} />}

      {node.kind === "parallel" && (
        <div>
          <input value={node.label ?? ""} placeholder="parallel label (optional)" onChange={(e) => set({ label: e.target.value })} style={{ ...inp(), width: "100%", boxSizing: "border-box", marginBottom: 6 }} />
          {/* PS-1: an arm is a plain job or a sequence (a serial chain running
              concurrently with its siblings) — the arm palette offers exactly those. */}
          <Lane nodes={node.jobs ?? []} lane={{ containerId: node.id, which: "jobs" }} tree={tree} onChange={onChange} jobs={jobs} errByNode={errByNode} armLane />
        </div>
      )}

      {node.kind === "workflow" && (
        <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
          <input
            value={node.name ?? ""}
            placeholder="step name (how later steps refer to this one)"
            onChange={(e) => set({ name: e.target.value })}
            style={{ ...inp(), width: "100%", boxSizing: "border-box" }}
          />
          <input
            value={node.workflow ?? ""}
            placeholder="workflow to run"
            onChange={(e) => set({ workflow: e.target.value })}
            style={{ ...inp(), width: "100%", boxSizing: "border-box" }}
          />
          <div style={{ fontSize: c.fontXs, color: c.textSec }}>
            Runs another workflow as one step and waits for it. Only the inputs you map cross over — this
            workflow's variables do not flow in automatically. Nesting is capped at 3 deep.
          </div>
        </div>
      )}

      {node.kind === "sequence" && (
        <div>
          <input value={node.label ?? ""} placeholder="sequence label (optional)" onChange={(e) => set({ label: e.target.value })} style={{ ...inp(), width: "100%", boxSizing: "border-box", marginBottom: 6 }} />
          <Lane nodes={node.steps ?? []} lane={{ containerId: node.id, which: "steps" }} tree={tree} onChange={onChange} jobs={jobs} errByNode={errByNode} />
        </div>
      )}

      {node.kind === "branch" && (
        <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
          <ConditionEditor node={node} tree={tree} set={set} />
          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 8 }}>
            <Arm label="Pass" color={c.success} nodes={node.pass ?? []} lane={{ containerId: node.id, which: "pass" }} tree={tree} onChange={onChange} jobs={jobs} errByNode={errByNode} />
            <Arm label="Fail" color={c.danger} nodes={node.fail ?? []} lane={{ containerId: node.id, which: "fail" }} tree={tree} onChange={onChange} jobs={jobs} errByNode={errByNode} />
          </div>
        </div>
      )}
    </div>
  );
}

function Arm(props: { label: string; color: string; nodes: CanvasNode[]; lane: LaneRef; tree: CanvasNode[]; onChange: (t: CanvasNode[]) => void; jobs: JobOption[]; errByNode: Map<string, string[]> }) {
  return (
    <div style={{ borderLeft: `2px solid ${props.color}`, paddingLeft: 8 }}>
      <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 700, color: props.color, textTransform: "uppercase", letterSpacing: 0.7, marginBottom: 4 }}>{props.label}</div>
      <Lane nodes={props.nodes} lane={props.lane} tree={props.tree} onChange={props.onChange} jobs={props.jobs} errByNode={props.errByNode} />
    </div>
  );
}

function Lane({
  nodes,
  lane,
  tree,
  onChange,
  jobs,
  errByNode,
  armLane,
}: {
  nodes: CanvasNode[];
  lane: LaneRef;
  tree: CanvasNode[];
  onChange: (t: CanvasNode[]) => void;
  jobs: JobOption[];
  errByNode: Map<string, string[]>;
  /** A parallel block's arm lane (PS-1): arms are plain jobs or sequences. */
  armLane?: boolean;
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      {nodes.map((node, i) => (
        <NodeRow key={node.id} node={node} index={i} count={nodes.length} tree={tree} onChange={onChange} jobs={jobs} errByNode={errByNode} />
      ))}
      <div style={{ display: "flex", gap: 6 }}>
        <button style={addBtn()} onClick={() => onChange(appendNode(tree, lane, newJobNode()))}>+ Job</button>
        {/* SW: a sub-workflow step is not offered as a bare parallel ARM —
            walkSteps distinguishes exactly two arm shapes, so wrap it in a
            sequence to run it as one (validateTree says the same). */}
        {!armLane && <button style={addBtn()} onClick={() => onChange(appendNode(tree, lane, newWorkflowNode()))}>+ Sub-workflow</button>}
        {armLane && <button style={addBtn()} onClick={() => onChange(appendNode(tree, lane, newSequenceNode()))}>+ Sequence</button>}
        {!armLane && <button style={addBtn()} onClick={() => onChange(appendNode(tree, lane, newParallelNode()))}>+ Parallel</button>}
        {!armLane && <button style={addBtn()} onClick={() => onChange(appendNode(tree, lane, newBranchNode()))}>+ Branch</button>}
      </div>
    </div>
  );
}

export function WorkflowCanvasEditor({
  value,
  onChange,
  jobs,
  layout,
  onLayoutChange,
}: {
  value: CanvasNode[];
  onChange: (t: CanvasNode[]) => void;
  jobs: JobOption[];
  // WC-P7: advisory hand-arranged node positions. When onLayoutChange is provided the
  // preview canvas becomes drag-to-arrange, and the positions persist with the save.
  layout?: LayoutMap;
  onLayoutChange?: (next: LayoutMap) => void;
}) {
  const errors = validateTree(value);
  const errByNode = new Map<string, string[]>();
  for (const e of errors) {
    const a = errByNode.get(e.nodeId) ?? [];
    a.push(`${e.field}: ${e.message}`);
    errByNode.set(e.nodeId, a);
  }

  // WC-P4: on-canvas A12 wiring. Dragging producer → consumer adds an env input on
  // the consumer (named later in its inspector). `isValidEdge` mirrors the engine:
  // both must be jobs and the producer must be upstream of the consumer.
  const asJob = (id: string): CanvasNode | null => {
    const n = findNode(value, id);
    return n && n.kind === "job" && n.name ? n : null;
  };
  const connectInput = (producerId: string, consumerId: string) => {
    const producer = asJob(producerId);
    const consumer = asJob(consumerId);
    if (!producer || !consumer) return;
    const existing = consumer.inputs ?? [];
    // Seed a valid, unique key so the wire is immediately visible (draws its edge) and
    // persists on save — the user refines the key/output in the inspector.
    const envKey = defaultInputKey(producer.name!, existing.map((r) => r.envKey));
    onChange(updateNode(value, consumerId, { inputs: [...existing, { envKey, fromStep: producer.name!, fromOutput: "" }] }));
  };
  const validInput = (producerId: string, consumerId: string): boolean => {
    if (producerId === consumerId) return false;
    const producer = asJob(producerId);
    const consumer = asJob(consumerId);
    return !!producer && !!consumer && upstreamProducers(value, consumerId).includes(producer.name!);
  };

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12 }} role="group" aria-label="Workflow steps editor">
      {value.length === 0 && (
        <div style={{ fontSize: c.fontSm, color: c.textSec, fontStyle: "italic" }}>
          No steps yet — add a <strong>Job</strong>, <strong>Parallel</strong> group, or <strong>Branch</strong> to start.
        </div>
      )}
      <Lane nodes={value} lane={{ containerId: null, which: "root" }} tree={value} onChange={onChange} jobs={jobs} errByNode={errByNode} />

      <div style={{ fontSize: c.fontSm, color: errors.length ? c.danger : c.success }}>
        {errors.length === 0 ? "✓ Valid graph" : `${errors.length} issue${errors.length === 1 ? "" : "s"} — see the highlighted steps above`}
      </div>

      {value.length > 0 && (
        <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
          <div style={{ display: "flex", alignItems: "baseline", gap: 8, flexWrap: "wrap" }}>
            <div style={{ fontSize: c.fontXs, color: c.textSec, flex: 1, minWidth: 240 }}>
              Drag a job's <strong>handle</strong> to a downstream job to wire an env input (A12).
              {onLayoutChange && <> Drag a node's <strong>body</strong> to arrange the graph — the layout saves with the workflow.</>}
            </div>
            {onLayoutChange && layout && Object.keys(layout).length > 0 && (
              <button style={addBtn()} onClick={() => onLayoutChange({})} title="Discard the hand-arranged layout and auto-lay-out with dagre">
                Reset layout
              </button>
            )}
          </div>
          <WorkflowCanvas steps={toSteps(value)} connectable onConnectEdge={connectInput} isValidEdge={validInput} layout={layout} onLayoutChange={onLayoutChange} />
        </div>
      )}
    </div>
  );
}

// React Flow's stylesheet — imported inside this lazy-loaded module so it ships in
// the editor chunk, not the eager bundle (the app's first CSS asset). The canvas
// itself arrives in WC-P2; this establishes the CSS-in-lazy-chunk seam (WC-P0).
import "@xyflow/react/dist/style.css";
import { useEffect, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { api, csrfHeader, fetchCapabilities } from "../api/client";
import { useGet, rows } from "../hooks";
import { c } from "../theme";
import { StepChain } from "./workflows/StepChain";
import { WorkflowCanvas } from "./workflows/WorkflowCanvas";
import { WorkflowCanvasEditor } from "./workflows/WorkflowCanvasEditor";
import { toCanvasTree, toSteps, parseLayout, type DefStep, type CanvasNode, type LayoutMap } from "./workflows/canvasModel";
import { withFreshIds } from "./workflows/canvasEdit";
import type { WfStep } from "./workflows/shared";
import { ambiguousNames, disambiguate, type NamedRef } from "../utils/disambiguate";
import { Btn, InfoBody, InfoToggle } from "../components/ui";
import { CalendarPicker } from "./scheduling/CalendarPicker";
import { ReactionsEditor, reactionsError, saveReactionsFor, useLoadedReactions } from "./scheduling/ReactionsEditor";
import { deleteBlockedNote, reactionsWatchingRefusal } from "./scheduling/reactions";
import type { components } from "../api/schema";

type Job = components["schemas"]["Job"];
type Schedule = components["schemas"]["Schedule"];
type Workflow = components["schemas"]["Workflow"];
type WorkflowStep = components["schemas"]["WorkflowStep"];
type ValidationError = components["schemas"]["WorkflowValidationError"];

const WF_NAME_RE = /^[a-z0-9][a-z0-9_-]{0,63}$/;

type StepKind = "job" | "parallel" | "branch";

// One job reference inside a parallel block or a branch arm: a name + optional
// per-step source override (A11). One-level authoring — leaves are plain jobs.
interface JobLeaf {
  name: string;
  jobSource?: string;
  // R2F-2 — the job's permanent identity, when this leaf was picked (or loaded)
  // with one. Written ALONGSIDE the name, never instead of it: the name stays
  // the human-readable reference everywhere, the uid is what resolves.
  jobUid?: string;
}

// One A12 input row: an env KEY fed from an upstream step's named output.
interface InputRow {
  envKey: string;
  fromStep: string;
  fromOutput: string;
}

// One authored top-level step. A discriminated shape over the three engine step
// kinds (D2 one level). Job steps carry the A12 + retry advanced fields; parallel
// /branch members are plain job leaves.
interface EditorStep {
  kind: StepKind;
  // job
  name?: string;
  jobSource?: string;
  jobUid?: string; // R2F-2 — see JobLeaf
  label?: string;
  inputs?: InputRow[];
  retries?: string; // "" ⇒ unset (job default)
  backoffSeconds?: string; // "" ⇒ unset
  continueOnError?: boolean | null; // null ⇒ unset (job default)
  // parallel
  jobs?: JobLeaf[];
  // branch
  condition?: { jobRef: string; type: string; field: string; operator: string; value: string };
  pass?: JobLeaf[];
  fail?: JobLeaf[];
}

const newJobStep = (): EditorStep => ({ kind: "job", name: "", inputs: [], retries: "", backoffSeconds: "", continueOnError: null });
const newParallelStep = (): EditorStep => ({ kind: "parallel", label: "", jobs: [] });
const newBranchStep = (): EditorStep => ({
  kind: "branch",
  label: "",
  condition: { jobRef: "", type: "job_status", field: "", operator: "==", value: "" },
  pass: [],
  fail: [],
});

// ── load-mode editability + mapping ────────────────────────────────────────────

function leafIsPlainJob(s: WorkflowStep): boolean {
  return (!s.type || s.type === "job") && !s.jobs?.length && !s.pass && !s.fail && !s.condition;
}
// One level only: the editor can represent job / parallel-of-jobs / branch-of-jobs.
// Anything deeper (nested parallel/branch inside a block or arm) is read-only.
function isEditableGraph(steps: WorkflowStep[]): boolean {
  return steps.every((s) => {
    if (!s.type || s.type === "job") return leafIsPlainJob(s);
    if (s.type === "parallel") return (s.jobs ?? []).every(leafIsPlainJob);
    if (s.type === "branch") {
      const pass = ((s.pass as { steps?: WorkflowStep[] } | null)?.steps ?? []);
      const fail = ((s.fail as { steps?: WorkflowStep[] } | null)?.steps ?? []);
      return [...pass, ...fail].every(leafIsPlainJob);
    }
    return false;
  });
}

function toEditorStep(s: WorkflowStep): EditorStep {
  const leaf = (j: WorkflowStep): JobLeaf => ({ name: j.name ?? "", jobSource: j.jobSource, jobUid: j.jobUid });
  if (s.type === "parallel") {
    return { kind: "parallel", label: s.label, jobs: (s.jobs ?? []).map(leaf) };
  }
  if (s.type === "branch") {
    const cond = (s.condition as { jobRef?: string; type?: string; field?: string; operator?: string; value?: string } | null) ?? {};
    const arm = (b: { steps?: WorkflowStep[] } | null | undefined) => (b?.steps ?? []).map(leaf);
    return {
      kind: "branch",
      label: s.label,
      condition: { jobRef: cond.jobRef ?? "", type: cond.type ?? "job_status", field: cond.field ?? "", operator: cond.operator ?? "==", value: cond.value ?? "" },
      pass: arm(s.pass as { steps?: WorkflowStep[] } | null),
      fail: arm(s.fail as { steps?: WorkflowStep[] } | null),
    };
  }
  return {
    kind: "job",
    name: s.name ?? "",
    jobSource: s.jobSource,
    jobUid: s.jobUid,
    label: s.label,
    inputs: Object.entries(s.inputs ?? {}).map(([envKey, ref]) => ({
      envKey,
      fromStep: (ref as { fromStep?: string })?.fromStep ?? "",
      fromOutput: (ref as { fromOutput?: string })?.fromOutput ?? "",
    })),
    retries: s.retries != null ? String(s.retries) : "",
    backoffSeconds: s.backoffSeconds != null ? String(s.backoffSeconds) : "",
    continueOnError: s.continueOnError ?? null,
  };
}

// ── emit + preview ─────────────────────────────────────────────────────────────

const numOrUndef = (s?: string): number | undefined => {
  if (s == null || s.trim() === "") return undefined;
  const n = parseInt(s, 10);
  return Number.isFinite(n) && n >= 0 ? n : undefined;
};
// R2F-2: a leaf emits the identity it CARRIES and never one derived at save
// time. A legacy name-only step stays name-only unless the operator re-picks the
// job — opening and saving a stored graph must not silently rewrite references
// the editor did not touch (the same rule tags follow).
const emitLeaf = (j: JobLeaf) => ({
  type: "job",
  name: j.name,
  ...(j.jobSource ? { jobSource: j.jobSource } : {}),
  ...(j.jobUid ? { jobUid: j.jobUid } : {}),
});
const cleanLeaves = (jobs?: JobLeaf[]) => (jobs ?? []).filter((j) => j.name);

function emitStep(s: EditorStep): Record<string, unknown> {
  if (s.kind === "parallel") {
    return { type: "parallel", ...(s.label?.trim() ? { label: s.label.trim() } : {}), jobs: cleanLeaves(s.jobs).map(emitLeaf) };
  }
  if (s.kind === "branch") {
    const cond = s.condition!;
    return {
      type: "branch",
      ...(s.label?.trim() ? { label: s.label.trim() } : {}),
      condition: {
        type: cond.type,
        jobRef: cond.jobRef,
        ...(cond.type === "output_match" ? { field: cond.field, operator: cond.operator, value: cond.value } : {}),
      },
      pass: { steps: cleanLeaves(s.pass).map(emitLeaf) },
      fail: { steps: cleanLeaves(s.fail).map(emitLeaf) },
    };
  }
  const inputs = (s.inputs ?? []).filter((r) => r.envKey.trim() && r.fromStep);
  const inputsObj: Record<string, { fromStep: string; fromOutput: string }> = {};
  for (const r of inputs) inputsObj[r.envKey.trim()] = { fromStep: r.fromStep, fromOutput: r.fromOutput.trim() };
  const retries = numOrUndef(s.retries);
  const backoff = numOrUndef(s.backoffSeconds);
  return {
    type: "job",
    name: s.name,
    ...(s.jobSource ? { jobSource: s.jobSource } : {}),
    ...(s.jobUid ? { jobUid: s.jobUid } : {}),
    ...(s.label?.trim() ? { label: s.label.trim() } : {}),
    ...(Object.keys(inputsObj).length ? { inputs: inputsObj } : {}),
    ...(retries !== undefined ? { retries } : {}),
    ...(backoff !== undefined ? { backoffSeconds: backoff } : {}),
    ...(s.continueOnError != null ? { continueOnError: s.continueOnError } : {}),
  };
}

function conditionLabel(cond: EditorStep["condition"]): string {
  if (!cond) return "condition";
  if (cond.type === "output_match") return `${cond.jobRef || "?"}.${cond.field || "?"} ${cond.operator} ${cond.value}`.trim();
  return `${cond.jobRef || "?"} succeeded`;
}
function toPreview(s: EditorStep): WfStep {
  if (s.kind === "parallel") return { type: "parallel", label: s.label, jobs: cleanLeaves(s.jobs).map((j) => ({ type: "job", name: j.name })) };
  if (s.kind === "branch")
    return {
      type: "branch",
      label: s.label,
      condition: { label: conditionLabel(s.condition) },
      pass: cleanLeaves(s.pass).map((j) => ({ type: "job", name: j.name })),
      fail: cleanLeaves(s.fail).map((j) => ({ type: "job", name: j.name })),
    };
  return { type: "job", name: s.name, label: s.label };
}

function producedNames(s: EditorStep): string[] {
  if (s.kind === "job") return s.name ? [s.name] : [];
  if (s.kind === "parallel") return cleanLeaves(s.jobs).map((j) => j.name);
  return [...cleanLeaves(s.pass), ...cleanLeaves(s.fail)].map((j) => j.name);
}
function upstreamNames(steps: EditorStep[], index: number): string[] {
  const out: string[] = [];
  for (let i = 0; i < index; i++) out.push(...producedNames(steps[i]));
  return Array.from(new Set(out.filter(Boolean)));
}

// ── inline-entry round-trip (AW-11 / CAL-23) ───────────────────────────────────

export interface PreservedInline {
  name: string;
  cron: string;
  env: Record<string, string>;
  startAt?: string | null;
  endAt?: string | null;
  interval?: string | null;
  skipCalendars?: string[];
  onlyCalendars?: string[];
}

// CAL-23 — the one place a preserved inline entry becomes wire JSON, used by BOTH
// save paths (linear buildBody and submitCanvas). The mapped return type forces a
// key for EVERY field of the generated wire type, so the next field added to the
// spec cannot be silently cleared on workflow save — the exact omission that
// wiped activation windows until the AW review caught it.
export type WfInlineScheduleWire = NonNullable<components["schemas"]["WorkflowComposeInput"]["schedules"]>[number];
export function preservedInlineToWire(s: PreservedInline): { [K in keyof Required<WfInlineScheduleWire>]: WfInlineScheduleWire[K] } {
  return {
    name: s.name,
    cron: s.cron,
    env: s.env,
    startAt: s.startAt ?? null,
    endAt: s.endAt ?? null,
    interval: s.interval ?? null,
    skipCalendars: s.skipCalendars ?? [],
    onlyCalendars: s.onlyCalendars ?? [],
  };
}

// ── component ──────────────────────────────────────────────────────────────────

export function WorkflowEditor() {
  const navigate = useNavigate();
  const editId = useSearchParams()[0].get("id");
  const isEdit = !!editId;
  // RX-15 — reactions are their own resource with their own endpoint, so they
  // load and save alongside the workflow rather than inside its body.
  const [reactionErr, setReactionErr] = useState<string | null>(null);


  const [canCompose, setCanCompose] = useState<boolean | null>(null);
  useEffect(() => {
    fetchCapabilities().then((cap) => setCanCompose(cap.compose));
  }, []);

  const { data: jobsData } = useGet<unknown>(() => api.GET("/jobs"), []);
  const { data: schedData } = useGet<unknown>(() => api.GET("/schedule-defs"), []);
  const jobs = rows<Job>(jobsData);
  const schedules = rows<Schedule>(schedData);

  const [name, setName] = useState("");
  const reactions = useLoadedReactions("workflow", name, isEdit);
  const [description, setDescription] = useState("");
  const [steps, setSteps] = useState<EditorStep[]>([]);
  const [scheduleRefs, setScheduleRefs] = useState<string[]>([]);
  // Inline schedule entries are not authorable here (this editor binds refs only),
  // but they must round-trip untouched through an edit — INCLUDING the activation
  // window (AW-11) and the calendar bindings (CAL-13), or saving a workflow would
  // silently clear them. The one exception: the calendar bindings ARE editable in
  // place (the CalendarPicker below), since a change-freeze policy should not
  // require API surgery on a workflow authored in-app.
  const [preservedInline, setPreservedInline] = useState<PreservedInline[]>([]);
  const [enabled, setEnabled] = useState(true);

  const [busy, setBusy] = useState(false);
  const [validating, setValidating] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [okMsg, setOkMsg] = useState<string | null>(null);
  const [validateErrors, setValidateErrors] = useState<ValidationError[]>([]);

  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [gitSource, setGitSource] = useState(false);
  const [complexGraph, setComplexGraph] = useState(false);
  const [canvasTree, setCanvasTree] = useState<CanvasNode[] | null>(null);
  const [canvasLayout, setCanvasLayout] = useState<LayoutMap>({}); // WC-P7 advisory node positions
  const [canvasDirty, setCanvasDirty] = useState(false);
  const [forceCanvas, setForceCanvas] = useState(false);
  const [pendingDelete, setPendingDelete] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [delErr, setDelErr] = useState<string | null>(null);
  // RX-24 — the server's refusal when reactions watch this workflow. Non-null
  // swaps both confirm rows (canvas and simple) for the forced one.
  const [delBlock, setDelBlock] = useState<string | null>(null);

  useEffect(() => {
    if (!editId) return;
    let cancelled = false;
    (async () => {
      const { data, error } = await api.GET("/workflows/{workflowId}", { params: { path: { workflowId: Number(editId) } } });
      if (cancelled) return;
      if (error || !data) {
        setLoadErr(`Could not load workflow "${editId}".`);
        return;
      }
      const wf = data as Workflow;
      if (wf.source !== "amadeus") {
        setGitSource(true);
        return;
      }
      const loaded = (wf.steps ?? []) as WorkflowStep[];
      setName(wf.name ?? "");
      setDescription(wf.description ?? "");
      setEnabled(!wf.disabled);
      const entries = wf.schedules ?? [];
      setScheduleRefs(entries.filter((s) => !!s.sourceRef).map((s) => s.sourceRef as string));
      setPreservedInline(
        entries
          .filter((s) => !s.sourceRef)
          .map((s) => ({
            name: s.name ?? "",
            cron: s.cron ?? "",
            env: s.env ?? {},
            startAt: s.startAt ?? null,
            endAt: s.endAt ?? null,
            interval: s.interval ?? null,
            skipCalendars: s.skipCalendars ?? [],
            onlyCalendars: s.onlyCalendars ?? [],
          })),
      );
      // WC-P7: restore any hand-arranged layout regardless of which editor loads —
      // it carries through a later Simple → Advanced switch.
      setCanvasLayout(parseLayout(wf.layout));
      if (!isEditableGraph(loaded)) {
        // Nested graph (branch/parallel deeper than one level): edit it in the
        // canvas editor (WC-P3) rather than the read-only dead-end (RT-2).
        setCanvasTree(withFreshIds(toCanvasTree(loaded as unknown as DefStep[])));
        setComplexGraph(true);
        return;
      }
      setSteps(loaded.map(toEditorStep));
    })();
    return () => {
      cancelled = true;
    };
  }, [editId]);

  // WC-P5: warn before leaving with unsaved canvas edits (tab close / reload).
  useEffect(() => {
    if (!canvasDirty) return;
    const h = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", h);
    return () => window.removeEventListener("beforeunload", h);
  }, [canvasDirty]);

  // ── step mutation helpers ──
  const patchStep = (i: number, patch: Partial<EditorStep>) => setSteps((ss) => ss.map((s, j) => (j === i ? { ...s, ...patch } : s)));
  const addStep = (kind: StepKind) =>
    setSteps((ss) => [...ss, kind === "parallel" ? newParallelStep() : kind === "branch" ? newBranchStep() : newJobStep()]);
  const removeStep = (i: number) => setSteps((ss) => ss.filter((_, j) => j !== i));
  const move = (i: number, dir: -1 | 1) =>
    setSteps((ss) => {
      const j = i + dir;
      if (j < 0 || j >= ss.length) return ss;
      const next = [...ss];
      [next[i], next[j]] = [next[j], next[i]];
      return next;
    });
  const toggleRef = (n: string) => setScheduleRefs((cur) => (cur.includes(n) ? cur.filter((x) => x !== n) : [...cur, n]));
  // CAL-13 — the one preserved-entry field that IS editable here.
  const patchInlineBinding = (i: number, b: { skipCalendars: string[]; onlyCalendars: string[] }) =>
    setPreservedInline((cur) => cur.map((s, j) => (j === i ? { ...s, skipCalendars: b.skipCalendars, onlyCalendars: b.onlyCalendars } : s)));

  function clientError(): string | null {
    if (!WF_NAME_RE.test(name.trim())) return "Name must be a slug (a-z, 0-9, _-, ≤64 chars).";
    if (steps.length === 0) return "Add at least one step.";
    for (const s of steps) {
      if (s.kind === "job" && !s.name) return "Every job step needs a job selected.";
      if (s.kind === "parallel" && cleanLeaves(s.jobs).length === 0) return "Every parallel block needs at least one job.";
      if (s.kind === "branch") {
        if (!s.condition?.jobRef) return "Every branch needs a condition job.";
        if (cleanLeaves(s.pass).length === 0 && cleanLeaves(s.fail).length === 0) return "Every branch needs at least one job in an arm.";
      }
    }
    return null;
  }

  // RX-15 — persist the reactions once the workflow row exists. Separate from
  // buildBody on purpose: reactions are not part of the workflow definition, and
  // folding them in would make a validate call look like it had checked them.
  async function persistReactions() {
    if (!reactions.dirty || !name.trim()) return;
    const rerr = await saveReactionsFor("workflow", name.trim(), reactions.list);
    if (rerr) {
      setReactionErr(`The workflow was saved, but its reactions were not: ${rerr}`);
      return;
    }
    setReactionErr(null);
    reactions.setBaseline(reactions.list);
  }

  function buildBody() {
    return {
      name: name.trim(),
      description: description.trim(),
      enabled,
      scheduleRefs,
      schedules: preservedInline.map(preservedInlineToWire),
      steps: steps.map(emitStep),
    };
  }

  async function runValidate(body: ReturnType<typeof buildBody>): Promise<boolean> {
    const { data } = await api.POST("/workflows/validate", { body: body as never });
    const result = data as components["schemas"]["WorkflowValidationResult"] | undefined;
    setValidateErrors(result?.errors ?? []);
    return !!result?.ok;
  }

  async function validate() {
    setErr(null);
    setOkMsg(null);
    setValidateErrors([]);
    const ce = clientError();
    if (ce) return setErr(ce);
    setValidating(true);
    const ok = await runValidate(buildBody());
    setValidating(false);
    if (ok) setOkMsg("Looks good — no validation errors.");
  }

  async function submit() {
    setErr(null);
    setOkMsg(null);
    setValidateErrors([]);
    const ce = clientError();
    if (ce) return setErr(ce);
    const body = buildBody();
    setBusy(true);
    const ok = await runValidate(body);
    if (!ok) {
      setBusy(false);
      return;
    }
    const { response, error } = isEdit
      ? await api.PUT("/workflows/{workflowId}", { params: { path: { workflowId: Number(editId) }, header: csrfHeader }, body: body as never })
      : await api.POST("/workflows", { body: body as never });
    setBusy(false);
    if (error || !response.ok) {
      setErr(errMessage(error) || `${isEdit ? "Save" : "Create"} failed (${response.status}).`);
      return;
    }
    await persistReactions();
    setOkMsg(`Workflow "${name.trim()}" ${isEdit ? "updated" : "created"} (amadeus-source).`);
    if (isEdit) return;
    setName("");
    setDescription("");
    setSteps([]);
    setScheduleRefs([]);
    setPreservedInline([]);
  }

  // RX-24 — `force` re-issues the delete past the reactions guard, which is the
  // one 409 on this route an operator can actually answer.
  async function del(force = false) {
    if (!editId) return;
    setDelErr(null);
    setDeleting(true);
    const { response, error } = await api.DELETE("/workflows/{workflowId}", { params: { path: { workflowId: Number(editId) }, query: force ? { force: true } : {}, header: csrfHeader } });
    setDeleting(false);
    if (error || !response.ok) {
      const watching = force ? null : reactionsWatchingRefusal(error);
      if (watching) {
        setDelBlock(watching);
        setDelErr(null);
        return; // stay in the confirm row, now offering the forced delete
      }
      setDelErr(errMessage(error) || (response.status === 409 ? "Only amadeus-source workflows can be deleted in-app." : `Delete failed (${response.status}).`));
      setPendingDelete(false);
      setDelBlock(null);
      return;
    }
    navigate("/workflows", { state: { toast: `Workflow "${name.trim() || editId}" deleted.` } });
  }

  // Save path for the WC-P3 canvas editor (nested graphs). Full-state resend: the
  // tree → toSteps, plus the loaded enabled/schedules (RT-3 — never drop them).
  // WC-P6 rollout toggle: move between the linear ("Simple") editor and the canvas
  // ("Advanced") editor, converting state through the round-trip serializer. Simple
  // is only reachable when the graph is one level deep (isEditableGraph).
  const toGraphEditor = () => {
    setErr(null);
    setOkMsg(null);
    setValidateErrors([]);
    setCanvasTree(withFreshIds(toCanvasTree(steps.map(emitStep) as unknown as DefStep[])));
    setForceCanvas(true);
  };
  const canvasIsSimple = (): boolean => (canvasTree ? isEditableGraph(toSteps(canvasTree) as unknown as WorkflowStep[]) : true);
  const toSimpleEditor = () => {
    if (!canvasTree) return;
    const defs = toSteps(canvasTree) as unknown as WorkflowStep[];
    if (!isEditableGraph(defs)) return;
    setErr(null);
    setOkMsg(null);
    setValidateErrors([]);
    setSteps(defs.map(toEditorStep));
    setForceCanvas(false);
  };

  async function submitCanvas() {
    if (!canvasTree) return;
    setErr(null);
    setOkMsg(null);
    setValidateErrors([]);
    if (!WF_NAME_RE.test(name.trim())) return setErr("Name must be a slug (a-z, 0-9, _-, ≤64 chars).");
    const body = {
      name: name.trim(),
      description: description.trim(),
      enabled,
      scheduleRefs,
      schedules: preservedInline.map(preservedInlineToWire),
      steps: toSteps(canvasTree),
      // WC-P7: the canvas is authoritative for layout, so always send it — including an
      // empty {} after a Reset, which clears the stored layout. (The linear editor's
      // buildBody omits layout entirely; that omission is what COALESCE preserves.)
      // Advisory only — never part of steps_hash.
      layout: canvasLayout,
    };
    setBusy(true);
    const ok = await runValidate(body as unknown as ReturnType<typeof buildBody>);
    if (!ok) {
      setBusy(false);
      return;
    }
    const { response, error } = isEdit
      ? await api.PUT("/workflows/{workflowId}", { params: { path: { workflowId: Number(editId) }, header: csrfHeader }, body: body as never })
      : await api.POST("/workflows", { body: body as never });
    setBusy(false);
    if (error || !response.ok) {
      setErr(errMessage(error) || `${isEdit ? "Save" : "Create"} failed (${response.status}).`);
      return;
    }
    await persistReactions();
    setOkMsg(`Workflow "${name.trim()}" ${isEdit ? "updated" : "created"} (amadeus-source).`);
    setCanvasDirty(false);
    if (!isEdit) {
      setName("");
      setDescription("");
      setCanvasTree([]);
      setCanvasLayout({});
      setScheduleRefs([]);
      setPreservedInline([]);
      setForceCanvas(false);
    }
  }

  if (canCompose === false) {
    return (
      <div style={{ color: c.textSec, maxWidth: 560 }}>
        In-app workflow composition requires the <strong>Compose</strong> capability (Admin-only in v20).
        Git-defined workflows are authored through the GitLab publish flow.
      </div>
    );
  }
  if (loadErr) return <div style={{ color: c.danger, maxWidth: 560 }}>{loadErr}</div>;
  if (gitSource)
    return (
      <div style={{ color: c.textSec, maxWidth: 560 }}>
        This workflow is <strong>Git-authored</strong> — read-only here. Edit it through the GitLab publish flow;
        only amadeus-source workflows are editable in-app.
      </div>
    );
  if ((complexGraph || forceCanvas) && canvasTree) {
    const markDirty = () => setCanvasDirty(true);
    const leave = () => {
      if (canvasDirty && !window.confirm("Discard unsaved changes to this workflow?")) return;
      navigate("/workflows");
    };
    const simpleOk = canvasIsSimple();
    return (
      // VC.12: the graph canvas expands to full content width; the intro and the
      // metadata form stay width-capped (carded) so their inputs don't stretch.
      <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        <div style={{ color: c.textSec, fontSize: c.fontSm, maxWidth: 760 }}>
          {isEdit ? (
            <>Editing the <strong>amadeus-source</strong> workflow <span style={{ fontFamily: c.mono }}>{name}</span> in the <strong>graph editor</strong>.</>
          ) : (
            <>Composing a new <strong>amadeus-source</strong> workflow in the <strong>graph editor</strong>.</>
          )}{" "}
          The canvas authors the full structure — jobs, parallel groups, and nested branch arms.
          {canvasDirty && <span style={{ color: c.warning, marginLeft: 6, fontWeight: 600 }}>• unsaved changes</span>}
        </div>

        {/* WC-P5 metadata panel — name / description / enabled / schedules editable alongside the graph. Carded + width-capped (VC.12). */}
        <div style={{ maxWidth: 760, width: "100%", boxSizing: "border-box", display: "flex", flexDirection: "column", gap: 8, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, padding: 16, background: c.panel }}>
          <label style={{ display: "flex", flexDirection: "column", gap: 3, fontSize: c.fontSm, color: c.textSec }}>
            Name{isEdit && <span style={{ color: c.textMuted }}> — immutable on edit</span>}
            <input value={name} onChange={(e) => { setName(e.target.value); markDirty(); }} placeholder="release-pipeline" disabled={isEdit} style={{ ...input(), width: "100%", boxSizing: "border-box", opacity: isEdit ? 0.6 : 1 }} />
          </label>
          <label style={{ display: "flex", flexDirection: "column", gap: 3, fontSize: c.fontSm, color: c.textSec }}>
            Description
            <input value={description} onChange={(e) => { setDescription(e.target.value); markDirty(); }} placeholder="(optional)" style={{ ...input(), width: "100%", boxSizing: "border-box" }} />
          </label>
          <label style={{ display: "flex", alignItems: "center", gap: 8, fontSize: c.fontSm, color: c.text }}>
            <input type="checkbox" checked={enabled} onChange={(e) => { setEnabled(e.target.checked); markDirty(); }} />
            Enabled <span style={{ color: c.textSec }}>(unchecked pauses the workflow)</span>
          </label>
          {schedules.length > 0 && (
            <div style={{ fontSize: c.fontSm, color: c.textSec }}>
              <div style={{ marginBottom: 3 }}>Schedules</div>
              <div style={{ display: "flex", flexWrap: "wrap", gap: 12 }}>
                {schedules.map((s) => (
                  <label key={s.name} style={{ display: "flex", alignItems: "center", gap: 5 }}>
                    <input type="checkbox" checked={scheduleRefs.includes(s.name ?? "")} onChange={() => { toggleRef(s.name ?? ""); markDirty(); }} />
                    <span style={{ fontFamily: c.mono }}>{s.name}</span>
                  </label>
                ))}
              </div>
            </div>
          )}
          <InlineCalendarBindings entries={preservedInline} onPatch={(i, b) => { patchInlineBinding(i, b); markDirty(); }} />
        </div>

        <WorkflowCanvasEditor value={canvasTree} onChange={(t) => { setCanvasTree(t); markDirty(); }} jobs={jobs.map((j) => ({ name: j.name ?? "", source: j.source, uid: j.uid, agencies: (j as NamedRef).agencies ?? undefined }))} layout={canvasLayout} onLayoutChange={(m) => { setCanvasLayout(m); markDirty(); }} />
        {validateErrors.length > 0 && (
          <div style={{ color: c.danger, fontSize: c.fontSm, display: "flex", flexDirection: "column", gap: 2 }}>
            {validateErrors.map((e, i) => (
              <div key={i}>
                {(e.step || e.field) && <span style={{ fontFamily: c.mono }}>{[e.step, e.field].filter(Boolean).join(" · ")}: </span>}
                {e.message}
              </div>
            ))}
          </div>
        )}
        {err && <div style={{ color: c.danger }}>{err}</div>}
        {okMsg && <div style={{ color: c.success }}>{okMsg}</div>}
        {delErr && <div style={{ color: c.danger, fontSize: c.fontSm }}>{delErr}</div>}
        <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}>
          <button onClick={submitCanvas} disabled={busy} style={btn()}>{busy ? (isEdit ? "Saving…" : "Creating…") : isEdit ? "Save" : "Create workflow"}</button>
          <button onClick={toSimpleEditor} disabled={busy || !simpleOk} title={simpleOk ? "Switch to the linear (Simple) editor" : "This graph nests parallel/branch steps — not representable in the linear editor"} style={btnGhost()}>Simple editor</button>
          <button onClick={leave} disabled={busy} style={btnGhost()}>← Back to workflows</button>
          {isEdit && (
            <span style={{ marginLeft: "auto", display: "inline-flex", gap: 8, alignItems: "center" }}>
              {pendingDelete ? (
                <>
                  <span style={{ fontSize: c.fontSm, color: c.danger }}>{delBlock ? deleteBlockedNote(delBlock) : "Delete this workflow?"}</span>
                  <button onClick={() => del(!!delBlock)} disabled={deleting} style={dangerBtn()}>{deleting ? "Deleting…" : delBlock ? "Delete anyway" : "Confirm delete"}</button>
                  <button onClick={() => { setPendingDelete(false); setDelErr(null); setDelBlock(null); }} disabled={deleting} style={btnGhost()}>Cancel</button>
                </>
              ) : (
                <button onClick={() => setPendingDelete(true)} disabled={busy} style={dangerBtn()}>Delete workflow</button>
              )}
            </span>
          )}
        </div>
      </div>
    );
  }

  const previewSteps = steps.map(toPreview);

  return (
    // Centered, carded form column (VC.12), matching Compose / Schedule Builder.
    <div
      style={{
        maxWidth: 760,
        margin: "0 auto",
        width: "100%",
        display: "flex",
        flexDirection: "column",
        gap: 16,
        background: c.panel,
        border: `1px solid ${c.border}`,
        borderRadius: c.radiusSurface,
        padding: 24,
      }}
    >
      <div style={{ color: c.textSec, fontSize: c.fontSm }}>
        {isEdit ? (
          <>
            Editing the <strong>amadeus-source</strong> workflow <span style={{ fontFamily: c.mono }}>{name}</span>.
            Steps can be jobs, parallel groups, or a branch; it runs through the same engine as a Git workflow.
          </>
        ) : (
          <>Compose an <strong>amadeus-source</strong> workflow: a chain of job, parallel, and branch steps, optionally scheduled.</>
        )}
      </div>

      <Field label="Name">
        <input style={{ ...input(), opacity: isEdit ? 0.6 : 1 }} value={name} onChange={(e) => setName(e.target.value)} placeholder="release-pipeline" disabled={isEdit} />
        {isEdit && <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 4 }}>Name is the identity — immutable on edit.</div>}
      </Field>
      <Field label="Description (optional)">
        <input style={input()} value={description} onChange={(e) => setDescription(e.target.value)} />
      </Field>

      <Field label="Steps (run in order)">
        {steps.length === 0 ? (
          // VU-14 — the empty state carries the add-step affordance rather than
          // stating the absence and leaving the kind buttons below to be found.
          // Same handler as "+ Job"; a job step is the only kind a workflow can
          // start with that is valid on its own.
          <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap", marginBottom: 8 }}>
            <span style={{ fontSize: c.fontSm, color: c.textMuted }}>No steps yet — a workflow runs its steps in order.</span>
            <Btn small onClick={() => addStep("job")}>
              Add a job step
            </Btn>
          </div>
        ) : (
          <div style={{ display: "flex", flexDirection: "column", gap: 10, marginBottom: 10 }}>
            {steps.map((s, i) => (
              <StepCard
                key={i}
                index={i}
                total={steps.length}
                step={s}
                jobs={jobs}
                upstream={upstreamNames(steps, i)}
                onPatch={(p) => patchStep(i, p)}
                onRemove={() => removeStep(i)}
                onMove={(d) => move(i, d)}
              />
            ))}
          </div>
        )}
        <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
          <button style={btn()} onClick={() => addStep("job")}>+ Job</button>
          <button style={btnGhost()} onClick={() => addStep("parallel")}>+ Parallel group</button>
          <button style={btnGhost()} onClick={() => addStep("branch")}>+ Branch</button>
          <button style={btnGhost()} onClick={toGraphEditor} title="Compose visually on the graph canvas — supports nested branches/parallels">Graph editor (Advanced) →</button>
        </div>
      </Field>

      {steps.length > 0 && (
        <Field label="Preview">
          <div style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, background: c.panel2, padding: "14px 12px", overflowX: "auto" }}>
            <StepChain steps={previewSteps} />
          </div>
        </Field>
      )}

      {steps.length > 0 && (
        <Field label="Graph (read-only preview)">
          <WorkflowCanvas steps={steps.map(emitStep) as unknown as DefStep[]} />
        </Field>
      )}

      {/* RX-15 — the fourth trigger kind, beside the other trigger surfaces.
          Edit-mode only: a reaction attaches to the workflow BY NAME, so there
          is nothing to attach to until it exists. */}
      <Field
        label="Reactions (run this workflow when something else finishes)"
        info={
          <>
            Runs this workflow when another definition finishes. A reaction has no clock, so it never appears in
            Upcoming — and a newly added one starts from now: it fires on the <strong>next</strong> matching
            completion, never retroactively on one that already happened.
          </>
        }
      >
        <ReactionsEditor
          value={reactions.list}
          onChange={reactions.setList}
          ownerKind="workflow"
          ownerName={isEdit ? name : ""}
          ownerSource="amadeus"
          loadError={reactions.loadError}
        />
        {reactionsError({ kind: "workflow", name, source: "amadeus" }, reactions.list) && (
          <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 6 }}>
            {reactionsError({ kind: "workflow", name, source: "amadeus" }, reactions.list)}
          </div>
        )}
        {reactionErr && <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 6 }}>{reactionErr}</div>}
      </Field>

      <Field label="Schedules">
        {schedules.length === 0 ? (
          // VU-14 — a workflow cannot author a shared schedule, so the empty state
          // hands over the Schedule Builder rather than restating the consequence.
          // Reaching the editor already required Compose, which is the same gate.
          <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
            <span style={{ fontSize: c.fontSm, color: c.textMuted }}>
              No first-class schedules yet — the workflow will be manual-only.
            </span>
            <Btn small onClick={() => navigate("/schedule-builder")}>
              New schedule
            </Btn>
          </div>
        ) : (
          <div style={{ display: "flex", flexWrap: "wrap", gap: 8 }}>
            {schedules.map((s) => (
              <label key={`${s.source}:${s.name}`} style={{ display: "flex", alignItems: "center", gap: 6, fontSize: c.fontSm, color: c.textSec }}>
                <input type="checkbox" checked={scheduleRefs.includes(s.name ?? "")} onChange={() => toggleRef(s.name ?? "")} />
                <span style={{ fontFamily: c.mono }}>{s.name}</span>
                <span style={{ color: c.textMuted }}>{s.cron}</span>
              </label>
            ))}
          </div>
        )}
        {preservedInline.length > 0 && (
          <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 8 }}>
            {preservedInline.length} inline schedule{preservedInline.length === 1 ? "" : "s"} preserved on save (authored via the API).
          </div>
        )}
        <InlineCalendarBindings entries={preservedInline} onPatch={patchInlineBinding} />
      </Field>

      <label style={{ display: "flex", alignItems: "center", gap: 8, fontSize: c.fontSm, color: c.textSec }}>
        <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} /> Enabled
      </label>

      {validateErrors.length > 0 && (
        <div style={{ border: `1px solid ${c.danger}40`, borderRadius: c.radiusSurface, background: `${c.danger}10`, padding: "10px 14px" }}>
          <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.danger, textTransform: "uppercase", letterSpacing: 0.7, marginBottom: 6 }}>Validation errors</div>
          <ul style={{ margin: 0, paddingLeft: 18, display: "flex", flexDirection: "column", gap: 4 }}>
            {validateErrors.map((e, i) => (
              <li key={i} style={{ fontSize: c.fontSm, color: c.text }}>
                {(e.step || e.field) && <span style={{ fontFamily: c.mono, color: c.danger }}>{[e.step, e.field].filter(Boolean).join(" · ")}: </span>}
                {e.message}
              </li>
            ))}
          </ul>
        </div>
      )}

      {err && <div style={{ color: c.danger, fontSize: c.fontSm }}>{err}</div>}
      {okMsg && <div style={{ color: c.primary, fontSize: c.fontSm }}>{okMsg}</div>}

      <div style={{ display: "flex", gap: 10 }}>
        <button onClick={submit} disabled={busy || validating} style={btn()}>
          {isEdit ? (busy ? "Saving…" : "Save") : busy ? "Creating…" : "Create workflow"}
        </button>
        <button onClick={validate} disabled={busy || validating} style={btnGhost()}>{validating ? "Validating…" : "Validate"}</button>
        {isEdit && <button onClick={() => navigate("/workflows")} disabled={busy || validating} style={btnGhost()}>Cancel</button>}
      </div>

      {isEdit && (
        <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap", borderTop: `1px solid ${c.border}`, paddingTop: 14 }}>
          {pendingDelete ? (
            <>
              <span style={{ fontSize: c.fontSm, color: delBlock ? c.danger : c.textSec }}>
                {delBlock ? deleteBlockedNote(delBlock) : "Delete this workflow and its schedule bindings?"}
              </span>
              <button onClick={() => del(!!delBlock)} disabled={deleting} style={dangerBtn()}>{deleting ? "Deleting…" : delBlock ? "Delete anyway" : "Confirm delete"}</button>
              <button onClick={() => { setPendingDelete(false); setDelErr(null); setDelBlock(null); }} disabled={deleting} style={btnGhost()}>Cancel</button>
            </>
          ) : (
            <button onClick={() => setPendingDelete(true)} disabled={busy || validating} style={dangerBtn()}>Delete workflow</button>
          )}
          {delErr && <span style={{ fontSize: c.fontSm, color: c.danger }}>{delErr}</span>}
        </div>
      )}
    </div>
  );
}

// ── inline-entry calendar bindings (CAL-13) ─────────────────────────────────────
// Preserved inline entries round-trip untouched EXCEPT their calendar bindings,
// which are editable here per entry: a change-freeze policy should be attachable
// without re-authoring the entry over the API. Renders nothing when there are no
// inline entries.
function InlineCalendarBindings({
  entries,
  onPatch,
}: {
  entries: PreservedInline[];
  onPatch: (i: number, b: { skipCalendars: string[]; onlyCalendars: string[] }) => void;
}) {
  if (entries.length === 0) return null;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 10, marginTop: 8 }}>
      {entries.map((s, i) => (
        <div key={`${s.name}:${i}`} style={{ borderLeft: `3px solid ${c.border}`, paddingLeft: 10 }}>
          <div style={{ fontSize: c.fontXs, color: c.textSec, marginBottom: 4 }}>
            Working calendars for inline schedule <span style={{ fontFamily: c.mono }}>{s.name || `#${i + 1}`}</span>
          </div>
          <CalendarPicker
            value={{ skipCalendars: s.skipCalendars ?? [], onlyCalendars: s.onlyCalendars ?? [] }}
            onChange={(b) => onPatch(i, b)}
            spec={s}
            compact
          />
        </div>
      ))}
    </div>
  );
}

// ── one top-level step card ─────────────────────────────────────────────────────

function StepCard({
  index,
  total,
  step,
  jobs,
  upstream,
  onPatch,
  onRemove,
  onMove,
}: {
  index: number;
  total: number;
  step: EditorStep;
  jobs: Job[];
  upstream: string[];
  onPatch: (p: Partial<EditorStep>) => void;
  onRemove: () => void;
  onMove: (dir: -1 | 1) => void;
}) {
  const [advanced, setAdvanced] = useState(false);
  const kindColor = step.kind === "parallel" ? c.accent : step.kind === "branch" ? c.warning : c.primary;
  return (
    // These cards repeat inside the already-bordered form column, so the outline
    // was a third frame around content the page had already boxed twice (VU-5).
    // The kind-coloured left rail is information — it stays — and the panel2 fill
    // is what separates one step from the next now.
    <div style={{ borderLeft: `3px solid ${kindColor}`, borderRadius: c.radiusSurface, background: c.panel2, padding: "10px 12px" }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 8 }}>
        <span style={{ fontSize: c.fontXs, fontWeight: 700, color: c.textSec, background: c.panel, border: `1px solid ${c.border}`, borderRadius: c.radiusChip, padding: "1px 5px" }}>{index + 1}</span>
        <select style={{ ...input(), width: 150 }} value={step.kind} onChange={(e) => onPatch(kindReset(e.target.value as StepKind))}>
          <option value="job">Job</option>
          <option value="parallel">Parallel group</option>
          <option value="branch">Branch</option>
        </select>
        <span style={{ flex: 1 }} />
        {step.kind === "job" && <button style={miniBtn()} onClick={() => setAdvanced((a) => !a)}>{advanced ? "Advanced ▲" : "Advanced ▼"}</button>}
        <button style={miniBtn()} onClick={() => onMove(-1)} disabled={index === 0}>↑</button>
        <button style={miniBtn()} onClick={() => onMove(1)} disabled={index === total - 1}>↓</button>
        <button style={miniBtn()} onClick={onRemove}>✕</button>
      </div>

      {step.kind === "job" && (
        <>
          <JobSelect jobs={jobs} value={leafValue({ name: step.name ?? "", jobSource: step.jobSource, jobUid: step.jobUid }, jobs)} onChange={(v) => onPatch(parseLeaf(v, jobs))} />
          {advanced && <JobAdvanced step={step} upstream={upstream} onPatch={onPatch} />}
        </>
      )}

      {step.kind === "parallel" && (
        <JobLeafList label="Jobs (run concurrently)" jobs={jobs} leaves={step.jobs ?? []} onChange={(jobsArr) => onPatch({ jobs: jobsArr })} />
      )}

      {step.kind === "branch" && (
        <BranchEditor step={step} jobs={jobs} upstream={upstream} onPatch={onPatch} />
      )}
    </div>
  );
}

// Reset to a fresh step of the chosen kind (switching kinds discards kind-specific fields).
function kindReset(kind: StepKind): EditorStep {
  return kind === "parallel" ? newParallelStep() : kind === "branch" ? newBranchStep() : newJobStep();
}

function JobAdvanced({ step, upstream, onPatch }: { step: EditorStep; upstream: string[]; onPatch: (p: Partial<EditorStep>) => void }) {
  const inputs = step.inputs ?? [];
  const setInput = (i: number, patch: Partial<InputRow>) => onPatch({ inputs: inputs.map((r, j) => (j === i ? { ...r, ...patch } : r)) });
  const addInput = () => onPatch({ inputs: [...inputs, { envKey: "", fromStep: "", fromOutput: "" }] });
  const removeInput = (i: number) => onPatch({ inputs: inputs.filter((_, j) => j !== i) });
  return (
    <div style={{ marginTop: 10, display: "flex", flexDirection: "column", gap: 12, borderTop: `1px solid ${c.border}`, paddingTop: 10 }}>
      <Field label="Label (optional)"><input style={input()} value={step.label ?? ""} onChange={(e) => onPatch({ label: e.target.value })} /></Field>

      <div>
        <div style={miniLabel()}>Inputs (A12 — env ← an upstream step's output)</div>
        {inputs.length === 0 ? (
          <div style={{ fontSize: c.fontXs, color: c.textMuted, marginBottom: 6 }}>No inputs. Feed this step env from an earlier step's captured output.</div>
        ) : (
          <div style={{ display: "flex", flexDirection: "column", gap: 6, marginBottom: 6 }}>
            {inputs.map((r, i) => (
              <div key={i} style={{ display: "flex", gap: 6, alignItems: "center" }}>
                <input style={{ ...input(), flex: 1, fontFamily: c.mono }} placeholder="ENV_KEY" value={r.envKey} onChange={(e) => setInput(i, { envKey: e.target.value })} />
                <span style={{ color: c.textMuted, fontSize: c.fontSm }}>←</span>
                <select style={{ ...input(), flex: 1 }} value={r.fromStep} onChange={(e) => setInput(i, { fromStep: e.target.value })}>
                  <option value="">— from step —</option>
                  {upstream.map((n) => <option key={n} value={n}>{n}</option>)}
                </select>
                <input style={{ ...input(), flex: 1, fontFamily: c.mono }} placeholder="output key" value={r.fromOutput} onChange={(e) => setInput(i, { fromOutput: e.target.value })} />
                <button style={miniBtn()} onClick={() => removeInput(i)}>✕</button>
              </div>
            ))}
          </div>
        )}
        <button style={miniBtn()} onClick={addInput} disabled={upstream.length === 0}>+ input</button>
        {upstream.length === 0 && <span style={{ fontSize: c.fontXs, color: c.textMuted, marginLeft: 8 }}>needs an upstream step first</span>}
      </div>

      <div>
        <div style={miniLabel()}>Retry (overrides the job default)</div>
        <div style={{ display: "flex", gap: 10, alignItems: "center", flexWrap: "wrap" }}>
          <label style={smallField()}>Retries<input type="number" min={0} style={{ ...input(), width: 80 }} placeholder="default" value={step.retries ?? ""} onChange={(e) => onPatch({ retries: e.target.value })} /></label>
          <label style={smallField()}>Backoff (s)<input type="number" min={0} style={{ ...input(), width: 90 }} placeholder="default" value={step.backoffSeconds ?? ""} onChange={(e) => onPatch({ backoffSeconds: e.target.value })} /></label>
          <label style={smallField()}>Continue on error
            <select style={{ ...input(), width: 110 }} value={step.continueOnError == null ? "" : step.continueOnError ? "yes" : "no"} onChange={(e) => onPatch({ continueOnError: e.target.value === "" ? null : e.target.value === "yes" })}>
              <option value="">default</option>
              <option value="yes">yes</option>
              <option value="no">no</option>
            </select>
          </label>
        </div>
      </div>
    </div>
  );
}

function BranchEditor({ step, jobs, upstream, onPatch }: { step: EditorStep; jobs: Job[]; upstream: string[]; onPatch: (p: Partial<EditorStep>) => void }) {
  const cond = step.condition!;
  type Cond = { jobRef: string; type: string; field: string; operator: string; value: string };
  const setCond = (patch: Partial<Cond>) => onPatch({ condition: { ...cond, ...patch } });
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
      <div>
        <div style={miniLabel()}>Condition</div>
        <div style={{ display: "flex", gap: 6, alignItems: "center", flexWrap: "wrap" }}>
          <select style={{ ...input(), width: 160 }} value={cond.jobRef} onChange={(e) => setCond({ jobRef: e.target.value })}>
            <option value="">— job to check —</option>
            {upstream.map((n) => <option key={n} value={n}>{n}</option>)}
          </select>
          <select style={{ ...input(), width: 150 }} value={cond.type} onChange={(e) => setCond({ type: e.target.value })}>
            <option value="job_status">succeeded</option>
            <option value="output_match">output matches</option>
          </select>
          {cond.type === "output_match" && (
            <>
              <input style={{ ...input(), width: 120, fontFamily: c.mono }} placeholder="output key" value={cond.field} onChange={(e) => setCond({ field: e.target.value })} />
              <select style={{ ...input(), width: 90 }} value={cond.operator} onChange={(e) => setCond({ operator: e.target.value })}>
                <option value="==">==</option>
                <option value="!=">!=</option>
                <option value="contains">contains</option>
              </select>
              <input style={{ ...input(), width: 120, fontFamily: c.mono }} placeholder="value" value={cond.value} onChange={(e) => setCond({ value: e.target.value })} />
            </>
          )}
        </div>
        {upstream.length === 0 && <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 4 }}>Add a job step before the branch so it has something to check.</div>}
      </div>
      <JobLeafList label="Pass arm (condition true)" jobs={jobs} leaves={step.pass ?? []} onChange={(p) => onPatch({ pass: p })} />
      <JobLeafList label="Fail arm (condition false)" jobs={jobs} leaves={step.fail ?? []} onChange={(f) => onPatch({ fail: f })} />
    </div>
  );
}

function JobLeafList({ label, jobs, leaves, onChange }: { label: string; jobs: Job[]; leaves: JobLeaf[]; onChange: (l: JobLeaf[]) => void }) {
  return (
    <div>
      <div style={miniLabel()}>{label}</div>
      {leaves.length > 0 && (
        <div style={{ display: "flex", flexDirection: "column", gap: 6, marginBottom: 6 }}>
          {leaves.map((leaf, i) => (
            <div key={i} style={{ display: "flex", gap: 6, alignItems: "center" }}>
              <JobSelect jobs={jobs} value={leafValue(leaf, jobs)} onChange={(v) => onChange(leaves.map((l, j) => (j === i ? parseLeaf(v, jobs) : l)))} />
              <button style={miniBtn()} onClick={() => onChange(leaves.filter((_, j) => j !== i))}>✕</button>
            </div>
          ))}
        </div>
      )}
      <button style={miniBtn()} onClick={() => onChange([...leaves, { name: "" }])}>+ job</button>
    </div>
  );
}

// R2F-2 — the option VALUE is the job's uid, so picking says which job even when
// two departments own the name. Legacy name-only leaves keep working through the
// NAME_VALUE sentinel below: they display against the job they resolve to, and
// keep their name-only shape until the operator re-picks.
const NAME_VALUE = "name:";

function JobSelect({ jobs, value, onChange }: { jobs: Job[]; value: string; onChange: (v: string) => void }) {
  const dupes = ambiguousNames(jobs as NamedRef[]);
  // A stored leaf whose name resolves to no listed job (deleted, renamed, or
  // out of this operator's scope) would otherwise silently display as "— pick a
  // job —", which reads as "this step has no job" rather than "this step points
  // somewhere you cannot see". Render what it says instead.
  const unlisted = value.startsWith(NAME_VALUE) ? value.slice(NAME_VALUE.length) : "";
  return (
    <select style={{ ...input(), flex: 1 }} value={value} onChange={(e) => onChange(e.target.value)}>
      <option value="">— pick a job —</option>
      {unlisted && <option value={value}>{unlisted.split(" ")[0]} (by name — not in your job list)</option>}
      {jobs.map((j) => (
        <option key={j.uid || `${j.source}:${j.name}:${j.id}`} value={jobOptionValue(j)}>
          {disambiguate(j as NamedRef, dupes)} [{j.source}] ({j.type})
        </option>
      ))}
    </select>
  );
}

/** A job's option value: its uid, falling back to the name form pre-R2F-2 rows use. */
const jobOptionValue = (j: Job): string => j.uid || `${NAME_VALUE}${j.name} ${j.source ?? ""}`.trim();

// The select's current value for a leaf. A uid-bearing leaf is its uid. A
// name-only leaf displays against the job its name resolves to when that is
// UNAMBIGUOUS — showing the operator what will actually run — while the leaf
// object itself is untouched, so a save still emits it name-only. When the name
// is ambiguous or unlisted, it falls back to the name sentinel rather than
// picking a twin to display, because displaying one would be a claim the step
// does not make.
function leafValue(l: JobLeaf, jobs: Job[]): string {
  if (l.jobUid) return l.jobUid;
  if (!l.name) return "";
  const matches = jobs.filter((j) => j.name === l.name && (!l.jobSource || j.source === l.jobSource));
  if (matches.length === 1 && matches[0].uid) return matches[0].uid;
  return `${NAME_VALUE}${l.name} ${l.jobSource ?? ""}`.trim();
}

// Picking a real option writes name + source + identity together. Re-selecting
// the name sentinel (the unlisted option) leaves the leaf name-only, as it was.
function parseLeaf(v: string, jobs: Job[]): JobLeaf {
  if (v.startsWith(NAME_VALUE)) {
    const [n, src] = v.slice(NAME_VALUE.length).split(" ");
    return { name: n ?? "", jobSource: src || undefined };
  }
  const j = jobs.find((x) => x.uid === v);
  if (!j) return { name: "" };
  return { name: j.name ?? "", jobSource: j.source, jobUid: j.uid };
}

function errMessage(error: unknown): string {
  if (error && typeof error === "object" && "message" in error) return String((error as { message?: unknown }).message ?? "");
  return "";
}

// EP-7 — `info` mirrors the Job Composer's Field exactly (same ⓘ atom, same
// rule: education behind the toggle, consequences inline). The two authoring
// surfaces keep their own Field copies for their own spacing, but the ⓘ is the
// ONE shared component — a third hand-rolled disclosure is the regression the
// component kit exists to prevent.
function Field({ label, info, children }: { label: string; info?: React.ReactNode; children: React.ReactNode }) {
  const [showInfo, setShowInfo] = useState(false);
  return (
    <div>
      <div style={{ display: "flex", alignItems: "center", gap: 6, marginBottom: 4 }}>
        <span style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textMuted, textTransform: "uppercase", letterSpacing: 0.7 }}>{label}</span>
        {info != null && <InfoToggle open={showInfo} onToggle={() => setShowInfo((v) => !v)} />}
      </div>
      {info != null && showInfo && <InfoBody margin="0 0 8px">{info}</InfoBody>}
      {children}
    </div>
  );
}

// Functions, not module-level consts, so a theme toggle re-reads the active
// palette. Capturing c.* tokens at module load freezes the load-time theme
// (default: dark). See theme.ts: "never capture token values in module-level
// constants."
const miniLabel = (): React.CSSProperties => ({ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textMuted, textTransform: "uppercase", letterSpacing: 0.7, marginBottom: 5 });
const smallField = (): React.CSSProperties => ({ display: "flex", flexDirection: "column", gap: 3, fontSize: c.fontXs, color: c.textSec });
const input = (): React.CSSProperties => ({ width: "100%", padding: "8px 10px", background: c.panel2, border: `1px solid ${c.borderStrong}`, borderRadius: c.radiusChip, color: c.text, fontSize: c.fontSm, fontFamily: c.sans });
const btn = (): React.CSSProperties => ({ padding: "9px 18px", background: c.primary, color: c.onSolid, border: "none", borderRadius: c.radiusChip, fontSize: c.fontSm, fontWeight: 600, cursor: "pointer" });
const btnGhost = (): React.CSSProperties => ({ padding: "9px 18px", background: "transparent", color: c.text, border: `1px solid ${c.border}`, borderRadius: c.radiusChip, fontSize: c.fontSm, cursor: "pointer" });
const dangerBtn = (): React.CSSProperties => ({ padding: "9px 18px", background: c.danger, color: c.onSolid, border: "none", borderRadius: c.radiusChip, fontSize: c.fontSm, fontWeight: 600, cursor: "pointer" });
const miniBtn = (): React.CSSProperties => ({ padding: "2px 8px", background: c.panel2, color: c.textSec, border: `1px solid ${c.border}`, borderRadius: c.radiusChip, fontSize: c.fontSm, cursor: "pointer", whiteSpace: "nowrap" });

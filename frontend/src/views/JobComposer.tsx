import { useEffect, useRef, useState, useMemo } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { api } from "../api/client";
import { fetchCapabilities } from "../api/client";
import { useGet, rows } from "../hooks";
import { c } from "../theme";
import { guideForRunType } from "../components/docLinks";
import { EnvRowsEditor, Btn, ExecutorChoice, InfoBody, InfoToggle, Input, Select, SourceBadge, fieldLabelStyle, inputStyle, type EnvKV,
  DocLink,
} from "../components/ui";
import { RunnerPinInput, useRunnerTags } from "../components/RunnerPin";
import { ComposeKeyPicker, ComposeReferencePicker, putBindings } from "../components/ReferenceBindings";
import { ScriptVariablesPanel } from "../components/ScriptVariables";
import { ScriptPicker } from "../components/ScriptPicker";
import { ScopePicker } from "../components/ScopePicker";
import { CRON_PRESETS, cronPreview, describeSpec, modeOf, nextSpecTimes, validateSpec } from "./scheduling/cron";
import { WindowEditor } from "./scheduling/WindowEditor";
import { CalendarPicker, bindingError } from "./scheduling/CalendarPicker";
import { ReactionsEditor, reactionsError, saveReactionsFor, useLoadedReactions } from "./scheduling/ReactionsEditor";
import { deleteBlockedNote, reactionsWatchingRefusal } from "./scheduling/reactions";
import { IntervalField, ModePicker } from "./scheduling/ModePicker";
import { isIdentityCapable, isRunnerOnly } from "../runtypes";
import type { components } from "../api/schema";

type Script = components["schemas"]["Script"];
type Schedule = components["schemas"]["Schedule"];
type Scope = components["schemas"]["Scope"];
type EnvVar = components["schemas"]["EnvVar"];
type EnvSecret = components["schemas"]["EnvSecret"];
type Job = components["schemas"]["Job"];
type ReferenceBinding = components["schemas"]["ReferenceBinding"];

type ConcurrencyPolicy = "Allow" | "Forbid" | "Queue";

// Advanced job-definition fields the composer round-trips but does not yet expose
// in the form (JC-P2 adds the UI). The compose PUT is a full-replace upsert, so any
// field omitted from the body is overwritten with its zero value — these are held
// verbatim from GET /jobs/{id} and re-sent unchanged so an edit never strips them.
interface Preserved {
  timeoutSeconds: number;
  retries: number;
  backoffSeconds: number;
  continueOnError: boolean;
  concurrencyPolicy: ConcurrencyPolicy;
  concurrencyKey: string;
  tags: string[];
  requestable: boolean;
  warnAfterSeconds: number;
  mustFinishBy: string;
  watch: { path: string; stableSeconds?: number }[];
  // JR-Q5 — per-job run-input enforcement. Lives in Preserved (not its own state) so
  // the JC-P1 full-replace PUT always re-sends it and an edit can't silently reset a
  // job from `block` back to `warn`.
  promptEnforcement: "warn" | "block";
  // RP-14 — env-var NAMES the runner resolves from its own environment into an
  // ansible/terraform child process. Here (rather than its own state) for the
  // promptEnforcement reason: the full-replace PUT must always re-send it.
  envPassthrough: string[];
}
const DEFAULT_PRESERVED: Preserved = {
  timeoutSeconds: 0,
  retries: 0,
  backoffSeconds: 0,
  continueOnError: false,
  concurrencyPolicy: "Allow",
  concurrencyKey: "",
  tags: [],
  envPassthrough: [],
  requestable: false,
  warnAfterSeconds: 0,
  mustFinishBy: "",
  watch: [],
  promptEnforcement: "warn",
};

// JC-P3 — an inline (job-local) schedule authored directly in the composer: name +
// cron + plaintext env, sent as JobComposeInput.schedules[] (distinct from
// scheduleRefs, which bind reusable first-class schedules). Inline entries persist
// with definition_schedules.source_ref = NULL (so they read back with sourceRef null).
export interface InlineSchedule {
  name: string;
  cron: string;
  env: EnvKV[];
  // Activation window (AW-11): optional RFC3339 bounds deferring the first fire
  // and/or expiring the entry. Both null is the pre-window behavior.
  startAt?: string | null;
  endAt?: string | null;
  // Phase 2 — the anchored-interval mode's duration ("7d"), mutually exclusive
  // with cron and anchored on startAt.
  interval?: string | null;
  // CAL-13 — working-calendar bindings, both polarities.
  skipCalendars?: string[];
  onlyCalendars?: string[];
}
const ENTRY_NAME_RE = /^[a-z0-9][a-z0-9_-]{0,63}$/;

// CAL-23 — the one place an inline entry becomes wire JSON. The mapped return
// type forces a key for EVERY field of the generated wire type, so a field added
// to the spec cannot be silently dropped here again (the AW activation window
// was lost on every composer save exactly this way).
export type InlineScheduleWire = NonNullable<components["schemas"]["JobComposeInput"]["schedules"]>[number];
export function inlineScheduleToWire(s: InlineSchedule): { [K in keyof Required<InlineScheduleWire>]: InlineScheduleWire[K] } {
  return {
    name: s.name,
    cron: s.cron,
    env: envRowsToMap(s.env),
    startAt: s.startAt ?? null,
    endAt: s.endAt ?? null,
    interval: s.interval ?? null,
    skipCalendars: s.skipCalendars ?? [],
    onlyCalendars: s.onlyCalendars ?? [],
  };
}

// UDV1 — a declared prompt-variable row in the editor. `options` is edited as a
// comma/newline string and split into JobPrompt.options[] on submit. `default` is
// always a string in the form; "" round-trips as JSON null on the wire.
type JobPrompt = components["schemas"]["JobPrompt"];
interface PromptRow {
  name: string;
  label: string;
  required: boolean;
  default: string;
  options: string;
}
const splitOptions = (s: string): string[] =>
  s
    .split(/[,\n]/)
    .map((o) => o.trim())
    .filter(Boolean);
// Form rows → JobPrompt[] for the wire; drops rows with an empty name (the bind key).
function promptRowsToWire(rows: PromptRow[]): JobPrompt[] {
  const out: JobPrompt[] = [];
  for (const r of rows) {
    const name = r.name.trim();
    if (!name) continue;
    const p: JobPrompt = { name };
    if (r.label.trim()) p.label = r.label.trim();
    if (r.required) p.required = true;
    if (r.default !== "") p.default = r.default;
    const opts = splitOptions(r.options);
    if (opts.length) p.options = opts;
    out.push(p);
  }
  return out;
}
// JobPrompt[] (from a loaded job) → editable form rows.
function promptWireToRows(ps: JobPrompt[] | undefined): PromptRow[] {
  return (ps ?? []).map((p) => ({
    name: p.name ?? "",
    label: p.label ?? "",
    required: !!p.required,
    default: p.default ?? "",
    options: (p.options ?? []).join(", "),
  }));
}

// Trim empty keys into a plaintext env map for the wire.
function envRowsToMap(rows: EnvKV[]): Record<string, string> {
  const m: Record<string, string> = {};
  for (const r of rows) {
    const k = r.key.trim();
    if (k) m[k] = r.value;
  }
  return m;
}

// In-app Job composition (A11 / v20 Phase 3): bind a Git Script × Schedule(s) ×
// Scope × execution options into an amadeus-source Job — no Git round-trip. Gated
// on the Compose capability (Admin-only in v20); non-admins see a notice. ?id=
// selects edit mode (V1.1-15), reusing the whole composer to PUT an existing
// amadeus job; Delete lives in this view behind a confirm (D4). ?cloneFrom=
// prefills every field from an existing job but stays in create mode (POST,
// name editable, no Delete); ?script= presets just the script ref, for the
// Scripts catalog's "Create Job" hand-off.
export function JobComposer() {
  const navigate = useNavigate();
  const search = useSearchParams()[0];
  const editId = search.get("id");
  // Clone: same prefill as edit, but the result is a NEW job. ?id= wins if both
  // are present — a clone of an open editor is not a state this UI produces.
  const cloneId = editId ? null : search.get("cloneFrom");
  const isEdit = !!editId;
  // The job to prefill the form from, in either mode.
  const loadId = editId ?? cloneId;

  const [canCompose, setCanCompose] = useState<boolean | null>(null);
  // EV-6 parity — key-binding writes go through the reference-bindings PUT, which
  // is gated on ManageEnvVars (same as JobKeyField in job detail).
  const [canManageEnv, setCanManageEnv] = useState(false);
  // AF-2 — may this caller author an ALL-scoped job? Only an unrestricted compose
  // grant may, because a scheduled fire of an unbound job runs scope-unchecked
  // (RB-30). A departmental composer is offered named scopes only, rather than
  // being shown an option the server will refuse.
  const [canComposeUnbound, setCanComposeUnbound] = useState(false);
  useEffect(() => {
    fetchCapabilities().then((caps) => {
      setCanCompose(caps.compose);
      setCanManageEnv(caps.manageEnvVars);
      setCanComposeUnbound(caps.composeUnbound);
    });
  }, []);

  const { data: schedData } = useGet<unknown>(() => api.GET("/schedule-defs"), []);
  // CA — stored-credential labels for the "connect as" key picker.
  const credsQ = useGet<{ label?: string }[]>(() => api.GET("/ssh/credentials"), []);
  const credentialLabels = (credsQ.data ?? []).map((cr) => cr.label ?? "").filter(Boolean);
  const { data: scopesData } = useGet<unknown>(() => api.GET("/scopes"), []);
  const { data: envVarsData } = useGet<unknown>(() => api.GET("/env-vars"), []);
  // JC-P4 — secret metadata for the scope-context panel (keys only; the List
  // endpoint never returns values, and we never call reveal).
  const { data: secretsData } = useGet<unknown>(() => api.GET("/env-secrets"), []);

  const schedules = rows<Schedule>(schedData);
  const scopesList = rows<Scope>(scopesData);
  const envVarsList = rows<EnvVar>(envVarsData);
  const secretsList = rows<EnvSecret>(secretsData);
  // Secret KEYS for the become-password picker (RA-12). Names only — the list
  // endpoint never returns values and reveal is never called from here.
  const secretKeys = useMemo(
    () => secretsList.map((x) => x.key ?? "").filter(Boolean).sort((a, b) => a.localeCompare(b)),
    [secretsData],
  );

  const scopeSuggestions = useMemo(() => {
    const set = new Set<string>();
    scopesList.forEach((s) => {
      if (s.scope) set.add(s.scope);
    });
    envVarsList.forEach((ev) => {
      if (ev.scope) set.add(ev.scope);
    });
    return Array.from(set).sort();
  }, [scopesList, envVarsList]);

  const [name, setName] = useState("");
  // ?script= (Scripts → "Create Job") preseeds the picker by NAME; the picker
  // self-resolves it via GET /scripts/{name}, so no extra fetch is needed here.
  // Ignored when a whole job is being loaded — its scriptRef is authoritative.
  const [scriptRef, setScriptRef] = useState(loadId ? "" : (search.get("script") ?? ""));
  // JC17 — the resolved Script object for the current scriptRef, lifted from the
  // ScriptPicker (which owns the catalog load + the GET /scripts/{name} prefill so
  // an off-page or edit-loaded ref still resolves). Drives run-type/executor, the
  // lint indicator, and declared-variable hints below.
  const [selectedScript, setSelectedScript] = useState<Script | null>(null);
  // A3 — true when scriptRef points at a deleted/unknown script (the picker resolved
  // it to a 404). Soft-warns at the Script field; does not hard-block save.
  const [scriptMissing, setScriptMissing] = useState(false);
  // AF-1 — TRI-STATE, and that is the whole point: null = the author has not
  // chosen yet (create mode's starting state, and the only state that blocks
  // save), "" = All chosen deliberately, "Prod" = a named scope. Before this the
  // field defaulted to "" and an unanswered form silently produced a global job.
  const [scope, setScope] = useState<string | null>(null);
  const [targetHost, setTargetHost] = useState("");
  // CA Phase B — declarative "connect as" identity (username + credential LABEL).
  // The credential picker follows the reference-binding rule (CA-Q1): setting or
  // changing it needs ManageEnvVars server-side, so it renders disabled without.
  const [sshUser, setSshUser] = useState("");
  const [sshCredential, setSshCredential] = useState("");
  const [becomePasswordSecret, setBecomePasswordSecret] = useState("");
  const [executor, setExecutor] = useState("");
  // RT-3 — the DECLARED pin. The Composer edits the job SPEC, so this is the
  // git-equivalent field only; the operator OVERRIDE is not settable here on
  // purpose (RT-Q7) — this form is a full replace and would clobber it.
  const [runnerTag, setRunnerTag] = useState("");
  const { tags: runnerTags } = useRunnerTags();
  // EV-6 parity — the job's declared SSH-key bindings (labels only), promoted to
  // their own section beside Executor exactly as job detail promoted JobKeyField.
  // Draft-until-save like every other composer field: loaded from
  // /job-reference-bindings on edit, written back through putBindings AFTER the
  // job row saves (create mode has no jobId to write against until then).
  // loadedKeys is the server's set at load time, for dirty detection.
  const [sshKeys, setSshKeys] = useState<string[]>([]);
  // JP-4a — the job's declared SECRET/VARIABLE bindings, authored here now that
  // the Jobs expanded view shows them read-only. Same draft-until-save lifecycle
  // as sshKeys above: loaded from /job-reference-bindings on edit, written back
  // through putBindings after the job row exists. loadedRefs is the server's set
  // at load time, for dirty detection.
  // Both kinds share keyErr for the write outcome: they are one PUT to one
  // endpoint, so a failure is one failure and two messages would misreport it.
  const [refBindings, setRefBindings] = useState<ReferenceBinding[]>([]);
  const [loadedRefs, setLoadedRefs] = useState<ReferenceBinding[]>([]);
  // RX-15 — reactions are a SEPARATE resource (their own endpoint), so they load
  // and save alongside the job rather than inside its body, exactly like the SSH
  // key bindings below.
  const [reactionErr, setReactionErr] = useState<string | null>(null);
  const reactions = useLoadedReactions("job", name, isEdit);
  const [loadedKeys, setLoadedKeys] = useState<string[]>([]);
  const [keyErr, setKeyErr] = useState<string | null>(null);
  // JC10 — job-level env: applies to every run, beneath schedule + per-run override.
  const [jobEnvRows, setJobEnvRows] = useState<EnvKV[]>([]);
  // UDV1 — declared prompt variables: surfaced as fillable fields in the ad-hoc Run dialog.
  const [jobPrompts, setJobPrompts] = useState<PromptRow[]>([]);
  const [scheduleRefs, setScheduleRefs] = useState<string[]>([]);
  const [inlineSchedules, setInlineSchedules] = useState<InlineSchedule[]>([]);
  const [enabled, setEnabled] = useState(true);
  const [description, setDescription] = useState("");
  // JC-P1/JC-P2 — the advanced job-definition fields, all exposed in the Advanced
  // disclosure. `requestable` joined them in ET-B: it was preserved-but-unexposed
  // (JC7) while nothing enforced it, and hiding a control that now gates the
  // trigger API would leave operators unable to open a job to their integrations.
  // Held here and always re-sent so the full-replace PUT preserves rather than
  // strips them.
  const [preserved, setPreserved] = useState<Preserved>(DEFAULT_PRESERVED);
  // Raw mirror of preserved.tags so the comma-separated input edits smoothly.
  const [tagsInput, setTagsInput] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [okMsg, setOkMsg] = useState<string | null>(null);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  // Set when the loaded job is git-sourced — the form is replaced by a read-only
  // notice (the backend 409s the PUT as a backstop).
  const [gitSource, setGitSource] = useState(false);
  const [pendingDelete, setPendingDelete] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [delErr, setDelErr] = useState<string | null>(null);
  // RX-24 — the server's refusal when reactions watch this job. Non-null swaps
  // the confirm control for a forced one; it holds the message because that
  // message is the list of reactions the operator has to decide about.
  const [delBlock, setDelBlock] = useState<string | null>(null);

  // Edit/clone mode: load the existing amadeus job into the form. A custom
  // (non-suggestion) scope round-trips as-is via the creatable ScopePicker (JC23),
  // so setScope alone suffices — no manual-mode flip, hence [loadId] is the only
  // dep. Clone differs from edit in exactly two lines here: the name gets a
  // "-copy" suffix (it must differ — name is the identity), and loadedKeys stays
  // empty so the carried-over SSH keys count as dirty and are written to the NEW
  // job after the POST.
  useEffect(() => {
    if (!loadId) return;
    let cancelled = false;
    (async () => {
      const { data, error } = await api.GET("/jobs/{jobId}", {
        params: { path: { jobId: Number(loadId) } },
      });
      if (cancelled) return;
      if (error || !data) {
        setLoadErr(`Could not load job "${loadId}".`);
        return;
      }
      const j = data as Job;
      if (j.source !== "amadeus") {
        setGitSource(true);
        return;
      }
      setName(cloneId ? `${j.name ?? ""}-copy` : (j.name ?? ""));
      setScriptRef(j.scriptRef ?? "");
      // JC23 — a custom (non-suggestion) scope round-trips as-is; the creatable
      // ScopePicker renders it whether or not it's a known suggestion.
      setScope(j.scope ?? "");
      setTargetHost(j.host ?? "");
      setSshUser(j.sshUser ?? "");
      setSshCredential(j.sshCredential ?? "");
      setBecomePasswordSecret(j.becomePasswordSecret ?? "");
      setExecutor(j.executor ?? "");
      setRunnerTag(j.runnerTag ?? "");
      // JC10 — prefill job-level env for an exact round-trip (full-state resend).
      setJobEnvRows(Object.entries(j.env ?? {}).map(([key, value]) => ({ key, value })));
      // UDV1 — prefill declared prompts for an exact round-trip (full-state resend).
      setJobPrompts(promptWireToRows(j.prompts));
      setDescription(j.description ?? "");
      // JC-P3/JC8 — split the expanded entries by sourceRef: ref-expanded entries
      // (sourceRef set) re-bind as scheduleRefs; inline entries (sourceRef null)
      // repopulate the inline editor with their cron + env, so editing an inline-
      // authored job no longer flattens its schedules into (unresolvable) refs.
      const entries = j.schedules ?? [];
      setScheduleRefs(entries.filter((s) => !!s.sourceRef).map((s) => s.sourceRef as string));
      setInlineSchedules(
        entries
          .filter((s) => !s.sourceRef)
          .map((s) => ({
            name: s.name ?? "",
            cron: s.cron ?? "",
            env: Object.entries(s.env ?? {}).map(([key, value]) => ({ key, value })),
            startAt: s.startAt ?? null,
            endAt: s.endAt ?? null,
            interval: s.interval ?? null,
            skipCalendars: s.skipCalendars ?? [],
            onlyCalendars: s.onlyCalendars ?? [],
          })),
      );
      // JC9 — prefill from the raw `enabled` flag (exact); the derived `status` is a
      // fallback only for an older backend that doesn't return it. Fixes the D1
      // lossiness where a schedule-paused-but-enabled job prefilled as disabled.
      setEnabled(j.enabled ?? j.status !== "paused");
      // JC-P1 — capture the advanced fields verbatim for a lossless round-trip.
      // (Inline-vs-ref schedule bucketing is deferred to JC-P3, which consumes the
      // JC-P8 `sourceRef` field now exposed on each schedule entry.)
      setPreserved({
        timeoutSeconds: j.timeoutSeconds ?? 0,
        retries: j.retries ?? 0,
        backoffSeconds: j.backoffSeconds ?? 0,
        continueOnError: !!j.continueOnError,
        concurrencyPolicy: (j.concurrencyPolicy as ConcurrencyPolicy | undefined) ?? "Allow",
        concurrencyKey: j.concurrencyKey ?? "",
        promptEnforcement: j.promptEnforcement === "block" ? "block" : "warn",
        tags: j.tags ?? [],
        requestable: !!j.requestable,
        warnAfterSeconds: j.warnAfterSeconds ?? 0,
        mustFinishBy: j.mustFinishBy ?? "",
        watch: j.watch ?? [],
        envPassthrough: j.envPassthrough ?? [],
      });
      setTagsInput((j.tags ?? []).join(", "));
      // EV-6 parity — prefill the declared key bindings (labels only; the derived
      // AMADEUS_KEY_ form is re-derived at render and on write).
      const kb = await api.GET("/job-reference-bindings/{jobId}", {
        params: { path: { jobId: Number(loadId) } },
      });
      if (cancelled) return;
      const loaded = (kb.data as { bindings?: ReferenceBinding[] } | undefined)?.bindings ?? [];
      const ks = loaded.filter((b) => b.kind === "key" && b.name).map((b) => b.name as string);
      setSshKeys(ks);
      // JP-4a — the secret/var half of the same fetch. It used to be discarded
      // here and preserved blind at save; the composer owns it now.
      const refs = loaded.filter((b) => (b.kind === "secret" || b.kind === "var") && b.name).map((b) => ({ ...b }));
      setRefBindings(refs);
      // Clone: baseline stays [] so the keys read as dirty and get written to
      // the new job once it exists (the create path in submit()).
      if (!cloneId) {
        setLoadedKeys(ks);
        setLoadedRefs(refs);
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [loadId]);

  function toggleRef(n: string) {
    setScheduleRefs((cur) => (cur.includes(n) ? cur.filter((x) => x !== n) : [...cur, n]));
  }

  // JC-P2 — advanced-field helpers. setP patches one preserved field; toNonNeg
  // clamps a numeric input to a non-negative int (the backend has no CHECK).
  const setP = (patch: Partial<Preserved>) => setPreserved((p) => ({ ...p, ...patch }));
  const toNonNeg = (s: string) => {
    const n = parseInt(s, 10);
    return Number.isFinite(n) && n > 0 ? n : 0;
  };

  // JC-P3 — inline-schedule mutation helpers + derived per-row name errors (slug /
  // duplicate) and whether any inline env exists (drives the plaintext caveat).
  const updateInline = (i: number, patch: Partial<InlineSchedule>) =>
    setInlineSchedules((es) => es.map((e, j) => (j === i ? { ...e, ...patch } : e)));
  const addInline = () =>
    setInlineSchedules((es) => [...es, { name: `schedule-${es.length + 1}`, cron: "0 7 * * *", env: [] }]);
  const removeInline = (i: number) => setInlineSchedules((es) => es.filter((_, j) => j !== i));
  const inlineErrors = inlineSchedules.map((e) => {
    const nm = e.name.trim();
    if (!nm) return null;
    if (!ENTRY_NAME_RE.test(nm)) return "name must be a slug (a-z, 0-9, _-)";
    if (inlineSchedules.filter((x) => x.name.trim().toLowerCase() === nm.toLowerCase()).length > 1) return "duplicate name";
    return null;
  });
  const inlineHasEnv = inlineSchedules.some((e) => e.env.some((r) => r.key.trim() !== ""));

  // JC-P5 — run-type / executor / scope-capability awareness from the selected script
  // and scope. Capability is advisory (warn, never block); the SSH lock is hard
  // (the backend 422s ssh × ansible/terraform at run time). selectedScript is
  // resolved by the ScriptPicker (state above), so it works for off-page scripts too.
  const runType = selectedScript?.runType;
  const runnerOnly = isRunnerOnly(runType);
  // RP-10 — mirrors the server's execspec.IdentityCapableRunType: ssh-family and
  // ansible may carry a declared "connect as"; terraform 422s.
  const identityCapable = isIdentityCapable(runType);
  const chosenScope = scopesList.find((s) => s.scope === scope);
  const chosenTypes = chosenScope?.capability?.types;
  const scopeIncompatible = !!(chosenScope && runType && chosenTypes && chosenTypes.length > 0 && !chosenTypes.includes(runType));
  const scopeHosts = chosenScope?.hosts ?? [];

  // JC-P4 — env context for the chosen scope. '*'/empty scope = a global (mirrors the
  // backend redactor's `scope = ? OR scope = '*'`). injectedKeys = the env actually
  // put on the process at run time (job-level env + the firing schedule's env,
  // inline + ref; schedule wins on key collision).
  const inScope = (s?: string) => s === scope || s === "*" || !s;
  const scopeVars = scope ? envVarsList.filter((v) => inScope(v.scope)) : [];
  const scopeSecrets = scope ? secretsList.filter((s) => inScope(s.scope)) : [];
  const injectedKeys = Array.from(
    new Set([
      ...jobEnvRows.map((r) => r.key.trim()).filter(Boolean),
      ...scheduleRefs.flatMap((n) => Object.keys(schedules.find((s) => s.name === n)?.env ?? {})),
      ...inlineSchedules.flatMap((e) => e.env.map((r) => r.key.trim()).filter(Boolean)),
    ]),
  );
  // JC — variable hints: the selected script declares which env vars it reads. A
  // var is "satisfied" when it's already injected (job-level or schedule env) or
  // provided by a defined Env Var / Secret in the chosen scope; addJobVar wires a
  // missing one into the job-level env editor, seeded with the script's default.
  const satisfiedVars = new Set<string>([
    ...injectedKeys,
    ...scopeVars.map((v) => v.key ?? "").filter(Boolean),
    ...scopeSecrets.map((s) => s.key ?? "").filter(Boolean),
  ]);
  const addJobVar = (name: string, def: string) =>
    setJobEnvRows((prev) => (prev.some((r) => r.key.trim() === name) ? prev : [...prev, { key: name, value: def }]));

  // UDV5 — seed declared prompts from the script's detected variables (the inferred
  // extractor repurposed as a curation aid). Proposes a row per referenced variable
  // not already declared; the author then curates (label, required, drop). A required
  // flag is seeded from the script's optionality (no ${VAR:-default} ⇒ required).
  // Nothing persists until the form is saved — the human review makes the heuristic safe.
  const seedPromptsFromScript = () => {
    const declared = new Set(jobPrompts.map((p) => p.name.trim()).filter(Boolean));
    const add: PromptRow[] = (selectedScript?.variables ?? [])
      .filter((v) => v.name && !declared.has(v.name))
      .map((v) => ({ name: v.name as string, label: "", required: v.default == null, default: v.default ?? "", options: "" }));
    if (add.length) setJobPrompts((prev) => [...prev, ...add]);
  };

  // T2.9/JR-Q6 — auto-seed the job's Run inputs from what the SCRIPT declares.
  //
  // This is the root-cause fix for run inputs going undeclared: the admin composing a
  // job from someone else's script inherits the author's required set by default,
  // instead of it depending on them noticing the import button. Declared beats inferred
  // (UDV1) — the heuristic seed above stays as the manual fallback for scripts that
  // declare nothing.
  //
  // Deliberately conservative (never clobber): seeds only when the list is EMPTY, and
  // only once per script selection. An author who has already edited the list — or
  // cleared it on purpose — keeps what they have. Editing an existing job is untouched
  // because its saved prompts populate the list before this can fire.
  const seededFor = useRef<string | null>(null);
  useEffect(() => {
    const scriptName = selectedScript?.name ?? null;
    if (!scriptName || seededFor.current === scriptName) return;
    seededFor.current = scriptName;
    const declared = selectedScript?.prompts ?? [];
    if (declared.length === 0) return;
    setJobPrompts((prev) => (prev.length > 0 ? prev : promptWireToRows(declared)));
  }, [selectedScript]);

  // UDV5 — drift lint: script-referenced variables that nothing declares or provides.
  // Advisory only; never blocks a save. Covered = a declared prompt, an injected
  // job/schedule env key, or a scope Env Var/Secret (satisfiedVars).
  const promptNames = new Set(jobPrompts.map((p) => p.name.trim()).filter(Boolean));
  const driftVars = (selectedScript?.variables ?? [])
    .map((v) => v.name)
    .filter((n): n is string => !!n && !promptNames.has(n) && !satisfiedVars.has(n));

  // T1.11 — a job-level env row with a blank value is almost always a mis-modeled run
  // input: the author meant "someone must supply this" but the job-level editor has no
  // required flag, so it silently ships an empty string to every run and the operator is
  // never asked. Advisory only; never blocks a save.
  const blankEnvRows = jobEnvRows
    .filter((r) => r.key.trim() !== "" && r.value.trim() === "" && !promptNames.has(r.key.trim()))
    .map((r) => r.key.trim());
  // One-click remedy: move the blank row out of job-level env and declare it as a
  // required run input instead.
  const convertToRunInput = (key: string) => {
    setJobEnvRows((prev) => prev.filter((r) => r.key.trim() !== key));
    setJobPrompts((prev) =>
      prev.some((p) => p.name.trim() === key) ? prev : [...prev, { name: key, label: "", required: true, default: "", options: "" }],
    );
  };

  // T3.6/G7 — a required run input on a SCHEDULED job. Schedules, workflow steps, and
  // the API all bypass the Run dialog, so nobody is there to answer at fire time: the
  // input will be recorded unfilled on every single fire (or, on a `block` job, the run
  // is refused outright). This is only catchable at compose time, which is here.
  //
  // Satisfied = the input has a default, or the job-level env supplies it, or the
  // firing schedule's own env does. Advisory; never blocks a save.
  const hasSchedule = scheduleRefs.length > 0 || inlineSchedules.some((s) => s.name.trim() && s.cron.trim());
  const scheduleEnvKeys = new Set(
    inlineSchedules.flatMap((s) => s.env.map((e) => e.key.trim()).filter(Boolean)),
  );
  const jobEnvKeys = new Set(
    jobEnvRows.filter((r) => r.value.trim() !== "").map((r) => r.key.trim()).filter(Boolean),
  );
  const unschedulableInputs = !hasSchedule
    ? []
    : jobPrompts
        .filter(
          (p) =>
            p.required &&
            p.name.trim() !== "" &&
            p.default.trim() === "" &&
            !jobEnvKeys.has(p.name.trim()) &&
            !scheduleEnvKeys.has(p.name.trim()),
        )
        .map((p) => p.name.trim());

  // T7.2 — credential-shaped run-input names. Answers ride the plaintext per-run env
  // override path, so a token/password asked here is stored and transmitted in the
  // clear; the Secret Store is the correct home. Advisory, name-based, authoring-time.
  const CREDENTIALISH = /(^|_)(TOKEN|PASSWORD|PASSWD|SECRET|APIKEY|API_KEY|PRIVATE_KEY|CREDENTIAL|PAT)S?$/i;
  const credentialishPrompts = jobPrompts
    .map((p) => p.name.trim())
    .filter((n) => n !== "" && CREDENTIALISH.test(n));

  // JC-P7 — count of complete inline schedules, for the effective-binding preview.
  const nInlineValid = inlineSchedules.filter((s) => s.name.trim() && s.cron.trim()).length;

  // JC-P5 — runner-only run-types can't use SSH; drop a stale ssh override to runner.
  useEffect(() => {
    if (runnerOnly && executor === "ssh") setExecutor("runner");
  }, [runnerOnly, executor]);

  // RP-10 — same shape for identity: re-binding the job to a script whose run
  // type can't carry one (terraform) hides the fields, and a hidden value would
  // otherwise still be submitted and 422 at save with no visible cause.
  useEffect(() => {
    if (!identityCapable) {
      setSshUser("");
      setSshCredential("");
    }
  }, [identityCapable]);

  // RX-24 — `force` re-issues the same delete past the reactions guard. The
  // guard's 409 is told apart by its error code, so the confirm row can offer
  // the second choice instead of dead-ending on a refusal the API can clear.
  async function del(force = false) {
    if (!editId) return;
    setDelErr(null);
    setDeleting(true);
    const { response, error } = await api.DELETE("/jobs/{jobId}", {
      params: { path: { jobId: Number(editId) }, query: force ? { force: true } : {} },
    });
    setDeleting(false);
    if (error || !response.ok) {
      const watching = force ? null : reactionsWatchingRefusal(error);
      if (watching) {
        setDelBlock(watching);
        setDelErr(null);
        return; // stay in the confirm row, now offering the forced delete
      }
      setDelErr(
        errMessage(error) ||
          (response.status === 409
            ? "Only amadeus-source jobs can be deleted in-app."
            : `Delete failed (${response.status}).`),
      );
      setPendingDelete(false);
      setDelBlock(null);
      return;
    }
    navigate("/jobs", { state: { toast: `Job "${name.trim() || editId}" deleted.` } });
  }

  async function submit() {
    setErr(null);
    setOkMsg(null);
    if (!name.trim()) return setErr("Name is required.");
    if (!scriptRef) return setErr("Pick a script.");
    // JC-P3 — validate + assemble inline schedules. A row with only a name or only a
    // cron is incomplete; a complete row needs a slug name + valid cron, names unique.
    // Empty rows are dropped. (The backend re-validates; this is the early UX guard.)
    const inlineClean = inlineSchedules.map((s) => ({
      name: s.name.trim(),
      cron: s.cron.trim(),
      env: s.env,
      startAt: s.startAt ?? null,
      endAt: s.endAt ?? null,
      interval: s.interval?.trim() ? s.interval.trim() : null,
      skipCalendars: s.skipCalendars ?? [],
      onlyCalendars: s.onlyCalendars ?? [],
    }));
    // An entry counts as authored once it carries a name or ANY firing rule —
    // interval and one-shot rows have no cron, so keying off cron alone would
    // silently drop them.
    const inlineEntries = inlineClean.filter((s) => s.name || s.cron || s.interval || s.startAt);
    for (const s of inlineEntries) {
      if (!s.name) return setErr("Each inline schedule needs a name.");
      if (!ENTRY_NAME_RE.test(s.name)) return setErr(`Inline schedule "${s.name}": name must be a slug (a-z, 0-9, _-).`);
      const specErr = validateSpec(s);
      if (specErr) return setErr(`Inline schedule "${s.name}": ${specErr.toLowerCase()}.`);
      const calErr = bindingError(s);
      if (calErr) return setErr(`Inline schedule "${s.name}": ${calErr}`);
    }
    const inlineNames = inlineEntries.map((s) => s.name.toLowerCase());
    if (new Set(inlineNames).size !== inlineNames.length) return setErr("Inline schedule names must be unique.");
    // AF-1 — the choice must be made, and only silence is refused. Client-side so
    // the author is told in the form rather than by a 422 after a full round-trip;
    // the server enforces the same rule for every other client.
    if (scope == null) {
      return setErr(
        'Pick a scope — it decides which agencies can see this job. Choose "All agencies (global)" if it should be visible to everyone.',
      );
    }
    setBusy(true);
    const body: Record<string, unknown> = {
      name: name.trim(),
      scriptRef,
      // AF-1 — always a string by here: submit() refuses to run with scope null
      // (the guard above), and the server 422s an absent field as a backstop.
      scope: (scope ?? "").trim(),
      targetHost: targetHost.trim(),
      // CA — always sent (even "") so the full-replace PUT clears a removed
      // identity rather than silently keeping it.
      sshUser: sshUser.trim(),
      sshCredential: sshCredential.trim(),
      // RA-12/RA-21 — a NAME, never a value. Sent only for ansible: the server 422s
      // it on any other run type, and a field left set while the run type changes
      // would turn an unrelated edit into a rejected save.
      becomePasswordSecret: runType === "ansible" ? becomePasswordSecret.trim() : "",
      // JC10 — job-level env; always sent (even {}) so the full-replace PUT preserves
      // it under the JC1 resend rather than stripping it. envRowsToMap trims empty keys.
      env: envRowsToMap(jobEnvRows),
      // UDV1 — declared prompts; always sent (even []) so the full-replace PUT
      // preserves them rather than stripping. promptRowsToWire drops empty-name rows.
      prompts: promptRowsToWire(jobPrompts),
      scheduleRefs,
      // JC-P3 — inline (job-local) schedules alongside the reusable scheduleRefs.
      // CAL-23 — through the typed wire mapping, never an ad-hoc object literal:
      // this line used to drop startAt/endAt/interval on every save.
      schedules: inlineEntries.map(inlineScheduleToWire),
      enabled,
      description: description.trim(),
      // RT-3 — the declared pin rides the same full-replace PUT as every other
      // spec field, so clearing the input clears the pin. This is one of the two
      // places a pin is authored; the other is the Run dialog, for one run.
      runnerTag: runnerTag.trim(),
      // JC-P1 — always re-send the advanced fields so the full-replace PUT preserves
      // them. On create these are the backend defaults, so it is behavior-neutral.
      timeoutSeconds: preserved.timeoutSeconds,
      retries: preserved.retries,
      backoffSeconds: preserved.backoffSeconds,
      continueOnError: preserved.continueOnError,
      concurrencyPolicy: preserved.concurrencyPolicy,
      concurrencyKey: preserved.concurrencyKey,
      tags: preserved.tags,
      requestable: preserved.requestable,
      warnAfterSeconds: preserved.warnAfterSeconds,
      mustFinishBy: preserved.mustFinishBy,
      watch: preserved.watch,
      promptEnforcement: preserved.promptEnforcement,
      // RP-14 — only sent for local-toolchain types; the server 422s it on an
      // ssh-family job, whose remote env is built entirely from the manifest.
      envPassthrough: runnerOnly ? preserved.envPassthrough : [],
    };
    if (executor) body.executor = executor;
    setKeyErr(null);
    // PUT/DELETE /jobs/{jobId} declare `header?: never` — the csrf middleware
    // injects the token, so pass NO header placeholder (only path + body).
    const { data, response, error } = isEdit
      ? await api.PUT("/jobs/{jobId}", { params: { path: { jobId: Number(editId) } }, body: body as never })
      : await api.POST("/jobs", { body: body as never });
    if (error || !response.ok) {
      setBusy(false);
      setErr(errMessage(error) || `${isEdit ? "Save" : "Create"} failed (${response.status}).`);
      return;
    }
    // EV-6 parity — persist the SSH-key bindings once the job row exists (create
    // learns the id from the response). putBindings preserves the secret/var kinds
    // this form does not show. Skipped when unchanged, so a composer save never
    // needs ManageEnvVars unless keys were actually edited.
    // RX-15 — persist the reactions once the job row exists, same shape as the
    // key bindings below (and the same honest failure message: the job saved,
    // its reactions did not, and here is where to fix them).
    if (reactions.dirty) {
      const rerr = await saveReactionsFor("job", name.trim(), reactions.list);
      if (rerr) {
        setReactionErr(`The job was ${isEdit ? "saved" : "created"}, but its reactions were not: ${rerr}`);
      } else {
        setReactionErr(null);
        reactions.setBaseline(reactions.list);
      }
    }

    // JP-4a — keys AND secrets/variables are one binding set on one endpoint, and
    // the composer now owns all three kinds, so they go in a SINGLE putBindings
    // call. (Two sequential writes would each re-read and re-merge, making the
    // second silently authoritative over the first for any kind both touched.)
    // Still skipped when nothing changed, so a composer save needs ManageEnvVars
    // only when bindings were actually edited. putBindings keeps its
    // preserve-other-kinds merge — it is shared with the Scripts editor and
    // JobKeyField, and its fresh-read-abort is what stops a failed re-read from
    // deleting the kinds we did not send.
    const refKey = (b: ReferenceBinding) => `${b.kind} ${b.name} ${b.as ?? ""}`;
    const keysDirty = [...sshKeys].sort().join("|") !== [...loadedKeys].sort().join("|");
    const refsDirty =
      refBindings.map(refKey).sort().join("|") !== loadedRefs.map(refKey).sort().join("|");
    if (keysDirty || refsDirty) {
      const jobId = isEdit ? Number(editId) : (data as Job | undefined)?.id;
      const mine: ReferenceBinding[] = [
        ...sshKeys.map((n) => ({ kind: "key" as const, name: n, reference: `AMADEUS_KEY_${n}` })),
        ...refBindings,
      ];
      const kerr =
        jobId == null
          ? "could not resolve the new job's id"
          : await putBindings({ job: jobId }, mine, (b) => b.kind === "key" || b.kind === "secret" || b.kind === "var");
      if (kerr) {
        setKeyErr(
          `The job was ${isEdit ? "saved" : "created"}, but its secrets, variables and SSH keys were not: ${kerr}. They can be re-applied by editing the job again.`,
        );
      } else {
        setKeyErr(null);
        setLoadedKeys(sshKeys);
        setLoadedRefs(refBindings);
      }
    }
    setBusy(false);
    setOkMsg(`Job "${name.trim()}" ${isEdit ? "updated" : "created"} (amadeus-source).`);
    // Keep the form populated after an edit; only reset on create.
    if (isEdit) return;
    setName("");
    setScriptRef("");
    setScope("");
    setTargetHost("");
    setSshUser("");
    setSshCredential("");
    setExecutor("");
    setRunnerTag("");
    setSshKeys([]);
    setLoadedKeys([]);
    setScheduleRefs([]);
    setInlineSchedules([]);
    setDescription("");
    setPreserved(DEFAULT_PRESERVED);
    setTagsInput("");
  }

  if (canCompose === false) {
    return (
      <div style={{ color: c.textSec, maxWidth: 560 }}>
        In-app job composition requires the <strong>Compose</strong> capability (Admin-only in v20).
        Git-defined jobs are authored through the GitLab publish flow.
      </div>
    );
  }

  if (loadErr) {
    return <div style={{ color: c.danger, maxWidth: 560 }}>{loadErr}</div>;
  }

  if (gitSource) {
    return (
      <div style={{ color: c.textSec, maxWidth: 560 }}>
        This job is <strong>Git-authored</strong> — read-only here. Edit it through the GitLab publish
        flow; only amadeus-source jobs are editable in-app.
      </div>
    );
  }

  return (
    // Centered, carded form column (VC.12) — matches the Settings container
    // language instead of a hard-left column against bare page background.
    <div
      style={{
        maxWidth: 720,
        margin: "0 auto",
        width: "100%",
        display: "flex",
        flexDirection: "column",
        gap: 16,
        background: c.panel,
        border: `1px solid ${c.border}`,
        borderRadius: c.radiusSurface,
        // VU-5: border or shadow, never both — the form column is in page flow,
        // so the border stays and the shadow goes.
        padding: 24,
      }}
    >
      <div style={{ color: c.textSec, fontSize: c.fontSm }}>
        {isEdit ? (
          <>
            Editing the <strong>amadeus-source</strong> job <span style={{ fontFamily: c.mono }}>{name}</span>.
            Saving rebinds its script, schedules, scope, and execution options. It runs through the same
            engine as a Git job — only its origin differs.
          </>
        ) : cloneId ? (
          <>
            Cloning an existing job into a <strong>new amadeus-source</strong> job: every setting is
            prefilled from the original — adjust what differs (starting with the name) and create it.
            The original job is not touched.
          </>
        ) : (
          <>
            Compose an <strong>amadeus-source</strong> job: bind a Git script to schedules, a scope, and
            execution options. It runs through the same engine as a Git job — only its origin differs.
          </>
        )}
      </div>

      <Field label="Name">
        <Input
          style={{ ...input(), opacity: isEdit ? 0.6 : 1 }}
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="nightly-backup"
          disabled={isEdit}
        />
        {isEdit && <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 4 }}>Name is the identity — immutable on edit.</div>}
      </Field>

      <Field label="Script">
        {/* JC17 — searchable, folder-grouped Script picker (replaces the flat <select>). */}
        <ScriptPicker value={scriptRef} onChange={setScriptRef} onResolved={setSelectedScript} onMissing={setScriptMissing} style={input()} />
        {/* JC-P9 — cross-link to the (Git-only) Scripts catalog + surface lint warnings. */}
        <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 4 }}>
          Scripts are Git-defined (read-only).{" "}
          <button type="button" onClick={() => navigate("/scripts")} style={linkBtn()}>
            Browse the Scripts catalog →
          </button>
          {(() => {
            const g = guideForRunType(selectedScript?.runType);
            return g ? (
              <span style={{ marginLeft: 6 }}>
                &middot; <DocLink href={g.href}>{g.label}</DocLink>
              </span>
            ) : null;
          })()}
          {selectedScript?.warnings && selectedScript.warnings.length > 0 && (
            <span style={{ color: c.warning, marginLeft: 6 }}>
              ⚠ {selectedScript.warnings.length} lint warning{selectedScript.warnings.length === 1 ? "" : "s"}
            </span>
          )}
        </div>
        {/* A3 — dangling-ref soft warning: the saved scriptRef no longer resolves. Does
            not block save (the backend is the hard gate); just flags it in context. */}
        {scriptMissing && (
          <div style={{ fontSize: c.fontXs, color: c.warning, marginTop: 4 }}>
            ⚠ “{scriptRef}” isn’t in the Scripts catalog — saving will reference a missing script.
          </div>
        )}
      </Field>

      <div style={{ display: "flex", gap: 12 }}>
        <Field label="Scope" style={{ flex: 1 }}>
          {/* JC22/JC23 — searchable, creatable scope combobox (replaces the select/manual toggle).
              AF-1 — with an explicit All option, because the scope is what decides
              which agencies see this job and that must be answered, not defaulted. */}
          <ScopePicker
            value={scope}
            onChange={setScope}
            suggestions={scopeSuggestions}
            scopes={scopesList}
            style={input()}
            allowAll={canComposeUnbound}
          />
          {scope == null && (
            <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4 }}>
              Decides which agencies can see this job.{" "}
              {canComposeUnbound ? (
                <>
                  Pick <strong>All agencies (global)</strong> if everyone should.
                </>
              ) : (
                <>Pick one of the scopes your role is granted.</>
              )}
            </div>
          )}
          {scope === "" && (
            <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4 }}>
              Visible to every agency, and runnable by anyone who may trigger jobs.
            </div>
          )}
          {/* AF-Q3 — advisory, never blocking: a scope that maps to no agency is
              fail-CLOSED (only unrestricted admins see the job), so nothing is at
              risk — but it is almost always an unfinished scope→agency mapping
              rather than an intent, and Settings is where it gets fixed. Only
              raised for a scope we actually know: a typed custom scope has no row
              to read agencies from, and warning about it would be guessing. */}
          {scope != null && scope !== "" && chosenScope && (chosenScope.agencies ?? []).length === 0 && (
            <div style={{ fontSize: c.fontXs, color: c.warning, marginTop: 4 }}>
              ⚠ <code style={{ fontFamily: c.mono }}>{scope}</code> is not mapped to any agency, so only unrestricted
              admins will see this job. Map it under Settings → Scopes to give a department access.
            </div>
          )}
        </Field>
        <Field label="Target host (optional)" style={{ flex: 1 }}>
          {scopeHosts.length > 0 ? (
            <Select style={input()} value={targetHost} onChange={(e) => setTargetHost(e.target.value)}>
              <option value="">— all hosts in {scope} —</option>
              {scopeHosts.map((h) => (
                <option key={h} value={h}>
                  {h}
                </option>
              ))}
              {targetHost && !scopeHosts.includes(targetHost) && (
                <option value={targetHost}>{targetHost} (not in scope)</option>
              )}
            </Select>
          ) : (
            <Input style={input()} value={targetHost} onChange={(e) => setTargetHost(e.target.value)} placeholder="host-01" />
          )}
        </Field>
      </div>

      {/* CA Phase B — declarative "connect as" identity. RP-10: offered for
          ssh-family AND ansible jobs (ansible applies it as connection
          extra-vars, which beat the inventory); terraform authenticates through
          its providers, so the server 422s it and the block stays hidden. */}
      {identityCapable && (
        <div style={{ display: "flex", gap: 12 }}>
          <Field label="Connect as (optional)" style={{ flex: 1 }}>
            <Input
              style={input()}
              value={sshUser}
              onChange={(e) => setSshUser(e.target.value)}
              placeholder={runType === "ansible" ? "Inventory default user" : "Per-host default user"}
            />
          </Field>
          <Field label="SSH key (optional)" style={{ flex: 1 }}>
            <Select
              style={{ ...input(), cursor: canManageEnv ? "pointer" : "not-allowed", opacity: canManageEnv ? 1 : 0.6 }}
              value={sshCredential}
              onChange={(e) => setSshCredential(e.target.value)}
              disabled={!canManageEnv}
              title={canManageEnv ? undefined : "Binding an SSH key to a job requires the Manage Env Vars permission."}
            >
              <option value="">{runType === "ansible" ? "— inventory default key —" : "— per-host default key —"}</option>
              {credentialLabels.map((label) => (
                <option key={label} value={label}>
                  {label}
                </option>
              ))}
              {sshCredential && !credentialLabels.includes(sshCredential) && (
                <option value={sshCredential}>{sshCredential} (missing)</option>
              )}
            </Select>
          </Field>
        </div>
      )}
      {/* RA-12 / RA-21 — the become password. Ansible only: the flag reaches
          ansible-playbook alone, and the server refuses it on any other run type
          rather than storing something silently inert.

          A NAME, never a value — the password stays in the Secrets catalogue with
          every control that implies, and the agent receives it as a 0600 file it
          wipes at run end, never as an environment variable. Setting it is a GRANT
          over stored secret material, so it carries the same Manage Env Vars rule
          as the SSH key above (and the same departmental check server-side: you
          cannot bind another department's password). */}
      {runType === "ansible" && (
        <Field label="Become password (optional)">
          <Select
            style={{ ...input(), cursor: canManageEnv ? "pointer" : "not-allowed", opacity: canManageEnv ? 1 : 0.6 }}
            value={becomePasswordSecret}
            onChange={(e) => setBecomePasswordSecret(e.target.value)}
            disabled={!canManageEnv}
            title={canManageEnv ? undefined : "Binding a become password to a job requires the Manage Env Vars permission."}
          >
            <option value="">&mdash; none (passwordless sudo) &mdash;</option>
            {secretKeys.map((k) => (
              <option key={k} value={k}>
                {k}
              </option>
            ))}
            {becomePasswordSecret && !secretKeys.includes(becomePasswordSecret) && (
              <option value={becomePasswordSecret}>{becomePasswordSecret} (missing)</option>
            )}
          </Select>
        </Field>
      )}
      {runType === "ansible" && becomePasswordSecret && (
        <div style={{ fontSize: c.fontXs, color: c.textSec }}>
          Runs of this job get <code style={{ fontFamily: c.mono }}>--become-password-file</code> pointing at a private
          file the agent writes and wipes; the value never enters the run environment. The job will wait for a runner with
          ansible-core&nbsp;&ge;&nbsp;2.12 rather than run without escalation.{" "}
          <strong>Passwordless sudo is still preferred</strong> where the target's policy allows it.
        </div>
      )}

      {identityCapable && (sshUser.trim() || sshCredential) && (
        <div style={{ fontSize: c.fontXs, color: c.textSec }}>
          Every run of this job connects as <strong>{sshUser.trim() || "each host's configured user"}</strong> with{" "}
          <strong>{sshCredential ? `the ${sshCredential} key` : "each host's configured key"}</strong>; the Run dialog can
          still override either per run.{" "}
          {runType === "ansible" ? (
            <>
              This overrides <code style={{ fontFamily: c.mono }}>ansible_user</code>
              {sshCredential ? (
                <> and <code style={{ fontFamily: c.mono }}>ansible_ssh_private_key_file</code></>
              ) : null}{" "}
              from the scope's inventory on every host.
            </>
          ) : (
            <>Bastion hops are unaffected.</>
          )}
        </div>
      )}

      {scopeIncompatible && (
        <div style={{ fontSize: c.fontSm, color: c.warning, background: c.warningBg, border: `1px solid ${c.warning}30`, borderRadius: c.radiusSurface, padding: "8px 10px" }}>
          ⚠ Scope <strong>{scope}</strong> doesn't declare <strong>{runType}</strong> support. The job is allowed, but runs may stay queued waiting for a capable runner.
        </div>
      )}

      {/* Run-dialog parity — the same ExecutorChoice cards the Run modal uses,
          plus the composer's third option: no override, resolved from the script. */}
      <Field label="Executor">
        <div style={{ display: "flex", gap: 8 }}>
          <ExecutorChoice
            label="Auto"
            sub="From script / run type"
            selected={executor === ""}
            onClick={() => setExecutor("")}
          />
          <ExecutorChoice
            label="SSH"
            sub="In-app SSH"
            selected={executor === "ssh"}
            disabled={runnerOnly}
            title={runnerOnly ? `SSH can't run ${runType} — it needs a runner with the local ${runType} toolchain.` : undefined}
            onClick={() => setExecutor("ssh")}
          />
          <ExecutorChoice
            label="Runner"
            sub="Runner agent"
            selected={executor === "runner"}
            onClick={() => setExecutor("runner")}
          />
        </div>
        <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 6 }}>
          {runnerOnly ? (
            <>
              <strong>{runType}</strong> needs a runner with the local toolchain — SSH can't run it.
            </>
          ) : executor === "" ? (
            <>Resolved from the script at trigger time — shell run-types default to SSH.</>
          ) : (
            <>Will run via <strong>{executor === "ssh" ? "the in-app SSH executor" : "a runner agent"}</strong>.</>
          )}
        </div>
      </Field>

      {/* RT-3 — the DECLARED runner pin, directly after Executor: the two answer
          adjacent halves of "where does this run" (which KIND of executor, then
          WHICH runner). Only meaningful for runner-executor runs, so it disables
          itself on a pinned-SSH job rather than silently accepting a value the
          trigger would reject with a 422 (RT-Q5). */}
      <Field label="Run on (optional)">
        <RunnerPinInput
          id="composer-runner-pin"
          value={runnerTag}
          onChange={setRunnerTag}
          tags={runnerTags}
          disabled={executor === "ssh"}
          placeholder={executor === "ssh" ? "Not available for the SSH executor" : "Any eligible runner"}
        />
        <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 6 }}>
          {executor === "ssh" ? (
            <>The in-app SSH executor has no runner to pin — switch to Runner or Auto to target one.</>
          ) : runnerTag.trim() ? (
            <>
              Runs are claimed only by runners tagged <strong>{runnerTag.trim()}</strong>. Tags are edited on the
              Runners page. A tag nothing carries yet is allowed — the run waits for one.
            </>
          ) : (
            <>Any runner eligible for this job's department and run type may claim it.</>
          )}
        </div>
      </Field>

      {/* EV-6 parity — SSH keys as their own section, directly after Executor (the
          field they complete: one says how the job runs, the other what key
          material the run gets). Same presentation as job detail's JobKeyField;
          draft-until-save like the rest of the form. */}
      <Field
        label="SSH keys"
        info={
          <>
            Stored keys this job's runs receive as <code style={{ fontFamily: c.mono }}>AMADEUS_KEY_&lt;label&gt;</code>{" "}
            key files — for a playbook or script that does its own SSH. Saved with the job; agency-filtered, not
            scope-filtered.
            {identityCapable && (
              <>
                {" "}Not the same as <strong>SSH key</strong> above: that one is the identity this job{" "}
                <em>connects with</em>; these are extra key files the {runType === "ansible" ? "playbook" : "script"}{" "}
                can use itself.
              </>
            )}
          </>
        }
      >
        <ComposeKeyPicker keys={sshKeys} onChange={setSshKeys} scope={scope} executor={executor || null} canManage={canManageEnv} />
        {!canManageEnv && sshKeys.length === 0 && (
          <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 4 }}>
            Assigning keys needs the Manage Env Vars permission.
          </div>
        )}
      </Field>

      {/* JP-4a — secrets and variables are declared HERE now. The Jobs expanded
          view shows the effective set read-only (job's own plus any the
          referenced script declares); this is the surface that authors the
          job's half. Sits beside SSH keys because all three are one binding
          set on one endpoint, written together after the job row saves. */}
      <Field
        label="Secrets & variables"
        info={
          <>
            The Env Vars references this job's runs receive — only what is declared here is injected at dispatch.
            Names, never values. Each is checked against this job's scope
            {scope ? <> (<code style={{ fontFamily: c.mono }}>{scope}</code>)</> : " (global)"}; the Run dialog
            re-checks against the scope a run actually targets, and can add more for one run.
            {scriptRef && (
              <>
                {" "}References declared on <strong>{scriptRef}</strong> are added automatically at dispatch — declare
                them on the Scripts page, not here.
              </>
            )}
          </>
        }
      >
        <ComposeReferencePicker
          bindings={refBindings}
          onChange={setRefBindings}
          scope={scope}
          canManage={canManageEnv}
        />
        {!canManageEnv && refBindings.length === 0 && (
          <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 4 }}>
            Declaring secrets and variables needs the Manage Env Vars permission.
          </div>
        )}
      </Field>

      {/* RX-15 — reactions sit with the other trigger surfaces: this is the
          fourth trigger kind, alongside the schedules above. Edit-mode only,
          because a reaction is attached to the definition BY NAME and there is
          nothing to attach to until the job exists. */}
      <Field
        label="Reactions (run this job when another one finishes)"
        info={
          <>
            Runs this job when another definition finishes. A reaction has no clock, so it never appears in
            Upcoming — and a newly added one starts from now: it fires on the <strong>next</strong> matching
            completion, never retroactively on one that already happened.
          </>
        }
      >
        <ReactionsEditor
          value={reactions.list}
          onChange={reactions.setList}
          ownerKind="job"
          ownerName={isEdit ? name : ""}
          ownerSource="amadeus"
          loadError={reactions.loadError}
        />
        {reactionsError({ kind: "job", name, source: "amadeus" }, reactions.list) && (
          <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 6 }}>
            {reactionsError({ kind: "job", name, source: "amadeus" }, reactions.list)}
          </div>
        )}
        {reactionErr && <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 6 }}>{reactionErr}</div>}
      </Field>

      <Field
        label="Environment (fixed values, applied to every run)"
        info={
          <>
            Values baked into the job — <strong>never asked</strong> at run time. Applies to every run; a schedule's env
            (and a per-run override) wins on the same key. For a value an operator should supply or confirm when they
            run the job, use <strong>Run inputs</strong> below instead. Plaintext — not redacted; use the scope secrets
            system for credentials.
          </>
        }
      >
        {selectedScript && (
          <div style={{ marginBottom: 10 }}>
            <ScriptVariablesPanel
              variables={selectedScript.variables}
              runType={selectedScript.runType}
              satisfied={satisfiedVars}
              onAdd={addJobVar}
              addLabel="add"
            />
          </div>
        )}
        <EnvRowsEditor rows={jobEnvRows} onChange={setJobEnvRows} addLabel="+ env var" style={{ background: c.panel2 }} />
        {/* T1.11 — the G2 mis-modeling caught at the point it happens. */}
        {blankEnvRows.length > 0 && (
          <div style={{ fontSize: c.fontXs, color: c.warning, background: c.warningBg, border: `1px solid ${c.warning}30`, borderRadius: c.radiusSurface, padding: "8px 10px", marginTop: 8 }}>
            <div style={{ marginBottom: 6 }}>
              ⚠ {blankEnvRows.length === 1 ? "This env var has" : "These env vars have"} no value, so every run gets an
              empty string and no one is ever asked for it. Did you mean to declare{" "}
              {blankEnvRows.length === 1 ? "it" : "them"} as a <strong>Run input</strong>?
            </div>
            <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
              {blankEnvRows.map((k) => (
                <Btn key={k} small onClick={() => convertToRunInput(k)} title={`Move ${k} to Run inputs as required`}>
                  Convert <code>{k}</code> to a Run input
                </Btn>
              ))}
            </div>
          </div>
        )}
      </Field>

      <Field
        label="Run inputs (asked at run time)"
        info={
          <>
            Values an operator is asked to supply — or to confirm the default of — in the <strong>Run dialog</strong>{" "}
            before a manual run. Answers are applied as per-run env overrides. Marking one <strong>required</strong>{" "}
            makes the operator fill it or tick "Run without it"; it never hard-blocks the run, and an unfilled required
            input is recorded on the run.
          </>
        }
      >
        {/* T7.2 — making inputs prominent invites pasting credentials into a plaintext
            path. Flag credential-shaped names at authoring time, where it's cheap to fix. */}
        {credentialishPrompts.length > 0 && (
          <div style={{ fontSize: c.fontXs, color: c.warning, background: c.warningBg, border: `1px solid ${c.warning}30`, borderRadius: c.radiusSurface, padding: "8px 10px", marginBottom: 8 }}>
            ⚠ <code>{credentialishPrompts.join(", ")}</code> look{credentialishPrompts.length === 1 ? "s" : ""} like{" "}
            {credentialishPrompts.length === 1 ? "a credential" : "credentials"}. Run-input answers are stored, sent,
            and shown in run logs in plaintext. Keep credentials in the Secret Store and reference them as{" "}
            <code>AMADEUS_SECRET_&lt;name&gt;</code> instead.
          </div>
        )}
        {selectedScript && (selectedScript.variables?.length ?? 0) > 0 && (
          <div style={{ marginBottom: 8 }}>
            <Btn small onClick={seedPromptsFromScript}>
              + Import {selectedScript.variables?.length} detected variable
              {(selectedScript.variables?.length ?? 0) === 1 ? "" : "s"} from script
            </Btn>
          </div>
        )}
        <PromptRowsEditor rows={jobPrompts} onChange={setJobPrompts} />

        {/* T3.6/G7 — nobody is present to answer a prompt when a schedule fires. */}
        {unschedulableInputs.length > 0 && (
          <div style={{ fontSize: c.fontXs, color: c.warning, background: c.warningBg, border: `1px solid ${c.warning}30`, borderRadius: c.radiusSurface, padding: "8px 10px", marginTop: 8 }}>
            ⚠ This job is <strong>scheduled</strong>, but <code>{unschedulableInputs.join(", ")}</code>{" "}
            {unschedulableInputs.length === 1 ? "is required and has" : "are required and have"} no default and nothing
            supplies {unschedulableInputs.length === 1 ? "it" : "them"}. No one is present to answer when a schedule
            fires, so every scheduled run will record{" "}
            {unschedulableInputs.length === 1 ? "it" : "them"} as unfilled
            {preserved.promptEnforcement === "block" ? " — and with enforcement set to Block, be refused outright" : ""}.
            Give {unschedulableInputs.length === 1 ? "it a" : "them"} default, or supply{" "}
            {unschedulableInputs.length === 1 ? "it" : "them"} from the job env or the schedule's env.
          </div>
        )}

        {/* T3.4/JR-Q5 — opt-in hard enforcement. */}
        {jobPrompts.some((p) => p.required && p.name.trim() !== "") && (
          <div style={{ marginTop: 12, paddingTop: 12, borderTop: `1px solid ${c.border}` }}>
            <div style={{ fontSize: c.fontXs, fontWeight: 600, color: c.textSec, marginBottom: 6 }}>
              When a required input has no value
            </div>
            <Select
              value={preserved.promptEnforcement}
              onChange={(e) => setPreserved({ ...preserved, promptEnforcement: e.target.value === "block" ? "block" : "warn" })}
              style={{ maxWidth: 420 }}
            >
              <option value="warn">Warn — run anyway and record it (default)</option>
              <option value="block">Block — refuse the run</option>
            </Select>
            <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 6 }}>
              {preserved.promptEnforcement === "block" ? (
                <>
                  Any request to run this job is rejected until every required input has a value — the Run dialog{" "}
                  <em>and</em> direct API calls alike, and an operator cannot override it. Choose this for jobs that
                  must never run on a guess. Note that <strong>scheduled and workflow-triggered</strong> fires are not
                  yet refused this way; the warning above is how that case is caught, at authoring time.
                </>
              ) : (
                <>
                  The run proceeds and the unfilled name is recorded on it for audit. An operator is still shown the
                  gap in the Run dialog and must tick <strong>Run without it</strong> to continue.
                </>
              )}
            </div>
          </div>
        )}
        {driftVars.length > 0 && (
          <div style={{ fontSize: c.fontXs, color: c.warning, background: c.warningBg, border: `1px solid ${c.warning}30`, borderRadius: c.radiusSurface, padding: "8px 10px", marginTop: 8 }}>
            ⚠ The script references {driftVars.length} variable{driftVars.length === 1 ? "" : "s"} that nothing
            declares: <code>{driftVars.join(", ")}</code>. Add a <strong>Run input</strong> (asked at run time) or an{" "}
            <strong>Environment</strong> row (fixed value), or ignore if supplied elsewhere.
          </div>
        )}
      </Field>

      {/* JC20 — unified Schedule section: one "Schedule" area with two labelled
          sub-blocks (Reusable shared / Inline this-job-only) + a schedule-only recap.
          Presentation only — scheduleRefs vs inlineSchedules state, the compose payload,
          and the JC-P3/JC8 round-trip bucketing are unchanged. */}
      {/* EP-7 — the section intro AND both sub-block one-liners fold into this
          ONE ⓘ. Three info dots inside one section would be clutter replacing
          clutter; the sub-labels ("Reusable (shared)" / "Inline (this job
          only)") already name the distinction, and the ⓘ explains it once. */}
      <Field
        label="Schedule"
        info={
          <>
            When this job runs. Bind shared schedules, or author one just for this job.{" "}
            <strong>Reusable (shared)</strong> binds first-class schedules shared across jobs;{" "}
            <strong>Inline (this job only)</strong> is a job-local cron + env authored here and not shared with any
            other job.
          </>
        }
      >
        {/* Reusable (shared) sub-block */}
        <div style={subLabel()}>Reusable (shared)</div>
        {schedules.length === 0 ? (
          <div style={{ fontSize: c.fontSm, color: c.textMuted }}>No first-class schedules yet.</div>
        ) : (
          <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
            {schedules.map((s) => (
              <ReusableScheduleRow
                key={`${s.source}:${s.name}`}
                schedule={s}
                checked={scheduleRefs.includes(s.name ?? "")}
                onToggle={() => toggleRef(s.name ?? "")}
              />
            ))}
          </div>
        )}
        <div style={{ marginTop: 8 }}>
          {/* JC21 — was an inline linkBtn "+ New Schedule →"; now a small button on its own row. */}
          <Btn small onClick={() => navigate("/schedule-builder")}>+ New schedule</Btn>
        </div>

        {/* Inline (this job only) sub-block. JC-P3 — job-local cron + plaintext env, sent
            as schedules[]; persists with sourceRef=NULL and re-buckets on edit load. */}
        <div style={{ ...subLabel(), marginTop: 18, borderTop: `1px solid ${c.border}`, paddingTop: 14 }}>Inline (this job only)</div>
        {inlineSchedules.length > 0 && (
          <div style={{ display: "flex", flexDirection: "column", gap: 10, marginBottom: 8 }}>
            {inlineSchedules.map((e, i) => (
              <InlineScheduleRow
                key={i}
                entry={e}
                onChange={(patch) => updateInline(i, patch)}
                onRemove={() => removeInline(i)}
                nameError={inlineErrors[i]}
              />
            ))}
          </div>
        )}
        <Btn small onClick={addInline}>+ Add inline schedule</Btn>
        {inlineHasEnv && (
          <div style={{ marginTop: 8, fontSize: c.fontSm, color: c.warning, background: c.warningBg, border: `1px solid ${c.warning}30`, borderRadius: c.radiusSurface, padding: "8px 10px" }}>
            Inline schedule <strong>env is plaintext</strong>, injected when the schedule fires — not redacted. Use the scope secrets system for credentials.
          </div>
        )}

        {/* JC20 / B1 — schedule-only recap; doubles as the combined empty state when
            nothing is bound. The global four-facet "Effective binding" recap stays above
            Save. Gated on what's BOUND (refs + valid inline), not what merely exists. */}
        <div style={{ marginTop: 14, borderTop: `1px solid ${c.border}`, paddingTop: 10, fontSize: c.fontSm, color: c.textMuted }}>
          {scheduleRefs.length + nInlineValid === 0
            ? "No schedules bound — the job will run manually only."
            : `Bound: ${scheduleRefs.length} reusable + ${nInlineValid} inline.`}
        </div>
      </Field>

      {/* JC-P4 — scope env context: what is actually injected (the firing schedule's
          env) vs what the scope makes available (vars + secret keys, for SSH-auth +
          log redaction, NOT auto-injected as $VARS). Honest framing per architecture §3.7. */}
      {/* EP-7 — the prose moves behind the ⓘ but the cross-link does NOT: it is
          an ACTION, and hiding a navigation affordance behind an explanation
          toggle is the FX-7 inversion (only irrelevance hides). */}
      {scope && (
        <Field
          label="Scope context"
          info={<>How environment reaches a run in scope <strong>{scope}</strong>.</>}
        >
          <div style={{ marginBottom: 10 }}>
            {/* JC-P9 — cross-link to where scope vars/secrets are managed. */}
            <button type="button" onClick={() => navigate("/env-vars")} style={linkBtn()}>
              Manage in Env Vars →
            </button>
          </div>
          <div style={{ display: "flex", flexDirection: "column", gap: 12, fontSize: c.fontSm }}>
            <div>
              <div style={{ fontWeight: 600, color: c.textSec }}>Injected at run time</div>
              <div style={{ color: c.textMuted, margin: "2px 0 6px" }}>
                Job-level env + the firing schedule's env (schedule wins on key collision), plus any per-run
                override — the values added to the process environment.
              </div>
              <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
                {injectedKeys.length > 0 ? (
                  injectedKeys.map((k) => <KeyChip key={k} label={k} />)
                ) : (
                  <span style={{ color: c.textMuted }}>No schedule env — manual runs can add ad-hoc overrides.</span>
                )}
              </div>
            </div>
            <div>
              <div style={{ fontWeight: 600, color: c.textSec }}>
                Available to {scope} <span style={{ fontWeight: 400, color: c.textMuted }}>(not auto-injected)</span>
              </div>
              <div style={{ color: c.textMuted, margin: "2px 0 6px" }}>
                Used for SSH-key resolution (secret values are also masked in logs) — not added to the process env.
              </div>
              <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
                {scopeVars.length === 0 && scopeSecrets.length === 0 ? (
                  <span style={{ color: c.textMuted }}>No scoped vars or secrets.</span>
                ) : (
                  <>
                    {scopeVars.map((v) => (
                      <KeyChip key={`v:${v.key}`} label={v.key ?? ""} isGlobal={v.scope === "*" || !v.scope} />
                    ))}
                    {scopeSecrets.map((s) => (
                      <KeyChip key={`s:${s.key}`} label={s.key ?? ""} secret isGlobal={s.scope === "*" || !s.scope} />
                    ))}
                  </>
                )}
              </div>
            </div>
          </div>
        </Field>
      )}

      <Field label="Description (optional)">
        <Input style={input()} value={description} onChange={(e) => setDescription(e.target.value)} />
      </Field>

      <label style={{ display: "flex", alignItems: "center", gap: 8, fontSize: c.fontSm, color: c.textSec }}>
        <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} /> Enabled
      </label>

      {/* JC-P2 — advanced job-definition fields (collapsed by default). Editing these
          and the basics above re-sends the full state so the PUT preserves everything. */}
      <details style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, background: c.panel2, padding: "10px 14px" }}>
        <summary style={{ cursor: "pointer", fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textMuted, textTransform: "uppercase", letterSpacing: 0.7 }}>
          Advanced
        </summary>
        <div style={{ display: "flex", flexDirection: "column", gap: 14, marginTop: 14 }}>
          {/* RP-14 — env_passthrough: Git YAML has carried spec.env_passthrough
              since protocol v3, but the composer could not author it, so an
              in-app ansible job could never read a runner-local variable. Shown
              only for local-toolchain types: an ssh-family run's remote env is
              built entirely from the manifest, so there is nothing to pass
              through and the server 422s it. */}
          {runnerOnly && (
            <Field
              label="Runner environment passthrough (optional)"
              info={
                <>
                  Environment-variable <strong>names</strong> the runner reads from{" "}
                  <strong>its own environment</strong> and passes into this job's {runType} process — for a value the agent
                  host holds and Cronomicon does not (a site licence key, a locally-injected token). Names only; the value never
                  leaves the runner. An unset name fails the run loudly rather than resolving empty. Not for stored Secrets or
                  Variables — reference those instead.
                </>
              }
            >
              <Input
                style={{ ...input(), fontFamily: c.mono }}
                value={preserved.envPassthrough.join(", ")}
                onChange={(e) =>
                  setPreserved({
                    ...preserved,
                    envPassthrough: e.target.value.split(",").map((n) => n.trim()).filter(Boolean),
                  })
                }
                placeholder="e.g. SITE_LICENCE_KEY, VAULT_ADDR"
              />
            </Field>
          )}
          <div style={{ display: "flex", gap: 12 }}>
            <Field label="Timeout (seconds)" style={{ flex: 1 }}>
              <Input
                type="number"
                min={0}
                style={input()}
                value={preserved.timeoutSeconds || ""}
                placeholder="0 = none"
                onChange={(e) => setP({ timeoutSeconds: toNonNeg(e.target.value) })}
              />
            </Field>
            <Field label="Retries" style={{ flex: 1 }}>
              <Input
                type="number"
                min={0}
                style={input()}
                value={preserved.retries || ""}
                placeholder="0"
                onChange={(e) => setP({ retries: toNonNeg(e.target.value) })}
              />
            </Field>
            <Field label="Backoff (seconds)" style={{ flex: 1 }}>
              <Input
                type="number"
                min={0}
                style={input()}
                value={preserved.backoffSeconds || ""}
                placeholder="0"
                onChange={(e) => setP({ backoffSeconds: toNonNeg(e.target.value) })}
              />
            </Field>
          </div>

          <label style={{ display: "flex", alignItems: "center", gap: 8, fontSize: c.fontSm, color: c.textSec }}>
            <input type="checkbox" checked={preserved.continueOnError} onChange={(e) => setP({ continueOnError: e.target.checked })} />
            Continue on error — a failed run of this step doesn't halt its workflow (default for workflow steps)
          </label>

          {/* SL — soft deadlines. Deliberately NOT next to "Timeout": one warns a
              person, the other kills the run, and putting them side by side is
              how an operator sets the wrong one. */}
          <div style={{ display: "flex", gap: 12 }}>
            <Field label="Warn after (seconds)" style={{ flex: 1 }}>
              <input
                style={input()}
                type="number"
                min={0}
                value={preserved.warnAfterSeconds || ""}
                placeholder="0"
                onChange={(e) => setP({ warnAfterSeconds: toNonNeg(e.target.value) })}
              />
            </Field>
            <Field label="Must finish by (HH:MM)" style={{ flex: 1 }}>
              <input
                style={input()}
                value={preserved.mustFinishBy}
                placeholder="06:00"
                onChange={(e) => setP({ mustFinishBy: e.target.value })}
              />
            </Field>
          </div>
          <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: -6 }}>
            Both only <strong>warn</strong> — neither stops a run. Timeout is what kills one. “Must finish
            by” is a wall-clock time in the application timezone; a time earlier than the run's start means
            the next day, so 06:00 works for an overnight job.
          </div>

          {/* ET-D — file-arrival triggers. One path per line keeps the common
              case simple; a per-path stability window is available in YAML,
              which the hint says so nobody assumes it is missing. */}
          <Field label="Run when a file arrives (one absolute path or glob per line)">
            <textarea
              style={{ ...input(), minHeight: 62, fontFamily: c.mono, resize: "vertical" }}
              value={preserved.watch.map((w) => w.path).join("\n")}
              placeholder={"/srv/incoming/*.csv\n/var/spool/amadeus/*.xml"}
              onChange={(e) =>
                setP({
                  watch: e.target.value
                    .split("\n")
                    .map((l) => l.trim())
                    .filter(Boolean)
                    .map((path) => ({
                      path,
                      // Preserve a per-path window the YAML may have set.
                      stableSeconds: preserved.watch.find((w) => w.path === path)?.stableSeconds,
                    })),
                })
              }
            />
          </Field>
          {preserved.watch.length > 0 && (
            <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: -6 }}>
              A runner watches these only if it was started with <code>-allow-watch</code> and the path is
              inside its <code>-watch-paths</code> allowlist. The job runs once per arrival with
              <code> AMADEUS_WATCH_PATH</code> and <code>AMADEUS_WATCH_FILE</code> set; the file itself is
              not copied anywhere. Set a per-path <code>stable_seconds</code> in YAML for large files.
            </div>
          )}

          {/* ET-B/PF-Q15 — exposed for the first time. The field has been parsed
              and stored since A7 but enforced nowhere, so it stayed hidden (JC7);
              now it gates the service-account trigger API and an operator has to
              be able to set it. It does NOT affect who may click Run. */}
          <label style={{ display: "flex", alignItems: "center", gap: 8, fontSize: c.fontSm, color: c.textSec }}>
            <input type="checkbox" checked={preserved.requestable} onChange={(e) => setP({ requestable: e.target.checked })} />
            Externally triggerable — allow service-account tokens to start this job over the API
            (Settings → Service Accounts). Does not change who can run it from the UI.
          </label>

          <div style={{ display: "flex", gap: 12 }}>
            <Field label="Concurrency policy" style={{ flex: 1 }}>
              <Select
                style={input()}
                value={preserved.concurrencyPolicy}
                onChange={(e) => setP({ concurrencyPolicy: e.target.value as ConcurrencyPolicy })}
              >
                <option value="Allow">Allow — run overlapping</option>
                <option value="Forbid">Forbid — skip if already running</option>
                <option value="Queue">Queue — wait, then run when the gate clears</option>
              </Select>
            </Field>
            {preserved.concurrencyPolicy !== "Allow" && (
              <Field label="Concurrency key (optional)" style={{ flex: 1 }}>
                <Input
                  style={input()}
                  value={preserved.concurrencyKey}
                  onChange={(e) => setP({ concurrencyKey: e.target.value })}
                  placeholder="defaults to job name"
                />
              </Field>
            )}
          </div>

          <Field label="Tags (comma-separated)">
            <Input
              style={input()}
              value={tagsInput}
              placeholder="backup, db, prod"
              onChange={(e) => {
                setTagsInput(e.target.value);
                setP({ tags: e.target.value.split(",").map((t) => t.trim()).filter(Boolean) });
              }}
            />
          </Field>
        </div>
      </details>

      {/* JC-P7 — effective-binding recap: what this job will save as. */}
      <div style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, background: c.panel2, padding: "10px 14px", fontSize: c.fontSm }}>
        <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textMuted, textTransform: "uppercase", letterSpacing: 0.7, marginBottom: 6 }}>
          Effective binding
        </div>
        <div style={{ color: c.textSec, lineHeight: 1.7 }}>
          <strong style={{ fontFamily: c.mono }}>{scriptRef || "—"}</strong>
          {runType ? ` (${runType})` : ""}
          {" × "}
          {scheduleRefs.length + nInlineValid === 0
            ? "Manual"
            : `${scheduleRefs.length} reusable + ${nInlineValid} inline`}
          {" × "}
          {scope ? (
            <>
              <strong>{scope}</strong>
              {` (${scopeHosts.length} host${scopeHosts.length === 1 ? "" : "s"}${targetHost ? `, → ${targetHost}` : ""}; ${scopeVars.length} var${scopeVars.length === 1 ? "" : "s"} / ${scopeSecrets.length} secret key${scopeSecrets.length === 1 ? "" : "s"})`}
            </>
          ) : (
            "no scope"
          )}
          {" × "}
          <strong>{executor || "auto"}</strong>
          {sshKeys.length > 0 ? ` (+${sshKeys.length} SSH key${sshKeys.length === 1 ? "" : "s"})` : ""}
        </div>
      </div>

      {err && <div style={{ color: c.danger, fontSize: c.fontSm }}>{err}</div>}
      {keyErr && <div style={{ color: c.warning, fontSize: c.fontSm }}>{keyErr}</div>}
      {okMsg && <div style={{ color: c.primary, fontSize: c.fontSm }}>{okMsg}</div>}

      <div style={{ display: "flex", gap: 10 }}>
        <button onClick={submit} disabled={busy} style={btn()}>
          {isEdit ? (busy ? "Saving…" : "Save") : busy ? "Creating…" : "Create job"}
        </button>
        {isEdit && (
          <button onClick={() => navigate("/jobs")} disabled={busy} style={btnGhost()}>
            Cancel
          </button>
        )}
      </div>

      {/* Delete (edit mode only — D4): confirm-gated and placed apart from Save to
          avoid mis-clicks. No csrf header — PUT/DELETE inject it via middleware. */}
      {isEdit && (
        <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap", borderTop: `1px solid ${c.border}`, paddingTop: 14 }}>
          {pendingDelete ? (
            <>
              <span style={{ fontSize: c.fontSm, color: delBlock ? c.danger : c.textSec }}>
                {delBlock ? deleteBlockedNote(delBlock) : "Delete this job and its schedule bindings?"}
              </span>
              <button onClick={() => del(!!delBlock)} disabled={deleting} style={dangerBtn()}>
                {deleting ? "Deleting…" : delBlock ? "Delete anyway" : "Confirm delete"}
              </button>
              <button
                onClick={() => { setPendingDelete(false); setDelErr(null); setDelBlock(null); }}
                disabled={deleting}
                style={btnGhost()}
              >
                Cancel
              </button>
            </>
          ) : (
            <button onClick={() => setPendingDelete(true)} disabled={busy} style={dangerBtn()}>
              Delete job
            </button>
          )}
          {delErr && <span style={{ fontSize: c.fontSm, color: c.danger }}>{delErr}</span>}
        </div>
      )}
    </div>
  );
}

function errMessage(error: unknown): string {
  if (error && typeof error === "object" && "message" in error) {
    return String((error as { message?: unknown }).message ?? "");
  }
  return "";
}

// JC-P3 — one inline-schedule card: name + cron (presets + validated input + live
// preview via the shared cronPreview, which tolerates 5- and 6-field crons) + the
// shared env editor. Mutations bubble up via onChange so the parent owns the list.
function InlineScheduleRow({
  entry,
  onChange,
  onRemove,
  nameError,
}: {
  entry: InlineSchedule;
  onChange: (patch: Partial<InlineSchedule>) => void;
  onRemove: () => void;
  nameError?: string | null;
}) {
  const cronTrim = entry.cron.trim();
  const mode = modeOf(entry);
  // AW-12 — clamp the preview to the entry's window so a deferred schedule shows
  // its real first fire; nextSpecTimes handles all three modes.
  const preview = cronPreview(entry.cron, 3, entry);
  const nexts = nextSpecTimes(entry);
  return (
    <div style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, padding: 12, background: c.panel2, display: "flex", flexDirection: "column", gap: 8 }}>
      <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
        <Input
          style={{ ...input(), fontFamily: c.mono, flex: 1, borderColor: nameError ? c.danger : c.border }}
          value={entry.name}
          placeholder="schedule name"
          onChange={(e) => onChange({ name: e.target.value })}
        />
        <Btn small onClick={onRemove} title="Remove schedule">✕</Btn>
      </div>
      {nameError && <div style={{ fontSize: c.fontXs, color: c.danger }}>{nameError}</div>}

      <ModePicker
        value={entry}
        onChange={({ cron, ...rest }) => onChange({ ...rest, ...(cron !== undefined ? { cron: cron ?? "" } : {}) })}
      />

      {mode === "cron" && (
        <>
          <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
            {CRON_PRESETS.map((p) => {
              const on = p.expr === cronTrim;
              return (
                <button
                  key={p.expr}
                  type="button"
                  onClick={() => onChange({ cron: p.expr })}
                  style={{
                    padding: "4px 8px",
                    fontSize: c.fontXs,
                    borderRadius: c.radiusChip,
                    cursor: "pointer",
                    border: `1px solid ${on ? c.primary : c.border}`,
                    background: on ? c.primary : "transparent",
                    color: on ? c.onSolid : c.textSec,
                  }}
                >
                  {p.label}
                </button>
              );
            })}
          </div>

          <Input
            style={{ ...input(), fontFamily: c.mono, borderColor: cronTrim && !preview.valid ? c.danger : c.border }}
            value={entry.cron}
            placeholder="0 7 * * *"
            onChange={(e) => onChange({ cron: e.target.value })}
          />
        </>
      )}

      {mode === "interval" && (
        <IntervalField
          value={entry.interval}
          onChange={(v) => onChange({ interval: v })}
          hasAnchor={!!entry.startAt}
        />
      )}

      <div style={{ fontSize: c.fontSm, color: c.textMuted }}>
        {mode === "cron" && cronTrim && !preview.valid
          ? "Can't preview — validated on save (5- and 6-field accepted)."
          : `${describeSpec(entry)}${mode === "cron" && preview.sixField ? " · seconds at minute resolution" : ""}${nexts.length ? ` · next (app zone) ${nexts[0]}` : preview.nextsTruncated ? " · no fire found within the preview horizon" : ""}`}
      </div>

      <div>
        <div style={{ fontSize: c.fontXs, color: c.textMuted, marginBottom: 6 }}>Active period (optional)</div>
        <WindowEditor
          value={{ startAt: entry.startAt, endAt: entry.endAt }}
          onChange={(w) => onChange({ startAt: w.startAt, endAt: w.endAt })}
          compact
          startIsFireInstant={mode === "once"}
        />
      </div>

      {/* CAL-13 — working-calendar bindings for this entry (compact: the row is
          already dense; the full copy lives in the Schedule Builder). */}
      <div>
        <div style={{ fontSize: c.fontXs, color: c.textMuted, marginBottom: 6 }}>Working calendars (optional)</div>
        <CalendarPicker
          value={{ skipCalendars: entry.skipCalendars ?? [], onlyCalendars: entry.onlyCalendars ?? [] }}
          onChange={(b) => onChange({ skipCalendars: b.skipCalendars, onlyCalendars: b.onlyCalendars })}
          spec={entry}
          compact
        />
      </div>

      <div>
        <div style={{ fontSize: c.fontXs, color: c.textMuted, marginBottom: 6 }}>Env — this schedule only (plaintext)</div>
        <EnvRowsEditor rows={entry.env} onChange={(env) => onChange({ env })} addLabel="+ env var" style={{ background: c.panel2 }} />
      </div>
    </div>
  );
}

// JC-P6 — one reusable (first-class) schedule as a rich, bindable row: a Source badge
// (git vs amadeus, also disambiguating cross-source name collisions), human cron via
// the shared cronPreview (tolerates 6-field), next fire, and an env-key summary.
function ReusableScheduleRow({ schedule, checked, onToggle }: { schedule: Schedule; checked: boolean; onToggle: () => void }) {
  const preview = cronPreview(schedule.cron ?? "");
  const envKeys = Object.keys(schedule.env ?? {});
  return (
    <label
      style={{
        display: "flex",
        gap: 10,
        alignItems: "flex-start",
        padding: "8px 10px",
        borderRadius: c.radiusSurface,
        border: `1px solid ${checked ? c.primary : c.border}`,
        background: checked ? `${c.primary}12` : c.panel2,
        cursor: "pointer",
      }}
    >
      <input type="checkbox" checked={checked} onChange={onToggle} style={{ marginTop: 2 }} />
      <div style={{ flex: 1, minWidth: 0 }}>
        <div style={{ display: "flex", alignItems: "center", gap: 6 }}>
          <SourceBadge source={schedule.source} />
          <span style={{ fontFamily: c.mono, fontSize: c.fontSm, color: c.text }}>{schedule.name}</span>
        </div>
        <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 3 }}>
          <span style={{ fontFamily: c.mono }}>{schedule.cron}</span>
          {preview.valid && <span> · {preview.human}</span>}
          {preview.nexts.length > 0 && <span> · next (app zone) {preview.nexts[0]}</span>}
          {preview.nexts.length === 0 && preview.nextsTruncated && (
            <span> · no fire found within the preview horizon</span>
          )}
        </div>
        {envKeys.length > 0 && (
          <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 3 }}>
            env: <span style={{ fontFamily: c.mono }}>{envKeys.join(", ")}</span>
          </div>
        )}
      </div>
    </label>
  );
}

// UDV1 — editor for a job's declared prompt variables. Each row is name + optional
// label/default/options + a required toggle. `options` is a free-text comma/newline
// list (split on submit). Theme-safe: all styles read c.* at render via factories.
function PromptRowsEditor({ rows, onChange }: { rows: PromptRow[]; onChange: (rows: PromptRow[]) => void }) {
  const set = (i: number, patch: Partial<PromptRow>) => onChange(rows.map((r, idx) => (idx === i ? { ...r, ...patch } : r)));
  const remove = (i: number) => onChange(rows.filter((_, idx) => idx !== i));
  const add = () => onChange([...rows, { name: "", label: "", required: false, default: "", options: "" }]);
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
      {rows.map((r, i) => (
        <div key={i} style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, padding: 8, display: "flex", flexDirection: "column", gap: 6 }}>
          <div style={{ display: "flex", gap: 6 }}>
            <Input style={{ ...input(), flex: 1 }} value={r.name} onChange={(e) => set(i, { name: e.target.value })} placeholder="VARIABLE_NAME" />
            <Input style={{ ...input(), flex: 1 }} value={r.label} onChange={(e) => set(i, { label: e.target.value })} placeholder="Label (optional)" />
            <Btn small onClick={() => remove(i)} title="Remove run input">✕</Btn>
          </div>
          <div style={{ display: "flex", gap: 6, alignItems: "center" }}>
            <Input style={{ ...input(), flex: 1 }} value={r.default} onChange={(e) => set(i, { default: e.target.value })} placeholder="Default (optional)" />
            <Input style={{ ...input(), flex: 1 }} value={r.options} onChange={(e) => set(i, { options: e.target.value })} placeholder="Options: dev, staging, prod (optional)" />
            <label style={{ display: "flex", alignItems: "center", gap: 4, fontSize: c.fontSm, color: c.textSec, whiteSpace: "nowrap" }}>
              <input type="checkbox" checked={r.required} onChange={(e) => set(i, { required: e.target.checked })} /> required
            </label>
          </div>
        </div>
      ))}
      <Btn small onClick={add} style={{ alignSelf: "flex-start" }}>+ run input</Btn>
    </div>
  );
}

// JC-P4 — a single env/secret key chip for the scope-context panel. Secrets are
// keys only (value never fetched); globals (scope '*'/empty) carry a muted marker.
function KeyChip({ label, secret, isGlobal }: { label: string; secret?: boolean; isGlobal?: boolean }) {
  return (
    <span
      title={secret ? "secret (key only — value never shown)" : isGlobal ? "global (all scopes)" : undefined}
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 3,
        padding: "2px 8px",
        borderRadius: c.radiusChip,
        fontSize: c.fontXs,
        fontFamily: c.mono,
        background: secret ? c.warningBg : c.panel2,
        border: `1px solid ${secret ? `${c.warning}40` : c.border}`,
        color: secret ? c.warning : c.textSec,
      }}
    >
      {secret && <span style={{ fontFamily: c.sans }} aria-label="secret">🔒</span>}
      {label}
      {isGlobal && <span style={{ color: c.textMuted, fontFamily: c.sans }}>*</span>}
    </span>
  );
}

// RP-4 — the label idiom is authored once in components/ui.tsx (fieldLabelStyle)
// so the Composer and the Run dialog read as the same form; the Composer keeps
// its own tighter field spacing via the margin parameter.
// EP-7 — `info` is the section's static education text, behind the ⓘ on the
// label row instead of permanently inline. The rule it follows is EV-1's, the
// one Section's own comment states: education goes behind the toggle, a
// CONSEQUENCE of the operator's current input stays inline and conditional.
// So the explanatory paragraphs move here; validation verdicts, permission
// notices and "what your current selection means" lines stay where they are.
//
// Why the Composer needed this at all: eight sections each opened with one to
// six lines of prose, permanently, so the form's resting state read as a wall
// regardless of whether anyone was learning anything from it. Same complaint
// RU-10 named in the Run dialog — but these are SECTION intros, so the fix is
// Section's ⓘ, not RU-10's focus-driven "engaged" helpers. A section intro has
// no focus state to engage on; do not conflate the two mechanisms.
function Field({ label, info, children, style }: { label: string; info?: React.ReactNode; children: React.ReactNode; style?: React.CSSProperties }) {
  const [showInfo, setShowInfo] = useState(false);
  return (
    <div style={style}>
      {/* The spread comes FIRST: fieldLabelStyle carries `display: "block"`, so
          spreading it after the flex properties silently kills the gap and the
          ⓘ ends up flush against the label. */}
      <div style={{ ...fieldLabelStyle("0 0 4px"), display: "flex", alignItems: "center", gap: 6 }}>
        <span>{label}</span>
        {info != null && <InfoToggle open={showInfo} onToggle={() => setShowInfo((v) => !v)} />}
      </div>
      {info != null && showInfo && <InfoBody margin="0 0 8px">{info}</InfoBody>}
      {children}
    </div>
  );
}

// JC20 — sub-block label inside the unified Schedule section; mirrors Field's label
// (read c.* at render — theme-safe, no frozen const).
const subLabel = (): React.CSSProperties => fieldLabelStyle("0 0 4px");

// JC-P7 — style functions, not frozen consts: the theme tokens (`c`) are mutated in
// place on theme toggle, so a captured object literal would keep stale colors. Calling
// these each render rebuilds them against the active palette (matches components/ui).
// RP-4 — the control style is the shared inputStyle(); only the font stays pinned to
// the sans stack (inputStyle inherits, which is right for the dialog's modal context).
const input = (): React.CSSProperties => ({
  ...inputStyle(),
  fontFamily: c.sans,
});

const btn = (): React.CSSProperties => ({
  padding: "9px 18px",
  background: c.primary,
  color: c.onSolid,
  border: "none",
  borderRadius: c.radiusChip,
  fontSize: c.fontSm,
  fontWeight: 600,
  cursor: "pointer",
});

const btnGhost = (): React.CSSProperties => ({
  padding: "9px 18px",
  background: "transparent",
  color: c.text,
  border: `1px solid ${c.border}`,
  borderRadius: c.radiusChip,
  fontSize: c.fontSm,
  cursor: "pointer",
});

const dangerBtn = (): React.CSSProperties => ({
  padding: "9px 18px",
  background: c.danger,
  color: c.onSolid,
  border: "none",
  borderRadius: c.radiusChip,
  fontSize: c.fontSm,
  fontWeight: 600,
  cursor: "pointer",
});

// JC-P9 — inline text-link button (cross-links to sibling primitive views).
const linkBtn = (): React.CSSProperties => ({
  background: "none",
  border: "none",
  padding: 0,
  color: c.primary,
  fontSize: c.fontXs,
  fontWeight: 600,
  cursor: "pointer",
});

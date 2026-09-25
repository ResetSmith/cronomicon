import { useEffect, useState } from "react";
import { api } from "../../api/client";
import { useGet, rows } from "../../hooks";
import { c } from "../../theme";
import { Link } from "react-router-dom";
import { CRON_PRESETS, cronToHuman, nextCronTimesDetailed, validateCron } from "../scheduling/cron";
import { banner, btn, chipBtn, h2, input, label, panel, pre } from "../scheduling/ui";
import { EnvRowsEditor, InlineLoading, Modal, SkeletonRows } from "../../components/ui";
import { RUN_TYPES } from "../../runtypes";

// ── Local shapes ──────────────────────────────────────────────────────────────
interface JobListItem {
  id?: number;
  name: string;
  schedule?: string | null;
}
interface ScheduleEntryDetail {
  name?: string;
  cron?: string;
  env?: Record<string, string> | null;
}
interface JobDetail {
  id?: number;
  name?: string;
  type?: string;
  description?: string | null;
  scope?: string | null;
  host?: string | null;
  executor?: string | null;
  // RT — the DECLARED pin, which is the only job-level pin there is since
  // v1.3.5. It belongs in the YAML this builder publishes; the per-run pin does
  // not, being one run's decision rather than the definition's.
  runnerTag?: string | null;
  command?: string | null;
  script?: string | null;
  scriptPath?: string | null;
  concurrencyPolicy?: string;
  concurrencyKey?: string | null;
  timeoutSeconds?: number | null;
  retries?: number;
  tags?: string[];
  requestable?: boolean;
  warnAfterSeconds?: number | null;
  mustFinishBy?: string | null;
  watch?: { path?: string; stableSeconds?: number }[];
  schedule?: string | null;
  schedules?: ScheduleEntryDetail[];
}
interface SchedulePush {
  id?: string;
  userEmail?: string;
  commitSha?: string | null;
  branch?: string;
  filesChanged?: string[];
  status?: string;
  errorMessage?: string | null;
  timestamp?: string;
}
interface SyncEvent {
  commit?: string | null;
}
interface LineError {
  file?: string;
  line?: number;
  field?: string;
  message: string;
}
type PublishResult =
  | { kind: "success"; push: SchedulePush; baseSha: string }
  | { kind: "conflict"; staleSha: string; currentSha: string; diff: string; message: string }
  | { kind: "validation"; message: string; errors: LineError[] }
  | { kind: "error"; status: number; message: string };

// One editable schedule entry; env is rendered as ordered key/value rows.
interface EnvRow {
  key: string;
  value: string;
}
interface Entry {
  name: string;
  cron: string;
  env: EnvRow[];
}

const NEW_JOB = "__new__";
const ENTRY_NAME_RE = /^[a-z0-9][a-z0-9_-]{0,63}$/;
const short = (sha?: string | null) => (sha ? sha.slice(0, 8) : "—");
const isCronLike = (s?: string | null) => !!s && s.trim().split(/\s+/).length === 5;

// The next-fire hint, which must distinguish "no more fires" from "the preview
// ran out of horizon" (FX2-E): an impossible or very sparse date silently
// rendered nothing here, indistinguishable from a valid rare schedule.
const nextHint = (cron: string): string => {
  const { times, truncated } = nextCronTimesDetailed(cron, 1);
  if (times.length > 0) return ` · next (app zone) ${times[0]}`;
  return truncated ? " · no fire found within the preview horizon" : "";
};

// yamlScalar emits a plain YAML scalar where safe, else a double-quoted one.
function yamlScalar(v: string): string {
  if (/^[A-Za-z0-9_./@+-]+$/.test(v) && !/^(true|false|null|yes|no|on|off|~)$/i.test(v)) return v;
  return '"' + v.replace(/\\/g, "\\\\").replace(/"/g, '\\"') + '"';
}
// yamlField emits `key: value`, using a block scalar for multi-line values.
function yamlField(key: string, value: string, indent: number): string {
  const pad = " ".repeat(indent);
  if (value.includes("\n")) {
    const body = value
      .replace(/\n+$/, "")
      .split("\n")
      .map((l) => " ".repeat(indent + 2) + l)
      .join("\n");
    return `${pad}${key}: |-\n${body}\n`;
  }
  return `${pad}${key}: ${yamlScalar(value)}\n`;
}

function jobSource(d: JobDetail): { kind: "command" | "script" | "scriptPath"; body: string } | null {
  if (d.command) return { kind: "command", body: d.command };
  if (d.script) return { kind: "script", body: d.script };
  if (d.scriptPath) return { kind: "scriptPath", body: d.scriptPath };
  return null;
}

// "Publish to GitLab" authoring mode for Jobs — reached from Jobs via the hidden
// /jobs/publish route (mirrors how Jobs launches Compose). Authors a `kind: Job`
// YAML (executor, scope→target_host, concurrency, timeout, retries, tags +
// schedules) and Git-publishes it via POST /schedules/publish with an If-Match
// base_sha precondition (A2). The push audit lives in History → Schedule Pushes,
// not here (single source of truth).
export function PublishBuilder() {
  // ── data ──
  const jobsQ = useGet<unknown>(() => api.GET("/jobs"));
  const jobs = rows<JobListItem>(jobsQ.data);

  const [selected, setSelected] = useState<string>(NEW_JOB);
  const job = selected === NEW_JOB ? null : jobs.find((j) => j.name === selected) ?? null;
  const detailQ = useGet<JobDetail>(
    () => (job?.id != null ? api.GET("/jobs/{jobId}", { params: { path: { jobId: job.id } } }) : Promise.resolve({ data: null })),
    [job?.id],
  );
  const detail = selected === NEW_JOB ? null : detailQ.data;

  // ── editable form state ──
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [runType, setRunType] = useState("bash");
  const [command, setCommand] = useState("");
  const [manual, setManual] = useState(false);
  const [entries, setEntries] = useState<Entry[]>([{ name: "default", cron: "0 7 * * *", env: [] }]);
  const [commitMessage, setCommitMessage] = useState("");

  // ── publish flow ──
  const [phase, setPhase] = useState<"idle" | "confirm" | "publishing">("idle");
  const [result, setResult] = useState<PublishResult | null>(null);

  const [syncDep, setSyncDep] = useState(0);
  const syncQ = useGet<unknown>(() => api.GET("/git/history"), [syncDep]);
  const latestSync = rows<SyncEvent>(syncQ.data).find((e) => e.commit);
  const [shaOverride, setShaOverride] = useState<{ sha: string; source: string } | null>(null);
  const baseSha = shaOverride?.sha ?? latestSync?.commit ?? null;
  const baseShaSource = shaOverride?.source ?? (latestSync ? "latest Git sync" : null);

  // Seed the schedule editor from job detail (read path, review item 12): a
  // multi-entry job round-trips into the editor instead of collapsing to one.
  useEffect(() => {
    setResult(null);
    if (selected === NEW_JOB) {
      setName("");
      setDescription("");
      setRunType("bash");
      setCommand("");
      setManual(false);
      setEntries([{ name: "default", cron: "0 7 * * *", env: [] }]);
      return;
    }
    if (!detail) return;
    setName(detail.name ?? "");
    setDescription(detail.description ?? "");
    const seeded: Entry[] = (detail.schedules ?? []).map((s) => ({
      name: s.name ?? "default",
      cron: s.cron ?? "",
      env: Object.entries(s.env ?? {}).map(([key, value]) => ({ key, value })),
    }));
    if (seeded.length > 0) {
      setManual(false);
      setEntries(seeded);
    } else if (isCronLike(detail.schedule)) {
      setManual(false);
      setEntries([{ name: "default", cron: detail.schedule!.trim(), env: [] }]);
    } else {
      setManual(true);
      setEntries([{ name: "default", cron: "0 7 * * *", env: [] }]);
    }
  }, [selected, detail]);

  const isNew = !job;
  const jobName = (isNew ? name.trim() : detail?.name) || "";
  const filePath = `jobs/${jobName || "my-scheduled-job"}.yaml`;

  // ── entry mutation helpers ──
  const updateEntry = (i: number, patch: Partial<Entry>) => setEntries((es) => es.map((e, j) => (j === i ? { ...e, ...patch } : e)));
  const addEntry = () => setEntries((es) => [...es, { name: `schedule-${es.length + 1}`, cron: "0 * * * *", env: [] }]);
  const removeEntry = (i: number) => setEntries((es) => es.filter((_, j) => j !== i));

  // ── validation ──
  const cleanEntries = entries.map((e) => ({
    name: e.name.trim(),
    cron: e.cron.trim(),
    env: e.env.filter((r) => r.key.trim() !== ""),
  }));
  const nameCounts = cleanEntries.reduce<Record<string, number>>((acc, e) => ((acc[e.name.toLowerCase()] = (acc[e.name.toLowerCase()] ?? 0) + 1), acc), {});
  const entryErrors = manual
    ? []
    : cleanEntries.map((e) => {
        if (!ENTRY_NAME_RE.test(e.name)) return "name must be a slug (a-z, 0-9, _-, ≤64)";
        if (nameCounts[e.name.toLowerCase()] > 1) return "duplicate name";
        if (!validateCron(e.cron)) return "invalid cron";
        return null;
      });
  const entriesValid = manual || (cleanEntries.length > 0 && entryErrors.every((e) => e === null));
  const hasEnv = cleanEntries.some((e) => e.env.length > 0);
  const sourceOK = isNew ? command.trim() !== "" : jobSource(detail ?? {}) !== null;
  const canPublish = phase !== "publishing" && !!baseSha && !!jobName && entriesValid && sourceOK;

  // ── canonical YAML (apiVersion/kind/metadata/spec) with the full Gap B set ──
  const buildYaml = (): string => {
    let y = "apiVersion: cronomicon.io/v1\n";
    y += "kind: Job\n";
    y += "metadata:\n";
    y += `  name: ${yamlScalar(jobName || "my-scheduled-job")}\n`;
    y += "spec:\n";
    y += `  run_type: ${isNew ? runType : detail?.type ?? "bash"}\n`;
    if (description.trim()) y += yamlField("description", description.trim(), 2);

    // Source — exactly one of command/script/scriptPath (validator EX.1).
    if (isNew) {
      y += yamlField("command", command, 2);
    } else {
      const src = jobSource(detail ?? {});
      if (src) y += yamlField(src.kind === "scriptPath" ? "scriptPath" : src.kind, src.body, 2);
    }

    // Preserved Gap B / definition fields (so a schedule edit never strips them).
    if (detail?.scope) y += `  scope: ${yamlScalar(detail.scope)}\n`;
    if (detail?.host) y += `  target_host: ${yamlScalar(detail.host)}\n`;
    if (detail?.executor) y += `  executor: ${yamlScalar(detail.executor)}\n`;
    // RT: the DECLARED pin, and the same Gap-B argument as every field around it —
    // a field this builder cannot emit is a field a schedule edit silently strips,
    // and stripping a runner pin sends the job back to running anywhere eligible.
    // Deliberately detail.runnerTag, NOT runnerTagEffective: the two are equal
    // today, but the effective value is a RESOLVED answer and this file emits a
    // DECLARATION. Publishing a resolved value is how a transient decision gets
    // frozen into the repository as if it were the author's intent.
    if (detail?.runnerTag) y += `  runner_tag: ${yamlScalar(detail.runnerTag)}\n`;
    if (detail?.concurrencyPolicy) y += `  concurrency_policy: ${yamlScalar(detail.concurrencyPolicy)}\n`;
    if (detail?.concurrencyKey) y += `  concurrency_key: ${yamlScalar(detail.concurrencyKey)}\n`;
    if (detail?.timeoutSeconds) y += `  timeout_seconds: ${detail.timeoutSeconds}\n`;
    if (detail?.retries) y += `  retries: ${detail.retries}\n`;
    if (detail?.tags && detail.tags.length > 0) y += `  tags: [${detail.tags.map(yamlScalar).join(", ")}]\n`;
    // ET-B: requestable gates the service-account trigger API. Omitting it here
    // would let a schedule edit silently strip the flag and take an integration
    // offline — precisely the Gap-B failure this block exists to prevent.
    if (detail?.requestable) y += `  requestable: true\n`;
    // SL: same Gap-B argument as requestable — a field this builder cannot emit
    // is a field a schedule edit silently strips, and stripping a deadline turns
    // off an alert nobody notices is off.
    if (detail?.warnAfterSeconds) y += `  warn_after_seconds: ${detail.warnAfterSeconds}\n`;
    if (detail?.mustFinishBy) y += `  must_finish_by: ${yamlScalar(detail.mustFinishBy)}\n`;
    // ET-D: same Gap-B argument again — a field this builder cannot emit is a
    // field a schedule edit silently strips, and stripping a watch turns off a
    // trigger with nothing to show for it.
    if (detail?.watch && detail.watch.length > 0) {
      y += `  watch:\n`;
      for (const w of detail.watch) {
        if (!w.path) continue;
        y += `    - path: ${yamlScalar(w.path)}\n`;
        if (w.stableSeconds) y += `      stable_seconds: ${w.stableSeconds}\n`;
      }
    }

    // Schedule: legacy single string when a lone default/no-env entry; else list.
    const useLegacy = !manual && cleanEntries.length === 1 && cleanEntries[0].name === "default" && cleanEntries[0].env.length === 0;
    if (manual || cleanEntries.length === 0) {
      y += `  schedule: Manual\n`;
    } else if (useLegacy) {
      y += `  schedule: ${yamlScalar(cleanEntries[0].cron)}\n`;
    } else {
      y += "  schedules:\n";
      for (const e of cleanEntries) {
        y += `    - name: ${yamlScalar(e.name)}\n`;
        y += `      cron: ${yamlScalar(e.cron)}\n`;
        if (e.env.length > 0) {
          y += "      env:\n";
          for (const r of e.env) y += `        ${yamlScalar(r.key.trim())}: ${yamlScalar(r.value)}\n`;
        }
      }
    }
    return y;
  };
  const yaml = buildYaml();

  // ── POST /schedules/publish with If-Match: base_sha (A2 hard contract) ──
  const confirmPublish = async () => {
    if (!baseSha) return;
    const sentSha = baseSha;
    setPhase("publishing");
    setResult(null);
    const { data, error, response } = await api.POST("/schedules/publish", {
      params: { header: { "X-CSRF-Token": "", "If-Match": sentSha } },
      body: {
        filePath,
        content: yaml,
        ...(commitMessage.trim() ? { commitMessage: commitMessage.trim() } : {}),
        new: isNew,
      },
    });
    setPhase("idle");
    if (response.status === 201 && data) {
      const push = data as SchedulePush;
      if (push.commitSha) setShaOverride({ sha: push.commitSha, source: "last publish (201)" });
      setResult({ kind: "success", push, baseSha: sentSha });
    } else if (response.status === 412) {
      const e = (error ?? {}) as { baseSha?: string; currentSha?: string; diff?: string; message?: string };
      setResult({
        kind: "conflict",
        staleSha: e.baseSha ?? sentSha,
        currentSha: e.currentSha ?? "",
        diff: e.diff ?? "",
        message: e.message ?? "The file changed since your edit was based — review the diff and retry.",
      });
    } else if (response.status === 422) {
      const e = (error ?? {}) as { message?: string; errors?: LineError[] };
      setResult({ kind: "validation", message: e.message ?? "YAML schema validation failed", errors: e.errors ?? [] });
    } else {
      const e = (error ?? {}) as { message?: string; code?: string };
      setResult({ kind: "error", status: response.status, message: e.message ?? e.code ?? `Request failed (${response.status})` });
    }
  };
  const reloadBaseSha = (currentSha: string) => {
    if (currentSha) setShaOverride({ sha: currentSha, source: "412 reload (current tip)" });
    else {
      setShaOverride(null);
      setSyncDep((n) => n + 1);
    }
    setResult(null);
  };

  return (
    <div>
      {jobsQ.loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {jobsQ.error && <div style={{ color: c.danger }}>Error: {jobsQ.error}</div>}

      {!jobsQ.loading && !jobsQ.error && (
        <div style={{ display: "flex", gap: 16, alignItems: "flex-start", flexWrap: "wrap" }}>
          {/* ── Editor ── */}
          <div style={{ ...panel(), flex: "0 1 420px", minWidth: 320 }}>
            <h2 style={h2()}>Schedule Builder</h2>

            <label style={label()}>Job</label>
            <select value={selected} onChange={(e) => setSelected(e.target.value)} style={{ ...input() }}>
              <option value={NEW_JOB}>— new job YAML —</option>
              {jobs.map((j) => (
                <option key={String(j.id ?? j.name)} value={j.name}>
                  {j.name}
                  {j.schedule ? ` (${j.schedule})` : ""}
                </option>
              ))}
            </select>

            {!isNew && detailQ.loading && <InlineLoading what="job detail" />}

            {isNew && (
              <>
                <label style={label()}>Job Name</label>
                <input value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. daily-backup" style={input()} />
                <label style={label()}>Run Type</label>
                <select value={runType} onChange={(e) => setRunType(e.target.value)} style={input()}>
                  {RUN_TYPES.map((t) => (
                    <option key={t} value={t}>
                      {t}
                    </option>
                  ))}
                </select>
                <label style={label()}>Command</label>
                <input value={command} onChange={(e) => setCommand(e.target.value)} placeholder="echo hello" style={{ ...input(), fontFamily: c.mono }} />
              </>
            )}

            <label style={label()}>Description</label>
            <input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="What does this do?" style={input()} />

            {!isNew && detail && (
              <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: -6, marginBottom: 12, lineHeight: 1.5 }}>
                Preserved on publish: {jobSource(detail)?.kind ?? "source"}
                {detail.concurrencyPolicy && detail.concurrencyPolicy !== "Allow" ? ` · ${detail.concurrencyPolicy}` : ""}
                {detail.timeoutSeconds ? ` · timeout ${detail.timeoutSeconds}s` : ""}
                {detail.retries ? ` · ${detail.retries} retries` : ""}
                {detail.tags && detail.tags.length > 0 ? ` · tags: ${detail.tags.join(", ")}` : ""}
              </div>
            )}

            {/* Schedule mode */}
            <label style={label()}>Schedule</label>
            <select value={manual ? "manual" : "scheduled"} onChange={(e) => setManual(e.target.value === "manual")} style={input()}>
              <option value="scheduled">Scheduled (one or more cron entries)</option>
              <option value="manual">Manual only</option>
            </select>

            {!manual && (
              <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
                {entries.map((e, i) => (
                  <div key={i} style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, padding: 12, background: c.panel2 }}>
                    <div style={{ display: "flex", gap: 8, alignItems: "center", marginBottom: 8 }}>
                      <input
                        value={e.name}
                        onChange={(ev) => updateEntry(i, { name: ev.target.value })}
                        placeholder="entry name"
                        style={{ ...input(), marginBottom: 0, fontFamily: c.mono, flex: 1, borderColor: entryErrors[i] ? c.danger : c.borderStrong }}
                      />
                      {entries.length > 1 && (
                        <button onClick={() => removeEntry(i)} style={{ ...chipBtn(), color: c.danger, borderColor: `${c.danger}50` }}>
                          remove
                        </button>
                      )}
                    </div>

                    <div style={{ display: "flex", flexWrap: "wrap", gap: 6, marginBottom: 8 }}>
                      {CRON_PRESETS.map((p) => (
                        <button
                          key={p.expr}
                          onClick={() => updateEntry(i, { cron: p.expr })}
                          style={{
                            ...chipBtn(),
                            background: p.expr === e.cron.trim() ? c.primary : c.panel,
                            color: p.expr === e.cron.trim() ? c.onSolid : c.textSec,
                            borderColor: p.expr === e.cron.trim() ? c.primary : c.border,
                          }}
                        >
                          {p.label}
                        </button>
                      ))}
                    </div>
                    <input
                      value={e.cron}
                      onChange={(ev) => updateEntry(i, { cron: ev.target.value })}
                      placeholder="0 7 * * *"
                      style={{ ...input(), marginBottom: 6, fontFamily: c.mono, borderColor: validateCron(e.cron) ? c.borderStrong : c.danger }}
                    />
                    <div style={{ fontSize: c.fontSm, color: entryErrors[i] ? c.danger : c.textSec, marginBottom: 8 }}>
                      {entryErrors[i] ? `✗ ${entryErrors[i]}` : cronToHuman(e.cron)}
                      {!entryErrors[i] ? nextHint(e.cron) : ""}
                    </div>

                    {/* env rows (shared editor — JC6) */}
                    <EnvRowsEditor rows={e.env} onChange={(env) => updateEntry(i, { env })} addLabel="+ env var" />
                  </div>
                ))}
                <button onClick={addEntry} style={{ ...chipBtn(), alignSelf: "flex-start" }}>
                  + Add schedule
                </button>
                {hasEnv && (
                  <div style={{ ...banner(c.warning), marginTop: 2 }}>
                    Schedule <code>env</code> is committed to Git in <strong>plaintext</strong> — use the secrets/scope system for credentials, not here.
                  </div>
                )}
              </div>
            )}

            <label style={{ ...label(), marginTop: 16 }}>Commit Message (optional)</label>
            <input value={commitMessage} onChange={(e) => setCommitMessage(e.target.value)} placeholder="Defaults to a generated message" style={input()} />
          </div>

          {/* ── Preview + publish ── */}
          <div style={{ ...panel(), flex: "1 1 420px", minWidth: 320 }}>
            <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: 10 }}>
              <h2 style={{ ...h2(), marginBottom: 0 }}>YAML Preview</h2>
              <button
                onClick={() => setPhase("confirm")}
                disabled={!canPublish}
                style={{ ...btn(), background: canPublish ? c.primary : c.panel2, color: canPublish ? c.onSolid : c.textSec, cursor: canPublish ? "pointer" : "not-allowed" }}
              >
                {phase === "publishing" ? "Publishing…" : "Publish to GitLab"}
              </button>
            </div>

            <div style={{ fontSize: c.fontSm, color: c.textSec, marginBottom: 10, fontFamily: c.mono }}>
              {filePath} · base_sha <span style={{ color: baseSha ? c.text : c.warning }}>{syncQ.loading ? "resolving…" : short(baseSha)}</span>
              {baseShaSource && <span> ({baseShaSource})</span>}
            </div>
            {!syncQ.loading && !baseSha && (
              <div style={{ ...banner(c.warning), marginBottom: 10 }}>
                No base_sha available — the If-Match precondition is required (A2). Retry once a Git sync has run.
              </div>
            )}
            {!sourceOK && isNew && <div style={{ ...banner(c.warning), marginBottom: 10 }}>A new job needs a command.</div>}

            <pre style={pre()}>{yaml}</pre>

            {result?.kind === "success" && (
              <div style={{ border: `1px solid ${c.success}40`, borderRadius: c.radiusSurface, marginTop: 12, overflow: "hidden" }}>
                <div style={{ padding: "10px 14px", background: `${c.success}18`, color: c.success, fontSize: c.fontSm }}>
                  ✓ <strong>201 Published</strong> — commit <code style={{ fontFamily: c.mono }}>{short(result.push.commitSha)}</code> pushed (branch {result.push.branch ?? "main"})
                </div>
                <div style={{ padding: "8px 14px", fontSize: c.fontSm, color: c.textSec }}>
                  The updated definition appears in Jobs after the next sync (≤ 5 min, or instantly via webhook).
                </div>
              </div>
            )}
            {result?.kind === "conflict" && (
              <div style={{ border: `1px solid ${c.danger}50`, borderRadius: c.radiusSurface, marginTop: 12, overflow: "hidden" }}>
                <div style={{ padding: "10px 14px", background: `${c.danger}18`, color: c.danger, fontSize: c.fontSm }}>
                  ✗ <strong>412 Precondition Failed</strong> — someone else changed this file
                </div>
                <div style={{ padding: "8px 14px", fontSize: c.fontSm, color: c.textSec }}>
                  {result.message} Base <code style={{ fontFamily: c.mono }}>{short(result.staleSha)}</code> is behind tip{" "}
                  <code style={{ fontFamily: c.mono }}>{short(result.currentSha)}</code>.
                </div>
                {result.diff && (
                  <pre style={{ ...pre(), margin: "0 14px 10px", maxHeight: 220 }}>
                    {result.diff.split("\n").map((line, i) => (
                      <div key={i} style={{ color: line.startsWith("+") ? c.success : line.startsWith("-") ? c.danger : line.startsWith("@@") ? c.info : c.textSec }}>
                        {line || " "}
                      </div>
                    ))}
                  </pre>
                )}
                <div style={{ padding: "0 14px 12px" }}>
                  <button onClick={() => reloadBaseSha(result.currentSha)} style={{ ...btn(), background: c.primary, color: c.onSolid }}>
                    Reload latest base_sha & retry on top
                  </button>
                </div>
              </div>
            )}
            {result?.kind === "validation" && (
              <div style={{ border: `1px solid ${c.danger}50`, borderRadius: c.radiusSurface, marginTop: 12, overflow: "hidden" }}>
                <div style={{ padding: "10px 14px", background: `${c.danger}18`, color: c.danger, fontSize: c.fontSm }}>
                  ✗ <strong>422 Validation failed</strong> — {result.message}
                </div>
                {result.errors.map((e, i) => (
                  <div key={i} style={{ padding: "6px 14px", fontSize: c.fontSm, fontFamily: c.mono, color: c.textSec, borderTop: `1px solid ${c.border}` }}>
                    {e.file ?? filePath}:{e.line ?? "?"} {e.field ? `[${e.field}] ` : ""}— {e.message}
                  </div>
                ))}
              </div>
            )}
            {result?.kind === "error" && (
              <div style={{ ...banner(c.danger), marginTop: 12 }}>
                ✗ {result.status === 403 ? "403 Forbidden — missing schedule publish permission." : result.status === 503 ? "503 — GitLab unreachable, retry later." : `${result.status} — error.`} {result.message}
              </div>
            )}
          </div>
        </div>
      )}

      {/* ── Push audit lives in History (single source of truth) ── */}
      <div style={{ marginTop: 24, fontSize: c.fontSm, color: c.textSec }}>
        Recent publishes are tracked in{" "}
        <Link to="/runs?tab=schedule-pushes" style={{ color: c.primary, textDecoration: "none" }}>
          History → Schedule Pushes
        </Link>
        .
      </div>

      {/* ── Confirm Publish modal ── */}
      {phase === "confirm" && (
        <Modal
          title="Confirm Publish to GitLab"
          wide
          onClose={() => setPhase("idle")}
          /* FX-10 — the YAML preview is already capped, but the whole panel is
             capped too: on a short viewport Confirm & Publish went under the fold
             with it. */
          footer={
            <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
              <button onClick={() => setPhase("idle")} style={{ ...btn(), background: c.panel2, color: c.textSec, border: `1px solid ${c.border}` }}>
                Cancel
              </button>
              <button onClick={() => void confirmPublish()} style={{ ...btn(), background: c.primary, color: c.onSolid }}>
                Confirm & Publish
              </button>
            </div>
          }
        >
          <div style={{ fontSize: c.fontSm, color: c.textSec, marginBottom: 10 }}>
            POST /api/v1/schedules/publish · If-Match <code style={{ fontFamily: c.mono }}>{short(baseSha)}</code> · {filePath} · new={String(isNew)}
          </div>
          <pre style={{ ...pre(), maxHeight: 260 }}>{yaml}</pre>
          <div style={{ fontSize: c.fontSm, color: c.textSec, marginTop: 10 }}>
            The server rejects with 412 if the file changed since base_sha (A2).
          </div>
        </Modal>
      )}
    </div>
  );
}


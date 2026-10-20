import { useEffect, useMemo, useState } from "react";
import { allReadable, api, csrfHeader } from "../api/client";
import type { components } from "../api/schema";
import { rows, useGet } from "../hooks";
import { c } from "../theme";
import { Btn, InlineLoading, Modal } from "../components/ui";

// Give a definition to an agency (2.5.0 C7.3; C10, IN-12, question 98).
//
// A global administrator's: a job or workflow that is a file of its agency's
// repository is committed, with what it needs, into another agency's
// repository (POST /agencies/{agencyId}/assign). The same action takes an
// update: assigning again replaces the files whose origin changed. The server
// decides everything; this dialog picks the agency and a scope for each origin
// scope, and shows the answer whole.

type Assignment = components["schemas"]["Assignment"];
type Scope = components["schemas"]["Scope"];
type Job = components["schemas"]["Job"];
type Workflow = components["schemas"]["Workflow"];
type AgencyRow = { id?: string; name?: string };

/** A refusal's body: the code, the sentence, and what it names. */
type Refusal = { code?: string; message?: string; paths?: string[]; calendars?: string[]; upstreams?: string[] };

const ACTION: Record<string, string> = {
  added: "added",
  usesOwn: "the agency's own, used",
  replaced: "replaced (the origin changed)",
  unchanged: "unchanged",
};

/** The jobs and sub-workflows a step graph runs, as the server walks it. */
function stepRefs(steps: unknown[] | undefined, jobs: Set<string>, wfs: Set<string>) {
  for (const raw of steps ?? []) {
    const s = raw as Record<string, unknown>;
    const nested = (k: string) => (Array.isArray(s[k]) ? (s[k] as unknown[]) : undefined);
    if (s.type === "workflow") {
      if (typeof s.workflow === "string" && s.workflow) wfs.add(s.workflow);
    } else if ((s.type === "job" || !s.type) && typeof s.name === "string" && !nested("jobs") && !nested("steps") && !s.pass && !s.fail) {
      jobs.add(s.name);
    }
    stepRefs(nested("jobs"), jobs, wfs);
    stepRefs(nested("steps"), jobs, wfs);
    for (const arm of ["pass", "fail"]) {
      const a = s[arm] as { steps?: unknown[] } | undefined;
      stepRefs(a?.steps, jobs, wfs);
    }
  }
}

/** The scopes a workflow's jobs run on in its repository ("" for a job with none), followed through its sub-workflows. */
export function workflowOriginScopes(wf: Workflow, workflows: Workflow[], jobs: Job[]): string[] {
  const sameRepo = (r?: string) => r === wf.repoId;
  const seenWf = new Set<string>();
  const jobNames = new Set<string>();
  const visit = (w: Workflow) => {
    if (!w.name || seenWf.has(w.name)) return;
    seenWf.add(w.name);
    const subs = new Set<string>();
    stepRefs(w.steps as unknown[] | undefined, jobNames, subs);
    for (const name of subs) {
      const sub = workflows.find((x) => x.name === name && x.source === "git" && sameRepo(x.repoId));
      if (sub) visit(sub);
    }
  };
  visit(wf);
  const scopes = new Set<string>();
  for (const name of jobNames) {
    const j = jobs.find((x) => x.name === name && x.source === "git" && sameRepo(x.repoId));
    scopes.add(j?.scope ?? "");
  }
  return [...scopes].sort();
}

export function AssignDialog({
  kind,
  uid,
  name,
  originAgency,
  originScopes,
  onClose,
}: {
  kind: "job" | "workflow";
  uid: string;
  name: string;
  originAgency?: string;
  /** The scopes the copied jobs run on in the origin ("" for none); null while still being worked out. */
  originScopes: string[] | null;
  onClose: () => void;
}) {
  const agenciesQ = useGet<unknown>(() => api.GET("/agencies"), []);
  // Every agency's scopes (a global administrator reads them all): the target's
  // are those it owns alone, which is what its sync accepts for a job.
  const scopesQ = useGet<unknown>(() => api.GET("/scopes", { headers: allReadable }), []);
  const agencies = rows<AgencyRow>(agenciesQ.data).filter((a) => a.id && a.id !== originAgency);
  const [target, setTarget] = useState("");
  const targetScopes = useMemo(
    () =>
      rows<Scope>(scopesQ.data)
        .filter((s) => s.agencies?.length === 1 && s.agencies[0].id === target)
        .map((s) => s.name ?? s.scope ?? "")
        .filter(Boolean)
        .sort(),
    [scopesQ.data, target],
  );
  const [pick, setPick] = useState<Record<string, string>>({});
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<Assignment | null>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  // A target with one scope: every origin scope maps to it.
  useEffect(() => {
    if (targetScopes.length === 1 && originScopes) {
      setPick(Object.fromEntries(originScopes.map((s) => [s, targetScopes[0]])));
    } else {
      setPick({});
    }
  }, [target, targetScopes, originScopes]);

  const targetName = agencies.find((a) => a.id === target)?.name ?? target;
  const ready = !!target && !!originScopes && originScopes.every((s) => pick[s]);

  async function submit() {
    if (!originScopes) return;
    setBusy(true);
    setRefusal(null);
    const scopes = Object.fromEntries(originScopes.map((s) => [s, pick[s]]));
    const { data, error } = await api.POST("/agencies/{agencyId}/assign", {
      params: { path: { agencyId: target }, header: csrfHeader },
      body: { kind, uid, scope: pick[originScopes[0]], scopes, ...(message.trim() ? { commitMessage: message.trim() } : {}) },
    });
    setBusy(false);
    if (error) setRefusal(error as Refusal);
    else setResult(data as Assignment);
  }

  const label = { fontSize: c.fontXs, fontWeight: 600, color: c.textSec, marginBottom: 4 } as const;
  const select = { padding: "6px 8px", borderRadius: c.radiusChip, border: `1px solid ${c.border}`, background: c.panelInput, color: c.text, fontSize: c.fontSm } as const;

  if (result) {
    return (
      <Modal title={`${kind === "job" ? "Job" : "Workflow"} ${name} given to ${targetName}`} wide onClose={onClose} footer={<Btn primary onClick={onClose}>Close</Btn>}>
        <div style={{ fontSize: c.fontSm, color: c.text, marginBottom: 12, lineHeight: 1.6 }}>
          Committed to {targetName}'s repository on <strong>{result.branch}</strong> as{" "}
          <code style={{ fontFamily: c.mono }}>{result.commitSha.slice(0, 12)}</code>, from the origin at{" "}
          <code style={{ fontFamily: c.mono }}>{result.originCommit.slice(0, 12)}</code>.{" "}
          {result.syncStarted && `${targetName}'s sync has started: the ${kind} appears there when it finishes.`}
        </div>
        <div style={label}>Files</div>
        <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm, marginBottom: 12 }}>
          <tbody>
            {result.files.map((f) => (
              <tr key={f.path} style={{ borderTop: `1px solid ${c.borderLight}` }}>
                <td style={{ padding: "4px 6px", fontFamily: c.mono, fontSize: c.fontXs }}>{f.path}</td>
                <td style={{ padding: "4px 6px", color: c.textSec }}>{f.role}</td>
                <td style={{ padding: "4px 6px" }}>
                  {ACTION[f.action] ?? f.action}
                  {f.differs && (
                    <span style={{ color: c.warning }} title="Its file is not the origin's: a comparison of the two files, not a record of who changed what.">
                      {" "}· differs from the origin's file
                    </span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {result.bindings.length > 0 && (
          <>
            <div style={label}>Bindings to make again in {targetName}</div>
            <ul style={{ margin: "0 0 12px 18px", fontSize: c.fontSm }}>
              {result.bindings.map((b, i) => (
                <li key={i}>
                  {b.kind} <strong>{b.name}</strong>
                  {b.as ? ` as ${b.as}` : ""}
                </li>
              ))}
            </ul>
          </>
        )}
        {result.lacks.length > 0 && (
          <>
            <div style={label}>What {targetName} does not have</div>
            <ul style={{ margin: "0 0 0 18px", fontSize: c.fontSm }}>
              {result.lacks.map((l, i) => (
                <li key={i}>
                  {l.kind} <strong>{l.name}</strong>
                </li>
              ))}
            </ul>
          </>
        )}
      </Modal>
    );
  }

  const footer = (
    <div style={{ display: "flex", flexDirection: "column", gap: 8, width: "100%" }}>
      {refusal && (
        <div role="alert" style={{ color: c.danger, fontSize: c.fontSm }}>
          {refusal.message ?? refusal.code ?? "The assignment was refused."}
          {(refusal.paths?.length ?? 0) > 0 && <div style={{ fontFamily: c.mono, fontSize: c.fontXs }}>{refusal.paths!.join(", ")}</div>}
          {(refusal.calendars?.length ?? 0) > 0 && <div>Calendars: {refusal.calendars!.join(", ")}</div>}
          {(refusal.upstreams?.length ?? 0) > 0 && <div>Reacts to: {refusal.upstreams!.join(", ")}</div>}
        </div>
      )}
      <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
        <Btn onClick={onClose}>Cancel</Btn>
        <Btn primary disabled={!ready || busy} onClick={submit}>
          {busy ? "Committing…" : "Give to the agency"}
        </Btn>
      </div>
    </div>
  );

  return (
    <Modal title={`Give ${kind} ${name} to an agency`} wide onClose={onClose} footer={footer}>
      <div style={{ fontSize: c.fontSm, color: c.textSec, marginBottom: 14, lineHeight: 1.6 }}>
        One commit into the agency's repository carries the {kind}
        {kind === "job" ? ", its script and its schedules" : ", its jobs with their scripts and schedules, and its sub-workflows"}, as
        they are now; what the agency already has by name is its own and is used. Giving it again takes an update: only
        the files that changed here are replaced.
      </div>
      <div style={{ marginBottom: 14 }}>
        <div style={label}>Agency</div>
        {agenciesQ.loading ? (
          <InlineLoading />
        ) : (
          <select aria-label="Agency" value={target} onChange={(e) => setTarget(e.target.value)} style={select}>
            <option value="">Choose an agency…</option>
            {agencies.map((a) => (
              <option key={a.id} value={a.id}>
                {a.name}
              </option>
            ))}
          </select>
        )}
      </div>
      {target && (
        <div style={{ marginBottom: 14 }}>
          <div style={label}>Scopes in {targetName}</div>
          {originScopes == null || scopesQ.loading ? (
            <InlineLoading />
          ) : targetScopes.length === 0 ? (
            <div style={{ color: c.danger, fontSize: c.fontSm }}>
              {targetName} owns no scope of its own: a job in an agency's repository must name one.
            </div>
          ) : (
            originScopes.map((s) => (
              <div key={s} style={{ display: "flex", alignItems: "center", gap: 10, marginBottom: 6, fontSize: c.fontSm }}>
                <span style={{ minWidth: 220 }}>{s ? <>Jobs on <strong>{s}</strong></> : "Jobs with no scope"}</span>
                <select
                  aria-label={`Scope for ${s || "jobs with no scope"}`}
                  value={pick[s] ?? ""}
                  onChange={(e) => setPick((p) => ({ ...p, [s]: e.target.value }))}
                  style={select}
                >
                  <option value="">Choose…</option>
                  {targetScopes.map((t) => (
                    <option key={t} value={t}>
                      {t}
                    </option>
                  ))}
                </select>
              </div>
            ))
          )}
        </div>
      )}
      <div>
        <div style={label}>Commit message (optional)</div>
        <input
          aria-label="Commit message"
          value={message}
          onChange={(e) => setMessage(e.target.value)}
          placeholder={`cronomicon: assign ${kind} ${name}`}
          style={{ ...select, width: "100%" }}
        />
      </div>
    </Modal>
  );
}

/** The dialog for a workflow: its origin scopes are its jobs', worked out from the
 * jobs of the agency in use (where the workflow is) and its sub-workflows. */
export function WorkflowAssignDialog({ wf, workflows, onClose }: { wf: Workflow; workflows: Workflow[]; onClose: () => void }) {
  const jobsQ = useGet<unknown>(() => api.GET("/jobs"), []);
  const originScopes = jobsQ.loading ? null : workflowOriginScopes(wf, workflows, rows<Job>(jobsQ.data));
  return (
    <AssignDialog
      kind="workflow"
      uid={wf.uid ?? ""}
      name={wf.name ?? ""}
      originAgency={wf.ownerAgency}
      originScopes={originScopes}
      onClose={onClose}
    />
  );
}

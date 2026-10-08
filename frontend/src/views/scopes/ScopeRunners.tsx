import { useEffect, useMemo, useState } from "react";
import { api, csrfHeader, errMsg } from "../../api/client";
import { useGet, rows } from "../../hooks";
import type { components } from "../../api/schema";
import { c } from "../../theme";
import { Link } from "react-router-dom";
import { AlertBanner, Btn, InlineLoading, Modal } from "../../components/ui";

// Scope↔runner bindings — the UI (SB band).
//
// A scope may name the runners allowed to serve it. "Only this runner reaches
// this VLAN" is a fact about where a scope's HOSTS are, so it is set here, on
// the scope, by whoever may configure the app — not on each job that happens to
// target it, which is where the runner-tag pin this replaced used to put it.
//
// Three rules shape everything in this file, and all three are the server's:
//
//   - A scope with no bound runner is unrestricted: any runner eligible for its
//     agency may serve it, and shell jobs default to SSH from the server.
//   - A binding outlives its runner. A runner that has been deregistered is
//     still listed, still restricts the scope, and nothing can claim the scope's
//     runs until it is replaced. That is deliberate — the alternative is a
//     confined scope quietly reopening to its whole agency — so the UI's job is
//     to make the state LOUD, never to tidy it away.
//   - Binding changes more than dispatch. Jobs with no executor of their own
//     move from SSH (the server's keys and known_hosts) to the runners (theirs);
//     jobs that ask for SSH start being refused. So the dialog previews before
//     it saves.

export type BoundRunner = components["schemas"]["BoundRunner"];
type Preview = components["schemas"]["ScopeRunnersPreview"];
type RetiredPin = components["schemas"]["RetiredRunnerPin"];

/** The slice of a scope row these components read. */
export interface BindableScope {
  id?: string;
  scope: string;
  agencies?: { id: string; name: string }[];
  boundRunners?: BoundRunner[];
}

/** The slice of a runner row these components read. */
export interface RunnerLite {
  id?: string;
  name: string;
  status?: string | null;
  agencies?: { id: string; name: string }[];
  /** "server" for the local runner; an agent otherwise. */
  kind?: string;
}

const isReachable = (status?: string | null) => status === "online" || status === "degraded";

/** True when the binding names a runner that could claim work right now. */
export const canServe = (b: BoundRunner) => b.registered && b.eligible && isReachable(b.status);

/** One phrase for why a bound runner is not serving; "" when it is. */
export function boundRunnerState(b: BoundRunner): string {
  if (!b.registered) return "deregistered";
  if (!b.eligible) return "not in this scope's agency";
  if (!isReachable(b.status)) return b.status || "offline";
  return "";
}

/** The scope's agency by name ("Global" for one with none listed: where every scope without another agency is). */
export function scopePoolName(scope: BindableScope): string {
  const names = (scope.agencies ?? []).map((a) => a.name);
  return names.length === 0 ? "Global" : names.join(", ");
}

// A runner is eligible for a scope by the claim query's agency rule: it serves
// one of the scope's agencies (Global is one, since 2.3.0, and a scope with no
// other agency is in it). The server enforces it on save; this copy only
// decides what the picker offers, so an ineligible runner is shown disabled
// with the reason instead of being offered and then refused. A scope that
// lists no agency at all is damage, and no runner is eligible for it.
// The local runner (kind "server") is not offered as a binding target: until it
// claims by the runner rule, a scope bound to it would send its jobs where
// nothing takes them, and the server refuses the save. It is left out of the
// pickers, not shown disabled — there is nothing an operator could do about it.
const bindable = (r: RunnerLite) => r.kind !== "server";

function eligibleFor(scope: BindableScope, runner: RunnerLite): boolean {
  const scopeAgencies = new Set((scope.agencies ?? []).map((a) => a.id));
  return (runner.agencies ?? []).some((a) => scopeAgencies.has(a.id));
}

const chip = (tone: "plain" | "warning"): React.CSSProperties => ({
  display: "inline-flex",
  alignItems: "center",
  gap: 6,
  padding: "2px 8px",
  borderRadius: c.radiusChip,
  fontSize: c.fontXs,
  fontFamily: c.mono,
  background: tone === "warning" ? c.warningBg : c.panel2,
  border: `1px solid ${tone === "warning" ? c.warning : c.border}`,
  color: c.text,
  whiteSpace: "nowrap",
});

// BoundRunnersCell is the catalog column. It answers one question at a glance —
// is this scope confined, and is anyone home — and leaves the detail to the row.
export function BoundRunnersCell({ bound }: { bound?: BoundRunner[] }) {
  const list = bound ?? [];
  if (list.length === 0) {
    return (
      <span
        title="Not bound — any runner eligible for this scope's agency may run its jobs."
        style={{ color: c.textSec, fontSize: c.fontXs, fontStyle: "italic", cursor: "help" }}
      >
        any eligible
      </span>
    );
  }
  const serving = list.filter(canServe).length;
  const label = list.length === 1 ? list[0].name : `${list[0].name} +${list.length - 1}`;
  const title =
    serving === 0
      ? `Bound to ${list.map((b) => b.name).join(", ")}, and none can claim work right now — this scope's runs are waiting.`
      : `Bound to ${list.map((b) => b.name).join(", ")} — ${serving} of ${list.length} able to claim work.`;
  return (
    <span title={title} style={{ display: "inline-flex", alignItems: "center", gap: 6, maxWidth: "100%" }}>
      {serving === 0 && (
        <span aria-label="No bound runner can claim work" style={{ color: c.warning, fontSize: c.fontSm }}>
          ⚠
        </span>
      )}
      <span style={{ ...chip(serving === 0 ? "warning" : "plain"), overflow: "hidden", textOverflow: "ellipsis" }}>{label}</span>
    </span>
  );
}

// ScopeRunnersField is the expanded-row section: what the scope is bound to, in
// words, and the way in to change it. Read-only for a caller who cannot
// configure the app, like every other scope write in this view.
export function ScopeRunnersField({
  scope,
  canEdit,
  onSaved,
}: {
  scope: BindableScope;
  canEdit: boolean;
  onSaved: (message: string) => void;
}) {
  const [editing, setEditing] = useState(false);
  const [replacing, setReplacing] = useState<BoundRunner | null>(null);
  const bound = scope.boundRunners ?? [];
  const serving = bound.filter(canServe).length;

  return (
    <div style={{ marginTop: 12 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
        <span style={{ fontFamily: c.sansCond, textTransform: "uppercase", fontSize: c.fontXs, letterSpacing: 0.5, color: c.textSec }}>
          Runners{bound.length ? ` (${bound.length})` : ""}
        </span>
        {bound.length === 0 ? (
          <span style={{ color: c.textSec, fontSize: c.fontSm }}>
            Not bound — any runner that serves {scopePoolName(scope)} may run this scope's jobs, and shell jobs default to SSH
            from the server.
          </span>
        ) : (
          bound.map((b) => {
            const state = boundRunnerState(b);
            return (
              <span key={b.runnerId} style={chip(state ? "warning" : "plain")}>
                {b.name}
                {state && <span style={{ color: c.warning, fontFamily: c.sans }}>· {state}</span>}
                {canEdit && !b.registered && (
                  <button
                    onClick={() => setReplacing(b)}
                    title="Hand this runner's scopes to another runner"
                    style={{ background: "none", border: "none", padding: 0, margin: 0, color: c.primary, cursor: "pointer", fontSize: c.fontXs, fontFamily: c.sans, textDecoration: "underline" }}
                  >
                    replace
                  </button>
                )}
              </span>
            );
          })
        )}
        {canEdit && scope.id != null && (
          <Btn small onClick={() => setEditing(true)}>
            {bound.length === 0 ? "Bind runners…" : "Change…"}
          </Btn>
        )}
      </div>
      {bound.length > 0 && (
        <div style={{ marginTop: 6, fontSize: c.fontSm, color: serving === 0 ? c.warning : c.textSec }}>
          {serving === 0 ? (
            <>
              ⚠ No bound runner can claim work right now, so this scope's runs are waiting. A binding is kept when its
              runner goes away — on purpose, so the scope does not quietly reopen to every runner that serves {scopePoolName(scope)}.
            </>
          ) : (
            <>Only these runners may run this scope's jobs. Jobs with no executor of their own run on them; SSH is refused.</>
          )}
        </div>
      )}
      {editing && scope.id != null && (
        <ScopeRunnersDialog
          scope={scope}
          onClose={() => setEditing(false)}
          onSaved={(message) => {
            setEditing(false);
            onSaved(message);
          }}
        />
      )}
      {replacing && (
        <ReplaceRunnerDialog
          from={{ id: replacing.runnerId, name: replacing.name }}
          onClose={() => setReplacing(null)}
          onDone={(message) => {
            setReplacing(null);
            onSaved(message);
          }}
        />
      )}
    </div>
  );
}

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

function JobNames({ jobs }: { jobs: { name: string }[] }) {
  const shown = jobs.slice(0, 6).map((j) => j.name);
  const more = jobs.length - shown.length;
  return (
    <span style={{ fontFamily: c.mono, fontSize: c.fontXs }}>
      {shown.join(", ")}
      {more > 0 ? ` +${more} more` : ""}
    </span>
  );
}

// PreviewPanel renders what the server says the save would do. Every line is a
// consequence the form cannot show on its own; a save with none of them says so
// rather than leaving the panel empty.
function PreviewPanel({ preview, selectedCount }: { preview: Preview; selectedCount: number }) {
  const lines: { tone: "info" | "warning"; node: React.ReactNode }[] = [];
  if (preview.jobsMovingToRunner.length > 0) {
    lines.push({
      tone: "info",
      node: (
        <>
          <strong>{plural(preview.jobsMovingToRunner.length, "job")}</strong> will move from SSH on the server to the bound
          runners — they will connect with the runners' own keys and known hosts: <JobNames jobs={preview.jobsMovingToRunner} />
        </>
      ),
    });
  }
  if (preview.jobsRefused.length > 0) {
    lines.push({
      tone: "warning",
      node: (
        <>
          <strong>{plural(preview.jobsRefused.length, "job")}</strong> {preview.jobsRefused.length === 1 ? "asks" : "ask"} for
          the SSH executor and will be refused while the scope is bound, until that setting is removed:{" "}
          <JobNames jobs={preview.jobsRefused} />
        </>
      ),
    });
  }
  if (preview.jobsMovingToSsh.length > 0) {
    lines.push({
      tone: "warning",
      node: (
        <>
          <strong>{plural(preview.jobsMovingToSsh.length, "job")}</strong> will go back to running over SSH from the
          server: <JobNames jobs={preview.jobsMovingToSsh} />
        </>
      ),
    });
  }
  if (preview.queuedSshRuns > 0) {
    lines.push({
      tone: "warning",
      node: (
        <>
          <strong>{plural(preview.queuedSshRuns, "run")}</strong> already queued or scheduled for SSH will still run from
          the server; the binding applies to runs produced after it is saved.
        </>
      ),
    });
  }
  for (const r of preview.runners) {
    if (!r.registered) {
      lines.push({ tone: "warning", node: <><strong>{r.name}</strong> is no longer registered and cannot claim work.</> });
      continue;
    }
    if (!r.eligible) {
      lines.push({ tone: "warning", node: <><strong>{r.name}</strong> is not in this scope's agency; the save will be refused.</> });
      continue;
    }
    if (r.missingRunTypes.length > 0) {
      lines.push({
        tone: "warning",
        node: (
          <>
            <strong>{r.name}</strong> cannot run {r.missingRunTypes.join(", ")} jobs, which this scope has.
          </>
        ),
      });
    }
    if (preview.jobsNeedingInjection > 0 && !r.allowsSecretInjection) {
      lines.push({
        tone: "warning",
        node: (
          <>
            <strong>{r.name}</strong> may not receive secrets, and {plural(preview.jobsNeedingInjection, "job")} on this
            scope {preview.jobsNeedingInjection === 1 ? "binds" : "bind"} one — enable secret injection on the runner.
          </>
        ),
      });
    }
    // The move to a runner moves host-key trust with it: the server's own pins
    // stop mattering and this runner's known_hosts starts to.
    if (r.hostsWithoutKey > 0) {
      lines.push({
        tone: "warning",
        node: (
          <>
            <strong>{r.name}</strong> does not yet trust {r.hostsWithoutKey} of this scope's {plural(preview.scopeHosts, "host")}. Its runs
            on {r.hostsWithoutKey === 1 ? "that host" : "those hosts"} fail at the first connection until you scan the scope from
            that runner and approve the keys.
          </>
        ),
      });
    }
  }
  if (preview.willBeBound && preview.runners.length === 1 && preview.runners[0].registered) {
    lines.push({
      tone: "info",
      node: <>One runner serves this scope: if it is offline, the scope's runs wait. Binding a second removes that.</>,
    });
  }

  return (
    <div aria-live="polite" style={{ marginTop: 14 }}>
      <div style={{ fontFamily: c.sansCond, textTransform: "uppercase", fontSize: c.fontXs, letterSpacing: 0.5, color: c.textSec, marginBottom: 6 }}>
        What saving will change
      </div>
      {lines.length === 0 ? (
        <div style={{ fontSize: c.fontSm, color: c.textSec }}>
          {selectedCount === 0 && !preview.currentlyBound
            ? "Nothing — the scope stays unbound."
            : "No job changes executor, and every chosen runner can serve what runs on this scope."}
        </div>
      ) : (
        <ul style={{ margin: 0, padding: 0, listStyle: "none", display: "grid", gap: 6 }}>
          {lines.map((l, i) => (
            <li key={i} style={{ fontSize: c.fontSm, color: c.text, paddingLeft: 10, borderLeft: `3px solid ${l.tone === "warning" ? c.warning : c.info}` }}>
              {l.node}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// ScopeRunnersDialog edits a scope's bound-runner set. The set is a full
// replace, so the dialog starts from what is bound today — ghosts included,
// since unticking one is how it is removed — and Save is disabled until the
// preview for the CURRENT selection has arrived: the point of the preview is
// that it is read before the change, not after.
export function ScopeRunnersDialog({
  scope,
  onClose,
  onSaved,
}: {
  scope: BindableScope;
  onClose: () => void;
  onSaved: (message: string) => void;
}) {
  const runnersQ = useGet<unknown>(() => api.GET("/runners"), []);
  const fleet = rows<RunnerLite>(runnersQ.data).filter(bindable);
  const bound = scope.boundRunners ?? [];
  const initial = useMemo(() => bound.map((b) => b.runnerId).sort(), [bound]);
  const [selected, setSelected] = useState<string[]>(initial);
  const [preview, setPreview] = useState<{ key: string; data: Preview } | null>(null);
  const [previewErr, setPreviewErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [saveErr, setSaveErr] = useState<string | null>(null);

  const selKey = [...selected].sort().join("|");
  const dirty = selKey !== initial.join("|");

  // Preview every selection, latest wins. A stale answer for an earlier
  // selection is dropped by its key rather than shown against this one.
  useEffect(() => {
    let live = true;
    setPreviewErr(null);
    api
      .POST("/scopes/{scopeId}/runners/preview", {
        params: { path: { scopeId: scope.id! }, header: csrfHeader },
        body: { runnerIds: selected },
      })
      .then(({ data, error }) => {
        if (!live) return;
        if (error || !data) setPreviewErr(error ? errMsg(error) : "no preview returned");
        else setPreview({ key: selKey, data: data as Preview });
      });
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selKey, scope.id]);

  const toggle = (id: string) =>
    setSelected((cur) => (cur.includes(id) ? cur.filter((x) => x !== id) : [...cur, id]));

  // Rows: every registered runner, plus any binding whose runner is gone. The
  // ghosts come first — they are the reason someone opened this dialog.
  const fleetIds = new Set(fleet.map((r) => String(r.id)));
  const ghosts = bound.filter((b) => !fleetIds.has(b.runnerId));
  const eligible = fleet.filter((r) => eligibleFor(scope, r) || initial.includes(String(r.id)));
  const ineligible = fleet.filter((r) => !eligibleFor(scope, r) && !initial.includes(String(r.id)));

  const save = async () => {
    setBusy(true);
    setSaveErr(null);
    const { error } = await api.PUT("/scopes/{scopeId}/runners", {
      params: { path: { scopeId: scope.id! }, header: csrfHeader },
      body: { runnerIds: selected },
    });
    setBusy(false);
    if (error) {
      setSaveErr(errMsg(error));
      return;
    }
    onSaved(selected.length === 0 ? `Binding removed: ${scope.scope}` : `Runners bound: ${scope.scope}`);
  };

  const previewReady = preview?.key === selKey && !previewErr;
  const row = (id: string, name: string, note: string, opts: { disabled?: boolean; warn?: boolean }) => (
    <label
      key={id}
      title={opts.disabled ? note : undefined}
      style={{
        display: "flex",
        alignItems: "center",
        gap: 10,
        padding: "7px 10px",
        borderBottom: `1px solid ${c.borderLight}`,
        cursor: opts.disabled ? "not-allowed" : "pointer",
        opacity: opts.disabled ? 0.6 : 1,
      }}
    >
      <input type="checkbox" checked={selected.includes(id)} disabled={opts.disabled || busy} onChange={() => toggle(id)} />
      <span style={{ fontFamily: c.mono, fontSize: c.fontSm, color: c.text }}>{name}</span>
      <span style={{ marginLeft: "auto", fontSize: c.fontXs, color: opts.warn ? c.warning : c.textSec }}>{note}</span>
    </label>
  );

  return (
    <Modal
      title={`Runners for ${scope.scope}`}
      onClose={onClose}
      footer={
        <div style={{ display: "flex", alignItems: "center", gap: 10, justifyContent: "flex-end", flexWrap: "wrap" }}>
          {saveErr && <span role="alert" style={{ color: c.danger, fontSize: c.fontSm, marginRight: "auto" }}>{saveErr}</span>}
          <Btn onClick={onClose} disabled={busy}>
            Cancel
          </Btn>
          <Btn
            primary
            onClick={save}
            disabled={busy || !dirty || !previewReady}
            title={!dirty ? "Nothing changed" : !previewReady ? "Waiting for the preview of this selection" : undefined}
          >
            {busy ? "Saving…" : selected.length === 0 ? "Remove binding" : `Bind ${plural(selected.length, "runner")}`}
          </Btn>
        </div>
      }
    >
      <p style={{ margin: "0 0 12px", fontSize: c.fontSm, color: c.textSec }}>
        Choose the runners that can reach this scope's hosts. With none chosen, any runner that serves {scopePoolName(scope)} may
        run its jobs. Runs already queued follow the change.
      </p>
      {runnersQ.loading ? (
        <InlineLoading what="runners" />
      ) : (
        <div style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, overflow: "hidden" }}>
          {ghosts.map((g) => row(g.runnerId, g.name, "deregistered — untick to remove", { warn: true }))}
          {eligible.map((r) =>
            row(String(r.id), r.name, isReachable(r.status) ? r.status ?? "" : r.status || "offline", { warn: !isReachable(r.status) }),
          )}
          {ineligible.map((r) =>
            row(
              String(r.id),
              r.name,
              `does not serve ${scopePoolName(scope)}`,
              { disabled: true },
            ),
          )}
          {ghosts.length + eligible.length + ineligible.length === 0 && (
            <div style={{ padding: 12, fontSize: c.fontSm, color: c.textSec }}>No runners are registered.</div>
          )}
        </div>
      )}
      {previewErr ? (
        <div role="alert" style={{ marginTop: 14, fontSize: c.fontSm, color: c.danger }}>
          Could not preview this change: {previewErr}
        </div>
      ) : previewReady ? (
        <PreviewPanel preview={preview!.data} selectedCount={selected.length} />
      ) : (
        <div style={{ marginTop: 14 }}>
          <InlineLoading what="the effect of this change" />
        </div>
      )}
    </Modal>
  );
}

// ReplaceRunnerDialog hands every scope one runner is bound to over to another,
// in one step — "this host was replaced". The runner being replaced is usually
// deregistered already, which is why it arrives as an id and a name and not as
// a row from the fleet.
export function ReplaceRunnerDialog({
  from,
  onClose,
  onDone,
}: {
  from: { id: string; name: string };
  onClose: () => void;
  /** `to` is the runner that took over, so the caller can offer the next step. */
  onDone: (message: string, to: { id: string; name: string }) => void;
}) {
  const runnersQ = useGet<unknown>(() => api.GET("/runners"), []);
  const candidates = rows<RunnerLite>(runnersQ.data).filter((r) => String(r.id) !== from.id && bindable(r));
  const [to, setTo] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const replace = async () => {
    setBusy(true);
    setErr(null);
    const { data, error } = await api.POST("/scope-runners/replace", {
      params: { header: csrfHeader },
      body: { fromRunnerId: from.id, toRunnerId: to },
    });
    setBusy(false);
    if (error) {
      setErr(errMsg(error));
      return;
    }
    const scopes = (data as { scopes?: string[] } | undefined)?.scopes ?? [];
    const toName = candidates.find((r) => String(r.id) === to)?.name ?? to;
    onDone(`${toName} now serves ${scopes.length === 0 ? "the scopes" : scopes.join(", ")} in place of ${from.name}`, { id: to, name: toName });
  };

  return (
    <Modal
      title={`Replace ${from.name}`}
      onClose={onClose}
      footer={
        <div style={{ display: "flex", alignItems: "center", gap: 10, justifyContent: "flex-end", flexWrap: "wrap" }}>
          {err && <span role="alert" style={{ color: c.danger, fontSize: c.fontSm, marginRight: "auto" }}>{err}</span>}
          <Btn onClick={onClose} disabled={busy}>
            Cancel
          </Btn>
          <Btn primary onClick={replace} disabled={busy || to === ""}>
            {busy ? "Replacing…" : "Replace"}
          </Btn>
        </div>
      }
    >
      <p style={{ margin: "0 0 12px", fontSize: c.fontSm, color: c.textSec }}>
        Every scope bound to <strong>{from.name}</strong> will be bound to the runner you choose instead, in one step.
        The replacement must be eligible for each of those scopes' agencies; if it is not for any one of them, nothing
        changes and the scopes in the way are named.
      </p>
      {runnersQ.loading ? (
        <InlineLoading what="runners" />
      ) : (
        <label style={{ display: "grid", gap: 6, fontSize: c.fontSm, color: c.textSec }}>
          Replacement runner
          <select
            value={to}
            onChange={(e) => setTo(e.target.value)}
            disabled={busy}
            style={{ fontSize: c.fontSm, padding: "6px 8px", borderRadius: c.radiusChip, background: c.panelInput, color: c.text, border: `1px solid ${c.borderStrong}` }}
          >
            <option value="">Choose a runner…</option>
            {candidates.map((r) => (
              <option key={String(r.id)} value={String(r.id)}>
                {r.name}
                {isReachable(r.status) ? "" : ` (${r.status || "offline"})`}
              </option>
            ))}
          </select>
        </label>
      )}
    </Modal>
  );
}

// BindingNotices says, on the Scopes page, how many jobs lost the confinement a
// runner tag used to give them: the runner pins that could not be turned into a
// scope binding — by the upgrade, or by a Git job still carrying `runner_tag`
// on a scope nobody bound. Each of those jobs USED to be confined to particular
// runners and is not any more, which is the one thing that change must not let
// happen quietly.
//
// It is a pointer since 2.3.0 (LR-85): the list itself, with why each pin could
// not be converted and the dismissal, is in the Notices inbox, which is where
// every standing condition is read. This banner stays because the remedy is
// made here, by binding the scope's runners; a notice disappears by itself when
// its scope is bound.
export function BindingNotices({ dep }: { dep: number; onChanged?: () => void }) {
  const q = useGet<unknown>(() => api.GET("/scope-binding-notices"), [dep]);
  const notices = rows<RetiredPin>(q.data);
  if (notices.length === 0) return null;
  const scopes = [...new Set(notices.map((n) => n.scope || "").filter(Boolean))].sort();
  return (
    <AlertBanner type="warning">
      <span>
        <strong>{plural(notices.length, "job")}</strong> used to be confined to particular runners by a runner tag and{" "}
        {notices.length === 1 ? "is" : "are"} not any more
        {scopes.length > 0 ? (
          <>
            {" "}
            (scope{scopes.length === 1 ? "" : "s"} <span style={{ fontFamily: c.mono }}>{scopes.join(", ")}</span>)
          </>
        ) : null}
        . Bind the scope's runners below to confine {notices.length === 1 ? "it" : "them"} again.{" "}
        <Link to="/notices" style={{ color: c.primary, fontWeight: 600 }}>
          Review in Notices
        </Link>
      </span>
    </AlertBanner>
  );
}

// RunsOn is the job detail's answer to "where does this run": the bound runners
// when the job's scope has any, each with the reason it is not serving if it is
// not, and otherwise the plain statement that any eligible runner may take it.
export function RunsOn({ bound, scope }: { bound: BoundRunner[]; scope?: string | null }) {
  if (bound.length === 0) {
    return <span style={{ color: c.textSec }}>Any eligible runner</span>;
  }
  return (
    <span style={{ display: "inline-flex", gap: 6, flexWrap: "wrap", alignItems: "center" }}>
      {bound.map((b) => {
        const state = boundRunnerState(b);
        return (
          <span key={b.runnerId} style={chip(state ? "warning" : "plain")}>
            {b.name}
            {state && <span style={{ color: c.warning, fontFamily: c.sans }}>· {state}</span>}
          </span>
        );
      })}
      {scope && <span style={{ color: c.textSec, fontSize: c.fontXs }}>bound to scope {scope}</span>}
    </span>
  );
}

// useBoundRunners answers "which runners is this scope bound to" for views that
// hold only a scope NAME — the job detail and the Run dialog. It reads the same
// GET /scopes every scope picker already uses, so it shows nothing a caller who
// can see the scope could not already see. An empty name, an unknown one, and a
// scope with no binding all yield [].
export function useBoundRunners(scope: string | null | undefined): { bound: BoundRunner[]; loading: boolean } {
  const q = useGet<unknown>(() => api.GET("/scopes"), []);
  const bound = useMemo(() => {
    if (!scope) return [];
    const row = rows<BindableScope>(q.data).find((s) => s.scope === scope);
    return row?.boundRunners ?? [];
  }, [q.data, scope]);
  return { bound, loading: q.loading };
}

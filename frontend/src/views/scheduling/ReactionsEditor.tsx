// RX-15 — inline reaction authoring, mirroring how calendar bindings landed in
// the composer surfaces in CAL 1C.
//
// A reaction is NOT part of the job/workflow body: it has its own endpoint
// (PUT /reactions/{ownerKind}/{ownerName}), because a reaction's identity is
// (owner, name) and the API replaces the set wholesale. So the composer saves
// the definition first and then the reactions, exactly as it already does for
// SSH-key bindings — including the "the job was saved but its X were not"
// message, which is the honest report when the second call is the one that
// failed.
//
// Two consequences the UI has to state rather than hide:
//
//   - A reaction can only be authored against a definition that EXISTS, since
//     the endpoint addresses it by name. In create mode the section says so
//     instead of collecting input it would have to throw away.
//   - Reactions on a GIT-source definition are authored in Git. The API 409s,
//     deliberately: an in-app write would be replaced by the next sync, so the
//     editor points at `spec.reactions` rather than letting someone lose work.
import { useEffect, useState } from "react";
import { api, csrfHeader } from "../../api/client";
import { rows, useGet } from "../../hooks";
import { useNameDisambiguator } from "../../utils/disambiguate";
import { c } from "../../theme";
import { Btn } from "../../components/ui";
import type { components } from "../../api/schema";
import { input, label } from "./ui";

export type ReactionDraft = components["schemas"]["ReactionInput"];
type ServerReaction = components["schemas"]["Reaction"];

// Byte-matches reactionNameRe on the server and the YAML path's slug rule. The
// three must agree, or "move this reaction into Git" fails on a name the app
// accepted.
export const REACTION_NAME_RE = /^[a-z0-9][a-z0-9_-]{0,63}$/;

export const OUTCOMES = ["success", "failure", "stopped", "any"] as const;

// A function, not a shared const: each row gets its own object, so editing one
// can never mutate another through an accidentally shared reference.
export const emptyReaction = (n: number): ReactionDraft => ({
  name: `reaction-${n}`,
  onKind: "job",
  onName: "",
  onSource: "cronomicon",
  onOutcome: "success",
  delaySeconds: 0,
  minIntervalSeconds: 0,
  includeWorkflowChildren: false,
  enabled: true,
});

// reactionToWire is the preservedInline defence, and the reason it is a mapped
// return type rather than a spread: the PUT is a WHOLESALE REPLACE, so a field
// this editor forgets to send is a field the save silently deletes. The mapped
// type makes the compiler refuse to build if a field is ever added to
// ReactionInput and not listed here — which is exactly the drift that bit the
// calendar bindings and the workflow editor's inline schedules.
export function reactionToWire(r: ReactionDraft): { [K in keyof Required<ReactionDraft>]: ReactionDraft[K] } {
  return {
    name: r.name,
    onKind: r.onKind,
    onName: r.onName,
    onSource: r.onSource,
    onOutcome: r.onOutcome,
    delaySeconds: r.delaySeconds,
    minIntervalSeconds: r.minIntervalSeconds,
    includeWorkflowChildren: r.includeWorkflowChildren,
    enabled: r.enabled,
  };
}

// Fills every field from the server row so a value this editor does not render
// still survives the round trip rather than being dropped by the next save.
export function reactionFromServer(r: ServerReaction): ReactionDraft {
  return {
    name: r.name ?? "",
    onKind: r.onKind ?? "job",
    onName: r.onName ?? "",
    onSource: r.onSource ?? "git",
    onOutcome: r.onOutcome ?? "success",
    delaySeconds: r.delaySeconds ?? 0,
    minIntervalSeconds: r.minIntervalSeconds ?? 0,
    includeWorkflowChildren: r.includeWorkflowChildren ?? false,
    enabled: r.enabled ?? true,
  };
}

// The client-side mirror of the server's 422s, in the same order, so the inline
// message and the server's refusal say the same thing.
//
// Deliberately NOT mirrored: cycle detection (cross-plane — it needs the whole
// stored edge set, including edges authored in Git that this form cannot see)
// and the min-interval retention ceiling (the window is a server setting). Both
// surface as the server's own 422 text, which is better than a client guess
// that could disagree.
export function reactionsError(
  owner: { kind: "job" | "workflow"; name: string; source?: string },
  list: ReactionDraft[],
): string | null {
  const seen = new Set<string>();
  for (const r of list) {
    const n = (r.name ?? "").trim();
    if (!n) return "every reaction needs a name";
    if (!REACTION_NAME_RE.test(n)) {
      return `${n}: name must be a slug — lowercase letters, digits, '-' or '_'`;
    }
    if (seen.has(n)) return `duplicate reaction name ${n}`;
    seen.add(n);
    if (!(r.onName ?? "").trim()) return `${n}: choose what this reacts to`;
    if (
      r.onKind === owner.kind &&
      r.onName === owner.name &&
      (r.onSource ?? "git") === (owner.source ?? "cronomicon")
    ) {
      return `${n}: a definition cannot react to itself`;
    }
    if ((r.delaySeconds ?? 0) < 0 || (r.minIntervalSeconds ?? 0) < 0) {
      return `${n}: delay and minimum interval cannot be negative`;
    }
  }
  return null;
}

type Target = { kind: string; name: string; source: string; uid?: string; agencies?: string[] };

// The picker's options: every job and workflow, carrying its SOURCE. The option
// value encodes kind:source:name so all three are set atomically — picking a
// name and inferring its source separately is how a reaction ends up watching
// the wrong one of two same-named definitions.
function useReactionTargets(): { targets: Target[]; loading: boolean } {
  const jobsQ = useGet<unknown>(() => api.GET("/jobs", { params: { query: { page: 1, pageSize: 200 } } }), []);
  const wfQ = useGet<unknown>(() => api.GET("/workflows", { params: { query: { page: 1, pageSize: 200 } } }), []);
  const targets: Target[] = [
    // R2F-3 — uid + agencies ride along so an option can qualify a name two
    // departments share. The stored EDGE is still (kind, source, name) plus the
    // on_uid stamped at write (R2-2); this is display only.
    ...rows<{ name?: string; source?: string; uid?: string; agencies?: string[] }>(jobsQ.data).map((j) => ({
      kind: "job",
      name: j.name ?? "",
      source: j.source ?? "git",
      uid: j.uid,
      agencies: j.agencies,
    })),
    ...rows<{ name?: string; source?: string; uid?: string; agencies?: string[] }>(wfQ.data).map((w) => ({
      kind: "workflow",
      name: w.name ?? "",
      source: w.source ?? "git",
      uid: w.uid,
      agencies: w.agencies,
    })),
  ]
    .filter((t) => t.name)
    .sort((a, b) => (a.kind === b.kind ? a.name.localeCompare(b.name) : a.kind.localeCompare(b.kind)));
  return { targets, loading: jobsQ.loading || wfQ.loading };
}

const targetKey = (t: { kind?: string; source?: string; name?: string }) =>
  `${t.kind ?? "job"}:${t.source ?? "git"}:${t.name ?? ""}`;

export function ReactionsEditor({
  value,
  onChange,
  ownerKind,
  ownerName,
  ownerSource,
  disabled,
  loadError,
}: {
  value: ReactionDraft[];
  onChange: (next: ReactionDraft[]) => void;
  ownerKind: "job" | "workflow";
  ownerName: string;
  ownerSource?: string;
  disabled?: boolean;
  loadError?: string | null;
}) {
  const { targets } = useReactionTargets();
  // R2F-3 — the option list is the visible set. Jobs and workflows share it, so
  // a job and a workflow of the same name do NOT badge each other: the `kind ·`
  // prefix already tells those apart, and ambiguity is measured per name across
  // whichever definitions actually collide.
  const targetLabel = useNameDisambiguator(targets, (t) => ({
    uid: t.uid,
    name: t.name,
    source: t.source,
    agencies: t.agencies,
    group: t.kind,
  }));
  const set = (i: number, patch: Partial<ReactionDraft>) =>
    onChange(value.map((r, j) => (j === i ? { ...r, ...patch } : r)));

  // A git-source definition's reactions live in its YAML. Saying where, rather
  // than only refusing, is the difference between a dead end and a redirect.
  if (ownerSource === "git") {
    return (
      <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
        This {ownerKind} is defined in Git, so its reactions are authored there too — add a{" "}
        <code>spec.reactions</code> list to its YAML. An in-app edit here would be replaced by the next sync.
      </div>
    );
  }

  // Refuse to edit from an unread state — see useLoadedReactions.
  if (loadError) {
    return (
      <div style={{ fontSize: c.fontSm, color: c.danger, lineHeight: 1.6 }}>
        Could not load this {ownerKind}'s reactions: {loadError}. Editing is disabled so a save cannot replace
        reactions that were never read.
      </div>
    );
  }

  if (!ownerName.trim()) {
    return (
      <div style={{ fontSize: c.fontSm, color: c.textMuted, lineHeight: 1.6 }}>
        Save this {ownerKind} first — a reaction is attached to it by name, so there has to be something to attach to.
      </div>
    );
  }

  return (
    <div>
      {/* EP-7 — the static intro that used to sit here now lives in each host's
          Field ⓘ (Job Composer and Workflow Editor), because both hosts are
          composer surfaces with the same wall-of-prose problem and lifting it
          fixes the twins together rather than propping one out. The editor's
          OTHER messages stay: "Save this {kind} first" and the git-source
          pointer above are CONSEQUENCES of current state, not education. */}
      {value.map((r, i) => (
        <div
          key={i}
          style={{
            border: `1px solid ${c.border}`,
            borderRadius: c.radiusSurface,
            padding: 12,
            marginBottom: 10,
            background: c.panel2,
            opacity: r.enabled === false ? 0.6 : 1,
          }}
        >
          <div style={{ display: "flex", gap: 10, flexWrap: "wrap" }}>
            <div style={{ flex: "1 1 160px", minWidth: 140 }}>
              <label style={label()}>Name</label>
              <input
                style={{ ...input(), fontFamily: c.mono, marginBottom: 8 }}
                value={r.name ?? ""}
                disabled={disabled}
                onChange={(e) => set(i, { name: e.target.value })}
              />
            </div>
            <div style={{ flex: "2 1 240px", minWidth: 200 }}>
              <label style={label()}>When this finishes</label>
              <select
                style={{ ...input(), marginBottom: 8 }}
                disabled={disabled}
                value={targetKey(r)}
                onChange={(e) => {
                  const [kind, source, ...rest] = e.target.value.split(":");
                  set(i, { onKind: kind as "job" | "workflow", onSource: source as "git" | "cronomicon", onName: rest.join(":") });
                }}
              >
                <option value={targetKey(r)}>{r.onName ? `${r.onKind} · ${r.onName}` : "Choose…"}</option>
                {targets
                  .filter((t) => targetKey(t) !== targetKey(r))
                  .map((t) => (
                    <option key={targetKey(t)} value={targetKey(t)}>
                      {t.kind} · {targetLabel(t)}
                      {t.source === "git" ? " (git)" : ""}
                    </option>
                  ))}
              </select>
            </div>
            <div style={{ flex: "1 1 130px", minWidth: 120 }}>
              <label style={label()}>With outcome</label>
              <select
                style={{ ...input(), marginBottom: 8 }}
                disabled={disabled}
                value={r.onOutcome ?? "success"}
                onChange={(e) => set(i, { onOutcome: e.target.value as ReactionDraft["onOutcome"] })}
              >
                {OUTCOMES.map((o) => (
                  <option key={o} value={o}>{o}</option>
                ))}
              </select>
            </div>
          </div>

          <div style={{ display: "flex", gap: 10, flexWrap: "wrap", alignItems: "flex-end" }}>
            <div style={{ flex: "0 1 120px" }}>
              <label style={label()}>Delay (s)</label>
              <input
                type="number"
                min={0}
                style={{ ...input(), marginBottom: 8 }}
                disabled={disabled}
                value={r.delaySeconds ?? 0}
                onChange={(e) => set(i, { delaySeconds: Number(e.target.value) || 0 })}
              />
            </div>
            <div style={{ flex: "0 1 150px" }}>
              <label style={label()} title="At most one run per this interval. An event arriving inside it is DROPPED, not queued — the event has already been consumed, so there is no instant to retry at.">
                Min interval (s)
              </label>
              <input
                type="number"
                min={0}
                style={{ ...input(), marginBottom: 8 }}
                disabled={disabled}
                value={r.minIntervalSeconds ?? 0}
                onChange={(e) => set(i, { minIntervalSeconds: Number(e.target.value) || 0 })}
              />
            </div>
            <label style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: c.fontXs, color: c.textSec, marginBottom: 14 }}>
              <input
                type="checkbox"
                disabled={disabled}
                checked={r.enabled !== false}
                onChange={(e) => set(i, { enabled: e.target.checked })}
              />
              enabled
            </label>
            {r.onKind === "job" && (
              <label
                title="Also fire when that job runs as a step INSIDE a workflow. Off by default, because it gives the parent workflow fan-out that appears nowhere in its step graph."
                style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: c.fontXs, color: c.textSec, marginBottom: 14 }}
              >
                <input
                  type="checkbox"
                  disabled={disabled}
                  checked={!!r.includeWorkflowChildren}
                  onChange={(e) => set(i, { includeWorkflowChildren: e.target.checked })}
                />
                incl. workflow steps
              </label>
            )}
            <Btn
              small
              danger
              disabled={disabled}
              style={{ marginBottom: 14, marginLeft: "auto" }}
              onClick={() => onChange(value.filter((_, j) => j !== i))}
            >
              Remove
            </Btn>
          </div>
        </div>
      ))}

      <Btn small disabled={disabled} onClick={() => onChange([...value, emptyReaction(value.length + 1)])}>
        + Add reaction
      </Btn>
    </div>
  );
}

// loadReactionsFor fetches one definition's authored reactions as drafts.
export async function loadReactionsFor(kind: "job" | "workflow", name: string): Promise<ReactionDraft[]> {
  const { data, error } = await api.GET("/reactions/{ownerKind}/{ownerName}", {
    params: { path: { ownerKind: kind, ownerName: name } },
  });
  if (error) {
    // Thrown, not swallowed: the caller has to be able to tell "none" from
    // "could not read", because a wholesale-replace save treats them the same.
    const e = error as { message?: string };
    throw new Error(e?.message ?? "could not load this definition's reactions");
  }
  return rows<ServerReaction>(data).map(reactionFromServer);
}

// saveReactionsFor replaces the definition's whole reaction list. Returns an
// error string, matching putBindings' contract so the composer's
// "saved, but its reactions were not" path reads the same way.
export async function saveReactionsFor(
  kind: "job" | "workflow",
  name: string,
  list: ReactionDraft[],
): Promise<string | null> {
  const { error } = await api.PUT("/reactions/{ownerKind}/{ownerName}", {
    params: { path: { ownerKind: kind, ownerName: name }, header: csrfHeader },
    body: { reactions: list.map(reactionToWire) },
  } as never);
  if (!error) return null;
  const e = error as { message?: string };
  return e?.message ?? "request failed";
}

// useLoadedReactions loads an existing definition's reactions once, and reports
// the baseline so the caller can tell whether they were edited.
export function useLoadedReactions(kind: "job" | "workflow", name: string, enabled: boolean) {
  const [list, setList] = useState<ReactionDraft[]>([]);
  const [baseline, setBaseline] = useState<ReactionDraft[]>([]);
  // A FAILED load must not look like "this definition has no reactions".
  //
  // The save is a wholesale replace, so an editor that silently started from an
  // empty list would delete every existing reaction the moment the operator
  // added one — destroying rows it never managed to read. `loadError` is what
  // turns that into a visible refusal instead of quiet data loss.
  const [loadError, setLoadError] = useState<string | null>(null);
  const [loaded, setLoaded] = useState(false);
  useEffect(() => {
    if (!enabled || !name.trim()) return;
    let live = true;
    setLoaded(false);
    setLoadError(null);
    loadReactionsFor(kind, name)
      .then((rs) => {
        if (!live) return;
        setList(rs);
        setBaseline(rs);
        setLoaded(true);
      })
      .catch((e: unknown) => {
        if (!live) return;
        setLoadError(e instanceof Error ? e.message : "could not load this definition's reactions");
      });
    return () => {
      live = false;
    };
  }, [kind, name, enabled]);
  // Never dirty while the load has not succeeded: a save from an unread state
  // would replace real rows with whatever this form happens to hold.
  const dirty =
    (loaded || !enabled) &&
    JSON.stringify(list.map(reactionToWire)) !== JSON.stringify(baseline.map(reactionToWire));
  return { list, setList, baseline, setBaseline, dirty, loadError };
}

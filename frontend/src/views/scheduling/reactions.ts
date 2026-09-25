// RX — the shared reaction model, mirroring calendars.ts's split between the
// fetch and the pure helpers so every surface reads one source.
//
// There is deliberately no "who reacts to me" endpoint: the API filters the edge
// list by OWNER only, because both directions are the same edge read from
// opposite ends and a second endpoint would be a second thing to keep correct.
// The whole list is one small fetch, already scope-filtered server-side, so both
// directions are derived here.
import { api } from "../../api/client";
import { rows, useGet } from "../../hooks";
import type { components } from "../../api/schema";

export type Reaction = components["schemas"]["Reaction"];

export function useReactionEdges(refresh = 0): { edges: Reaction[]; loading: boolean; error: string | null } {
  const { data, error, loading } = useGet<unknown>(() => api.GET("/reactions", {}), [refresh]);
  // rows() rather than a cast: it returns [] for anything that is neither an
  // array nor a paged envelope, which keeps a view from exploding on a mocked
  // or future-shaped response.
  return { edges: rows<Reaction>(data), loading, error };
}

// A definition's identity is (kind, source, name) — NEVER the name alone.
//
// The two sources are disjoint namespaces and the same name can legitimately
// exist in both, so matching on name merges two definitions' graphs into one.
// That is the exact defect the Phase C review caught on the backend's own
// per-definition read; it would be the same bug here, one layer up.
const sameDef =
  (kind: string, source: string | undefined, name: string | undefined) =>
    (k?: string, s?: string, n?: string) =>
      k === kind && (s ?? "git") === (source ?? "git") && n === name;

/** Edges where this definition is the one that RUNS. */
export function reactsTo(edges: Reaction[], kind: string, source?: string, name?: string): Reaction[] {
  const eq = sameDef(kind, source, name);
  return edges.filter((r) => eq(r.ownerKind, r.ownerSource, r.ownerName));
}

/** Edges where this definition is the one being WATCHED. */
export function reactedOnBy(edges: Reaction[], kind: string, source?: string, name?: string): Reaction[] {
  const eq = sameDef(kind, source, name);
  return edges.filter((r) => eq(r.onKind, r.onSource, r.onName));
}

// RX-24 — both definition-delete routes 409 for two unrelated reasons: the row
// is git-source (never clearable here), or reactions watch it (clearable with
// ?force=true). Branching on the status alone cannot tell them apart, and the
// four delete surfaces did exactly that for a release — every refusal rendered
// "Only cronomicon-source jobs can be deleted in-app", which is guaranteed FALSE
// for this one, since the git check has already passed by the time it fires.
//
// Keyed on the server's error CODE rather than its prose, so rewording the
// refusal cannot silently disable the force affordance — the same discipline
// the History calendar filter uses against its stored field.
const REACTIONS_WATCHING = "reactions_watching";

/**
 * The refusal message when a delete was blocked by watching reactions, or null
 * for any other failure. A non-null return is the caller's signal to offer
 * "delete anyway" — it is precisely the set of refusals ?force=true clears.
 */
export function reactionsWatchingRefusal(error: unknown): string | null {
  if (!error || typeof error !== "object") return null;
  const e = error as { code?: unknown; message?: unknown };
  if (e.code !== REACTIONS_WATCHING) return null;
  if (typeof e.message !== "string" || !e.message) return "Reactions watch this definition.";
  // The server's message ends by naming the override — "pass ?force=true to
  // delete and leave those reactions dangling" — which is the right thing to
  // tell an API caller and noise beside a button that IS the override. Drop the
  // tail, keep the part only the server can supply: which reactions blocked it.
  return e.message.replace(/\s*—\s*pass \?force=true.*$/s, "").trim();
}

/**
 * The refusal plus what forcing it actually costs, as one sentence for the
 * text-only confirm rows in the two editors. One copy, because an operator who
 * reads it in the Composer and again in the Workflow Editor should not have to
 * work out whether two different wordings mean two different things.
 */
export function deleteBlockedNote(refusal: string): string {
  return `${refusal} Deleting anyway keeps them, flagged "missing" on Schedules → Reactions, where they can never fire until you repoint or delete them.`;
}

/** "30s" / "5m" / "2h" / "—" for a zero (disabled) interval. */
export function secondsLabel(n?: number): string {
  if (!n || n <= 0) return "—";
  if (n % 3600 === 0) return `${n / 3600}h`;
  if (n % 60 === 0) return `${n / 60}m`;
  return `${n}s`;
}

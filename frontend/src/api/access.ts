// "Which agencies do I administer?" — read from GET /me/access (LR-87).
//
// GET /capabilities answers "may I do this SOMEWHERE" (the flat flags) and "may
// I do it for the whole installation" (the …Global flags). Since 2.3.0 a third
// question has an answer worth showing: an agency's administrators enrol their
// own agents, own their host records and bastions, and name the one agency a new
// scope, secret, variable or key belongs to. A form that asks "whose is this?"
// needs the list of agencies the caller may answer with, and that is what the
// server reports here, grant by grant.
//
// This is for POPULATING a picker and for wording. It is not a gate: the server
// decides every write, and a control that needs a yes/no reads the capability
// flags or a per-row flag, as before.
import { useEffect, useState } from "react";
import { api } from "./client";
import type { components } from "./schema";

export type MyAccess = components["schemas"]["MyAccess"];
export type AgencyRef = { id: string; name: string };

/** The id of the built-in Global agency (migration 1220). */
export const GLOBAL_AGENCY = "global";

export async function fetchMyAccess(): Promise<MyAccess | null> {
  try {
    const res = await fetch("/api/v1/me/access", { credentials: "include" });
    if (!res.ok) return null;
    return (await res.json()) as MyAccess;
  } catch {
    return null;
  }
}

/** True when one all-agencies grant carries the permission: a global administrator for it. */
export function isGlobalFor(access: MyAccess | null, permission: string): boolean {
  return !!access?.grants.some((g) => g.allScopes && g.permissions.includes(permission));
}

/**
 * The agencies the caller holds `permission` on through an agency grant, sorted
 * by name, each once. A global administrator's reach is not listed here (it is
 * every agency): ask isGlobalFor, and read the catalog for the names.
 */
export function agenciesFor(access: MyAccess | null, permission: string): AgencyRef[] {
  const seen = new Map<string, string>();
  for (const g of access?.grants ?? []) {
    if (g.allScopes || !g.agencyId || !g.permissions.includes(permission)) continue;
    if (!seen.has(g.agencyId)) seen.set(g.agencyId, g.agencyName || g.agencyId);
  }
  return [...seen].map(([id, name]) => ({ id, name })).sort((a, b) => a.name.localeCompare(b.name));
}

/** useMyAccess loads the caller's grants once. `undefined` while loading, `null` when it could not be read. */
export function useMyAccess(): MyAccess | null | undefined {
  const [access, setAccess] = useState<MyAccess | null | undefined>(undefined);
  useEffect(() => {
    let cancelled = false;
    fetchMyAccess().then((a) => {
      if (!cancelled) setAccess(a);
    });
    return () => {
      cancelled = true;
    };
  }, []);
  return access;
}

/**
 * useAgencyIdByName resolves an agency NAME to its id through the catalog.
 * Rows that show their owner carry its name (it is what a person reads); the
 * routes that ask about an agency take its id. Names are unique. `undefined`
 * until known, and for a name the catalog does not hold.
 */
export function useAgencyIdByName(name: string | undefined): string | undefined {
  const [id, setId] = useState<string | undefined>(undefined);
  useEffect(() => {
    if (!name) {
      setId(undefined);
      return;
    }
    if (name === "Global") {
      setId(GLOBAL_AGENCY);
      return;
    }
    let cancelled = false;
    api.GET("/agencies").then((res) => {
      const list = (res.data as { id?: string; name?: string }[] | undefined) ?? [];
      if (!cancelled) setId((Array.isArray(list) ? list : []).find((a) => a.name === name)?.id);
    });
    return () => {
      cancelled = true;
    };
  }, [name]);
  return id;
}

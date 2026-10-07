// An agency names Vault paths inside the prefixes assigned to it (LR-80, v2.3.0).
//
// A global administrator assigns each agency a list of path prefixes on the
// installation's one Vault connection (Scopes → Agencies). A Vault-backed
// secret or SSH key that the agency owns must lie inside one of them; Global's
// rows are a global administrator's and have no path limit.
//
// What is here is the form's side of that: read the owner's prefixes, and say
// before submit whether a path is inside one. The check mirrors the server's
// (internal/vaultpath: whole segments, case-sensitive, the path as written),
// and it is a HINT: the server decides every write, and a reference this file
// accepted may still be refused there.
import { useEffect, useState } from "react";
import { api } from "./client";
import { GLOBAL_AGENCY } from "./access";

/** The path part of a `path#field` reference. */
export function vaultRefPath(ref: string): string {
  const i = ref.lastIndexOf("#");
  return i >= 0 ? ref.slice(0, i) : ref;
}

function segments(path: string): string[] | null {
  const p = path.replace(/^\/+|\/+$/g, "");
  if (!p) return null;
  const segs = p.split("/");
  for (const s of segs) {
    if (s === "" || s === "." || s === ".." || s !== s.trim()) return null;
    // eslint-disable-next-line no-control-regex
    if (/[%\\?#\u0000-\u001f\u007f]/.test(s)) return null;
  }
  return segs;
}

/**
 * Is `ref` inside one of `prefixes`? A prefix contains a path when its segments
 * are the path's first segments: `secret/data/tax` contains `secret/data/tax/db`
 * and not `secret/data/tax-audit/db`. A reference that begins or ends with a
 * slash, or has an empty, `.` or `..` segment, is inside nothing.
 */
export function vaultPathAllowed(ref: string, prefixes: string[]): boolean {
  const path = vaultRefPath(ref.trim());
  if (path.startsWith("/") || path.endsWith("/")) return false;
  const ps = segments(path);
  if (!ps) return false;
  return prefixes.some((prefix) => {
    const xs = segments(prefix);
    return !!xs && xs.length <= ps.length && xs.every((x, i) => x === ps[i]);
  });
}

export type VaultPrefixState =
  /** Global's row, or no owner chosen yet: nothing to check here. */
  | { kind: "unlimited" }
  | { kind: "loading" }
  /** The owner agency's prefixes. Empty means it can name no Vault path. */
  | { kind: "prefixes"; prefixes: string[] }
  /** The list could not be read (not the caller's agency, or an older server). */
  | { kind: "unknown" };

/**
 * useVaultPrefixes reads the Vault path prefixes of the agency that owns (or
 * will own) a row. `undefined`, "" and Global read as unlimited: with no owner
 * there is nothing to check yet, and Global's rows have no path limit.
 */
export function useVaultPrefixes(agencyId: string | undefined, enabled: boolean = true): VaultPrefixState {
  const [state, setState] = useState<VaultPrefixState>({ kind: "unlimited" });
  useEffect(() => {
    if (!enabled || !agencyId || agencyId === GLOBAL_AGENCY) {
      setState({ kind: "unlimited" });
      return;
    }
    let cancelled = false;
    setState({ kind: "loading" });
    api.GET("/agencies/{agencyId}/vault-prefixes", { params: { path: { agencyId } } }).then((res) => {
      if (cancelled) return;
      const prefixes = (res.data as { prefixes?: string[] } | undefined)?.prefixes;
      setState(res.error || !prefixes ? { kind: "unknown" } : { kind: "prefixes", prefixes });
    });
    return () => {
      cancelled = true;
    };
  }, [agencyId, enabled]);
  return state;
}

/**
 * Why a Vault path may not be named for this owner, or "" when it may. With a
 * `ref` it also judges that reference.
 */
export function vaultPathWhy(state: VaultPrefixState, agencyName: string, ref?: string): string {
  if (state.kind !== "prefixes") return "";
  if (state.prefixes.length === 0) {
    return `${agencyName} has no Vault paths assigned, so it cannot hold a Vault-backed row. A global administrator assigns them on Scopes → Agencies.`;
  }
  if (ref && ref.trim() && !vaultPathAllowed(ref, state.prefixes)) {
    return `That path is outside the Vault paths assigned to ${agencyName}: ${state.prefixes.join(", ")}.`;
  }
  return "";
}

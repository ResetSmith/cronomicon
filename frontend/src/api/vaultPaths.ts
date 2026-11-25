// An agency names references in its secret store inside its prefixes (LR-80,
// v2.3.0; any kind of store since 2.5.5).
//
// A global administrator assigns each agency without a store of its own a list
// of path prefixes on the installation's Vault (Scopes → Agencies). An external
// secret or SSH key the agency owns must lie inside one of them. An agency with
// a store of its own (2.5.1 SG-26) sets its own prefixes, optional: none means
// any reference in its store.
//
// Since 2.5.5 (SM-10) the form does NOT judge a reference itself: each kind of
// store has its own grammar (a Vault path, an AWS secret name or ARN, …), and
// mirroring each in TypeScript would drift. It reads the owner's prefixes, its
// provider and how to ask for a reference (`refHint`), and asks the server
// (`POST /agencies/{agencyId}/vault/validate-ref`) whether a reference may be
// named: the same judgement as the write, which still decides.
import { useEffect, useState } from "react";
import { api, csrfHeader } from "./client";
import { GLOBAL_AGENCY } from "./access";

export type RefHint = { label: string; placeholder: string; format: string };

/** HashiCorp's, for a server that sends no hint (and for Global's rows). */
export const VAULT_REF_HINT: RefHint = {
  label: "Vault path",
  placeholder: "secret/data/myapp#KEY_NAME",
  format: "A KV v2 path and a field: engine/data/path#FIELD (without #FIELD the field \"value\" is read).",
};

export type VaultPrefixState =
  /** Global's row, or no owner chosen yet: nothing to check here. */
  | { kind: "unlimited" }
  | { kind: "loading" }
  /** The owner agency's prefixes. Empty means it can name no reference,
   *  unless it has a store of its own (`ownVault`), where it means any. */
  | { kind: "prefixes"; prefixes: string[]; ownVault: boolean; provider: string; refHint: RefHint }
  /** The list could not be read (not the caller's agency, or an older server). */
  | { kind: "unknown" };

/** The hint for a state: the owner's provider's, else Vault's. */
export function refHintOf(state: VaultPrefixState): RefHint {
  return state.kind === "prefixes" ? state.refHint : VAULT_REF_HINT;
}

/**
 * useVaultPrefixes reads the prefixes, provider and reference hint of the
 * agency that owns (or will own) a row. `undefined`, "" and Global read as
 * unlimited: with no owner there is nothing to check yet, and Global's rows
 * have no path limit.
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
      const body = res.data as { prefixes?: string[]; ownVault?: boolean; provider?: string; refHint?: RefHint } | undefined;
      const prefixes = body?.prefixes;
      setState(
        res.error || !prefixes
          ? { kind: "unknown" }
          : {
              kind: "prefixes",
              prefixes,
              ownVault: !!body?.ownVault,
              provider: body?.provider || "hashicorp",
              refHint: body?.refHint ?? VAULT_REF_HINT,
            },
      );
    });
    return () => {
      cancelled = true;
    };
  }, [agencyId, enabled]);
  return state;
}

/**
 * Why the owner cannot name ANY reference, or "": an agency with no store of
 * its own and no prefixes assigned. Whether one reference may be named is the
 * server's to say (checkStoreRef).
 */
export function vaultPathWhy(state: VaultPrefixState, agencyName: string): string {
  if (state.kind !== "prefixes") return "";
  if (state.ownVault || state.prefixes.length > 0) return "";
  return `${agencyName} has no Vault paths assigned, so it cannot hold an external row. A global administrator assigns them on Scopes → Agencies, or ${agencyName} connects a secret store of its own.`;
}

/**
 * checkStoreRef asks the server whether the agency may name ref: "" when it
 * may, else the reason. `current` is the reference the row names now (an
 * edit that leaves it as it was is not judged again where only the grammar
 * would). A failed request is "" (the write decides; this is a hint).
 */
export async function checkStoreRef(agencyId: string, ref: string, current = "", label: "secret" | "key" = "secret"): Promise<string> {
  if (!agencyId || agencyId === GLOBAL_AGENCY || !ref.trim()) return "";
  const res = await api.POST("/agencies/{agencyId}/vault/validate-ref", {
    params: { path: { agencyId }, header: csrfHeader },
    body: { ref: ref.trim(), ...(current ? { current } : {}), label },
  });
  const v = res.data as { ok?: boolean; reason?: string } | undefined;
  if (res.error || !v || v.ok !== false) return "";
  return v.reason || "That reference cannot be named.";
}

/**
 * useStoreRefCheck is checkStoreRef as the user types: asked once the field
 * has been still for a moment, and the answer for the latest text only.
 */
export function useStoreRefCheck(agencyId: string | undefined, ref: string, current = "", enabled = true): string {
  const [why, setWhy] = useState("");
  useEffect(() => {
    setWhy("");
    if (!enabled || !agencyId || agencyId === GLOBAL_AGENCY || !ref.trim()) return;
    let cancelled = false;
    const t = setTimeout(() => {
      checkStoreRef(agencyId, ref, current).then((reason) => {
        if (!cancelled) setWhy(reason);
      });
    }, 300);
    return () => {
      cancelled = true;
      clearTimeout(t);
    };
  }, [agencyId, ref, current, enabled]);
  return why;
}

import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { api } from "./api/client";

// The authenticated operator (the /me payload). Kept as a stable local shape;
// the backend Identity carries these fields.
export interface Me {
  email: string;
  name?: string;
  roles?: string[];
  scopes?: string[];
  /** The caller's AD groups, as the OIDC claim gave them. Already in the /me
   *  payload (auth.Identity.Groups) — declared here so a surface that has to
   *  answer "is this mapping the one granting ME this role?" can (FX-9). */
  groups?: string[];
}

interface AuthState {
  me: Me | null;
  loading: boolean;
}

const AuthCtx = createContext<AuthState>({ me: null, loading: true });

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>({ me: null, loading: true });

  useEffect(() => {
    let cancelled = false;
    (async () => {
      const { data, error } = await api.GET("/me");
      if (cancelled) return;
      setState({ me: error ? null : (data as unknown as Me), loading: false });
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  return <AuthCtx.Provider value={state}>{children}</AuthCtx.Provider>;
}

export const useAuth = () => useContext(AuthCtx);

// canTriggerJobs reports whether the operator may run/kill/pause jobs per the
// role matrix (TriggerJobs/KillJobs = admin|approver|operator, not viewer).
//
// @deprecated Use the server-computed `triggerJobs` / `killJobs` capability flags
// (RB-3) instead — `fetchCapabilities()` in api/client.ts. This function hardcodes
// the permission matrix on the client, which is a duplicate source of truth that
// Phase 1 breaks: once roles are DB rows (RB-6/RB-7) a custom role carrying
// triggerJobs is not in this set, so its holders would lose the button while the
// API still accepts their request. The capability flag is computed from whatever
// the matrix actually says.
//
// Kept only for callers not yet migrated; there are none in-tree as of v0.56.0.
// It remains a UX affordance either way, never a security boundary — the run path
// still enforces scope but not the verb until RB-2 (v0.56.4).
const TRIGGER_ROLES = new Set(["admin", "approver", "operator"]);
export function canTriggerJobs(me: Me | null): boolean {
  return !!me?.roles?.some((r) => TRIGGER_ROLES.has(r.toLowerCase()));
}

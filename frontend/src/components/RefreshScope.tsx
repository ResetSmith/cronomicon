import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from "react";

// EP-3 (the expanded-panels plan) — the seam that lets ONE Refresh
// button re-fetch every request inside an expanded panel.
//
// Why a context and not threaded callbacks. A panel is not one fetch: JobDetail
// alone issues five, two of them inside nested components (useRunnerTags,
// JobKeyField) that the button holds no reference to. Collecting each hook's
// refetch and wiring them to the button works on the day it is written and then
// degrades silently — the next fetch someone adds to the panel is not wired, so
// the button quietly stops covering part of its own panel, with no error and no
// failing test. Explicit wiring fails by OMISSION, and omission failures are
// invisible.
//
// So the nonce is ambient: `useGet` reads this context and treats a bump as an
// extra dependency, in "refresh" mode (EP-2). Every fetch beneath a provider
// participates — present and future, own file or nested — with zero call-site
// changes.
//
// Two guards this design needs:
//   - The DEFAULT value is a live no-op (nonce 0, bump does nothing). Every
//     useGet outside a provider behaves exactly as it did before EP-3, which is
//     what keeps this additive across ~40 call sites.
//   - Scopes must NOT nest. Two nonces in one subtree means the inner half
//     answers to a different button than the outer half — a refresh that covers
//     part of a panel is worse than no refresh, because it looks complete.

export type RefreshScopeValue = {
  /** Bumped by `bump()`; useGet re-fetches (in refresh mode) when it changes. */
  nonce: number;
  /** Ask every fetch in this subtree to re-run. */
  bump: () => void;
  /** How many refresh-mode fetches are currently in flight in this subtree. */
  pending: number;
  /** Called by useGet around a refresh-mode fetch. No-ops outside a provider. */
  begin: () => void;
  end: () => void;
  /** False for the default value — how the provider detects nesting. */
  provided: boolean;
};

const noop = () => {};

const DEFAULT: RefreshScopeValue = {
  nonce: 0,
  bump: noop,
  pending: 0,
  begin: noop,
  end: noop,
  provided: false,
};

export const RefreshScopeCtx = createContext<RefreshScopeValue>(DEFAULT);

/** useRefreshScope — read the ambient scope. Outside a provider it is inert. */
export function useRefreshScope(): RefreshScopeValue {
  return useContext(RefreshScopeCtx);
}

/**
 * RefreshScope — wrap one expanded panel's content.
 *
 * `pending` is a real in-flight count, not a timer. A button disabled for a
 * fixed 600ms lies twice: it claims to still be working on a fast request and
 * claims to be finished on a slow one. useGet increments on a refresh-mode
 * fetch start and decrements when it settles — including when the component
 * unmounted mid-flight, or the count would leak upward forever.
 */
export function RefreshScope({ children }: { children: ReactNode }) {
  const outer = useContext(RefreshScopeCtx);
  const [nonce, setNonce] = useState(0);
  const [pending, setPending] = useState(0);

  if (import.meta.env.DEV && outer.provided) {
    // Not thrown: a nested scope still renders something usable, and taking a
    // whole panel down over a layout mistake is the worse trade. Loud enough to
    // be found in development, which is where it is fixable.
    console.warn(
      "RefreshScope is nested inside another RefreshScope — the inner subtree answers to a different Refresh button, so one button will refresh only part of its panel.",
    );
  }

  const bump = useCallback(() => setNonce((n) => n + 1), []);
  const begin = useCallback(() => setPending((p) => p + 1), []);
  const end = useCallback(() => setPending((p) => (p > 0 ? p - 1 : 0)), []);

  const value = useMemo<RefreshScopeValue>(
    () => ({ nonce, bump, pending, begin, end, provided: true }),
    [nonce, pending, bump, begin, end],
  );

  return <RefreshScopeCtx.Provider value={value}>{children}</RefreshScopeCtx.Provider>;
}

/**
 * useRefreshTarget — the Shape-B escape hatch (EP-4).
 *
 * Five panels (Scopes, the three Env Vars tabs, Git Sync, Activity, Users &
 * Access) render LIST-ROW data and fetch nothing of their own, so "refresh this
 * panel" there means "refetch the list". Those views pass their existing
 * refresh/dep/bump counter's setter here, and the panel's Refresh button drives
 * it alongside the ambient nonce.
 *
 * Kept in this module rather than invented per view so both shapes go through
 * one vocabulary — the button never has to know which kind of panel it is in.
 */
export function useRefreshAction(also?: () => void): { refresh: () => void; pending: number } {
  const scope = useRefreshScope();
  const alsoRef = useRef(also);
  alsoRef.current = also;
  const refresh = useCallback(() => {
    scope.bump();
    alsoRef.current?.();
  }, [scope]);
  return { refresh, pending: scope.pending };
}

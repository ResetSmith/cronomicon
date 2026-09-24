import { useEffect, useMemo, useRef, useState } from "react";
import { useRefreshScope } from "./components/RefreshScope";
import { usePager, type PagerState } from "./components/ui";
import { clampPage, sliceForPage } from "./utils/pager";
import { defaultDirFor, sortRows, type SortColumn, type SortDir } from "./utils/sort";
import { errMsg } from "./api/client";

// Minimal data-fetching hook over the typed client. Callers pass a thunk that
// runs an api.GET and returns { data, error }. Re-renders on load/error.
//
// intervalMs (optional) turns on background polling: after the initial load the
// thunk re-runs on that cadence so live views (e.g. the Jobs list catching a
// queued→running→done transition) stay current without a manual refresh. Poll
// refetches are SILENT — they don't toggle `loading` (so the view never flashes
// its skeleton on each tick) and a transient poll error keeps the last good data
// rather than wiping the table. Only the initial load shows the loader / clears
// to an error.
// EP-2 (20260818-expanded-panels.md) — a third fetch mode, "refresh", beside
// the original two. A manual refresh (the expanded panels' Refresh button, via
// refetch()) is neither of the modes that existed: "initial" flashes the
// skeleton over data the operator is reading, and "poll" swallows its errors —
// which would let a failed refresh look identical to a successful one to the
// person who just asked for it. Refresh keeps the last good data on screen
// while the request runs (no loading toggle; `refreshing` is its own flag for
// the button's spinner) and SURFACES a failure as `error`, keeping the stale
// data visible beneath it rather than blanking the panel.
export function useGet<T>(
  fetcher: () => Promise<{ data?: unknown; error?: unknown }>,
  deps: unknown[] = [],
  intervalMs?: number,
) {
  const [state, setState] = useState<{ data: T | null; error: string | null; loading: boolean; refreshing: boolean }>({
    data: null,
    error: null,
    loading: true,
    refreshing: false,
  });
  // refetch() marks the NEXT effect run as a refresh and bumps the nonce to
  // cause it. A ref, not effect-local state: the effect must be able to tell
  // "re-ran because refetch()" from "re-ran because a dep changed" (the latter
  // is a new identity — e.g. a different jobId — and must stay an initial load,
  // skeleton and all).
  const [nonce, setNonce] = useState(0);
  const refreshRequested = useRef(false);
  // EP-3 — the ambient panel scope. Outside a RefreshScope this is the inert
  // default (nonce 0, no-op begin/end), so every call site behaves as before.
  // Read through refs inside the effect: begin/end are stable useCallbacks, but
  // routing them through refs keeps the effect from depending on the context
  // object's identity, which changes on every pending-count tick.
  const scope = useRefreshScope();
  const scopeBegin = useRef(scope.begin);
  const scopeEnd = useRef(scope.end);
  scopeBegin.current = scope.begin;
  scopeEnd.current = scope.end;
  const lastScopeNonce = useRef(scope.nonce);
  useEffect(() => {
    let cancelled = false;
    // Three ways this effect can re-run, and only two of them are a refresh:
    // refetch() (explicit), a scope bump (the panel's Refresh button), or a
    // dependency change — which is a NEW IDENTITY (a different jobId, a
    // different page) and must stay a full initial load, skeleton and all.
    const scopeBumped = scope.nonce !== lastScopeNonce.current;
    lastScopeNonce.current = scope.nonce;
    const firstMode: "initial" | "refresh" = refreshRequested.current || scopeBumped ? "refresh" : "initial";
    refreshRequested.current = false;
    const run = (mode: "initial" | "poll" | "refresh") => {
      if (mode === "initial") setState((s) => ({ ...s, loading: true }));
      if (mode === "refresh") {
        setState((s) => ({ ...s, refreshing: true }));
        scopeBegin.current();
      }
      // `settled` guards the scope's in-flight count against a double
      // decrement; `.finally` runs even when the request was cancelled by an
      // unmount, which is what stops the count leaking upward forever and
      // wedging the button in its disabled state.
      let settled = false;
      const settle = () => {
        if (settled) return;
        settled = true;
        if (mode === "refresh") scopeEnd.current();
      };
      fetcher()
        .then(({ data, error }) => {
          if (cancelled) return;
          // Background poll hit a request-level error: leave the last good data
          // in place rather than clearing the view on a transient blip.
          if (mode === "poll" && error) return;
          setState((s) => ({
            // On a failed refresh the last good data stays (the ?? falls back to
            // it); an initial failure clears to null exactly as before.
            data: (data as T) ?? (mode === "refresh" ? s.data : null),
            error: error ? String((error as { message?: string })?.message ?? "request failed") : null,
            loading: false,
            refreshing: false,
          }));
        })
        .catch((e) => {
          // Never leave the view stuck on "Loading…" (or a button stuck on
          // "Refreshing…") if the request itself rejects (network error,
          // aborted, parse failure). Surface it on initial load AND on refresh
          // — the operator asked for the refresh and is watching; only a
          // background poll swallows it (keep the last good data).
          if (cancelled || mode === "poll") return;
          setState((s) => ({
            data: mode === "refresh" ? s.data : null,
            error: String((e as { message?: string })?.message ?? "request failed"),
            loading: false,
            refreshing: false,
          }));
        })
        .finally(settle);
    };
    run(firstMode);
    let timer: ReturnType<typeof setInterval> | undefined;
    if (intervalMs && intervalMs > 0) {
      timer = setInterval(() => run("poll"), intervalMs);
    }
    return () => {
      cancelled = true;
      if (timer) clearInterval(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, intervalMs, nonce, scope.nonce]);
  const refetch = () => {
    refreshRequested.current = true;
    setNonce((n) => n + 1);
  };
  return { ...state, refetch };
}

// useLiveGet wraps useGet to poll ONLY while there is in-flight work. It polls at
// `activeMs` while `isActive(data)` is true (e.g. any visible run is still
// running/queued) and stops once everything is terminal, so a settled view isn't
// polled needlessly. Active-state polls are silent (useGet's run(false) path); a
// deps change — e.g. a manual refresh after triggering a run — refetches once and
// re-arms polling if the fresh data is active again.
export function useLiveGet<T>(
  fetcher: () => Promise<{ data?: unknown; error?: unknown }>,
  deps: unknown[] = [],
  isActive: (data: T | null) => boolean = () => false,
  activeMs = 4000,
) {
  const [poll, setPoll] = useState<number | undefined>(activeMs);
  const state = useGet<T>(fetcher, deps, poll);
  useEffect(() => {
    // setPoll with an unchanged value is a no-op (React bails on Object.is), so
    // this settles after one transition rather than looping.
    setPoll(isActive(state.data) ? activeMs : undefined);
    // isActive is recreated each render; gating on state.data/activeMs is intended.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [state.data, activeMs]);
  return state;
}

// useColumnWidths (V1.1-7): per-table column widths persisted in localStorage
// under `colw:<tableId>`. Pair with the <ResizableTh> component (components/ui).
export function useColumnWidths(tableId: string) {
  const key = `colw:${tableId}`;
  const [widths, setWidths] = useState<Record<string, number>>(() => {
    try {
      const raw = localStorage.getItem(key);
      return raw ? (JSON.parse(raw) as Record<string, number>) : {};
    } catch {
      return {};
    }
  });
  const setWidth = (col: string, w: number) => {
    setWidths((prev) => {
      const next = { ...prev, [col]: w };
      try {
        localStorage.setItem(key, JSON.stringify(next));
      } catch {
        /* private browsing — keep in-memory only */
      }
      return next;
    });
  };
  const reset = () => {
    setWidths({});
    try {
      localStorage.removeItem(key);
    } catch {
      /* ignore */
    }
  };
  return { widths, setWidth, reset };
}

// useSidebarCollapsed: the Shell's icons-only sidebar preference, persisted in
// localStorage so a collapse survives reloads and new tabs. Same private-
// browsing tolerance as useColumnWidths — a failed write just means the choice
// is in-memory for this page.
export const SIDEBAR_COLLAPSED_KEY = "am_sidebar_collapsed";

export function useSidebarCollapsed(): [boolean, () => void] {
  const [collapsed, setCollapsed] = useState<boolean>(() => {
    try {
      return localStorage.getItem(SIDEBAR_COLLAPSED_KEY) === "1";
    } catch {
      return false;
    }
  });
  const toggle = () => {
    setCollapsed((prev) => {
      const next = !prev;
      try {
        localStorage.setItem(SIDEBAR_COLLAPSED_KEY, next ? "1" : "0");
      } catch {
        /* private browsing — keep in-memory only */
      }
      return next;
    });
  };
  return [collapsed, toggle];
}

// Pure response-shape helpers live in utils/paging (no React/DOM/theme imports so
// they're unit-testable in node); re-exported here so existing
// `import { rows, paged } from "../hooks"` call sites are unchanged.
export { rows, paged, type Paged } from "./utils/paging";

// useClientPager is the client-side analogue of the server `paged()` path: it
// pages an already-resident, already-filtered array in the browser (no extra
// fetch). It owns a usePager() and re-clamps the stored page against the current
// length each render, so a shrinking set (a deleted row, a tighter filter) pulls
// the user back into range instead of stranding them on an empty trailing page.
// Used where the payload is small and already loaded (Jobs/Scripts catalogs,
// Schedules › Upcoming). The returned `page` is the clamped index — pass it to
// <Pager page={page} …> so the control and the slice agree.
export function useClientPager<T>(items: T[]): { pageItems: T[]; total: number; page: number; pager: PagerState } {
  const pager = usePager();
  const total = items.length;
  const page = clampPage(pager.page, total, pager.pageSize);
  const pageItems = sliceForPage(items, page, pager.pageSize);
  return { pageItems, total, page, pager };
}

// useDebounced returns `value` delayed by `ms` — it re-emits only after the input
// has stopped changing for that window. Used to throttle search-as-you-type before
// it drives a server query (e.g. the Script picker's capped `?q=` fallback), so a
// burst of keystrokes issues one request, not one per character. ms=0 passes
// through on the next tick.
export function useDebounced<T>(value: T, ms: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setDebounced(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return debounced;
}

// useInlineTags — shared optimistic tag-edit plumbing for the catalog tables
// (Jobs, Scripts, Schedules, Workflows) and the Env Vars tabs (CC.18; hoisted
// from envvars/ui.tsx). Keeps a per-row override map so an edit shows immediately
// and survives the list re-render, a per-row monotonic seq so a superseded PUT
// can't clobber a newer edit, and a per-row error string; it reverts the override
// on failure. `dep` resets the maps on a fresh list load; `keyOf` identifies a
// row; `tagsOf` reads its authoritative (server) tags; `put` performs the write.
export function useInlineTags<T>(
  dep: number,
  keyOf: (row: T) => string,
  tagsOf: (row: T) => string[] | undefined,
  put: (row: T, next: string[]) => Promise<{ data?: { tags?: string[] }; error?: unknown }>,
) {
  const [overrides, setOverrides] = useState<Record<string, string[]>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  useEffect(() => {
    setOverrides({});
    setErrors({});
  }, [dep]);
  const seq = useRef<Record<string, number>>({});

  const tagsFor = (row: T): string[] => overrides[keyOf(row)] ?? tagsOf(row) ?? [];

  const save = async (row: T, next: string[]) => {
    const key = keyOf(row);
    const prev = tagsFor(row); // pre-edit snapshot for an error revert
    const mySeq = (seq.current[key] = (seq.current[key] ?? 0) + 1);
    setErrors((e) => {
      const { [key]: _drop, ...rest } = e;
      return rest;
    });
    setOverrides((o) => ({ ...o, [key]: next }));
    const res = await put(row, next);
    if (mySeq !== seq.current[key]) return; // a newer edit superseded this one
    if (res.error) {
      setOverrides((o) => ({ ...o, [key]: prev }));
      setErrors((e) => ({ ...e, [key]: errMsg(res.error) }));
      return;
    }
    setOverrides((o) => ({ ...o, [key]: res.data?.tags ?? next }));
  };

  return { tagsFor, save, errors };
}

// useInlineAnnotation (AN-3) — useInlineTags' shape for operator annotations.
//
// Same three moving parts and the same reasons: a per-row override map so an
// edit repaints the catalog row's Critical chip without waiting for a refetch, a
// per-row monotonic seq so a slow PUT cannot clobber a newer edit, and a per-row
// error that reverts the override.
//
// It differs from useInlineTags in where the authoritative value comes from.
// Tags are on every row, so that hook reads them itself; an annotation's notes
// are DETAIL-only, so the caller passes the right base per surface — the list
// row (chip + contact) for the table, the fetched detail (everything) for the
// expanded panel — and this hook only decides whether an override outranks it.
export function useInlineAnnotation<T, A extends object>(
  dep: number,
  keyOf: (row: T) => string,
  put: (row: T, next: A) => Promise<{ data?: A; error?: unknown }>,
) {
  const [overrides, setOverrides] = useState<Record<string, A>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  useEffect(() => {
    setOverrides({});
    setErrors({});
  }, [dep]);
  const seq = useRef<Record<string, number>>({});

  /** valueFor prefers a pending/just-saved override over the server's `base`. */
  const valueFor = (row: T, base: A): A => overrides[keyOf(row)] ?? base;

  const save = async (row: T, next: A) => {
    const key = keyOf(row);
    const prev = overrides[key]; // undefined ⇒ revert to "no override", not to a stale copy
    const mySeq = (seq.current[key] = (seq.current[key] ?? 0) + 1);
    setErrors((e) => {
      const { [key]: _drop, ...rest } = e;
      return rest;
    });
    setOverrides((o) => ({ ...o, [key]: next }));
    const res = await put(row, next);
    if (mySeq !== seq.current[key]) return; // a newer edit superseded this one
    if (res.error) {
      setOverrides((o) => {
        if (prev === undefined) {
          const { [key]: _drop, ...rest } = o;
          return rest;
        }
        return { ...o, [key]: prev };
      });
      setErrors((e) => ({ ...e, [key]: errMsg(res.error) }));
      return;
    }
    // The response carries the server's own view (trimmed contact, assigned
    // notesBy/notesAt), which is what the attribution line must render.
    setOverrides((o) => ({ ...o, [key]: res.data ?? next }));
  };

  return { valueFor, save, errors };
}

// useTableSort (TS-2/TS-4, the sorting-update plan): one sort idiom for every
// table. Columns are declared once (utils/sort SortColumn — extractor, type,
// optional rank map); the hook owns {key, dir}, sorts a copy of `rows`, and
// hands the header everything it needs (toggle / ariaSort / dir). Toggle
// semantics (lifted from the registration-token table): first click on a column
// applies its defaultDir — date/number columns open desc (newest/largest
// first), text asc — and a second click flips; picking a new column resets to
// that column's default. `tiebreak` keeps equal-valued rows deterministic.
//
// With a `tableId` the choice persists in localStorage under `sort:<tableId>`
// (TS-Q1), mirroring useColumnWidths' `colw:` — same private-browsing
// tolerance, and a stored key that no longer names a column is ignored.
// `onChange` fires on every toggle — server-paged callers reset to page 1 there.
//
// `serverSide` (TS-23): the table's ordering comes from the endpoint's
// ?sort=&order= params, so `sorted` passes rows through untouched — the hook
// only owns the header state (toggle/carets/aria/persistence). The caller
// feeds `sortKey`/`sortDir` into its query. Never re-sort a server-ordered
// page client-side: the two collations can disagree and rows would jump
// between pages.
export function useTableSort<T>(
  rows: T[],
  columns: SortColumn<T>[],
  defaultSort: { key: string; dir?: SortDir } | null,
  opts?: { tableId?: string; tiebreak?: Array<{ key: string; dir?: SortDir }>; onChange?: () => void; serverSide?: boolean },
) {
  const storageKey = opts?.tableId ? `sort:${opts.tableId}` : null;
  const resolve = (s: { key: string; dir?: SortDir } | null): { key: string; dir: SortDir } | null => {
    const col = s && columns.find((cl) => cl.key === s.key);
    return col ? { key: col.key, dir: s.dir ?? defaultDirFor(col) } : null;
  };
  const [sort, setSort] = useState<{ key: string; dir: SortDir } | null>(() => {
    if (storageKey) {
      try {
        const raw = localStorage.getItem(storageKey);
        if (raw) {
          const parsed = JSON.parse(raw) as { key?: string; dir?: string };
          const restored = resolve({ key: parsed.key ?? "", dir: parsed.dir === "desc" ? "desc" : "asc" });
          if (restored) return restored;
        }
      } catch {
        /* unreadable stored value — fall through to the default */
      }
    }
    return resolve(defaultSort);
  });

  const toggle = (key: string) => {
    const col = columns.find((cl) => cl.key === key);
    if (!col) return;
    const next: { key: string; dir: SortDir } =
      sort?.key === key ? { key, dir: sort.dir === "asc" ? "desc" : "asc" } : { key, dir: defaultDirFor(col) };
    setSort(next);
    if (storageKey) {
      try {
        localStorage.setItem(storageKey, JSON.stringify(next));
      } catch {
        /* private browsing — keep in-memory only */
      }
    }
    opts?.onChange?.();
  };

  const serverSide = opts?.serverSide ?? false;
  const sorted = useMemo(
    () => (serverSide ? rows : sortRows(rows, columns, sort, opts?.tiebreak)),
    // columns/tiebreak are recreated each render by design (theme-safe inline
    // specs); rows + the active sort are what actually change the output.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [rows, sort?.key, sort?.dir, serverSide],
  );

  return {
    sorted,
    sortKey: sort?.key ?? null,
    sortDir: sort?.dir ?? ("asc" as SortDir),
    toggle,
    isActive: (key: string) => sort?.key === key,
    // For the <th>: aria-sort is set only on the active column, per WAI-ARIA.
    ariaSort: (key: string): "ascending" | "descending" | undefined =>
      sort?.key === key ? (sort.dir === "asc" ? "ascending" : "descending") : undefined,
  };
}

// useToast — the transient corner-toast pattern (CC.22): a message string that
// auto-clears after `ms`. The timer is held in a ref and cleared before each
// re-arm and on unmount, so a toast fired within the window doesn't dismiss the
// prior one early (the CC.4 race) and no timer fires after unmount. Returns the
// current message (render it however the view likes) and a fire function.
export function useToast(ms = 3500): [string | null, (msg: string) => void] {
  const [toast, setToast] = useState<string | null>(null);
  const timer = useRef<number | undefined>(undefined);
  useEffect(() => () => window.clearTimeout(timer.current), []);
  const fire = (msg: string) => {
    setToast(msg);
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setToast(null), ms);
  };
  return [toast, fire];
}

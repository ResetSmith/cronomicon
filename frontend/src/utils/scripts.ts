import { useEffect, useState } from "react";
import { api, errMsg } from "../api/client";
import { paged } from "./paging";
import { parentOf } from "./tree";
import type { components } from "../api/schema";

type Script = components["schemas"]["Script"];

// FB3 — above this many scripts we stop eager-loading the whole catalog (the
// client-side folder tree / picker list would be too large) and fall back to
// server-side search. Below it we load every page so search + folder grouping see
// all rows. Shared by the Scripts catalog and the Job Composer's Script picker so
// the two never disagree on where the cap kicks in (JC18/JC19).
const SCRIPTS_CAP = 1000;

// FB1 — a script's folder LOCATION is its source file path (the leading "scripts/"
// stripped) so the browser mirrors the Git tree; its IDENTITY for detail/links is
// the DB name. They coincide for raw scripts and diverge for Cronomicon wrappers
// (name = metadata.name, file lives at source_path). Compose-authored scripts have
// no file, so fall back to the name as the path.
export function scriptDisplayPath(s: Script): string {
  // Strip the "scripts/" root and any stray leading/trailing slashes; if nothing
  // is left (e.g. source_path is just "scripts/") fall back to the name so the
  // script still appears in the tree rather than being silently dropped.
  const stripped = (s.sourcePath ?? "").replace(/^scripts\//, "").replace(/^\/+|\/+$/g, "");
  return stripped || (s.name ?? "");
}

// scriptFolder is the containing folder of a script's display path ("" at the
// root). The Composer's picker groups by this and the catalog opens it for a
// deep-linked leaf — both via the shared scriptDisplayPath, so folder derivation
// is identical in both places (JC19).
export function scriptFolder(s: Script): string {
  return parentOf(scriptDisplayPath(s));
}

// scriptSearchText is the haystack for the picker's client-side filter: name +
// folder/path + description + tags (JC17 — "match on name + folder path +
// description + tags").
export function scriptSearchText(s: Script): string {
  return [s.name ?? "", scriptDisplayPath(s), s.description ?? "", ...(s.tags ?? [])].join(" ");
}

// resolveCatalogItems is the cap decision: which rows to render. When capped and a
// (trimmed) query is present, the server-filtered set is authoritative (or [] while
// in flight); otherwise the full client-side set is filtered by the caller. Pulled
// out of useAllScripts as a pure function so the branch is unit-testable without a
// DOM (the repo has no component-test harness).
export function resolveCatalogItems(
  capped: boolean,
  query: string,
  serverResults: Script[] | null,
  fullItems: Script[],
  serverTagActive = false,
): Script[] {
  return capped && (query.trim() !== "" || serverTagActive) ? serverResults ?? [] : fullItems;
}

// useAllScripts delivers the full catalog (not just page 1, the old bug). It loads
// every page up to SCRIPTS_CAP; above the cap it serves the first page and flips to
// server-side search (q=) as the term changes. `bump` forces a reload (e.g. after a
// Git Pull). Returns the rows to render plus the catalog total and whether the cap
// kicked in (drives the banner + whether the caller filters client-side). Shared by
// the Scripts catalog and the Composer's Script picker.
export function useAllScripts(search: string, bump: number, tags: string[] = [], tagMatch: "any" | "all" = "any") {
  const [full, setFull] = useState<{ items: Script[]; total: number; capped: boolean; loading: boolean; error: string | null }>({
    items: [],
    total: 0,
    capped: false,
    loading: true,
    error: null,
  });
  // Catalog load (unfiltered), once per bump. Determines `capped` and, when under
  // the cap, the complete item set.
  useEffect(() => {
    let cancelled = false;
    setFull((s) => ({ ...s, loading: true }));
    (async () => {
      const PS = 200; // = backend maxPageSize
      const first = await api.GET("/scripts", { params: { query: { page: 1, pageSize: PS } } });
      if (cancelled) return;
      if (first.error) {
        setFull({ items: [], total: 0, capped: false, loading: false, error: errMsg(first.error) });
        return;
      }
      const p0 = paged<Script>(first.data);
      if (p0.totalItems > SCRIPTS_CAP) {
        setFull({ items: p0.items, total: p0.totalItems, capped: true, loading: false, error: null });
        return;
      }
      const all = [...p0.items];
      for (let pg = 2; pg <= p0.totalPages && !cancelled; pg++) {
        const r = await api.GET("/scripts", { params: { query: { page: pg, pageSize: PS } } });
        if (r.error) break;
        all.push(...paged<Script>(r.data).items);
      }
      if (cancelled) return;
      setFull({ items: all, total: p0.totalItems, capped: false, loading: false, error: null });
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [bump]);

  // Server-side search, only while capped (otherwise the caller filters the full
  // set client-side). `capped` is derived from the unfiltered count above, so a
  // filtered query never flips the mode.
  const [serverResults, setServerResults] = useState<Script[] | null>(null);
  const [searchLoading, setSearchLoading] = useState(false);
  const q = search.trim();
  // FU-2 Phase 2: above the cap the tag dropdown only sees the loaded page, so tag
  // filtering must go server-side too (?tag=&tagMatch=), same as ?q= search. A
  // stable string key keeps the effect from re-firing on identical selections.
  const tagKey = tags.join("\n");
  useEffect(() => {
    const hasFilter = q !== "" || tags.length > 0;
    if (!full.capped || !hasFilter) {
      setServerResults(null);
      setSearchLoading(false);
      return;
    }
    let cancelled = false;
    setSearchLoading(true);
    (async () => {
      const query: { page: number; pageSize: number; q?: string; tag?: string[]; tagMatch?: "any" | "all" } = { page: 1, pageSize: 200 };
      if (q) query.q = q;
      if (tags.length) {
        query.tag = tags;
        query.tagMatch = tagMatch;
      }
      const r = await api.GET("/scripts", { params: { query } });
      if (cancelled) return;
      setServerResults(r.error ? [] : paged<Script>(r.data).items);
      setSearchLoading(false);
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [full.capped, q, tagKey, tagMatch]);

  const items = resolveCatalogItems(full.capped, q, serverResults, full.items, tags.length > 0);
  // searchLoading folds into loading so a capped server search shows "Loading…"
  // rather than a premature "no scripts match" while results are in flight.
  return { items, total: full.total, capped: full.capped, loading: full.loading || searchLoading, error: full.error };
}

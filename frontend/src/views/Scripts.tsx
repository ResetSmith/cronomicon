import { Fragment, useEffect, useMemo, useState, type CSSProperties } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { api, csrfHeader, fetchCapabilities } from "../api/client";
import { useGet, rows, useClientPager, useColumnWidths, useInlineTags, useTableSort } from "../hooks";
import { ColumnsMenu, TableHead, renderCells, useTableColumns, type TableColumn } from "../components/table";
import { c } from "../theme";
import { useNameDisambiguator } from "../utils/disambiguate";
import type { components } from "../api/schema";
import { RefreshScope } from "../components/RefreshScope";
import { AlertBanner, Btn, CopyButton, RefreshButton, ExpandChevron, Field, HoverTr, InlineLoading, InlineTags, Pager, SearchBar, SectionLabel, SkeletonRows, TableSurface, TagEditor, TagFilterSelect, TypeBadge, WarningChip, WarningSummaryChip, matchesTags, tdStyle } from "../components/ui";
import { ScriptVariablesPanel } from "../components/ScriptVariables";
import { ReferenceBindingsEditor } from "../components/ReferenceBindings";
import { FolderBrowser } from "../components/FolderBrowser";
import { parentOf } from "../utils/tree";
import { useAllScripts, scriptDisplayPath } from "../utils/scripts";
import { fmtInAppZone } from "../utils/datetime";
import { type SortColumn } from "../utils/sort";

type Script = components["schemas"]["Script"];
// CO-4 — the table row: the script plus the per-render-mode display name.
type ScriptRow = { s: Script; displayName: string };
type ScriptContent = components["schemas"]["ScriptContent"];
type EnvVar = components["schemas"]["EnvVar"];
type EnvSecret = components["schemas"]["EnvSecret"];

// Default column widths (px) for the resizable Scripts table — mirrors the Jobs
// page (JOB_COL_W). Keep this sum (1024) and the <table> minWidth below in
// lockstep if a column is added or removed. `type`, `source`, and the trailing
// `expand` chevron render as fixed (non-resizable) columns.
const SCRIPT_COL_W: Record<string, number> = {
  name: 260,
  type: 90,
  source: 100,
  tags: 140,
  usedBy: 90,
  createdAt: 150,
  lastModifiedAt: 150,
  expand: 44,
};

// Cap how many tags render inline in the table so every row is a uniform height
// regardless of tag count (mirrors Jobs). Overflow collapses into a "+N" chip;
// the full, editable set lives in the row's expanded detail.
const MAX_INLINE_TAGS = 2;

const scriptName = (s: Script) => s.name ?? "";

// Read-only catalog over scripts/*.yaml synced from GitLab (B-Git). A script is
// the reusable executable unit; the "used by" reverse index makes a shared
// script's blast radius visible. Authoring is via the Git publish flow — no edit
// surface here.
// Sortable columns (TS-8, the sorting-update plan) — flat/search mode only;
// browse mode keeps the folder tree's alpha order. Tags excluded (multi-value).
const SCRIPT_SORT_COLS: SortColumn<Script>[] = [
  { key: "name", get: (s) => s.name, type: "text" },
  { key: "type", get: (s) => s.runType, type: "text" },
  { key: "source", get: (s) => sourceKind(s), type: "text" }, // the displayed value
  { key: "usedBy", get: (s) => s.usedByCount ?? 0, type: "number" },
  { key: "createdAt", get: (s) => s.createdAt, type: "date" },
  { key: "lastModifiedAt", get: (s) => s.lastModifiedAt, type: "date" },
];

export function Scripts() {
  const [search, setSearch] = useState("");
  const [expanded, setExpanded] = useState<string | null>(null);
  const [params, setParams] = useSearchParams();
  const focus = params.get("focus");
  // Current folder lives in the URL (?path=) so back/forward and deep links work.
  const path = params.get("path") ?? "";
  const setPath = (p: string) =>
    // Functional updater reads the live params at apply time (no stale snapshot).
    setParams((prev) => {
      const next = new URLSearchParams(prev);
      if (p) next.set("path", p);
      else next.delete("path");
      return next;
    });

  const [bump, setBump] = useState(0);
  const [pulling, setPulling] = useState(false);
  const [notice, setNotice] = useState<{ kind: "info" | "error"; text: string } | null>(null);
  const [type, setType] = useState("All");
  const [tagFilter, setTagFilter] = useState<string[]>([]);
  const [tagMatch, setTagMatch] = useState<"any" | "all">("any");
  const cw = useColumnWidths("scripts");

  // Compose capability gates the detail pane's "Create Job" hand-off into
  // JobComposer — same gate as the Jobs page's "+ Create" (D6/I-1: never offer
  // a create affordance that dead-ends on the composer's capability notice).
  const [canCompose, setCanCompose] = useState(false);
  useEffect(() => {
    fetchCapabilities().then((caps) => setCanCompose(caps.compose));
  }, []);

  // Optimistic per-script tag edits (keyed by name) so a detail-pane edit shows
  // in the table column immediately without a full catalog refetch. Cleared on a
  // Git Pull (bump), after which the fresh fetch carries the authoritative tags.
  const inlineTags = useInlineTags<Script>(
    bump,
    (s) => s.name ?? "",
    (s) => s.tags,
    (s, next) =>
      api.PUT("/script-tags/{name}", { params: { path: { name: s.name ?? "" }, header: csrfHeader }, body: { tags: next } }),
  );

  const { items, total, capped, loading, error } = useAllScripts(search, bump, tagFilter, tagMatch);

  // A deep-linked script (?focus=) can be off-page on a capped catalog, so fetch
  // it directly and merge it in — the tree + focus navigation then work regardless
  // of the cap or which page it sits on.
  const [pinned, setPinned] = useState<Script | null>(null);
  useEffect(() => {
    if (!focus) {
      setPinned(null);
      return;
    }
    let cancelled = false;
    api.GET("/scripts/{name}", { params: { path: { name: focus } } }).then((r) => {
      if (!cancelled && !r.error && r.data) setPinned(r.data as Script);
    });
    return () => {
      cancelled = true;
    };
  }, [focus]);
  const allItems = useMemo(
    () => (pinned && !items.some((s) => s.name === pinned.name) ? [...items, pinned] : items),
    [items, pinned],
  );

  // Trigger an on-demand GitLab re-sync of the whole definitions repo. The 202
  // is fire-and-forget (no synchronous count), so we just bump the refetch.
  async function gitPull() {
    setPulling(true);
    setNotice(null);
    const { error: err } = await api.POST("/git/sync", { params: { header: csrfHeader } });
    setPulling(false);
    if (err) {
      setNotice({ kind: "error", text: (err as { message?: string })?.message ?? "git pull failed" });
    } else {
      setNotice({ kind: "info", text: "Pull started — list will refresh." });
      setBump((b) => b + 1);
    }
  }

  // Deep link from a job's detail ("View script →"): auto-expand the focused
  // script once the list has loaded, then drop the query param.
  useEffect(() => {
    if (!focus) return;
    const target = allItems.find((s) => s.name === focus);
    if (!target) return;
    // Open the folder that holds the deep-linked script, expand it, drop ?focus.
    setExpanded(focus);
    const folder = parentOf(scriptDisplayPath(target));
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (folder) next.set("path", folder);
        else next.delete("path");
        next.delete("focus");
        return next;
      },
      { replace: true },
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [focus, allItems]);

  // When capped, `items` is already the server-filtered set (or page 1 when the
  // box is empty) — render as-is. Below the cap we hold the whole catalog and
  // filter it client-side over name + description.
  const q = search.trim().toLowerCase();
  const filtered = (
    capped
      ? allItems
      : allItems.filter(
          (s) => !q || (s.name ?? "").toLowerCase().includes(q) || (s.description ?? "").toLowerCase().includes(q),
        )
  )
    .filter((s) => type === "All" || (s.runType ?? "") === type)
    // Below the cap we filter tags client-side over the whole catalog; when capped,
    // useAllScripts already applied the tag filter server-side (FU-2 Phase 2), so
    // the loaded set is authoritative and re-filtering here would double-drop.
    .filter((s) => (capped ? true : matchesTags(s.tags, tagFilter, tagMatch)));

  // Column sort (TS-8) — flat/search mode only; browse mode keeps the tree's
  // alpha order. Script name default asc; Tags excluded (multi-value).
  const sort = useTableSort(filtered, SCRIPT_SORT_COLS, { key: "name", dir: "asc" }, { tableId: "scripts" });

  // Client-side pagination over the filtered set (flat/search mode). `total` from
  // useAllScripts is the catalog count, so the page total is aliased to avoid the
  // name clash. The page re-clamps as filters narrow; handlers reset to page 0.
  const { pageItems, total: pagedTotal, page, pager } = useClientPager(sort.sorted);

  // VU-14 — the filtered-empty catalog offers the filters back. Search is server-
  // side above the cap, so clearing it also re-widens the fetch.
  const clearFilters = () => {
    setSearch("");
    setType("All");
    setTagFilter([]);
    pager.setPage(0);
  };

  // Distinct run types across the loaded catalog, for the toolbar Type filter
  // (mirrors the Jobs page). When capped this reflects only the loaded page.
  const types = useMemo(
    () => ["All", ...Array.from(new Set(allItems.map((s) => s.runType).filter((t): t is NonNullable<Script["runType"]> => !!t))).sort()],
    [allItems],
  );

  // CO-4 — the column spec replaces this view's own headCell/fixedCell/
  // tableHeader trio (a drifted copy of the Jobs pair). Built in render: the
  // cells read `c.*` and close over `expanded` and the inline-tag resolver.
  //
  // Row is {script, displayName}: displayName differs per render mode (full name
  // in search, filename in the browser) and is supplied per row by FolderBrowser.
  const scriptColumnSpec = (): TableColumn<ScriptRow>[] => [
    {
      key: "name",
      label: "Script",
      sortKey: "name",
      width: SCRIPT_COL_W.name,
      pin: "first",
      cell: ({ s, displayName }) => (
        <>
          <span style={{ display: "inline-flex", alignItems: "center", gap: 8 }}>
            <span style={{ fontWeight: 600, fontFamily: c.mono }}>{displayName}</span>
            <WarningSummaryChip warnings={s.warnings ?? []} />
          </span>
          {s.description && <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 2 }}>{s.description}</div>}
        </>
      ),
    },
    { key: "type", label: "Type", sortKey: "type", width: SCRIPT_COL_W.type, fixed: true, cell: ({ s }) => <TypeBadge type={s.runType} /> },
    { key: "source", label: "Source", sortKey: "source", width: SCRIPT_COL_W.source, fixed: true, tdStyle: { color: c.textSec }, cell: ({ s }) => sourceKind(s) },
    {
      key: "tags",
      label: "Tags",
      width: SCRIPT_COL_W.tags,
      tdStyle: { whiteSpace: "nowrap" },
      cell: ({ s }) => <InlineTags tags={inlineTags.tagsFor(s)} max={MAX_INLINE_TAGS} />,
    },
    {
      key: "usedBy",
      label: "Used by",
      sortKey: "usedBy",
      width: SCRIPT_COL_W.usedBy,
      tdStyle: { color: c.textSec },
      cell: ({ s }) => `${s.usedByCount ?? 0} job${(s.usedByCount ?? 0) !== 1 ? "s" : ""}`,
    },
    {
      key: "createdAt",
      label: "Created On",
      sortKey: "createdAt",
      width: SCRIPT_COL_W.createdAt,
      tdStyle: { color: c.textSec, whiteSpace: "nowrap" },
      cell: ({ s }) => fmtWhen(s.createdAt),
    },
    {
      key: "lastModifiedAt",
      label: "Last Edited",
      sortKey: "lastModifiedAt",
      width: SCRIPT_COL_W.lastModifiedAt,
      tdStyle: { color: c.textSec, whiteSpace: "nowrap" },
      cell: ({ s }) => fmtWhen(s.lastModifiedAt),
    },
    {
      key: "expand",
      label: "",
      menuLabel: "Expand",
      width: SCRIPT_COL_W.expand,
      fixed: true,
      // CO-Q4 — unusually, the disclosure control is this table's TRAILING 44px
      // cell rather than its opener, so it pins "last".
      pin: "last",
      tdStyle: { textAlign: "right" },
      cell: ({ s }) => <ExpandChevron open={expanded === (s.name ?? "")} />,
    },
  ];

  const cols = useTableColumns<ScriptRow>("scripts", scriptColumnSpec());

  // One catalog row, reused by the flat search results (displayName = full name,
  // so the path is visible) and the folder browser (displayName = filename, since
  // the breadcrumb already shows the path). Identity/expand key stays the DB name.
  const renderScriptRow = (s: Script, displayName: string) => {
    const name = s.name ?? "";
    const isExp = expanded === name;
    const tags = inlineTags.tagsFor(s);
    const base = tdStyle();
    // Drop the row's bottom border when expanded so it joins its detail panel
    // (mirrors the Jobs row treatment). verticalAlign:top keeps the type/source
    // cells aligned to the first line when a description wraps the name cell.
    const cell = (extra?: CSSProperties): CSSProperties => ({
      ...base,
      verticalAlign: "top",
      borderBottom: isExp ? "none" : base.borderBottom,
      ...extra,
    });
    return (
      <Fragment key={name}>
        <HoverTr
          onClick={() => setExpanded(isExp ? null : name)}
          tint={isExp ? c.primaryBg : undefined}
          hoverTint={isExp ? c.primaryBg : c.panelHover}
        >
          {renderCells(cols.visible, { s, displayName }, { base: cell() })}
        </HoverTr>
        {isExp && (
          <tr>
            <td colSpan={cols.visible.length} style={{ padding: "14px 18px", background: c.primaryBg, borderBottom: `1px solid ${c.border}` }}>
              <RefreshScope>
              <ScriptDetail
                listRow={s}
                tags={tags}
                onSaveTags={(next) => inlineTags.save(s, next)}
                tagErr={inlineTags.errors[name]}
                canCompose={canCompose}
              />
              </RefreshScope>
            </td>
          </tr>
        )}
      </Fragment>
    );
  };

  return (
    <div>
      <div style={{ display: "flex", gap: 8, marginBottom: 16, alignItems: "center", flexWrap: "wrap" }}>
        <SearchBar value={search} onChange={(v) => { setSearch(v); pager.setPage(0); }} placeholder="Search scripts…" style={{ flex: 1, minWidth: 200, maxWidth: 340 }} />
        <label style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: c.fontSm, color: c.textMuted }}>
          Type
          <select
            value={type}
            onChange={(e) => { setType(e.target.value); pager.setPage(0); }}
            style={{ padding: "8px 10px", borderRadius: c.radiusChip, border: `1px solid ${c.borderStrong}`, background: c.panelInput, color: c.text, fontSize: c.fontSm }}
          >
            {types.map((t) => (
              <option key={t} value={t}>{t}</option>
            ))}
          </select>
        </label>
        <TagFilterSelect items={allItems} selected={tagFilter} onChange={(v) => { setTagFilter(v); pager.setPage(0); }} getTags={(s) => s.tags} matchMode={tagMatch} onMatchModeChange={(m) => { setTagMatch(m); pager.setPage(0); }} />
        <ColumnsMenu cols={cols} cw={cw} />
        <Btn
          primary
          onClick={gitPull}
          disabled={pulling}
          title="Pulls the whole definitions repo from GitLab (jobs, scripts, schedules, workflows)."
          style={{ padding: "8px 14px", fontSize: c.fontSm }}
        >
          {pulling ? "Pulling…" : "↻ Git Pull"}
        </Btn>
      </div>
      {notice && (
        <div style={{ marginBottom: 12, fontSize: c.fontSm, color: notice.kind === "error" ? c.danger : c.textSec }}>
          {notice.text}
        </div>
      )}

      {capped && (
        <div style={{ marginBottom: 12, fontSize: c.fontSm, color: c.textSec, background: c.panel2, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, padding: "8px 10px" }}>
          Large catalog ({total} scripts). {search.trim() ? "Showing server matches for your search." : "Only the first 200 are loaded — use search to find a specific script."}
        </div>
      )}
      {error && <div style={{ color: c.danger, marginBottom: 12 }}>Error: {error}</div>}

      {/* B-1/VU-5: the catalog no longer sits in a Card — it spans between a top
          and a bottom rule, matching Jobs. */}
      <TableSurface>
        {loading ? (
          <div style={{ padding: 16 }}>
            <SkeletonRows rows={5} />
          </div>
        ) : total === 0 ? (
          // VU-14 — scripts are Git-only: there is no in-app authoring to offer, so
          // the empty state names the source and hands over the one control that can
          // actually change the answer, the same gitPull the toolbar runs.
          <div style={{ padding: 36, textAlign: "center", color: c.textMuted, fontSize: c.fontSm }}>
            <div>No scripts yet. They are synced from the scripts/ directory of the definitions repo — there is no in-app script authoring.</div>
            <div style={{ marginTop: 12 }}>
              <Btn small onClick={gitPull} disabled={pulling}>
                {pulling ? "Pulling…" : "Pull from GitLab"}
              </Btn>
            </div>
          </div>
        ) : filtered.length === 0 ? (
          <div style={{ padding: 36, textAlign: "center", color: c.textMuted, fontSize: c.fontSm }}>
            <div>No scripts match your filters.</div>
            <div style={{ marginTop: 12 }}>
              <Btn small onClick={clearFilters}>Clear filters</Btn>
            </div>
          </div>
        ) : (
          // LB14 pattern (mirrors Jobs): wrap in an overflowX scroll container and
          // use tableLayout:auto + minWidth (= SCRIPT_COL_W sum) so the table
          // compresses to the surface when wide and scrolls when narrow. The inner
          // container stays even though TableSurface also scrolls: only the table
          // may scroll sideways, the Pager below it must not.
          <>
          <div style={{ overflowX: "auto" }}>
            {q ? (
              // Search mode: flat results across all folders, full path shown, paged
              // client-side.
              <table style={{ width: "100%", borderCollapse: "collapse", tableLayout: "auto", minWidth: cols.minWidth, fontSize: c.fontSm }}>
                <thead><TableHead columns={cols.visible} sort={sort} cw={cw} /></thead>
                <tbody>{pageItems.map((s) => renderScriptRow(s, scriptDisplayPath(s)))}</tbody>
              </table>
            ) : (
              // Browse mode: navigate the folder tree one level at a time. The
              // current level is paged inside <FolderBrowser paginate> (25/page).
              <FolderBrowser
                // TS-24: the sorted array, so the active column orders each
                // folder's leaves; folders themselves stay alpha-first.
                items={sort.sorted}
                preserveLeafOrder
                getPath={scriptDisplayPath}
                getName={scriptName}
                path={path}
                onNavigate={setPath}
                rootLabel="Scripts"
                header={<TableHead columns={cols.visible} sort={sort} cw={cw} />}
                colCount={cols.visible.length}
                renderLeaf={(leaf) => renderScriptRow(leaf.item, leaf.label)}
                hideBreadcrumbAtRoot
                paginate
              />
            )}
          </div>
          {q && <Pager pager={pager} page={page} total={pagedTotal} noun="scripts" />}
          </>
        )}
      </TableSurface>
    </div>
  );
}

// ScriptDetail shows the body + metadata from the list row and fetches the
// detail endpoint for the usedBy reverse index. `tags` is the parent's
// override-aware tag set (so the editor and the table column stay in sync);
// onTagsSaved propagates an edit back up for the optimistic table update.
function ScriptDetail({ listRow, tags, onSaveTags, tagErr, canCompose }: { listRow: Script; tags: string[]; onSaveTags: (tags: string[]) => void; tagErr?: string; canCompose?: boolean }) {
  const name = listRow.name ?? "";
  const { data } = useGet<Script>(
    () => api.GET("/scripts/{name}", { params: { path: { name } } }),
    [name],
  );
  const s = data ?? listRow;
  // Tag edits go through the parent's shared useInlineTags hook (CC.18): optimistic
  // override + per-row stale-guard + revert-on-error all live there now.
  const usedBy = s.usedBy ?? [];
  // R2F-3 — usedByRefs is the identity-bearing twin of usedBy (same jobs, same
  // order). Preferred when present; a server that predates it still renders the
  // plain names, which is what usedBy has always been.
  const usedByRefs = s.usedByRefs ?? [];
  const jobLabel = useNameDisambiguator(usedByRefs, (j) => ({
    uid: j.uid,
    name: j.name,
    source: j.source,
    agencies: j.agencies,
  }));
  const usedByChips = usedByRefs.length > 0 ? usedByRefs : usedBy.map((n) => ({ name: n }));
  const warnings = s.warnings ?? [];
  const dangers = warnings.filter((w) => w.severity === "danger");

  // Cross-reference the script's referenced variables against the keys already
  // defined as Env Vars / Secrets (any scope) so a ✓ marks ones that exist in the
  // system and the rest read as "still to define". Fetched lazily on row expand.
  const envQ = useGet<unknown>(() => api.GET("/env-vars"), []);
  const secQ = useGet<unknown>(() => api.GET("/env-secrets"), []);
  const definedKeys = useMemo(
    () =>
      new Set(
        [
          ...rows<EnvVar>(envQ.data).map((v) => v.key ?? ""),
          ...rows<EnvSecret>(secQ.data).map((x) => x.key ?? ""),
        ].filter(Boolean),
      ),
    [envQ.data, secQ.data],
  );

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 10, flexWrap: "wrap" }}>
        <div style={{ fontSize: c.fontXs, color: c.textMuted, fontFamily: c.mono }}>Scripts / {scriptDisplayPath(s)}</div>
        <RefreshButton />
        {/* Hand-off into the composer with this script preselected (?script= is
            consumed by JobComposer / ScriptPicker, which resolves by name). */}
        {canCompose && (
          <Link to={`/compose?script=${encodeURIComponent(name)}`} style={{ textDecoration: "none" }}>
            <Btn small primary>+ Create Job</Btn>
          </Link>
        )}
      </div>
      <div style={{ display: "flex", gap: 28, flexWrap: "wrap", fontSize: c.fontSm }}>
        <Field label="Run type" value={s.runType ?? "—"} mono />
        <Field label="Executor" value={s.executor ?? "auto (from run type)"} />
        <Field label="Source file" value={s.sourcePath ?? "—"} mono />
        <Field label="Content hash" value={shortHash(s.contentHash)} mono title={s.contentHash ?? undefined} />
        <Field label="Synced" value={fmtWhen(s.syncedAt)} />
      </div>

      <div>
        <SectionLabel>Tags{tags.length ? ` (${tags.length})` : ""}</SectionLabel>
        <TagEditor tags={tags} onChange={onSaveTags} />
        {tagErr && <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 6 }}>{tagErr}</div>}
        <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 6 }}>
          Tags are stored in Cronomicon only — they are not written back to Git, and they are kept across syncs.
        </div>
      </div>

      {warnings.length > 0 && (
        <div>
          <SectionLabel>Lint</SectionLabel>
          {dangers.length > 0 && (
            <AlertBanner type="danger">{dangers.map((w) => w.message).join(" ")}</AlertBanner>
          )}
          <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
            {warnings.map((w, i) => (
              <WarningChip key={`${w.rule}-${i}`} severity={w.severity} rule={w.rule ?? "issue"} message={w.message ?? undefined} />
            ))}
          </div>
        </div>
      )}

      <ScriptBody script={s} />

      <div>
        <ScriptVariablesPanel variables={s.variables} runType={s.runType} satisfied={definedKeys} />
        <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 6 }}>
          ✓ already defined as an Env Var or Secret · ● required · ○ optional (the script supplies a default).
          These are wired into a run via the job&apos;s env, a schedule, or a per-run override.
        </div>
      </div>

      <div>
        <SectionLabel>References</SectionLabel>
        <ReferenceBindingsEditor owner={{ script: name }} scope="" />
        <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 6 }}>
          The Env Vars references (Secrets, Variables, SSH Keys) this script consumes; only declared references are
          injected at dispatch. Use “Suggest from body” to prefill from the script text — editing needs the Manage
          Env Vars permission.
        </div>
        <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 4 }}>
          A script has no scope of its own, so each reference is checked against the <strong>global</strong> scope —
          the answer that holds for every job. A reference marked as living only in a named scope still resolves for
          jobs in that scope.
        </div>
      </div>

      <div>
        <SectionLabel>Used by {usedBy.length} job{usedBy.length !== 1 ? "s" : ""}</SectionLabel>
        {usedBy.length === 0 ? (
          <div style={{ fontSize: c.fontSm, color: c.textMuted }}>
            No job references this script yet — it is an orphan.
          </div>
        ) : (
          <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
            {usedByChips.map((j, i) => (
              <span
                key={`${j.name}:${i}`}
                style={{
                  fontFamily: c.mono,
                  fontSize: c.fontXs,
                  padding: "3px 8px",
                  borderRadius: c.radiusChip,
                  background: c.panel2,
                  border: `1px solid ${c.border}`,
                  color: c.textSec,
                }}
              >
                {jobLabel(j)}
              </span>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

function ScriptBody({ script }: { script: Script }) {
  const kind = script.sourceKind;
  if (!kind) return null;
  // The body — inline command/script or a scriptPath file — is fetched on demand
  // from the content endpoint so the Scripts list no longer ships the blob columns
  // (CC.11). sourceKind (server-derived) is on both the list row and the detail
  // fetch, so this renders on expand without the bodies inline.
  const label = kind === "command" ? "Command" : kind === "file" ? "Script file" : "Script";
  return (
    <ScriptFileBody
      name={script.name ?? ""}
      label={label}
      path={kind === "file" ? (script.scriptPath ?? undefined) : undefined}
    />
  );
}

// ScriptFileBody lazily fetches a script's resolved body from the content
// endpoint when its row is expanded — for inline command/script scripts as well
// as scriptPath files (the endpoint resolves all three, CC.11). It is a separate
// component so the fetch hook runs only when a body is shown, keeping hook calls
// unconditional within each component. `path` is shown only for file-backed
// scripts.
function ScriptFileBody({ name, label, path }: { name: string; label: string; path?: string }) {
  const { data, error, loading } = useGet<ScriptContent>(
    () => api.GET("/script-content/{name}", { params: { path: { name } } }),
    [name],
  );
  return (
    <div>
      <SectionLabel>{label}</SectionLabel>
      {path && <div style={{ fontFamily: c.mono, fontSize: c.fontXs, color: c.textMuted, marginBottom: 6 }}>{path}</div>}
      {loading ? (
        <InlineLoading />
      ) : error ? (
        <div style={{ fontSize: c.fontSm, color: c.danger }}>
          {path ? "Could not read script file from the synced repository." : "Could not read the script body."}
        </div>
      ) : data?.encoding === "binary" ? (
        <div style={{ fontSize: c.fontSm, color: c.textMuted }}>Binary file ({data.byteLength} bytes) — view in Git.</div>
      ) : (data?.content ?? "") === "" ? (
        <div style={{ fontSize: c.fontSm, color: c.textMuted }}>{path ? "Empty file." : "Empty."}</div>
      ) : (
        <>
          <CodeBlockWithCopy text={data!.content!} />
          {data?.truncated && (
            <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 4 }}>
              Truncated to 1&nbsp;MiB — view the full file in Git.
            </div>
          )}
        </>
      )}
    </div>
  );
}

// CodeBlockWithCopy renders a body in the catalog code block with a Copy button.
function CodeBlockWithCopy({ text }: { text: string }) {
  // FX-13 — was a hand-rolled overlay button that fired writeText without
  // awaiting it, so it flashed "Copied" even when the write was rejected.
  return (
    <div style={{ position: "relative" }}>
      <CopyButton text={text} overlay variant="btn" ariaLabel="Copy script to clipboard" />
      <pre style={codeBlock()}>{text}</pre>
    </div>
  );
}

// The Scripts list badges each row by its server-derived sourceKind (CC.11) —
// the command/script blobs no longer travel in the list response.
function sourceKind(s: Script): string {
  return s.sourceKind || "—";
}

function shortHash(h?: string | null): string {
  if (!h) return "—";
  const hex = h.replace(/^sha256:/, "");
  return hex.length > 12 ? hex.slice(0, 12) : hex;
}

function fmtWhen(v?: string | null): string {
  return fmtInAppZone(v);
}

// Computed at call time (not a module-level const) so a theme toggle re-reads
// the active palette — module-level token capture freezes the load-time theme.
// See theme.ts: "never capture token values in module-level constants."
const codeBlock = (): React.CSSProperties => ({
  margin: 0,
  padding: "12px 14px",
  background: c.panel2,
  border: `1px solid ${c.border}`,
  borderRadius: c.radiusSurface,
  fontFamily: c.mono,
  fontSize: c.fontXs,
  lineHeight: 1.7,
  color: c.textSec,
  overflowX: "auto",
  overflowY: "auto",
  whiteSpace: "pre-wrap",
  maxHeight: 320,
});

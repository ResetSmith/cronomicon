import { Fragment, useEffect, useState, type ComponentType } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { api, csrfHeader, fetchCapabilities } from "../api/client";
import { useGet, rows, useClientPager, useInlineTags, useTableSort, useColumnWidths } from "../hooks";
import { ColumnsMenu, TableHead, renderCells, useTableColumns, type TableColumn } from "../components/table";
import { c } from "../theme";
import { useNameDisambiguator } from "../utils/disambiguate";
import type { components } from "../api/schema";
import { Btn, DetailPanel, ExpandChevron, Field, HoverTr, InlineTags, Pager, SearchBar, SectionLabel, SkeletonRows, SourceBadge, TagEditor, TagFilterSelect, matchesTags } from "../components/ui";
import { type SortColumn } from "../utils/sort";
import { FolderBrowser } from "../components/FolderBrowser";
import { InventoryTab } from "./scheduling/InventoryTab";
import { UpcomingTab } from "./scheduling/UpcomingTab";
import { ReactionsTab } from "./scheduling/ReactionsTab";
import { CalendarsTab } from "./scheduling/CalendarsTab";
import { GlobalCalendarBanner } from "./scheduling/GlobalCalendarBanner";
import { fmtInAppZone } from "../utils/datetime";
import { RefreshScope } from "../components/RefreshScope";

type Schedule = components["schemas"]["Schedule"];
// CO-4 — the table row: the schedule plus its per-render-mode display name.
type ScheduleRow = { sched: Schedule; displayName: string };

// CO-4 — introduced at adoption. This catalog predates useColumnWidths (V1.1-7
// era) and rendered at intrinsic widths; the shared header needs a default per
// column, and having one is also what lets these columns be resized at all.
const SCHEDULE_COL_W: Record<string, number> = {
  name: 260,
  cron: 150,
  source: 100,
  usedBy: 100,
  tags: 140,
  createdAt: 150,
  lastModifiedAt: 150,
  expand: 44,
};

// FB1 — a schedule's folder LOCATION is its source file path (the leading
// "schedules/" stripped) so the browser mirrors the Git tree; its IDENTITY for
// detail/actions stays the DB name (keyed by source:name since a name can recur
// across the git and cronomicon sources). Cronomicon-authored schedules have no file,
// so fall back to the name as the path so they still appear in the tree.
const scheduleDisplayPath = (s: Schedule) => {
  // Strip the "schedules/" root and any stray leading/trailing slashes; if nothing
  // is left (e.g. source_path is just "schedules/") fall back to the name so the
  // schedule still appears in the tree rather than being silently dropped.
  const stripped = (s.sourcePath ?? "").replace(/^schedules\//, "").replace(/^\/+|\/+$/g, "");
  return stripped || (s.name ?? "");
};
// Identity key for the folder tree — unique across sources (mirrors the React key
// used by the catalog rows). Folder location is purely cosmetic grouping.
const scheduleName = (s: Schedule) => `${s.source}:${s.name ?? ""}`;

// Operator-owned tags (tags-support.md §6.5). This catalog had no write surface
// before, so it lacked a CSRF header constant; the tag PUT needs one. The cap keeps
// every row a uniform height (overflow → "+N"; the full editable set is in detail).
const MAX_INLINE_TAGS = 2;

// Schedules is the single complete scheduling surface (the old "Schedule" hub was
// dissolved): the first-class schedule-defs Catalog, the read-only Inventory and
// Upcoming projections that span both jobs and workflows, and the Calendars
// authoring tab (CAL-18), and the Reactions edge list (RX-14). Neither is a new
// sidebar item — the
// sidebar count is a standing constraint, and both are scheduling data rather
// than their own domain. Reactions earns its place here for a sharper reason:
// a reaction has no clock, so Upcoming cannot project it and this list is the
// only place the cross-entity edges are visible ahead of time (§2.10). Tabs
// deep-link via ?tab=catalog|inventory|upcoming|calendars|reactions and mount
// lazily then stay alive
// (display:none) so each retains its own state.
const TABS = ["Catalog", "Inventory", "Upcoming", "Calendars", "Reactions"];
const SLUGS = ["catalog", "inventory", "upcoming", "calendars", "reactions"];
// VU-14 — the read-only projections hand their empty states a way back to the tab
// that can change the answer. The active tab is this component's state, so it has to
// be a callback: a URL link changing ?tab= would not re-select it.
const PANELS: ComponentType<{ onGoToTab?: (i: number) => void }>[] = [CatalogTab, InventoryTab, UpcomingTab, CalendarsTab, ReactionsTab];
// Single source of truth for the default landing tab (LB13): operators want to see
// what's scheduled to run next first, so a bare /schedules opens on Upcoming.
const DEFAULT = SLUGS.indexOf("upcoming");

export function Schedules() {
  const [params, setParams] = useSearchParams();
  const initial = Math.max(0, SLUGS.indexOf(params.get("tab") ?? SLUGS[DEFAULT]));
  const [tab, setTab] = useState(initial);
  const [seen, setSeen] = useState<number[]>([initial]);
  const select = (i: number) => {
    setTab(i);
    if (!seen.includes(i)) setSeen([...seen, i]);
    // Mirror the active tab into the URL so the view is shareable/bookmarkable
    // and the /schedule redirect can land on a specific tab. Upcoming is the
    // default, so drop the param for a clean URL.
    const next = new URLSearchParams(params);
    if (i === DEFAULT) next.delete("tab");
    else next.set("tab", SLUGS[i]);
    setParams(next, { replace: true });
  };

  return (
    <div>
      {/* CAL-28 — a global suppression nobody can see is this feature's worst
          failure mode, so it is stated above the tabs rather than only inside
          whichever tab happens to be open. */}
      <GlobalCalendarBanner onGoToCalendars={() => select(SLUGS.indexOf("calendars"))} />
      <div style={{ display: "flex", gap: 4, borderBottom: `1px solid ${c.border}`, marginBottom: 16 }}>
        {TABS.map((t, i) => (
          <button
            key={t}
            onClick={() => select(i)}
            style={{
              padding: "8px 14px",
              fontSize: c.fontSm,
              fontFamily: c.sans,
              cursor: "pointer",
              background: "transparent",
              border: "none",
              color: tab === i ? c.primary : c.textSec,
              fontWeight: tab === i ? 600 : 400,
              borderBottom: `2px solid ${tab === i ? c.primary : "transparent"}`,
              marginBottom: -1,
            }}
          >
            {t}
          </button>
        ))}
      </div>
      {PANELS.map((Panel, i) =>
        seen.includes(i) ? (
          <div key={TABS[i]} style={{ display: tab === i ? "block" : "none" }}>
            <Panel onGoToTab={select} />
          </div>
        ) : null,
      )}
    </div>
  );
}

// Read-only catalog over first-class Schedules (A10a) — schedules/*.yaml synced
// from GitLab plus operator-authored (cronomicon-source) rows. A schedule is the
// reusable cron primitive; the "used by" reverse index makes a shared schedule's
// blast radius visible. Git authoring is via the publish flow; cronomicon authoring
// is the Schedule Builder (+ New schedule). Rendered as the Catalog tab of the
// Schedules surface.
// Sortable columns (TS-8, the sorting-update plan); see the tableId note at
// the hook call for what is excluded and why.
const SCHEDULE_SORT_COLS: SortColumn<Schedule>[] = [
  { key: "name", get: (s) => s.name, type: "text" },
  { key: "source", get: (s) => s.source, type: "text" },
  { key: "usedBy", get: (s) => s.usedByCount ?? 0, type: "number" },
  { key: "createdAt", get: (s) => s.createdAt, type: "date" },
  { key: "lastModifiedAt", get: (s) => s.lastModifiedAt, type: "date" },
];

function CatalogTab() {
  const [search, setSearch] = useState("");
  const [expanded, setExpanded] = useState<string | null>(null);
  const [refresh, setRefresh] = useState(0);
  const [canCompose, setCanCompose] = useState(false);
  const [tagFilter, setTagFilter] = useState<string[]>([]);
  const [tagMatch, setTagMatch] = useState<"any" | "all">("any");
  // Optimistic per-schedule tag edits keyed by source:name (a name can recur across
  // the git and cronomicon sources). Reset on refresh — the refetch is authoritative.
  const inlineTags = useInlineTags<Schedule>(
    refresh,
    (s) => `${s.source}:${s.name ?? ""}`,
    (s) => s.tags,
    (s, next) =>
      api.PUT("/schedule-tags/{name}", {
        params: { path: { name: s.name ?? "" }, query: { source: s.source }, header: csrfHeader },
        body: { tags: next },
      }),
  );
  const [params, setParams] = useSearchParams();
  useEffect(() => {
    fetchCapabilities().then((caps) => setCanCompose(caps.compose));
  }, []);

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

  const { data, error, loading } = useGet<unknown>(() => api.GET("/schedule-defs"), [refresh]);
  const items = rows<Schedule>(data);

  const q = search.trim().toLowerCase();
  const filtered = items.filter(
    (s) =>
      matchesTags(inlineTags.tagsFor(s), tagFilter, tagMatch) &&
      (!q ||
        (s.name ?? "").toLowerCase().includes(q) ||
        (s.description ?? "").toLowerCase().includes(q)),
  );

  // Column sort (TS-8) — flat/search mode only; browse mode keeps the tree's
  // alpha order. Cron excluded (string-sorting a cron expression is meaningless
  // — Created/Next-run style columns are the sortable version of that info);
  // Tags excluded (multi-value).
  const cw = useColumnWidths("schedules");
  const sort = useTableSort(filtered, SCHEDULE_SORT_COLS, { key: "name", dir: "asc" }, { tableId: "schedule-catalog" });

  // Search mode pages the flat result client-side (parity with Jobs/Scripts); the
  // folder-browse mode is paged inside <FolderBrowser paginate>. A filter or search
  // edit resets to the first page so the user isn't stranded past the new last page.
  const { pageItems, total, page, pager } = useClientPager(sort.sorted);

  // VU-14 — hands the search + tag filter back from the filtered-empty state.
  const clearFilters = () => {
    setSearch("");
    setTagFilter([]);
    pager.setPage(0);
  };

  // CO-4 — the column spec. This catalog had NO width map (a fixed-width table
  // from the V1.1-7 era, so it never adopted useColumnWidths); SCHEDULE_COL_W
  // above is introduced here, which is what lets the shared header offer resize
  // on the content columns alongside order and visibility.
  //
  // Row is {sched, displayName}: displayName differs per render mode (display
  // path in search, filename in the browser) and is supplied per row.
  const scheduleColumnSpec = (): TableColumn<ScheduleRow>[] => [
    {
      key: "name",
      label: "Schedule",
      sortKey: "name",
      width: SCHEDULE_COL_W.name,
      pin: "first",
      cell: ({ sched: s, displayName }) => (
        <>
          <span style={{ fontWeight: 600, fontFamily: c.mono }}>{displayName}</span>
          {s.description && <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 2 }}>{s.description}</div>}
        </>
      ),
    },
    { key: "cron", label: "Cron", width: SCHEDULE_COL_W.cron, tdStyle: { fontFamily: c.mono, color: c.textSec, whiteSpace: "nowrap" }, cell: ({ sched: s }) => s.cron },
    { key: "source", label: "Source", sortKey: "source", width: SCHEDULE_COL_W.source, fixed: true, cell: ({ sched: s }) => <SourceBadge source={s.source} /> },
    {
      key: "usedBy",
      label: "Used by",
      sortKey: "usedBy",
      width: SCHEDULE_COL_W.usedBy,
      fixed: true,
      tdStyle: { color: c.textSec },
      cell: ({ sched: s }) => `${s.usedByCount ?? 0} ref${(s.usedByCount ?? 0) !== 1 ? "s" : ""}`,
    },
    {
      key: "tags",
      label: "Tags",
      width: SCHEDULE_COL_W.tags,
      tdStyle: { whiteSpace: "nowrap" },
      cell: ({ sched: s }) => <InlineTags tags={inlineTags.tagsFor(s)} max={MAX_INLINE_TAGS} />,
    },
    {
      key: "createdAt",
      label: "Created On",
      sortKey: "createdAt",
      width: SCHEDULE_COL_W.createdAt,
      fixed: true,
      tdStyle: { color: c.textSec, whiteSpace: "nowrap" },
      cell: ({ sched: s }) => fmtWhen(s.createdAt),
    },
    {
      key: "lastModifiedAt",
      label: "Last Edited",
      sortKey: "lastModifiedAt",
      width: SCHEDULE_COL_W.lastModifiedAt,
      fixed: true,
      tdStyle: { color: c.textSec, whiteSpace: "nowrap" },
      cell: ({ sched: s }) => fmtWhen(s.lastModifiedAt),
    },
    {
      key: "expand",
      label: "",
      menuLabel: "Expand",
      width: SCHEDULE_COL_W.expand,
      fixed: true,
      // CO-Q4 — like Scripts, the disclosure control is this table's TRAILING
      // cell rather than its opener, so it pins last.
      pin: "last",
      tdStyle: { textAlign: "right" },
      cell: ({ sched: s }) => <ExpandChevron open={expanded === `${s.source}:${s.name ?? ""}`} />,
    },
  ];

  const cols = useTableColumns<ScheduleRow>("schedules", scheduleColumnSpec());

  // TS-24: both render modes sort now, so the header no longer takes a
  // sortEnabled flag — it was only ever passed `false` by the browse branch.
  const tableHeader = () => (
    <TableHead
      columns={cols.visible}
      sort={sort}
      cw={cw}
      thStyle={th}
      trStyle={{ color: c.textSec, textAlign: "left", borderBottom: `1px solid ${c.border}` }}
    />
  );

  // One catalog row, reused by the flat search results (displayName = display path,
  // so the folder is visible) and the folder browser (displayName = filename, since
  // the breadcrumb already shows the path). Identity/expand key is source:name — a
  // name can recur across the git and cronomicon sources, so name alone would make two
  // same-named schedules expand together.
  const renderScheduleRow = (s: Schedule, displayName: string) => {
    const name = s.name ?? "";
    const key = `${s.source}:${name}`;
    const isExp = expanded === key;
    // Override-aware tag set (keyed source:name) so an optimistic edit shows at once.
    const tags = inlineTags.tagsFor(s);
    return (
      <Fragment key={key}>
        <HoverTr
          onClick={() => setExpanded(isExp ? null : key)}
          tint={isExp ? c.primaryBg : undefined}
          hoverTint={isExp ? c.primaryBg : c.panelHover}
          style={{ borderBottom: isExp ? "none" : `1px solid ${c.border}` }}
        >
          {renderCells(cols.visible, { sched: s, displayName }, { base: td })}
        </HoverTr>
        {isExp && (
          <tr style={{ borderBottom: `1px solid ${c.border}` }}>
            <td colSpan={cols.visible.length} style={{ padding: "16px 18px", background: c.primaryBg }}>
              <RefreshScope>
              <ScheduleDetail
                listRow={s}
                canCompose={canCompose}
                onChanged={() => setRefresh((n) => n + 1)}
                tags={tags}
                onSaveTags={(next) => inlineTags.save(s, next)}
                tagErr={inlineTags.errors[key]}
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
      <div style={{ display: "flex", alignItems: "center", gap: 12, marginBottom: 16, flexWrap: "wrap" }}>
        <SearchBar value={search} onChange={(v) => { setSearch(v); pager.setPage(0); }} placeholder="Search schedules…" style={{ flex: 1, minWidth: 200, maxWidth: 340 }} />
        <TagFilterSelect items={items} selected={tagFilter} onChange={(v) => { setTagFilter(v); pager.setPage(0); }} getTags={(s) => inlineTags.tagsFor(s)} matchMode={tagMatch} onMatchModeChange={(m) => { setTagMatch(m); pager.setPage(0); }} />
        <ColumnsMenu cols={cols} cw={cw} />
        {canCompose && (
          <Link to="/schedule-builder" style={{ textDecoration: "none" }}>
            <Btn primary style={{ padding: "8px 14px", fontSize: c.fontSm }}>+ New schedule</Btn>
          </Link>
        )}
      </div>

      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {error && <div style={{ color: c.danger }}>Error: {error}</div>}
      {/* VU-14 — the empty catalog offers the Schedule Builder (gated on Compose,
          exactly like the toolbar button, because the builder refuses a caller
          without it) and names Git as the other source; a filtered-empty list
          offers the search and tag filter back instead. */}
      {!loading && !error && items.length === 0 && (
        <div style={{ color: c.textSec }}>
          <div>No schedules yet. Schedules are synced from the schedules/ directory of the definitions repo, or authored in the Schedule Builder.</div>
          {canCompose && (
            <div style={{ marginTop: 12 }}>
              <Link to="/schedule-builder" style={{ textDecoration: "none" }}>
                <Btn small>New schedule</Btn>
              </Link>
            </div>
          )}
        </div>
      )}
      {!loading && !error && items.length > 0 && filtered.length === 0 && (
        <div style={{ color: c.textSec }}>
          <div>No schedules match your search.</div>
          <div style={{ marginTop: 12 }}>
            <Btn small onClick={clearFilters}>Clear filters</Btn>
          </div>
        </div>
      )}

      {filtered.length > 0 &&
        (q ? (
          // Search mode: flat results across all folders, full path shown, paged
          // client-side (parity with Jobs/Scripts).
          <>
            <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm }}>
              <thead>{tableHeader()}</thead>
              <tbody>{pageItems.map((s) => renderScheduleRow(s, scheduleDisplayPath(s)))}</tbody>
            </table>
            <Pager pager={pager} page={page} total={total} noun="schedules" />
          </>
        ) : (
          // Browse mode: navigate the folder tree one level at a time, paged within
          // the current level.
          <FolderBrowser
            // TS-24: the sorted array, so the active column orders each folder's
            // leaves; folders themselves stay alpha-first.
            items={sort.sorted}
            preserveLeafOrder
            getPath={scheduleDisplayPath}
            getName={scheduleName}
            path={path}
            onNavigate={setPath}
            rootLabel="Schedules"
            header={tableHeader()}
            colCount={cols.visible.length}
            renderLeaf={(leaf) => renderScheduleRow(leaf.item, leaf.label)}
            paginate
          />
        ))}
    </div>
  );
}

// `tags` is the parent's override-aware tag set (so the editor and the table column
// stay in sync); onTagsSaved propagates an edit back up for the optimistic table
// update. Tags are operator-owned and SQLite-only (tags-support.md) — editable for
// both git and cronomicon schedules (any logged-in user, D4), unlike Edit/Delete.
function ScheduleDetail({ listRow, canCompose, onChanged, tags, onSaveTags, tagErr }: { listRow: Schedule; canCompose: boolean; onChanged: () => void; tags: string[]; onSaveTags: (tags: string[]) => void; tagErr?: string }) {
  const name = listRow.name ?? "";
  const { data } = useGet<Schedule>(
    () => api.GET("/schedule-defs/{name}", { params: { path: { name }, query: { source: listRow.source } } }),
    [name, listRow.source],
  );
  const s = data ?? listRow;
  const usedBy = s.usedBy ?? [];
  // R2F-3 — this schedule's owners are the visible set: two same-named jobs
  // bound to one schedule are two chips, and without the badge they read as a
  // duplicate rather than as two departments.
  const ownerLabel = useNameDisambiguator(usedBy, (u) => ({ uid: u.uid, name: u.name, agencies: u.agencies, group: u.kind }));
  const env = s.env ?? {};
  const envKeys = Object.keys(env);
  const isCronomicon = (s.source ?? listRow.source) === "cronomicon";

  const [delErr, setDelErr] = useState<string | null>(null);
  const [pendingForce, setPendingForce] = useState(false);
  const [deleting, setDeleting] = useState(false);
  // Tag edits go through the parent's shared useInlineTags hook (CC.18).

  async function del(force: boolean) {
    setDelErr(null);
    setDeleting(true);
    const { response, error } = await api.DELETE("/schedule-defs/{name}", {
      params: { path: { name }, query: force ? { force: true } : {} },
    });
    setDeleting(false);
    if (response.status === 409 && !force) {
      // Block-by-default: surface the in-use message and offer a force-detach.
      setDelErr(errMessage(error) || "This schedule is referenced by other definitions.");
      setPendingForce(true);
      return;
    }
    if (error || !response.ok) {
      setDelErr(errMessage(error) || `Delete failed (${response.status}).`);
      setPendingForce(false);
      return;
    }
    onChanged();
  }

  return (
    // EP-4 Shape A — the schedule's own detail fetch.
    <DetailPanel style={{ display: "flex", flexDirection: "column", gap: 14 }}>
      <div style={{ display: "flex", gap: 28, flexWrap: "wrap", fontSize: c.fontSm }}>
        <Field label="Cron" value={s.cron ?? "—"} mono />
        <Field label="Source" value={s.source ?? "—"} />
        <Field label="Source file" value={s.sourcePath ?? "—"} mono />
        <Field label="Content hash" value={shortHash(s.contentHash)} mono title={s.contentHash ?? undefined} />
        <Field label="Synced" value={fmtWhen(s.syncedAt)} />
      </div>

      {envKeys.length > 0 && (
        <div>
          <SectionLabel>Env ({envKeys.length})</SectionLabel>
          <pre style={codeBlock()}>{envKeys.map((k) => `${k}=${env[k]}`).join("\n")}</pre>
        </div>
      )}

      <div>
        <SectionLabel>Used by {usedBy.length} definition{usedBy.length !== 1 ? "s" : ""}</SectionLabel>
        {usedBy.length === 0 ? (
          <div style={{ fontSize: c.fontSm, color: c.textMuted }}>
            No job or workflow references this schedule yet — it is an orphan.
          </div>
        ) : (
          <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
            {usedBy.map((u) => (
              <span
                key={`${u.kind}:${u.uid || u.name}`}
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
                {u.kind}: {ownerLabel(u)}
              </span>
            ))}
          </div>
        )}
      </div>

      <div>
        <SectionLabel>Tags{tags.length ? ` (${tags.length})` : ""}</SectionLabel>
        <TagEditor tags={tags} onChange={onSaveTags} />
        {tagErr && <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 6 }}>{tagErr}</div>}
        <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 6 }}>
          Tags are stored in Cronomicon only — they are not written back to Git, and they are kept across syncs.
        </div>
      </div>

      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap", borderTop: `1px solid ${c.border}`, paddingTop: 12 }}>
        {isCronomicon ? (
          canCompose ? (
            <>
              <Link to={`/schedule-builder?name=${encodeURIComponent(name)}`} style={{ textDecoration: "none" }}>
                <Btn small>Edit</Btn>
              </Link>
              {pendingForce ? (
                <>
                  <Btn small danger disabled={deleting} onClick={() => del(true)}>
                    Force delete ({usedBy.length} ref{usedBy.length !== 1 ? "s" : ""})
                  </Btn>
                  <Btn small disabled={deleting} onClick={() => { setPendingForce(false); setDelErr(null); }}>
                    Cancel
                  </Btn>
                </>
              ) : (
                <Btn small danger disabled={deleting} onClick={() => del(false)}>
                  Delete
                </Btn>
              )}
            </>
          ) : (
            <span style={{ fontSize: c.fontSm, color: c.textMuted }}>Cronomicon-authored — authoring requires the Compose capability.</span>
          )
        ) : (
          <span style={{ fontSize: c.fontSm, color: c.textMuted }}>
            Authored in Git{s.sourcePath ? <> (<span style={{ fontFamily: c.mono }}>{s.sourcePath}</span>)</> : null} — read-only here; edit via the GitLab publish flow.
          </span>
        )}
        {delErr && <span style={{ fontSize: c.fontSm, color: pendingForce ? c.warning : c.danger }}>{delErr}</span>}
      </div>
    </DetailPanel>
  );
}

function errMessage(error: unknown): string {
  if (error && typeof error === "object" && "message" in error) {
    return String((error as { message?: unknown }).message ?? "");
  }
  return "";
}

function shortHash(h?: string | null): string {
  if (!h) return "—";
  const hex = h.replace(/^sha256:/, "");
  return hex.length > 12 ? hex.slice(0, 12) : hex;
}

function fmtWhen(v?: string | null): string {
  return fmtInAppZone(v);
}

// A function, not a module-level const: the column head now reads c.* tokens, and
// capturing those at module load would freeze the load-time theme (see codeBlock).
const th = (): React.CSSProperties => ({ padding: "8px 16px", fontFamily: c.sansCond, fontWeight: 600, fontSize: c.fontXs, textTransform: "uppercase", letterSpacing: 0.7 });
const td: React.CSSProperties = { padding: "10px 16px", verticalAlign: "top" };
// A function, not a module-level const, so a theme toggle re-reads the active
// palette. Capturing c.* tokens at module load freezes the load-time theme
// (default: dark). See theme.ts: "never capture token values in module-level
// constants."
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
  whiteSpace: "pre-wrap",
  maxHeight: 320,
});

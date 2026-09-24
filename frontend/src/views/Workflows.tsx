import { Fragment, Suspense, lazy, useEffect, useState } from "react";
import { Link, useLocation, useSearchParams } from "react-router-dom";
import { api, csrfHeader, fetchCapabilities } from "../api/client";
import { useLiveGet, rows, useClientPager, useColumnWidths, useInlineAnnotation, useInlineTags, useTableSort, useToast } from "../hooks";
import { AnnotationSection, CriticalChip, annotationOf, type Annotation } from "../components/Annotation";
import { useGet } from "../hooks";
import { RefreshScope } from "../components/RefreshScope";
import { c } from "../theme";
import { useNameDisambiguator } from "../utils/disambiguate";
import { badgeStyle, fmtTime, type Workflow } from "./workflows/shared";
import { StepChain } from "./workflows/StepChain";
import { normalizeGraph } from "./workflows/graphView";
import { WorkflowRunsTable } from "./workflows/WorkflowRunsTable";
import { rollupKey, useCalendarRollupMap } from "./scheduling/calendars";
import { reactionsWatchingRefusal, useReactionEdges } from "./scheduling/reactions";
import { ReactionPanels } from "./scheduling/ReactionPanels";

// WC-R1: the dagre/React-Flow canvas is heavy (xyflow + dagre) and only complex
// graphs need it, so it loads on demand and shares its chunk with the editor.
const WorkflowCanvasLazy = lazy(() =>
  import("./workflows/WorkflowCanvas").then((m) => ({ default: m.WorkflowCanvas })),
);
import { FolderBrowser } from "../components/FolderBrowser";
import { AlertBanner, Btn, ConfirmDialog, DerivedAgencies, EmptyCell, ExpandChevron, RefreshButton, HoverTr, Modal, InlineTags, Pager, SearchBar, Section, SkeletonRows, TabBar, TagEditor, TagFilterSelect, Toast, jobStatusLabel, matchesStatus, matchesTags, statusLabel, statusTone, agencySortKey } from "../components/ui";
import { ColumnsMenu, TableHead, renderCells, useTableColumns, type TableColumn } from "../components/table";
import { type SortColumn } from "../utils/sort";
import { RevisionHistory } from "./RevisionHistory";
import { fmtInAppZone } from "../utils/datetime";

// F-3: the tabs no longer carry their own predicates. Each one is just its
// canonical label, matched by `matchesStatus` — the same fold the row's own
// badge uses. The hand-rolled versions had already drifted: `failed` compared
// bare `danger`, so a killed workflow read "Failed" in its Result column and
// was missing from the Failed tab; `running` missed queued the same way.
const WF_TABS = [
  { key: "all", label: "All" },
  { key: "running", label: statusLabel("running") },
  { key: "healthy", label: statusLabel("success") },
  { key: "failed", label: statusLabel("danger") },
] as const;


// E-4: there is no local status vocabulary any more. This map said "Passing"
// and "Warning" where the canonical statusLabel says "Success" and "Warn", so a
// workflow row disagreed with its own steps and with every other run surface in
// the app. A second copy of a vocabulary is how that drift happens.

// Default column widths (px) for the resizable Workflows table (V1.1-7).
// Every column gets an entry; only Workflow + Schedule carry a drag handle,
// the rest are fixed under the hybrid policy (Q5/Q6).
const COL_W: Record<string, number> = {
  workflow: 220,
  steps: 100,
  agency: 140,
  schedule: 150,
  lastRun: 150,
  nextRun: 150,
  critical: 90,
  result: 110,
  tags: 140,
  createdAt: 150,
  lastModifiedAt: 150,
  actions: 130,
};
// CO-2.5: the counts the header, the expanded row's colSpan and FolderBrowser's
// colCount need now come from the LIVE visible set (`cols.visible.length`), not
// from this map. COL_W survives as each column's DEFAULT width, which the spec
// reads and a stored `colw:` override outranks.
//
// The spec's row is {wf, displayName}: `displayName` differs per render mode
// (full path in search, filename in the browser) and is handed in per row by
// FolderBrowser, so it cannot be closed over when the spec is built.
export type WfRow = { wf: Workflow; displayName: string };

// Cap inline tags so every row stays a uniform height; overflow → "+N" chip, the
// full editable set lives in the expanded row (tags-support.md §6.6).
const MAX_INLINE_TAGS = 2;

// Sortable columns (TS-8, the sorting-update plan) — flat/search mode only;
// browse mode keeps the folder tree's alpha order. Result sorts by rank, worst
// first ascending (TS-Q5), on the same value the badge displays (disabled ⇒
// paused). Next Run mirrors its cell: a paused workflow shows "—", so it sorts
// as empty (last). Tags excluded (multi-value).
const WF_STATUS_RANK: Record<string, number> = {
  failure: 0,
  danger: 0,
  warning: 1,
  killed: 2,
  cancelled: 3,
  paused: 4,
  running: 5,
  queued: 6,
  success: 7,
};
// CO-1.2 — a factory rather than a module const, for the same reason as the Jobs
// twin: `critical` must sort on the value the row DISPLAYS, which may be an
// optimistic override held in the component's inlineAnnotation hook.
const wfSortCols = (resolveCritical: (w: Workflow) => boolean): SortColumn<Workflow>[] => [
  { key: "workflow", get: (w) => w.name, type: "text" },
  { key: "steps", get: (w) => (w.steps ?? []).length, type: "number" },
  { key: "schedule", get: (w) => w.schedule, type: "text" },
  { key: "lastRun", get: (w) => w.lastRunAt, type: "date" },
  { key: "nextRun", get: (w) => (w.disabled ? null : w.nextRunAt), type: "date" },
  // CO-Q6 — critical-first ascending, the TS-Q5 "worst first ascending" convention.
  { key: "critical", get: (w) => (resolveCritical(w) ? 0 : 1), type: "number", defaultDir: "asc" },
  { key: "result", get: (w) => (w.disabled ? "paused" : w.status), type: "rank", rank: WF_STATUS_RANK },
  { key: "createdAt", get: (w) => w.createdAt, type: "date" },
  { key: "lastModifiedAt", get: (w) => w.lastModifiedAt, type: "date" },
  // AF-1 — same documented TS-Q5 exception as the Jobs column. No scope is
  // passed: a workflow has none of its own, so it never sorts under "All".
  { key: "agency", get: (w) => agencySortKey(w.agencies), type: "text" },
];

// FB1 — a workflow's folder LOCATION is its source file path (the leading
// "workflows/" stripped) so the browser mirrors the Git tree; its IDENTITY for
// detail/expand/actions stays the DB id/name. They coincide for Git-synced
// workflows and diverge for Cronomicon-authored ones, which have no file — those
// fall back to the name as the path.
const workflowDisplayPath = (w: Workflow) => {
  // Strip the "workflows/" root and any stray leading/trailing slashes; if
  // nothing is left (e.g. source_path is null or just "workflows/") fall back to
  // the name so the workflow still appears in the tree rather than being dropped.
  const stripped = (w.sourcePath ?? "").replace(/^workflows\//, "").replace(/^\/+|\/+$/g, "");
  return stripped || w.name;
};
// Identity for the folder tree's leaf key — a unique stable string. Falls back
// to the name when id is absent (Cronomicon rows pre-persist).
const workflowName = (w: Workflow) => String(w.id ?? w.name);

/**
 * WorkflowAnnotation — the annotation panel for an expanded workflow row.
 *
 * It fetches the workflow's DETAIL rather than reading the list row it is handed.
 * The catalog list carries `critical` and `contact` only — notes are detail-only
 * server-side, so a page of rows does not haul up to 4KB each — and this page,
 * unlike Jobs, renders its expanded panel straight from the list row. Without
 * this fetch the section would show the chip and the contact and silently drop
 * the notes, which are the part someone actually came to read.
 *
 * A component rather than a hoisted hook because exactly one row is expanded at
 * a time (`expanded` is a single id, not a set), so mounting it with the panel
 * scopes the fetch to the row without a conditional hook.
 */
function WorkflowAnnotation({
  w,
  resolve,
  onSave,
  error,
}: {
  w: Workflow;
  resolve: (base: Annotation) => Annotation;
  onSave: (next: Annotation) => void;
  error?: string;
}) {
  const { data } = useGet<Workflow>(
    () => api.GET("/workflows/{workflowId}", { params: { path: { workflowId: w.id } } }),
    [w.id],
  );
  // Fall back to the list row until the detail lands: the chip and contact are
  // already correct there, so the section paints immediately and gains the notes.
  return (
    <AnnotationSection kind="workflow" value={resolve(annotationOf(data ?? w))} onSave={onSave} error={error} />
  );
}

export function Workflows() {
  const [refresh, setRefresh] = useState(0);
  const [expanded, setExpanded] = useState<number | null>(null);
  const [search, setSearch] = useState("");
  const [tagFilter, setTagFilter] = useState<string[]>([]);
  const [tagMatch, setTagMatch] = useState<"any" | "all">("any");
  // Optimistic per-workflow tag edits via the shared hook (CC.18): an expanded-row
  // edit repaints the column immediately, a per-id stale-guard drops superseded
  // PUTs, and a failure reverts. Keyed by DB id; reset on refresh (the refetch).
  const inlineTags = useInlineTags<Workflow>(
    refresh,
    (w) => String(w.id),
    (w) => w.tags,
    (w, next) =>
      api.PUT("/workflow-tags/{workflowId}", {
        params: { path: { workflowId: w.id }, header: csrfHeader },
        body: { tags: next },
      }),
  );
  // AN-3 — the annotation twin of the tags hook above (see Jobs.tsx).
  const inlineAnnotation = useInlineAnnotation<Workflow, Annotation>(
    refresh,
    (w) => String(w.id),
    (w, next) =>
      api.PUT("/workflow-annotation/{workflowId}", {
        params: { path: { workflowId: w.id }, header: csrfHeader },
        body: { critical: !!next.critical, contact: next.contact ?? "", notes: next.notes ?? "" },
      }),
  );
  const [tab, setTab] = useState<(typeof WF_TABS)[number]["key"]>("all");
  const [busyId, setBusyId] = useState<number | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [toast, fireToast] = useToast();
  const [deleting, setDeleting] = useState<Workflow | null>(null);
  // RH: which amadeus-source workflow's revision history is open, by name.
  const [historyFor, setHistoryFor] = useState<string | null>(null);
  const [delBusy, setDelBusy] = useState(false);
  // RX-24 — the server's refusal text when reactions watch this workflow. See
  // the twin in Jobs.tsx: non-null turns the confirm dialog into the forced one.
  const [delBlock, setDelBlock] = useState<string | null>(null);
  const [params, setParams] = useSearchParams();
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
  const location = useLocation();
  // Compose capability gates the amadeus-only Edit affordance (mirrors Jobs.tsx);
  // triggerJobs gates the Run/Pause controls (RB-3, mirrors Jobs.tsx — see the
  // longer note there on why this replaced the client-side role list and why it
  // remains a UX affordance rather than a boundary).
  const [canCompose, setCanCompose] = useState(false);
  const [canRun, setCanRun] = useState(false);
  useEffect(() => {
    fetchCapabilities().then((caps) => {
      setCanCompose(!!caps.compose);
      setCanRun(!!caps.triggerJobs);
    });
  }, []);

  // WB-O2: poll the list while any workflow is running/queued so live status
  // (and Next/Last run) updates without a manual refresh; stops once all settle.
  const { data, error, loading } = useLiveGet<unknown>(
    () => api.GET("/workflows"),
    [refresh],
    (d) => rows<Workflow>(d).some((w) => w.status === "running" || w.status === "queued"),
  );
  const items = rows<Workflow>(data);
  // R2F-3 — the Agency column tells the catalog rows apart; a dialog TITLE shows
  // the name alone, so it qualifies when this page holds two workflows by it.
  const wfTitleName = useNameDisambiguator(items, (w) => ({ uid: w.uid, name: w.name, agencies: w.agencies }));
  const cw = useColumnWidths("workflows");
  const activeTab = WF_TABS.find((t) => t.key === tab) ?? WF_TABS[0];
  const filtered = items.filter(
    (w) =>
      matchesStatus(activeTab.label, w.status) &&
      matchesTags(inlineTags.tagsFor(w), tagFilter, tagMatch) &&
      (!search ||
        w.name.toLowerCase().includes(search.toLowerCase()) ||
        (w.description ?? "").toLowerCase().includes(search.toLowerCase()) ||
        // AN-3/AN-Q5 — notes and contact join the predicate here because this
        // list is filtered client-side, so it costs nothing. The Jobs catalog is
        // server-paged and its `q` stays name-only: the same change there is a
        // query-level commitment, not a free one. The asymmetry is deliberate.
        (w.notes ?? "").toLowerCase().includes(search.toLowerCase()) ||
        (w.contact ?? "").toLowerCase().includes(search.toLowerCase()) ||
        // Searching the derived agency is the point of showing it: "what does my
        // department run?" is the first question once access is departmental.
        (w.agencies ?? []).some((a) => a.toLowerCase().includes(search.toLowerCase()))),
  );

  // Column sort (TS-8) — feeds the flat/search table only; browse mode hands
  // FolderBrowser the unsorted `filtered` and keeps the tree's alpha order.
  const sort = useTableSort(
    filtered,
    wfSortCols((w) => !!inlineAnnotation.valueFor(w, annotationOf(w)).critical),
    { key: "workflow", dir: "asc" },
    { tableId: "workflows" },
  );
  // TS-15: search mode used to render EVERY matching row (the one catalog with
  // no pager at all). Same client pager as Jobs/Scripts/Schedules; browse mode
  // still pages inside <FolderBrowser paginate>.
  const { pageItems, total, page, pager } = useClientPager(sort.sorted);

  // CAL-12 — one fetch for the page; empty when nothing on it binds a calendar.
  const rollupMap = useCalendarRollupMap();
  // RX-16 — one fetch for the page, like the calendar roll-up above: every
  // expanded row reads its own two directions out of the same edge list.
  const { edges: reactionEdges } = useReactionEdges();
  const calendarRollupFor = (w: Workflow) => rollupMap[rollupKey("workflow", w.source, w.name)] ?? "";

  // VU-14 — the status tab narrows the list as much as the search box does, so the
  // filtered-empty state returns all three to their defaults.
  const clearFilters = () => {
    setTab(WF_TABS[0].key);
    setSearch("");
    setTagFilter([]);
    pager.setPage(0);
  };


  // A handoff toast carried via navigation state (e.g. a workflow deleted from
  // the editor edit view, WB-E1). Show it once, then clear the history state so a
  // refresh doesn't replay it.
  useEffect(() => {
    const handoff = (location.state as { toast?: string } | null)?.toast;
    if (handoff) {
      fireToast(handoff);
      window.history.replaceState({}, "");
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const trigger = async (wf: Workflow) => {
    setBusyId(wf.id);
    setActionError(null);
    const { error: err } = await api.POST("/workflows/{workflowId}/trigger", {
      params: { path: { workflowId: wf.id }, header: csrfHeader },
    });
    setBusyId(null);
    if (err) {
      setActionError(`Trigger failed for ${wf.name}: ${(err as { message?: string })?.message ?? "request failed"}`);
      return;
    }
    // FX-E2 — the "ops notified" variant is gone with notifyOnTrigger itself:
    // the field had no backing column, was never true, and its branch here was
    // dead the day it was written. Nothing has ever notified anyone on trigger.
    fireToast(`Run queued: ${wf.name}`);
    setRefresh((n) => n + 1);
  };

  // AR — "Run later": park the trigger as a pending run at a chosen instant.
  // Visible on Schedules → Upcoming (badged ad-hoc, cancellable) until it fires.
  const [laterFor, setLaterFor] = useState<Workflow | null>(null);
  const [laterAt, setLaterAt] = useState("");
  const scheduleLater = async () => {
    if (!laterFor) return;
    const d = new Date(laterAt);
    if (!laterAt.trim() || Number.isNaN(d.getTime())) {
      setActionError("Pick a valid date and time.");
      return;
    }
    setBusyId(laterFor.id);
    setActionError(null);
    const { error: err } = await api.POST("/workflows/{workflowId}/trigger", {
      params: { path: { workflowId: laterFor.id }, header: csrfHeader },
      body: { runAt: d.toISOString() } as never,
    });
    setBusyId(null);
    if (err) {
      setActionError(`Schedule failed for ${laterFor.name}: ${(err as { message?: string })?.message ?? "request failed"}`);
      return;
    }
    fireToast(`Run scheduled: ${laterFor.name} · ${fmtInAppZone(d.toISOString(), { weekday: "short", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })}`);
    setLaterFor(null);
    setLaterAt("");
    setRefresh((n) => n + 1);
  };

  const toggleDisabled = async (wf: Workflow) => {
    setBusyId(wf.id);
    setActionError(null);
    const { error: err } = await api.PATCH("/workflows/{workflowId}", {
      params: { path: { workflowId: wf.id }, header: csrfHeader },
      body: { disabled: !wf.disabled },
    });
    setBusyId(null);
    if (err) {
      setActionError(`Update failed for ${wf.name}: ${(err as { message?: string })?.message ?? "request failed"}`);
      return;
    }
    fireToast(wf.disabled ? `Workflow resumed: ${wf.name}` : `Workflow paused: ${wf.name}`);
    setRefresh((n) => n + 1);
  };

  // Delete an amadeus-authored workflow from the expanded row. Same endpoint the
  // editor uses; the server rejects git-source rows with a 409, which is the real
  // guard — the button gate below is only UX. Collapses the row on success so the
  // list doesn't re-expand onto a deleted id.
  //
  // RX-24 — the route's OTHER 409 is "reactions watch this", told apart by its
  // error code and answerable with ?force=true, so it keeps the dialog open
  // rather than reporting a git-source reason that cannot be true here.
  const doDelete = async (wf: Workflow, force = false) => {
    setDelBusy(true);
    setActionError(null);
    const { response, error: err } = await api.DELETE("/workflows/{workflowId}", {
      params: { path: { workflowId: wf.id }, query: force ? { force: true } : {}, header: csrfHeader },
    });
    setDelBusy(false);
    if (err || !response.ok) {
      const watching = force ? null : reactionsWatchingRefusal(err);
      if (watching) {
        setDelBlock(watching);
        return; // dialog stays open, now offering the forced delete
      }
      setDeleting(null);
      setDelBlock(null);
      const msg =
        response.status === 409
          ? "Only amadeus-source workflows can be deleted in-app."
          : (err as { message?: string } | undefined)?.message ?? `Delete failed (${response.status}).`;
      setActionError(`Delete failed for ${wf.name}: ${msg}`);
      return;
    }
    setDeleting(null);
    setDelBlock(null);
    setExpanded(null);
    fireToast(`Workflow "${wf.name}" deleted.`);
    setRefresh((n) => n + 1);
  };

  // CO-2.3 — this view's headCell / fixedCell / tableHeader trio is GONE, and
  // with it the drift: its copies differed from the Jobs pair in ways nobody had
  // decided (a `th()` with no borderBottom, an actions <th> hand-written outside
  // the loop). <TableHead> is the one renderer; the per-table header style rides
  // in as `thStyle`.
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

  // CO-2.5 — the Workflows column spec, built during render for the same two
  // reasons as the Jobs twin: the cells close over component state (`expanded`,
  // `busyId`, the capability flags, the inline resolvers) and the styles read
  // `c.*`, which applyTheme reassigns.
  const workflowColumnSpec = (): TableColumn<WfRow>[] => [
    {
      key: "workflow",
      label: "Workflow",
      sortKey: "workflow",
      width: COL_W.workflow,
      // CO-Q4 — first-declared is pinned first, the Jobs twin's rule.
      pin: "first",
      cell: ({ wf: w, displayName }) => (
        <>
          <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
            <span style={{ fontWeight: 600 }}>{displayName}</span>
            {/* FX-8 — one flag, one word. `w.disabled` used to render as "disabled"
                here, "Paused" in the Result column and "Pause"/"Resume" on the button,
                so an operator scanning for the state the button names never found it.
                jobStatusLabel is the single source, exactly as Jobs uses it. */}
            {w.disabled && <span style={badgeStyle(c.textSec)}>{jobStatusLabel("paused")}</span>}
          </div>
          {w.description && <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 2 }}>{w.description}</div>}
        </>
      ),
    },
    {
      key: "steps",
      label: "Steps",
      sortKey: "steps",
      width: COL_W.steps,
      fixed: true,
      tdStyle: { color: c.textSec },
      cell: ({ wf: w }) => {
        const n = (w.steps ?? []).length;
        return `${n} step${n !== 1 ? "s" : ""}`;
      },
    },
    {
      key: "agency",
      label: "Agency",
      sortKey: "agency",
      width: COL_W.agency,
      // RB-23 (RF-17): the workflow's agencies, DERIVED through its constituent
      // jobs' scopes — a workflow has no scope of its own, so a workflow spanning
      // two departments lists both. AF-1 — no `scope` prop, so it never shows the
      // All chip: a workflow whose jobs are all global simply has none to show.
      cell: ({ wf: w }) => (
        <DerivedAgencies agencies={w.agencies ?? []} derivedFrom="Derived from the scopes of this workflow's jobs" />
      ),
    },
    {
      key: "schedule",
      label: "Schedule",
      sortKey: "schedule",
      width: COL_W.schedule,
      tdStyle: { fontFamily: c.mono, fontSize: c.fontXs, color: c.textSec, whiteSpace: "nowrap" },
      cell: ({ wf: w }) => w.schedule || "manual",
    },
    {
      key: "lastRun",
      label: "Last Run",
      sortKey: "lastRun",
      width: COL_W.lastRun,
      fixed: true,
      tdStyle: { color: c.textSec, whiteSpace: "nowrap" },
      cell: ({ wf: w }) => fmtTime(w.lastRunAt),
    },
    {
      key: "nextRun",
      label: "Next Run",
      sortKey: "nextRun",
      width: COL_W.nextRun,
      fixed: true,
      tdStyle: { color: c.textSec, whiteSpace: "nowrap" },
      cell: ({ wf: w }) => (w.disabled ? <EmptyCell /> : fmtTime(w.nextRunAt)),
    },
    {
      key: "critical",
      label: "Critical",
      sortKey: "critical",
      width: COL_W.critical,
      fixed: true,
      // AN-3 put Critical beside the result chip; CO-Q7 keeps the adjacency but
      // gives it its own column, so it can be sorted and hidden. Not duplicated
      // into both cells — see the Jobs twin.
      cell: ({ wf: w }) => (inlineAnnotation.valueFor(w, annotationOf(w)).critical ? <CriticalChip /> : <EmptyCell />),
    },
    {
      key: "result",
      label: "Result",
      sortKey: "result",
      width: COL_W.result,
      fixed: true,
      cell: ({ wf: w }) => (
        <span style={badgeStyle(w.disabled ? c.textSec : statusTone(w.status).color)}>
          {w.disabled ? jobStatusLabel("paused") : statusLabel(w.status ?? "") || "—"}
        </span>
      ),
    },
    {
      key: "tags",
      label: "Tags",
      width: COL_W.tags,
      tdStyle: { whiteSpace: "nowrap" },
      cell: ({ wf: w }) => <InlineTags tags={inlineTags.tagsFor(w)} max={MAX_INLINE_TAGS} />,
    },
    {
      key: "createdAt",
      label: "Created On",
      sortKey: "createdAt",
      width: COL_W.createdAt,
      fixed: true,
      tdStyle: { color: c.textSec, whiteSpace: "nowrap" },
      cell: ({ wf: w }) => fmtDateTime(w.createdAt),
    },
    {
      key: "lastModifiedAt",
      label: "Last Edited",
      sortKey: "lastModifiedAt",
      width: COL_W.lastModifiedAt,
      fixed: true,
      tdStyle: { color: c.textSec, whiteSpace: "nowrap" },
      cell: ({ wf: w }) => fmtDateTime(w.lastModifiedAt),
    },
    {
      key: "actions",
      label: "",
      // The header is deliberately blank, but the Columns menu still has to name
      // this row — "Always last" beside an empty label explains nothing.
      menuLabel: "Actions",
      width: COL_W.actions,
      fixed: true,
      // CO-Q4 — pinned last. Actions in column 3 is not a preference, it is a
      // mistake the UI should not offer.
      pin: "last",
      cell: ({ wf: w }) => (
        <div style={{ display: "flex", alignItems: "center", gap: 8, justifyContent: "flex-end" }} onClick={(e) => e.stopPropagation()}>
          {canRun && <Btn small disabled={busyId === w.id} onClick={() => trigger(w)}>▶ Run</Btn>}
          {canRun && (
            <Btn
              small
              disabled={busyId === w.id}
              title="Schedule a one-time run of this workflow"
              onClick={() => {
                const d = new Date(Date.now() + 60 * 60 * 1000);
                d.setMinutes(0, 0, 0);
                const pad = (n: number) => String(n).padStart(2, "0");
                setLaterAt(`${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`);
                setLaterFor(w);
              }}
            >
              ⏱ Run later
            </Btn>
          )}
          {/* Edit only for amadeus rows the caller may compose (hidden for git
              rows + non-admins). Delete lives in the editor behind a confirm. */}
          {canCompose && w.source === "amadeus" && (
            <Link to={`/workflow-editor?id=${w.id}`} style={{ textDecoration: "none" }}>
              <Btn small>Edit</Btn>
            </Link>
          )}
          <ExpandChevron open={expanded === w.id} />
        </div>
      ),
    },
  ];

  const cols = useTableColumns<WfRow>("workflows", workflowColumnSpec());

  // One workflow row, reused by the flat search results (displayName = full path,
  // so the folder is visible) and the folder browser (displayName = filename,
  // since the breadcrumb already shows the path). Identity/expand key + all
  // actions stay keyed on the DB id — the folder location is purely cosmetic.
  const renderWorkflowRow = (w: Workflow, displayName: string) => {
    const isExp = expanded === w.id;
    // Still needed by the EXPANDED panel's Tags section below; the row's own tag
    // column reads the same resolver from inside its cell.
    const tags = inlineTags.tagsFor(w);
    return (
      <Fragment key={w.id}>
        <HoverTr
          onClick={() => setExpanded(isExp ? null : w.id)}
          tint={isExp ? c.primaryBg : w.disabled ? `${c.textMuted}10` : undefined}
          hoverTint={isExp ? c.primaryBg : c.panelHover}
          style={{
            // Unlike Jobs, the ROW carries the border here, so no per-cell
            // rowStyle override is needed for the expanded case.
            borderBottom: isExp ? "none" : `1px solid ${c.border}`,
            opacity: w.disabled ? 0.65 : 1,
          }}
        >
          {renderCells(cols.visible, { wf: w, displayName }, { base: td })}
        </HoverTr>
        {isExp && (
          <tr style={{ borderBottom: `1px solid ${c.border}` }}>
            <td colSpan={cols.visible.length} style={{ padding: "16px 18px", background: c.primaryBg }}>
              {/* EV-3 — the meta strip that used to sit here (Last run / Next run /
                  Schedule) is gone: it restated three columns of the row it hangs
                  off, verbatim. Actions keep their top-right home (EV-4). */}
              {/* EP-4 Shape A — the scope wraps the whole panel so the workflow
                  detail fetch, the runs table and the reaction panels all
                  re-fetch together. Refresh leads the strip. */}
              <RefreshScope>
              <div style={{ display: "flex", justifyContent: "flex-end", gap: 6, flexWrap: "wrap" }}>
                <RefreshButton />
                <span style={{ display: "flex", gap: 6 }}>
                  {/* FX-8 — the same gate its siblings carry (Run at :280, Edit/Delete on
                      canCompose): pausing a workflow is a trigger-class action, so a
                      Viewer should not be offered it. No source gate — verified against
                      PATCH /workflows/{id}, which unlike DELETE does not reject git-source
                      rows, so a git workflow genuinely can be paused in-app and hiding the
                      button would remove a working action. (The backend PATCH enforces
                      neither role nor scope today, so this gate is UX consistency in the
                      same sense the triggerJobs capability documents — not a boundary.) */}
                  {canRun && (
                    <Btn small disabled={busyId === w.id} onClick={() => toggleDisabled(w)}>
                      {w.disabled ? "Resume" : "Pause"}
                    </Btn>
                  )}
                  {/* Delete was previously editor-only; it is now also a row action
                      here, behind the same confirm and the same amadeus-only gate
                      as Edit. Destructive last, dangerQuiet — Runners' order. */}
                  {/* RH: in-app definitions get history here; git rows get it from Git. */}
                  {canCompose && w.source === "amadeus" && (
                    <Btn small onClick={() => setHistoryFor(w.name ?? "")}>
                      History
                    </Btn>
                  )}
                  {canCompose && w.source === "amadeus" && (
                    <Btn small dangerQuiet disabled={busyId === w.id || delBusy} onClick={() => setDeleting(w)}>
                      Delete
                    </Btn>
                  )}
                </span>
              </div>

              <div style={{ display: "flex", gap: 36, flexWrap: "wrap", alignItems: "flex-start", marginTop: 14 }}>
                <div style={{ flex: "2 1 420px", minWidth: 0, display: "flex", flexDirection: "column", gap: 22 }}>
                  {/* AN-3 — above Steps, for the reason the jobs twin sits above
                      the overview grid: it says what this is and who owns it,
                      which you need before the graph underneath means anything. */}
                  <WorkflowAnnotation
                    w={w}
                    resolve={(base) => inlineAnnotation.valueFor(w, base)}
                    onSave={(next) => inlineAnnotation.save(w, next)}
                    error={inlineAnnotation.errors[String(w.id)]}
                  />

                  <Section title={`Steps (${(w.steps ?? []).length})`}>
                    {/* WC-R1: the numbered chain for graphs it can draw honestly;
                        the dagre canvas (pan/zoom/minimap) once the graph nests
                        containers inside branch arms — which StepChain flattens —
                        or grows past a single readable horizontal row. */}
                    {(() => {
                      const g = normalizeGraph(w.steps ?? []);
                      return g.useCanvas ? (
                        <Suspense fallback={<div style={{ color: c.textSec, fontSize: c.fontSm }}>Loading graph…</div>}>
                          <WorkflowCanvasLazy steps={g.def} height={420} />
                        </Suspense>
                      ) : (
                        <StepChain steps={w.steps ?? []} />
                      );
                    })()}
                  </Section>

                  {/* CAL-12 — "does this workflow run on holidays?" in one place.
                      Taken from the server's roll-up (one /schedules fetch for the
                      page): unlike the Jobs detail, a workflow catalog row carries
                      crons rather than its schedule entries, so there is nothing
                      local to compute it from. */}
                  {calendarRollupFor(w) && (
                    <Section title="Working calendars">
                      <div style={{ fontSize: c.fontSm, color: c.textSec }}>{calendarRollupFor(w)}</div>
                    </Section>
                  )}

                  {/* RX-16 — both directions of the reaction graph. A workflow's
                      internal coupling is visible in its step graph; its
                      CROSS-workflow coupling is not visible anywhere else. */}
                  <ReactionPanels edges={reactionEdges} kind="workflow" source={w.source} name={w.name} />

                  <Section
                    title={`Tags${tags.length ? ` (${tags.length})` : ""}`}
                    info="Tags are stored in Cronomicon only — they are not written back to Git, and they are kept across syncs."
                  >
                    <TagEditor tags={tags} onChange={(next) => inlineTags.save(w, next)} />
                    {inlineTags.errors[String(w.id)] && (
                      <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 6 }}>{inlineTags.errors[String(w.id)]}</div>
                    )}
                  </Section>
                </div>

                {/* Activity column — the Jobs page's RecentRuns shape: a compact,
                    workflow-pinned instance of the shared WorkflowRunsTable, with
                    the same step drill-down History has. This REPLACED the page's
                    bottom "Recent Workflow Runs" table (and the "↓ Run History"
                    filter button that pointed at it): with the table living in the
                    row, the bottom copy only duplicated History → Workflow Runs.
                    One storageKey across all rows, like the Jobs drill-in. */}
                <div style={{ flex: "1.9 1 430px", minWidth: 0, display: "flex", flexDirection: "column", gap: 22 }}>
                  <Section title="Recent runs">
                    <WorkflowRunsTable
                      compact
                      refresh={refresh}
                      filterWorkflow={{ id: w.id, name: w.name, source: w.source }}
                      storageKey="workflow-detail-runs"
                    />
                  </Section>
                </div>
              </div>
              </RefreshScope>
            </td>
          </tr>
        )}
      </Fragment>
    );
  };

  return (
    <div>
      <TabBar
        tabs={WF_TABS.map((t) => `${t.label} (${items.filter((w) => matchesStatus(t.label, w.status)).length})`)}
        active={WF_TABS.findIndex((t) => t.key === tab)}
        onChange={(i) => setTab(WF_TABS[i].key)}
      />

      <div style={{ display: "flex", alignItems: "center", gap: 12, marginBottom: 16, flexWrap: "wrap" }}>
        <SearchBar value={search} onChange={setSearch} placeholder="Search workflows…" style={{ flex: 1, minWidth: 200, maxWidth: 340 }} />
        <TagFilterSelect items={items} selected={tagFilter} onChange={setTagFilter} getTags={(w) => inlineTags.tagsFor(w)} matchMode={tagMatch} onMatchModeChange={setTagMatch} />
        {/* CO-3 — beside the other things that change what this table shows. */}
        <ColumnsMenu cols={cols} cw={cw} />
        {/* I-1 (VF-11) — same gate as the empty state's "Create a workflow"
            below, and as the editor's own. Ungated, it was a primary button
            leading a Viewer to WorkflowEditor's refusal notice. */}
        {canCompose && (
          <Link to="/workflow-editor" style={{ textDecoration: "none" }}>
            <Btn primary style={{ padding: "8px 14px", fontSize: c.fontSm }}>+ Create</Btn>
          </Link>
        )}
      </div>

      {actionError && (
        <AlertBanner type="danger" onDismiss={() => setActionError(null)}>
          {actionError}
        </AlertBanner>
      )}

      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {error && <div style={{ color: c.danger }}>Error: {error}</div>}
      {/* VU-14 — the empty catalog offers the Workflow Editor (gated on Compose,
          which is what the editor itself enforces) and names Git as the other
          source; a filtered-empty list offers the tab, search and tags back. */}
      {!loading && !error && items.length === 0 && (
        <div style={{ color: c.textSec }}>
          <div>No workflows yet. Workflows are synced from the workflows/ directory of the definitions repo, or authored in the workflow editor.</div>
          {canCompose && (
            <div style={{ marginTop: 12 }}>
              <Link to="/workflow-editor" style={{ textDecoration: "none" }}>
                <Btn small>Create a workflow</Btn>
              </Link>
            </div>
          )}
        </div>
      )}
      {!loading && !error && items.length > 0 && filtered.length === 0 && (
        <div style={{ color: c.textSec }}>
          <div>No workflows match your filters.</div>
          <div style={{ marginTop: 12 }}>
            <Btn small onClick={clearFilters}>Clear filters</Btn>
          </div>
        </div>
      )}

      {filtered.length > 0 &&
        (search.trim() ? (
          // Search mode: flat results across all folders, full path shown,
          // sorted (TS-8) and paged client-side (TS-15).
          <>
            <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm, tableLayout: "fixed" }}>
              <thead>{tableHeader()}</thead>
              <tbody>{pageItems.map((w) => renderWorkflowRow(w, workflowDisplayPath(w)))}</tbody>
            </table>
            <Pager pager={pager} page={page} total={total} noun="workflows" />
          </>
        ) : (
          // Browse mode: navigate the folder tree one level at a time.
          <FolderBrowser
            // TS-24: the sorted array, so the active column orders each folder's
            // leaves; folders themselves stay alpha-first.
            items={sort.sorted}
            preserveLeafOrder
            getPath={workflowDisplayPath}
            getName={workflowName}
            path={path}
            onNavigate={setPath}
            rootLabel="Workflows"
            header={tableHeader()}
            colCount={cols.visible.length}
            renderLeaf={(leaf) => renderWorkflowRow(leaf.item, leaf.label)}
          />
        ))}

      {laterFor && (
        <Modal title={`Run ${wfTitleName(laterFor)} later`} onClose={() => setLaterFor(null)}>
          <div style={{ fontSize: c.fontSm, color: c.textSec, marginBottom: 12, lineHeight: 1.5 }}>
            Schedules a <strong>one-time run</strong> of this workflow. It appears under{" "}
            <strong>Schedules → Upcoming</strong> (badged <em>ad-hoc</em>) until it fires, where it can be
            cancelled. Entered in your timezone.
          </div>
          <input
            type="datetime-local"
            value={laterAt}
            onChange={(e) => setLaterAt(e.target.value)}
            style={{ background: c.bg, color: c.text, border: `1px solid ${c.border}`, borderRadius: c.radiusChip, padding: "6px 8px", fontSize: c.fontSm, width: 220 }}
          />
          <div style={{ display: "flex", justifyContent: "flex-end", gap: 8, marginTop: 16 }}>
            <Btn small onClick={() => setLaterFor(null)}>Cancel</Btn>
            <Btn small primary disabled={busyId === laterFor.id} onClick={scheduleLater}>
              {busyId === laterFor.id ? "Scheduling…" : "Schedule run"}
            </Btn>
          </div>
        </Modal>
      )}

      {historyFor && (
        <RevisionHistory
          kind="workflow"
          name={historyFor}
          onClose={() => setHistoryFor(null)}
          onRestored={() => setRefresh((n) => n + 1)}
        />
      )}
      {deleting && (
        <ConfirmDialog
          title={delBlock ? "Delete Workflow — reactions watch it" : "Delete Workflow"}
          message={
            delBlock ? (
              <>
                <div style={{ color: c.danger, marginBottom: 10 }}>{delBlock}</div>
                Deleting anyway keeps those reactions rather than removing them: they will show as{" "}
                <strong>missing</strong> on <Link to="/schedules?tab=reactions">Schedules → Reactions</Link> and can
                never fire until you repoint or delete them.
              </>
            ) : (
              <>
                Delete <strong>{deleting.name}</strong>? Its run history is kept, but the workflow, its steps and
                its schedule are removed. The jobs it referenced are not deleted. This cannot be undone.
              </>
            )
          }
          confirmLabel={delBlock ? "Delete anyway" : "Delete Workflow"}
          busy={delBusy}
          onConfirm={() => doDelete(deleting, !!delBlock)}
          onCancel={() => {
            setDeleting(null);
            setDelBlock(null);
          }}
        />
      )}

            <Toast message={toast} />
    </div>
  );
}

const th = (): React.CSSProperties => ({
  padding: "12px 16px",
  fontFamily: c.sansCond,
  fontWeight: 600,
  fontSize: c.fontXs,
  textTransform: "uppercase",
  letterSpacing: 0.7,
  color: c.textMuted,
  whiteSpace: "nowrap",
});
const td: React.CSSProperties = { padding: "10px 16px", verticalAlign: "top" };

// Absolute app-zone datetime for the Created On / Last Edited columns (V1.1-17);
// `—` when null (git rows). Routes through the shared app-zone formatter (§5.2).
// Distinct from the compact fmtTime used for run times.
function fmtDateTime(v?: string | null): string {
  return fmtInAppZone(v);
}

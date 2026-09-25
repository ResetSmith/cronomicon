import { Fragment, useState } from "react";
import { Link } from "react-router-dom";
import { api, csrfHeader } from "../../api/client";
import { useLiveGet, paged, useColumnWidths, useTableSort } from "../../hooks";
import { ColumnsMenu, TableHead, renderCells, useTableColumns, type TableColumn } from "../../components/table";
import { useNameDisambiguator } from "../../utils/disambiguate";
import { type SortColumn } from "../../utils/sort";
import { c } from "../../theme";
import { btnStyle } from "./shared";
import { WorkflowRunDetail } from "./WorkflowRunDetail";
import { CalendarFilterSelect } from "../scheduling/CalendarFilterSelect";
import {
  EmptyRow,
  ErrorMsg,
  FilterSelect,
  Pager,
  SearchInput,
  StatusBadge,
  filterBar,
  fmtDuration,
  fmtTime,
  mono,
  shortTrace,
  td,
  th,
  usePager,
} from "../history/shared";
import { Btn, ExpandChevron, HoverTr, SkeletonRows, TableSurface, statusLabel } from "../../components/ui";
import { RefreshScope } from "../../components/RefreshScope";

interface WorkflowRun {
  traceId: string;
  workflowId?: number;
  workflowName?: string;
  // R2-1 identity + the union of the workflow's jobs' agencies (R2F-3), so a
  // run row can qualify a name two departments share.
  workflowUid?: string | null;
  agencies?: string[];
  status?: string;
  triggeredBy?: string | null;
  scheduleName?: string | null;
  startedAt?: string | null;
  completedAt?: string | null;
  durationMs?: number | null;
  jobTraceIds?: string[];
}

// F-1 — the filter speaks the display vocabulary, not the wire's. It used to
// offer the raw statuses, so the operator picked "danger" and got rows whose
// Status column said Failed, while a soft-cancelled run had no option at all.
// Each entry is now a wire value the server filters on, shown under the exact
// label statusLabel gives the row itself: pick Failed, get rows that say Failed.
//
// `queued` is deliberately absent: statusLabel folds it into Running, and the
// server groups the pair, so it is one option rather than two labelled the same.
// `cancelled` is present and works — see workflowStatusFilter in
// execution_mount.go, which had to fold the cancelled flag in for it to.
const STATUS_WIRE = ["running", "success", "warning", "danger", "skipped", "cancelled"] as const;
type StatusWire = (typeof STATUS_WIRE)[number];
const STATUSES = ["All", ...STATUS_WIRE.map((s) => statusLabel(s))];
const wireFor = (label: string): StatusWire | undefined => STATUS_WIRE.find((s) => statusLabel(s) === label);

// Default column widths (px) for table-layout:fixed before the user drags.
const COL_W: Record<string, number> = {
  workflow: 200,
  trace: 120,
  started: 150,
  completed: 150,
  user: 140,
  schedule: 150,
  jobs: 70,
  duration: 100,
  status: 110,
};

// Sortable columns (TS-23, the sorting-update plan). serverSide: the keys go to
// GET /workflow-runs as ?sort=&order= and the DATABASE orders the whole dataset
// before pagination. `get`/`type` here only describe the columns for the header
// affordance; no client re-sort happens. Trace and Jobs (count) stay unsortable.
const SORT_COLS: SortColumn<WorkflowRun>[] = [
  { key: "workflow", get: (w) => w.workflowName, type: "text" },
  { key: "schedule", get: (w) => w.scheduleName, type: "text" },
  { key: "started", get: (w) => w.startedAt, type: "date" },
  { key: "completed", get: (w) => w.completedAt, type: "date" },
  { key: "user", get: (w) => w.triggeredBy, type: "text" },
  { key: "duration", get: (w) => w.durationMs, type: "number" },
  { key: "status", get: (w) => w.status, type: "text" },
];

// Shared workflow-runs table (WB-O5): server-side paging + status/search filter +
// per-run step drill-down (WorkflowRunDetail), consumed by BOTH the Workflows page
// (expanded-row Activity column, compact + workflow-pinned) and History
// (WorkflowRunsTab, full). One component, no drift — both surfaces page
// server-side AND drill into steps.
//
// `compact` is the expanded-row embedding (the Jobs page's RecentRuns shape): the
// workflow filter is pinned by the caller, so the Workflow/Schedule/Completed/
// Triggered-By columns and the search/status filter bar would restate context or
// crowd a half-width column — they are dropped. Paging, sorting on the surviving
// columns, cancel, and the step drill-down all stay.

export function WorkflowRunsTable({
  title,
  filterWorkflow,
  onClearFilter,
  refresh = 0,
  storageKey,
  compact = false,
  reactedTo,
  traceFocus,
}: {
  title?: string;
  filterWorkflow?: { id: number; name: string; source?: string } | null;
  onClearFilter?: () => void;
  refresh?: number;
  storageKey: string;
  compact?: boolean;
  // RX-17 — the reverse because-of pivot: show only the workflow runs CAUSED BY
  // this run. A reaction can fire either kind, so answering the question only
  // for job runs answers half the graph — which is exactly what it did before.
  reactedTo?: string;
  // RX-17 — pivot to one workflow run by trace id. The forward because-of link
  // cannot know which table its upstream lives in, so it offers both tabs.
  traceFocus?: string;
}) {
  const [search, setSearchRaw] = useState("");
  const [status, setStatusRaw] = useState("All");
  // CAL-29, workflow half — the same audit question as the Executions tab, over
  // the workflow skip rows CAL-32 writes. Server-side against the structured
  // suppressed_by_calendar column, never the reason text.
  const [calendarFilter, setCalendarFilterRaw] = useState("");
  const [expanded, setExpanded] = useState<string | null>(null);
  const [localRefresh, setLocalRefresh] = useState(0);
  const [cancelling, setCancelling] = useState<string | null>(null);
  const pager = usePager();
  const cw = useColumnWidths(storageKey);

  // TS-23: serverSide — the hook owns only the header state (carets/aria/
  // persistence); the DATABASE does the ordering via ?sort=&order= below, so
  // the rows array passed here is irrelevant and a sort change refetches from
  // page 1. tableId = storageKey so the two instances (Workflows page, History
  // tab) persist their sort separately, like their column widths.
  const sort = useTableSort<WorkflowRun>([], SORT_COLS, { key: "started", dir: "desc" }, {
    tableId: storageKey,
    serverSide: true,
    onChange: () => pager.setPage(0),
  });

  // WB-S2: soft-cancel a running run, then force an immediate refetch.
  async function cancelRun(traceId: string) {
    setCancelling(traceId);
    await api.POST("/workflows/runs/{traceId}/cancel", { params: { path: { traceId }, header: csrfHeader } });
    setCancelling(null);
    setLocalRefresh((n) => n + 1);
  }

  // Server-side paging (status sent to the server; free-text search refines the
  // current page only). WB-O2: poll while any run on the page is in flight.
  const { data, error, loading } = useLiveGet<unknown>(
    () =>
      api.GET("/workflow-runs", {
        params: {
          query: {
            page: pager.page + 1,
            pageSize: pager.pageSize,
            // WB-D2: filter by the stable workflow name (+source) rather than the
            // reusable rowid, so renames/recreates don't strand or mis-attribute runs.
            ...(filterWorkflow ? { workflowName: filterWorkflow.name, ...(filterWorkflow.source ? { workflowSource: filterWorkflow.source as "git" | "cronomicon" } : {}) } : {}),
            ...(wireFor(status) ? { status: wireFor(status) } : {}),
            // "*" matches any calendar suppression; a name matches one.
            ...(calendarFilter ? { calendar: calendarFilter } : {}),
            ...(reactedTo ? { reactedTo } : {}),
            ...(traceFocus ? { trace: traceFocus } : {}),
            // TS-23: the dataset-wide sort. "started desc" matches the endpoint
            // default; sent explicitly so the header state and the wire agree.
            ...(sort.sortKey
              ? {
                  sort: sort.sortKey as "workflow" | "schedule" | "started" | "completed" | "user" | "duration" | "status",
                  order: sort.sortDir,
                }
              : {}),
          },
        },
      }),
    [pager.page, pager.pageSize, status, calendarFilter, filterWorkflow?.name, filterWorkflow?.source, refresh, localRefresh, sort.sortKey, sort.sortDir],
    (d) => paged<WorkflowRun>(d).items.some((w) => w.status === "running" || w.status === "queued"),
  );
  const pg = paged<WorkflowRun>(data);
  const items = pg.items;

  const setSearch = (v: string) => {
    setSearchRaw(v);
    pager.setPage(0);
  };
  const setStatus = (v: string) => {
    setStatusRaw(v);
    pager.setPage(0);
  };
  const setCalendarFilter = (v: string) => {
    setCalendarFilterRaw(v);
    pager.setPage(0);
  };

  const q = search.toLowerCase();
  const rowsShown = items.filter((w) => !q || (w.workflowName ?? "").toLowerCase().includes(q) || w.traceId.toLowerCase().includes(q));
  // Keyed on the rows actually rendered, so the badge reflects what is on screen.
  const wfLabel = useNameDisambiguator(rowsShown, (w) => ({ uid: w.workflowUid, name: w.workflowName, agencies: w.agencies }));

  // VU-14 — `status` and the per-workflow filter are server params, so an empty
  // page under either is a no-match, not "no workflow has ever run". The clear
  // action drops whichever narrowing this surface owns; the workflow filter has
  // its own clear control in the header (and belongs to the caller). In compact
  // the pinned workflow is the table's SCOPE, not a filter — an empty page there
  // means "this workflow has never run", and there is nothing to clear.
  const filtersActive = !!search || status !== "All" || !!calendarFilter || (!compact && !!filterWorkflow);
  // CO-6: was `compact ? 5 : 9` — two hand-typed counts that had to track the
  // conditional header. Derived from the live visible set now.
  const clearFilters = () => {
    setSearchRaw("");
    setStatusRaw("All");
    onClearFilter?.();
    pager.setPage(0);
  };

  // CO-6 — the column spec, replacing this component's headCell/fixedCell pair.
  //
  // `compact` decides which columns EXIST, not which are hidden: the embedded
  // instance is a deliberately narrower table, so its four extra columns are
  // absent from the spec rather than present-and-hidden. That keeps the two
  // concepts apart — a preset the caller chose vs. a preference the operator
  // chose — and means `cols.visible.length` is the honest count in both.
  //
  // The two instances already keep separate widths and sort under distinct
  // storageKeys (Workflows' expanded row vs the History tab), so their column
  // preferences separate for free on the same key.
  const spec: TableColumn<WorkflowRun>[] = [
    ...(compact
      ? []
      : [
          {
            key: "workflow",
            label: "Workflow",
            sortKey: "workflow",
            width: COL_W.workflow,
            pin: "first" as const,
            tdStyle: { fontWeight: 600, overflow: "hidden", textOverflow: "ellipsis" },
            cell: (w: WorkflowRun) => wfLabel(w) || "—",
          },
        ]),
    {
      key: "trace",
      label: "Trace ID",
      width: COL_W.trace,
      // In compact mode this is the leading column, so it takes the pin there.
      ...(compact ? { pin: "first" as const } : {}),
      cell: (w) => (
        <code style={{ ...mono(), color: c.accent, fontWeight: 600 }} title={w.traceId}>
          {shortTrace(w.traceId)}
        </code>
      ),
    },
    { key: "started", label: "Started", sortKey: "started", width: COL_W.started, fixed: true, tdStyle: { color: c.textSec, fontSize: c.fontSm }, cell: (w) => fmtTime(w.startedAt) },
    ...(compact
      ? []
      : [
          { key: "completed", label: "Completed", sortKey: "completed", width: COL_W.completed, fixed: true, tdStyle: { color: c.textSec, fontSize: c.fontSm }, cell: (w: WorkflowRun) => fmtTime(w.completedAt) },
          {
            key: "user",
            label: "Triggered By",
            sortKey: "user",
            width: COL_W.user,
            tdStyle: { color: c.textSec, fontSize: c.fontSm, overflow: "hidden", textOverflow: "ellipsis" },
            cell: (w: WorkflowRun) => w.triggeredBy ?? "Cronomicon",
          },
          {
            key: "schedule",
            label: "Schedule",
            sortKey: "schedule",
            width: COL_W.schedule,
            tdStyle: { color: c.textSec, fontSize: c.fontSm, overflow: "hidden", textOverflow: "ellipsis" },
            cell: (w: WorkflowRun) => w.scheduleName || "—",
          },
        ]),
    { key: "jobs", label: "Jobs", width: COL_W.jobs, fixed: true, tdStyle: { color: c.textSec, fontSize: c.fontSm }, cell: (w) => w.jobTraceIds?.length ?? 0 },
    { key: "duration", label: "Duration", sortKey: "duration", width: COL_W.duration, fixed: true, tdStyle: { ...mono(), fontSize: c.fontSm, textAlign: "right" }, cell: (w) => fmtDuration(w.durationMs) },
    {
      key: "status",
      label: "Result",
      sortKey: "status",
      width: COL_W.status,
      fixed: true,
      // Carries the row's disclosure chevron and the live-run Cancel, so it is
      // pinned last — hiding it would strip both controls off every row.
      pin: "last",
      cell: (w) => {
        const live = w.status === "running" || w.status === "queued";
        return (
          <span style={{ display: "inline-flex", alignItems: "center", gap: 8 }}>
            <StatusBadge status={w.status} />
            {live && (
              <button
                onClick={(e) => {
                  e.stopPropagation();
                  cancelRun(w.traceId);
                }}
                disabled={cancelling === w.traceId}
                title="Soft-cancel this run"
                style={{
                  padding: "2px 8px",
                  borderRadius: c.radiusChip,
                  border: `1px solid ${c.danger}50`,
                  background: "transparent",
                  color: c.danger,
                  fontSize: c.fontXs,
                  fontWeight: 600,
                  cursor: "pointer",
                  whiteSpace: "nowrap",
                }}
              >
                {cancelling === w.traceId ? "…" : "Cancel run"}
              </button>
            )}
            <ExpandChevron open={expanded === w.traceId} />
          </span>
        );
      },
    },
  ];
  const cols = useTableColumns<WorkflowRun>(storageKey, spec);

  return (
    <div>
      {!compact && (title || filterWorkflow) && (
        <div style={{ display: "flex", alignItems: "center", gap: 10, marginBottom: 12 }}>
          {title && <h2 style={{ fontSize: c.fontHead, margin: 0 }}>{title}</h2>}
          {filterWorkflow && onClearFilter && (
            <button onClick={onClearFilter} style={{ ...btnStyle(), color: c.primary, borderColor: `${c.primary}50` }}>
              {filterWorkflow.name} × clear filter
            </button>
          )}
        </div>
      )}

      {!compact && (
        <div style={filterBar}>
          <SearchInput value={search} onChange={setSearch} placeholder="Search this page by workflow or trace ID…" />
          <FilterSelect label="Result" value={status} options={STATUSES} onChange={setStatus} />
          <CalendarFilterSelect value={calendarFilter} onChange={setCalendarFilter} />
          <ColumnsMenu cols={cols} cw={cw} />
        </div>
      )}

      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {error && <ErrorMsg msg={error} />}
      {!loading && !error && (
        // No enclosing Card: the table sits between a top and bottom rule instead
        // of inside a fourth box (VU-5). Pager already brings its own top rule and
        // padding, so it reads as the table's footer rather than a floating strip.
        <TableSurface>
          <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm, tableLayout: "fixed" }}>
            <thead>
              <TableHead columns={cols.visible} sort={sort} cw={cw} thStyle={th} />
            </thead>
            <tbody>
              {rowsShown.length === 0 &&
                (items.length === 0 && !filtersActive ? (
                  compact ? (
                    <EmptyRow colSpan={cols.visible.length} title="No runs yet" hint="This workflow has not run yet." />
                  ) : (
                    <EmptyRow
                      colSpan={cols.visible.length}
                      title="No workflow runs yet"
                      hint="Workflow run history appears here after the first workflow execution."
                      action={
                        <Link to="/workflows" style={{ textDecoration: "none" }}>
                          <Btn small>Go to Workflows</Btn>
                        </Link>
                      }
                    />
                  )
                ) : (
                  <EmptyRow
                    colSpan={cols.visible.length}
                    title="No matching workflow runs"
                    hint="No run on this page matches the current search or result."
                    action={<Btn small onClick={clearFilters}>Clear filters</Btn>}
                  />
                ))}
              {rowsShown.map((w) => {
                const isExp = expanded === w.traceId;
                const live = w.status === "running" || w.status === "queued";
                return (
                  <Fragment key={w.traceId}>
                    <HoverTr
                      onClick={() => setExpanded(isExp ? null : w.traceId)}
                      tint={isExp ? c.primaryBg : undefined}
                      hoverTint={isExp ? c.primaryBg : c.panelHover}
                      style={{ borderBottom: isExp ? "none" : `1px solid ${c.border}` }}
                    >
                      {renderCells(cols.visible, w, { base: td })}
                    </HoverTr>
                    {isExp && (
                      <tr style={{ borderBottom: `1px solid ${c.border}` }}>
                        <td colSpan={cols.visible.length} style={{ padding: "14px 16px 18px", background: c.primaryBg }}>
                          <RefreshScope>
                          <WorkflowRunDetail traceId={w.traceId} live={live} />
                          </RefreshScope>
                        </td>
                      </tr>
                    )}
                  </Fragment>
                );
              })}
            </tbody>
          </table>
          <Pager pager={pager} page={pager.page} total={pg.totalItems} noun="workflow runs" shown={rowsShown.length} />
        </TableSurface>
      )}
    </div>
  );
}

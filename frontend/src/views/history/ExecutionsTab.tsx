import { Fragment, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { api } from "../../api/client";
import { useGet, useLiveGet, paged, useColumnWidths, useTableSort } from "../../hooks";
import { ColumnsMenu, TableHead, renderCells, useTableColumns } from "../../components/table";
import { useNameDisambiguator } from "../../utils/disambiguate";
import { type SortColumn } from "../../utils/sort";
import { c } from "../../theme";
import { RUN_TYPES } from "../../runtypes";
import {
  EmptyRow,
  ErrorMsg,
  Field,
  FilterSelect,
  Loading,
  Pager,
  SearchInput,
  StatusBadge,
  ExecutorBadge,
  LostBadge,
  StoppedBadge,
  isRunnerLost,
  filterBar,
  fmtDuration,
  fmtTime,
  mono,
  TraceId,
  td,
  th,
  usePager,
} from "./shared";
import { Btn, DetailPanel, EmptyCell, HoverTr, SkeletonRows, TableSurface, TypeBadge, matchesStatus, statusLabel } from "../../components/ui";
import { RunReferences } from "../../components/ReferenceBindings";
import { CalendarFilterSelect } from "../scheduling/CalendarFilterSelect";
import { RunLog } from "./RunLog";
import { RefreshScope } from "../../components/RefreshScope";

interface Run {
  traceId: string;
  jobId?: number;
  // R2-1 — the job's permanent identity, frozen at enqueue; NULL on rows the
  // backfill could not attribute. R2F-3 keys the name badge on it.
  jobUid?: string | null;
  jobName?: string;
  // The run's FROZEN agency snapshot (what it actually ran for), not live
  // membership — so a historical row keeps saying whose run it was.
  agencies?: string[];
  type?: string;
  kind?: string | null;
  scope?: string;
  executor?: string | null;
  // RT — the runner pin frozen on the run (intent), distinct from the runner
  // that claimed it (outcome). Null/absent ⇒ the run was not pinned.
  runnerTag?: string | null;
  status?: string;
  statusReason?: string | null;
  // CAL — the STRUCTURED provenance of a calendar suppression. The ?calendar=
  // filter matches on this column, never on statusReason's wording, so a copy
  // edit to the message can never break the audit query.
  suppressedByCalendar?: string | null;
  manual?: boolean;
  triggeredBy?: string | null;
  scheduleName?: string | null;
  killedBy?: string | null;
  // RX-17 — the durable because-of link: the run whose completion CAUSED this
  // one. Null on every run not produced by a reaction, which is every run that
  // existed before reactions.
  reactedToRunId?: string | null;
  reactionDepth?: number | null;
  triggerKind?: string | null;
  runnerId?: number | null;
  workflowTraceId?: string | null;
  queuedAt?: string | null;
  startedAt?: string | null;
  completedAt?: string | null;
  durationMs?: number | null;
  exitCode?: number | null;
  /** SL-3 — when the S3 archive sweep verified this run's log in the bucket; null until then. */
  logArchivedAt?: string | null;
  overrides?: {
    env?: Record<string, string>;
    hosts?: string[];
    scope?: string;
    executor?: string;
    // Run-input audit (20260724-job-run-update.md). promptWarnings is server-derived —
    // required inputs that were empty in the effective env at trigger time (UDV4/UDV6).
    // promptAnswers/promptAcknowledged are operator-supplied (T2.4) and describe how the
    // operator arrived at the run, which is what makes a third-party-requested run
    // defensible after the fact.
    promptWarnings?: string[];
    promptAnswers?: Record<string, string>;
    promptAcknowledged?: boolean;
    // CA — per-run "connect as" identity (names only: a username and a stored
    // SSH credential LABEL; key material never reaches this envelope).
    sshUser?: string;
    sshCredential?: string;
    // M3/RP — targeting the operator supplied at trigger time.
    groups?: string[];
    ansibleLimit?: string;
    // Phase 3 (RP-15) — advanced ansible options. ansibleCheck is the one that
    // changes what the run MEANS: it was a dry run, and nothing was applied.
    ansibleCheck?: boolean;
    ansibleDiff?: boolean;
    ansibleTags?: string[];
    ansibleSkipTags?: string[];
    ansibleVerbosity?: number;
    ansibleBecome?: boolean;
    ansibleBecomeUser?: string;
    ansibleExtraVars?: Record<string, string>;
  } | null;
}

// RX-17 — the forward because-of link. A reaction-fired run names its cause, and
// the link pivots History to that run rather than describing it: an operator
// following a cascade backwards wants the upstream's own row, with its log.
export function ReactionCauseLink({ runId, depth }: { runId: string; depth: number }) {
  const q = encodeURIComponent(runId);
  return (
    <span style={{ display: "inline-flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
      {/* Two links, because reacted_to_run_id records the upstream's ID but not
          its KIND, and a reaction can be fired by either. A single link into the
          jobs table would dead-end on every workflow-caused run — showing an
          empty table rather than admitting it looked in the wrong place. */}
      <Link
        to={`/runs?trace=${q}`}
        style={{ fontFamily: c.mono, color: c.primary, textDecoration: "none" }}
        title="Open the JOB run whose completion caused this one"
      >
        {runId.slice(0, 8)}
      </Link>
      <Link
        to={`/runs?tab=workflow-runs&trace=${q}`}
        style={{ fontSize: c.fontXs, color: c.primary, textDecoration: "none" }}
        title="If the cause was a workflow run, it is on the Workflow Runs tab"
      >
        (as workflow run)
      </Link>
      {depth > 1 && (
        <span
          style={{ fontSize: c.fontXs, color: c.textMuted }}
          title="How many reaction hops produced this run. A chain is capped at a fixed depth, which is the backstop for cycles no static check can see."
        >
          hop {depth} of the chain
        </span>
      )}
    </span>
  );
}

// RX-17 — the REVERSE direction. A run has no list of its children (that fact is
// owned by each child), so this is a pivot into the filtered list rather than a
// count rendered here: showing "3 runs" would need a per-row query on every page.
//
// TWO links, because a reaction can fire either kind and the two live on
// different History tabs. One link would answer half the graph while looking
// like it answered all of it — a workflow started by this run would simply not
// appear, with nothing saying so.
export function TriggeredRunsLink({ runId }: { runId: string }) {
  const q = encodeURIComponent(runId);
  const link = { color: c.primary, textDecoration: "none" } as const;
  return (
    <span style={{ display: "inline-flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
      <Link to={`/runs?reactedTo=${q}`} style={link} title="Jobs this run set off through a reaction">
        jobs →
      </Link>
      <span style={{ color: c.textMuted }}>·</span>
      <Link
        to={`/runs?tab=workflow-runs&reactedTo=${q}`}
        style={link}
        title="Workflows this run set off through a reaction"
      >
        workflows →
      </Link>
      <span style={{ fontSize: c.fontXs, color: c.textMuted }}>(none unless a reaction watches this run)</span>
    </span>
  );
}

// A run that proceeded with a declared REQUIRED input left empty. Server-derived, so
// this is trustworthy for audit — an operator's acknowledgment never suppresses it.
const unfilledInputs = (r?: Run | null): string[] => r?.overrides?.promptWarnings ?? [];

// RP — whether the run carried any advanced ansible option (Phase 3 envelope
// keys). Named so the overrides section's render condition stays legible.
const hasAnsibleOpts = (ov?: Run["overrides"] | null): boolean =>
  !!ov &&
  (!!ov.ansibleCheck || !!ov.ansibleDiff || (ov.ansibleTags?.length ?? 0) > 0 ||
    (ov.ansibleSkipTags?.length ?? 0) > 0 || !!ov.ansibleVerbosity || !!ov.ansibleBecome ||
    !!ov.ansibleBecomeUser || Object.keys(ov.ansibleExtraVars ?? {}).length > 0);

const TYPES = ["All", ...RUN_TYPES];

// Canonical Result vocabulary (V1.1-4). The dropdown shows these labels and
// `matchesStatus` folds the wire aliases behind each one, so a row can never
// carry a label its own filter excludes (F-3). No "Cancelled" here: cancellation
// is a *workflow-run* display status — the runs.status CHECK has no such value
// (migration 260) — so an option for it on this tab could never match a row.
// The Workflow Runs filter is where it belongs, and F-1 gave it one.
const RESULTS = ["All", ...["success", "warning", "danger", "skipped", "running"].map((s) => statusLabel(s))];

// RX-22 — the "stopped by a human" filter's two-way label/wire mapping. The wire
// values are the ?stopped= booleans the server reads (killedBy IS NOT NULL / IS
// NULL); "" is unfiltered. Kept as one pair of tables so the option list, the
// current selection and the URL round-trip cannot disagree.
const STOPPED_ALL = "All";
const STOPPED_LABELS: Record<string, string> = {
  "": STOPPED_ALL,
  true: "Stopped by a human",
  false: "Not stopped",
};
const STOPPED_WIRE: Record<string, string> = Object.fromEntries(
  Object.entries(STOPPED_LABELS).map(([wire, label]) => [label, wire]),
);

// RX-17 — "how was this run caused?", which is orthogonal to how it ended. The
// wire values are runs.trigger_kind; normalising on read means a hand-edited
// ?triggerKind=nonsense reads as unfiltered rather than silently returning
// nothing while the control claims "All".
const TRIGGER_ALL = "All";
const TRIGGER_LABELS: Record<string, string> = {
  "": TRIGGER_ALL,
  reaction: "Reaction",
  scheduled: "Schedule",
  manual: "Manual",
  workflow: "Workflow step",
  webhook: "Webhook",
};
const TRIGGER_WIRE: Record<string, string> = Object.fromEntries(
  Object.entries(TRIGGER_LABELS).map(([wire, label]) => [label, wire]),
);
const TRIGGER_WIRE_SET = new Set(Object.keys(TRIGGER_LABELS).filter(Boolean));

// Deep-link (?result=…) → canonical Result filter (V1.1-6: the Dashboard
// "Failed (24h)" tile links to /runs?result=fail). statusLabel already folds
// every alias a caller could send (fail/failed → Failed, ok → Success), so this
// is a lookup into RESULTS rather than a third copy of the alias table.
const resultFromParam = (raw: string): string => {
  const label = statusLabel(raw.toLowerCase());
  return RESULTS.includes(label) ? label : "All";
};

// Default column widths (px) so table-layout:fixed has a sensible starting point
// before the user drags (V1.1-7). Stored overrides come from useColumnWidths.
// CO-4 — the Job Name cell carried FOUR inline chips whose style objects were
// byte-identical apart from the tone, repeated in full each time. Lifting the
// spec out of the row made the duplication impossible to miss; one component,
// two tones.
function RunChip({ title, label, tone = "info" }: { title: string; label: string; tone?: "info" | "warning" }) {
  const col = tone === "warning" ? c.warning : c.info;
  return (
    <span
      title={title}
      style={{
        marginLeft: 8,
        fontSize: c.fontXs,
        fontWeight: 600,
        padding: "2px 7px",
        borderRadius: c.radiusChip,
        background: `${col}1a`,
        color: col,
        border: `1px solid ${col}30`,
        whiteSpace: "nowrap",
      }}
    >
      {label}
    </span>
  );
}

const COL_W: Record<string, number> = {
  job: 220,
  trace: 110,
  type: 110,
  executor: 110,
  schedule: 150,
  started: 150,
  completed: 150,
  user: 140,
  duration: 100,
  status: 110,
};

// Sortable columns (TS-5/TS-23, the sorting-update plan). serverSide: the
// keys go to GET /runs as ?sort=&order= and the DATABASE orders the whole
// dataset before pagination — clicking "Duration ▲" now gives the shortest run
// in the filtered set, not on the current page. The server ranks Result by
// severity (worst first ascending, TS-Q5), so `get`/`type` here only describe
// the columns for the header affordance; no client re-sort happens.
const SORT_COLS: SortColumn<Run>[] = [
  { key: "job", get: (r) => r.jobName, type: "text" },
  { key: "type", get: (r) => r.type, type: "text" },
  { key: "executor", get: (r) => r.executor, type: "text" },
  { key: "schedule", get: (r) => r.scheduleName, type: "text" },
  { key: "started", get: (r) => r.startedAt, type: "date" },
  { key: "completed", get: (r) => r.completedAt, type: "date" },
  { key: "user", get: (r) => r.triggeredBy, type: "text" },
  { key: "duration", get: (r) => r.durationMs, type: "number" },
  { key: "status", get: (r) => r.status, type: "text" },
];

export function ExecutionsTab() {
  const [searchParams, setSearchParams] = useSearchParams();
  const [search, setSearchRaw] = useState("");
  const [type, setTypeRaw] = useState("All");
  const [status, setStatusRaw] = useState(() => resultFromParam(searchParams.get("result") ?? ""));
  // CS-2 — `?job=` arrives from the Dashboard's Current Status rows and filters
  // to one job's complete run record. Read once into state, like `result`: the
  // URL seeds the filter, and the chip below owns it from then on.
  //
  // Applied SERVER-side (the query below), not by seeding the search box. The
  // free-text search is within-page refinement, so a job whose runs sit past
  // page 1 would render a confidently wrong "No matching runs" — and the pager
  // would still be counting every run in the system.
  const [jobFilter, setJobFilterRaw] = useState(() => searchParams.get("job") ?? "");
  // R2F-3 — the IDENTITY pivot, alongside the name filter above. Surfaces that
  // know which job they mean (the Dashboard's attention tiles) link by uid, so
  // following one from a department whose job name is shared does not sweep in
  // the other department's runs. Read straight from the URL and never edited
  // here: it is only ever set by following such a link.
  //
  // R2F-Q2: `?job=` stays what it says it is — an honest NAME filter, whose
  // results may span two same-named jobs. The rows carry their own badges.
  const jobUidFilter = searchParams.get("jobUid") ?? "";
  // CAL-29 — "every run suppressed by federal-holidays this fiscal year" in one
  // click. Server-side, like `job` and for the same reason: a client-side pass
  // over one page would confidently miscount the audit answer.
  const [calendarFilter, setCalendarFilterRaw] = useState(() => searchParams.get("calendar") ?? "");
  // RX-22 — "stopped by a human", server-side and orthogonal to Result. It has
  // to be its own control rather than a Result option: a dispositioned stop can
  // carry ANY status, so "stopped" is not a value Result could hold without
  // lying about the run's outcome. The two compose — Result=Success plus this
  // answers "what did we stop and then call fine?".
  // Normalised on read, NOT taken raw: the request below sends stopped=false for
  // any non-empty value that isn't exactly "true", while the control would still
  // render "All" — so a shared or hand-edited ?stopped=1 would silently hide
  // every operator-stopped run behind a filter the UI claims is off. Only the
  // two values the wire defines survive; anything else means unfiltered.
  // RX-17 — "show me everything a cascade produced". Its own control rather
  // than a Result option for the same reason `stopped` is: how a run was CAUSED
  // is orthogonal to how it ended.
  // RX-17 — the reverse because-of pivot. Read straight from the URL each
  // render rather than held in state: it is only ever set by following a link,
  // and mirroring it through state would let the two disagree after a Back.
  const reactedTo = searchParams.get("reactedTo") ?? "";
  // RX-17 — the forward pivot's landing spot: one run, by trace id.
  const traceFocus = searchParams.get("trace") ?? "";
  const [triggerFilter, setTriggerFilterRaw] = useState(() => {
    const raw = searchParams.get("triggerKind");
    return raw && TRIGGER_WIRE_SET.has(raw) ? raw : "";
  });
  const [stoppedFilter, setStoppedFilterRaw] = useState(() => {
    const raw = searchParams.get("stopped");
    return raw === "true" || raw === "false" ? raw : "";
  });
  const [expanded, setExpanded] = useState<string | null>(null);
  const pager = usePager();
  const cw = useColumnWidths("history-executions");

  // TS-23: serverSide — the hook owns only the header state (carets/aria/
  // persistence); the DATABASE does the ordering via ?sort=&order= below, so
  // the rows array passed here is irrelevant and a sort change refetches from
  // page 1. This replaces the old page-scoped client sort.
  const sort = useTableSort<Run>([], SORT_COLS, { key: "started", dir: "desc" }, {
    tableId: "history-executions",
    serverSide: true,
    onChange: () => pager.setPage(0),
  });

  // Server-side paging (PP-H7): fetch only the current page so older history
  // stays reachable past the former 200-row cap. `type` is sent to the server so
  // the total/paging reflect it; Result + free-text search refine the current
  // page only (no server param yet — labeled below).
  const { data, error, loading } = useGet<unknown>(
    () =>
      api.GET("/runs", {
        params: {
          query: {
            page: pager.page + 1,
            pageSize: pager.pageSize,
            ...(type !== "All" ? { type: type as "bash" | "ansible" | "terraform" | "powershell" | "perl" | "python" } : {}),
            // Exact job_name equality server-side, so totalItems and the pager
            // describe the filtered set rather than the whole table.
            ...(jobFilter ? { job: jobFilter } : {}),
            ...(jobUidFilter ? { jobUid: jobUidFilter } : {}),
            // CAL-29 — "*" matches any calendar suppression; a name matches one.
            ...(calendarFilter ? { calendar: calendarFilter } : {}),
            // RX-22 — killedBy IS NOT NULL / IS NULL, server-side so the pager
            // and totals describe the filtered set.
            ...(stoppedFilter ? { stopped: stoppedFilter === "true" } : {}),
            ...(triggerFilter ? { triggerKind: triggerFilter as "manual" | "scheduled" | "workflow" | "webhook" | "reaction" } : {}),
            // RX-17, the reverse link — "what did this run trigger?". Set only
            // by following a because-of link, never by a control: it is a
            // pivot, not a filter an operator composes.
            ...(reactedTo ? { reactedTo } : {}),
            ...(traceFocus ? { trace: traceFocus } : {}),
            // TS-23: the dataset-wide sort. "started desc" matches the endpoint
            // default; sent explicitly so the header state and the wire agree.
            ...(sort.sortKey
              ? {
                  sort: sort.sortKey as "job" | "type" | "executor" | "schedule" | "started" | "completed" | "user" | "duration" | "status",
                  order: sort.sortDir,
                }
              : {}),
          },
        },
      }),
    [pager.page, pager.pageSize, type, jobFilter, jobUidFilter, calendarFilter, stoppedFilter, triggerFilter, reactedTo, traceFocus, sort.sortKey, sort.sortDir],
  );
  const pg = paged<Run>(data);
  const items = pg.items;
  // R2F-3 — two departments may own a job of the same name, so a history row
  // showing the bare name can be either. The badge appears only when THIS page
  // actually contains both, which is also why it cannot leak the existence of a
  // job the viewer's scope filter removed.
  const jobLabel = useNameDisambiguator(items, (r) => ({ uid: r.jobUid, name: r.jobName, agencies: r.agencies }));

  const setSearch = (v: string) => {
    setSearchRaw(v);
    pager.setPage(0);
  };
  const setType = (v: string) => {
    setTypeRaw(v);
    pager.setPage(0);
  };
  const setStatus = (v: string) => {
    setStatusRaw(v);
    pager.setPage(0);
  };
  // Mirrored into the URL so an audit answer is a shareable link, and so the
  // back button undoes it (?calendar= also seeds it, like ?job=).
  const setCalendarFilter = (v: string) => {
    setCalendarFilterRaw(v);
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (v) next.set("calendar", v);
        else next.delete("calendar");
        return next;
      },
      { replace: true },
    );
    pager.setPage(0);
  };
  // RX-22 — mirrored into the URL for the same reason as the calendar filter:
  // "everything a human stopped last quarter" is an audit answer, and an audit
  // answer has to be a shareable link.
  const setTriggerFilter = (v: string) => {
    setTriggerFilterRaw(v);
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (v) next.set("triggerKind", v);
        else next.delete("triggerKind");
        return next;
      },
      { replace: true },
    );
    pager.setPage(0);
  };
  const setStoppedFilter = (v: string) => {
    setStoppedFilterRaw(v);
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (v) next.set("stopped", v);
        else next.delete("stopped");
        return next;
      },
      { replace: true },
    );
    pager.setPage(0);
  };
  // Clearing the job filter drops `job` from the URL too, or a refresh (or the
  // back button) would resurrect a filter the operator just dismissed.
  const clearJobFilter = () => {
    setJobFilterRaw("");
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        next.delete("job");
        return next;
      },
      { replace: true },
    );
    pager.setPage(0);
  };
  // VU-14 — the empty state needs to tell "nothing has ever run" apart from "the
  // filters excluded everything", and offer the filters back in the second case.
  const filtersActive = !!search || type !== "All" || status !== "All" || !!jobFilter || !!calendarFilter || !!stoppedFilter || !!triggerFilter || !!reactedTo || !!traceFocus;
  // ONE setSearchParams call, deliberately. react-router's functional updater is
  // handed the CURRENT render's searchParams, not a pending accumulation
  // (useSearchParams → nextInit(searchParams)), so calling the three individual
  // clearers back-to-back would have each compute from the same base and the
  // last navigate would win — clearing the grid while leaving ?calendar= and
  // ?stopped= in the URL for a refresh or the Back button to resurrect.
  const clearFilters = () => {
    setSearchRaw("");
    setTypeRaw("All");
    setStatusRaw("All");
    setCalendarFilterRaw("");
    setStoppedFilterRaw("");
    setTriggerFilterRaw("");
    setJobFilterRaw("");
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        next.delete("calendar");
        next.delete("stopped");
        next.delete("triggerKind");
        next.delete("reactedTo");
        next.delete("trace");
        next.delete("job");
        return next;
      },
      { replace: true },
    );
    pager.setPage(0);
  };

  // Within-page refinement (Result grouping + free-text search have no server
  // param yet — H7-S1/S2). `type` is already applied server-side.
  const q = search.toLowerCase();
  const filtered = items.filter(
    (r) =>
      (!q || (r.jobName ?? "").toLowerCase().includes(q) || r.traceId.toLowerCase().includes(q)) &&
      matchesStatus(status, r.status),
  );

  // The server returned this page already ordered (TS-23); the Result/search
  // refinement above only removes rows, never reorders them.
  const pageRows = filtered;

  // Header cell: a resizable <th> (V1.1-7) that doubles as a sort toggle when a
  // sort key is given. The resize handle stops propagation so dragging never sorts.
  // CO-4 — the column spec replaces this view's headCell/fixedCell pair. The
  // SORT here is server-side (TS-23): the keys go to GET /runs as ?sort=&order=
  // and the database orders the whole run history before pagination. Column
  // order and visibility below are presentation and stay client-side — never
  // re-sort a server-ordered page here, because the two collations can disagree
  // and rows would shear between pages.
  const cols = useTableColumns<Run>("history-executions", [
    {
      key: "job",
      label: "Job Name",
      sortKey: "job",
      width: COL_W.job,
      pin: "first",
      tdStyle: { fontWeight: 600, overflow: "hidden", textOverflow: "ellipsis" },
      cell: (r) => (
        <>
          {jobLabel(r) || <EmptyCell />}
          {r.workflowTraceId && <RunChip title={`Part of workflow run ${r.workflowTraceId}`} label="workflow" />}
          {r.kind === "ssh-test" && <RunChip title="SSH connection test" label="Test" />}
          {/* RP — a --check run is a REHEARSAL: it records success/failure like
              any other run, so without this badge a dry run is indistinguishable
              from the deploy it rehearsed. Info-tinted, not warning: a check run
              is a deliberate act, not a problem. */}
          {r.overrides?.ansibleCheck && (
            <RunChip title="Check mode — a dry run: ansible reported what would change and applied nothing." label="Check" />
          )}
          {/* T2.3 — spot an affected run without expanding every row. */}
          {unfilledInputs(r).length > 0 && (
            <RunChip
              tone="warning"
              title={`Ran with ${unfilledInputs(r).length} required input${unfilledInputs(r).length === 1 ? "" : "s"} unfilled: ${unfilledInputs(r).join(", ")}`}
              label={`⚠ ${unfilledInputs(r).length} unfilled`}
            />
          )}
        </>
      ),
    },
    { key: "trace", label: "Trace ID", width: COL_W.trace, cell: (r) => <TraceId id={r.traceId} /> },
    {
      key: "type",
      label: "Type",
      sortKey: "type",
      width: COL_W.type,
      fixed: true,
      cell: (r) =>
        r.kind === "ssh-test" ? <span style={{ color: c.textSec, fontSize: c.fontSm }}>Test</span> : r.type ? <TypeBadge type={r.type} /> : <EmptyCell />,
    },
    { key: "executor", label: "Executor", sortKey: "executor", width: COL_W.executor, fixed: true, cell: (r) => <ExecutorBadge executor={r.executor} /> },
    {
      key: "schedule",
      label: "Schedule",
      sortKey: "schedule",
      width: COL_W.schedule,
      tdStyle: { color: c.textSec, fontSize: c.fontSm, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" },
      cell: (r) => r.scheduleName || <EmptyCell />,
    },
    { key: "started", label: "Started", sortKey: "started", width: COL_W.started, fixed: true, tdStyle: { color: c.textSec, fontSize: c.fontSm }, cell: (r) => fmtTime(r.startedAt) },
    { key: "completed", label: "Completed", sortKey: "completed", width: COL_W.completed, fixed: true, tdStyle: { color: c.textSec, fontSize: c.fontSm }, cell: (r) => fmtTime(r.completedAt) },
    {
      key: "user",
      label: "User",
      sortKey: "user",
      width: COL_W.user,
      tdStyle: { color: c.textSec, fontSize: c.fontSm, overflow: "hidden", textOverflow: "ellipsis" },
      cell: (r) => (r.manual ? (r.triggeredBy ?? <EmptyCell />) : "Cronomicon"),
    },
    {
      key: "duration",
      label: "Duration",
      sortKey: "duration",
      width: COL_W.duration,
      fixed: true,
      tdStyle: { ...mono(), fontSize: c.fontSm, textAlign: "right" },
      cell: (r) => fmtDuration(r.durationMs),
    },
    {
      key: "status",
      label: "Result",
      sortKey: "status",
      width: COL_W.status,
      fixed: true,
      cell: (r) => (
        <span style={{ display: "inline-flex", alignItems: "center", gap: 6, flexWrap: "wrap" }}>
          <StatusBadge status={r.status} />
          {isRunnerLost(r.status, r.statusReason) && <LostBadge />}
          {/* RX-23 — a dispositioned stop can carry ANY status, including
              success, so the status badge alone no longer says whether a human
              ended the run. */}
          {r.killedBy && <StoppedBadge by={r.killedBy} />}
        </span>
      ),
    },
  ]);

  return (
    <div>
      <div style={filterBar}>
        <SearchInput value={search} onChange={setSearch} placeholder="Search this page by job or trace ID…" />
        <FilterSelect label="Type" value={type} options={TYPES} onChange={setType} />
        <FilterSelect label="Result" value={status} options={RESULTS} onChange={setStatus} />
        {/* RX-22 — its own control, deliberately not a Result option: a stop can
            be recorded as any status, so "stopped" is orthogonal to outcome
            rather than one of the outcomes. */}
        {/* RX-17 — how the run was CAUSED. Its own control, because a reaction
            can produce a run of any result and "why did this run?" is the
            question the implicit reaction graph otherwise leaves unanswerable. */}
        <FilterSelect
          label="Trigger"
          value={TRIGGER_LABELS[triggerFilter] ?? TRIGGER_ALL}
          options={Object.values(TRIGGER_LABELS)}
          onChange={(v) => setTriggerFilter(TRIGGER_WIRE[v] ?? "")}
        />
        <FilterSelect
          label="Stopped"
          value={STOPPED_LABELS[stoppedFilter] ?? STOPPED_ALL}
          options={Object.values(STOPPED_LABELS)}
          onChange={(v) => setStoppedFilter(STOPPED_WIRE[v] ?? "")}
        />
        <CalendarFilterSelect value={calendarFilter} onChange={setCalendarFilter} />
        {/* RX-17 — the reverse because-of pivot, arrived at by following a link
            rather than set here. Same rule as the job chip: a server-side filter
            the operator did not choose on this page must be visible and
            reversible, or the short table has no stated reason. */}
        {(reactedTo || traceFocus) && (
          <span
            data-testid="reacted-to-chip"
            style={{
              display: "inline-flex",
              alignItems: "center",
              gap: 8,
              padding: "5px 8px 5px 10px",
              borderRadius: c.radiusChip,
              border: `1px solid ${c.border}`,
              background: c.accentBg,
              color: c.text,
              fontSize: c.fontSm,
              whiteSpace: "nowrap",
            }}
          >
            <span style={{ color: c.textSec }}>{reactedTo ? "Triggered by run:" : "Run:"}</span>
            <strong style={{ fontWeight: 600, fontFamily: c.mono }}>{(reactedTo || traceFocus).slice(0, 8)}</strong>
            <button
              onClick={() => {
                const next = new URLSearchParams(searchParams);
                next.delete("reactedTo");
                next.delete("trace");
                setSearchParams(next, { replace: true });
                pager.setPage(0);
              }}
              aria-label="Clear the run pivot"
              title="Show runs from every cause"
              style={{ background: "none", border: "none", color: c.textSec, cursor: "pointer", fontSize: c.fontSm, padding: 0, lineHeight: 1 }}
            >
              ✕
            </button>
          </span>
        )}
        {/* CS-2 — a server-side filter the operator did not set from this page
            must be visible and reversible here. Without the chip, arriving from
            the Dashboard shows a short table with no stated reason. */}
        {jobFilter && (
          <span
            data-testid="job-filter-chip"
            style={{
              display: "inline-flex",
              alignItems: "center",
              gap: 8,
              padding: "5px 8px 5px 10px",
              borderRadius: c.radiusChip,
              border: `1px solid ${c.border}`,
              background: c.accentBg,
              color: c.text,
              fontSize: c.fontSm,
              whiteSpace: "nowrap",
            }}
          >
            <span style={{ color: c.textSec }}>Job:</span>
            <strong style={{ fontWeight: 600 }}>{jobFilter}</strong>
            <button
              onClick={clearJobFilter}
              aria-label={`Clear the ${jobFilter} job filter`}
              title="Show runs from every job"
              style={{
                border: "none",
                background: "transparent",
                color: c.textSec,
                cursor: "pointer",
                fontSize: c.fontSm,
                lineHeight: 1,
                padding: 0,
              }}
            >
              ✕
            </button>
          </span>
        )}
        <ColumnsMenu cols={cols} cw={cw} />
      </div>
      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {error && <ErrorMsg msg={error} />}
      {!loading && !error && (
        <TableSurface>
          <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm, tableLayout: "fixed" }}>
            <thead>
              <TableHead columns={cols.visible} sort={sort} cw={cw} thStyle={th} />
            </thead>
            <tbody>
              {/* VU-14 — a filtered-empty table offers the filter back, not a
                  record to create; a genuinely empty one points at where runs
                  come from. `type` is server-side, so an all-filters-off page
                  with no rows is the only honest "nothing recorded yet". */}
              {pageRows.length === 0 &&
                (items.length === 0 && !filtersActive ? (
                  <EmptyRow
                    colSpan={cols.visible.length}
                    title="No runs recorded yet"
                    hint="Every job run lands here, newest first."
                    action={
                      <Link to="/jobs" style={{ textDecoration: "none" }}>
                        <Btn small>Go to Jobs</Btn>
                      </Link>
                    }
                  />
                ) : (
                  <EmptyRow
                    colSpan={cols.visible.length}
                    title="No matching runs"
                    hint={
                      jobFilter
                        ? `No run matches the current filters. ${jobFilter} may not have run yet.`
                        : "No run on this page matches the current search, type, or result."
                    }
                    action={<Btn small onClick={clearFilters}>Clear filters</Btn>}
                  />
                ))}
              {pageRows.map((r) => (
                <Fragment key={r.traceId}>
                  <HoverTr
                    onClick={() => setExpanded(expanded === r.traceId ? null : r.traceId)}
                    tint={expanded === r.traceId ? c.primaryBg : undefined}
                    hoverTint={expanded === r.traceId ? c.primaryBg : c.panelHover}
                    style={{
                      borderBottom: `1px solid ${c.border}`,
                      opacity: r.status === "skipped" ? 0.55 : 1,
                    }}
                  >
                    {renderCells(cols.visible, r, { base: td })}
                  </HoverTr>
                  {expanded === r.traceId && (
                    <tr style={{ borderBottom: `1px solid ${c.border}` }}>
                      <td colSpan={cols.visible.length} style={{ padding: "16px 14px", background: c.bg }}>
                        <RefreshScope>
                        <RunDetail traceId={r.traceId} />
                        </RefreshScope>
                      </td>
                    </tr>
                  )}
                </Fragment>
              ))}
            </tbody>
          </table>
          <Pager pager={pager} page={pager.page} total={pg.totalItems} noun="runs" shown={pageRows.length} />
        </TableSurface>
      )}
    </div>
  );
}

/** Drill-in: full run record + redacted log, fetched on expand. Polls the run
 * record while it is still queued/running (the useLiveGet idiom) so the header
 * fields settle and RunLog's `active` actually flips off at terminal — with a
 * one-shot useGet, a drawer opened mid-run would tail forever. */
function RunDetail({ traceId }: { traceId: string }) {
  const detail = useLiveGet<Run>(
    () => api.GET("/runs/{traceId}", { params: { path: { traceId } } }),
    [traceId],
    (d) => d?.status === "running" || d?.status === "queued",
  );
  const d = detail.data;

  return (
    // EP-4 Shape A — the drawer's own fetch (and, once EP-8 lands, its log tail).
    <DetailPanel>
      {detail.loading && <Loading />}
      {detail.error && <ErrorMsg msg={detail.error} />}
      {d && (
        <div style={{ display: "grid", gridTemplateColumns: "repeat(4, minmax(0, 1fr))", gap: 12, marginBottom: 14 }}>
          <Field label="Trace ID" isMono>
            {d.traceId}
          </Field>
          {/* E-4: statusLabel, not the wire value. The collapsed row's badge
              says "Failed"; expanding it used to say "danger", and appended a
              raw statusReason like `runner_lost` that appears nowhere else in
              the UI. */}
          <Field label="Result">
            {statusLabel(d.status)}
            {d.statusReason ? ` (${d.statusReason.replace(/_/g, " ")})` : ""}
            {isRunnerLost(d.status, d.statusReason) && (
              <span style={{ marginLeft: 6, display: "inline-flex", verticalAlign: "middle" }}>
                <LostBadge />
              </span>
            )}
            {/* RX-23/RX-5 — killedBy is the provenance half of a disposition:
                the status says what it was recorded as, this says a human did
                it. Naming them here is the detail view's job. */}
            {d.killedBy && (
              <>
                <span style={{ marginLeft: 6, display: "inline-flex", verticalAlign: "middle" }}>
                  <StoppedBadge by={d.killedBy} />
                </span>
                <span style={{ color: c.textMuted }}> stopped by {d.killedBy}</span>
              </>
            )}
          </Field>
          {/* RX-17 — "why did this run?", answered by pointing rather than by
              prose. Both directions live here because a reaction's coupling is
              invisible everywhere else: the forward link is a field on this row,
              the reverse is a query, and an operator mid-incident needs whichever
              one they are standing on. */}
          {d.reactedToRunId && (
            <Field label="Triggered by">
              <ReactionCauseLink runId={d.reactedToRunId} depth={d.reactionDepth ?? 0} />
            </Field>
          )}
          <Field label="Set off">
            <TriggeredRunsLink runId={d.traceId} />
          </Field>
          {d.executor && (
            <Field label="Executor">
              <span style={{ display: "inline-flex" }}>
                <ExecutorBadge executor={d.executor} />
              </span>
            </Field>
          )}
          {/* RT-3 — the pin this run was DISPATCHED with, frozen at trigger time.
              Deliberately its own field beside the runner that actually took it:
              one is intent, the other outcome. A run that waited an hour shows
              WHAT it was waiting for here, which no other field can say. */}
          {d.runnerTag && <Field label="Pinned to runner tag" isMono>{d.runnerTag}</Field>}
          <Field label="Exit Code" isMono>
            {d.exitCode ?? "—"}
          </Field>
          <Field label="Duration" isMono>
            {fmtDuration(d.durationMs)}
          </Field>
          <Field label="Queued at">{fmtTime(d.queuedAt)}</Field>
          <Field label="Started at">{fmtTime(d.startedAt)}</Field>
          <Field label="Completed at">{fmtTime(d.completedAt)}</Field>
          <Field label="Triggered By">{d.manual ? (d.triggeredBy ?? "—") : "Cronomicon (scheduled)"}</Field>
          {d.scheduleName && <Field label="Schedule">{d.scheduleName}</Field>}
          {/* CAL — the structured provenance of the suppression, named so the
              audit answer is legible on the row itself and not only in the filter. */}
          {d.suppressedByCalendar && <Field label="Suppressed by calendar">{d.suppressedByCalendar}</Field>}
          {d.killedBy && <Field label="Killed By">{d.killedBy}</Field>}
          {d.scope && <Field label="Scope">{d.scope}</Field>}
          {/* SL-3 — the log below may be served from the archive tier once the
              local file is reaped; naming the moment it was copied is what lets
              an operator trust a log that outlived its disk. */}
          {d.logArchivedAt && <Field label="Log archived to S3">{fmtTime(d.logArchivedAt)}</Field>}
        </div>
      )}
      {/* T2.2 — run-input audit. Rendered ahead of the override block because "this ran
          without a value someone said was required" is the first thing an auditor needs,
          not a footnote under the env dump. */}
      {unfilledInputs(d).length > 0 && (
        <div style={{ marginBottom: 16 }}>
          <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textSec, textTransform: "uppercase", letterSpacing: 0.7, marginBottom: 8 }}>
            Run inputs
          </div>
          <div style={{ fontSize: c.fontSm, color: c.warning, background: c.warningBg, border: `1px solid ${c.warning}40`, borderRadius: c.radiusSurface, padding: "8px 10px" }}>
            ⚠ Ran with {unfilledInputs(d!).length} required input
            {unfilledInputs(d!).length === 1 ? "" : "s"} unfilled:{" "}
            <code style={{ fontFamily: c.mono }}>{unfilledInputs(d!).join(", ")}</code>
            <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 6 }}>
              {d?.overrides?.promptAcknowledged
                ? "The operator was shown this in the Run dialog and chose to run without it."
                : "No operator acknowledgment was recorded — this may have been triggered by a schedule, a workflow, or the API."}
            </div>
          </div>
          {d?.overrides?.promptAnswers && Object.keys(d.overrides.promptAnswers).length > 0 && (
            <div style={{ fontSize: c.fontSm, color: c.text, marginTop: 8 }}>
              <span style={{ color: c.textSec }}>Supplied: </span>
              <code style={{ fontFamily: c.mono }}>
                {Object.entries(d.overrides.promptAnswers).map(([k, v]) => `${k} (${v})`).join("   ")}
              </code>
            </div>
          )}
        </div>
      )}
      {d?.overrides && ((d.overrides.env && Object.keys(d.overrides.env).length > 0) || (d.overrides.hosts && d.overrides.hosts.length > 0) || (d.overrides.groups && d.overrides.groups.length > 0) || d.overrides.ansibleLimit || d.overrides.sshUser || d.overrides.sshCredential || hasAnsibleOpts(d.overrides)) && (
        <div style={{ marginBottom: 16 }}>
          <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textSec, textTransform: "uppercase", letterSpacing: 0.7, marginBottom: 8 }}>
            Ad-hoc overrides (this run)
          </div>
          {/* RP — check mode leads the block: it changes what the run MEANS (nothing
              was applied), so it cannot be one more line in a list of targeting
              tweaks. Everything else here reports; this one reinterprets. */}
          {d.overrides.ansibleCheck && (
            <div style={{ fontSize: c.fontSm, color: c.text, background: c.infoBg, border: `1px solid ${c.info}40`, borderRadius: c.radiusSurface, padding: "8px 10px", marginBottom: 8 }}>
              <strong style={{ color: c.info }}>Check mode (dry run)</strong> — ansible reported what would change and{" "}
              <strong>applied nothing</strong>. The result below records the rehearsal, not a deployment.
            </div>
          )}
          {(d.overrides.ansibleDiff || (d.overrides.ansibleTags?.length ?? 0) > 0 || (d.overrides.ansibleSkipTags?.length ?? 0) > 0 || !!d.overrides.ansibleVerbosity || d.overrides.ansibleBecome || d.overrides.ansibleBecomeUser) && (
            <div style={{ fontSize: c.fontSm, color: c.text, marginBottom: 6 }}>
              <span style={{ color: c.textSec }}>Ansible: </span>
              {[
                d.overrides.ansibleDiff ? "--diff" : "",
                (d.overrides.ansibleTags?.length ?? 0) > 0 ? `--tags ${d.overrides.ansibleTags!.join(",")}` : "",
                (d.overrides.ansibleSkipTags?.length ?? 0) > 0 ? `--skip-tags ${d.overrides.ansibleSkipTags!.join(",")}` : "",
                d.overrides.ansibleVerbosity ? `-${"v".repeat(Math.min(d.overrides.ansibleVerbosity, 4))}` : "",
                d.overrides.ansibleBecome || d.overrides.ansibleBecomeUser
                  ? `--become${d.overrides.ansibleBecomeUser ? ` (${d.overrides.ansibleBecomeUser})` : ""}`
                  : "",
              ]
                .filter(Boolean)
                .map((part, i) => (
                  <code key={i} style={{ fontFamily: c.mono, marginRight: 8 }}>{part}</code>
                ))}
            </div>
          )}
          {d.overrides.ansibleExtraVars && Object.keys(d.overrides.ansibleExtraVars).length > 0 && (
            <div style={{ fontSize: c.fontSm, color: c.text, marginBottom: 6 }}>
              <span style={{ color: c.textSec }}>Extra-vars: </span>
              <code style={{ fontFamily: c.mono }}>
                {Object.entries(d.overrides.ansibleExtraVars).map(([k, v]) => `${k}=${v}`).join("   ")}
              </code>
            </div>
          )}
          {/* CA — the run connected as an operator-chosen identity rather than each
              host's configured one. Label only for the key — never material. */}
          {(d.overrides.sshUser || d.overrides.sshCredential) && (
            <div style={{ fontSize: c.fontSm, color: c.text, marginBottom: 6 }}>
              <span style={{ color: c.textSec }}>Connected as: </span>
              {d.overrides.sshUser && <code style={{ fontFamily: c.mono }}>{d.overrides.sshUser}</code>}
              {d.overrides.sshUser && d.overrides.sshCredential && " · "}
              {d.overrides.sshCredential && (
                <>
                  key <code style={{ fontFamily: c.mono }}>{d.overrides.sshCredential}</code>
                </>
              )}
            </div>
          )}
          {d.overrides.hosts && d.overrides.hosts.length > 0 && (
            <div style={{ fontSize: c.fontSm, color: c.text, marginBottom: 6 }}>
              <span style={{ color: c.textSec }}>Hosts: </span>
              {d.overrides.hosts.join(", ")}
            </div>
          )}
          {d.overrides.groups && d.overrides.groups.length > 0 && (
            <div style={{ fontSize: c.fontSm, color: c.text, marginBottom: 6 }}>
              <span style={{ color: c.textSec }}>Groups: </span>
              {d.overrides.groups.join(", ")}
            </div>
          )}
          {d.overrides.ansibleLimit && (
            <div style={{ fontSize: c.fontSm, color: c.text, marginBottom: 6 }}>
              <span style={{ color: c.textSec }}>--limit: </span>
              <code style={{ fontFamily: c.mono }}>{d.overrides.ansibleLimit}</code>
            </div>
          )}
          {d.overrides.env && Object.keys(d.overrides.env).length > 0 && (
            <div style={{ fontSize: c.fontSm, color: c.text, marginBottom: 6 }}>
              <span style={{ color: c.textSec }}>Env: </span>
              <code style={{ fontFamily: c.mono }}>
                {Object.entries(d.overrides.env).map(([k, v]) => `${k}=${v}`).join("   ")}
              </code>
            </div>
          )}
          <div style={{ fontSize: c.fontXs, color: c.textSec }}>
            Operator-supplied for this run, stored plaintext (not secrets) — masked in logs only on a best-effort basis.
          </div>
        </div>
      )}
      <div style={{ marginBottom: 16 }}>
        <RunReferences traceId={traceId} />
      </div>
      {/* EP-8b — tail while the run is still going. `d` is the fetched detail,
          so a drawer opened on a queued run starts tailing as soon as it moves. */}
      <RunLog traceId={traceId} active={d?.status === "running" || d?.status === "queued"} />
    </DetailPanel>
  );
}

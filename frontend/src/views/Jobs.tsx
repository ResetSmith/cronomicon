import { Fragment, useEffect, useMemo, useState } from "react";
import { Link, useLocation, useSearchParams } from "react-router-dom";
import { api, csrfHeader, fetchCapabilities } from "../api/client";
import { useGet, rows, paged, useClientPager, useColumnWidths, useInlineAnnotation, useInlineTags, useTableSort, useToast } from "../hooks";
import { AnnotationBanner, AnnotationSection, CriticalChip, annotationOf, type Annotation } from "../components/Annotation";
import { c } from "../theme";
import { DOC_LINKS } from "../components/docLinks";
import { agencySuffix, ambiguousNames } from "../utils/disambiguate";
import type { components } from "../api/schema";
import { RefreshScope } from "../components/RefreshScope";
import { Badge, Btn, ConfirmDialog, DerivedAgencies, Disclosure, EmptyCell, RefreshButton, EnvRowsEditor, ExecutorChoice, Field, FormField, HoverTr, InlineTags, KILL_OUTCOMES, Modal, Pager, Rule, SearchBar, Section, SkeletonRows, SourceBadge, TabBar, TableSurface, TagEditor, TagFilterSelect, Toast, TypeBadge, type KillOutcome, agencySortKey, inputStyle, jobStatusLabel, matchesTags, usePager,
  DocLink,
} from "../components/ui";
import { ColumnsMenu, TableHead, renderCells, useTableColumns, type TableColumn } from "../components/table";
import { type SortColumn } from "../utils/sort";
import { RevisionHistory } from "./RevisionHistory";
import { RunAnalytics } from "../components/RunAnalytics";
import { JobEffectiveReferences, JobKeyField, ReferencePreflightPanel, useRunReferencePreflight } from "../components/ReferenceBindings";
import { RunnerPinInput, RunnerPinValue, useRunnerTags } from "../components/RunnerPin";
import { FolderBrowser } from "../components/FolderBrowser";
import { isIdentityCapable, isRunnerOnly, type Executor } from "../runtypes";
import { RecentRuns, DurationTrend } from "../components/RecentRuns";
import { RunInputsPanel, type InputState, type Provenance } from "./jobs/RunInputs";
import { buildRunSummary, type SummaryAccent, type SummaryRow } from "./jobs/runSummary";
import { fmtDuration, fmtInAppZone } from "../utils/datetime";
import { RunCalendarNote } from "./scheduling/RunCalendarNote";
import { calendarRollup } from "./scheduling/calendars";
import { reactionsWatchingRefusal, useReactionEdges } from "./scheduling/reactions";
import { ReactionPanels } from "./scheduling/ReactionPanels";

type Job = components["schemas"]["Job"];
type Scope = components["schemas"]["Scope"];
type Run = components["schemas"]["Run"];
type JobPrompt = components["schemas"]["JobPrompt"];
type ReferenceBinding = components["schemas"]["ReferenceBinding"];


// G-1/G-4: a tab's label is the canonical word for the state it filters, taken
// from the canonical function rather than retyped. The literals this replaces
// had already drifted — `success` read **Healthy** here and **Success** on
// Workflows, for one status, on two catalogs of the same shape.
const TABS = [
  { key: "all", label: "All" },
  { key: "running", label: jobStatusLabel("running") },
  { key: "success", label: jobStatusLabel("success") },
  { key: "danger", label: jobStatusLabel("danger") },
  { key: "paused", label: jobStatusLabel("paused") },
] as const;

const isScheduled = (j: Job) => !!j.schedule && j.schedule.toLowerCase() !== "manual";

// Cap how many tags render inline in the table so every row is a uniform height
// regardless of tag count (wrapping tags is what made rows grow). Overflow tags
// collapse into a "+N" chip and the full set shows in the row's expanded view.
const MAX_INLINE_TAGS = 2;

// FB1 — a job's folder LOCATION is its source file path (the leading "jobs/"
// stripped) so the browser mirrors the Git tree; its IDENTITY for detail/actions
// stays the DB id (id ?? name). They diverge for cronomicon-authored jobs, which may
// have no source file — fall back to the identity name so the job still appears
// in the tree rather than being silently dropped.
const jobDisplayPath = (j: Job) => {
  // Strip the "jobs/" root and any stray leading/trailing slashes; if nothing is
  // left (e.g. source_path is null or just "jobs/") fall back to the identity.
  const stripped = (j.sourcePath ?? "").replace(/^jobs\//, "").replace(/^\/+|\/+$/g, "");
  // Fall back to the HUMAN name, not jobName() — that resolves to the DB id, and
  // an cronomicon-authored job has no source file, so every such job used to enter
  // the tree labelled with a UUIDv7 (mirrors Workflows/Schedules, which always
  // fell back to the name). Identity still keys off jobName via getName.
  return stripped || j.name || jobName(j);
};
// Stable identity key for the tree (keyed for navigation/detail, not location).
const jobName = (j: Job) => String(j.id ?? j.name);

// Background poll cadence for the jobs list. A run executes asynchronously
// (POST /run enqueues as "queued" and returns 202; the scheduler/executor then
// drives queued→running→terminal), so without a periodic refetch a running job
// never shows up in the Running tab until a manual reload. Polling silently
// (see useGet's intervalMs) keeps the tab counts and rows live.
const JOBS_POLL_MS = 5000;

// Default column widths (px) for the resizable Jobs table (V1.1-7).
// LB14: the sum must match the table's minWidth below; the table renders
// tableLayout:auto inside an overflowX:auto wrapper so it compresses to the card
// at wide widths and scrolls (rather than clipping a column) when narrow.
// Created On / Last Edited were dropped from the table (VC.10) — they were "—" for
// every git-synced job; they now live in the expanded-row detail instead.
//
// CO-2.0 #1/#2: `agency` (added by AF-1) had no entry here, so it rendered at an
// undefined default width and contributed 0 to the hand-maintained minWidth sum —
// which was therefore already stale by a whole column. JOB_MIN_W below is now
// derived from this map instead of hand-copied, so the lockstep the old comment
// asked future editors to maintain by hand is no longer theirs to get wrong.
const JOB_COL_W: Record<string, number> = {
  name: 180,
  type: 100,
  scope: 120,
  agency: 140,
  host: 130,
  schedule: 130,
  tags: 140,
  critical: 90,
  status: 100,
  source: 80,
  lastRun: 150,
};
// CO-2.5: `minWidth` and every colSpan/colCount now come from the LIVE visible
// column set (`cols.minWidth` / `cols.visible.length`), not from this map — so
// hiding a column frees its width instead of leaving it reserved. CO-2.0 #3's
// bug was three hand-typed answers to "how many columns are there"; there is
// now one, and it is computed.
//
// The map survives as the source of each column's DEFAULT width, which the spec
// reads and a stored `colw:` override outranks.
export type JobRow = { job: Job; displayName: string };

// Sortable columns (TS-8, the sorting-update plan) — active in flat/search
// mode only; browse mode keeps the folder tree's alpha order. Status sorts by
// job-status rank, worst first ascending (TS-Q5). Tags are excluded (multi-value).
// CO-1.1 — a FACTORY, not a module const, because the `critical` column must sort
// on the value the row is DISPLAYING. That value can be an optimistic override
// from an in-panel edit, which lives in the component's `inlineAnnotation` hook;
// a module-level const cannot see it and would sort the server's stale value
// while the chip shows the operator's — a one-render disagreement indistinguish-
// able from a sort bug. The other columns read straight off the row and are
// unaffected. useTableSort re-creates its column array each render by design, so
// building this in-render costs nothing.
const JOB_STATUS_RANK: Record<string, number> = { danger: 0, paused: 1, queued: 2, running: 3, success: 4 };
const jobSortCols = (resolveCritical: (j: Job) => boolean): SortColumn<Job>[] => [
  { key: "name", get: (j) => j.name, type: "text" },
  { key: "type", get: (j) => j.type, type: "text" },
  { key: "scope", get: (j) => j.scope, type: "text" },
  { key: "host", get: (j) => j.host, type: "text" },
  { key: "schedule", get: (j) => j.schedule, type: "text" },
  // CO-Q6 — critical-first ascending, matching the TS-Q5 "worst first ascending"
  // convention the status rank already uses. Sorted as a number rather than a
  // boolean so it shares the existing comparator.
  { key: "critical", get: (j) => (resolveCritical(j) ? 0 : 1), type: "number", defaultDir: "asc" },
  { key: "status", get: (j) => j.status, type: "rank", rank: JOB_STATUS_RANK },
  { key: "source", get: (j) => j.source, type: "text" },
  { key: "lastRun", get: (j) => j.lastRunAt, type: "date" },
  // AF-1 — a documented exception to the TS-Q5 "no multi-value columns" rule:
  // a job carries zero or one agency in practice, and grouping a department's
  // jobs together is the column's whole purpose. agencySortKey ranks a global
  // job under "All" rather than lumping it with the unmapped blanks.
  { key: "agency", get: (j) => agencySortKey(j.agencies, j.scope ?? ""), type: "text" },
];

export function Jobs() {
  const [bump, setBump] = useState(0);
  // Fetch the catalog at the server max page size (200) rather than the default
  // 50 (which silently truncated the list — anything past the first 50 jobs by
  // name was unreachable). Status tabs, type/tag/search, and the folder tree all
  // operate over this resident set; the result is then paged client-side below
  // (PP). Jobs is a small sync-derived catalog, so 200 is effectively the whole
  // catalog; a "first N" banner appears if it is ever exceeded.
  const { data, error, loading } = useGet<unknown>(
    () => api.GET("/jobs", { params: { query: { page: 1, pageSize: 200 } } }),
    [bump],
    JOBS_POLL_MS,
  );
  const scopesQ = useGet<unknown>(() => api.GET("/scopes"));
  const pg = paged<Job>(data);
  const items = pg.items;
  // R2F-3 — which names on THIS page belong to more than one job. The Agency
  // column (AF-1) already tells the rows apart; this is for the surfaces that
  // show a name alone, like the Run dialog's title.
  const ambiguousJobNames = useMemo(
    () => ambiguousNames(items.map((j) => ({ uid: j.uid, name: j.name, agencies: j.agencies }))),
    [items],
  );
  const cw = useColumnWidths("jobs");
  const location = useLocation();

  // Current folder lives in the URL (?path=) so back/forward and deep links work.
  const [params, setParams] = useSearchParams();
  const path = params.get("path") ?? "";
  const setPath = (p: string) =>
    // Functional updater reads the live params at apply time (no stale snapshot).
    setParams((prev) => {
      const next = new URLSearchParams(prev);
      if (p) next.set("path", p);
      else next.delete("path");
      return next;
    });

  // Compose capability gates the cronomicon-only Edit affordance (D6); publish gates
  // the "+ Publish to GitLab" button on the PublishSchedule permission (PP-B1).
  // triggerJobs/killJobs gate Run/Kill/Pause/Resume (RB-3).
  const [canCompose, setCanCompose] = useState(false);
  const [canPublish, setCanPublish] = useState(false);
  // capsCanRun is the FLAT union — "may trigger somewhere" — and is only a
  // fallback (RB-3). Per-row truth is j.canRun, computed server-side against that
  // row's scope (RB-24); consuming it is what keeps the button from drifting away
  // from the answer the API will actually give.
  const [capsCanRun, setCapsCanRun] = useState(false);
  useEffect(() => {
    fetchCapabilities().then((caps) => {
      setCanCompose(caps.compose);
      setCanPublish(caps.publishSchedule);
      setCapsCanRun(caps.triggerJobs);
    });
  }, []);

  // Per-row authority (RB-24). The server computes canRun/canKill against each
  // row's scope, so a restricted operator sees Run on their department's jobs and
  // not on anyone else's. Fall back to the flat capability only when a row predates
  // the field (an older server, or a cached list) — never widen past it, so an
  // absent field can hide a button but can never conjure one.
  const rowCanRun = (j: Job) => (j.canRun ?? capsCanRun);
  const rowCanKill = (j: Job) => (j.canKill ?? capsCanRun);

  // Run/Kill/Pause/Resume visibility (RB-3). This replaced a client-side role list
  // (auth.canTriggerJobs) that hardcoded admin|approver|operator: identical today,
  // but the server flag is computed from the actual permission matrix, so a custom
  // role carrying triggerJobs keeps its buttons once roles become data (RB-6/RB-7).
  //
  // Still a UX affordance, NOT a security boundary: the run path enforces scope but
  // not the verb until RB-2 (v0.56.1). The flag is also a flat union — "may trigger
  // somewhere" — so it cannot answer per-row truth; that is RB-24 (canRun/canKill
  // on each row), which is what will make this honest at departmental granularity.
  //
  // Defaults false and flips true once /capabilities resolves, so the buttons fade
  // in rather than flashing and disappearing for a viewer.

  // CO-2.3 — headCell/fixedCell are GONE. Both were per-view copies of the same
  // two closures (Workflows had its own drifted pair, and fourteen more views
  // are queued to adopt); <TableHead> is the one renderer, choosing ResizableTh
  // or a plain <th> from each column's `fixed` flag.
  const scopes = rows<Scope>(scopesQ.data);

  const [search, setSearch] = useState("");
  const [type, setType] = useState("All");
  const [tagFilter, setTagFilter] = useState<string[]>([]);
  const [tagMatch, setTagMatch] = useState<"any" | "all">("any");
  // Optimistic per-job tag edits (keyed by DB id) so a detail-pane edit repaints
  // the table column immediately without waiting for the next /jobs poll. Reset on
  // bump (a manual refresh / git pull), after which the fresh fetch is authoritative.
  const inlineTags = useInlineTags<Job>(
    bump,
    (j) => String(j.id),
    (j) => j.tags,
    (j, next) => api.PUT("/job-tags/{jobId}", { params: { path: { jobId: j.id! }, header: csrfHeader }, body: { tags: next } }),
  );
  // AN-3 — the same optimistic plumbing for the operator annotation, so saving a
  // note in the expanded panel repaints that row's Critical chip at once.
  const inlineAnnotation = useInlineAnnotation<Job, Annotation>(
    bump,
    (j) => String(j.id),
    (j, next) =>
      api.PUT("/job-annotation/{jobId}", {
        params: { path: { jobId: j.id! }, header: csrfHeader },
        body: { critical: !!next.critical, contact: next.contact ?? "", notes: next.notes ?? "" },
      }),
  );
  const [tab, setTab] = useState<(typeof TABS)[number]["key"]>("all");
  const [expanded, setExpanded] = useState<number | null>(null);
  const [busyId, setBusyId] = useState<number | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [toast, fireToast] = useToast();
  const [runFor, setRunFor] = useState<Job | null>(null);
  const [killFor, setKillFor] = useState<Job | null>(null);
  const [deleting, setDeleting] = useState<Job | null>(null);
  // RH: which cronomicon-source job's revision history is open, by name.
  const [historyFor, setHistoryFor] = useState<string | null>(null);
  const [delBusy, setDelBusy] = useState(false);
  // RX-24 — the server's refusal text when reactions watch the job being
  // deleted. Non-null turns the confirm dialog into the forced one; it is state
  // rather than a boolean so the dialog can NAME the reactions that blocked it,
  // which is the whole reason the refusal lists them.
  const [delBlock, setDelBlock] = useState<string | null>(null);

  const types = useMemo(
    () => ["All", ...Array.from(new Set(items.map((j) => j.type).filter((t): t is NonNullable<Job["type"]> => !!t))).sort()],
    [items],
  );

  const counts = useMemo(() => {
    const m: Record<string, number> = { all: items.length, running: 0, success: 0, danger: 0, paused: 0 };
    for (const j of items) if (j.status && j.status in m) m[j.status]++;
    return m;
  }, [items]);

  const filtered = items.filter((j) => {
    if (tab !== "all" && j.status !== tab) return false;
    if (type !== "All" && j.type !== type) return false;
    if (!matchesTags(inlineTags.tagsFor(j), tagFilter, tagMatch)) return false;
    if (!search) return true;
    const q = search.toLowerCase();
    return (j.name ?? "").toLowerCase().includes(q) || (j.scope ?? "").toLowerCase().includes(q) || (j.host ?? "").toLowerCase().includes(q) || (j.agencies ?? []).some((a) => a.toLowerCase().includes(q));
  });

  // Column sort (TS-8, TS-24) — feeds BOTH render modes: the flat/search table
  // orders its rows directly, and browse mode feeds the same sorted array into
  // FolderBrowser with preserveLeafOrder so each folder's leaves follow suit.
  // The spec is built here rather than at module scope so Critical sorts on the
  // same (possibly overridden) value the row renders — see jobSortCols.
  const sort = useTableSort(
    filtered,
    jobSortCols((j) => !!inlineAnnotation.valueFor(j, annotationOf(j)).critical),
    { key: "name", dir: "asc" },
    { tableId: "jobs" },
  );

  // Client-side pagination over the already-filtered set (flat/search mode). The
  // page index re-clamps as filters narrow, so a tab/filter change never strands
  // the user on an empty trailing page; the filter handlers also reset to page 0.
  const { pageItems, total, page, pager } = useClientPager(sort.sorted);

  // VU-14 — the filtered-empty table offers the filters back rather than a record
  // to create. The status tab counts as a filter: "Failed (0)" is the most common
  // way to land on an empty Jobs table with a full catalog behind it.
  const clearFilters = () => {
    setTab("all");
    setSearch("");
    setType("All");
    setTagFilter([]);
    pager.setPage(0);
  };


  // A handoff toast carried via navigation state (e.g. a job deleted from the
  // JobComposer edit view, D4). Show it once, then clear the history state so a
  // refresh doesn't replay it.
  useEffect(() => {
    const handoff = (location.state as { toast?: string } | null)?.toast;
    if (handoff) {
      fireToast(handoff);
      window.history.replaceState({}, "");
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function act(
    job: Job,
    verb: "run" | "pause" | "resume" | "kill",
    runOpts?: {
      scope?: string;
      executor?: Executor;
      env?: Record<string, string>;
      targetHosts?: string[];
      targetGroups?: string[];
      ansibleLimit?: string;
      // T2.4 — run-input audit metadata; descriptive only, never changes the run.
      promptAnswers?: Record<string, string>;
      promptAcknowledged?: boolean;
      // RV — dialog sections the operator had open before running (audit-only).
      reviewedSections?: string[];
      // V2-11 — per-run stored-reference additions (names only).
      references?: { kind: "secret" | "var" | "key"; name: string }[];
      // CA — per-run "connect as" identity (username + stored-credential LABEL).
      sshUser?: string;
      sshCredential?: string;
      // Phase 3 (RP-19) — advanced ansible options, grouped like `identity` rather
      // than added as eight more positional params.
      ansibleCheck?: boolean;
      ansibleDiff?: boolean;
      ansibleTags?: string[];
      ansibleSkipTags?: string[];
      ansibleVerbosity?: number;
      ansibleBecome?: boolean;
      ansibleBecomeUser?: string;
      ansibleExtraVars?: Record<string, string>;
      // AR — "When to run": defer the run to an ISO instant (empty = now).
      runAt?: string;
      // RT-3 — per-run runner pin. Tri-state, and the middle state is the point:
      // undefined inherits the job's effective pin, "" runs THIS run unpinned
      // even though the job is pinned, and a tag pins it. Not folded into the
      // truthiness guards below for that reason — see the assignment.
      runnerTag?: string;
    },
    // RX-4 — what a stop MEANT. Only read for verb "kill"; undefined sends no
    // body, which the server records as the unclassified `killed`.
    killOutcome?: KillOutcome,
  ): Promise<{ ok: boolean; code?: string; message?: string }> {
    if (job.id == null) return { ok: false };
    setBusyId(job.id);
    setActionError(null);
    const path = { jobId: job.id };
    const runBody: {
      scope?: string;
      executor?: Executor;
      env?: Record<string, string>;
      targetHosts?: string[];
      targetGroups?: string[];
      ansibleLimit?: string;
      promptAnswers?: Record<string, string>;
      promptAcknowledged?: boolean;
      reviewedSections?: string[];
      references?: { kind: "secret" | "var" | "key"; name: string }[];
      sshUser?: string;
      sshCredential?: string;
      ansibleCheck?: boolean;
      ansibleDiff?: boolean;
      ansibleTags?: string[];
      ansibleSkipTags?: string[];
      ansibleVerbosity?: number;
      ansibleBecome?: boolean;
      ansibleBecomeUser?: string;
      ansibleExtraVars?: Record<string, string>;
      runAt?: string;
      runnerTag?: string;
    } = {};
    if (runOpts?.scope) runBody.scope = runOpts.scope;
    // RT-3 — `!== undefined`, never truthiness. "" is a decision (run unpinned),
    // not an absent value, and a `if (runOpts?.runnerTag)` here would silently
    // drop the break-glass case and let the job's pin apply after all.
    if (runOpts?.runnerTag !== undefined) runBody.runnerTag = runOpts.runnerTag;
    if (runOpts?.executor) runBody.executor = runOpts.executor;
    if (runOpts?.env && Object.keys(runOpts.env).length > 0) runBody.env = runOpts.env;
    if (runOpts?.targetHosts && runOpts.targetHosts.length > 0) runBody.targetHosts = runOpts.targetHosts;
    if (runOpts?.targetGroups && runOpts.targetGroups.length > 0) runBody.targetGroups = runOpts.targetGroups;
    if (runOpts?.ansibleLimit && runOpts.ansibleLimit.trim() !== "") runBody.ansibleLimit = runOpts.ansibleLimit;
    if (runOpts?.promptAnswers && Object.keys(runOpts.promptAnswers).length > 0) runBody.promptAnswers = runOpts.promptAnswers;
    if (runOpts?.promptAcknowledged) runBody.promptAcknowledged = true;
    if (runOpts?.reviewedSections && runOpts.reviewedSections.length > 0) runBody.reviewedSections = runOpts.reviewedSections;
    if (runOpts?.references && runOpts.references.length > 0) runBody.references = runOpts.references;
    if (runOpts?.sshUser) runBody.sshUser = runOpts.sshUser;
    if (runOpts?.sshCredential) runBody.sshCredential = runOpts.sshCredential;
    if (runOpts?.ansibleCheck) runBody.ansibleCheck = true;
    if (runOpts?.ansibleDiff) runBody.ansibleDiff = true;
    if (runOpts?.ansibleTags && runOpts.ansibleTags.length > 0) runBody.ansibleTags = runOpts.ansibleTags;
    if (runOpts?.ansibleSkipTags && runOpts.ansibleSkipTags.length > 0) runBody.ansibleSkipTags = runOpts.ansibleSkipTags;
    if (runOpts?.ansibleVerbosity) runBody.ansibleVerbosity = runOpts.ansibleVerbosity;
    if (runOpts?.ansibleBecome) runBody.ansibleBecome = true;
    if (runOpts?.ansibleBecomeUser) runBody.ansibleBecomeUser = runOpts.ansibleBecomeUser;
    if (runOpts?.ansibleExtraVars && Object.keys(runOpts.ansibleExtraVars).length > 0)
      runBody.ansibleExtraVars = runOpts.ansibleExtraVars;
    if (runOpts?.runAt) runBody.runAt = runOpts.runAt;
    const opts =
      verb === "run"
        ? { params: { path, header: csrfHeader }, body: runBody }
        : verb === "kill" && killOutcome
          ? { params: { path, header: csrfHeader }, body: { outcome: killOutcome } }
          : { params: { path, header: csrfHeader } };
    const { error: err } =
      verb === "run"
        ? await api.POST("/jobs/{jobId}/run", opts as never)
        : verb === "pause"
          ? await api.POST("/jobs/{jobId}/pause", opts as never)
          : verb === "resume"
            ? await api.POST("/jobs/{jobId}/resume", opts as never)
            : await api.POST("/jobs/{jobId}/kill", opts as never);
    setBusyId(null);
    if (err) {
      const e = err as { code?: string; message?: string };
      setActionError(`${verb} failed for ${job.name}: ${e?.message ?? "request failed"}`);
      return { ok: false, code: e?.code, message: e?.message };
    }
    const msg: Record<typeof verb, string> = {
      run: `Run queued: ${job.name}`,
      pause: `Schedule paused: ${job.name}`,
      resume: `Schedule resumed: ${job.name}`,
      kill: `Stop requested: ${job.name}${killOutcome && killOutcome !== "killed" ? ` (recorded as ${KILL_OUTCOMES.find((o) => o.value === killOutcome)?.label.toLowerCase()})` : ""}`,
    };
    fireToast(msg[verb]);
    setBump((n) => n + 1);
    return { ok: true };
  }

  // Delete an cronomicon-authored job from the expanded row. The same endpoint the
  // JobComposer edit view uses; the server rejects git-source rows with a 409,
  // which is the real guard — the button gate below is only UX. Collapses the
  // row on success so the list doesn't re-expand onto a deleted id.
  //
  // RX-24 — a second 409 lives on this route: reactions watch this job. It is
  // told apart by its error CODE, not by the status, and it keeps the dialog
  // open with a "delete anyway" instead of dismissing it, because the operator
  // has a real second choice here that they do not have for a git-source row.
  async function doDelete(job: Job, force = false) {
    if (job.id == null) return;
    setDelBusy(true);
    setActionError(null);
    const { response, error: err } = await api.DELETE("/jobs/{jobId}", {
      params: { path: { jobId: job.id }, query: force ? { force: true } : {} },
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
          ? "Only cronomicon-source jobs can be deleted in-app."
          : (err as { message?: string } | undefined)?.message ?? `Delete failed (${response.status}).`;
      setActionError(`Delete failed for ${job.name}: ${msg}`);
      return;
    }
    setDeleting(null);
    setDelBlock(null);
    setExpanded(null);
    fireToast(`Job "${job.name}" deleted.`);
    setBump((n) => n + 1);
  }

  // CO-2.5 — the column SPEC. Each column ties its stable key to the cell that
  // renders it, which is what makes the set permutable, hideable and countable;
  // the header and the row body used to be two hand-written sequences matched
  // only by position, with the count typed out a third time in every colSpan.
  //
  // Built during render, never hoisted: the cells close over this component's
  // state (`expanded`, the inlineTags/inlineAnnotation resolvers) AND the styles
  // read `c.*`, which applyTheme reassigns — a module-level spec would freeze the
  // load-time palette and silently break the Light/Dark toggle.
  //
  // The spec's row is {job, displayName}, not a bare Job: `displayName` differs
  // per RENDER MODE (full path in search, filename in the browser) and is handed
  // in per row by FolderBrowser, so it cannot be closed over at spec-build time.
  // Carrying it on the row keeps the columns pure functions of what they render —
  // the alternative, a mutable ref written just before each renderCells call,
  // works only because React happens to render synchronously.
  const jobColumnSpec = (): TableColumn<JobRow>[] => [
    // CO-Q4 — the column declared FIRST is pinned there and cannot be hidden.
    // The rule is positional rather than by-name (six of the queued adopters open
    // with a 44px expand control, not a name), but the reason is the same: a
    // catalog whose leading column can be moved or hidden stops being scannable,
    // and there is no way back to a row you can no longer identify.
    // VU2-4 — the name is the row's identity and must not wrap: under
    // tableLayout:auto (LB14) a wrappable cell yields its width to the
    // nowrap columns beside it, so realistic names broke across two lines
    // while Host and Critical held empty space. `nowrap` makes the name
    // claim its content width like Schedule and Last Run already do;
    // ellipsis caps a pathological name so it widens the row instead of
    // forcing the whole table into horizontal scroll. Raising the width
    // DEFAULT does not do this — under auto layout that number is a hint
    // content overrides, and it only inflates the table's minWidth.
    { key: "name", label: "Name", sortKey: "name", width: JOB_COL_W.name, pin: "first", tdStyle: { fontWeight: 600, whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis", maxWidth: 260 }, cell: ({ job: j, displayName }) => j.name ?? displayName },
    { key: "type", label: "Type", sortKey: "type", width: JOB_COL_W.type, fixed: true, cell: ({ job: j }) => (j.type ? <TypeBadge type={j.type} /> : <EmptyCell />) },
    { key: "scope", label: "Scope", sortKey: "scope", width: JOB_COL_W.scope, cell: ({ job: j }) => j.scope ?? <EmptyCell /> },
    {
      key: "agency",
      label: "Agency",
      sortKey: "agency",
      width: JOB_COL_W.agency,
      // RB-23 — DERIVED from the scope, never stored on the job. Once access is
      // departmental, "show me my department's jobs" is the first thing anyone
      // asks, and this catalog did not mention agencies at all. A scope can
      // belong to more than one agency, hence a list.
      cell: ({ job: j }) => (
        <DerivedAgencies agencies={j.agencies ?? []} scope={j.scope ?? ""} derivedFrom={`Derived from scope ${j.scope ?? ""}`} />
      ),
    },
    { key: "host", label: "Host", sortKey: "host", width: JOB_COL_W.host, cell: ({ job: j }) => j.host ?? <EmptyCell /> },
    {
      key: "schedule",
      label: "Schedule",
      sortKey: "schedule",
      width: JOB_COL_W.schedule,
      tdStyle: { fontFamily: c.mono, color: c.textSec, whiteSpace: "nowrap" },
      cell: ({ job: j }) => (
        <>
          {j.status === "paused" ? `⏸ ${jobStatusLabel("paused")}` : j.schedule ?? "manual"}
          {(j.scheduleCount ?? 0) > 1 && (
            <span
              title={`${j.scheduleCount} schedule entries`}
              style={{
                marginLeft: 6,
                fontSize: c.fontXs,
                fontWeight: 600,
                padding: "1px 6px",
                borderRadius: c.radiusChip,
                background: `${c.info}1a`,
                color: c.info,
                border: `1px solid ${c.info}30`,
                fontFamily: c.sans,
              }}
            >
              +{j.scheduleCount! - 1}
            </span>
          )}
        </>
      ),
    },
    {
      key: "tags",
      label: "Tags",
      width: JOB_COL_W.tags,
      tdStyle: { whiteSpace: "nowrap" },
      // Override-aware: an optimistic detail edit shows in the column at once.
      cell: ({ job: j }) => <InlineTags tags={inlineTags.tagsFor(j)} max={MAX_INLINE_TAGS} />,
    },
    {
      key: "critical",
      label: "Critical",
      sortKey: "critical",
      width: JOB_COL_W.critical,
      fixed: true,
      // AN-3 put the Critical chip INSIDE the status cell: both answer "how much
      // should I care about this row", and separating them put the louder signal
      // further from the eye. CO-Q7 keeps that adjacency argument — it is about
      // proximity, not about sharing a cell — and gives Critical its own column
      // immediately before Status. Deliberately NOT duplicated into both: one
      // fact in two places drifts the first time somebody edits one of them.
      cell: ({ job: j }) => (inlineAnnotation.valueFor(j, annotationOf(j)).critical ? <CriticalChip /> : <EmptyCell />),
    },
    {
      key: "status",
      label: "Status",
      sortKey: "status",
      width: JOB_COL_W.status,
      fixed: true,
      cell: ({ job: j }) => (j.status ? <Badge status={j.status} label={jobStatusLabel(j.status)} /> : <EmptyCell />),
    },
    { key: "source", label: "Source", sortKey: "source", width: JOB_COL_W.source, fixed: true, cell: ({ job: j }) => <SourceBadge source={j.source} /> },
    {
      key: "lastRun",
      label: "Last Run",
      sortKey: "lastRun",
      width: JOB_COL_W.lastRun,
      tdStyle: { color: c.textSec, whiteSpace: "nowrap" },
      cell: ({ job: j }) => (j.lastRunAt ? fmtDateTime(j.lastRunAt) : <EmptyCell />),
    },
  ];

  // CO-2.5 — the order/visibility preference for this table, merged over the
  // spec above. `cols.visible` is the single answer to "which columns, in what
  // order": the header maps it, the row body maps it, and every colSpan counts
  // it. The three used to be maintained by hand and had already drifted apart.
  const cols = useTableColumns<JobRow>("jobs", jobColumnSpec());

  // One job row + its expand-to-detail, reused by the flat search results and the
  // folder browser. The NAME column shows the human job name (metadata.name) in
  // both modes; displayName (full path in search, filename in the browser) is kept
  // only as a fallback for the rare job with no name. Folder location lives in the
  // tree path, not the name. The identity/expand key stays the DB id.
  const renderJobRow = (j: Job, displayName: string) => {
    const isExp = expanded === j.id;
    const busy = busyId === j.id;
    const running = j.status === "running";
    // Override-aware tag set: an optimistic detail edit shows in the column at once.
    const tags = inlineTags.tagsFor(j);
    return (
      <Fragment key={String(j.id ?? j.name)}>
        <HoverTr
          onClick={() => setExpanded(isExp ? null : (j.id ?? null))}
          tint={isExp ? c.primaryBg : running ? `${c.success}12` : undefined}
          hoverTint={isExp ? c.primaryBg : running ? `${c.success}20` : c.panelHover}
        >
          {/* An expanded row drops every cell's bottom border so the detail panel
              reads as part of the row rather than a separate band — one per-row
              override rather than the same ternary smeared across ten specs. */}
          {renderCells(cols.visible, { job: j, displayName }, { rowStyle: isExp ? { borderBottom: "none" } : undefined })}
        </HoverTr>
        {isExp && (
          <tr>
            <td colSpan={cols.visible.length} style={{ padding: "14px 18px", background: c.primaryBg, borderBottom: `1px solid ${c.border}` }}>
              {/* Row actions appear only once a job is expanded (V1.1), freeing
                  the table row for the Last Run column. EV-4: they are handed to
                  JobDetail, which renders them top-right of its header strip —
                  one action home, matching Runners — instead of in a lone strip
                  stacked above the detail. */}
              <RefreshScope>
              <JobDetail
                jobId={j.id}
                fallback={j}
                tags={tags}
                onSaveTags={(next) => inlineTags.save(j, next)}
                tagErr={inlineTags.errors[String(j.id)]}
                // AN-3 — the panel resolves its own base (the DETAIL row, which
                // alone carries the notes) and this closure decides whether a
                // pending override outranks it. Passing a resolved value instead
                // would hand the panel the LIST row's annotation, which has no
                // notes, and the section would render blank until a refetch.
                resolveAnnotation={(base) => inlineAnnotation.valueFor(j, base)}
                onSaveAnnotation={(next) => inlineAnnotation.save(j, next)}
                annotationErr={inlineAnnotation.errors[String(j.id)]}
                canEdit={canCompose && j.source === "cronomicon"}
                actions={(rowCanRun(j) || (canCompose && j.source === "cronomicon")) ? (
                <>
                  {/* Run stays available while a run is active — overlapping runs are
                      legal under the Allow policy (the default), and Forbid/Queue jobs
                      get the honest backend answer (409 / parked) instead of a hidden
                      button. Stop appears only while an instance is actually active. */}
                  {rowCanRun(j) && <Btn small primary disabled={busy} onClick={() => setRunFor(j)}>▶ Run</Btn>}
                  {(j.status === "running" || j.status === "queued") && rowCanKill(j) &&
                    <Btn small danger disabled={busy} onClick={() => setKillFor(j)}>Stop run…</Btn>}
                  {/* FX-2 — Resume is gated on the job BEING paused, never on it being
                      scheduled. Pause state is a row in paused_jobs and nothing about
                      removing a schedule clears it, so a job paused before its schedule
                      was dropped used to lose its only way back: the whole ternary was
                      behind isScheduled, the Resume arm vanished with the schedule, and
                      the state was unreachable from the UI. `isScheduled` is a fair
                      precondition for Pause (pausing a manual-only job suppresses
                      nothing) — it was only ever wrong for Resume. */}
                  {rowCanKill(j) &&
                    (j.status === "paused" ? (
                      <Btn small style={{ minWidth: 72 }} disabled={busy} onClick={() => act(j, "resume")}>Resume</Btn>
                    ) : (
                      isScheduled(j) && (
                        <Btn small style={{ minWidth: 72 }} disabled={busy} onClick={() => act(j, "pause")}>Pause</Btn>
                      )
                    ))}
                  {/* Edit + Delete only for cronomicon rows the caller may compose (D6
                      hides them for git rows + non-admins). Delete was previously
                      editor-only (D4); it is now also a row action here, behind the
                      same confirm. */}
                  {canCompose && j.source === "cronomicon" && (
                    <Link to={`/compose?id=${j.id}`} style={{ textDecoration: "none" }}>
                      <Btn small>Edit</Btn>
                    </Link>
                  )}
                  {/* Clone: the composer prefilled from this job, in create mode —
                      for "same settings, different purpose" (same gate as Edit:
                      the result is an cronomicon-source job). */}
                  {canCompose && j.source === "cronomicon" && (
                    <Link to={`/compose?cloneFrom=${j.id}`} style={{ textDecoration: "none" }}>
                      <Btn small>Clone</Btn>
                    </Link>
                  )}
                  {/* RH: in-app definitions get history here; git rows get it from Git. */}
                  {canCompose && j.source === "cronomicon" && (
                    <Btn small onClick={() => setHistoryFor(j.name ?? "")}>
                      History
                    </Btn>
                  )}
                  {canCompose && j.source === "cronomicon" && (
                    <Btn small dangerQuiet disabled={busy || delBusy} onClick={() => setDeleting(j)}>
                      Delete
                    </Btn>
                  )}
                </>
                ) : undefined}
              />
              </RefreshScope>
            </td>
          </tr>
        )}
      </Fragment>
    );
  };

  // POST /jobs/{jobId}/kill terminates one active (queued|running) run per
  // call, so clearing a stacked queue is a loop until the endpoint refuses. The
  // loop is bounded: the global concurrency cap means a job can never have
  // more than maxConcurrent active runs. Each stop is individually audited.
  //
  // It breaks on ANY error, which covers both 409s: no_active_run (the queue is
  // drained — the expected exit) and RX-6's already_terminal (a run finished
  // between selection and the write). Stopping early on the second is correct:
  // it means the world moved under us, and the operator can re-run the clear
  // against whatever is actually left rather than us looping on a stale premise.
  //
  // No disposition is sent, deliberately — see ConfirmKill. These runs never
  // started, so there is no outcome to assert about them.
  async function clearActiveRuns(job: Job) {
    if (job.id == null) return;
    setBusyId(job.id);
    setActionError(null);
    const opts = { params: { path: { jobId: job.id }, header: csrfHeader } };
    let cleared = 0;
    for (let i = 0; i < 100; i++) {
      const { error: err } = await api.POST("/jobs/{jobId}/kill", opts as never);
      if (err) break; // 409 no_active_run → queue drained
      cleared++;
    }
    setBusyId(null);
    if (cleared === 0) {
      setActionError(`Clear failed for ${job.name}: no active runs to kill`);
    } else {
      fireToast(`Cleared ${cleared} active run${cleared === 1 ? "" : "s"}: ${job.name}`);
    }
    setBump((n) => n + 1);
  }

  return (
    <div>
      {/* Status tabs */}
      <TabBar
        // VU2-4 — an empty status tab drops a step: "Running (0) · Success (0) ·
        // Failed (0)" at full strength made the strip unscannable. "All" is
        // never dimmed; it is the tab you return to, not a result count.
        tabs={TABS.map((t) => ({ label: `${t.label} (${counts[t.key] ?? 0})`, muted: t.key !== "all" && (counts[t.key] ?? 0) === 0 }))}
        active={TABS.findIndex((t) => t.key === tab)}
        onChange={(i) => {
          setTab(TABS[i].key);
          pager.setPage(0);
        }}
      />

      <div style={{ display: "flex", gap: 8, marginBottom: 16, alignItems: "center", flexWrap: "wrap" }}>
        <SearchBar value={search} onChange={(v) => { setSearch(v); pager.setPage(0); }} placeholder="Search jobs…" style={{ flex: 1, minWidth: 200, maxWidth: 340 }} />
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
        <TagFilterSelect items={items} selected={tagFilter} onChange={(v) => { setTagFilter(v); pager.setPage(0); }} getTags={(j) => inlineTags.tagsFor(j)} matchMode={tagMatch} onMatchModeChange={(m) => { setTagMatch(m); pager.setPage(0); }} />
        {/* CO-3 — beside the other things that change what this table shows. */}
        <ColumnsMenu cols={cols} cw={cw} />
        {/* I-1 (VF-11) — gated on Compose, like the empty state's "Create a job"
            below and like Schedules' toolbar, which has always done this. It was
            the one create affordance on the page that was ungated, so a Viewer
            saw a primary button that led to JobComposer's "requires the Compose
            capability" notice: the page disagreed with itself, and the
            disagreement cost a click to discover. */}
        {canCompose && (
          <Link to="/compose" style={{ textDecoration: "none" }}>
            <Btn primary style={{ padding: "8px 14px", fontSize: c.fontSm }}>+ Create</Btn>
          </Link>
        )}
        {/* GitLab publish authoring (kind: Job YAML) — gated on PublishSchedule
            (admin OR approver), matching the server requirePerm gate (PP-B1). */}
        {canPublish && (
          <Link to="/jobs/publish" style={{ textDecoration: "none" }}>
            <Btn style={{ padding: "8px 14px", fontSize: c.fontSm }}>+ Publish to GitLab</Btn>
          </Link>
        )}
        {Object.keys(cw.widths).length > 0 && (
          <button
            onClick={cw.reset}
            title="Reset column widths to defaults"
            style={{ padding: "7px 10px", borderRadius: c.radiusChip, border: `1px solid ${c.border}`, background: "transparent", color: c.textMuted, fontSize: c.fontSm, cursor: "pointer", whiteSpace: "nowrap" }}
          >
            Reset columns
          </button>
        )}
      </div>

      {actionError && <div style={{ color: c.danger, marginBottom: 12, fontSize: c.fontSm }}>{actionError}</div>}
      {error && <div style={{ color: c.danger, marginBottom: 12 }}>Error: {error}</div>}

      {pg.totalItems > items.length && (
        <div style={{ marginBottom: 12, fontSize: c.fontSm, color: c.textSec, background: c.panel2, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, padding: "8px 10px" }}>
          Large catalog ({pg.totalItems} jobs). Only the first {items.length} are loaded — filters, search, and paging apply to those.
        </div>
      )}

      {/* B-1/VU-5: the catalog no longer sits in a Card. A table is the page's
          content, not an object on it, so it spans between a top and a bottom
          rule and drops one level of nesting. */}
      <TableSurface>
        {loading ? (
          <div style={{ padding: 16 }}>
            <SkeletonRows rows={5} />
          </div>
        ) : filtered.length === 0 ? (
          // VU-14 — two different states, two different next steps: an empty
          // catalog offers in-app authoring (Compose-gated, since the composer
          // refuses a caller without it) and names Git as the other source; a
          // filtered-empty table offers the filters back.
          <div style={{ padding: 36, textAlign: "center", color: c.textMuted, fontSize: c.fontSm }}>
            {pg.totalItems === 0 ? (
              <>
                <div>No jobs yet. Jobs are synced from the jobs/ directory of the definitions repo, or authored in Compose.</div>
                {canCompose && (
                  <div style={{ marginTop: 12 }}>
                    <Link to="/compose" style={{ textDecoration: "none" }}>
                      <Btn small>Create a job</Btn>
                    </Link>
                  </div>
                )}
              </>
            ) : (
              <>
                <div>No jobs match your filters.</div>
                <div style={{ marginTop: 12 }}>
                  <Btn small onClick={clearFilters}>Clear filters</Btn>
                </div>
              </>
            )}
          </div>
        ) : (
          // LB14: wrap in an overflowX scroll container and use tableLayout:auto +
          // minWidth (= JOB_COL_W sum) so the table compresses to the surface when
          // wide and scrolls (instead of clipping the ACTIONS column) when narrow.
          // Mirrors the shipped SshTargets/LB10 pattern. The inner container stays
          // even though TableSurface also scrolls: only the table may scroll
          // sideways, the Pager below it must not.
          <>
          <div style={{ overflowX: "auto" }}>
          {search.trim() ? (
            // Search mode: flat results across all folders, full path shown (FB),
            // paged client-side.
            // LB14's minWidth is now summed from the VISIBLE columns per render,
            // so hiding one actually frees its width — a sum over all columns
            // would keep it reserved and the table would refuse to compress.
            <table style={{ width: "100%", borderCollapse: "collapse", tableLayout: "auto", minWidth: cols.minWidth }}>
              <thead>
                <TableHead columns={cols.visible} sort={sort} cw={cw} />
              </thead>
              <tbody>{pageItems.map((j) => renderJobRow(j, jobDisplayPath(j)))}</tbody>
            </table>
          ) : (
            // Browse mode: navigate the folder tree one level at a time, grouped by
            // source_path. The active status/type filters are already applied to
            // `filtered`, so folder contents respect the current tab. The current
            // level is paged inside <FolderBrowser paginate> (default 25/page).
            <FolderBrowser
              items={sort.sorted}
              preserveLeafOrder
              getPath={jobDisplayPath}
              getName={jobName}
              path={path}
              onNavigate={setPath}
              rootLabel="Jobs"
              // TS-24: browse mode shares this header AND its sort. Folders stay
              // alpha-first (they carry none of these columns); the leaves inside
              // the open folder follow the active column, because `items` is
              // already sorted and preserveLeafOrder keeps that order.
              header={<TableHead columns={cols.visible} sort={sort} cw={cw} />}
              colCount={cols.visible.length}
              renderLeaf={(leaf) => renderJobRow(leaf.item, leaf.label)}
              hideBreadcrumbAtRoot
              paginate
            />
          )}
          </div>
          {search.trim() && <Pager pager={pager} page={page} total={total} noun="jobs" />}
          </>
        )}
      </TableSurface>

      {runFor && (
        <RunDialog
          job={runFor}
          scopes={scopes}
          nameAmbiguous={ambiguousJobNames.has(runFor.name ?? "")}
          busy={busyId === runFor.id}
          onCancel={() => setRunFor(null)}
          onRun={async (scope, executor, env, targetHosts, targetGroups, ansibleLimit, audit, references, identity, ansibleOpts, placement) => {
            const res = await act(runFor, "run", {
              scope,
              executor,
              env,
              targetHosts,
              targetGroups,
              ansibleLimit,
              ...(ansibleOpts ?? {}),
              references,
              ...audit,
              ...identity,
              // RT-3 — spread, so an absent runnerTag stays absent rather than
              // becoming an explicit undefined the body-builder would have to
              // re-distinguish.
              ...(placement ?? {}),
            });
            // Keep the dialog open on an invalid-executor rejection so the
            // operator can pick a different executor; close on success.
            return res;
          }}
          onDone={() => setRunFor(null)}
        />
      )}

      {killFor && (
        <ConfirmKill
          job={killFor}
          busy={busyId === killFor.id}
          onCancel={() => setKillFor(null)}
          onConfirm={(outcome) => {
            const j = killFor;
            setKillFor(null);
            act(j, "kill", undefined, outcome);
          }}
          onConfirmAll={() => {
            const j = killFor;
            setKillFor(null);
            clearActiveRuns(j);
          }}
        />
      )}

      {historyFor && (
        <RevisionHistory kind="job" name={historyFor} onClose={() => setHistoryFor(null)} onRestored={() => setBump((n) => n + 1)} />
      )}
      {deleting && (
        <ConfirmDialog
          title={delBlock ? "Delete Job — reactions watch it" : "Delete Job"}
          message={
            delBlock ? (
              // RX-24 — the server names the reactions, so show what it said
              // rather than paraphrasing a list the operator needs exactly.
              <>
                <div style={{ color: c.danger, marginBottom: 10 }}>{delBlock}</div>
                Deleting anyway keeps those reactions rather than removing them: they will show as{" "}
                <strong>missing</strong> on <Link to="/schedules?tab=reactions">Schedules → Reactions</Link> and can never
                fire until you repoint or delete them.
              </>
            ) : (
              <>
                Delete <strong>{deleting.name}</strong>? Its run history is kept, but the job, its schedule and
                its references are removed. This cannot be undone.
              </>
            )
          }
          confirmLabel={delBlock ? "Delete anyway" : "Delete Job"}
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

// Expanded-row detail: re-fetch the single job for the freshest schedule/result,
// plus this job's recent run history (with inline log drill-in) and a duration
// trend computed from those runs. Everything here is API-backed — the prototype's
// dependency graph and SSH connection panel have no v1 endpoint, so they're omitted.
// `tags` is the parent's override-aware tag set (so the editor and the table column
// stay in sync); onTagsSaved propagates an edit back up for the optimistic table
// update. Tags are operator-owned and SQLite-only (tags-support.md D6).
function JobDetail({ jobId, fallback, tags, onSaveTags, tagErr, actions, canEdit, resolveAnnotation, onSaveAnnotation, annotationErr }: { jobId?: number; fallback: Job; tags: string[]; onSaveTags: (tags: string[]) => void; tagErr?: string; actions?: React.ReactNode; canEdit?: boolean; resolveAnnotation?: (base: Annotation) => Annotation; onSaveAnnotation?: (next: Annotation) => void; annotationErr?: string }) {
  // RT-Q9 — what is displayed is the RESOLVED answer, straight from the server;
  // the detail never re-implements the pin precedence locally.
  const { tags: runnerTags } = useRunnerTags();
  const { data } = useGet<Job>(() => api.GET("/jobs/{jobId}", { params: { path: { jobId: jobId! } } }), [jobId]);
  const j = data ?? fallback;
  const name = j.name ?? fallback.name ?? "";
  // RX-16 — the whole edge list in one fetch; both directions are derived from
  // it. Safe here because exactly one JobDetail is mounted at a time (`expanded`
  // is a single id, not a set).
  const { edges: reactionEdges } = useReactionEdges();
  // Server-side paging (PP-H7 pattern, as History › Executions): fetch only the
  // current page so a job's full history stays reachable past the former 20-row
  // cap. The pager lives here (not in RecentRuns) because the fetch does.
  const runsPager = usePager();
  // R2-1 — ask by the job's permanent uid when we have it, falling back to the
  // name for a row that predates the backfill. The name filter matches every job
  // called this, which is one job today and may be several per agency later; the
  // uid is the only filter that keeps meaning THIS job.
  const runsUid = j.uid ?? fallback.uid;
  const runsQ = useGet<unknown>(
    () =>
      api.GET("/runs", {
        params: {
          query: runsUid
            ? { jobUid: runsUid, page: runsPager.page + 1, pageSize: runsPager.pageSize }
            : { job: name, page: runsPager.page + 1, pageSize: runsPager.pageSize },
        },
      }),
    [name, runsUid, runsPager.page, runsPager.pageSize],
  );
  const runsPg = paged<Run>(runsQ.data);
  const runs = runsPg.items;
  // The duration trend depicts the MOST RECENT runs. Pin it to page 1's rows
  // while the operator browses older pages — recomputing it over page N would
  // silently turn "recent" into "wherever you happen to be".
  const [trendRuns, setTrendRuns] = useState<Run[]>([]);
  useEffect(() => {
    if (runsPager.page === 0 && !runsQ.loading) setTrendRuns(runs);
    // runs is derived from runsQ.data; the data object is its stable identity.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runsQ.data, runsQ.loading, runsPager.page]);
  // Tag edits go through the parent's shared useInlineTags hook (CC.18).

  // EV-2 — populated fields only, with the absent ones collected into one muted
  // tail line instead of ~6 labelled "—" cells. Scope, Host, Schedule and Last
  // run are deliberately ABSENT from this grid: the collapsed row directly above
  // already carries all four, and repeating them was the single largest source
  // of noise here.
  // VU2-5 — `runFact` marks the fields that exist only once a job has RUN. A
  // job that has never run is missing all of them at once, and enumerating the
  // absence of four run facts is a noisy way to say "this has not run yet";
  // the tail line below collapses exactly that case. Definition facts (Created,
  // Last edited, Working calendars…) are NOT run facts: they are absent for
  // unrelated reasons and keep the enumeration, which is where it earns its keep.
  const overview: { label: string; value: React.ReactNode; mono?: boolean; present: boolean; runFact?: boolean }[] = [
    { label: "Executor", value: executorLabel(j), present: true },
    // RT-3 — "Run on" sits immediately after Executor: the two answer adjacent
    // halves of where this runs. Always present, because "any eligible runner"
    // is a real and useful answer rather than a missing value.
    {
      label: "Run on",
      value: <RunnerPinValue declared={j.runnerTag} tags={runnerTags} />,
      present: true,
    },
    { label: "Next run", value: j.status === "paused" ? "—" : fmtWhen(j.nextRunAt), present: j.status !== "paused" && j.nextRunAt != null, runFact: true },
    // AR — a parked ad-hoc run someone scheduled from the Run dialog; distinct
    // from Next run (the standing-schedule projection). Cancel lives on
    // Schedules → Upcoming.
    { label: "Scheduled ad-hoc run", value: fmtWhen(j.pendingRunAt), present: j.pendingRunAt != null, runFact: true },
    { label: "Last duration", value: j.lastDurationMs != null ? fmtDuration(j.lastDurationMs) : "—", mono: true, present: j.lastDurationMs != null, runFact: true },
    // FX-D1 — the newest SUPPRESSED fire, apart from the last run so neither
    // impersonates the other. The reason is the value: "when" without "why"
    // would just be an alarming timestamp.
    {
      label: "Last fire suppressed",
      value: j.lastSkippedAt ? `${fmtWhen(j.lastSkippedAt)} — ${j.lastSkipReason ?? "no reason recorded"}` : "—",
      present: j.lastSkippedAt != null,
      runFact: true,
    },
    { label: "Last changed", value: fmtWhen(j.lastChangedAt), present: j.lastChangedAt != null },
    { label: "Created", value: fmtDateTime(j.createdAt), mono: true, present: j.createdAt != null },
    { label: "Last edited", value: fmtDateTime(j.lastModifiedAt), mono: true, present: j.lastModifiedAt != null },
    // CAL-12 — "does this job run on holidays?" answered in one place. Computed
    // from the entries the detail already carries rather than fetching the whole
    // schedules inventory to look one definition up.
    { label: "Working calendars", value: calendarRollup(j.schedules ?? []), present: !!calendarRollup(j.schedules ?? []) },
    { label: "Workflow step", value: `#${j.workflowId}`, mono: true, present: j.workflowId != null },
    { label: "Waiting because", value: (j.queuedReason ?? "").replace(/_/g, " "), present: !!j.queuedReason },
  ];
  // VU2-5 — the tail splits in two. A job with no runs at all says so once, in
  // words that name the action that fixes it; anything else absent keeps the
  // EV-2 enumeration. `totalItems` is the authoritative "has this ever run"
  // signal — the run feed this panel already fetched — rather than an inference
  // from which fields happen to be null.
  const neverRan = !runsQ.loading && runsPg.totalItems === 0;
  const missingAll = overview.filter((f) => !f.present);
  const missing = missingAll.filter((f) => !(neverRan && f.runFact)).map((f) => f.label);
  const noRunsYet = neverRan && missingAll.some((f) => f.runFact);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 20, marginTop: 4, fontSize: c.fontSm }}>
      {/* EV-4 — actions get ONE home: top-right of the detail's header strip,
          matching Runners. They used to sit in a lone strip above the detail.
          EP-4 — the strip now renders unconditionally, because Refresh is always
          available even on a job this operator may not run or edit; Refresh
          leads so the non-destructive control is not beside Delete. */}
      <div style={{ display: "flex", justifyContent: "flex-end", gap: 6, flexWrap: "wrap" }}>
        <RefreshButton />
        {actions}
      </div>

      {/* AN-3 — the annotation leads the panel. It answers "what am I looking at
          and who owns it", which is the question you have BEFORE the overview
          grid's fields mean anything; below the fold as a footnote it would be
          read after the decision it exists to inform. */}
      {onSaveAnnotation && (
        <AnnotationSection
          kind="job"
          value={(resolveAnnotation ?? ((b) => b))(annotationOf(j))}
          onSave={onSaveAnnotation}
          error={annotationErr}
        />
      )}

      {/* EV-2 — two columns at wide viewports: configuration (left), activity
          (right). The flex-basis wrap collapses to one column when narrow. */}
      <div style={{ display: "flex", gap: 36, flexWrap: "wrap", alignItems: "flex-start" }}>
        <div style={{ flex: "2 1 420px", minWidth: 0, display: "flex", flexDirection: "column", gap: 22 }}>
          <div>
            <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(170px, 1fr))", gap: "14px 24px" }}>
              {overview.filter((f) => f.present).map((f, i) => (
                <Fragment key={f.label}>
                  <Field label={f.label} value={f.value} mono={f.mono} />
                  {/* EV-6 — the SSH key follows Executor, the field it completes:
                      one says how the job runs, the other what key material the run
                      gets (and whether that executor delivers it at all). It used to
                      be one chip inside the mixed References list below, ranked
                      equally with a dozen optional variables. */}
                  {i === 0 && jobId != null && (
                    <JobKeyField jobId={jobId} scope={j.scope ?? ""} executor={j.executor ?? null} />
                  )}
                </Fragment>
              ))}
            </div>
            {noRunsYet && (
              <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 10 }}>No runs yet — Run it to start recording history.</div>
            )}
            {missing.length > 0 && (
              <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 10 }}>Not recorded: {missing.join(" · ")}</div>
            )}
          </div>

          {/* The pin is READ-ONLY here — "Run on" above states the resolved
              answer and attributes it. Placement is authored in two places and
              two only: the Composer (the job's declared pin) and the Run dialog
              (this run, this once). An editor on the detail panel was a third
              home for the same idea, and a persistent one for a decision that
              is usually a one-run decision. */}
          <Section
            title={`Tags${tags.length ? ` (${tags.length})` : ""}`}
            info="Tags are stored in Cronomicon only — they are not written back to Git, and they are kept across syncs."
          >
            <TagEditor tags={tags} onChange={onSaveTags} />
            {tagErr && <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 6 }}>{tagErr}</div>}
          </Section>

          {/* JP-4b — read-only, and absent entirely when this job receives no
              references. Authoring moved to the composer; what this shows is the
              EFFECTIVE set (the job's own plus the script's), which is what
              dispatch injects and what no other surface could tell you. */}
          {jobId != null && (
            <JobEffectiveReferences
              jobId={jobId}
              scriptRef={j.scriptRef}
              scope={j.scope ?? ""}
              // JP-Q8 — the editor moved, so the section says where it went. Same
              // gate as the row's Edit button (compose + cronomicon source): a caller
              // who cannot reach the composer is not sent to it.
              action={
                canEdit ? (
                  <Link to={`/compose?id=${jobId}`} style={{ textDecoration: "none" }}>
                    <Btn small>Edit in Composer</Btn>
                  </Link>
                ) : undefined
              }
            />
          )}

          {jobId != null && (j.watch?.length ?? 0) > 0 && (
            <FileArrivalsSection jobId={jobId} />
          )}

          {j.schedules && j.schedules.length > 0 && (
            <Section title={`Schedules (${j.schedules.length})`}>
              {/* B-1/VU-5 — these were bordered mini-cards inside an already-framed
                  detail panel. A hairline plus leading groups them just as well
                  without the middle frame. */}
              <div style={{ display: "flex", flexDirection: "column" }}>
                {j.schedules.map((s, i) => {
                  const envCount = s.env ? Object.keys(s.env).length : 0;
                  return (
                    <Fragment key={s.name ?? i}>
                      {i > 0 && <Rule />}
                      <div
                        style={{
                          display: "flex",
                          alignItems: "center",
                          gap: 12,
                          padding: "8px 2px",
                          fontSize: c.fontSm,
                        }}
                      >
                        <span style={{ fontFamily: c.mono, fontWeight: 600, color: c.text }}>{s.name}</span>
                        <span style={{ fontFamily: c.mono, color: c.textSec }}>{s.cron}</span>
                        {envCount > 0 && <span style={{ color: c.textMuted }}>· {envCount} env</span>}
                        <span style={{ color: c.textSec, marginLeft: "auto" }}>
                          next {j.status === "paused" ? "—" : fmtWhen(s.nextRunAt)}
                        </span>
                      </div>
                    </Fragment>
                  );
                })}
              </div>
            </Section>
          )}

          {/* RX-16 — reactions are the fourth trigger kind, so they sit beside
              Schedules and above the definition body. Both directions: the
              "reacted on by" half is the one nothing else on this page could
              show, because a reaction lives on the OTHER definition. */}
          <ReactionPanels edges={reactionEdges} kind="job" source={j.source} name={name} />

          <JobSource job={j} />
          <JobPrompts job={j} />
        </div>

        {/* Activity column — what has this job been doing. */}
        <div style={{ flex: "1.9 1 430px", minWidth: 0, display: "flex", flexDirection: "column", gap: 22 }}>
          <DurationTrend runs={trendRuns} />
          {/* SL-E — the windowed view. DurationTrend answers "what did the last
              twenty runs do"; this answers "is this getting worse". */}
          <Section title="Analytics">
            <RunAnalytics job={j.name} source={j.source === "cronomicon" ? "cronomicon" : "git"} />
          </Section>
          <Section title="Recent runs">
            <RecentRuns
              runs={runs}
              loading={runsQ.loading}
              error={runsQ.error}
              emptyText="No runs recorded for this job yet."
              pager={runsPager}
              page={runsPager.page}
              total={runsPg.totalItems}
            />
          </Section>
        </div>
      </div>
    </div>
  );
}

function executorLabel(j: Job): string {
  if (j.executor === "ssh") return "SSH (in-app)";
  if (j.executor === "runner") return "Runner agent";
  return "Auto (resolved at run)";
}

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
});

// The job's executable source. When the job references a first-class Script
// (B-Git), the body lives on the Script — link there instead of re-rendering it.
// Otherwise (legacy inline jobs) show the inline command/script/scriptPath.
function JobSource({ job }: { job: Job }) {
  if (job.scriptRef) {
    return (
      <Section title="Script">
        <Link
          to={`/scripts?focus=${encodeURIComponent(job.scriptRef)}`}
          style={{
            display: "inline-flex",
            alignItems: "center",
            gap: 8,
            padding: "9px 13px",
            background: c.panel2,
            border: `1px solid ${c.border}`,
            borderRadius: c.radiusChip,
            fontFamily: c.mono,
            fontSize: c.fontSm,
            color: c.primary,
            textDecoration: "none",
          }}
        >
          {job.scriptRef} <span aria-hidden>→</span>
        </Link>
      </Section>
    );
  }
  const kind = job.command ? "command" : job.script ? "script" : job.scriptPath ? "scriptPath" : null;
  if (!kind) return null;
  const label = kind === "command" ? "Command" : kind === "script" ? "Script" : "Script file";
  const body = kind === "scriptPath" ? job.scriptPath! : kind === "command" ? job.command! : job.script!;
  return (
    <Section title={label}>
      <pre style={{ ...codeBlock(), maxHeight: 280 }}>{body}</pre>
    </Section>
  );
}

// JP-2 — cap the Run inputs list at 5 rows and scroll the rest, mirroring the
// RecentRuns idiom (RECENT_RUNS_MAX_H = 340 ≈ 10 rows). Pixels, not a row count:
// rows are hairline-separated flex rows of near-constant height (8px pad ×2 +
// one fontSm line + 1px Rule ≈ 33px, measured in the running app), so 5 × 33 =
// 165 lands on a whole row rather than slicing the sixth. A wrapped row (a long
// label at a narrow width) is taller, so the visible count can dip below five —
// same approximation RecentRuns' cap accepts. Plain number, so no theme token is
// frozen at module load.
const PROMPT_ROWS_MAX_H = 165;

// Read-only list of the job's declared run inputs (UDV1). These are the values the
// ad-hoc Run dialog will ask an operator to supply; here we only DISPLAY what's
// declared (name, label, required, default, allowed options). Filling values and
// running happen in RunDialog. Titled "Run inputs" to match the Run dialog and the
// Composer (JR-Q2 — one term across all three surfaces); hidden when none are declared.
function JobPrompts({ job }: { job: Job }) {
  const prompts = job.prompts ?? [];
  if (prompts.length === 0) return null;
  const requiredCount = prompts.filter((p) => p.required).length;
  const blocking = job.promptEnforcement === "block";
  // T3.7/G7 — the same compose-time check (T3.6), surfaced read-only for a job that is
  // ALREADY saved and scheduled. Nobody is present to answer when a schedule fires, so
  // a required input with no default that nothing supplies is unfillable on every
  // scheduled run. Read-only: this panel never edits, it only tells the truth.
  const jobEnvKeys = new Set(
    Object.entries(job.env ?? {})
      .filter(([, v]) => (v ?? "").trim() !== "")
      .map(([k]) => k),
  );
  const scheduleEnvKeys = new Set((job.schedules ?? []).flatMap((s) => Object.keys(s.env ?? {})));
  const scheduled = (job.schedules ?? []).length > 0 || !!job.schedule;
  const unschedulable = !scheduled
    ? []
    : prompts
        .filter(
          (p) =>
            p.required &&
            !!p.name &&
            (p.default ?? "").trim() === "" &&
            !jobEnvKeys.has(p.name) &&
            !scheduleEnvKeys.has(p.name),
        )
        .map((p) => p.name);
  return (
    <Section
      title={`Run inputs (${prompts.length}${requiredCount ? `, ${requiredCount} required` : ""}${blocking ? ", enforced" : ""})`}
      info="These inputs are supplied when you run the job. Use ▶ Run to fill them in or confirm their defaults."
    >
      {unschedulable.length > 0 && (
        <div style={{ fontSize: c.fontXs, color: c.warning, background: c.warningBg, border: `1px solid ${c.warning}30`, borderRadius: c.radiusSurface, padding: "8px 10px", marginBottom: 8 }}>
          ⚠ This job is scheduled, but <code>{unschedulable.join(", ")}</code>{" "}
          {unschedulable.length === 1 ? "is required and has" : "are required and have"} no default and nothing supplies{" "}
          {unschedulable.length === 1 ? "it" : "them"}. No one is present to answer when a schedule fires, so every
          scheduled run records {unschedulable.length === 1 ? "it" : "them"} as unfilled.
        </div>
      )}
      {/* B-1/VU-5 — same de-boxing as the Schedules list above: one hairline
          between rows instead of a border around each. JP-2 — more than five
          declared inputs scroll inside a capped port instead of growing the
          section; the cap is conditional so exactly five never shows a
          scrollbar sliver. The unschedulable warning above stays outside the
          scrollport (as RecentRuns keeps its pager outside). */}
      <div
        style={{
          display: "flex",
          flexDirection: "column",
          maxHeight: prompts.length > 5 ? PROMPT_ROWS_MAX_H : undefined,
          overflowY: prompts.length > 5 ? "auto" : undefined,
        }}
      >
        {prompts.map((p, i) => (
          <Fragment key={p.name}>
            {i > 0 && <Rule />}
            <div
              style={{
                display: "flex",
                alignItems: "baseline",
                flexWrap: "wrap",
                gap: 10,
                padding: "8px 2px",
                fontSize: c.fontSm,
              }}
            >
              {/* Primary: the human label, falling back to the env key. */}
              <span style={{ fontWeight: 600, color: c.text }}>{p.label || p.name}</span>
              {/* The env key it binds to — only shown separately when a label exists. */}
              {p.label && <code style={{ fontFamily: c.mono, color: c.textMuted }}>{p.name}</code>}
              {p.required && (
                <span style={{ fontSize: c.fontXs, fontWeight: 600, color: c.warning }}>● required</span>
              )}
              {p.default != null && p.default !== "" && (
                <span style={{ color: c.textSec }}>
                  default <code style={{ fontFamily: c.mono, color: c.textSec }}>{p.default}</code>
                </span>
              )}
              {p.options && p.options.length > 0 && (
                <span style={{ color: c.textMuted, marginLeft: "auto" }}>
                  options: {p.options.join(", ")}
                </span>
              )}
            </div>
          </Fragment>
        ))}
      </div>
    </Section>
  );
}



// Run dialog: optional scope override + executor picker (R5.2). The API allows
// running against a scope whose declared types exclude the job type (advisory
// model) — we soft-warn. The executor picker enforces the capability matrix:
// ansible/terraform are runner-only (SSH disabled with a tooltip); shell types
// (bash/perl/powershell/python) accept either. The chosen executor is sent as the
// `executor` field on POST /jobs/{jobId}/run; a 422 invalid_executor is
// surfaced inline so the operator can correct it without losing the dialog.
//
// RU-7 — a quiet label that groups controls INSIDE an open fold without adding a
// second disclosure level to click through. Deliberately not a Disclosure: the
// grouping is for the eye, and nothing here should ever be collapsible on its own.
// Tokens are read per render (never hoisted to a module const) so the Light Mode
// toggle repaints it — the theme-staleness rule.
// Exported for Jobs.RunDialog.test.tsx — the run-input gating (T1.5/T1.7) is pure
// dialog-local state, so testing it through the whole Jobs page would mean mocking
// every catalog fetch to assert logic that never leaves this component.
export function RunDialog({
  job,
  scopes,
  nameAmbiguous,
  busy,
  onCancel,
  onRun,
  onDone,
}: {
  job: Job;
  scopes: Scope[];
  // R2F-3 — the dialog holds ONE job, so it cannot decide on its own whether
  // that job's name is ambiguous. The catalog it was opened from can, and
  // passes the answer in: true ⇒ the title qualifies the name with the job's
  // agency, because "Run backup-daily" is not enough to act on when two
  // departments own a backup-daily.
  nameAmbiguous?: boolean;
  busy: boolean;
  onCancel: () => void;
  onRun: (
    scope: string | undefined,
    executor: Executor | undefined,
    env: Record<string, string> | undefined,
    targetHosts: string[] | undefined,
    targetGroups: string[] | undefined,
    ansibleLimit: string | undefined,
    // T2.4 — run-input audit metadata, grouped rather than added as two more
    // positional params to a signature that is already six deep.
    audit?: { promptAnswers?: Record<string, string>; promptAcknowledged?: boolean; reviewedSections?: string[]; runAt?: string },
    // V2-11 — per-run stored-reference ADDITIONS (kind + bare name), additive over
    // the job's declared bindings. Trailing so the audit param keeps its position.
    references?: { kind: "secret" | "var" | "key"; name: string }[],
    // CA — per-run "connect as" identity (username + stored-credential LABEL,
    // names only). Grouped like `audit`; trailing so earlier params keep position.
    identity?: { sshUser?: string; sshCredential?: string },
    // Phase 3 — advanced ansible options; trailing so every earlier param keeps
    // its position, and grouped for the same reason `identity` is.
    ansibleOpts?: {
      ansibleCheck?: boolean;
      ansibleDiff?: boolean;
      ansibleTags?: string[];
      ansibleSkipTags?: string[];
      ansibleVerbosity?: number;
      ansibleBecome?: boolean;
      ansibleBecomeUser?: string;
      ansibleExtraVars?: Record<string, string>;
    },
    // RT-3 — per-run placement. Grouped like `identity`, and appended AFTER
    // ansibleOpts rather than beside it: every one of these trailing params is
    // positional, so inserting in the middle silently re-binds the existing
    // callers' arguments. runnerTag undefined ⇒ inherit the job's effective pin,
    // "" ⇒ run this once unpinned, "x" ⇒ pin this run to x.
    placement?: { runnerTag?: string },
  ) => Promise<{ ok: boolean; code?: string; message?: string }>;
  onDone: () => void;
}) {
  const [scope, setScope] = useState<string>(job.scope ?? "");
  // R2F-3 — the title names the job the operator is about to RUN, so when the
  // name alone is ambiguous it carries the agency. Not a badge computed here:
  // the catalog decided (nameAmbiguous), because only it can see the collision.
  const runTitleName = nameAmbiguous ? `${job.name}${agencySuffix(job.agencies)}` : job.name;
  const runnerOnly = isRunnerOnly(job.type);
  // RP-3 — a job whose executor is Auto (unset) still resolves to a concrete
  // choice here (a run is always concrete), but the dialog SAYS so instead of
  // presenting the resolution as if the job had pinned it.
  const executorAuto = job.executor !== "ssh" && job.executor !== "runner";
  // Default the picker: runner-only run-types force Runner; otherwise prefer the
  // job's own executor when it's a concrete choice, else SSH (the shell default).
  const defaultExecutor: Executor = runnerOnly ? "runner" : job.executor === "runner" ? "runner" : "ssh";
  const [executor, setExecutor] = useState<Executor>(defaultExecutor);
  const [runErr, setRunErr] = useState<string | null>(null);
  // F1 per-run env overrides + F2 host subset within the bound scope.
  const [envRows, setEnvRows] = useState<{ key: string; value: string }[]>([]);
  const [limitHosts, setLimitHosts] = useState(false);
  const [pickedHosts, setPickedHosts] = useState<string[]>([]);
  // M3 — group subset + raw ansible --limit passthrough.
  const [limitGroups, setLimitGroups] = useState(false);
  const [pickedGroups, setPickedGroups] = useState<string[]>([]);
  const [ansibleLimit, setAnsibleLimit] = useState("");
  // Phase 3 (RP-19) — advanced ansible options, per-run only. They render inside
  // the Advanced options section (RV), collapsed by default: deliberately not
  // part of the ordinary run path.
  const [ansCheck, setAnsCheck] = useState(false);
  const [ansDiff, setAnsDiff] = useState(false);
  const [ansTags, setAnsTags] = useState("");
  const [ansSkipTags, setAnsSkipTags] = useState("");
  const [ansVerbosity, setAnsVerbosity] = useState(0);
  const [ansBecome, setAnsBecome] = useState(false);
  const [ansBecomeUser, setAnsBecomeUser] = useState("");
  const [ansExtraVars, setAnsExtraVars] = useState<{ key: string; value: string }[]>([]);
  // CA — per-run "connect as" identity. RP-10: offered for ssh-family AND
  // ansible runs (the server applies the latter as connection extra-vars);
  // terraform still 422s, so the block stays hidden there. The key picker is
  // gated on ManageEnvVars (CA-Q1 — selecting a stored key is a grant over key
  // material, the same rule as reference additions) and rendered
  // disabled-with-tooltip without it, so the affordance stays discoverable.
  const [sshUser, setSshUser] = useState("");
  const [sshCredential, setSshCredential] = useState("");
  const identityCapable = isIdentityCapable(job.type);
  const [canPickKey, setCanPickKey] = useState(false);
  // RB-29: scope REACH (not a permission) — an unrestricted caller may run an
  // unscoped job unbound; a restricted one must bind a scope they hold.
  const [unrestricted, setUnrestricted] = useState(false);
  useEffect(() => {
    fetchCapabilities().then((caps) => {
      setCanPickKey(caps.manageEnvVars);
      setUnrestricted(caps.unrestricted);
    });
  }, []);
  const credsQ = useGet<{ label?: string }[]>(() => api.GET("/ssh/credentials"), []);
  const credentialLabels = (credsQ.data ?? []).map((cr) => cr.label ?? "").filter(Boolean);

  const chosen = scopes.find((s) => s.scope === scope);
  const chosenTypes = chosen?.capability?.types;
  const incompatible = chosen && job.type && chosenTypes && chosenTypes.length > 0 && !chosenTypes.includes(job.type);

  // Hosts come from the effective scope (the override, else the job's own scope).
  const effScope = scope || job.scope || "";
  const scopeHosts = scopes.find((s) => s.scope === effScope)?.hosts ?? [];
  // RP-1 — the host subset is offered for BOTH executors now: SSH connects to the
  // selection; a runner run carries it as targetHosts, which the server folds into
  // the ansible --limit (RunLimit) or the manifest target set. The per-executor
  // truth ("cronomicon-inventory runners only", "terraform ignores it") lives in the
  // helper line rather than in a hidden control.
  const canPickHosts = scopeHosts.length > 0;
  // RB-26/RB-29 — a job with no declared scope carries no authority of its own, so
  // a RESTRICTED caller must bind one here. Without this the rule is discoverable
  // only as a 403 on a job that worked yesterday, with the remedy hidden inside a
  // collapsed section — the worst possible shape for a new rule.
  const jobUnscoped = !(job.scope ?? "");
  const mustBindScope = jobUnscoped && !unrestricted;
  const scopeMissing = mustBindScope && !scope;
  // M3 — groups come from the effective scope's projection (NAMES only). Offered
  // for BOTH executors: SSH expands group members to host targets, a runner passes
  // the group names as ansible --limit. Both target the identical set.
  const scopeGroups = scopes.find((s) => s.scope === effScope)?.groups ?? [];
  const canPickGroups = scopeGroups.length > 0;
  // Reset subsets whenever the effective scope changes (hosts/groups differ per scope).
  useEffect(() => {
    setLimitHosts(false);
    setPickedHosts([]);
    setLimitGroups(false);
    setPickedGroups([]);
  }, [effScope]);

  // `prompts` and `env` are DETAIL-only fields — the Jobs list row that opens this
  // dialog carries neither, so the dialog fetches its own detail rather than trusting
  // the caller to have one. Without this the declared run inputs never rendered at all
  // when the dialog was opened from the list (the only way to open it), which is how
  // UDV1's whole run-time surface came to be silently dead.
  const detailQ = useGet<Job>(
    () => api.GET("/jobs/{jobId}", { params: { path: { jobId: job.id! } } }),
    [job.id],
  );
  const jobDetail = detailQ.data ?? job;

  // RT-3 — the per-run pin. `null` means "inherit the job's effective pin" and
  // is the untouched default; a string (including "") is an explicit per-run
  // decision. The job-level answer is runnerTagEffective, resolved SERVER-side
  // (RT-Q7) so the dialog never re-implements the precedence.
  // RT-3 — from jobDetail, NEVER from `job`. Both pin fields are detail-only on
  // the API, and `job` is the catalog LIST row the dialog was opened from, where
  // they are undefined. Reading it there prefills "any
  // runner" for a pinned job — the dialog would then say the run goes anywhere
  // while the server correctly sends it to the pin. Caught on screen; tsc is
  // happy either way because both shapes are the same optional type.
  const jobPin = jobDetail.runnerTagEffective ?? "";
  const [runnerTagOverride, setRunnerTagOverride] = useState<string | null>(null);
  const { tags: runnerTags, loading: pinLoading } = useRunnerTags();
  const effectivePin = runnerTagOverride ?? jobPin;

  // UDV1 — declared run inputs. Pre-fill each with its default so an untouched default
  // is still submitted (UDV8). Answers fold into the per-run env overrides.
  const declaredPrompts = jobDetail.prompts ?? [];
  const [promptVals, setPromptVals] = useState<Record<string, string>>({});
  // The declarations arrive asynchronously with the detail, so seed the defaults when
  // they land — keeping anything the operator has already typed.
  const promptSig = declaredPrompts.map((p) => `${p.name} ${p.default ?? ""}`).join("");
  useEffect(() => {
    setPromptVals((prev) => {
      const next: Record<string, string> = {};
      for (const p of declaredPrompts) next[p.name] = prev[p.name] ?? p.default ?? "";
      return next;
    });
    // declaredPrompts is a fresh array each render; promptSig is its stable identity.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [promptSig]);
  // T1.5/JR-Q7 — per-row confirmation, so an operator affirms a default rather than
  // silently inheriting it. `edited` is set when they type or select; `ackedDefault`
  // when they click "Use default". Either one confirms the row — editing a value IS
  // acknowledging it, so there is never a second click to dismiss.
  const [edited, setEdited] = useState<Record<string, boolean>>({});
  const [ackedDefault, setAckedDefault] = useState<Record<string, boolean>>({});
  const setPrompt = (name: string, v: string) => {
    setPromptVals((prev) => ({ ...prev, [name]: v }));
    setEdited((prev) => ({ ...prev, [name]: true }));
  };
  const confirmDefault = (name: string) => setAckedDefault((prev) => ({ ...prev, [name]: true }));
  const isConfirmed = (name: string) => !!edited[name] || !!ackedDefault[name];
  // T1.7 — the operator's explicit "run without a required value" escape (JR-Q4b).
  // Client-state only in v1: it ungates the button but is NOT persisted (JR-Q8 defers
  // the wire field to Phase 2/T2.4). The run's promptWarnings record remains the audit
  // trail until then.
  const [runAnyway, setRunAnyway] = useState(false);
  // JR-Q5/T3.3 — a `block` job refuses the run server-side while a required input is
  // empty, and refuses it regardless of any acknowledgment (JR-Q10). So the escape is
  // HIDDEN here rather than disabled: offering a checkbox that only produces a 422
  // would be worse than not offering one.
  const blocking = jobDetail.promptEnforcement === "block";

  // T1.4 — what this run will ACTUALLY receive for an input, and where it comes from.
  // The precedence mirrors the server exactly so the dialog can never disagree with the
  // promptWarnings it will produce:
  //   per-run override row  (envRows — beats the prompt answer in submit() below)
  //   → prompt answer       (typed, or the pre-filled default)
  //   → job-level env       (jobs.env_json — the BASE layer of
  //                          envmerge.Merge(jr.Env, body.Env) in execution_mount.go)
  //
  // JR-Q1 — a scope Env Vars row of the same name is deliberately absent from this
  // chain. Env Vars reach a run only through an explicit reference binding and only
  // under the derived CRONOMICON_VAR_<name> key (internal/runref); nothing publishes a
  // bare `NAME`, so counting one claimed "✓" for a variable that would be missing at
  // run time while the server still recorded it unfilled.
  const resolveInput = (p: JobPrompt): { value: string; from: Provenance | null } => {
    const ov = envRows.find((r) => r.key.trim() === p.name && r.value.trim() !== "");
    if (ov) return { value: ov.value.trim(), from: "override" };
    const v = (promptVals[p.name] ?? "").trim();
    if (v !== "") return { value: v, from: edited[p.name] ? "you" : "default" };
    const je = (jobDetail.env ?? {})[p.name];
    if (je !== undefined && je.trim() !== "") return { value: je.trim(), from: "job env" };
    return { value: "", from: null };
  };
  // Two distinct states, deliberately (JR-Q7): `unfilled` means nothing will be sent —
  // it gates the button. `unconfirmed` means a default WILL be sent but the operator
  // hasn't affirmed it — surfaced, never blocking.
  const inputStates: InputState[] = declaredPrompts.map((p) => {
    const { value, from } = resolveInput(p);
    const unfilled = !!p.required && value === "";
    const unconfirmed = !!p.required && !unfilled && from === "default" && !isConfirmed(p.name);
    return { p, value, from, unfilled, unconfirmed };
  });
  const unfilledCount = inputStates.filter((s) => s.unfilled).length;
  const inputsBlock = unfilledCount > 0 && (blocking || !runAnyway);

  const toggleHost = (h: string) =>
    setPickedHosts((prev) => (prev.includes(h) ? prev.filter((x) => x !== h) : [...prev, h]));
  const toggleGroup = (g: string) =>
    setPickedGroups((prev) => (prev.includes(g) ? prev.filter((x) => x !== g) : [...prev, g]));
  // Never silently send [] as "target nothing": a limit toggle with no selection
  // blocks submission rather than fanning out (§7.1).
  const subsetInvalid = canPickHosts && limitHosts && pickedHosts.length === 0;
  const groupSubsetInvalid = canPickGroups && limitGroups && pickedGroups.length === 0;
  // RP-Q4 — the raw --limit pattern and the host/group pickers are mutually
  // exclusive: each disables the other while set, and submission double-checks.
  // Ansible's `:` is a UNION, so any clever combination could target MORE hosts
  // than either input alone suggests (the groups.go pin-broadening warning).
  // Tags are typed as a comma list and sent as an array; the server refuses a tag
  // containing a comma or pattern char, so splitting here matches its parse.
  const splitTags = (v: string) => v.split(",").map((t) => t.trim()).filter(Boolean);
  const ansTagList = splitTags(ansTags);
  const ansSkipTagList = splitTags(ansSkipTags);
  const ansExtraVarMap: Record<string, string> = {};
  for (const r of ansExtraVars) {
    const k = r.key.trim();
    if (k) ansExtraVarMap[k] = r.value;
  }
  // The two connection vars "Connect as" owns. `-e` is the same precedence tier
  // identity uses, so letting one through here would let a run connect as
  // someone other than what its own record says — the server 422s it, and the
  // dialog says so before the operator hits Run.
  const RESERVED_EXTRA_VARS = ["ansible_user", "ansible_ssh_private_key_file"];
  const reservedExtraVar = Object.keys(ansExtraVarMap).find((k) => RESERVED_EXTRA_VARS.includes(k));
  const ansOptsActive =
    ansCheck || ansDiff || ansTagList.length > 0 || ansSkipTagList.length > 0 ||
    ansVerbosity > 0 || ansBecome || ansBecomeUser.trim() !== "" || Object.keys(ansExtraVarMap).length > 0;

  const rawLimitActive = ansibleLimit.trim() !== "";
  const chipsActive = limitHosts || limitGroups;
  const limitConflict = rawLimitActive && chipsActive;

  // `confirmedDeviations` — true when this submit came through the deviation
  // window's Confirm button; recorded in the audit metadata alongside the
  // reviewed sections so History can see the deviations were confirmed.
  const submit = async (confirmedDeviations = false) => {
    setRunErr(null);
    const env: Record<string, string> = {};
    // UDV2/UDV8 — declared prompt answers (defaults included even if untouched) fold
    // into the per-run env overrides first; a manual override row with the same key
    // wins.
    for (const p of declaredPrompts) {
      const v = (promptVals[p.name] ?? "").trim();
      if (v) env[p.name] = v;
    }
    for (const r of envRows) {
      const k = r.key.trim();
      if (k) env[k] = r.value;
    }
    // RP-Q4 — chips and the raw pattern never travel together; the UI disables
    // one while the other is set, and this guard makes the exclusion true even
    // if state slipped past that (e.g. a toggle left on behind a disabled box).
    const targetHosts = canPickHosts && limitHosts && !rawLimitActive ? pickedHosts : undefined;
    const targetGroups = canPickGroups && limitGroups && !rawLimitActive ? pickedGroups : undefined;
    const rawLimit =
      executor === "runner" && job.type === "ansible" && rawLimitActive && !chipsActive ? ansibleLimit.trim() : undefined;
    // T2.4 — record HOW the operator arrived here: where each declared input's value
    // came from, and whether they knowingly proceeded past an unfilled required one.
    // Purely descriptive — it never changes what the run does.
    const promptAnswers: Record<string, string> = {};
    for (const s of inputStates) {
      if (s.from) promptAnswers[s.p.name] = s.from;
    }
    const res = await onRun(
      scope && scope !== job.scope ? scope : undefined,
      executor,
      Object.keys(env).length > 0 ? env : undefined,
      targetHosts,
      targetGroups,
      rawLimit,
      {
        promptAnswers,
        // Only meaningful when there was actually something to acknowledge.
        promptAcknowledged: runAnyway && unfilledCount > 0,
        // RV — which sections were open at some point before Run. Audit-only:
        // History can then tell a reviewed manual run from an API call. The
        // variables section defaults open, so its entry means "was on screen",
        // exactly like the others.
        reviewedSections: [
          ...(requireReview ? ["variables"] : []),
          ...(targetsVisited ? ["targets"] : []),
          ...(methodVisited ? ["method"] : []),
          ...(advancedVisited ? ["advanced"] : []),
          ...(whenVisited ? ["when-to-run"] : []),
          ...(confirmedDeviations ? ["confirmation"] : []),
        ],
        // AR — the deferral instant (absent = run now).
        runAt: runAt ?? undefined,
      },
      // V2-11 — per-run reference additions, names only (kind + bare name).
      addedRefs.length > 0 ? addedRefs.map((b) => ({ kind: b.kind as "secret" | "var" | "key", name: b.name })) : undefined,
      // CA — per-run identity, names only (the credential is a LABEL).
      sshUser.trim() || sshCredential
        ? { sshUser: sshUser.trim() || undefined, sshCredential: sshCredential || undefined }
        : undefined,
      // Phase 3 — only sent when something was actually set, so an ordinary run's
      // request body stays exactly what it was before these options existed.
      ansOptsActive
        ? {
            ansibleCheck: ansCheck || undefined,
            ansibleDiff: ansDiff || undefined,
            ansibleTags: ansTagList.length > 0 ? ansTagList : undefined,
            ansibleSkipTags: ansSkipTagList.length > 0 ? ansSkipTagList : undefined,
            ansibleVerbosity: ansVerbosity > 0 ? ansVerbosity : undefined,
            ansibleBecome: ansBecome || undefined,
            ansibleBecomeUser: ansBecomeUser.trim() || undefined,
            ansibleExtraVars: Object.keys(ansExtraVarMap).length > 0 ? ansExtraVarMap : undefined,
          }
        : undefined,
      // RT-3 — sent only when the operator actually touched the control, so an
      // ordinary run's body is byte-identical to a pre-RT one and the job's own
      // pin resolves server-side. When they DID touch it, "" is forwarded as-is:
      // that is the per-run unpin, not an absent value.
      runnerTagOverride !== null ? { runnerTag: runnerTagOverride } : undefined,
    );
    if (res.ok) {
      onDone();
    } else if (res.code === "invalid_executor") {
      // Force the runner choice and keep the dialog open so the operator can retry.
      setExecutor("runner");
      setRunErr(res.message ?? "SSH cannot run this job type — use the runner executor.");
    } else if (res.code === "key_binding_requires_runner") {
      // KB — the run RESOLVED to the ssh executor and the job binds an SSH key the
      // executor cannot deliver. Unlike invalid_executor this does NOT force the
      // runner choice: a Secret binding is the other legitimate way out, and the
      // server message names both. Keep the dialog open to retry either way.
      setRunErr(res.message ?? "This job binds an SSH key, which only a runner can deliver — choose the runner executor or bind the key as a Secret.");
    } else if (res.code === "prompt_required") {
      // The server refused a `block` job. Keep the dialog open with the message —
      // which names the offending inputs — so the operator can fill them in place.
      setRunErr(res.message ?? "This job requires every required run input to have a value.");
    } else if (res.code) {
      // Any other rejection (e.g. 409 concurrency, 422 scope_membership / group_membership)
      // — show the server message inline and keep the dialog open to retry.
      setRunErr(res.message ?? "Run could not be queued.");
    } else {
      // Network/unknown failure already shown at page level — close the dialog.
      onDone();
    }
  };

  // RV — the dialog is three stacked sections: Required variables · Where it
  // runs · Advanced options. Each is a Disclosure whose collapsed summary keeps
  // its one un-hideable fact visible (RD1's rule, now applied three times).
  //
  // Visited-gating (RV-Q1): when the job declares run inputs, the Run button
  // stays disabled until "Where it runs" has been open at least once — the
  // operator must at least glance at where this is going before committing.
  // "Visited" is has-been-open-ever, not is-open-now: closing a section after
  // reading it is fine. A job with NO declared inputs keeps its one-click run
  // (RV-Q2) — the gate exists for runs that already demand operator attention.
  const [varsOpen, setVarsOpen] = useState(true);
  // RU-5 — the per-run override subgroup inside "Run inputs". Collapsed by
  // default; the fold above it stays the section, this is only a subgroup.
  const [envOpen, setEnvOpen] = useState(false);
  // RD5 — "Where it runs" split in two: Targets (which machines) and Method
  // (how we reach them). Each has its own open + visited state; the RV review
  // gate keys on Targets, because "where is this going" is the glance it exists
  // to force.
  const [targetsOpen, setTargetsOpen] = useState(false);
  const [methodOpen, setMethodOpen] = useState(false);
  const [advancedOpen, setAdvancedOpen] = useState(false);
  // AR — "When to run": null = now; an ISO instant defers the run. The section
  // is a fourth Disclosure; a set instant is a deviation, so it always passes
  // through the confirmation window.
  const [whenOpen, setWhenOpen] = useState(false);
  const [runAt, setRunAt] = useState<string | null>(null);
  const [targetsVisited, setTargetsVisited] = useState(false);
  const [methodVisited, setMethodVisited] = useState(false);
  const [advancedVisited, setAdvancedVisited] = useState(false);
  // RU-8/RU-Q5 — "When to run" has existed since AR but was never recorded: a
  // deferred run reached the audit only as a `confirmation` deviation, so
  // "did the operator look at the timing?" was unanswerable for a run that kept
  // the default. Tracked like the others; the key is additive on the wire.
  const [whenVisited, setWhenVisited] = useState(false);
  useEffect(() => {
    if (targetsOpen) setTargetsVisited(true);
  }, [targetsOpen]);
  useEffect(() => {
    if (methodOpen) setMethodVisited(true);
  }, [methodOpen]);
  useEffect(() => {
    if (whenOpen) setWhenVisited(true);
  }, [whenOpen]);
  // RU-5 — "Run inputs" now renders for every job (it carries the overrides
  // editor), so its default-open has to become conditional rather than constant:
  // open when the job declares prompts, collapsed when the fold holds nothing but
  // the override subgroup. Keyed on the LENGTH so it fires once, when the detail
  // fetch resolves — an operator's own toggle afterwards is never overridden.
  useEffect(() => {
    setVarsOpen(declaredPrompts.length > 0);
  }, [declaredPrompts.length]);

  // RB-29: open "Where it runs" up front when a scope MUST be chosen. The picker
  // already existed but was collapsed, so the one control that satisfies the new
  // rule was the one thing the user could not see. Opening it also marks the
  // section visited, which the RV review gate requires before the run is allowed.
  useEffect(() => {
    if (mustBindScope) setTargetsOpen(true);
  }, [mustBindScope]);

  // A single reachable scope is a prompt with one answer — preselect it rather
  // than make the operator restate what the system already knows.
  useEffect(() => {
    if (mustBindScope && !scope && scopes.length === 1 && scopes[0].scope) {
      setScope(scopes[0].scope);
    }
  }, [mustBindScope, scope, scopes]);
  useEffect(() => {
    if (advancedOpen) setAdvancedVisited(true);
  }, [advancedOpen]);
  const requireReview = declaredPrompts.length > 0;
  const reviewGate = requireReview && !targetsVisited;
  // An incompatible scope is a warning the operator has to see, so it opens the
  // fold for them — via an effect rather than by forcing `open`, which would leave
  // the toggle dead.
  useEffect(() => {
    if (incompatible) setTargetsOpen(true);
  }, [incompatible]);
  // T1.7 — the reference preflight. A reference that will not resolve in the
  // EFFECTIVE scope fails the run closed at dispatch, so it earns exactly the same
  // treatment as an incompatible scope: it shows in the collapsed summary and
  // springs the fold open. The check is owned HERE, not inside the fold's panel,
  // because Disclosure unmounts its children while collapsed — a check that only
  // runs once opened could never be what opens it.
  // V2-11 — the operator's per-run reference additions. State lives HERE (not in
  // the panel) for the same reason the preflight does: the fold unmounts its
  // children while collapsed, and a chosen addition must survive that.
  const [addedRefs, setAddedRefs] = useState<ReferenceBinding[]>([]);
  const preflight = useRunReferencePreflight({
    jobId: job.id ?? undefined,
    scriptRef: jobDetail.scriptRef,
    scope: effScope,
    added: addedRefs,
  });
  const unresolvedRefs = preflight.unresolved;
  useEffect(() => {
    if (unresolvedRefs > 0) setVarsOpen(true);
  }, [unresolvedRefs]);

  const namedOverrides = envRows.filter((r) => r.key.trim() !== "").length;
  // RP-3 — while the selection still IS the job's Auto resolution, say so; the
  // moment the operator picks the other card it is a concrete override.
  const executorWord = executor === "ssh" ? "SSH" : "Runner";
  const targetsSummary = [
    effScope || "no scope",
    canPickGroups && limitGroups && pickedGroups.length > 0
      ? `${pickedGroups.length} group${pickedGroups.length === 1 ? "" : "s"}`
      : canPickHosts && limitHosts && pickedHosts.length > 0
        ? `${pickedHosts.length} host${pickedHosts.length === 1 ? "" : "s"}`
        : scopeHosts.length > 0
          ? `all ${scopeHosts.length} host${scopeHosts.length === 1 ? "" : "s"}`
          : "",
  ]
    .filter(Boolean)
    .join(" · ");
  const methodSummary = [
    executorAuto && executor === defaultExecutor ? `Auto → ${executorWord}` : executorWord,
    sshUser.trim() ? `as ${sshUser.trim()}` : "",
    sshCredential ? `key ${sshCredential}` : "",
    // RT-3 — the collapsed line states the pin whenever there IS one, and states
    // an explicit per-run unpin too. The section's whole job is that the facts
    // which reinterpret a run stay visible while it is folded, and "this went
    // somewhere other than where the job says" is exactly such a fact.
    executor !== "ssh" && effectivePin ? `on ${effectivePin}` : "",
    executor !== "ssh" && runnerTagOverride === "" && jobPin !== "" ? "unpinned for this run" : "",
  ]
    .filter(Boolean)
    .join(" · ");

  // RV — the Advanced section's collapsed line. Check mode leads and stays
  // uppercase: a dry run reads as an ordinary run everywhere else, and the
  // stacked section summaries are always on screen, so this is where the one
  // fact that reinterprets the whole run stays visible while collapsed.
  const advancedSummary =
    [
      ansCheck ? "CHECK MODE" : "",
      // RU-6 — the raw pattern moved in here, so the collapsed line has to name it.
      // The summary already printed its EFFECTS (via the hosts phrase); a pattern
      // the operator can't see from the collapsed state is one they can't undo.
      rawLimitActive ? `--limit ${ansibleLimit.trim()}` : "",
      ansDiff ? "diff" : "",
      ansTagList.length > 0 ? `tags ${ansTagList.join(",")}` : "",
      ansSkipTagList.length > 0 ? `skip ${ansSkipTagList.join(",")}` : "",
      ansVerbosity > 0 ? `-${"v".repeat(ansVerbosity)}` : "",
      ansBecome || ansBecomeUser.trim() ? `become${ansBecomeUser.trim() ? ` ${ansBecomeUser.trim()}` : ""}` : "",
      Object.keys(ansExtraVarMap).length > 0
        ? `${Object.keys(ansExtraVarMap).length} extra-var${Object.keys(ansExtraVarMap).length === 1 ? "" : "s"}`
        : "",
    ]
      .filter(Boolean)
      .join(" · ") || "defaults";

  // RV — the variables section's collapsed line mirrors the panel's own meter.
  // RU-5 — and now also counts the per-run env overrides that moved in with it,
  // since the collapsed bar is the only thing on screen saying they exist.
  const outstandingCount = inputStates.filter((st) => st.unfilled || st.unconfirmed).length;
  const varsSummary =
    [
      inputStates.length > 0 ? `${inputStates.length - outstandingCount} of ${inputStates.length} ready` : "",
      namedOverrides > 0 ? `${namedOverrides} override${namedOverrides === 1 ? "" : "s"}` : "",
      // RD5 — references moved in here from "Where it runs": they are things the
      // run RECEIVES, and an unresolved one still has to show while collapsed.
      addedRefs.length > 0 ? `${addedRefs.length} reference${addedRefs.length === 1 ? "" : "s"} added` : "",
      unresolvedRefs > 0 ? `${unresolvedRefs} reference${unresolvedRefs === 1 ? "" : "s"} unresolved` : "",
    ]
      .filter(Boolean)
      .join(" · ") || "none declared";

  // RU-2 — the unfilled inputs are now named where they live: RunInputsPanel
  // derives the list from the same `states` it renders. The dialog only needs the
  // count (`unfilledCount`) for the recap fragment and the button label.

  // RC — one confirmation, always: every ad-hoc run goes through the
  // confirmation window, which states what will happen and names each setting
  // that deviates from the job's defaults against the default it replaced.
  // (RC-2, 2026-09-09: the original two-tier scheme confirmed a default-settings
  // run inline by pressing the armed Run button twice. Two presses of a button
  // labelled "Run now" is not being told anything — the window is.)
  //
  // What counts as a deviation is what the operator changed in THIS dialog.
  // The run-anyway escape for unfilled answers is deliberately not in the list:
  // it already carries its own explicit acknowledgment checkbox, and warning
  // twice for one act teaches operators to stop reading warnings.
  // FX-Q1 — the pause is NOT a deviation: deviations are settings the operator
  // changed in this dialog, and nobody changed this. It gets its own banner in
  // the confirmation window instead, and forces that window to appear.
  const isPaused = job.status === "paused";

  const deviations: { label: string; detail: string }[] = [];
  if (scope && scope !== (job.scope ?? "")) {
    deviations.push({ label: "Scope", detail: `${scope} — job default: ${job.scope || "(none)"}` });
  }
  if (!runnerOnly && executor !== defaultExecutor) {
    deviations.push({
      label: "Executor",
      detail: `${executorWord} — job default: ${executorAuto ? `Auto → ${defaultExecutor === "ssh" ? "SSH" : "Runner"}` : defaultExecutor === "ssh" ? "SSH" : "Runner"}`,
    });
  }
  if (canPickHosts && limitHosts && pickedHosts.length > 0 && !rawLimitActive) {
    deviations.push({ label: "Host subset", detail: `${pickedHosts.length} of ${scopeHosts.length}: ${pickedHosts.join(", ")}` });
  }
  if (canPickGroups && limitGroups && pickedGroups.length > 0 && !rawLimitActive) {
    deviations.push({ label: "Group subset", detail: pickedGroups.join(", ") });
  }
  if (rawLimitActive && executor === "runner" && job.type === "ansible") {
    deviations.push({ label: "Raw --limit", detail: ansibleLimit.trim() });
  }
  if (sshUser.trim() || sshCredential) {
    deviations.push({
      label: "Connect as",
      detail: [sshUser.trim() && `user ${sshUser.trim()}`, sshCredential && `key ${sshCredential}`].filter(Boolean).join(" · "),
    });
  }
  // RS-1.1 — a per-run runner pin IS a deviation. RT-3 (v1.3.2) landed after this
  // list was built and nobody extended it, so re-pinning a run to another tag —
  // or explicitly unpinning it from the job's — produced no row, never forced the
  // confirm window, and never disarmed the button. All three consumers of this
  // array missed it. "This went somewhere other than where the job says" is
  // exactly the class of fact the RT-3 collapsed-summary comment above says must
  // stay visible.
  //
  // Guarded on executor like that summary is: a pin is meaningless on an SSH run,
  // and the submit path only carries it for runner runs.
  if (executor !== "ssh" && runnerTagOverride !== null) {
    deviations.push({
      label: "Runner pin",
      detail: `${effectivePin || "unpinned"} — job default: ${jobPin || "(none)"}`,
    });
  }
  if (ansCheck) deviations.push({ label: "Check mode", detail: "dry run — applies nothing" });
  if (ansDiff) deviations.push({ label: "Diff", detail: "--diff" });
  if (ansTagList.length > 0) deviations.push({ label: "Tags", detail: `only: ${ansTagList.join(", ")}` });
  if (ansSkipTagList.length > 0) deviations.push({ label: "Skip tags", detail: ansSkipTagList.join(", ") });
  if (ansVerbosity > 0) deviations.push({ label: "Verbosity", detail: `-${"v".repeat(ansVerbosity)}` });
  if (ansBecome || ansBecomeUser.trim()) {
    deviations.push({ label: "Become", detail: ansBecomeUser.trim() ? `as ${ansBecomeUser.trim()}` : "root" });
  }
  if (Object.keys(ansExtraVarMap).length > 0) {
    deviations.push({ label: "Extra variables", detail: Object.keys(ansExtraVarMap).join(", ") });
  }
  if (namedOverrides > 0) {
    deviations.push({
      label: "Environment overrides",
      detail: envRows.filter((r) => r.key.trim() !== "").map((r) => r.key.trim()).join(", "),
    });
  }
  if (addedRefs.length > 0) {
    deviations.push({ label: "References added", detail: addedRefs.map((b) => b.name).join(", ") });
  }
  if (runAt) {
    deviations.push({
      label: "Scheduled",
      detail: `runs ${fmtInAppZone(runAt, { weekday: "short", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })} (app zone) — default: now`,
    });
  }

  // RC-2 — the confirmation window. It reads LIVE state, so there is no
  // signature to keep stale-proof: whatever it shows is what Confirm submits.
  const [confirmStage, setConfirmStage] = useState(false);

  // The one-sentence targeting phrase that leads the confirmation window. It reflects the EFFECTIVE selection — a subset or
  // raw --limit must not read as "all N hosts" while the row below says 1 of N.
  const hostsPhrase =
    rawLimitActive && executor === "runner" && job.type === "ansible"
      ? `--limit ${ansibleLimit.trim()} in ${effScope || "no scope"}`
      : canPickHosts && limitHosts && pickedHosts.length > 0
        ? `${pickedHosts.length} selected host${pickedHosts.length === 1 ? "" : "s"} in ${effScope}`
        : canPickGroups && limitGroups && pickedGroups.length > 0
          ? `${pickedGroups.length} group${pickedGroups.length === 1 ? "" : "s"} in ${effScope}`
          : scopeHosts.length > 0
            ? `all ${scopeHosts.length} host${scopeHosts.length === 1 ? "" : "s"} in ${effScope}`
            : effScope
              ? `its targets in ${effScope}`
              : "its targets";

  // RU-1 — the remaining recap fragments, and the two "is this a deviation?" tests
  // that decide whether a fragment is muted or accented. Both mirror the matching
  // entries in `deviations` above rather than inventing their own rule, so the line
  // and the confirmation window can't disagree about what counts as changed.
  const whenPhrase = runAt
    ? `at ${fmtInAppZone(runAt, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })}`
    : "now";
  const recapSubsetActive =
    rawLimitActive ||
    (canPickHosts && limitHosts && pickedHosts.length > 0) ||
    (canPickGroups && limitGroups && pickedGroups.length > 0);
  const recapExecutorChanged = !runnerOnly && executor !== defaultExecutor;

  // RS-3 — the rail's summary. Built from the SAME derived values the recap
  // sentence and `deviations` read, so all three answer from one state and
  // cannot disagree (RU-1). The function is pure and lives in
  // views/jobs/runSummary.ts; everything below is just the wiring.
  const runSummary = buildRunSummary({
    inputStates,
    envRows,
    addedRefs,
    scope: effScope,
    jobScope: job.scope ?? "",
    hostsPhrase,
    targetsChanged: recapSubsetActive,
    // The names behind the phrase — SAME branch order as hostsPhrase above, so
    // the detail line can never name a different selection than the count the
    // phrase states. A raw --limit branch passes none (the phrase carries the
    // literal itself).
    targetNames:
      rawLimitActive && executor === "runner" && job.type === "ansible"
        ? []
        : canPickHosts && limitHosts && pickedHosts.length > 0
          ? pickedHosts
          : canPickGroups && limitGroups && pickedGroups.length > 0
            ? pickedGroups
            : [],
    executorWord: executorAuto && !runnerOnly ? `Auto → ${executorWord}` : executorWord,
    executorChanged: recapExecutorChanged,
    sshUser,
    sshCredential,
    // From `jobDetail`, never `job` — the identity fields are detail-only, the
    // same reason `jobPin` above reads from it. Without these the rail showed a
    // job-declared identity as nothing at all.
    jobSshUser: jobDetail.sshUser ?? "",
    jobSshCredential: jobDetail.sshCredential ?? "",
    identityCapable,
    effectivePin,
    jobPin,
    pinApplies: executor !== "ssh",
    pinChanged: runnerTagOverride !== null,
    whenPhrase,
    deferred: !!runAt,
    ansCheck,
    ansDiff,
    ansTagList,
    ansSkipTagList,
    ansVerbosity,
    ansBecome,
    ansBecomeUser,
    ansExtraVarMap,
  });

  // RC-2 — Run always opens the confirmation window. FX-Q1/FX-C's rule (a
  // paused job must be confirmed through the window, because the override is
  // only defensible if the operator was told) is now simply the general case.
  const onRunClick = () => setConfirmStage(true);

  // RU-13/RU-14 — the commit zone renders in one of two places: as Modal's
  // `footer` (stacked, <1200px) or inside the right-hand rail (≥1200px). The
  // pieces are built ONCE here and composed twice below, so the two layouts
  // cannot drift — same recap, same escape, same buttons, same gating.

  const errorBanner = runErr && (
    <div style={{ fontSize: c.fontSm, color: c.danger, background: c.dangerBg, border: `1px solid ${c.danger}40`, borderRadius: c.radiusSurface, padding: "8px 10px", marginBottom: 10 }}>
      {runErr}
    </div>
  );

  // RU-1 — the recap line. Built from the same live state submit() reads, so
  // it can never disagree with what the run will actually do (the same
  // reasoning as the deviation window's lead). It answers "what will this do"
  // at ALL times, not only in the confirmation window — and it lives outside the
  // scrollport in both layouts, so it never scrolls away. Fragments that carry
  // a deviation from the job's defaults take the warning or info accent, so the
  // line also whispers "you changed something".
  const recapLine = (
    <div
      style={{
        display: "flex",
        flexWrap: "wrap",
        alignItems: "baseline",
        gap: 6,
        fontSize: c.fontSm,
        lineHeight: 1.5,
        color: c.textSec,
        marginBottom: 10,
      }}
    >
      <strong style={{ color: c.text, fontWeight: 600 }}>{job.name}</strong>
      <span style={{ color: c.textMuted }}>·</span>
      <span style={{ color: recapSubsetActive ? c.warning : c.textSec, fontWeight: recapSubsetActive ? 600 : 400 }}>
        {hostsPhrase}
      </span>
      <span style={{ color: c.textMuted }}>·</span>
      <span style={{ color: recapExecutorChanged ? c.warning : c.textSec, fontWeight: recapExecutorChanged ? 600 : 400 }}>
        via {executorWord}
      </span>
      <span style={{ color: c.textMuted }}>·</span>
      <span style={{ color: runAt ? c.info : c.textSec, fontWeight: runAt ? 600 : 400 }}>{whenPhrase}</span>
      {ansCheck && (
        <>
          <span style={{ color: c.textMuted }}>·</span>
          <span style={{ color: c.info, fontWeight: 600 }}>check mode</span>
        </>
      )}
      {unfilledCount > 0 && (
        <>
          <span style={{ color: c.textMuted }}>·</span>
          <span style={{ color: c.warning, fontWeight: 600 }}>
            {unfilledCount === 1 ? "1 answer missing" : `${unfilledCount} answers missing`}
          </span>
        </>
      )}
    </div>
  );

  // T1.7/JR-Q4b — an unfilled required input gates the button, but never becomes a
  // dead end: the operator can always proceed deliberately. UDV4's "a required
  // prompt never blocks the run" holds — the server still accepts it either way.
  // RU-2 — the consequence prose lives in the answers panel, beside the rows it
  // describes. Only the escape sits in the commit zone, compressed to one line:
  // it ungates the Run button, so it has to be reachable without scrolling (the
  // RD1 lesson). Blocking jobs have no escape, so this renders nothing for them.
  const escapeLine = unfilledCount > 0 && !blocking && (
    <label style={{ display: "flex", alignItems: "center", gap: 8, cursor: "pointer", fontSize: c.fontSm, fontWeight: 600, color: c.warning, marginBottom: 10 }}>
      <input type="checkbox" checked={runAnyway} onChange={(e) => setRunAnyway(e.target.checked)} />
      Run without {unfilledCount === 1 ? "it" : "them"}
    </label>
  );

  const actionRow = (
    <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
      <Btn small disabled={busy} onClick={onCancel}>Cancel</Btn>
      <Btn small primary disabled={busy || scopeMissing || subsetInvalid || groupSubsetInvalid || limitConflict || !!reservedExtraVar || inputsBlock || reviewGate} onClick={onRunClick}>
        {busy
          ? "Queuing…"
          : /* RB-29: name the missing thing rather than sitting greyed out. The
                 server would answer 403 scope_required here, so blocking with an
                 explanation is strictly better than letting the request go. */
              scopeMissing
              ? "Choose a scope to run against"
              : unfilledCount > 0 && inputsBlock
              ? blocking
                ? `${unfilledCount} answer${unfilledCount === 1 ? "" : "s"} still needed`
                : `Run without ${unfilledCount} answer${unfilledCount === 1 ? "" : "s"}`
              : reviewGate
                ? "Review where it runs"
                : unfilledCount > 0
                  ? `Run without ${unfilledCount} answer${unfilledCount === 1 ? "" : "s"}`
                  : "Run now"}
      </Btn>
    </div>
  );

  // FX-10/F-3 — the stacked commit zone, handed to Modal's `footer`. This dialog
  // is where the sticky bar was invented (RD1) and where FX-6 found what sticky
  // costs — a bottom-pinned bar grows UPWARD over the content once it outgrows
  // the scrollport. As a footer it lives outside the scrollport entirely.
  const commitBar = (
    <>
      {errorBanner}
      {recapLine}
      {escapeLine}
      {actionRow}
    </>
  );

  // RU-14/RS-3 — the rail: the same commit zone, plus the one thing the stacked
  // layout cannot afford — a LIVE account of the run. Until RS-3 this listed
  // deviations only, so a stock job with a filled prompt showed an empty rail:
  // the surface whose job is to say what will happen said nothing exactly when
  // nothing unusual was happening. It now renders the grouped summary, which
  // reads from the same state `submit()` does, so ticking check mode or typing
  // an answer changes it beside the operator's hand while they are still
  // editing, not after the first press of Run.
  //
  // The confirm window is UNTOUCHED (RU-Q7): it still lists deviations only,
  // still writes the `confirmation` audit key, and still fences the
  // muscle-memory path. Its question is "what did you change" — the rail's is
  // "what will this use", and conflating them was what left the rail empty.
  const summaryRow = (r: SummaryRow) => {
    // The border is what makes "changed" pop against "stated" — one accent axis,
    // as the recap sentence already uses. A quiet row renders flat: a rail where
    // every row is a card is a rail where nothing stands out.
    const accented = r.accent !== undefined;
    const tone = r.accent === "info" ? c.info : c.warning;
    return (
      <div
        key={r.key}
        style={
          accented
            ? { padding: "6px 8px", background: c.panel2, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface }
            : { padding: "2px 0" }
        }
      >
        {(r.label !== "" || r.note) && (
          <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: accented ? tone : c.textMuted, textTransform: "uppercase", letterSpacing: 0.7 }}>
            {r.label}
            {r.note && (
              <span style={{ color: accented ? tone : c.textMuted, opacity: accented ? 0.85 : 1, fontWeight: 500 }}>
                {r.label !== "" ? " · " : ""}
                {r.note}
              </span>
            )}
          </div>
        )}
        {/* title carries the untruncated value — buildRunSummary caps the
            display at 64 chars for a 300px column, and the full string is the
            thing an operator may actually need to read back. */}
        <div title={r.value} style={{ fontSize: c.fontXs, color: accented ? c.text : c.textSec, fontFamily: c.mono, wordBreak: "break-word" }}>
          {r.value}
        </div>
        {/* The itemization line — quieter than the value it expands on, so
            "2 groups" stays the headline and the names read as its footnote. */}
        {r.detail && (
          <div title={r.detail} style={{ fontSize: c.fontXs, color: c.textMuted, fontFamily: c.mono, wordBreak: "break-word" }}>
            {r.detail}
          </div>
        )}
      </div>
    );
  };

  const railPanel = (
    <div style={{ display: "flex", flexDirection: "column", minHeight: "100%" }}>
      <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textMuted, letterSpacing: 0.7, marginBottom: 8, textTransform: "uppercase" }}>
        This run
      </div>
      {/* The recap sentence STAYS, unchanged and above: it is the one-line
          answer, and the summary below is its itemization. Both read the same
          state, so they cannot disagree. */}
      {recapLine}
      <div style={{ display: "flex", flexDirection: "column", gap: 10, marginTop: 6, marginBottom: 10 }}>
        {runSummary.map((g) => (
          <div key={g.key} style={{ display: "flex", flexDirection: "column", gap: 4 }}>
            <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textMuted, textTransform: "uppercase", letterSpacing: 0.7, opacity: 0.75 }}>
              {g.title}
            </div>
            {g.rows.map(summaryRow)}
          </div>
        ))}
      </div>
      {/* The commit controls sit at the BOTTOM of the rail, aligned with where
          the stacked footer puts them — the summary grows downward from the
          recap, the buttons stay anchored, and the rail's own overflow
          (ui.tsx) absorbs a long one. */}
      <div style={{ marginTop: "auto", paddingTop: 12 }}>
        {errorBanner}
        {escapeLine}
        {actionRow}
      </div>
    </div>
  );

  // RC — the deviation confirmation window's own footer: Back returns to the
  // form untouched; Confirm commits. The submit failure paths drop back to the
  // form (confirmStage is cleared before submitting), so a 409/422 lands on the
  // familiar error banner rather than inside the recap.
  const confirmBar = (
    <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
      <Btn small disabled={busy} onClick={() => setConfirmStage(false)}>← Back</Btn>
      <Btn
        small
        primary
        disabled={busy}
        onClick={() => {
          setConfirmStage(false);
          void submit(true);
        }}
      >
        {busy ? "Queuing…" : "Confirm & run"}
      </Btn>
    </div>
  );

  // RC-3 — the confirm window's two lists share one row shape: a fixed label
  // column, hairline-separated, so the eye reads down the values. A `note` is
  // the provenance ("default", "you") or the problem ("required — not filled");
  // an accented row carries the warning tone on its note, the same axis the
  // rail uses, so a default reads quiet and an operator-supplied value pops.
  const confirmRow = (key: string, label: string, value: string, i: number, note?: string, accent?: SummaryAccent) => (
    <div
      key={key}
      role="listitem"
      style={{ display: "grid", gridTemplateColumns: "minmax(110px, max-content) 1fr", gap: 14, alignItems: "baseline", padding: "9px 12px", borderTop: i === 0 ? undefined : `1px solid ${c.borderLight}` }}
    >
      <span style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textMuted, textTransform: "uppercase", letterSpacing: 0.7, whiteSpace: "nowrap" }}>
        {label}
      </span>
      <span style={{ fontSize: c.fontSm, color: c.text, fontFamily: c.mono, wordBreak: "break-word", lineHeight: 1.45 }}>
        {value}
        {note && (
          <span style={{ fontFamily: c.sans, color: accent === "warning" ? c.warning : accent === "info" ? c.info : c.textMuted, marginLeft: 8 }}>
            ({note})
          </span>
        )}
      </span>
    </div>
  );
  const confirmEyebrow = (text: string) => (
    <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textSec, textTransform: "uppercase", letterSpacing: 0.7, marginBottom: 6 }}>
      {text}
    </div>
  );
  // Declared prompts only (the `prompt:` rows of the summary's Inputs group);
  // the group's env-override and reference rows are already deviations.
  const confirmInputRows = (runSummary.find((g) => g.key === "inputs")?.rows ?? []).filter((r) =>
    r.key.startsWith("prompt:"),
  );

  if (confirmStage) {
    return (
      <Modal title={`Confirm run — ${runTitleName}`} wide onClose={onCancel} footer={confirmBar}>
        {/* RC — the deviation window. One sentence of what happens, then each
            deviation against the default it replaces. Everything here reads
            LIVE state, so it can never disagree with what Confirm submits. */}
        <div style={{ fontSize: c.fontSm, color: c.text, marginBottom: 12 }}>
          <strong>{job.name}</strong> runs on {hostsPhrase} via{" "}
          <strong>{executor === "ssh" ? "the in-app SSH executor" : "a runner agent"}</strong>
          {deviations.length > 0 ? (
            <>
              {" "}— with{" "}
              {deviations.length === 1 ? "this setting" : `these ${deviations.length} settings`} changed from the
              job's defaults:
            </>
          ) : (
            "."
          )}
        </div>
        {/* FX-Q1 — the pause, named. This is the whole reason a paused job is
            allowed past the gate at all: the operator overrides it knowingly. */}
        {isPaused && (
          <div style={{ fontSize: c.fontSm, color: c.text, background: c.warningBg, border: `1px solid ${c.warning}40`, borderRadius: c.radiusSurface, padding: "8px 10px", marginBottom: 12 }}>
            <strong style={{ color: c.warning }}>This job is paused or disabled.</strong> Running it now overrides
            that for this run only — the job stays as it is afterwards, and its schedule stays stopped. Automated
            starts remain blocked meanwhile: its schedule, a reaction, a file arrival, and (when it is paused) an
            API token.
          </div>
        )}
        {/* RC-3 — every declared input, ALWAYS, defaults included. The window
            used to list deviations only, so a job whose answers all came from
            defaults confirmed with no inputs on screen at all — and a default
            is exactly the value an operator most needs to see restated before
            it is sent, because nobody typed it in this dialog. The rows come
            from the same summary model the rail renders (runSummary.ts), so
            the two surfaces cannot disagree about a value or its provenance.
            Env overrides and references stay in the deviations list below —
            they are changes, and listing them twice teaches skipping. */}
        {confirmInputRows.length > 0 && (
          <>
            {confirmEyebrow("Inputs")}
            <div
              role="list"
              aria-label="Inputs this run will use"
              style={{ maxHeight: "30vh", overflowY: "auto", scrollbarGutter: "stable", background: c.panel2, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, marginBottom: 12 }}
            >
              {confirmInputRows.map((r, i) =>
                confirmRow(r.key, r.label, r.value, i, r.note, r.accent),
              )}
            </div>
            {confirmEyebrow(deviations.length === 0 ? "Settings" : "Changed settings")}
          </>
        )}
        {/* One bounded list, not a stack of cards: a fixed label column lines
            the values up so the eye reads down them, hairlines separate rows,
            and the box scrolls on its own past ~40vh so a long list never
            pushes the Confirm button off the panel or hides an entry. */}
        {deviations.length === 0 && (
          <div style={{ fontSize: c.fontSm, color: c.textSec, padding: "9px 12px", background: c.panel2, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface }}>
            No settings changed — this run uses the job exactly as configured.
          </div>
        )}
        {deviations.length > 0 && (
          <div
            role="list"
            aria-label="Settings changed from the job's defaults"
            style={{ maxHeight: "40vh", overflowY: "auto", scrollbarGutter: "stable", background: c.panel2, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface }}
          >
            {deviations.map((d, i) => confirmRow(d.label, d.label, d.detail, i))}
          </div>
        )}
        {ansCheck && (
          <div style={{ fontSize: c.fontSm, color: c.text, background: c.infoBg, border: `1px solid ${c.info}40`, borderRadius: c.radiusSurface, padding: "8px 10px", marginTop: 12 }}>
            <strong style={{ color: c.info }}>Check mode</strong> — this is a dry run: ansible reports what would change
            and applies nothing.
          </div>
        )}
      </Modal>
    );
  }

  return (
    // RU-13 — both layouts are passed; Modal picks by viewport. The confirm
    // window above deliberately stays footer-only (RU-Q7: nothing in this phase
    // may change its behavior).
    <Modal title={`Run ${runTitleName}`} wide onClose={onCancel} footer={commitBar} rail={railPanel}>
      {/* AN-3 — operator context above the sections: is this critical, and who
          owns it. Fed by `jobDetail` (the dialog's own detail fetch) rather than
          the list row it was opened from, so the note's first line is available.
          Deliberately NOT a fourth reviewedSections gate: the RV gate exists for
          inputs the operator must SUPPLY, and making context a visited-gate is
          how you train people to click through gates. */}
      <AnnotationBanner value={annotationOf(jobDetail)} />

      {/* T1.1/RD1/RV — the answers come FIRST, now as the first of four
          sections. It defaults OPEN when the job declares prompts (the only
          section that does): that is the one part of the dialog which genuinely
          needs the operator, so it is on screen — and thereby "visited" — from
          the first paint. The panel owns its own count, meter, filter and
          pagination (views/jobs/RunInputs.tsx).
          RU-4 — titled "Run inputs", matching the job-detail panel and the
          filename. The old "Required variables" was wrong twice over: the
          section counts optional inputs too, and it now also holds the per-run
          environment overrides.
          RU-5 — which is why it renders even with no declared prompts: the
          overrides editor lived in Advanced and must not disappear with the
          panel. The audit key stays `variables` (RU-8: titles rename, keys don't). */}
      <Disclosure
        title="Inputs"
        summary={varsSummary}
        open={varsOpen}
        onToggle={() => setVarsOpen((o) => !o)}
        tone="primary"
      >
        {declaredPrompts.length > 0 && (
          <RunInputsPanel
            states={inputStates}
            promptVals={promptVals}
            setPrompt={setPrompt}
            confirmDefault={confirmDefault}
            embedded
            blocking={blocking}
          />
        )}

        {/* RU-5 — the F1 per-run env overrides, moved out of Advanced. They are an
            input to the run in exactly the way the declared prompts are, and
            `resolveInput`'s precedence (an override OUTRANKS a prompt answer of
            the same name) was invisible while the two sat in different sections.
            Adjacency is the fix: the chain now reads top-to-bottom on screen.
            Collapsed by default — this is still the rarer of the two.
            `envRows` state stays hoisted in RunDialog: Disclosure unmounts its
            children, and a typed override must survive closing the subgroup. */}
        <div style={{ marginTop: declaredPrompts.length > 0 ? 12 : 0 }}>
          {/* RU-12 — quiet, and doubly earned here: this bar is NESTED, and until
              the tones existed it rendered identically to the top-level sections
              below it — "Add an override" and "When to run" were visually the same
              rank of thing. */}
          <Disclosure
            title="Add an override"
            summary={namedOverrides > 0 ? `${namedOverrides} set` : "none"}
            open={envOpen}
            onToggle={() => setEnvOpen((o) => !o)}
            tone="quiet"
          >
            <div style={{ fontSize: c.fontSm, color: c.textSec, marginBottom: 8 }}>
              Applied to this run only, on top of any schedule env. An override wins over a declared input of the same
              name.
            </div>
            <EnvRowsEditor rows={envRows} onChange={setEnvRows} addLabel="+ Add variable" />
          </Disclosure>
        </div>

<div style={{ marginTop: 12 }}>
        {/* T1.7/G6 — reference preflight against the EFFECTIVE scope. This is the only
            surface that can do it: a job whose own scope is "All" can be run against a
            specific scope with the picker above, and references resolve against the
            RUN's scope, so job detail cannot predict the answer and this can. It sits
            directly under the scope picker because that control is what changes it. */}
        <ReferencePreflightPanel
          state={preflight}
          scope={effScope}
          added={addedRefs}
          onAdd={(b) => setAddedRefs((prev) => (prev.some((x) => x.kind === b.kind && x.name === b.name) ? prev : [...prev, b]))}
          onRemove={(b) => setAddedRefs((prev) => prev.filter((x) => !(x.kind === b.kind && x.name === b.name)))}
          executor={executor}
        />
        </div>

        {/* T7.1/RU-5 — one plaintext caveat for the whole section. It used to be
            stated twice, once here and once in the overrides helper in Advanced;
            now that both surfaces are in this fold, it covers both from the bottom.
            Making inputs prominent invites pasting credentials. */}
        <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 10 }}>
          Plain text, visible in the run log — never paste a password or key. Store it as a Secret and reference{" "}
          <code style={{ fontFamily: c.mono }}>CRONOMICON_SECRET_&lt;name&gt;</code>.
        </div>
      </Disclosure>

      {/* FX-6 — targeting sits in NORMAL FLOW, directly under the answers. It used to
          live inside the pinned commit bar below, on the reasoning that an operator
          must be able to confirm they are about to hit Production without scrolling.
          That held only while it was collapsed: opening it made the pinned block
          taller than the modal's scrollport, and a sticky element pinned by its
          BOTTOM edge grows upward — so the opaque bar painted straight over "Answers
          this run needs". The collapsed summary still states scope and executor, so
          the fact that must never be hidden is still never hidden; only the editing
          form scrolls, which is what an operator deliberately editing targeting
          expects.

          RU-11 — from here down, `helperMode="engaged"` splits the helper prose two
          ways, and the split is by CONSEQUENCE, not by length:
            · what the control DOES (executor resolution, connect-as narrative,
              host/group semantics, tags, verbosity, become, check mode, the raw
              --limit description) → engaged: shown on focus or when the control is
              non-default, hidden while it sits at its default. Nobody needs a
              paragraph explaining a checkbox they haven't touched.
            · what could GO WRONG or LEAK (both plaintext-secret caveats, the
              reserved-extra-var guard, the subset/group-subset and limit-conflict
              verdicts, the RB-26 unscoped-run wording, the incompatible-scope
              warning) → always. Suppressing a warning until someone engages with the
              control is exactly backwards: the operator who most needs it is the one
              who isn't looking.
          Nothing is deleted; the same words appear the moment the field is engaged.
          The danger tone is unsuppressible in FormField itself, so a validation
          verdict cannot be hidden by a call site that opts into "engaged" and gets
          the classification wrong. */}
      <div style={{ marginTop: 14 }}>
        <Disclosure
          title="Targets"
          summary={targetsSummary}
          open={targetsOpen}
          onToggle={() => setTargetsOpen((o) => !o)}
        >
      {/* RU-7 — two quiet subheadings, not two folds. RU-Q3 kept the executor and
          Connect-as in this section (they answer "where does this run"), so the
          length is mitigated by grouping rather than by relocation: TARGETING is
          which machines, CONNECTION is how we reach them. The executor cards used
          to sit BETWEEN the reference preflight and the host chips, splitting
          targeting in half; connection now follows targeting whole. */}
      <div style={{ fontSize: c.fontXs, marginTop: 2, marginBottom: 10 }}>
        <DocLink href={DOC_LINKS.runTargeting}>How targeting decides a run&rsquo;s reach</DocLink>
      </div>
      {mustBindScope && (
        <div style={{ fontSize: c.fontSm, color: c.textSec, background: c.panel2, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, padding: "8px 10px", marginBottom: 8 }}>
          This job declares no scope of its own, so choose the one to run it against.
          The run targets that scope's hosts and its department's runners.
        </div>
      )}
      <FormField label={mustBindScope ? "Target scope (required)" : "Target scope"}>
        <select value={scope} onChange={(e) => setScope(e.target.value)} style={{ ...inputStyle(), cursor: "pointer", marginBottom: 8 }}>
          {/* RB-26: for an unscoped job the empty value is not a "default", it is
              the unbound/general-pool run — which only an unrestricted operator may
              perform. Say so rather than letting it read as a harmless default. */}
          <option value="" disabled={mustBindScope}>
            {job.scope
              ? `Job default (${job.scope})`
              : unrestricted
                ? "No scope — run unbound on the general pool"
                : "Choose a scope…"}
          </option>
          {scopes.map((s) => (
            <option key={s.id ?? s.scope} value={s.scope ?? ""}>
              {s.scope}
              {s.capability?.types?.length ? ` — ${s.capability.types.join(", ")}` : ""}
            </option>
          ))}
        </select>
      </FormField>
      {incompatible && (
        <div style={{ fontSize: c.fontSm, color: c.warning, background: c.warningBg, border: `1px solid ${c.warning}30`, borderRadius: c.radiusSurface, padding: "8px 10px", marginBottom: 8 }}>
          ⚠ {scope} does not declare <strong>{job.type}</strong> support. The run is allowed but may stay queued waiting for a capable runner.
        </div>
      )}


      {/* F2/RP-1 — host subset within the bound scope, offered for BOTH executors.
          What the selection actually does per run type lives in the helper line. */}
      {canPickHosts && (
        <FormField
          label="Hosts"
          helperMode="engaged"
          active={limitHosts}
          helperTone={subsetInvalid ? "danger" : undefined}
          helper={
            !limitHosts
              ? job.type === "terraform"
                ? "Terraform decides its own targets from its configuration; the scope chooses which runner takes the run."
                : executor === "ssh"
                  ? `Runs on all ${scopeHosts.length} host${scopeHosts.length === 1 ? "" : "s"} in ${effScope}.`
                  : job.type === "ansible"
                    ? `Targets the whole ${effScope} inventory (${scopeHosts.length} host${scopeHosts.length === 1 ? "" : "s"}); the playbook's hosts pattern applies.`
                    : `Runs on all ${scopeHosts.length} host${scopeHosts.length === 1 ? "" : "s"} in ${effScope}.`
              : subsetInvalid
                ? "Select at least one host, or untick to run on the whole scope."
                : executor === "ssh"
                  ? `SSH: connects to the ${pickedHosts.length} selected host${pickedHosts.length === 1 ? "" : "s"}.`
                  : job.type === "ansible"
                    ? `Ansible: passes the ${pickedHosts.length} selected host${pickedHosts.length === 1 ? "" : "s"} as --limit.`
                    : job.type === "terraform"
                      ? "Recorded on the run for audit — terraform does not consume host targeting."
                      : `Runner: limits the run to the ${pickedHosts.length} selected host${pickedHosts.length === 1 ? "" : "s"} (honored by cronomicon-inventory runners).`
          }
        >
          <label
            style={{ display: "flex", alignItems: "center", gap: 8, fontSize: c.fontSm, cursor: rawLimitActive ? "not-allowed" : "pointer", opacity: rawLimitActive ? 0.6 : 1 }}
            title={rawLimitActive ? "Clear the raw --limit pattern in Advanced to pick hosts." : undefined}
          >
            <input type="checkbox" checked={limitHosts} disabled={rawLimitActive} onChange={(e) => setLimitHosts(e.target.checked)} />
            Limit to specific hosts in <strong>{effScope}</strong> ({scopeHosts.length} available)
          </label>
          {limitHosts && (
            <div style={{ display: "flex", flexWrap: "wrap", gap: 6, marginTop: 8 }}>
              {scopeHosts.map((h) => {
                const on = pickedHosts.includes(h);
                return (
                  <label key={h} style={{ display: "flex", alignItems: "center", gap: 6, fontSize: c.fontSm, border: `1px solid ${on ? c.primary : c.border}`, background: on ? `${c.primary}18` : "transparent", borderRadius: c.radiusChip, padding: "4px 8px", cursor: "pointer" }}>
                    <input type="checkbox" checked={on} onChange={() => toggleHost(h)} />
                    {h}
                  </label>
                );
              })}
            </div>
          )}
        </FormField>
      )}

      {/* M3 — group subset within the bound scope (both executors). RP-2 — the
          helper is keyed on run type, not just executor: only ansible turns the
          selection into --limit. */}
      {canPickGroups && (
        <FormField
          label="Groups"
          helperMode="engaged"
          active={limitGroups}
          helperTone={groupSubsetInvalid ? "danger" : undefined}
          helper={
            !limitGroups
              ? "Target whole inventory groups instead of individual hosts."
              : groupSubsetInvalid
                ? "Select at least one group, or untick to run on the whole scope."
                : executor === "ssh"
                  ? `SSH: expands ${pickedGroups.length} group${pickedGroups.length === 1 ? "" : "s"} to member hosts.`
                  : job.type === "ansible"
                    ? `Ansible: passes ${pickedGroups.length} group${pickedGroups.length === 1 ? "" : "s"} as --limit.`
                    : job.type === "terraform"
                      ? "Recorded on the run for audit — terraform does not consume group targeting."
                      : `Runner: targets the ${pickedGroups.length} selected group${pickedGroups.length === 1 ? "" : "s"}' member hosts.`
          }
        >
          <label
            style={{ display: "flex", alignItems: "center", gap: 8, fontSize: c.fontSm, cursor: rawLimitActive ? "not-allowed" : "pointer", opacity: rawLimitActive ? 0.6 : 1 }}
            title={rawLimitActive ? "Clear the raw --limit pattern in Advanced to pick groups." : undefined}
          >
            <input type="checkbox" checked={limitGroups} disabled={rawLimitActive} onChange={(e) => setLimitGroups(e.target.checked)} />
            Limit to inventory groups in <strong>{effScope}</strong> ({scopeGroups.length} available)
          </label>
          {limitGroups && (
            <div style={{ display: "flex", flexWrap: "wrap", gap: 6, marginTop: 8 }}>
              {scopeGroups.map((g) => {
                const on = pickedGroups.includes(g.name ?? "");
                return (
                  <label
                    key={g.name}
                    style={{ display: "flex", alignItems: "center", gap: 6, fontSize: c.fontSm, border: `1px solid ${on ? c.primary : c.border}`, background: on ? `${c.primary}18` : "transparent", borderRadius: c.radiusChip, padding: "4px 8px", cursor: "pointer" }}
                  >
                    <input type="checkbox" checked={on} onChange={() => toggleGroup(g.name ?? "")} />
                    {g.name} <span style={{ color: c.textSec }}>({g.hostCount ?? 0})</span>
                  </label>
                );
              })}
            </div>
          )}
        </FormField>
      )}

        </Disclosure>
      </div>

      {/* RD5 — Method: HOW the run reaches its targets. Executor, then the runner
          pin, then Connect as — "which kind of executor", "which runner", "as
          whom", in that order. Formerly the Connection half of "Where it runs". */}
      <div style={{ marginTop: 14 }}>
        <Disclosure
          title="Method"
          summary={methodSummary}
          open={methodOpen}
          onToggle={() => setMethodOpen((o) => !o)}
        >
      {/* RU-11 — RP-3's resolution sentence stays always-on for an Auto job: it is
          not describing what the control does, it is disclosing a resolution the
          operator never made and cannot otherwise see. Same for runnerOnly, which
          explains a card that is disabled. Only a PINNED executor sitting at its
          default gets the engaged treatment — there the sentence merely restates
          the card that is already visibly selected. */}
      <FormField
        label="Executor"
        helperMode="engaged"
        active={runnerOnly || executorAuto || executor !== defaultExecutor}
        helper={
          runnerOnly ? (
            <>
              <strong>{job.type}</strong> requires a runner with the local toolchain — SSH is unavailable for this run-type.
            </>
          ) : (
            <>
              Will run via <strong>{executor === "ssh" ? "the in-app SSH executor" : "a runner agent"}</strong>
              {executorAuto && executor === defaultExecutor ? " (resolved from the job's Auto default)" : ""}.
            </>
          )
        }
      >
        <div style={{ display: "flex", gap: 8 }}>
          <ExecutorChoice
            label="SSH"
            sub={executorAuto && defaultExecutor === "ssh" ? "In-app SSH · job default (Auto)" : "In-app SSH"}
            selected={executor === "ssh"}
            disabled={runnerOnly}
            title={runnerOnly ? `SSH can't run ${job.type} — it needs a runner with the local ${job.type} toolchain.` : undefined}
            onClick={() => setExecutor("ssh")}
          />
          <ExecutorChoice
            label="Runner"
            sub={executorAuto && defaultExecutor === "runner" ? "Runner agent · job default (Auto)" : "Runner agent"}
            selected={executor === "runner"}
            onClick={() => setExecutor("runner")}
          />
        </div>
      </FormField>

      {/* RT-3 — the per-run runner pin, directly under Executor: the two answer
          "which kind of executor" and "which runner" in that order.

          Hidden on the SSH executor rather than disabled-with-a-caveat: there is
          no runner to pin, and the server 422s the combination (RT-Q5), so
          offering a control that can only produce a rejection is worse than not
          offering one. The Connect-as field below is hidden on runner-only types
          for the mirror-image reason.

          helperMode="engaged" per RU-11's split: the prose describes what the
          control DOES, so it appears on focus or when the value is non-default.
          The exception is the 0-online case, which is a WARNING — the run will
          queue — and warnings are never suppressed. */}
      {executor !== "ssh" && (
        <FormField
          label="Run on"
          helperMode="engaged"
          active={runnerTagOverride !== null || jobPin !== ""}
          helper={
            effectivePin === "" ? (
              runnerTagOverride === "" && jobPin !== "" ? (
                <>
                  This run ignores the job's pin (<span style={{ fontFamily: c.mono }}>{jobPin}</span>) and may be
                  claimed by any eligible runner. The job itself is unchanged.
                </>
              ) : (
                <>Any runner eligible for this job's department and run type may claim it.</>
              )
            ) : (
              <>
                {/* Deliberately NOT repeating the capacity here: the input prints
                    it directly underneath, and the zero-online case gets its own
                    warning block below. Saying it three times made the pinned
                    state read as three separate problems. */}
                Only runners tagged <strong>{effectivePin}</strong> may claim this run
                {runnerTagOverride === null && jobPin !== "" ? " — the job's pin" : ""}.
              </>
            )
          }
        >
          <RunnerPinInput
            id={`run-pin-${job.id ?? "x"}`}
            value={effectivePin}
            onChange={(v) => setRunnerTagOverride(v)}
            tags={runnerTags}
          />
          {/* RU-11 — a WARNING, so it is never suppressed by helperMode="engaged".
              A pin with nothing online is legal (RT-Q4: the runner may be
              enrolled in a minute) but the run WILL sit queued, and that is the
              one thing an operator pressing Run needs to know before they do.
              Guarded on the tag list having loaded, so a slow /runners fetch
              cannot flash "no runner carries this" over a perfectly good pin. */}
          {effectivePin !== "" && !pinLoading &&
            !runnerTags.some((t) => t.tag.toLowerCase() === effectivePin.trim().toLowerCase() && t.online > 0) && (
              <div style={{ fontSize: c.fontSm, color: c.warning, background: c.warningBg, border: `1px solid ${c.warning}30`, borderRadius: c.radiusSurface, padding: "8px 10px", marginTop: 8 }}>
                ⚠ No runner tagged <strong>{effectivePin}</strong> is online. The run is accepted but stays queued until
                one appears — or run it on any runner instead.
              </div>
            )}
          <div style={{ display: "flex", gap: 8, marginTop: 6, flexWrap: "wrap" }}>
            {/* The break-glass control. Explicitly separate from clearing the
                field, because clearing to "" IS the unpin — this button just
                makes the decision reachable in one click and names it. */}
            {effectivePin !== "" && (
              <Btn small onClick={() => setRunnerTagOverride("")}>
                Run on any runner
              </Btn>
            )}
            {runnerTagOverride !== null && (
              <Btn small onClick={() => setRunnerTagOverride(null)}>
                Use the job's pin{jobPin ? ` (${jobPin})` : " (none)"}
              </Btn>
            )}
          </div>
        </FormField>
      )}

      {/* CA — per-run "connect as" identity. Hidden for runner-only run types
          (ansible/terraform), where identity belongs to the inventory/toolchain
          and the server 422s these fields. */}
      {identityCapable && (
        <FormField
          label="Connect as"
          helperMode="engaged"
          active={!!(sshUser.trim() || sshCredential || jobDetail.sshUser || jobDetail.sshCredential)}
          helper={
            sshUser.trim() || sshCredential ? (
              <>
                {/* RP-10 — the wording follows the mechanism. On ansible the
                    identity comes from the scope's inventory and the override is
                    delivered as extra-vars, which BEAT it; naming the two inventory
                    vars is what tells an operator who wired ansible_user in Git that
                    this wins. Bastion hops are an in-app-SSH concept and are left
                    out there — ansible routes via the inventory's own ProxyJump. */}
                This run connects as{" "}
                <strong>{sshUser.trim() || (job.type === "ansible" ? "the inventory's user" : "each host's configured user")}</strong>{" "}
                with{" "}
                <strong>
                  {sshCredential
                    ? `the ${sshCredential} key`
                    : job.type === "ansible"
                      ? "the inventory's key"
                      : "each host's configured key"}
                </strong>{" "}
                on every target.
                {job.type === "ansible" ? (
                  <>
                    {" "}It overrides <code style={{ fontFamily: c.mono }}>ansible_user</code>
                    {sshCredential ? (
                      <> and <code style={{ fontFamily: c.mono }}>ansible_ssh_private_key_file</code></>
                    ) : null}{" "}
                    from the scope's inventory.
                  </>
                ) : (
                  <> Bastion hops are unaffected.</>
                )}
                {executor === "runner" && sshCredential && (
                  <> The key is delivered to the runner for this run only — runners not flagged for secret injection (or using a local inventory) will not take it.</>
                )}
              </>
            ) : jobDetail.sshUser || jobDetail.sshCredential ? (
              <>
                This job connects as <strong>{jobDetail.sshUser || "each host's configured user"}</strong> with{" "}
                <strong>{jobDetail.sshCredential ? `the ${jobDetail.sshCredential} key` : "each host's configured key"}</strong>{" "}
                (from the job definition). Override either for this run only.
              </>
            ) : job.type === "ansible" ? (
              <>Each host uses the identity its inventory declares. An override here beats that inventory for every host in this run.</>
            ) : (
              <>Each target uses its host's configured login and key. Override either for this run only.</>
            )
          }
        >
          <div style={{ display: "flex", gap: 8 }}>
            <input
              value={sshUser}
              onChange={(e) => setSshUser(e.target.value)}
              placeholder={jobDetail.sshUser ? `${jobDetail.sshUser} (from job)` : job.type === "ansible" ? "Inventory default user" : "Per-host default user"}
              style={{ ...inputStyle(), flex: 1, fontFamily: c.mono }}
            />
            <select
              value={sshCredential}
              onChange={(e) => setSshCredential(e.target.value)}
              disabled={!canPickKey}
              title={canPickKey ? undefined : "Selecting an SSH key for a run requires the Manage Env Vars permission."}
              style={{ ...inputStyle(), flex: 1, cursor: canPickKey ? "pointer" : "not-allowed", opacity: canPickKey ? 1 : 0.6 }}
            >
              <option value="">
                {jobDetail.sshCredential
                  ? `Job default (${jobDetail.sshCredential})`
                  : job.type === "ansible"
                    ? "Inventory default key"
                    : "Per-host default key"}
              </option>
              {credentialLabels.map((label) => (
                <option key={label} value={label}>
                  {label}
                </option>
              ))}
            </select>
          </div>
        </FormField>
      )}

        </Disclosure>
      </div>

      {/* AR — When to run: now (default) or a deferred instant. A deferred run
          is parked as a pending run — visible on Schedules → Upcoming (with a
          Cancel) and on this job's row — and fires through the normal run path
          at its instant. Everything set above is frozen at Run, not at fire. */}
      <div style={{ marginTop: 14 }}>
        <Disclosure
          title="Timing"
          summary={runAt ? `SCHEDULED · ${fmtInAppZone(runAt, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })}` : "now"}
          open={whenOpen}
          onToggle={() => setWhenOpen((o) => !o)}
        >
          <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
            <label style={{ display: "flex", alignItems: "center", gap: 8, fontSize: c.fontSm, color: c.text, cursor: "pointer" }}>
              <input type="radio" checked={runAt === null} onChange={() => setRunAt(null)} />
              Now — enqueue immediately
            </label>
            <label style={{ display: "flex", alignItems: "center", gap: 8, fontSize: c.fontSm, color: c.text, cursor: "pointer" }}>
              <input type="radio" checked={runAt !== null} onChange={() => setRunAt(defaultRunAtISO())} />
              At a specific time
            </label>
            {/* CAL-14/CAL-15 — the same note answers both: whether the chosen
                instant (or now) is a day this job's schedules would have fired
                on. A warning, never a block — manual runs are not calendar-gated. */}
            <RunCalendarNote entries={jobDetail.schedules ?? []} at={runAt} />
            {runAt !== null && (
              <div style={{ marginLeft: 24, display: "flex", flexDirection: "column", gap: 4 }}>
                <input
                  type="datetime-local"
                  value={isoToLocalInput(runAt)}
                  onChange={(e) => {
                    const iso = localInputToISO(e.target.value);
                    if (iso) setRunAt(iso);
                  }}
                  style={{ background: c.bg, color: c.text, border: `1px solid ${c.border}`, borderRadius: c.radiusChip, padding: "6px 8px", fontSize: c.fontSm, width: 220 }}
                />
                <span style={{ fontSize: c.fontXs, color: c.textMuted }}>
                  Entered in your timezone · fires {fmtInAppZone(runAt, { weekday: "short", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })} (app zone).
                  Settings are frozen now; the run appears under Schedules → Upcoming until it fires, where it can be cancelled.
                </span>
              </div>
            )}
          </div>
        </Disclosure>
      </div>

      {/* RV — the LAST section: the options that change what the run MEANS or what
          it receives, out of the everyday path. RU-3 demoted it below "When to run"
          — timing is an everyday question and it used to sit beneath ansible's
          verbosity selector. What is left in here is ansible's set (Phase 3) plus
          the raw --limit that RU-6 moved in; the per-run env overrides left for
          "Run inputs". The collapsed summary keeps CHECK MODE visible, because that
          is the one advanced fact that reinterprets the whole run. */}
      <div style={{ marginTop: 14 }}>
        <Disclosure
          title="Advanced"
          summary={advancedSummary}
          open={advancedOpen}
          onToggle={() => setAdvancedOpen((o) => !o)}
          tone="quiet"
        >
          {job.type === "ansible" && (
            <>
          {/* M3 — raw ansible --limit passthrough (ansible runner runs only).
            RP-Q4 — mutually exclusive with the host/group pickers: each disables
            the other, because ansible's `:` unions patterns and any combination
            could broaden the target set past what either input says alone. */}
        {executor === "runner" && job.type === "ansible" && (
          <FormField
            label="Ansible --limit"
            helperMode="engaged"
            active={rawLimitActive}
            helperTone={limitConflict ? "danger" : undefined}
            helper={
              limitConflict ? (
                <>Untick the host/group limits in <strong>Targets</strong> to use a raw pattern — they can't be combined.</>
              ) : (
                <>
                  Raw ansible <code style={{ fontFamily: c.mono }}>--limit</code> pattern for selections the pickers can't
                  express. Mutually exclusive with the host/group selection in <strong>Targets</strong>. Ansible runs only.
                </>
              )
            }
          >
            <input
              value={ansibleLimit}
              onChange={(e) => setAnsibleLimit(e.target.value)}
              placeholder="e.g. webservers:&staged:!quarantine"
              disabled={chipsActive}
              title={chipsActive ? "Untick the host/group limits in Targets to use a raw pattern." : undefined}
              style={{ ...inputStyle(), fontFamily: c.mono, cursor: chipsActive ? "not-allowed" : undefined, opacity: chipsActive ? 0.6 : 1 }}
            />
          </FormField>
        )}

          <FormField
            label="Mode"
            helperMode="engaged"
            active={ansCheck || ansDiff}
            helper={
              ansCheck
                ? "Dry run: ansible reports what WOULD change and applies nothing. The run still succeeds or fails normally in History, so this is the only place that says it was a rehearsal."
                : "Run normally, applying changes."
            }
          >
            <label style={{ display: "flex", alignItems: "center", gap: 8, fontSize: c.fontSm, cursor: "pointer" }}>
              <input type="checkbox" checked={ansCheck} onChange={(e) => setAnsCheck(e.target.checked)} />
              Check mode (<code style={{ fontFamily: c.mono }}>--check</code>) — don't change anything
            </label>
            <label style={{ display: "flex", alignItems: "center", gap: 8, fontSize: c.fontSm, cursor: "pointer", marginTop: 6 }}>
              <input type="checkbox" checked={ansDiff} onChange={(e) => setAnsDiff(e.target.checked)} />
              Show diffs (<code style={{ fontFamily: c.mono }}>--diff</code>)
            </label>
          </FormField>

          <FormField
            label="Tags"
            helperMode="engaged"
            active={ansTagList.length > 0 || ansSkipTagList.length > 0}
            helper="Comma-separated task tags. Only tasks carrying a listed tag run; skipped tags are excluded even if also selected. Leave both empty to run the whole playbook."
          >
            <div style={{ display: "flex", gap: 8 }}>
              <input
                value={ansTags}
                onChange={(e) => setAnsTags(e.target.value)}
                placeholder="only these tags — e.g. certs,config"
                style={{ ...inputStyle(), flex: 1, fontFamily: c.mono }}
              />
              <input
                value={ansSkipTags}
                onChange={(e) => setAnsSkipTags(e.target.value)}
                placeholder="skip these tags"
                style={{ ...inputStyle(), flex: 1, fontFamily: c.mono }}
              />
            </div>
          </FormField>

          <FormField
            label="Verbosity"
            helperMode="engaged"
            active={ansVerbosity > 0}
            helper="Ansible's -v levels. Higher levels print connection detail and full task results — useful for a failing run, noisy for a healthy one."
          >
            <select
              value={ansVerbosity}
              onChange={(e) => setAnsVerbosity(Number(e.target.value))}
              style={{ ...inputStyle(), cursor: "pointer" }}
            >
              <option value={0}>Normal</option>
              <option value={1}>-v — task results</option>
              <option value={2}>-vv — task and handler detail</option>
              <option value={3}>-vvv — connection detail</option>
              <option value={4}>-vvvv — connection debug</option>
            </select>
          </FormField>

          <FormField
            label="Privilege escalation"
            helperMode="engaged"
            active={ansBecome || ansBecomeUser.trim() !== ""}
            helper="Escalate on the remote host (sudo by default). This grants nothing new — a playbook can already declare become itself — and it fails loudly if the target has no passwordless escalation configured."
          >
            <label style={{ display: "flex", alignItems: "center", gap: 8, fontSize: c.fontSm, cursor: "pointer" }}>
              <input type="checkbox" checked={ansBecome} onChange={(e) => setAnsBecome(e.target.checked)} />
              Become (<code style={{ fontFamily: c.mono }}>--become</code>)
            </label>
            <input
              value={ansBecomeUser}
              onChange={(e) => setAnsBecomeUser(e.target.value)}
              placeholder="become user (default: root)"
              style={{ ...inputStyle(), fontFamily: c.mono, marginTop: 6 }}
            />
          </FormField>

          <FormField
            label="Extra variables"
            helperTone={reservedExtraVar ? "danger" : undefined}
            helper={
              reservedExtraVar ? (
                <>
                  <code style={{ fontFamily: c.mono }}>{reservedExtraVar}</code> is set by <strong>Connect as</strong>, in{" "}
                  <strong>Method</strong> — use that field instead. Ansible takes the last value for a repeated variable,
                  so allowing it here could make the run connect as someone other than what its own record says.
                </>
              ) : (
                <>
                  Passed as <code style={{ fontFamily: c.mono }}>-e name=value</code>, which outranks inventory and playbook
                  variables. Not the same thing as an environment override in <strong>Inputs</strong>: this sets an ansible
                  variable, that sets a process env var. <strong>Visible in the process list</strong> on the runner host — never
                  put a secret here; store it as a Secret and add it under References, in <strong>Inputs</strong>.
                </>
              )
            }
          >
            {/* RU-5 — the label stays distinct from "Add an override" in Run inputs:
                the two are one fold apart now, and they do different things (env var
                vs ansible -e variable). The helper above says so outright. */}
            <EnvRowsEditor rows={ansExtraVars} onChange={setAnsExtraVars} addLabel="+ Add extra-var" />
          </FormField>
            </>
          )}
          {/* RU-5 — the F1 per-run env overrides used to close this section. They
              are now a subgroup of "Run inputs", next to the declared prompts they
              outrank. A non-ansible job therefore opens an EMPTY Advanced fold,
              which is the honest answer: everything left in here is ansible's. */}
          {job.type !== "ansible" && (
            <div style={{ fontSize: c.fontSm, color: c.textSec }}>
              Nothing to set for a <strong>{job.type}</strong> run — these options are ansible's. Per-run environment
              overrides moved to <strong>Inputs</strong>.
            </div>
          )}
        </Disclosure>
      </div>

    </Modal>
  );
}

// AR helpers — datetime-local speaks the browser's wall clock; the API speaks
// absolute ISO instants.
function defaultRunAtISO(): string {
  // Default the picker one hour out, on the hour — a sane, obviously-editable start.
  const d = new Date(Date.now() + 60 * 60 * 1000);
  d.setMinutes(0, 0, 0);
  return d.toISOString();
}
function isoToLocalInput(iso: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "";
  const d = new Date(t);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}
function localInputToISO(v: string): string | null {
  if (!v.trim()) return null;
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? null : d.toISOString();
}

// RX-4 — the Stop dialog. The operator picks what the stop MEANT; the run's
// status becomes that, and killedBy records them either way. The default is
// "Stopped" (the wire's `killed`), so an operator who just wants the run to
// end gets exactly today's behaviour without engaging with the choice.
//
// "Stop all queued" deliberately takes NO disposition: it clears a stacked
// queue of runs that never started, so there is no work whose outcome anyone
// could assert. Offering a disposition there would invite recording a success
// for a run that never executed.
function ConfirmKill({ job, busy, onCancel, onConfirm, onConfirmAll }: { job: Job; busy: boolean; onCancel: () => void; onConfirm: (outcome: KillOutcome) => void; onConfirmAll: () => void }) {
  const [outcome, setOutcome] = useState<KillOutcome>("killed");
  const chosen = KILL_OUTCOMES.find((o) => o.value === outcome)!;
  return (
    <Modal title={`Stop ${job.name}?`} onClose={onCancel}>
      <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6, marginBottom: 16 }}>
        <strong>Stop latest</strong> terminates the most recent active run on <strong>{job.host ?? job.scope ?? "its target"}</strong> and records it in History as you choose below.
        <br />
        <strong>Stop all queued</strong> also clears every queued copy of this job — useful when scheduled runs have stacked up with no runner to execute them. Each stopped run is audited individually and recorded as stopped.
      </div>

      <div style={{ marginBottom: 16 }}>
        <div style={{ fontSize: c.fontXs, fontWeight: 600, color: c.textSec, marginBottom: 8, letterSpacing: 0.3 }}>
          RECORD THIS RUN AS
        </div>
        <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
          {KILL_OUTCOMES.map((o) => (
            <Btn
              key={o.value}
              small
              primary={o.value === outcome}
              disabled={busy}
              onClick={() => setOutcome(o.value)}
            >
              {o.label}
            </Btn>
          ))}
        </div>
        <div style={{ fontSize: c.fontXs, color: c.textMuted, lineHeight: 1.5, marginTop: 8 }}>
          {chosen.hint}
        </div>
        {/* The stop is only QUEUED for the runner, so the process may still be
            alive when the status is written. Saying so keeps the disposition
            honest — it is an assertion of intent, not an observed exit. */}
        <div style={{ fontSize: c.fontXs, color: c.textMuted, lineHeight: 1.5, marginTop: 6 }}>
          Recorded immediately; the stop signal reaches the runner on its next poll. The exit code is left as-is.
        </div>
      </div>

      <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
        <Btn small disabled={busy} onClick={onCancel}>Cancel</Btn>
        <Btn small danger disabled={busy} onClick={() => onConfirm(outcome)}>{busy ? "Stopping…" : "Stop latest"}</Btn>
        <Btn small danger disabled={busy} onClick={onConfirmAll}>{busy ? "Stopping…" : "Stop all queued"}</Btn>
      </div>
    </Modal>
  );
}


// FX-E1 — the file-arrival ledger, finally readable. v0.57.31 recorded every
// arrival ("the durable answer to 'the file landed, why did nothing happen?'")
// and no surface could show one: the answer lived only in SQLite. Rendered only
// for jobs that declare a watch, fetched only when the detail is open.
function FileArrivalsSection({ jobId }: { jobId: number }) {
  type Sighting = {
    id: string;
    path: string;
    sizeBytes: number;
    seenAt: string;
    runId?: string | null;
    refusedReason?: string | null;
  };
  const [items, setItems] = useState<Sighting[] | null>(null);
  useEffect(() => {
    let live = true;
    api
      .GET("/jobs/{jobId}/file-sightings", { params: { path: { jobId: String(jobId) }, query: { pageSize: 10 } } })
      .then((res) => {
        if (live) setItems(((res.data as { items?: Sighting[] } | undefined)?.items ?? []) as Sighting[]);
      })
      .catch(() => {
        if (live) setItems([]);
      });
    return () => {
      live = false;
    };
  }, [jobId]);
  if (items === null) return null;
  return (
    <Section
      title={`File arrivals${items.length ? ` (last ${items.length})` : ""}`}
      info="Every arrival the watch observed, newest first — including the ones that started nothing. A refused arrival names the gate that stopped it; the file does not re-fire when the gate clears."
    >
      {items.length === 0 ? (
        <div style={{ fontSize: c.fontXs, color: c.textMuted }}>No arrivals observed yet.</div>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
          {items.map((sg) => (
            <div key={sg.id} style={{ display: "flex", gap: 10, alignItems: "baseline", flexWrap: "wrap", fontSize: c.fontXs }}>
              <span style={{ fontFamily: c.mono, color: c.text, wordBreak: "break-all" }}>{sg.path}</span>
              <span style={{ color: c.textMuted, whiteSpace: "nowrap" }}>{fmtWhen(sg.seenAt)}</span>
              {sg.runId ? (
                <span style={{ color: c.success }}>ran</span>
              ) : sg.refusedReason ? (
                // "did not START", not "did not run": one of these reasons is
                // "queued behind an active run", where a run WAS parked and will
                // execute when the gate clears. Claiming it did not run would
                // make the ledger lie about the one case that ends well.
                <span style={{ color: c.warning }} title={sg.refusedReason}>
                  did not start — {sg.refusedReason}
                </span>
              ) : (
                <span style={{ color: c.textMuted }}>recorded</span>
              )}
            </div>
          ))}
        </div>
      )}
    </Section>
  );
}

function fmtWhen(iso?: string | null): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  const dMin = Math.round((Date.now() - t) / 60000);
  const abs = Math.abs(dMin);
  const suf = dMin >= 0 ? "ago" : "from now";
  if (abs < 1) return "just now";
  if (abs < 60) return `${abs}m ${suf}`;
  if (abs < 48 * 60) return `${Math.round(abs / 60)}h ${suf}`;
  return `${Math.round(abs / 1440)}d ${suf}`;
}


// Absolute app-zone datetime for the Created / Last edited fields in the expanded
// row detail (V1.1-17; the columns were dropped in VC.10). `—` when null (git
// rows). Routes through the shared app-zone formatter (§5.2). Distinct from the
// relative fmtWhen above.
function fmtDateTime(v?: string | null): string {
  return fmtInAppZone(v);
}

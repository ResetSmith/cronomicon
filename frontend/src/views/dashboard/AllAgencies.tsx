import { useEffect, useMemo, useRef, useState } from "react";
import type { CSSProperties, ReactNode } from "react";
import { Link, useNavigate } from "react-router-dom";
import { allReadable, api, csrfHeader, errMsg } from "../../api/client";
import { paged, rows, useGet, useLiveGet } from "../../hooks";
import { Score } from "../../components/Score";
import { buildScoreMarks, scoreTruncatedAt, type ScoreRun, type ScoreUpcoming } from "../../components/score-feed";
import { ScoreCutNote, fetchScoreRuns } from "../../components/score-runs";
import { excludeSuppressed } from "../scheduling/calendars";
import { appZoneParts, fmtInAppZone } from "../../utils/datetime";
import { useAgency } from "../../agency";
import { Btn, InlineLoading, StatTile } from "../../components/ui";
import { CriticalChip } from "../../components/Annotation";
import { RunContact } from "../history/shared";
import { relationLabel } from "../history/RunNotes";
import { c } from "../../theme";
import { CreateFirstAgencyCard } from "./FirstAgency";
import { CentralHoldsBanner } from "../../components/CentralRunsNothing";

// The Dashboard with All agencies chosen (2.5.2 OP5, OP-21 to OP-26; the
// Operations overview of 2.4.4 F3 until then). One page across every agency,
// for those whose grant reaches every agency (the operations team, and global
// administrators), in the shape of an agency's Dashboard so it is one screen
// to learn: the status strip, the tiles, the Score, then Needs attention where
// an agency's Dashboard has Recent errors, and the agencies. Lists stay one
// agency at a time; every line here opens its item in its own agency (the link
// carries ?agency=, and the shell switches and says so). The only change made
// here is acknowledging a run (OP-20).

type OverviewAgency = {
  id: string;
  name: string;
  jobs: number;
  pausedJobs: number;
  /** Jobs on a schedule (OP-26): an agency Dashboard's "Active schedules". */
  scheduledJobs?: number;
  workflows: number;
  pausedWorkflows: number;
  runs24h: Record<string, number>;
  nextRunAt?: string | null;
  repository?: { status?: string | null; syncedAt?: string | null } | null;
  openNotices: number;
};
type AttentionItem = {
  kind: "job_failed" | "workflow_failed" | "run_lost" | "run_overdue" | "sync_failed";
  agencyId: string;
  agencyName: string;
  name: string;
  id?: string;
  traceId?: string;
  // Who to call (2.5.2 OP-1): the job's or workflow's annotation.
  contact?: string;
  critical?: boolean;
  /** The agency's contact (OP-28), when the item names none of its own. */
  agencyContact?: string;
  /** The run's record in brief (OP-20); absent where there is nothing. */
  acknowledged?: { by: string; relation: string; at: string };
  latestNote?: string;
  noteCount?: number;
  /** A failed job's previous failed run was acknowledged (OP-11). */
  previousAck?: { by: string; runAt: string };
  at: string;
};

/**
 * AttentionAck — an item's Acknowledgement cell (2.5.2 OP-20, mockup board 1).
 * In hand: who and when, the latest note on one line, and the note count
 * linking to the run. Not yet: an Acknowledge button that opens a small box
 * for an optional first note. A repository is not a run and has nothing to
 * acknowledge. The run's own detail holds the whole thread.
 */
function AttentionAck({
  item,
  open,
  onOpen,
  onClose,
  onDone,
  central,
}: {
  item: AttentionItem;
  open: boolean;
  onOpen: () => void;
  onClose: () => void;
  onDone: () => void;
  central: string;
}) {
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  if (item.kind === "sync_failed" || !item.traceId) return <span style={{ color: c.textMuted }}>Not a run</span>;
  const traceId = item.traceId;
  const count = item.noteCount ?? 0;
  const notesLink =
    count > 0 ? (
      <Link to={attentionHref(item)} style={{ color: c.primary, textDecoration: "none", fontSize: c.fontXs }}>
        {count} note{count === 1 ? "" : "s"}
      </Link>
    ) : null;

  if (item.acknowledged) {
    const label = relationLabel(item.acknowledged.relation, item.agencyName, central);
    return (
      <div style={{ display: "flex", flexDirection: "column", gap: 3 }}>
        <span data-testid="attention-acked" style={{ display: "inline-flex", alignItems: "center", gap: 6, color: c.success, fontWeight: 600 }}>
          <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true">
            <path d="M3 8.5l3 3 7-7" />
          </svg>
          Acknowledged by {item.acknowledged.by}
          {label ? ` (${label})` : ""}, {when(item.acknowledged.at)}
        </span>
        {item.latestNote && <span style={{ color: c.text, overflowWrap: "anywhere" }}>“{item.latestNote}”</span>}
        {notesLink}
      </div>
    );
  }

  async function acknowledge() {
    setBusy(true);
    setErr(null);
    const req = { params: { path: { traceId }, header: csrfHeader }, body: { kind: "acknowledged" as const, body: draft.trim() } };
    // A single run's notes are not narrowed by the agency in use (OP-13), so
    // this answers for any agency's run from here.
    const { error } = item.kind === "workflow_failed" ? await api.POST("/workflow-runs/{traceId}/notes", req) : await api.POST("/runs/{traceId}/notes", req);
    setBusy(false);
    if (error) {
      setErr(errMsg(error));
      return;
    }
    setDraft("");
    onDone();
  }

  if (!open) {
    return (
      <div style={{ display: "flex", flexDirection: "column", gap: 4, alignItems: "flex-start" }}>
        {item.latestNote && <span style={{ color: c.textSec, overflowWrap: "anywhere" }}>“{item.latestNote}”</span>}
        {notesLink}
        <Btn small onClick={onOpen}>
          Acknowledge
        </Btn>
      </div>
    );
  }
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6, border: `1px solid ${c.borderStrong}`, borderRadius: c.radiusSurface, padding: 8, background: c.panelInput }}>
      <label htmlFor={`ack-${traceId}`} style={{ fontSize: c.fontXs, color: c.textSec }}>
        What you know so far (optional)
      </label>
      <textarea
        id={`ack-${traceId}`}
        rows={2}
        value={draft}
        maxLength={1000}
        onChange={(e) => setDraft(e.target.value)}
        style={{ resize: "vertical", background: c.panel, color: c.text, border: `1px solid ${c.borderStrong}`, borderRadius: c.radiusChip, padding: "6px 8px", fontFamily: c.sans, fontSize: c.fontSm }}
      />
      <span style={{ fontSize: c.fontXs, color: c.textMuted }}>Do not paste credentials. Notes are seen by {item.agencyName} and by Ops.</span>
      <span style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
        <Btn small onClick={onClose} disabled={busy}>
          Cancel
        </Btn>
        <Btn small primary onClick={acknowledge} disabled={busy}>
          Acknowledge
        </Btn>
      </span>
      {err && <span style={{ fontSize: c.fontSm, color: c.danger }}>{err}</span>}
    </div>
  );
}
type OverviewData = { agencies: OverviewAgency[]; attention: AttentionItem[]; attentionTotal: number; generatedAt: string };
type Hit = { kind: "job" | "workflow"; id: string; name: string; agencyId: string; agencyName: string };

// The Dashboard's pace.
const REFRESH_MS = 30_000;

const KIND_LABEL: Record<AttentionItem["kind"], string> = {
  job_failed: "Last run failed",
  workflow_failed: "Workflow's last run failed",
  run_lost: "Run lost (its agent went away)",
  run_overdue: "Running past its timeout",
  sync_failed: "Repository sync failed",
};

/**
 * Where an attention item is opened: in its own agency, at the run it names
 * (2.5.2 OP-4). A failed job opened its run list and a failed workflow the
 * whole tab, which made the person find the run again.
 */
export function attentionHref(it: AttentionItem): string {
  const a = `agency=${encodeURIComponent(it.agencyId)}`;
  const trace = it.traceId ? `trace=${encodeURIComponent(it.traceId)}&` : "";
  switch (it.kind) {
    case "job_failed":
    case "run_lost":
    case "run_overdue":
      return `/runs?${trace}${a}`;
    case "workflow_failed":
      return `/runs?tab=workflow-runs&${trace}${a}`;
    case "sync_failed":
      return `/runs?tab=git-sync&${a}`;
  }
}

/** Where a search hit is opened: a job's runs, or its agency's workflows. */
export function hitHref(h: Hit): string {
  const a = `agency=${encodeURIComponent(h.agencyId)}`;
  return h.kind === "job" ? `/runs?jobUid=${encodeURIComponent(h.id)}&${a}` : `/workflows?${a}`;
}

function when(at?: string | null): string {
  if (!at) return "—";
  const d = new Date(at);
  return Number.isNaN(d.getTime()) ? at : d.toLocaleString();
}

function Chip({ n, colour, title }: { n: number; colour: string; title: string }) {
  if (!n) return null;
  return (
    <span
      title={title}
      style={{
        display: "inline-block",
        minWidth: 18,
        padding: "0 5px",
        marginRight: 4,
        borderRadius: c.radiusChip,
        background: `${colour}22`,
        border: `1px solid ${colour}55`,
        color: colour,
        fontSize: c.fontXs,
        fontWeight: 700,
        fontFamily: c.mono,
        textAlign: "center",
        lineHeight: "18px",
      }}
    >
      {n}
    </span>
  );
}

function Section({ title, aside, children }: { title: string; aside?: ReactNode; children: ReactNode }) {
  return (
    <section style={{ background: c.panel, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, padding: 16, marginBottom: 20 }}>
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline", marginBottom: 10 }}>
        <h2 style={{ margin: 0, fontSize: c.fontBody, fontWeight: 600, color: c.text }}>{title}</h2>
        {aside}
      </div>
      {children}
    </section>
  );
}

/**
 * OverviewScore — the Dashboard's Score across every agency (2.4.4 F4): runs
 * in the last day and fires in the next twelve hours, each named with its
 * agency. Its feeds name no agency (allReadable), which the overview's
 * audience reads in full; a mark opens its run in its own agency (the run
 * link switches, SW-8).
 */
function OverviewScore() {
  const ag = useAgency();
  const navigate = useNavigate();
  const runsQ = useLiveGet<unknown>(() => fetchScoreRuns({ allAgencies: true }), [], () => true, REFRESH_MS);
  const upcomingQ = useLiveGet<unknown>(
    () => api.GET("/schedules/upcoming", { params: { query: { window: "24h" } }, headers: allReadable }),
    [],
    () => true,
    REFRESH_MS,
  );
  const runnersQ = useGet<unknown>(() => api.GET("/runners", { headers: allReadable }));
  const runsPage = paged<ScoreRun>(runsQ.data);
  const runs = runsPage.items;
  const upcomingAll = rows<ScoreUpcoming & { suppressed?: boolean }>(upcomingQ.data);
  const upcoming = excludeSuppressed(upcomingAll);
  const runnerNames = useMemo(() => {
    const m = new Map<string, string>();
    for (const r of rows<{ id?: string; name?: string }>(runnersQ.data)) if (r.id && r.name) m.set(r.id, r.name);
    return m;
  }, [runnersQ.data]);
  // Every mark says whose it is (the readout's Agency column): the strip mixes agencies.
  const marks = useMemo(
    () =>
      buildScoreMarks(runs, upcoming, runnerNames, {
        run: (r) => r.jobName ?? "—",
        upcoming: (u) => u.ownerName ?? "—",
        runAgency: (r) => ag.nameOf(r.ownerAgency || "global"),
        upcomingAgency: (u) => u.ownerAgencies?.[0] ?? ag.nameOf("global"),
      }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [runsQ.data, upcomingQ.data, runnerNames],
  );
  const now = Math.floor(Date.now() / 60_000) * 60_000;
  const zone = (opts: Intl.DateTimeFormatOptions) => (at: number) => fmtInAppZone(new Date(at).toISOString(), { ...opts, hour12: false });
  return (
    <div data-testid="overview-score" style={{ marginBottom: 20 }}>
      <ScoreCutNote shown={runs.length} total={runsPage.totalItems} where=", one agency at a time" />
      <Score
        marks={marks}
        now={now}
        hourInZone={(at) => appZoneParts(new Date(at)).hour}
        labelInZone={zone({ hour: "2-digit", minute: "2-digit" })}
        timeInZone={zone({ hour: "2-digit", minute: "2-digit", second: "2-digit" })}
        stampInZone={zone({ month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })}
        runHref={(t) => `/runs?trace=${encodeURIComponent(t)}`}
        onOpenRun={(t) => navigate(`/runs?trace=${encodeURIComponent(t)}`)}
        truncatedAt={scoreTruncatedAt(upcomingAll)}
        loading={runsQ.loading}
        pinnedReadout
      />
    </div>
  );
}

/**
 * AllAgenciesStrip — the status strip (OP-22), as an agency's Dashboard has
 * one: how many items need attention and how many nobody has acknowledged,
 * with the day's failures and warnings; it scrolls to Needs attention, and
 * dismissing it holds until the situation changes. Quiet when nothing does.
 */
function AllAgenciesStrip({
  total,
  unacked,
  failed,
  warned,
  agencies,
  onShow,
}: {
  total: number;
  unacked: number;
  failed: number;
  warned: number;
  agencies: number;
  onShow: () => void;
}) {
  const situation = `${total}/${unacked}/${failed}/${warned}`;
  const [dismissed, setDismissed] = useState<string | null>(null);
  const shell: CSSProperties = {
    display: "flex",
    alignItems: "baseline",
    gap: 10,
    flexWrap: "wrap",
    padding: "10px 16px",
    marginBottom: 14,
    borderRadius: c.radiusSurface,
    border: `1px solid ${c.border}`,
    background: c.panel,
    fontSize: c.fontBody,
  };
  if (total === 0) {
    return (
      <div aria-label="Status" style={shell}>
        <span style={{ fontWeight: 600, color: c.text }}>Nothing needs attention</span>
        <span style={{ fontSize: c.fontSm, color: c.textSec }}>across {agencies} agencies.</span>
      </div>
    );
  }
  if (dismissed === situation) return null;
  const parts = [failed > 0 ? `${failed} failed` : "", warned > 0 ? `${warned} warned` : ""].filter(Boolean);
  return (
    <div aria-label="Status" style={{ ...shell, border: `1px solid ${c.danger}`, background: c.dangerBg }}>
      <button
        onClick={onShow}
        title="Jump to Needs attention"
        style={{ flex: 1, display: "flex", alignItems: "baseline", gap: 10, flexWrap: "wrap", background: "transparent", border: "none", padding: 0, textAlign: "left", font: "inherit", fontSize: c.fontBody, cursor: "pointer" }}
      >
        <span style={{ fontWeight: 600, color: c.danger }}>
          {total} item{total === 1 ? "" : "s"} need{total === 1 ? "s" : ""} attention, {unacked} not yet acknowledged.
        </span>
        {parts.length > 0 && <span style={{ fontSize: c.fontSm, color: c.textSec }}>{parts.join(" · ")} in the last 24 hours.</span>}
      </button>
      <button
        onClick={() => setDismissed(situation)}
        aria-label="Dismiss this alert until something changes"
        title="Dismiss — returns if anything changes"
        style={{ background: "transparent", border: "none", padding: "0 0 0 12px", color: c.danger, opacity: 0.7, fontSize: c.fontBody, lineHeight: 1, cursor: "pointer" }}
      >
        ✕
      </button>
    </div>
  );
}

export function AllAgenciesDashboard() {
  const ag = useAgency();
  const q = useLiveGet<OverviewData>(() => api.GET("/overview") as never, [], () => true, REFRESH_MS);
  const [term, setTerm] = useState("");
  const [hits, setHits] = useState<Hit[] | null>(null);
  // 2.5.2 OP-20 — off by default: the incoming shift most needs the items
  // already in hand and their latest note, which this would hide.
  const [unackedOnly, setUnackedOnly] = useState(false);
  // The item whose Acknowledge box is open, by its run's trace id.
  const [ackFor, setAckFor] = useState<string | null>(null);
  const attentionRef = useRef<HTMLDivElement>(null);
  const showAttention = () => attentionRef.current?.scrollIntoView?.({ behavior: "smooth", block: "start" });

  // The search: a name, across agencies; each match says whose it is.
  useEffect(() => {
    const t = term.trim();
    if (!t) {
      setHits(null);
      return;
    }
    let cancelled = false;
    const timer = window.setTimeout(() => {
      void api.GET("/overview/search", { params: { query: { q: t } } }).then((res) => {
        if (!cancelled) setHits((res.data as Hit[] | undefined) ?? []);
      });
    }, 250);
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, [term]);

  if (!ag.canOverview) {
    return <div style={{ color: c.textSec }}>All agencies is for those whose access reaches every agency.</div>;
  }
  if (q.loading && !q.data) return <InlineLoading what="every agency" />;
  if (q.error && !q.data) return <div style={{ color: c.danger }}>Every agency could not be read: {q.error}</div>;
  const data = q.data as OverviewData;
  const ackedCount = data.attention.filter((it) => it.acknowledged).length;
  const shown = unackedOnly ? data.attention.filter((it) => !it.acknowledged) : data.attention;
  // The tiles, summed over agencies (OP-22).
  const sum = (f: (a: OverviewAgency) => number) => data.agencies.reduce((n, a) => n + f(a), 0);
  const failed24h = sum((a) => (a.runs24h?.failure ?? 0) + (a.runs24h?.killed ?? 0) + (a.runs24h?.lost ?? 0));
  const warned24h = sum((a) => a.runs24h?.warning ?? 0);
  const runningNow = sum((a) => a.runs24h?.running ?? 0);
  const scheduled = sum((a) => a.scheduledJobs ?? 0);
  const totalJobs = sum((a) => a.jobs);

  const th: CSSProperties = { textAlign: "left", padding: "6px 8px", fontSize: c.fontXs, color: c.textSec, textTransform: "uppercase", letterSpacing: 0.5, borderBottom: `1px solid ${c.border}` };
  const td: CSSProperties = { padding: "8px", fontSize: c.fontSm, color: c.text, borderBottom: `1px solid ${c.borderLight}`, verticalAlign: "top" };
  const link: CSSProperties = { color: c.primary, textDecoration: "none", fontWeight: 600 };

  return (
    <div data-testid="overview">
      {/* 2.5.3 CA-16: with every agency deleted, the way to make one. */}
      <CreateFirstAgencyCard />
      {/* Question 214: what Central still holds, said where work starts. */}
      <CentralHoldsBanner />
      <AllAgenciesStrip
        total={data.attentionTotal}
        unacked={data.attention.length - ackedCount}
        failed={failed24h}
        warned={warned24h}
        agencies={data.agencies.length}
        onShow={showAttention}
      />

      {/* OP-23 — there is no cross-agency History for the tiles to open, so
          Failed (24h) goes to Needs attention and the rest link nowhere. */}
      <div data-testid="all-agencies-tiles" style={{ display: "flex", gap: 12, marginBottom: 18, flexWrap: "wrap" }}>
        <button
          type="button"
          onClick={showAttention}
          title="Go to Needs attention"
          style={{ display: "flex", padding: 0, border: "none", background: "transparent", borderRadius: c.radiusSurface, cursor: "pointer", font: "inherit" }}
        >
          <StatTile label="Failed (24h)" value={String(failed24h)} color={failed24h ? c.danger : c.textMuted} tint={failed24h ? c.dangerBg : undefined} />
        </button>
        <div style={{ display: "flex" }}>
          <StatTile label="Running now" value={String(runningNow)} color={runningNow ? c.info : c.textMuted} />
        </div>
        <div style={{ display: "flex" }}>
          <StatTile label="Active schedules" value={String(scheduled)} color={c.textMuted} />
        </div>
        <div style={{ display: "flex" }}>
          <StatTile label="Total jobs" value={String(totalJobs)} color={c.textMuted} />
        </div>
        <div style={{ display: "flex" }}>
          <StatTile label="Agencies" value={String(data.agencies.length)} color={c.textMuted} />
        </div>
      </div>

      <OverviewScore />

      <div ref={attentionRef}>
        <NeedsAttention
          data={data}
          shown={shown}
          ackedCount={ackedCount}
          unackedOnly={unackedOnly}
          setUnackedOnly={setUnackedOnly}
          ackFor={ackFor}
          setAckFor={setAckFor}
          onAcked={() => {
            setAckFor(null);
            q.refetch();
          }}
          central={ag.nameOf("global")}
          th={th}
          td={td}
          link={link}
        />
      </div>

      <Section
        title="Agencies"
        aside={
          <span style={{ display: "inline-flex", alignItems: "center", gap: 12, flexWrap: "wrap" }}>
            <input
              value={term}
              onChange={(e) => setTerm(e.target.value)}
              placeholder="Find a job or workflow"
              aria-label="Find a job or workflow"
              style={{ width: 240, boxSizing: "border-box", padding: "5px 10px", borderRadius: c.radiusChip, border: `1px solid ${c.borderStrong}`, background: c.panelInput, color: c.text, fontSize: c.fontSm }}
            />
            <span style={{ fontSize: c.fontXs, color: c.textMuted }}>Updated {when(data.generatedAt)}</span>
          </span>
        }
      >
        {hits && (
          <div data-testid="find-hits" style={{ marginBottom: 12 }}>
            {hits.length === 0 ? (
              <div style={{ fontSize: c.fontSm, color: c.textMuted }}>Nothing of that name.</div>
            ) : (
              hits.map((h) => (
                <div key={h.kind + h.id} style={{ display: "flex", gap: 10, padding: "4px 0", fontSize: c.fontSm }}>
                  <span style={{ color: c.textMuted, width: 70 }}>{h.kind}</span>
                  <Link to={hitHref(h)} style={link}>
                    {h.name}
                  </Link>
                  <span style={{ color: c.textSec }}>in {h.agencyName}</span>
                </div>
              ))
            )}
          </div>
        )}
        <table style={{ width: "100%", borderCollapse: "collapse" }}>
          <thead>
            <tr>
              <th style={th}>Agency</th>
              <th style={th}>Jobs</th>
              <th style={th}>Workflows</th>
              <th style={th}>Runs, last 24 hours</th>
              <th style={th}>Next run</th>
              <th style={th}>Repository</th>
              <th style={th}>Notices</th>
            </tr>
          </thead>
          <tbody>
            {data.agencies.map((a) => {
              const r = a.runs24h ?? {};
              const failed = (r.failure ?? 0) + (r.killed ?? 0);
              return (
                <tr key={a.id} data-testid={`overview-row-${a.id}`}>
                  <td style={td}>
                    <Link to={`/?agency=${encodeURIComponent(a.id)}`} style={link} title={`Work in ${a.name}`}>
                      {a.name}
                    </Link>
                  </td>
                  <td style={td}>
                    {a.jobs}
                    {a.pausedJobs > 0 && <span style={{ color: c.textMuted }}> ({a.pausedJobs} paused)</span>}
                  </td>
                  <td style={td}>
                    {a.workflows}
                    {a.pausedWorkflows > 0 && <span style={{ color: c.textMuted }}> ({a.pausedWorkflows} paused)</span>}
                  </td>
                  <td style={td}>
                    <Chip n={r.success ?? 0} colour={c.success} title="succeeded" />
                    <Chip n={failed} colour={c.danger} title="failed or stopped" />
                    <Chip n={r.lost ?? 0} colour={c.danger} title="lost: the agent went away" />
                    <Chip n={r.warning ?? 0} colour={c.warning} title="warning" />
                    <Chip n={(r.running ?? 0) + (r.queued ?? 0)} colour={c.info} title="running or queued" />
                    {!Object.values(r).some(Boolean) && <span style={{ color: c.textMuted }}>none</span>}
                  </td>
                  <td style={td}>{when(a.nextRunAt)}</td>
                  <td style={td}>
                    {a.repository ? (
                      <span style={{ color: a.repository.status === "success" ? c.text : c.warning }}>
                        {a.repository.status ?? "never synced"}
                        {a.repository.syncedAt && <span style={{ color: c.textMuted }}> · {when(a.repository.syncedAt)}</span>}
                      </span>
                    ) : (
                      <span style={{ color: c.textMuted }}>none</span>
                    )}
                  </td>
                  <td style={td}>{a.openNotices > 0 ? <Chip n={a.openNotices} colour={c.warning} title="open notices" /> : <span style={{ color: c.textMuted }}>0</span>}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </Section>

    </div>
  );
}

/** Needs attention (OP-20, OP-22): what needs a person, with who to call and who has it in hand. */
function NeedsAttention({
  data,
  shown,
  ackedCount,
  unackedOnly,
  setUnackedOnly,
  ackFor,
  setAckFor,
  onAcked,
  central,
  th,
  td,
  link,
}: {
  data: OverviewData;
  shown: AttentionItem[];
  ackedCount: number;
  unackedOnly: boolean;
  setUnackedOnly: (v: boolean) => void;
  ackFor: string | null;
  setAckFor: (v: string | null) => void;
  onAcked: () => void;
  central: string;
  th: CSSProperties;
  td: CSSProperties;
  link: CSSProperties;
}) {
  return (
      <Section
      title="Needs attention"
      aside={
        <span style={{ display: "inline-flex", alignItems: "center", gap: 14, flexWrap: "wrap" }}>
          <label style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: c.fontSm, color: c.textSec, cursor: "pointer" }}>
            <input type="checkbox" checked={unackedOnly} onChange={(e) => setUnackedOnly(e.target.checked)} style={{ margin: 0, accentColor: c.primary }} />
            Unacknowledged only
          </label>
          <span style={{ fontSize: c.fontXs, color: c.textMuted }}>
            {data.attentionTotal > data.attention.length ? `The ${data.attention.length} newest of ${data.attentionTotal}` : `${data.attentionTotal}`}
            {ackedCount > 0 && ` · ${ackedCount} acknowledged`}
          </span>
        </span>
      }
    >
      {shown.length === 0 ? (
        <div style={{ fontSize: c.fontSm, color: c.textMuted }}>
          {data.attention.length === 0 ? "Nothing needs attention." : "Everything listed is acknowledged."}
        </div>
      ) : (
        <table style={{ width: "100%", borderCollapse: "collapse" }}>
          <thead>
            <tr>
              <th style={th}>What</th>
              <th style={th}>Name</th>
              <th style={th}>Agency</th>
              <th style={th}>Contact</th>
              <th style={th}>When</th>
              <th style={{ ...th, width: 320 }}>Acknowledgement</th>
            </tr>
          </thead>
          <tbody>
            {shown.map((it, i) => (
              <tr key={i} data-testid="attention-item" style={it.acknowledged ? { opacity: 0.62 } : undefined}>
                <td style={td}>{KIND_LABEL[it.kind] ?? it.kind}</td>
                <td style={td}>
                  <Link to={attentionHref(it)} style={link} title={`Open in ${it.agencyName}`}>
                    {it.name}
                  </Link>
                  {it.critical && (
                    <span style={{ marginLeft: 8, display: "inline-flex", verticalAlign: "middle" }}>
                      <CriticalChip />
                    </span>
                  )}
                  {/* OP-11 — a new failure is a new item; say it has history. */}
                  {it.previousAck && (
                    <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 3 }}>
                      Its previous failure ({when(it.previousAck.runAt)}) was acknowledged by {it.previousAck.by}
                    </div>
                  )}
                </td>
                <td style={td}>{it.agencyName}</td>
                <td style={td}>
                  {it.kind === "sync_failed" && !it.agencyContact ? (
                    <span style={{ color: c.textMuted }}>—</span>
                  ) : (
                    <RunContact contact={it.contact ?? ""} critical={false} agencyContact={it.agencyContact} />
                  )}
                </td>
                <td style={td}>{when(it.at)}</td>
                <td style={td}>
                  <AttentionAck
                    item={it}
                    open={ackFor === it.traceId}
                    onOpen={() => setAckFor(it.traceId ?? null)}
                    onClose={() => setAckFor(null)}
                    onDone={onAcked}
                    central={central}
                  />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </Section>
  );
}

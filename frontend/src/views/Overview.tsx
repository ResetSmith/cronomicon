import { useEffect, useMemo, useState } from "react";
import type { CSSProperties, ReactNode } from "react";
import { Link, useNavigate } from "react-router-dom";
import { allReadable, api } from "../api/client";
import { rows, useGet, useLiveGet } from "../hooks";
import { Score } from "../components/Score";
import { HOURS_BACK } from "../components/score-model";
import { buildScoreMarks, scoreTruncatedAt, type ScoreRun, type ScoreUpcoming } from "../components/score-feed";
import { excludeSuppressed } from "./scheduling/calendars";
import { appZoneParts, fmtInAppZone } from "../utils/datetime";
import { useAgency } from "../agency";
import { InlineLoading } from "../components/ui";
import { c } from "../theme";

// The Operations overview (2.4.4 F3; plan-2.4.4 SW-12, SW-13). ONE read-only
// page across every agency, for those whose grant reaches every agency (the
// operations team, and global administrators). Lists stay one agency at a
// time; every line here opens its item in its own agency (the link carries
// ?agency=, and the shell switches and says so). Nothing on it is editable.

type OverviewAgency = {
  id: string;
  name: string;
  jobs: number;
  pausedJobs: number;
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
  at: string;
};
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

/** Where an attention item is opened: in its own agency. */
export function attentionHref(it: AttentionItem): string {
  const a = `agency=${encodeURIComponent(it.agencyId)}`;
  switch (it.kind) {
    case "job_failed":
      return it.id ? `/runs?jobUid=${encodeURIComponent(it.id)}&${a}` : `/runs?${a}`;
    case "run_lost":
    case "run_overdue":
      return it.traceId ? `/runs?trace=${encodeURIComponent(it.traceId)}&${a}` : `/runs?${a}`;
    case "workflow_failed":
      return `/runs?tab=workflow-runs&${a}`;
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
  const runsQ = useLiveGet<unknown>(
    () =>
      api.GET("/runs", {
        params: { query: { kind: "job", from: new Date(Date.now() - (HOURS_BACK + 1) * 3600 * 1000).toISOString(), pageSize: 200 } },
        headers: allReadable,
      }),
    [],
    () => true,
    REFRESH_MS,
  );
  const upcomingQ = useLiveGet<unknown>(
    () => api.GET("/schedules/upcoming", { params: { query: { window: "24h" } }, headers: allReadable }),
    [],
    () => true,
    REFRESH_MS,
  );
  const runnersQ = useGet<unknown>(() => api.GET("/runners", { headers: allReadable }));
  const runs = rows<ScoreRun>(runsQ.data);
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

export function Overview() {
  const ag = useAgency();
  const q = useLiveGet<OverviewData>(() => api.GET("/overview") as never, [], () => true, REFRESH_MS);
  const [term, setTerm] = useState("");
  const [hits, setHits] = useState<Hit[] | null>(null);

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
    return <div style={{ color: c.textSec }}>The overview is for those whose access reaches every agency.</div>;
  }
  if (q.loading && !q.data) return <InlineLoading what="the overview" />;
  if (q.error && !q.data) return <div style={{ color: c.danger }}>The overview could not be read: {q.error}</div>;
  const data = q.data as OverviewData;

  const th: CSSProperties = { textAlign: "left", padding: "6px 8px", fontSize: c.fontXs, color: c.textSec, textTransform: "uppercase", letterSpacing: 0.5, borderBottom: `1px solid ${c.border}` };
  const td: CSSProperties = { padding: "8px", fontSize: c.fontSm, color: c.text, borderBottom: `1px solid ${c.borderLight}`, verticalAlign: "top" };
  const link: CSSProperties = { color: c.primary, textDecoration: "none", fontWeight: 600 };

  return (
    <div data-testid="overview">
      <OverviewScore />
      <Section
        title="Find a job or workflow"
        aside={<span style={{ fontSize: c.fontXs, color: c.textMuted }}>Across every agency</span>}
      >
        <input
          value={term}
          onChange={(e) => setTerm(e.target.value)}
          placeholder="A name, or part of one"
          aria-label="Find a job or workflow"
          style={{ width: "100%", boxSizing: "border-box", padding: "7px 10px", borderRadius: c.radiusChip, border: `1px solid ${c.border}`, background: c.panelInput, color: c.text, fontSize: c.fontSm }}
        />
        {hits && (
          <div style={{ marginTop: 8 }}>
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
      </Section>

      <Section title="Agencies" aside={<span style={{ fontSize: c.fontXs, color: c.textMuted }}>Updated {when(data.generatedAt)}</span>}>
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

      <Section
        title="Needs attention"
        aside={
          <span style={{ fontSize: c.fontXs, color: c.textMuted }}>
            {data.attentionTotal > data.attention.length ? `The ${data.attention.length} newest of ${data.attentionTotal}` : `${data.attentionTotal}`}
          </span>
        }
      >
        {data.attention.length === 0 ? (
          <div style={{ fontSize: c.fontSm, color: c.textMuted }}>Nothing needs attention.</div>
        ) : (
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr>
                <th style={th}>What</th>
                <th style={th}>Name</th>
                <th style={th}>Agency</th>
                <th style={th}>When</th>
              </tr>
            </thead>
            <tbody>
              {data.attention.map((it, i) => (
                <tr key={i} data-testid="attention-item">
                  <td style={td}>{KIND_LABEL[it.kind] ?? it.kind}</td>
                  <td style={td}>
                    <Link to={attentionHref(it)} style={link} title={`Open in ${it.agencyName}`}>
                      {it.name}
                    </Link>
                  </td>
                  <td style={td}>{it.agencyName}</td>
                  <td style={td}>{when(it.at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Section>
    </div>
  );
}

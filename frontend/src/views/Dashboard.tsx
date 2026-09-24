import { useMemo, useRef, useState, type CSSProperties } from "react";
import { useNavigate } from "react-router-dom";
import { api } from "../api/client";
import { useGet, useLiveGet, rows, paged } from "../hooks";
import { ambiguousNames, disambiguate, useNameDisambiguator, type NamedRef } from "../utils/disambiguate";
import { c } from "../theme";
import { DOC_LINKS } from "../components/docLinks";
import { SkeletonRows, StatTile, statusLabel, statusTone, DocLink } from "../components/ui";
import { Score } from "../components/Score";
import { HOURS_BACK, type Mark } from "../components/score-model";
import { GlobalCalendarBanner } from "./scheduling/GlobalCalendarBanner";
import { excludeSuppressed } from "./scheduling/calendars";
import { appZoneParts, fmtInAppZone } from "../utils/datetime";

// ── Local row types (list endpoints are loosely typed in the generated client) ──

interface Job {
  id?: number | string;
  name: string;
  type?: string;
  host?: string | null;
  scope?: string | null;
  status?: string;
  schedule?: string | null;
  nextRunAt?: string | null;
}

interface Run {
  // R2-1 identity + the run's frozen agency snapshot (R2F-3).
  jobUid?: string | null;
  agencies?: string[];
  traceId?: string;
  jobName?: string;
  type?: string;
  status?: string; // queued | running | success | warning | danger | skipped
  startedAt?: string | null;
  completedAt?: string | null;
  durationMs?: number | null;
  triggeredBy?: string | null;
  kind?: string | null; // "job" | "ssh-test"; ssh-test diagnostics are excluded from job metrics
  // SR-1 — the Score readout's per-run detail.
  triggerKind?: string | null;
  scheduleName?: string | null;
  executor?: string | null;
  runnerId?: string | null;
}

interface RunnerRow {
  id?: string;
  name?: string;
}

// One projected fire from GET /schedules/upcoming — spans both jobs and
// workflows, so it's accurate across multi-schedule owners (unlike the per-job
// `schedule` string heuristic this card used to derive from).
interface Upcoming {
  ownerKind?: string;
  ownerName?: string;
  // R2F-3 — the owning definition's identity and derived agencies.
  ownerUid?: string;
  ownerAgencies?: string[];
  scheduleName?: string;
  cron?: string;
  at?: string | null;
  // CAL-11 — a working calendar will suppress this fire. The projection annotates
  // rather than drops (the Upcoming tab shows the gap), so every surface that
  // answers "what runs next" has to exclude these itself: a suppressed fire is
  // not an upcoming run, and plotting one puts a note on the Score for something
  // that will never happen.
  suppressed?: boolean;
  suppressedBy?: string;
  suppressedLabel?: string;
}


// The verdict strip's dismissal (see the hook in Dashboard). Per browser, like
// column layouts and widths — a UI preference whose entire blast radius is one
// person's screen does not need a table, a migration and a CSRF-gated write.
const VERDICT_DISMISS_KEY = "dash:verdictDismissed";

// ── Small derive helpers ──

// Run statuses use "danger" where outcomeColor expects "failure".
const isScheduled = (j: Job) => !!j.schedule && j.schedule.toLowerCase() !== "manual";


function fmtWhen(iso?: string | null): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  const deltaMin = Math.round((Date.now() - t) / 60000);
  const abs = Math.abs(deltaMin);
  const suffix = deltaMin >= 0 ? "ago" : "from now";
  if (abs < 1) return "just now";
  if (abs < 60) return `${abs}m ${suffix}`;
  if (abs < 48 * 60) return `${Math.round(abs / 60)}h ${suffix}`;
  return `${Math.round(abs / 1440)}d ${suffix}`;
}

// ── View ──

export function Dashboard() {
  const navigate = useNavigate();
  const jobsQ = useGet<unknown>(() => api.GET("/jobs"));
  // SR-1 — id → name for the readout's "Runner · name" column (session-gated, like the Runners view).
  const runnersQ = useGet<unknown>(() => api.GET("/runners"));
  // The stat tiles need EXACT counts — a single client-filtered /runs page silently
  // truncated them past ~one page (CC.23). Issue targeted, LIVE filtered requests:
  // the page envelope's totalItems is the true count, and kind=job excludes ssh-test
  // connectivity probes server-side. running is fetched with a small page so the
  // "Currently Running" list is exact too; failed only needs the latest row for the
  // banner (pageSize 1) + its totalItems. useLiveGet polls so the tiles refresh
  // while the dashboard stays open. `from` is recomputed each fetch → sliding 24h.
  const runningQ = useLiveGet<unknown>(
    () => api.GET("/runs", { params: { query: { status: "running", kind: "job", pageSize: 20 } } }),
    [],
    () => true,
  );
  // pageSize 20, not 1: this feed serves the banner (latest row), the stat tile
  // (totalItems) AND the Current Status list, which names every job that failed
  // — the "+6 more" the banner truncates is exactly what that box exists to show.
  const failedQ = useLiveGet<unknown>(
    () =>
      api.GET("/runs", {
        params: {
          query: { status: "danger", kind: "job", from: new Date(Date.now() - 24 * 3600 * 1000).toISOString(), pageSize: 20 },
        },
      }),
    [],
    () => true,
  );
  // Warnings feed the Current Status list too — a run that warned is an
  // attention item, just a quieter one than a failure. Server-filtered for the
  // same CC.23 reason as the other tiles: a client-side filter over one /runs
  // page silently undercounts.
  const warnedQ = useLiveGet<unknown>(
    () =>
      api.GET("/runs", {
        params: {
          query: { status: "warning", kind: "job", from: new Date(Date.now() - 24 * 3600 * 1000).toISOString(), pageSize: 20 },
        },
      }),
    [],
    () => true,
  );
  // ── The Score's two feeds (D-1). No backend work: both endpoints already do
  // what the timeline needs.
  //
  // Trailing half. /runs takes arbitrary from/to bounds, but the bound is on
  // `created_at` (enqueue time), NOT `startedAt` — so ask for an hour more than
  // the window displays and clip on startedAt below. Without that margin, a run
  // queued just before the window but started inside it would be missing from
  // the plot. `from` is computed inside the thunk, so each poll slides it.
  const scoreRunsQ = useLiveGet<unknown>(
    () =>
      api.GET("/runs", {
        params: { query: { kind: "job", from: new Date(Date.now() - (HOURS_BACK + 1) * 3600 * 1000).toISOString(), pageSize: 200 } },
      }),
    [],
    () => true,
  );
  // Forward half. `window` is an ENUM (`24h` | `7d`), not a duration — any other
  // value is silently coerced to 24h server-side — so request 24h and trim to
  // the 12h the Score shows. At that horizon the projection caps
  // (perEntryCap 200 / totalCap 500) are not reached by a normal install; at the
  // 7d a range picker would have needed, a */30 schedule alone projects 336 and
  // is cut at 200 with no signal (VU-Q8).
  const upcomingQ = useLiveGet<unknown>(
    () => api.GET("/schedules/upcoming", { params: { query: { window: "24h" } } }),
    [],
    () => true,
  );

  // The full score is behind a toggle: it is one staff per job, so it is the
  // right depth on demand rather than by default (D-6).
  const [fullScore, setFullScore] = useState(false);
  // VU2-1 — the verdict strip's attention state points AT the evidence, which
  // is on this page rather than another: scrolling beats navigating when the
  // detail the reader wants is 600px below them.
  const errorsRef = useRef<HTMLDivElement>(null);

  const jobs = rows<Job>(jobsQ.data);
  const scoreRuns = rows<Run>(scoreRunsQ.data);
  // SR-1 — a run row carries only the claiming runner's id; the name lives on
  // the runners list, which every session may read. One small fetch, one map.
  const runnerNames = useMemo(() => {
    const m = new Map<string, string>();
    for (const r of rows<RunnerRow>(runnersQ.data)) if (r.id && r.name) m.set(r.id, r.name);
    return m;
  }, [runnersQ.data]);
  // Already sorted ascending by the backend projection. CAL-11 — suppressed
  // instants are filtered out HERE, once, so neither the Score nor the "next up"
  // line can forget: both answer "what will run", and a calendar-suppressed fire
  // will not.
  const upcomingAll = rows<Upcoming>(upcomingQ.data);
  const upcoming = excludeSuppressed(upcomingAll);

  const loading = jobsQ.loading || scoreRunsQ.loading;
  const error = jobsQ.error || scoreRunsQ.error;

  // Quantised to the minute so the Score's memoised window stays stable between
  // 4-second polls. At a 36-hour span a minute is a third of a pixel, so nothing
  // visibly lags; recomputing on every render would rebuild the whole plot's
  // geometry four times a minute for no visible gain.
  const now = Math.floor(Date.now() / 60_000) * 60_000;

  // ── Normalising the two feeds into one set of marks ──
  // The four cards this replaces were one dataset split by tense, each rendering
  // it differently. Here the split is a single field, and everything downstream
  // is tense-agnostic.
  const marks = useMemo<Mark[]>(() => {
    const out: Mark[] = [];
    // R2F-3 — past runs and future fires share one timeline, so they share one
    // ambiguity set: a name is badged if EITHER half of the timeline holds two
    // definitions by that name. Computed here rather than via the hook because
    // the visible set is the union of two differently-shaped lists.
    const refs: NamedRef[] = [
      ...scoreRuns.map((r) => ({ uid: r.jobUid, name: r.jobName, agencies: r.agencies })),
      ...upcoming.map((u) => ({ uid: u.ownerUid, name: u.ownerName, agencies: u.ownerAgencies })),
    ];
    const ambiguous = ambiguousNames(refs);
    const markLabel = (ref: NamedRef) => disambiguate(ref, ambiguous);
    for (const r of scoreRuns) {
      // Plot the START. The API filtered on enqueue time, so clip here — and a
      // run with no startedAt has not begun and has no place on the timeline.
      const at = r.startedAt ? Date.parse(r.startedAt) : NaN;
      if (Number.isNaN(at) || r.kind === "ssh-test") continue;
      out.push({
        at,
        tense: "past",
        jobName: markLabel({ uid: r.jobUid, name: r.jobName, agencies: r.agencies }) || "—",
        status: r.status,
        durationMs: r.durationMs ?? null,
        traceId: r.traceId,
        type: r.type,
        triggerKind: r.triggerKind,
        triggeredBy: r.triggeredBy,
        scheduleName: r.scheduleName,
        executor: r.executor,
        runnerName: r.runnerId ? (runnerNames.get(r.runnerId) ?? null) : null,
      });
    }
    for (const u of upcoming) {
      const at = u.at ? Date.parse(u.at) : NaN;
      if (Number.isNaN(at)) continue;
      out.push({
        at,
        tense: "future",
        jobName: markLabel({ uid: u.ownerUid, name: u.ownerName, agencies: u.ownerAgencies }) || "—",
        ownerKind: u.ownerKind,
      });
    }
    return out;
  }, [scoreRuns, upcoming, runnerNames]);

  // D-2b. The API gives NO truncation signal — the response is `{ window, items }`
  // and both caps are silent — so a full 500 is the only inference available.
  // A false positive is harmless (an explicit terminator where the data merely
  // fits exactly); the forward half trailing off with no explanation is not.
  // Inferred from the RAW projection, before the CAL-11 suppression filter: the
  // cap is applied server-side to every item it returned, so filtering first
  // would hide a real truncation whenever any projected fire was suppressed.
  const truncatedAt =
    upcomingAll.length >= 500 && upcomingAll[upcomingAll.length - 1]?.at ? Date.parse(upcomingAll[upcomingAll.length - 1].at!) : null;

  // Ticks and the day rule follow the APPLICATION timezone — the zone the
  // scheduler fires in, and the zone /schedules/upcoming projects in. Browser-
  // local would visibly desync the marks from the gridlines.
  const hourInZone = (at: number) => appZoneParts(new Date(at)).hour;
  const labelInZone = (at: number) => fmtInAppZone(new Date(at).toISOString(), { hour: "2-digit", minute: "2-digit", hour12: false });
  // SR-1 — seconds matter in a readout row: a cluster's members are up to one
  // notehead apart, and "did these run together or in sequence" is the question.
  const timeInZone = (at: number) => fmtInAppZone(new Date(at).toISOString(), { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
  const stampInZone = (at: number) =>
    fmtInAppZone(new Date(at).toISOString(), { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false });

  // Running + failed come from their own server-filtered feeds, so the counts are
  // exact (totalItems) and ssh-test-free (kind=job) rather than truncated to one
  // page (CC.23). scheduled/total-jobs stay page-derived (out of CC.23's scope).
  const runningPage = paged<Run>(runningQ.data);
  const runningCount = runningPage.totalItems;
  const failedPage = paged<Run>(failedQ.data);
  const failedRuns = failedPage.items;
  const failedCount = failedPage.totalItems;
  const warnedPage = paged<Run>(warnedQ.data);
  const warnedRuns = warnedPage.items;
  const warnedCount = warnedPage.totalItems;
  const scheduled = jobs.filter(isScheduled);

  // ── Current Status (CS) ──
  // The Score answers WHEN; nothing on the page answered "is everything okay?"
  // in words. This folds the failure/warning feeds per JOB — a flapping job is
  // one row with a count, not seven identical rows — worst outcome wins, latest
  // occurrence shown. Failures sort before warnings, then by recency.
  const attention = useMemo(() => {
    const byJob = new Map<string, { name: string; uid?: string | null; agencies?: string[]; status: string; count: number; latestAt: number | null }>();
    const fold = (list: Run[], status: string) => {
      for (const r of list) {
        const name = r.jobName ?? "—";
        // R2F-3 — fold by IDENTITY, not by name. Keyed on the name, two
        // departments' failures of their same-named jobs merged into ONE tile
        // with a summed count, and clicking it filtered history to both. Falls
        // back to the name for pre-backfill rows, which is what they had.
        const key = r.jobUid || name;
        const at = Date.parse(r.completedAt ?? r.startedAt ?? "");
        const cur = byJob.get(key);
        if (!cur) {
          byJob.set(key, { name, uid: r.jobUid, agencies: r.agencies, status, count: 1, latestAt: Number.isNaN(at) ? null : at });
        } else {
          cur.count += 1;
          if (!Number.isNaN(at) && (cur.latestAt == null || at > cur.latestAt)) cur.latestAt = at;
          if (status === "danger") cur.status = "danger";
        }
      }
    };
    // Failures fold first so a job that failed AND warned reads as failed.
    fold(failedRuns, "danger");
    fold(warnedRuns, "warning");
    return [...byJob.values()].sort((a, b) =>
      a.status === b.status ? (b.latestAt ?? 0) - (a.latestAt ?? 0) : a.status === "danger" ? -1 : 1,
    );
  }, [failedRuns, warnedRuns]);
  // ── Dismissing the verdict strip ──
  // The strip can be silenced, but ONLY for the situation it is currently
  // reporting. The dismissal stores a SIGNATURE of the attention set, and the
  // strip reappears the moment that signature changes — a new job failing, an
  // existing one failing again, a warning turning into a failure, or an old
  // failure ageing out from under a newer one. So "dismiss" means "I have seen
  // THIS", never "stop telling me".
  //
  // This is the one behaviour the CS-1 banner had that Current status could not
  // replace, and the reason that banner was safe to dismiss at all. `latestAt`
  // is in the signature deliberately, not just the count: a job that failed
  // twice and then had its older failure age out of the 24h window would return
  // to count 1 and re-match a stale dismissal, hiding a failure that is newer
  // than the one the operator acknowledged.
  const attentionSig = useMemo(
    () => attention.map((a) => `${a.uid || a.name}:${a.status}:${a.count}:${a.latestAt ?? 0}`).join("|"),
    [attention],
  );
  const [dismissedSig, setDismissedSig] = useState<string | null>(() => {
    // Private browsing and hardened profiles can throw on access, not just on
    // write — the same tolerance useColumnWidths applies.
    try {
      return localStorage.getItem(VERDICT_DISMISS_KEY);
    } catch {
      return null;
    }
  });
  const verdictDismissed = attentionSig !== "" && dismissedSig === attentionSig;
  const dismissVerdict = () => {
    setDismissedSig(attentionSig);
    try {
      localStorage.setItem(VERDICT_DISMISS_KEY, attentionSig);
    } catch {
      // A failed write costs the operator a re-dismiss after reload, which is
      // strictly better than a crash on a read-only storage.
    }
  };

  // The badge appears only when this tile list actually holds two jobs of one
  // name — the whole point being that the two tiles are now distinguishable.
  const attentionLabel = useNameDisambiguator(attention, (a) => ({ uid: a.uid, name: a.name, agencies: a.agencies }));
  // Each feed is capped at a page; past that the per-job counts are floors, so
  // say so rather than presenting a truncated list as the whole story.
  const attentionTruncated = failedCount > failedRuns.length || warnedCount > warnedRuns.length;
  // The forward-looking facts. `upcoming` is sorted ascending by the backend and
  // already excludes calendar-suppressed instants (CAL-11, filtered once above).
  //
  // VU2-3 — three, not one. CS-4 deliberately showed a single fire to keep the
  // Dashboard from turning into a second Schedules page; the card is full-width
  // and one line under-used it, and the question a glance asks immediately after
  // "what's next" is "...and then?". Three answers that without becoming a
  // browser — the card still links to Upcoming for the whole list.
  const nextUps = useMemo(() => upcoming.filter((u) => u.at && Date.parse(u.at) > now).slice(0, 3), [upcoming, now]);


  // The app's section-eyebrow idiom (the Full score's type headers use it too).
  // Declared HERE, not at module scope: a module-level style object captures
  // whatever `c` held at import time and never repaints on the theme toggle.
  const eyebrow: CSSProperties = {
    fontSize: c.fontXs,
    fontFamily: c.sansCond,
    textTransform: "uppercase",
    letterSpacing: 0.7,
    color: c.textMuted,
  };

  // Job names and their types drive the full score's staves and sections. Every
  // job gets a staff, including one with no events in the window — that reads
  // correctly as a rest, and its absence would read as "no such job".
  const jobNames = useMemo(() => jobs.map((j) => j.name).filter(Boolean), [jobs]);
  const jobTypes = useMemo(() => Object.fromEntries(jobs.map((j) => [j.name, j.type])), [jobs]);

  return (
    <div>
      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {error && <div style={{ color: c.danger }}>Error: {error}</div>}
      {!loading && !error && (
        <>
          {/* ── The verdict (VU2-1) ──
              The page's thesis, first. This Dashboard answers one question —
              "is everything okay?" — and until now the answer was the LAST
              thing on it, below the tiles and the timeline, rendered at title
              weight with an accent edge precisely when nothing needed
              attention. Loud on a good day, buried on a bad one.

              CS-4 put the verdict after the timeline on purpose (words for the
              reader who will never hover the Score). VU2-Q1 reverses that
              deliberately: the reading order that serves an operator is verdict
              first, evidence after — and the Score is the evidence.

              It sits ABOVE the calendar banner because that banner is this
              strip's explanation: "healthy, and quiet because a freeze is on"
              reads in that order and not the other. */}
          <Verdict
            jobCount={jobs.length}
            attentionCount={attention.length}
            failedCount={failedCount}
            warnedCount={warnedCount}
            runningCount={runningCount}
            dismissed={verdictDismissed}
            onDismiss={dismissVerdict}
            onShowErrors={() => errorsRef.current?.scrollIntoView({ behavior: "smooth", block: "start" })}
            onShowRunning={() => navigate("/runs?result=running")}
          />

          {/* CAL-28 — a fleet-wide holiday or change freeze is the explanation for
              an otherwise alarming quiet dashboard, so it is stated here as well
              as on Schedules. Clicking through lands on the Calendars tab. */}
          <GlobalCalendarBanner onGoToCalendars={() => navigate("/schedules?tab=calendars")} />

          {/* ── Stat row (D-7 / VU-12) ──
              Four equal centred tiles gave a zero and a five-alarm failure the
              same visual weight. This row encodes urgency instead: the two
              tiles that can demand action carry a tint when they are non-zero
              and stay quiet when they are not, and the two reference counts are
              quiet always. StatTile's `tint` already supported this; the
              dashboard simply never used it. */}
          {/* Left-aligned and sized to their content rather than stretched across
              the page: four tiles spread edge-to-edge gave a zero the same
              presence as a five-alarm failure, which is the template pattern
              VU-12 names. The tiles keep StatTile's geometry — it is shared with
              Runners — but the row no longer pretends every number matters
              equally. */}
          <div style={{ display: "flex", gap: 12, marginBottom: 18, justifyContent: "flex-start" }}>
            <ClickableTile onClick={() => navigate("/runs?result=fail")} title="View failed runs in History">
              <div style={{ display: "flex", width: 190 }}>
                <StatTile
                  label="Failed (24h)"
                  value={String(failedCount)}
                  color={failedCount > 0 ? c.danger : c.textMuted}
                  tint={failedCount > 0 ? c.dangerBg : undefined}
                />
              </div>
            </ClickableTile>
            {/* VU2-2 — every tile in this row is a link now. Two of the four
                were inert, which in a row whose neighbours lift on hover reads
                as "this number is not about anything". `?result=running` needs
                no History work: resultFromParam folds any status alias through
                statusLabel, and RESULTS carries Running. */}
            <ClickableTile onClick={() => navigate("/runs?result=running")} title="View running jobs in History">
              <div style={{ display: "flex", width: 190 }}>
                <StatTile
                  label="Running now"
                  value={String(runningCount)}
                  color={runningCount > 0 ? c.info : c.textMuted}
                  tint={runningCount > 0 ? c.infoBg : undefined}
                />
              </div>
            </ClickableTile>
            <ClickableTile onClick={() => navigate("/schedules?tab=inventory")} title="View the schedule inventory">
              <div style={{ display: "flex", width: 170 }}>
                <StatTile label="Active schedules" value={String(scheduled.length)} color={c.textMuted} />
              </div>
            </ClickableTile>
            <ClickableTile onClick={() => navigate("/jobs")} title="View the job catalog">
              <div style={{ display: "flex", width: 170 }}>
                <StatTile label="Total jobs" value={String(jobs.length)} color={c.textMuted} />
              </div>
            </ClickableTile>
          </div>

          {/* CS-1 — the failed-run banner used to sit here. Current Status
              below replaced it: the banner named only the LATEST failure and
              truncated the rest to "+6 more", which is strictly less than the
              per-job list now says, so a bad day was double-red for no added
              information. Its one unique behaviour — re-showing after dismissal
              when a genuinely new failure arrived — has nothing to replace
              because Current Status has no dismissal to recover from; a new
              failure simply sorts to the top of Recent errors. */}

          {/* ── The Score (Phase D) ──
              Replaces the histogram, Upcoming, Recent Completions and Currently
              Running (VU-Q3(a)). Those four were one dataset split by tense,
              each rendering it as a different list, leaving the operator to
              reassemble "what happened and what is about to" in their head. */}
          {/* FX-1 — one panel. The full score is the same panel expanded, not a
              second block underneath it: the toggle lives in the Score's header
              and swaps the single lane for the per-job staves in place. The state
              stays here rather than inside Score so the Dashboard keeps owning
              what it feeds the panel. */}
          <Score
            marks={marks}
            now={now}
            hourInZone={hourInZone}
            labelInZone={labelInZone}
            stampInZone={stampInZone}
            timeInZone={timeInZone}
            runHref={(t) => `/runs?trace=${encodeURIComponent(t)}`}
            onOpenRun={(t) => navigate(`/runs?trace=${encodeURIComponent(t)}`)}
            truncatedAt={truncatedAt}
            loading={scoreRunsQ.loading}
            jobNames={jobNames}
            types={jobTypes}
            expanded={fullScore}
            onToggleExpand={() => setFullScore((v) => !v)}
          />

          {/* ── Current Status (CS-4) ──
              The verdict in words, for the reader who will never hover the
              timeline. Two blocks, in the order a glance wants them: what
              happens NEXT, then what went wrong.

              Two surfaces rather than one panel with a divider: Up Next is
              always-on reference and Recent errors is conditional urgency, so
              sharing a border would let a long error list visually swallow the
              schedule fact. */}
          <section aria-label="Current status">
            <h2 style={{ fontSize: c.fontHead, fontWeight: 600, color: c.text, margin: "0 0 10px" }}>Current status</h2>

            {/* Up next — the single forward-looking fact, from the projection
                feed the Score already consumes. */}
            <div style={{ marginBottom: 12 }}>
            <ClickableTile onClick={() => navigate("/schedules?tab=upcoming")} title="View every upcoming run">
              <div
                style={{
                  flex: 1,
                  border: `1px solid ${c.border}`,
                  borderRadius: c.radiusSurface,
                  background: c.panel,
                  padding: "12px 16px",
                }}
              >
                <div style={eyebrow}>Up next</div>
                {nextUps.length > 0 ? (
                  // VU2-3 — the soonest fire keeps the prominence it had; the
                  // two behind it render a step quieter, so the card still
                  // answers "what's next" before it answers "and then?".
                  <div style={{ display: "flex", flexDirection: "column", gap: 4, marginTop: 6 }}>
                    {nextUps.map((u, i) => (
                      <div key={`${u.ownerName ?? "run"}@${u.at}`} style={{ display: "flex", alignItems: "baseline", gap: 10, flexWrap: "wrap" }}>
                        <span style={{ fontSize: c.fontBody, fontWeight: 600, color: i === 0 ? c.text : c.textSec }}>{u.ownerName ?? "Scheduled run"}</span>
                        {/* The same tag the Score's readout uses, for the same
                            reason: a workflow's projection is not a job's. */}
                        {u.ownerKind === "workflow" && <span style={{ fontSize: c.fontXs, color: c.info }}>workflow</span>}
                        <span style={{ fontFamily: c.mono, fontSize: c.fontSm, color: c.textSec }}>{stampInZone(Date.parse(u.at!))}</span>
                        <span style={{ fontSize: c.fontXs, color: c.textMuted }}>{fmtWhen(u.at)}</span>
                      </div>
                    ))}
                  </div>
                ) : (
                  // RX-18 (§2.10) — the qualifier matters most exactly here.
                  // "Nothing is scheduled" reads as "nothing will run", and
                  // since reactions have no instant to project, that reading is
                  // now wrong whenever a cascade is armed. Naming the boundary
                  // in the empty state is the cheapest place to be honest about
                  // it; the full note lives on Upcoming.
                  <div style={{ marginTop: 6, fontSize: c.fontSm, color: c.textMuted }}>
                    Nothing is <em>scheduled</em> in the next 24 hours.
                    <span style={{ fontSize: c.fontXs }}> Reactions are not projected here — they fire when another definition finishes.</span>
                  </div>
                )}
              </div>
            </ClickableTile>
            </div>

            {/* Recent errors, and ONLY when there are errors. VU2-1 retired the
                all-clear card that used to fill this slot: the verdict strip at
                the top of the page now says "all healthy" once, quietly, and a
                second all-clear here was the same fact stated twice — the
                louder of the two being the one that fires when nothing is
                wrong. When the list is empty the section simply ends after Up
                next; the strip above has already answered. */}
            {attention.length > 0 && (
              <div ref={errorsRef} style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, background: c.panel, overflow: "hidden" }}>
                <div style={{ display: "flex", alignItems: "baseline", gap: 10, padding: "10px 14px", borderBottom: `1px solid ${c.borderLight}`, flexWrap: "wrap" }}>
                  <span style={eyebrow}>Recent errors</span>
                  <span style={{ fontSize: c.fontXs, color: c.textMuted }}>
                    {failedCount > 0 ? `${failedCount} failed` : ""}
                    {failedCount > 0 && warnedCount > 0 ? " · " : ""}
                    {warnedCount > 0 ? `${warnedCount} with warnings` : ""}
                    {" — last 24 hours"}
                  </span>
                </div>
                {attention.map((item, i) => (
                  <AttentionRow
                    key={item.uid || item.name}
                    name={attentionLabel(item)}
                    status={item.status}
                    count={item.count}
                    when={item.latestAt != null ? fmtWhen(new Date(item.latestAt).toISOString()) : "—"}
                    last={i === attention.length - 1 && !attentionTruncated}
                    // CS-3 — the job's OWN run record, not every run that shares
                    // its outcome. History filters `job` server-side, so the
                    // page and its total describe that job alone.
                    // R2F-3 — pivot by IDENTITY when the run carried one, so a
                    // shared name does not sweep the other department's runs
                    // into this job's history. Name for pre-backfill rows.
                    onClick={() =>
                      navigate(
                        item.uid
                          ? `/runs?jobUid=${encodeURIComponent(item.uid)}`
                          : `/runs?job=${encodeURIComponent(item.name)}`,
                      )
                    }
                  />
                ))}
                {attentionTruncated && (
                  <div
                    role="button"
                    tabIndex={0}
                    onClick={() => navigate("/runs?result=fail")}
                    onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); navigate("/runs?result=fail"); } }}
                    style={{ padding: "9px 14px", fontSize: c.fontXs, color: c.textMuted, cursor: "pointer" }}
                  >
                    Some older runs are not listed — open History for everything.
                  </div>
                )}
              </div>
            )}
          </section>
        </>
      )}
    </div>
  );
}

// One attention item: a job, its worst outcome in the window, how often, and how
// recently. The whole row deep-links into History filtered to THAT JOB (CS-3) —
// the question a failing row raises is "what happened to this one", which a
// list of every failure in the system does not answer.
function AttentionRow({
  name,
  status,
  count,
  when,
  last,
  onClick,
}: {
  name: string;
  status: string;
  count: number;
  when: string;
  /** Suppresses the divider so the panel's own border closes the list. */
  last: boolean;
  onClick: () => void;
}) {
  const [hover, setHover] = useState(false);
  const tone = statusTone(status);
  return (
    <div
      role="button"
      tabIndex={0}
      onClick={onClick}
      onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); onClick(); } }}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      data-testid="attention-row"
      title={`View ${name} in History`}
      style={{
        display: "flex",
        alignItems: "center",
        gap: 12,
        padding: "10px 14px",
        cursor: "pointer",
        background: hover ? c.panel2 : "transparent",
        borderBottom: last ? "none" : `1px solid ${c.borderLight}`,
      }}
    >
      <i aria-hidden style={{ width: 9, height: 9, borderRadius: "50%", background: tone.color, flexShrink: 0 }} />
      <span style={{ color: c.text, fontWeight: 600, fontSize: c.fontSm }}>{name}</span>
      <span style={{ color: tone.color, fontSize: c.fontXs }}>
        {statusLabel(status)}
        {count > 1 ? ` ×${count}` : ""}
      </span>
      <span style={{ marginLeft: "auto", color: c.textMuted, fontSize: c.fontXs }}>{when}</span>
    </div>
  );
}


// ClickableTile survives the Score: the stat row still deep-links into
// Schedules and History. Everything else that lived down here — JobHistogram,
// its Legend and range picker, Section, ExpandBtn, Row, Empty and the row
// typography — died with the four cards the Score replaced (VU-Q3(a)).

// ── Verdict (VU2-1) ──────────────────────────────────────────────────────────
// One line, at the top, stating what the whole page is about to show in detail.
//
// The three states are deliberately NOT symmetrical in weight. Calm is a quiet
// panel line: "everything is fine" is the expected reading, and a page that
// celebrates it teaches the reader to ignore the strip, which is exactly the
// muscle you do not want trained when the day it matters arrives. Attention
// takes the danger tint and the count. No-jobs is an invitation.
//
// Success green is deliberately absent from the calm state. VU-12 settled that
// vocabulary for the stat tiles — quiet IS the success signal here, and colour
// is reserved for what demands action.
//
// The attention state can be DISMISSED, and that is safe for exactly one
// reason: the dismissal is bound to the situation on screen (see attentionSig
// in the view), so it silences what the operator HAS seen and cannot swallow
// what they have not. Recent errors stays put either way — what gets quietened
// is the summary, never the detail.
//
// The strip does NOT name the next fire, though the VU2 plan first said it
// would. Up next owns that fact (CS-4: state it once — the all-clear used to
// repeat it, and two boxes carrying one fact read as a bug), and Up next has
// the room RX-18's reactions qualifier needs to make "nothing scheduled" true.
// Health here, schedule there: one home per idea.
function Verdict({
  jobCount,
  attentionCount,
  failedCount,
  warnedCount,
  runningCount,
  dismissed,
  onDismiss,
  onShowErrors,
  onShowRunning,
}: {
  jobCount: number;
  attentionCount: number;
  failedCount: number;
  warnedCount: number;
  runningCount: number;
  dismissed: boolean;
  onDismiss: () => void;
  onShowErrors: () => void;
  onShowRunning: () => void;
}) {
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

  if (jobCount === 0) {
    // The invitation the retired all-clear card used to carry, word for word.
    return (
      <div style={shell} aria-label="Status">
        <span style={{ fontWeight: 600, color: c.text }}>No jobs yet.</span>
        <span style={{ fontSize: c.fontSm, color: c.textSec }}>Create a job in Compose and it will report in here.</span>
        <span style={{ fontSize: c.fontSm, color: c.textSec }}>
          New to Cronomicon? <DocLink href={DOC_LINKS.operatorCourse}>Take the Operator Course</DocLink>
        </span>
      </div>
    );
  }

  if (attentionCount > 0) {
    // Dismissed for THIS situation. Recent errors is untouched below — silencing
    // the summary must not hide the detail, or dismissing would cost the
    // operator the very list the strip was pointing at.
    if (dismissed) return null;
    // Counts come from the EXACT totals (CC.23), not from the fetched page
    // lengths — the list below may be truncated, the verdict must not be.
    const parts = [failedCount > 0 ? `${failedCount} failed` : "", warnedCount > 0 ? `${warnedCount} warned` : ""].filter(Boolean);
    return (
      <div aria-label="Status" style={{ ...shell, border: `1px solid ${c.danger}`, background: c.dangerBg, gap: 0 }}>
        {/* The message and the dismiss control are two REAL buttons rather than
            one clickable div with an onClick span inside it: an interactive
            element nested in a role="button" is neither announced nor reachable
            correctly, and these two do opposite things — one takes you to the
            evidence, the other says you have already seen it. */}
        <button
          onClick={onShowErrors}
          title="Jump to the recent errors"
          style={{
            flex: 1,
            display: "flex",
            alignItems: "baseline",
            gap: 10,
            flexWrap: "wrap",
            background: "transparent",
            border: "none",
            padding: 0,
            textAlign: "left",
            font: "inherit",
            fontSize: c.fontBody,
            cursor: "pointer",
          }}
        >
          <span style={{ fontWeight: 600, color: c.danger }}>
            {attentionCount} job{attentionCount === 1 ? "" : "s"} need{attentionCount === 1 ? "s" : ""} attention.
          </span>
          <span style={{ fontSize: c.fontSm, color: c.textSec }}>{parts.join(" · ")} in the last 24 hours.</span>
        </button>
        <button
          onClick={onDismiss}
          aria-label="Dismiss this alert until something changes"
          title="Dismiss — returns if anything new fails or warns"
          style={{
            background: "transparent",
            border: "none",
            padding: "0 0 0 12px",
            color: c.danger,
            opacity: 0.7,
            fontSize: c.fontBody,
            lineHeight: 1,
            cursor: "pointer",
          }}
        >
          {/* The app's dismiss glyph (AlertBanner, Notice). */}
          ✕
        </button>
      </div>
    );
  }

  return (
    <div style={shell} aria-label="Status">
      <span style={{ fontWeight: 600, color: c.text }}>
        All {jobCount} job{jobCount === 1 ? "" : "s"} healthy.
      </span>
      <span style={{ fontSize: c.fontSm, color: c.textSec }}>Nothing has failed or warned in the last 24 hours.</span>
      {runningCount > 0 ? (
        <span
          role="button"
          tabIndex={0}
          onClick={onShowRunning}
          onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); onShowRunning(); } }}
          style={{ fontSize: c.fontSm, color: c.info, cursor: "pointer" }}
        >
          {runningCount} running now
        </span>
      ) : (
        <span style={{ fontSize: c.fontSm, color: c.textMuted }}>Nothing running.</span>
      )}
    </div>
  );
}

function ClickableTile({ onClick, title, children }: { onClick: () => void; title?: string; children: React.ReactNode }) {
  const [hover, setHover] = useState(false);
  return (
    <div
      role="button"
      tabIndex={0}
      onClick={onClick}
      onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); onClick(); } }}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      title={title}
      style={{
        // Sized by its child, not stretched: the stat row is left-aligned now
        // (D-7), and a `flex: 1` here made the clickable tiles grow while the
        // plain ones did not, so the row spaced unevenly at narrow widths.
        display: "flex",
        cursor: "pointer",
        borderRadius: c.radiusSurface,
        transition: "transform 0.15s, box-shadow 0.15s",
        transform: hover ? "translateY(-1px)" : "none",
        boxShadow: hover ? c.shadow : "none",
      }}
    >
      {children}
    </div>
  );
}


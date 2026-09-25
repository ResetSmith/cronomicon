import { useEffect, useState } from "react";
import { api } from "../api/client";
import { c } from "../theme";

// Run analytics (SL-E): stat tiles + most-common-failure list over a picked
// window. The per-day charts this panel once drew (OutcomeBars / DurationLine)
// were removed in JP-1 — the DurationTrend sparkline in RecentRuns.tsx is the
// one remaining run chart. Theme tokens are still read at render, never hoisted
// to module consts (theme-staleness rule).

type Failure = { reason: string; count: number };
// The API also returns per-day `buckets`; the charts that consumed them were
// removed (JP-1) and the field is deliberately not modeled here.
type Analytics = {
  windowDays: number;
  total: number;
  success: number;
  failure: number;
  warning: number;
  killed: number;
  skipped: number;
  running: number;
  successRate: number | null;
  p50Ms?: number | null;
  p95Ms?: number | null;
  maxMs?: number | null;
  slaBreaches: number;
  missedRuns: number;
  failures: Failure[];
};

const WINDOWS = [
  { label: "7d", days: 7 },
  { label: "30d", days: 30 },
  { label: "90d", days: 90 },
];

function fmtMs(ms?: number | null): string {
  if (ms == null || ms <= 0) return "—";
  if (ms < 1000) return `${ms}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)}s`;
  const m = Math.floor(s / 60);
  const rem = Math.round(s % 60);
  if (m < 60) return `${m}m ${rem}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

function Stat({ label, value, tone, hint }: { label: string; value: string; tone?: string; hint?: string }) {
  return (
    <div style={{ minWidth: 92 }} title={hint}>
      <div
        style={{
          fontSize: c.fontXs,
          fontFamily: c.sansCond,
          fontWeight: 700,
          letterSpacing: 0.6,
          textTransform: "uppercase",
          color: c.textSec,
        }}
      >
        {label}
      </div>
      <div style={{ fontSize: c.fontTitle, fontWeight: 700, color: tone ?? c.text }}>{value}</div>
    </div>
  );
}

export function RunAnalytics({ job, source }: { job?: string; source?: "git" | "cronomicon" }) {
  const [days, setDays] = useState(30);
  const [data, setData] = useState<Analytics | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    api
      .GET("/analytics/runs", { params: { query: { job, source, window: `${days}d` } } })
      .then(({ data: d }) => {
        if (cancelled) return;
        setData((d as Analytics) ?? null);
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [job, source, days]);

  const rate = data?.successRate;
  // Tone the rate rather than always printing it green: a number that is always
  // the same colour stops being read.
  const rateTone = rate == null ? c.textSec : rate >= 0.95 ? c.success : rate >= 0.8 ? c.warning : c.danger;

  return (
    <div>
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 10, paddingBottom: 8 }}>
        <span style={{ fontSize: c.fontXs, color: c.textSec }}>
          {loading
            ? "Loading…"
            : `${data?.total ?? 0} run${(data?.total ?? 0) === 1 ? "" : "s"} in the last ${
                data?.windowDays ?? days
              } days${(data?.skipped ?? 0) > 0 ? ` · ${data?.skipped} suppressed` : ""}`}
        </span>
        <div style={{ display: "flex", gap: 4 }}>
          {WINDOWS.map((wd) => (
            <button
              key={wd.days}
              onClick={() => setDays(wd.days)}
              style={{
                fontSize: c.fontXs,
                padding: "3px 9px",
                cursor: "pointer",
                borderRadius: c.radiusChip,
                border: `1px solid ${days === wd.days ? c.primary : c.border}`,
                background: days === wd.days ? `${c.primary}1a` : "transparent",
                color: days === wd.days ? c.primary : c.textSec,
              }}
            >
              {wd.label}
            </button>
          ))}
        </div>
      </div>

      <div style={{ display: "flex", flexWrap: "wrap", gap: 18, paddingBottom: 12 }}>
        <Stat
          label="Success rate"
          value={rate == null ? "—" : `${(rate * 100).toFixed(1)}%`}
          tone={rateTone}
          hint="Successes over terminal outcomes. Skipped runs are excluded — a job correctly suppressed by a calendar is not a failure."
        />
        <Stat label="Runs" value={String(data?.total ?? 0)} hint="Fires that actually executed. Suppressed fires are counted separately." />
        {(data?.skipped ?? 0) > 0 ? (
          <Stat
            label="Suppressed"
            value={String(data?.skipped ?? 0)}
            hint="Fires deliberately stopped — by a working calendar, a pause, or a concurrency policy. Not failures, and not runs."
          />
        ) : null}
        <Stat label="Failures" value={String(data?.failure ?? 0)} tone={(data?.failure ?? 0) > 0 ? c.danger : undefined} />
        <Stat label="Median" value={fmtMs(data?.p50Ms)} hint="Median duration across the window." />
        <Stat label="p95" value={fmtMs(data?.p95Ms)} hint="95th-percentile duration — the slow tail." />
        <Stat
          label="Late"
          value={String(data?.slaBreaches ?? 0)}
          tone={(data?.slaBreaches ?? 0) > 0 ? c.warning : undefined}
          hint="Runs warned as past their warn-after or must-finish-by deadline."
        />
        <Stat
          label="Missed"
          value={String(data?.missedRuns ?? 0)}
          tone={(data?.missedRuns ?? 0) > 0 ? c.warning : undefined}
          hint="Scheduled fires that produced no run, with no suppression on record to explain them."
        />
      </div>

      {(data?.failures?.length ?? 0) > 0 && (
        <div style={{ paddingTop: 14 }}>
          <div style={{ fontSize: c.fontXs, color: c.textSec, paddingBottom: 6 }}>
            Most common failure reasons
          </div>
          {data!.failures.map((f) => (
            <div
              key={f.reason}
              style={{ display: "flex", gap: 10, alignItems: "baseline", fontSize: c.fontSm, paddingBottom: 3 }}
            >
              <span style={{ color: c.danger, fontWeight: 700, minWidth: 26 }}>{f.count}</span>
              <span style={{ color: c.textSec, wordBreak: "break-word" }}>{f.reason}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

import { Fragment, useState } from "react";
import { c } from "../theme";
import type { components } from "../api/schema";
import { Badge, HoverTr, InlineLoading, Pager, SectionLabel, statusLabel, type PagerState } from "./ui";
import { TraceId } from "../views/history/shared";
import { RunLog } from "../views/history/RunLog";
import { fmtDuration } from "../utils/datetime";

// Shared run-history UI extracted from Jobs.tsx (v0.47.21) so the Runners view
// can reuse it for per-runner job history. Both callers fetch GET /runs with
// their own filter (job=… vs runnerId=…) and pass the resulting runs in; this
// module is purely presentational.

type Run = components["schemas"]["Run"];

const RECENT_RUNS_MAX_H = 340;

// Relative "3m ago" / "just now" formatter — a copy of the Jobs.tsx local helper
// (kept private here so the component stays self-contained).
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

// A function, not a module-level const, so a theme toggle re-reads the active
// palette (theme.ts: never capture token values in module-level constants).
// `sticky` is false once a row is expanded (FX-11): with the container's own
// scrollport gone, a sticky header would pin against the PAGE scroll and paint
// over the log the operator just opened.
const runTh = (sticky: boolean): React.CSSProperties => ({ textAlign: "left", padding: "7px 12px", fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textMuted, textTransform: "uppercase", letterSpacing: 0.7, whiteSpace: "nowrap", ...(sticky ? { position: "sticky" as const, top: 0, zIndex: 1 } : null), background: c.panel2 });
const runTd: React.CSSProperties = { padding: "7px 12px", whiteSpace: "nowrap" };

function runDotColor(status?: Run["status"]): string {
  if (status === "danger") return c.danger;
  if (status === "warning") return c.warning;
  return c.primary;
}

// Sparkline of recent run durations (oldest→newest, left→right). Dots are tinted
// by run result so a failing tail is visible at a glance. Pure SVG, no deps.
export function DurationTrend({ runs }: { runs: Run[] }) {
  const [hover, setHover] = useState<number | null>(null);
  const pts = runs
    .filter((r) => r.durationMs != null && r.status !== "running" && r.status !== "queued")
    .slice(0, 20)
    .reverse();
  if (pts.length < 2) return null;
  const vals = pts.map((r) => r.durationMs as number);
  const max = Math.max(...vals);
  const min = Math.min(...vals);
  const span = max - min || 1;
  const W = 280, H = 46, pad = 5;
  const coords = vals.map((v, i) => ({
    x: pad + (i / (vals.length - 1)) * (W - pad * 2),
    y: pad + (1 - (v - min) / span) * (H - pad * 2),
    status: pts[i].status,
    durationMs: v,
  }));
  const avg = Math.round(vals.reduce((a, b) => a + b, 0) / vals.length);
  return (
    // EV-2/R7 — frameless. This was a tinted, bordered card sitting beside a bare
    // table in the same column, so two lists of equal standing were contained
    // differently for no reason. The column split does the grouping now.
    <div style={{ display: "flex", alignItems: "center", gap: 18 }}>
      <div>
        <SectionLabel>Duration trend</SectionLabel>
        <div style={{ fontSize: c.fontSm, color: c.textSec }}>
          Last {pts.length} runs · avg <strong style={{ color: c.text }}>{fmtDuration(avg)}</strong>
        </div>
      </div>
      <div style={{ position: "relative", flexShrink: 0 }}>
        <svg width={W} height={H} style={{ display: "block" }}>
          <polyline points={coords.map((p) => `${p.x},${p.y}`).join(" ")} fill="none" stroke={c.primary} strokeWidth="1.5" strokeLinejoin="round" strokeLinecap="round" />
          {coords.map((p, i) => (
            <g key={i}>
              {/* The visible dot is non-interactive so the larger transparent hit
                  circle layered on top catches the hover across its whole area. */}
              <circle cx={p.x} cy={p.y} r={2.5} fill={runDotColor(p.status)} style={{ pointerEvents: "none" }} />
              <circle
                cx={p.x}
                cy={p.y}
                r={8}
                fill="transparent"
                style={{ cursor: "pointer" }}
                onMouseEnter={() => setHover(i)}
                onMouseLeave={() => setHover((h) => (h === i ? null : h))}
              />
            </g>
          ))}
        </svg>
        {hover != null && coords[hover] && (
          <div
            style={{
              position: "absolute",
              left: coords[hover].x,
              top: coords[hover].y - 8,
              transform: "translate(-50%, -100%)",
              padding: "3px 7px",
              background: c.panel,
              border: `1px solid ${c.border}`,
              borderRadius: c.radiusChip,
              fontSize: c.fontXs,
              fontWeight: 600,
              color: c.text,
              whiteSpace: "nowrap",
              pointerEvents: "none",
              boxShadow: "0 2px 10px rgba(0,0,0,0.35)",
              zIndex: 5,
            }}
          >
            {fmtDuration(coords[hover].durationMs)}
          </div>
        )}
      </div>
    </div>
  );
}

// Recent runs table, each row expandable to its redacted log (same drill-in
// pattern as the History › Executions tab). `label` / `emptyText` let each caller
// EV-1 — the CALLER supplies the section caption (wrap this in a `Section`).
// It used to own an optional `label`, which left Runners passing `label=""` to
// suppress a caption it had already rendered from outside while Jobs relied on
// the default — two conventions for one component. Only `emptyText` stays
// per-caller, because the empty state is genuinely view-specific.
export function RecentRuns({
  runs,
  loading,
  error,
  emptyText = "No runs recorded yet.",
  pager,
  page = 0,
  total = 0,
  noun = "runs",
}: {
  runs: Run[];
  loading: boolean;
  error: string | null;
  emptyText?: string;
  /** Optional server-side paging (PP-H7 pattern): when the caller owns a
   * usePager() and fetches per page, pass it (plus page/total) and the table
   * grows a Pager footer. The footer sits INSIDE the border but OUTSIDE the
   * scrollport, so it stays put while the rows scroll. Rendered only once the
   * history outgrows a single page — an expanded row with three runs should not
   * carry a full pager strip. */
  pager?: PagerState;
  page?: number;
  total?: number;
  noun?: string;
}) {
  const [openTrace, setOpenTrace] = useState<string | null>(null);
  // Keyed off a row that is actually present, not merely off a remembered trace
  // id: a refetch that drops the expanded run must restore the cap with it.
  const expanded = runs.some((r) => r.traceId === openTrace);
  const showPager = !!pager && (total > pager.pageSize || page > 0);
  return (
    <div>
      {loading ? (
        <InlineLoading what="runs" />
      ) : error ? (
        <div style={{ fontSize: c.fontSm, color: c.danger, padding: "8px 0" }}>{error}</div>
      ) : runs.length === 0 ? (
        <div style={{ fontSize: c.fontSm, color: c.textMuted, padding: "8px 0" }}>{emptyText}</div>
      ) : (
        // Cap the list at ~10 rows tall and scroll the rest. The header is sticky
        // so the columns stay labelled while scrolling.
        //
        // FX-11 — the cap applies to the COLLAPSED list only. RunLog's <pre> is
        // 400px tall, so an expanded drill-in inside a 340px scrollport produced
        // nested scrollbars, shoved the clicked row out of view by its own
        // expansion, and let the sticky <th> paint over the top of the log. A log
        // you deliberately expanded is the thing you came to read, so the container
        // grows to it instead.
        <div style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, overflow: "hidden" }}>
        <div style={{ maxHeight: expanded ? undefined : RECENT_RUNS_MAX_H, overflowY: expanded ? "visible" : "auto", overflowX: "auto" }}>
          <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm }}>
            <thead>
              <tr style={{ background: c.panel2 }}>
                <th style={runTh(!expanded)}>Trace</th>
                <th style={runTh(!expanded)}>Result</th>
                <th style={runTh(!expanded)}>Started</th>
                <th style={runTh(!expanded)}>Duration</th>
                <th style={runTh(!expanded)}>Exit</th>
                <th style={runTh(!expanded)}>By</th>
              </tr>
            </thead>
            <tbody>
              {runs.map((r) => {
                const open = openTrace === r.traceId;
                return (
                  <Fragment key={r.traceId}>
                    <HoverTr
                      onClick={() => setOpenTrace(open ? null : (r.traceId ?? null))}
                      tint={open ? c.primaryBg : undefined}
                      hoverTint={open ? c.primaryBg : c.panelHover}
                    >
                      <td style={runTd}><TraceId id={r.traceId} color={c.accent} /></td>
                      <td style={runTd}>{r.status ? <Badge status={r.status} label={statusLabel(r.status)} /> : "—"}</td>
                      <td style={{ ...runTd, color: c.textSec }}>{fmtWhen(r.startedAt)}</td>
                      <td style={{ ...runTd, fontFamily: c.mono }}>{r.durationMs != null ? fmtDuration(r.durationMs) : "—"}</td>
                      <td style={{ ...runTd, fontFamily: c.mono, color: r.exitCode ? c.danger : c.textSec }}>{r.exitCode ?? "—"}</td>
                      <td style={{ ...runTd, color: c.textSec }}>{r.manual ? (r.triggeredBy ?? "—") : "Cronomicon"}</td>
                    </HoverTr>
                    {open && r.traceId && (
                      <tr>
                        <td colSpan={6} style={{ padding: "10px 14px", background: c.panel, borderBottom: `1px solid ${c.border}` }}>
                          {/* width:0 + minWidth:100% collapses this cell's intrinsic
                              width so an unbreakable log line (a long path, a URL, a
                              base64 blob) can never inflate the table and drag the
                              run columns wide with it — the overflow scrolls inside
                              RunLog's own <pre> (overflowX:auto), which is the view
                              the operator is actually reading. */}
                          <div style={{ width: 0, minWidth: "100%" }}>
                            {r.statusReason && (
                              <div style={{ fontSize: c.fontXs, color: c.warning, marginBottom: 8 }}>Reason: {r.statusReason.replace(/_/g, " ")}</div>
                            )}
                            {/* EP-8b — this is the drill-in an operator opens right
                                after pressing Run, so it is the surface tailing
                                matters most on. */}
                            <RunLog traceId={r.traceId} label="" active={r.status === "running" || r.status === "queued"} />
                          </div>
                        </td>
                      </tr>
                    )}
                  </Fragment>
                );
              })}
            </tbody>
          </table>
        </div>
        {showPager && <Pager pager={pager!} page={page} total={total} noun={noun} shown={runs.length} />}
        </div>
      )}
    </div>
  );
}

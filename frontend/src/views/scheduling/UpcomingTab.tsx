import { useState } from "react";
import { api, csrfHeader } from "../../api/client";
import { useGet, rows, useClientPager, useColumnWidths, useTableSort } from "../../hooks";
import { ColumnsMenu, TableHead, renderCells, useTableColumns } from "../../components/table";
import { type SortColumn } from "../../utils/sort";
import { c } from "../../theme";
import { ReactionsNotProjectedNote } from "./ReactionsNotProjectedNote";
import { Btn, HoverTr, Pager, SkeletonRows, TableSurface } from "../../components/ui";
import { OwnerBadge, fmtWhen, th, td } from "./ui";
import { useNameDisambiguator } from "../../utils/disambiguate";

// One projected fire from GET /schedules/upcoming.
interface ScheduleUpcoming {
  ownerKind?: string;
  ownerName?: string;
  // R2F-3 — the owning definition's identity and derived agencies. A workflow
  // owner carries no agencies (it has no scope of its own), so it renders bare.
  ownerUid?: string;
  ownerAgencies?: string[];
  scheduleName?: string;
  cron?: string;
  at?: string;
  // AR — a deferred ad-hoc run (a parked manual trigger), cancellable by id.
  adHoc?: boolean;
  pendingId?: string;
  scheduledBy?: string;
  missReason?: string;
  // QP — parked behind a held concurrency gate rather than behind the clock.
  // It has no future instant to show: its `at` is when it JOINED the queue.
  queued?: boolean;
  concurrencyKey?: string;
  // CAL-11 — a working calendar will suppress this fire. Annotated, never
  // dropped: Upcoming is the one place a holiday gap is worth seeing.
  suppressed?: boolean;
  suppressedBy?: string;
  suppressedLabel?: string;
}

type Window = "24h" | "7d";

// The server caps the projection (perEntryCap=200 / totalCap=500). When the list
// reaches the cap the projection was clipped, so a caption marks the count as a
// floor rather than letting "476–500 of 500" read as complete (PP).
const PROJECTION_CAP = 500;

// Default column widths (px) so table-layout:fixed has a sensible starting point
// before the user drags; the sum (810) equals the table minWidth. Mirrors the
// sibling InventoryTab (V1.1-7). Stored overrides come from useColumnWidths.
const COL_W: Record<string, number> = {
  when: 180,
  type: 90,
  owner: 220,
  schedule: 180,
  cron: 140,
};

// Sortable columns (Phase 2, the sorting-update plan §3.2), driven by
// useTableSort. When defaults ASCENDING — this table IS a time projection, so
// soonest-first is its natural order (§3.1) — hence the explicit defaultDir
// overriding the date-type desc default. Cron stays unsortable.
const SORT_COLS: SortColumn<ScheduleUpcoming>[] = [
  { key: "when", get: (f) => f.at, type: "date", defaultDir: "asc" },
  { key: "type", get: (f) => f.ownerKind, type: "text" },
  { key: "owner", get: (f) => f.ownerName, type: "text" },
  { key: "schedule", get: (f) => f.scheduleName, type: "text" },
];

// `onGoToTab` is supplied by the Schedules host (the tab index is its state, so a URL
// link would not switch it) — an empty projection hands the operator the Inventory,
// which is where a paused or disabled schedule explains itself (VU-14).
export function UpcomingTab({ onGoToTab }: { onGoToTab?: (i: number) => void }) {
  const [window, setWindow] = useState<Window>("24h");
  const [refresh, setRefresh] = useState(0);
  const { data, error, loading } = useGet<unknown>(
    () => api.GET("/schedules/upcoming", { params: { query: { window } } }),
    [window, refresh],
  );
  // AR — cancel a deferred ad-hoc run, then refetch so the row disappears.
  const [cancelling, setCancelling] = useState<string | null>(null);
  const cancelPending = async (id: string) => {
    setCancelling(id);
    await api.DELETE("/pending-runs/{id}", { params: { path: { id }, header: csrfHeader } } as never);
    setCancelling(null);
    setRefresh((n) => n + 1);
  };
  const missed = ((data as { missedAdHoc?: ScheduleUpcoming[] } | undefined)?.missedAdHoc ?? []) as ScheduleUpcoming[];
  // Soonest-first projection (server-sorted). A flat column table, paged
  // client-side over the already-capped array — no day-header grouping, so the
  // slice is a plain window with constant rows per page (PP).
  const items = rows<ScheduleUpcoming>(data);
  // R2F-3 — projected fires and missed ones are ONE visible set: a name shared
  // by two owners should badge in both lists or neither.
  const ownerLabel = useNameDisambiguator([...items, ...missed], (f) => ({
    uid: f.ownerUid,
    name: f.ownerName,
    agencies: f.ownerAgencies,
    group: f.ownerKind,
  }));
  // CAL-11 — suppressed instants stay in the list (struck through) but are worth
  // counting separately: "12 projected runs" reads wrong when three of them will
  // not happen.
  const suppressedCount = items.filter((f) => f.suppressed).length;
  const cw = useColumnWidths("schedule-upcoming");
  // Sort feeds INTO the client pager (§3.1): the pager slices the sorted array.
  const sort = useTableSort(items, SORT_COLS, { key: "when", dir: "asc" }, { tableId: "schedule-upcoming" });
  const { pageItems, total, page, pager } = useClientPager(sort.sorted);

  // CO-4 — the column spec. Built in render: every cell reads `c.*`, and the
  // Cron cell closes over `cancelling`/`cancelPending`.
  const cols = useTableColumns<ScheduleUpcoming>("schedule-upcoming", [
    {
      key: "when",
      label: "When",
      sortKey: "when",
      width: COL_W.when,
      pin: "first",
      cell: (f) => (
        <span
          style={{
            fontFamily: c.mono,
            color: f.suppressed ? c.textMuted : c.text,
            textDecoration: f.suppressed ? "line-through" : undefined,
          }}
        >
          {/* QP: a queued row's `at` is when it joined the line, not when it will
              fire — which nobody knows. Saying "waiting" is honest; rendering a
              past timestamp as a future run is not. */}
          {f.queued ? "waiting for the gate" : f.at ? fmtWhen(f.at) : "—"}
        </span>
      ),
      tdStyle: { whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" },
    },
    { key: "type", label: "Type", sortKey: "type", width: COL_W.type, fixed: true, cell: (f) => <OwnerBadge kind={f.ownerKind} /> },
    {
      key: "owner",
      label: "Owner",
      sortKey: "owner",
      width: COL_W.owner,
      tdStyle: { fontWeight: 600, overflow: "hidden", textOverflow: "ellipsis" },
      cell: (f) => (
        <span style={{ color: f.suppressed ? c.textMuted : c.text }}>
          <span style={{ textDecoration: f.suppressed ? "line-through" : undefined }}>{ownerLabel(f)}</span>
          {/* CAL-11 — the reason chip: a struck-through row with no explanation
              is a mystery, and naming the calendar is exactly what makes a
              holiday gap legible. */}
          {f.suppressed && (
            <div
              title={`Suppressed by the working calendar "${f.suppressedBy}"${f.suppressedLabel ? ` — ${f.suppressedLabel}` : ""}. It will not run.`}
              style={{ marginTop: 3, display: "inline-block", fontSize: c.fontXs, fontWeight: 600, fontFamily: c.sans, padding: "1px 7px", borderRadius: c.radiusPill, background: `${c.textMuted}18`, color: c.textMuted, border: `1px solid ${c.textMuted}40`, whiteSpace: "nowrap", maxWidth: "100%", overflow: "hidden", textOverflow: "ellipsis" }}
            >
              skipped · {f.suppressedLabel || f.suppressedBy}
            </div>
          )}
        </span>
      ),
    },
    {
      key: "schedule",
      label: "Schedule",
      sortKey: "schedule",
      width: COL_W.schedule,
      tdStyle: { fontFamily: c.mono, color: c.textSec, overflow: "hidden", textOverflow: "ellipsis" },
      cell: (f) =>
        f.queued ? (
          <span
            title={`Waiting for the running job to finish${f.concurrencyKey ? ` (key ${f.concurrencyKey})` : ""}`}
            style={{ fontFamily: "inherit", fontSize: c.fontXs, fontWeight: 600, padding: "2px 8px", borderRadius: c.radiusPill, background: `${c.warning}18`, color: c.warning, border: `1px solid ${c.warning}40` }}
          >
            queued
          </span>
        ) : f.adHoc ? (
          <span
            title={f.scheduledBy ? `Scheduled by ${f.scheduledBy}` : undefined}
            style={{ fontFamily: "inherit", fontSize: c.fontXs, fontWeight: 600, padding: "2px 8px", borderRadius: c.radiusPill, background: `${c.info}18`, color: c.info, border: `1px solid ${c.info}40` }}
          >
            ad-hoc
          </span>
        ) : (
          f.scheduleName
        ),
    },
    {
      key: "cron",
      label: "Cron",
      width: COL_W.cron,
      tdStyle: { fontFamily: c.mono, color: c.textMuted, overflow: "hidden", textOverflow: "ellipsis" },
      cell: (f) =>
        (f.adHoc || f.queued) && f.pendingId ? (
          <Btn small disabled={cancelling === f.pendingId} onClick={() => cancelPending(f.pendingId!)}>
            {cancelling === f.pendingId ? "Cancelling…" : "Cancel"}
          </Btn>
        ) : (
          f.cron
        ),
    },
  ]);

  return (
    <div>
      <div style={{ display: "flex", gap: 4, marginBottom: 16, alignItems: "center" }}>
        {(["24h", "7d"] as Window[]).map((w) => (
          <button
            key={w}
            onClick={() => {
              setWindow(w);
              pager.setPage(0);
            }}
            style={{
              padding: "6px 14px",
              fontSize: c.fontSm,
              fontWeight: window === w ? 600 : 400,
              border: `1px solid ${window === w ? c.primary : c.border}`,
              borderRadius: c.radiusChip,
              cursor: "pointer",
              background: window === w ? c.primary : "transparent",
              color: window === w ? c.onSolid : c.textSec,
              fontFamily: "inherit",
            }}
          >
            Next {w === "24h" ? "24 hours" : "7 days"}
          </button>
        ))}
        <span style={{ color: c.textSec, fontSize: c.fontXs, marginLeft: "auto" }}>
          {items.length} projected {items.length === 1 ? "run" : "runs"}
          {suppressedCount > 0 && ` · ${suppressedCount} suppressed`}
        </span>
        <ColumnsMenu cols={cols} cw={cw} />
      </div>

      {/* RX-18 (§2.10) — the cost this feature makes Upcoming pay, stated here
          rather than discovered. Upcoming projects INSTANTS; a reaction has
          none, so this table stopped being the complete answer to "what will
          run" the moment reactions shipped. CAL 1C set the precedent for saying
          so in place: a projection that quietly omits a whole trigger kind is
          worse than one that admits its own boundary. */}
      <ReactionsNotProjectedNote onGoToTab={onGoToTab} />

      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {error && <div style={{ color: c.danger }}>Error: {error}</div>}

      {!loading && !error && (
        <>
          {/* The table frame is a top and bottom rule, not a fourth box (VU-5);
              Pager brings its own top rule, so it reads as the table footer. */}
          <TableSurface>
            <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm, tableLayout: "fixed", minWidth: cols.minWidth }}>
              <thead>
                <TableHead
                  columns={cols.visible}
                  sort={sort}
                  cw={cw}
                  thStyle={th}
                  trStyle={{ color: c.textSec, borderBottom: `1px solid ${c.border}` }}
                />
              </thead>
              <tbody>
                {/* VU-14 — an empty projection has two plausible causes and one
                    control each: the window is too short (widen it), or every
                    schedule is paused/disabled (the Inventory says which). */}
                {items.length === 0 && (
                  <tr>
                    <td colSpan={cols.visible.length} style={{ ...td, color: c.textSec, textAlign: "center", padding: "28px 14px" }}>
                      <div>No runs scheduled in this window. Active, unpaused schedules with a future fire appear here.</div>
                      <div style={{ marginTop: 12, display: "flex", gap: 8, justifyContent: "center" }}>
                        {window === "24h" && (
                          <Btn
                            small
                            onClick={() => {
                              setWindow("7d");
                              pager.setPage(0);
                            }}
                          >
                            Look ahead 7 days
                          </Btn>
                        )}
                        {onGoToTab && (
                          <Btn small onClick={() => onGoToTab(1)}>
                            Check the Inventory
                          </Btn>
                        )}
                      </div>
                    </td>
                  </tr>
                )}
                {pageItems.map((f, i) => (
                  <HoverTr
                    key={`${f.ownerName}/${f.scheduleName}/${f.at}/${i}`}
                    hoverTint={c.panelHover}
                    style={{ borderBottom: `1px solid ${c.border}` }}
                  >
                    {renderCells(cols.visible, f, { base: td })}
                  </HoverTr>
                ))}
              </tbody>
            </table>
            {items.length > 0 && <Pager pager={pager} page={page} total={total} noun="runs" />}
          </TableSurface>
          {missed.length > 0 && (
            <div style={{ marginTop: 12, padding: "10px 14px", border: `1px solid ${c.warning}40`, background: `${c.warning}10`, borderRadius: c.radiusSurface, fontSize: c.fontSm, color: c.textSec }}>
              <strong style={{ color: c.warning }}>{missed.length} missed {missed.every((m) => m.adHoc) ? "ad-hoc " : ""}{missed.length === 1 ? "run" : "runs"}</strong> — scheduled instants that passed while the run could not fire (server downtime past the 24-hour catch-up, a gate that never cleared, or the target was removed). Each stays listed until dismissed:
              <div style={{ marginTop: 6, display: "flex", flexDirection: "column", gap: 4 }}>
                {missed.map((m) => (
                  <div key={m.pendingId} style={{ display: "flex", alignItems: "center", gap: 8 }}>
                    <span style={{ fontFamily: c.mono }}>{ownerLabel(m)}</span>
                    <span style={{ color: c.textMuted }}>was to run {m.at ? fmtWhen(m.at) : "—"}{m.scheduledBy ? ` · by ${m.scheduledBy}` : ""}{m.missReason ? ` · ${m.missReason}` : ""}</span>
                    {m.pendingId && (
                      <Btn small disabled={cancelling === m.pendingId} onClick={() => cancelPending(m.pendingId!)}>
                        Dismiss
                      </Btn>
                    )}
                  </div>
                ))}
              </div>
            </div>
          )}
          {items.length >= PROJECTION_CAP && (
            <div style={{ marginTop: 10, fontSize: c.fontXs, color: c.textMuted }}>
              Showing the first {PROJECTION_CAP} projected runs — the projection was capped, so later fires aren’t listed.
            </div>
          )}
        </>
      )}
    </div>
  );
}

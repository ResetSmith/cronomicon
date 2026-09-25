import { useState } from "react";
import { api } from "../../api/client";
import { useGet, rows, useColumnWidths, useTableSort } from "../../hooks";
import { ColumnsMenu, TableHead, renderCells, useTableColumns } from "../../components/table";
import { type SortColumn } from "../../utils/sort";
import { c } from "../../theme";
import { Btn, SkeletonRows, TableSurface } from "../../components/ui";
import { describeSpec } from "./cron";
import { useCalendars } from "./calendars";
import { OwnerBadge, StateBadge, WindowNote, fmtWhen, th, td } from "./ui";

// One row of GET /schedules — the schedule inventory across jobs + workflows.
interface ScheduleEntryStatus {
  ownerKind?: string;
  ownerName?: string;
  // Third segment of the "kind:source:name" key the calendarRollup map is keyed
  // by — a definition name can recur across the git and cronomicon sources.
  ownerSource?: string;
  scheduleName?: string;
  cron?: string;
  env?: Record<string, string> | null;
  enabled?: boolean;
  paused?: boolean;
  nextRunAt?: string | null;
  lastRunAt?: string | null;
  // Activation window (AW-13) + the backend's derived state for it.
  startAt?: string | null;
  endAt?: string | null;
  windowState?: string | null;
  // Phase 2 firing modes: interval-driven and one-shot entries carry no cron.
  interval?: string | null;
  mode?: string | null;
  // CAL-5 — this entry's own working-calendar bindings, in both polarities.
  skipCalendars?: string[];
  onlyCalendars?: string[];
}

const KINDS = ["All", "Jobs", "Workflows"];

// Default column widths (px) so table-layout:fixed has a sensible starting point
// before the user drags (V1.1-7). Stored overrides come from useColumnWidths.
const COL_W: Record<string, number> = {
  owner: 200,
  entry: 180,
  cron: 160,
  env: 130,
  state: 120,
  next: 150,
  last: 150,
};

// Sortable columns (Phase 2, the sorting-update plan §3.2), driven by
// useTableSort. State sorts by semantic rank, worst first when ascending
// (TS-Q5): paused is the warning-toned attention state, disabled a deliberate
// off, active healthy — StateBadge derives the same label from the two flags.
// Env sorts by the var count the cell shows ("—" rows sort last); Cron stays
// unsortable.
// pending/expired sit between "deliberately off" and "healthy": they are not
// attention states like paused, but they are not firing either.
const STATE_RANK: Record<string, number> = { paused: 0, disabled: 1, expired: 2, pending: 3, active: 4 };
const stateOf = (e: ScheduleEntryStatus): string =>
  e.paused
    ? "paused"
    : !e.enabled
      ? "disabled"
      : e.windowState === "pending" || e.windowState === "expired"
        ? e.windowState
        : "active";
const SORT_COLS: SortColumn<ScheduleEntryStatus>[] = [
  { key: "owner", get: (e) => e.ownerName, type: "text" },
  { key: "entry", get: (e) => e.scheduleName, type: "text" },
  {
    key: "env",
    get: (e) => {
      const n = e.env ? Object.keys(e.env).length : 0;
      return n > 0 ? n : undefined; // "—" cells sort last, like other empties
    },
    type: "text", // numeric-aware localeCompare orders the counts correctly
  },
  { key: "state", get: stateOf, type: "rank", rank: STATE_RANK },
  { key: "next", get: (e) => e.nextRunAt, type: "date" },
  { key: "last", get: (e) => e.lastRunAt, type: "date" },
];

// `onGoToTab` is supplied by the Schedules host so an empty projection can hand the
// operator the Catalog tab, where schedules are actually authored (VU-14). The tab
// index lives in the host's state, so a URL link would not switch it.
export function InventoryTab({ onGoToTab }: { onGoToTab?: (i: number) => void }) {
  const { data, error, loading } = useGet<unknown>(() => api.GET("/schedules"));
  const all = rows<ScheduleEntryStatus>(data);
  // CAL-12 — the server's per-definition roll-up, keyed "kind:source:name" and
  // already restricted to definitions this caller may read. Taken from the
  // server rather than recomputed here: the inventory rows are the whole fleet,
  // so a client recomputation over them would be right by accident, and wrong
  // the moment the projection is paged or filtered.
  const rollup = (data as { calendarRollup?: Record<string, string> } | undefined)?.calendarRollup ?? {};
  // CAL-16 — a binding whose calendar no longer exists (force-deleted) does
  // nothing, and fails toward its polarity's intent: a dangling skip FIRES, a
  // dangling only NEVER fires. Both are invisible without this badge.
  const { calendars } = useCalendars();
  const known = new Set(calendars.map((cal) => cal.name ?? ""));
  const danglingOf = (e: ScheduleEntryStatus) =>
    [...(e.skipCalendars ?? []), ...(e.onlyCalendars ?? [])].filter((n) => !known.has(n));
  const rollupFor = (e: ScheduleEntryStatus) => rollup[`${e.ownerKind}:${e.ownerSource}:${e.ownerName}`] ?? "";

  const [kind, setKind] = useState("All");
  const [search, setSearch] = useState("");
  const cw = useColumnWidths("schedule-inventory");
  const q = search.toLowerCase();

  const items = all.filter(
    (e) =>
      (kind === "All" ||
        (kind === "Jobs" && e.ownerKind === "job") ||
        (kind === "Workflows" && e.ownerKind === "workflow")) &&
      (!q || (e.ownerName ?? "").toLowerCase().includes(q) || (e.scheduleName ?? "").toLowerCase().includes(q)),
  );

  // Sort after the kind/search filter (§3.1).
  const sort = useTableSort(items, SORT_COLS, { key: "owner", dir: "asc" }, { tableId: "schedule-inventory" });

  // VU-14 — hands the search + kind filter back from the filtered-empty state.
  const clearFilters = () => {
    setSearch("");
    setKind("All");
  };

  // CO-4 — the column spec, built in render (cells read `c.*` and close over the
  // calendar rollup/dangling resolvers).
  const cols = useTableColumns<ScheduleEntryStatus>("schedule-inventory", [
    {
      key: "owner",
      label: "Owner",
      sortKey: "owner",
      width: COL_W.owner,
      pin: "first",
      cell: (e) => (
        <>
          <span style={{ display: "inline-flex", gap: 8, alignItems: "center" }}>
            <OwnerBadge kind={e.ownerKind} />
            <span style={{ fontWeight: 600 }}>{e.ownerName}</span>
          </span>
          {/* CAL-12 — the single-place answer to "does this run on holidays?",
              stated on the owner rather than costing a walk over every one of
              its entries. */}
          {rollupFor(e) && (
            <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 3 }} title="Working calendars bound across this definition's schedule entries">
              🗓 {rollupFor(e)}
            </div>
          )}
        </>
      ),
    },
    {
      key: "entry",
      label: "Entry",
      sortKey: "entry",
      width: COL_W.entry,
      tdStyle: { fontFamily: c.mono, color: c.textSec },
      cell: (e) => (
        <>
          {e.scheduleName}
          {danglingOf(e).length > 0 && (
            <div
              style={{ fontFamily: c.sans, fontSize: c.fontXs, color: c.warning, marginTop: 3 }}
              title="This entry names a working calendar that no longer exists. A dangling skip binding lets the schedule fire; a dangling run-day binding means it never fires."
            >
              ⚠ missing calendar: {danglingOf(e).join(", ")}
            </div>
          )}
        </>
      ),
    },
    {
      key: "cron",
      label: "Cron",
      width: COL_W.cron,
      cell: (e) => (
        <>
          <div style={{ fontFamily: c.mono }}>{e.cron || e.interval || (e.mode === "once" ? "once" : "")}</div>
          <div style={{ fontSize: c.fontXs, color: c.textSec }}>{describeSpec(e)}</div>
        </>
      ),
    },
    {
      key: "env",
      label: "Env",
      sortKey: "env",
      width: COL_W.env,
      cell: (e) => {
        const n = e.env ? Object.keys(e.env).length : 0;
        return <span style={{ color: n ? c.text : c.textMuted }}>{n ? `${n} var${n === 1 ? "" : "s"}` : "—"}</span>;
      },
    },
    {
      key: "state",
      label: "State",
      sortKey: "state",
      width: COL_W.state,
      fixed: true,
      cell: (e) => (
        <div style={{ display: "flex", flexDirection: "column", gap: 3, alignItems: "flex-start" }}>
          <StateBadge enabled={e.enabled} paused={e.paused} windowState={e.windowState} />
          <WindowNote startAt={e.startAt} endAt={e.endAt} />
        </div>
      ),
    },
    {
      key: "next",
      label: "Next run",
      sortKey: "next",
      width: COL_W.next,
      fixed: true,
      tdStyle: { color: c.textSec, whiteSpace: "nowrap" },
      cell: (e) => fmtWhen(e.nextRunAt),
    },
    {
      key: "last",
      label: "Last run",
      sortKey: "last",
      width: COL_W.last,
      fixed: true,
      tdStyle: { color: c.textSec, whiteSpace: "nowrap" },
      cell: (e) => fmtWhen(e.lastRunAt),
    },
  ]);

  return (
    <div>
      <div style={{ display: "flex", gap: 10, marginBottom: 14, alignItems: "center", flexWrap: "wrap" }}>
        <input
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Search owner or entry…"
          style={{
            border: `1px solid ${c.borderStrong}`,
            borderRadius: c.radiusChip,
            padding: "7px 12px",
            fontSize: c.fontSm,
            background: c.panelInput,
            color: c.text,
            fontFamily: "inherit",
            outline: "none",
            minWidth: 220,
          }}
        />
        <div style={{ display: "flex", gap: 4 }}>
          {KINDS.map((k) => (
            <button
              key={k}
              onClick={() => setKind(k)}
              style={{
                padding: "6px 12px",
                fontSize: c.fontSm,
                fontWeight: kind === k ? 600 : 400,
                border: `1px solid ${kind === k ? c.primary : c.border}`,
                borderRadius: c.radiusChip,
                cursor: "pointer",
                background: kind === k ? c.primary : "transparent",
                color: kind === k ? c.onSolid : c.textSec,
                fontFamily: "inherit",
              }}
            >
              {k}
            </button>
          ))}
        </div>
        <span style={{ color: c.textSec, fontSize: c.fontXs, marginLeft: "auto" }}>
          {items.length} {items.length === 1 ? "entry" : "entries"}
        </span>
        <ColumnsMenu cols={cols} cw={cw} />
      </div>

      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {error && <div style={{ color: c.danger }}>Error: {error}</div>}

      {!loading && !error && (
        // The table frame is a top and bottom rule, not a fourth box around a
        // page that is already a box inside a box (VU-5).
        <TableSurface>
          <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm, tableLayout: "fixed" }}>
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
              {/* VU-14 — this tab is a read-only projection: nothing here creates a
                  schedule, so the empty catalog points at the Catalog tab (where the
                  Schedule Builder lives) instead of only naming a YAML key. The
                  filtered case gets its filters back. */}
              {items.length === 0 && (
                <tr>
                  <td colSpan={cols.visible.length} style={{ ...td, color: c.textSec, textAlign: "center", padding: "28px 14px" }}>
                    {all.length === 0 ? (
                      <>
                        <div>
                          No schedules yet. A schedule comes from spec.schedule / spec.schedules on a job or workflow,
                          or from the Catalog tab.
                        </div>
                        {onGoToTab && (
                          <div style={{ marginTop: 12 }}>
                            <Btn small onClick={() => onGoToTab(0)}>
                              Go to Catalog
                            </Btn>
                          </div>
                        )}
                      </>
                    ) : (
                      <>
                        <div>No schedules match the current filter.</div>
                        <div style={{ marginTop: 12 }}>
                          <Btn small onClick={clearFilters}>
                            Clear filters
                          </Btn>
                        </div>
                      </>
                    )}
                  </td>
                </tr>
              )}
              {sort.sorted.map((e, i) => (
                <tr key={`${e.ownerKind}/${e.ownerName}/${e.scheduleName}/${i}`} style={{ borderBottom: `1px solid ${c.border}` }}>
                  {renderCells(cols.visible, e, { base: td })}
                </tr>
              ))}
            </tbody>
          </table>
        </TableSurface>
      )}
    </div>
  );
}

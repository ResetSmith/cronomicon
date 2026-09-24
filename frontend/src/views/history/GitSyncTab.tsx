import { Fragment, useState } from "react";
import { api, csrfHeader } from "../../api/client";
import { useGet, paged, useColumnWidths, useTableSort } from "../../hooks";
import { ColumnsMenu, TableHead, renderCells, useTableColumns } from "../../components/table";
import { type SortColumn } from "../../utils/sort";
import { c } from "../../theme";
import {
  EmptyRow,
  ErrorMsg,
  Field,
  FilterSelect,
  Pager,
  SearchInput,
  StatusBadge,
  filterBar,
  fmtTime,
  mono,
  td,
  th,
  usePager,
} from "./shared";
import { Btn, DetailPanel, SkeletonRows, TableSurface } from "../../components/ui";

interface GitSyncEvent {
  id?: number;
  timestamp?: string;
  action?: string;
  status?: string;
  commit?: string | null;
  author?: string;
  message?: string;
  files?: string;
  changes?: string;
  details?: string;
}

// CO-6 — introduced at adoption. This tab predates useColumnWidths and rendered
// at intrinsic widths; the shared header needs a default per column, and having
// one is also what lets these columns be resized at all.
const COL_W: Record<string, number> = {
  timestamp: 150,
  action: 110,
  files: 180,
  status: 110,
  details: 260,
};

const ACTIONS = ["All", "Pulled", "Pushed"];
const STATUSES = ["All", "success", "warning", "failure"];

// Sortable columns (TS-23, the sorting-update plan). serverSide: the keys go
// to GET /git/history as ?sort=&order= and the DATABASE orders the whole log
// before pagination. `get`/`type` here only describe the columns for the header
// affordance; no client re-sort happens. Files Changed / Details stay unsortable.
const SORT_COLS: SortColumn<GitSyncEvent>[] = [
  { key: "timestamp", get: (g) => g.timestamp, type: "date" },
  { key: "action", get: (g) => g.action, type: "text" },
  { key: "status", get: (g) => g.status, type: "text" },
];

export function GitSyncTab() {
  const [bump, setBump] = useState(0);
  const [syncing, setSyncing] = useState(false);
  const [syncErr, setSyncErr] = useState<string | null>(null);
  const [search, setSearchRaw] = useState("");
  const [action, setActionRaw] = useState("All");
  const [status, setStatusRaw] = useState("All");
  const [expanded, setExpanded] = useState<number | null>(null);
  const pager = usePager();
  const cw = useColumnWidths("history-gitsync");

  // TS-23: serverSide — the hook owns only the header state (carets/aria/
  // persistence); the DATABASE does the ordering via ?sort=&order= below, so
  // the rows array passed here is irrelevant and a sort change refetches from
  // page 1.
  const sort = useTableSort<GitSyncEvent>([], SORT_COLS, { key: "timestamp", dir: "desc" }, {
    tableId: "history-gitsync",
    serverSide: true,
    onChange: () => pager.setPage(0),
  });

  // Server-side paging (PP-H7): pager state in the deps (Sync-now's `bump` still
  // forces a refetch). Action/status/search refine the current page only (no
  // server params yet — H7-S5). TS-23: ?sort=&order= is the dataset-wide sort;
  // "timestamp desc" matches the endpoint default, sent explicitly so the header
  // state and the wire agree.
  // EP-4 Shape B — the expanded panel renders this list's row, so its Refresh
  // drives this query's own refetch (EP-2) rather than a separate dep counter.
  const { data, error, loading, refetch } = useGet<unknown>(
    () =>
      api.GET("/git/history", {
        params: {
          query: {
            page: pager.page + 1,
            pageSize: pager.pageSize,
            ...(sort.sortKey
              ? {
                  sort: sort.sortKey as "timestamp" | "action" | "status",
                  order: sort.sortDir,
                }
              : {}),
          },
        },
      }),
    [bump, pager.page, pager.pageSize, sort.sortKey, sort.sortDir],
  );
  const pg = paged<GitSyncEvent>(data);
  const items = pg.items;

  const setSearch = (v: string) => {
    setSearchRaw(v);
    pager.setPage(0);
  };
  const setAction = (v: string) => {
    setActionRaw(v);
    pager.setPage(0);
  };
  const setStatus = (v: string) => {
    setStatusRaw(v);
    pager.setPage(0);
  };

  // VU-14 — filtered-empty and never-synced are different states with different
  // next steps; without this an active filter read as "no sync has ever run".
  const filtersActive = !!search || action !== "All" || status !== "All";
  const clearFilters = () => {
    setSearchRaw("");
    setActionRaw("All");
    setStatusRaw("All");
    pager.setPage(0);
  };

  const q = search.toLowerCase();
  const filtered = items.filter(
    (g) =>
      (!q ||
        (g.files ?? "").toLowerCase().includes(q) ||
        (g.message ?? "").toLowerCase().includes(q) ||
        (g.author ?? "").toLowerCase().includes(q) ||
        (g.action ?? "").toLowerCase().includes(q)) &&
      (action === "All" || g.action === action) &&
      (status === "All" || g.status === status),
  );

  const pageRows = filtered;

  async function syncNow() {
    setSyncing(true);
    setSyncErr(null);
    const { error: err } = await api.POST("/git/sync", { params: { header: csrfHeader } });
    setSyncing(false);
    if (err) setSyncErr((err as { message?: string })?.message ?? "sync failed");
    else setBump((b) => b + 1);
  }

  // CO-6 — the column spec, replacing this tab's own headCell (a fourth copy of
  // the closure <TableHead> exists to end). The SORT stays serverSide (TS-23):
  // the keys go to GET /git/history and the database orders the whole log before
  // pagination. Order and visibility below are presentation and client-side.
  const cols = useTableColumns<GitSyncEvent>("history-gitsync", [
    {
      key: "timestamp",
      label: "Timestamp",
      sortKey: "timestamp",
      width: COL_W.timestamp,
      fixed: true,
      pin: "first",
      tdStyle: { color: c.textSec, fontSize: c.fontSm, whiteSpace: "nowrap" },
      cell: (g) => fmtTime(g.timestamp),
    },
    {
      key: "action",
      label: "Action",
      sortKey: "action",
      width: COL_W.action,
      fixed: true,
      cell: (g) => (
        <span
          style={{
            display: "inline-block",
            padding: "2px 8px",
            borderRadius: c.radiusChip,
            fontSize: c.fontXs,
            fontWeight: 600,
            background: g.action === "Pulled" ? `${c.success}24` : `${c.info}24`,
            color: g.action === "Pulled" ? c.success : c.info,
          }}
        >
          {g.action ?? "—"}
        </span>
      ),
    },
    { key: "files", label: "Files Changed", width: COL_W.files, tdStyle: { color: c.textSec, fontSize: c.fontSm }, cell: (g) => g.files ?? "—" },
    { key: "status", label: "Status", sortKey: "status", width: COL_W.status, fixed: true, cell: (g) => <StatusBadge status={g.status} /> },
    { key: "details", label: "Details", width: COL_W.details, tdStyle: { color: c.textSec, fontSize: c.fontXs }, cell: (g) => g.details ?? "" },
  ]);

  return (
    <div>
      <div style={filterBar}>
        <SearchInput value={search} onChange={setSearch} placeholder="Search this page by file, message, or author…" />
        <FilterSelect label="Action" value={action} options={ACTIONS} onChange={setAction} />
        <FilterSelect label="Status" value={status} options={STATUSES} onChange={setStatus} />
        <ColumnsMenu cols={cols} cw={cw} />
        <button
          onClick={syncNow}
          disabled={syncing}
          style={{
            marginLeft: "auto",
            padding: "7px 14px",
            borderRadius: c.radiusChip,
            border: `1px solid ${c.primary}`,
            background: c.primaryBg,
            color: c.primary,
            fontSize: c.fontSm,
            fontWeight: 600,
            cursor: syncing ? "default" : "pointer",
            opacity: syncing ? 0.6 : 1,
          }}
        >
          {syncing ? "Syncing…" : "↻ Sync now"}
        </button>
      </div>
      {syncErr && <div style={{ color: c.danger, marginBottom: 12, fontSize: c.fontSm }}>Sync failed: {syncErr}</div>}
      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {error && <ErrorMsg msg={error} />}
      {!loading && !error && (
        <TableSurface>
          <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm }}>
            <thead>
              <TableHead columns={cols.visible} sort={sort} cw={cw} thStyle={th} />
            </thead>
            <tbody>
              {/* VU-14 — the empty log's next step is the sync itself (the same
                  handler as the toolbar's "Sync now"); the filtered one offers the
                  filters back. */}
              {pageRows.length === 0 &&
                (items.length === 0 && !filtersActive ? (
                  <EmptyRow
                    colSpan={cols.visible.length}
                    title="No Git sync events yet"
                    hint="Sync events appear after the first GitLab pull or push."
                    action={
                      <Btn small onClick={syncNow} disabled={syncing}>
                        {syncing ? "Syncing…" : "Sync now"}
                      </Btn>
                    }
                  />
                ) : (
                  <EmptyRow
                    colSpan={cols.visible.length}
                    title="No matching events"
                    hint="No event on this page matches the current search or filters."
                    action={<Btn small onClick={clearFilters}>Clear filters</Btn>}
                  />
                ))}
              {pageRows.map((g, i) => {
                const rowKey = g.id ?? i;
                const isOpen = expanded === rowKey;
                return (
                  <Fragment key={rowKey}>
                    <tr
                      onClick={() => setExpanded(isOpen ? null : rowKey)}
                      style={{
                        borderBottom: `1px solid ${c.border}`,
                        cursor: "pointer",
                        background: isOpen ? c.panel2 : "transparent",
                      }}
                    >
                      {renderCells(cols.visible, g, { base: td })}
                    </tr>
                    {isOpen && (
                      <tr style={{ borderBottom: `1px solid ${c.border}` }}>
                        <td colSpan={cols.visible.length} style={{ padding: "16px 14px", background: c.bg }}>
                          <DetailPanel also={refetch}>
                          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 16, marginBottom: 12 }}>
                            <Field label="Commit" isMono>
                              {g.commit ?? "—"}
                            </Field>
                            <Field label="Author">{g.author ?? "—"}</Field>
                          </div>
                          <div
                            style={{
                              marginBottom: 12,
                              padding: "10px 14px",
                              background: c.panel,
                              borderRadius: c.radiusSurface,
                              border: `1px solid ${c.border}`,
                              fontSize: c.fontSm,
                            }}
                          >
                            <div style={{ fontWeight: 600, marginBottom: 4 }}>{g.message ?? "—"}</div>
                            <div style={{ ...mono(), fontSize: c.fontXs }}>{g.changes ?? ""}</div>
                          </div>
                          {g.files && (
                            <div>
                              <div
                                style={{
                                  fontSize: c.fontXs,
                                  fontFamily: c.sansCond,
                                  fontWeight: 600,
                                  color: c.textSec,
                                  textTransform: "uppercase",
                                  letterSpacing: 0.7,
                                  marginBottom: 6,
                                }}
                              >
                                Files Changed
                              </div>
                              <div style={{ padding: "10px 14px", background: c.panel, borderRadius: c.radiusSurface, border: `1px solid ${c.border}` }}>
                                {g.files.split(",").map((file, idx) => (
                                  <div key={idx} style={{ padding: "3px 0", ...mono() }}>
                                    {file.trim()}
                                  </div>
                                ))}
                              </div>
                            </div>
                          )}
                          </DetailPanel>
                        </td>
                      </tr>
                    )}
                  </Fragment>
                );
              })}
            </tbody>
          </table>
          <Pager pager={pager} page={pager.page} total={pg.totalItems} noun="sync events" shown={pageRows.length} />
        </TableSurface>
      )}
    </div>
  );
}

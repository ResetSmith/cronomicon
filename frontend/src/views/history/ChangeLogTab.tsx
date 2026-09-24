import { useState } from "react";
import { api } from "../../api/client";
import { useGet, paged, useColumnWidths, useTableSort } from "../../hooks";
import { ColumnsMenu, TableHead, renderCells, useTableColumns } from "../../components/table";
import { type SortColumn } from "../../utils/sort";
import { c } from "../../theme";
import {
  EmptyRow,
  ErrorMsg,
  FilterSelect,
  Pager,
  SearchInput,
  filterBar,
  fmtTime,
  td,
  th,
  uniq,
  usePager,
} from "./shared";
import { Btn, SkeletonRows, TableSurface } from "../../components/ui";

// Default column widths (px) so table-layout:fixed has a sensible starting point
// before the user drags (V1.1-7). Stored overrides come from useColumnWidths.
const COL_W: Record<string, number> = {
  timestamp: 150,
  user: 180,
  category: 120,
  action: 120,
  target: 200,
  details: 200,
};

interface ChangeLogEntry {
  id?: number;
  timestamp?: string;
  user?: string;
  category?: string;
  action?: string;
  target?: string;
  details?: string;
}

// Sortable columns (TS-23, the sorting-update plan). serverSide: the keys go
// to GET /change-log as ?sort=&order= and the DATABASE orders the whole audit
// trail before pagination. `get`/`type` here only describe the columns for the
// header affordance; no client re-sort happens. Details stays unsortable.
const SORT_COLS: SortColumn<ChangeLogEntry>[] = [
  { key: "timestamp", get: (a) => a.timestamp, type: "date" },
  { key: "user", get: (a) => a.user, type: "text" },
  { key: "category", get: (a) => a.category, type: "text" },
  { key: "action", get: (a) => a.action, type: "text" },
  { key: "target", get: (a) => a.target, type: "text" },
];

export function ChangeLogTab() {
  const [search, setSearchRaw] = useState("");
  const [category, setCategoryRaw] = useState("All");
  const [action, setActionRaw] = useState("All");
  const [user, setUserRaw] = useState("All");
  const pager = usePager();
  const cw = useColumnWidths("history-changelog");

  // TS-23: serverSide — the hook owns only the header state (carets/aria/
  // persistence); the DATABASE does the ordering via ?sort=&order= below, so
  // the rows array passed here is irrelevant and a sort change refetches from
  // page 1.
  const sort = useTableSort<ChangeLogEntry>([], SORT_COLS, { key: "timestamp", dir: "desc" }, {
    tableId: "history-changelog",
    serverSide: true,
    onChange: () => pager.setPage(0),
  });

  // Server-side paging (PP-H7): category, user, and free-text search (q) are all
  // backend-supported, so they're sent to the server and the total reflects them
  // (search spans ALL history, not just the page). Only the Action filter has no
  // server param yet (H7-S3) — it refines the current page. Facet dropdown
  // options are page-scoped for now (H7-S4 would add a distinct-values endpoint).
  const { data, error, loading } = useGet<unknown>(
    () =>
      api.GET("/change-log", {
        params: {
          query: {
            page: pager.page + 1,
            pageSize: pager.pageSize,
            ...(category !== "All" ? { category } : {}),
            ...(user !== "All" ? { user } : {}),
            ...(search ? { q: search } : {}),
            // TS-23: the dataset-wide sort. "timestamp desc" matches the endpoint
            // default; sent explicitly so the header state and the wire agree.
            ...(sort.sortKey
              ? {
                  sort: sort.sortKey as "timestamp" | "user" | "category" | "action" | "target",
                  order: sort.sortDir,
                }
              : {}),
          },
        },
      }),
    [pager.page, pager.pageSize, category, user, search, sort.sortKey, sort.sortDir],
  );
  const pg = paged<ChangeLogEntry>(data);
  const items = pg.items;

  const setSearch = (v: string) => {
    setSearchRaw(v);
    pager.setPage(0);
  };
  const setCategory = (v: string) => {
    setCategoryRaw(v);
    pager.setPage(0);
  };
  const setAction = (v: string) => {
    setActionRaw(v);
    pager.setPage(0);
  };
  const setUser = (v: string) => {
    setUserRaw(v);
    pager.setPage(0);
  };

  // VU-14 — category/user/search are sent to the SERVER, so an empty `items` under
  // an active filter is a no-match, not an empty audit trail. Without this the view
  // told an operator searching for a target that nothing had ever been changed.
  const filtersActive = !!search || category !== "All" || action !== "All" || user !== "All";
  const clearFilters = () => {
    setSearchRaw("");
    setCategoryRaw("All");
    setActionRaw("All");
    setUserRaw("All");
    pager.setPage(0);
  };

  // Only Action is refined client-side (no server param yet); the rest is server-side.
  const filtered = items.filter((a) => action === "All" || a.action === action);
  const pageRows = filtered;

  const exportCsv = () => {
    const headers = ["Timestamp", "User", "Category", "Action", "Target", "Details"];
    const lines = [headers, ...filtered.map((a) => [a.timestamp ?? "", a.user ?? "", a.category ?? "", a.action ?? "", a.target ?? "", a.details ?? ""])];
    const csv = lines.map((r) => r.map((v) => `"${String(v).replace(/"/g, '""')}"`).join(",")).join("\n");
    const url = URL.createObjectURL(new Blob([csv], { type: "text/csv" }));
    const link = document.createElement("a");
    link.href = url;
    link.download = "change-log-page.csv";
    link.click();
    URL.revokeObjectURL(url);
  };

  // Header cell: a resizable <th> (V1.1-7) that doubles as a sort toggle when a
  // sort key is given. The resize handle stops propagation so dragging never sorts.
  // CO-4 — the column spec. NOTE the sort here is SERVER-side (TS-23): the keys
  // go to GET /change-log as ?sort=&order= and the database orders the whole
  // audit trail before pagination. Column ORDER and VISIBILITY below are
  // presentation and stay client-side; do not let that tempt anyone into
  // re-sorting a server-ordered page here, because the two collations can
  // disagree and rows would shear between pages.
  const cols = useTableColumns<ChangeLogEntry>("history-changelog", [
    {
      key: "timestamp",
      label: "Timestamp",
      sortKey: "timestamp",
      width: COL_W.timestamp,
      fixed: true,
      pin: "first",
      tdStyle: { color: c.textSec, fontSize: c.fontSm, whiteSpace: "nowrap" },
      cell: (a) => fmtTime(a.timestamp),
    },
    { key: "user", label: "User", sortKey: "user", width: COL_W.user, tdStyle: { fontWeight: 500, fontSize: c.fontSm }, cell: (a) => (a.user ?? "—").split("@")[0] },
    { key: "category", label: "Category", sortKey: "category", width: COL_W.category, fixed: true, tdStyle: { color: c.textSec, fontSize: c.fontSm }, cell: (a) => a.category ?? "—" },
    { key: "action", label: "Action", sortKey: "action", width: COL_W.action, fixed: true, tdStyle: { color: c.textSec, fontSize: c.fontSm }, cell: (a) => a.action ?? "—" },
    { key: "target", label: "Target", sortKey: "target", width: COL_W.target, tdStyle: { fontWeight: 600, fontSize: c.fontSm }, cell: (a) => a.target ?? "—" },
    { key: "details", label: "Details", width: COL_W.details, tdStyle: { color: c.textSec, fontSize: c.fontXs, maxWidth: 280 }, cell: (a) => a.details ?? "" },
  ]);

  return (
    <div>
      <div style={filterBar}>
        <SearchInput value={search} onChange={setSearch} placeholder="Search all history by target, user, or details…" />
        <FilterSelect label="Category" value={category} options={uniq(items.map((a) => a.category))} onChange={setCategory} />
        <FilterSelect label="Action" value={action} options={uniq(items.map((a) => a.action))} onChange={setAction} />
        <FilterSelect label="User" value={user} options={uniq(items.map((a) => a.user))} onChange={setUser} />
        <button
          onClick={exportCsv}
          style={{
            padding: "7px 12px",
            borderRadius: c.radiusChip,
            border: `1px solid ${c.border}`,
            background: c.panel2,
            color: c.textSec,
            fontSize: c.fontSm,
            fontFamily: c.sans,
            cursor: "pointer",
            whiteSpace: "nowrap",
          }}
        >
          Export page (CSV)
        </button>
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
              {pageRows.length === 0 &&
                (items.length === 0 && !filtersActive ? (
                  // No action: the change log fills itself as configuration is
                  // edited elsewhere; nothing here can create an entry (VU-14).
                  <EmptyRow colSpan={cols.visible.length} title="No changes recorded yet" hint="In-app configuration changes are audited here (1-year retention)." />
                ) : (
                  <EmptyRow
                    colSpan={cols.visible.length}
                    title="No matching entries"
                    hint="No audited change matches the current search or filters."
                    action={<Btn small onClick={clearFilters}>Clear filters</Btn>}
                  />
                ))}
              {pageRows.map((a, i) => (
                <tr key={a.id ?? i} style={{ borderBottom: `1px solid ${c.border}` }}>
                  {renderCells(cols.visible, a, { base: td })}
                </tr>
              ))}
            </tbody>
          </table>
          <Pager pager={pager} page={pager.page} total={pg.totalItems} noun="changes" shown={pageRows.length} />
        </TableSurface>
      )}
    </div>
  );
}

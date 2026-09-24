import { useState } from "react";
import { Link } from "react-router-dom";
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
  StatusBadge,
  filterBar,
  fmtTime,
  mono,
  td,
  th,
  usePager,
} from "./shared";
import { Btn, SkeletonRows, TableSurface } from "../../components/ui";

interface SchedulePush {
  id?: string;
  userEmail?: string;
  commitSha?: string | null;
  branch?: string;
  filesChanged?: string[];
  scheduleSlots?: string;
  status?: string;
  errorMessage?: string | null;
  timestamp?: string;
}

const STATUSES = ["All", "success", "failed"];

// Sortable columns (TS-23, the sorting-update plan). serverSide: the keys go
// to GET /schedule-pushes as ?sort=&order= and the DATABASE orders the whole
// dataset before pagination. `get`/`type` here only describe the columns for the
// header affordance; no client re-sort happens. Schedule Change / Commit stay
// unsortable.
const SORT_COLS: SortColumn<SchedulePush>[] = [
  { key: "timestamp", get: (p) => p.timestamp, type: "date" },
  { key: "user", get: (p) => p.userEmail, type: "text" },
  { key: "file", get: (p) => (p.filesChanged ?? []).join(", "), type: "text" },
  { key: "status", get: (p) => p.status, type: "text" },
];

// CO-6 — introduced at adoption, like the Git Sync twin: this tab predates
// useColumnWidths and rendered at intrinsic widths.
const COL_W: Record<string, number> = {
  timestamp: 150,
  user: 170,
  file: 200,
  slots: 240,
  commit: 100,
  status: 130,
};

export function SchedulePushesTab() {
  const [search, setSearchRaw] = useState("");
  const [status, setStatusRaw] = useState("All");
  const pager = usePager();
  const cw = useColumnWidths("history-schedulepushes");

  // TS-23: serverSide — the hook owns only the header state (carets/aria/
  // persistence); the DATABASE does the ordering via ?sort=&order= below, so
  // the rows array passed here is irrelevant and a sort change refetches from
  // page 1.
  const sort = useTableSort<SchedulePush>([], SORT_COLS, { key: "timestamp", dir: "desc" }, {
    tableId: "history-schedulepushes",
    serverSide: true,
    onChange: () => pager.setPage(0),
  });

  // Server-side paging (PP-H7): `status` is backend-supported and sent to the
  // server; free-text search refines the current page only (no server param).
  // TS-23: ?sort=&order= is the dataset-wide sort; "timestamp desc" matches the
  // endpoint default, sent explicitly so the header state and the wire agree.
  const { data, error, loading } = useGet<unknown>(
    () =>
      api.GET("/schedule-pushes", {
        params: {
          query: {
            page: pager.page + 1,
            pageSize: pager.pageSize,
            ...(status !== "All" ? { status: status as "success" | "failed" } : {}),
            ...(sort.sortKey
              ? {
                  sort: sort.sortKey as "timestamp" | "user" | "file" | "status",
                  order: sort.sortDir,
                }
              : {}),
          },
        },
      }),
    [pager.page, pager.pageSize, status, sort.sortKey, sort.sortDir],
  );
  const pg = paged<SchedulePush>(data);
  const items = pg.items;

  const setSearch = (v: string) => {
    setSearchRaw(v);
    pager.setPage(0);
  };
  const setStatus = (v: string) => {
    setStatusRaw(v);
    pager.setPage(0);
  };

  const q = search.toLowerCase();
  const filtered = items.filter(
    (p) =>
      !q ||
      (p.userEmail ?? "").toLowerCase().includes(q) ||
      (p.filesChanged ?? []).some((f) => f.toLowerCase().includes(q)) ||
      (p.scheduleSlots ?? "").toLowerCase().includes(q),
  );

  const pageRows = filtered;

  // VU-14 — `status` is a server param, so an empty page under a status filter is
  // a no-match rather than "nothing has ever been pushed".
  const filtersActive = !!search || status !== "All";
  const clearFilters = () => {
    setSearchRaw("");
    setStatusRaw("All");
    pager.setPage(0);
  };

  // CO-6 — the column spec, replacing this tab's own headCell (a fifth copy of
  // the same closure). The SORT stays serverSide (TS-23): the keys go to the
  // endpoint and the database orders the whole log before pagination. Order and
  // visibility below are presentation and client-side.
  const cols = useTableColumns<SchedulePush>("history-schedulepushes", [
    {
      key: "timestamp",
      label: "Timestamp",
      sortKey: "timestamp",
      width: COL_W.timestamp,
      fixed: true,
      pin: "first",
      tdStyle: { color: c.textSec, fontSize: c.fontSm, whiteSpace: "nowrap" },
      cell: (r) => fmtTime(r.timestamp),
    },
    { key: "user", label: "User", sortKey: "user", width: COL_W.user, tdStyle: { color: c.textSec, fontSize: c.fontSm }, cell: (r) => r.userEmail ?? "—" },
    {
      key: "file",
      label: "File",
      sortKey: "file",
      width: COL_W.file,
      tdStyle: { ...mono(), color: c.primary },
      cell: (r) => (r.filesChanged ?? []).join(", ") || "—",
    },
    { key: "slots", label: "Schedule Change", width: COL_W.slots, tdStyle: { ...mono(), maxWidth: 240 }, cell: (r) => r.scheduleSlots ?? "—" },
    {
      key: "commit",
      label: "Commit",
      width: COL_W.commit,
      fixed: true,
      tdStyle: mono(),
      cell: (r) => <span title={r.commitSha ?? undefined}>{r.commitSha ? r.commitSha.slice(0, 8) : "—"}</span>,
    },
    {
      key: "status",
      label: "Status",
      sortKey: "status",
      width: COL_W.status,
      fixed: true,
      cell: (r) => (
        <>
          <StatusBadge status={r.status} />
          {r.errorMessage && (
            <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 3, fontFamily: c.mono }}>{r.errorMessage}</div>
          )}
        </>
      ),
    },
  ]);

  return (
    <div>
      <div style={filterBar}>
        <SearchInput value={search} onChange={setSearch} placeholder="Search this page by user, file, or schedule…" />
        <FilterSelect label="Status" value={status} options={STATUSES} onChange={setStatus} />
        <ColumnsMenu cols={cols} cw={cw} />
      </div>
      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {error && <ErrorMsg msg={error} />}
      {!loading && !error && (
        <TableSurface>
          <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm }}>
            <thead>
              <TableHead columns={cols.visible} sort={sort} cw={cw} thStyle={th} />
            </thead>
            <tbody>
              {pageRows.length === 0 &&
                (items.length === 0 && !filtersActive ? (
                  <EmptyRow
                    colSpan={cols.visible.length}
                    title="No schedule pushes yet"
                    hint="A push is recorded here when a schedule is published to GitLab."
                    action={
                      <Link to="/schedules" style={{ textDecoration: "none" }}>
                        <Btn small>Go to Schedules</Btn>
                      </Link>
                    }
                  />
                ) : (
                  <EmptyRow
                    colSpan={cols.visible.length}
                    title="No matching pushes"
                    hint="No push on this page matches the current search or status."
                    action={<Btn small onClick={clearFilters}>Clear filters</Btn>}
                  />
                ))}
              {pageRows.map((p, i) => (
                <tr key={p.id ?? i} style={{ borderBottom: `1px solid ${c.border}` }}>
                  {renderCells(cols.visible, p, { base: td })}
                </tr>
              ))}
            </tbody>
          </table>
          <Pager pager={pager} page={pager.page} total={pg.totalItems} noun="pushes" shown={pageRows.length} />
        </TableSurface>
      )}
    </div>
  );
}

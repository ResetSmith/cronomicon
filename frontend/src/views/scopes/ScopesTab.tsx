import { Fragment, useEffect, useState } from "react";
import { api } from "../../api/client";
import { useColumnWidths, useGet, useInlineTags, rows, useTableSort } from "../../hooks";
import { ColumnsMenu, TableHead, renderCells, useTableColumns } from "../../components/table";
import { type SortColumn } from "../../utils/sort";
import { c } from "../../theme";
import { DetailPanel, EmptyCell, ExpandChevron, HoverTr, InlineLoading, InlineTags, SkeletonRows, TagEditor, TagFilterSelect, matchesTags } from "../../components/ui";
import {
  Btn,
  ConfirmDialog,
  Modal,
  Notice,
  SearchBar,
  csrfHeader,
  errMsg,
  fmtDate,
  inputStyle,
  labelStyle,
  tdStyle,
  } from "../envvars/ui";
import { BindingNotices, BoundRunnersCell, ScopeRunnersField, type BoundRunner } from "./ScopeRunners";


export interface ScopeRow {
  id?: string; // UUIDv7 (K-3)
  source?: "git" | "cronomicon";
  scope: string;
  name?: string | null;
  description?: string;
  hostCount?: number;
  gitlabUrl?: string | null;
  sidecarPath?: string | null;
  hosts?: string[];
  // Git metadata of a git-source scope: the owner its sidecar or pragma declares
  // and the line-numbered errors from the strict pragma parse. (Until 2.1.0 both
  // rode on a `capability` object beside the scope's "supported run types".)
  owner?: string | null;
  pragmaErrors?: { file?: string; line?: number; message: string }[];
  hasInventory?: boolean;
  inventoryFormat?: string | null;
  projectionStatus?: string | null; // ok | degraded | unavailable
  agencies?: { id: string; name: string }[]; // network-isolation zones (T3.8 — a SET; scopes.agency_id was dropped in migration 700)
  tags?: string[]; // operator-owned (migration 1160); never synced from Git
  // The runners this scope is restricted to (migration 1180, SB band). [] ⇒ any
  // runner eligible for its agency. Operator-owned and never synced, like the
  // agency. A binding whose runner was deregistered is still listed, and still
  // restricts.
  boundRunners?: BoundRunner[];
  lastChangedAt?: string | null;
}

// InventoryDoc mirrors the GET /scopes/{id}/inventory response (advisory parsed
// projection). The projection is best-effort and NON-authoritative — ansible
// reads the raw file via `-i`.
interface InventoryDoc {
  source?: string;
  format?: string;
  editable?: boolean;
  hasInventory?: boolean;
  raw?: string;
  parseStatus?: string; // ok | degraded | unavailable
  parseReason?: string;
  parseLine?: number;
  projection?: {
    advisory?: boolean;
    hosts?: { name: string; vars?: Record<string, string> }[];
    groups?: { name: string; hosts?: string[]; children?: string[]; vars?: Record<string, string> }[];
  };
}

interface BrokenRef {
  entity?: string;
  name?: string;
}

// Default column widths (px) so table-layout:fixed has a sensible starting point
// before the user drags (V1.1-7). Stored overrides come from useColumnWidths.
const COL_W: Record<string, number> = {
  expand: 44,
  scope: 180,
  source: 100,
  description: 190,
  tags: 140,
  hosts: 100,
  agencies: 150,
  runners: 140,
  updated: 150,
  actions: 140,
};
// The defaults sum to 1334px, which is what fits beside the sidebar at 1600 —
// Scope and Description gave up 50px between them when the Runners column
// arrived (SB), so the actions cell does not slide off the right edge.

// Sortable columns (Phase 2, the sorting-update plan §3.2), driven by
// useTableSort. Source sorts by the rendered label (Cronomicon/Git) and Hosts by
// the same count the cell shows; Description / Tags / expand / actions stay
// unsortable.
const SORT_COLS: SortColumn<ScopeRow>[] = [
  { key: "scope", get: (s) => s.scope, type: "text" },
  { key: "source", get: (s) => (s.source === "cronomicon" ? "Cronomicon" : "Git"), type: "text" },
  { key: "hosts", get: (s) => s.hostCount ?? s.hosts?.length, type: "number" },
  { key: "updated", get: (s) => s.lastChangedAt, type: "date" },
];

export function ScopesTab({
  scopes,
  loading,
  error,
  refetch,
  canEdit,
  dep = 0,
}: {
  scopes: ScopeRow[];
  loading: boolean;
  error: string | null;
  refetch: () => void;
  canEdit: boolean;
  /** The parent's list-load counter. A fresh load drops the optimistic tag overrides. */
  dep?: number;
}) {
  const [search, setSearch] = useState("");
  const [tagFilter, setTagFilter] = useState<string[]>([]);
  const [tagMatch, setTagMatch] = useState<"any" | "all">("any");
  const [expanded, setExpanded] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<ScopeRow | null>(null);
  const [deleting, setDeleting] = useState<ScopeRow | null>(null);
  const [busy, setBusy] = useState(false);
  const [resyncing, setResyncing] = useState(false);
  const [notice, setNotice] = useState<{ kind: "info" | "error"; text: string } | null>(null);
  // Agency options for the per-scope binding selector (agency-support.md M1).
  const { data: agencyData } = useGet<unknown>(() => api.GET("/agencies"), []);
  const agencies = rows<{ id: string; name: string }>(agencyData);
  const cw = useColumnWidths("envvars-scopes");

  // Operator-owned scope tags (migration 1160) — optimistic edits via the
  // dedicated PUT /scope-tags/{scopeId}; the override map resets on every list
  // load (dep). They work on git-source scopes too: the tags never come from Git.
  const tagState = useInlineTags<ScopeRow>(
    dep,
    (s) => String(s.id),
    (s) => s.tags,
    (s, next) =>
      api.PUT("/scope-tags/{scopeId}", { params: { path: { scopeId: s.id! }, header: csrfHeader }, body: { tags: next } }),
  );

  const resync = async () => {
    setResyncing(true);
    const { data, error: e } = await api.POST("/scopes/resync", { params: { header: csrfHeader } });
    setResyncing(false);
    if (e) {
      setNotice({ kind: "error", text: `Re-sync failed: ${errMsg(e)}` });
      return;
    }
    const r = data as { scopesSynced?: number; deltas?: unknown[]; errors?: unknown[] } | undefined;
    setNotice({
      kind: "info",
      text: r
        ? `Re-synced ${r.scopesSynced ?? 0} scopes — ${r.deltas?.length ?? 0} scope changes — ${r.errors?.length ?? 0} pragma errors`
        : "Re-synced inventories from GitLab",
    });
    refetch();
  };

  const filtered = scopes.filter(
    (s) =>
      (!search ||
        s.scope.toLowerCase().includes(search.toLowerCase()) ||
        (s.description || "").toLowerCase().includes(search.toLowerCase())) &&
      matchesTags(tagState.tagsFor(s), tagFilter, tagMatch),
  );
  const clearFilters = () => {
    setSearch("");
    setTagFilter([]);
  };

  // Sort after the search filter (§3.1). The tableId reuses the column-width
  // id so the two per-table preferences share one identity.
  const sort = useTableSort(filtered, SORT_COLS, { key: "scope", dir: "asc" }, { tableId: "envvars-scopes" });

  // CO-4 — the column spec. Built in render: cells read `c.*` and close over
  // `expanded` and `canEdit`.
  const cols = useTableColumns<ScopeRow>("envvars-scopes", [
    {
      key: "expand",
      label: "",
      menuLabel: "Expand",
      width: COL_W.expand,
      fixed: true,
      pin: "first",
      cell: (row) => <ExpandChevron open={expanded === row.id} />,
    },
    {
      key: "scope",
      label: "Scope",
      sortKey: "scope",
      width: COL_W.scope,
      tdStyle: { fontWeight: 600 },
      cell: (row) => {
        const n = row.pragmaErrors?.length ?? 0;
        if (n === 0) return row.scope;
        // The ⚠ flags a git inventory whose `# cronomicon:v1` pragma did not
        // parse; the expanded row lists the errors line by line.
        return (
          <span style={{ display: "inline-flex", gap: 6, alignItems: "center" }}>
            {row.scope}
            <span title={`${n} pragma parse error${n === 1 ? "" : "s"}`} aria-label={`${n} pragma parse error${n === 1 ? "" : "s"}`} style={{ color: c.warning, fontSize: c.fontSm }}>
              ⚠
            </span>
          </span>
        );
      },
    },
    {
      key: "source",
      label: "Source",
      sortKey: "source",
      width: COL_W.source,
      fixed: true,
      cell: (row) => {
        const isLocal = row.source === "cronomicon";
        return <span style={{ fontSize: c.fontSm, color: isLocal ? c.info : c.textSec }}>{isLocal ? "Cronomicon" : "Git"}</span>;
      },
    },
    {
      key: "description",
      label: "Description",
      width: COL_W.description,
      tdStyle: { color: c.textSec, fontSize: c.fontSm, maxWidth: 280, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" },
      cell: (row) => row.description || <EmptyCell />,
    },
    { key: "tags", label: "Tags", width: COL_W.tags, cell: (row) => <InlineTags tags={tagState.tagsFor(row)} max={2} /> },
    {
      key: "hosts",
      label: "Hosts",
      sortKey: "hosts",
      width: COL_W.hosts,
      tdStyle: { textAlign: "right", color: c.textSec },
      cell: (row) => row.hostCount ?? (row.hosts?.length ?? <EmptyCell />),
    },
    {
      key: "agencies",
      label: "Agencies",
      width: COL_W.agencies,
      // RB-22 — a scope's agencies on the scope's own row. This was answered from
      // the Membership matrix until that grid was deleted; the question ("which
      // zones can run this scope's jobs?") belongs where the scope is.
      //
      // SB — the empty state says "general pool", not "unrestricted". A scope in
      // no agency is NOT open to any runner: the claim rule is disjoint, and
      // only a runner in no agency may take its work. The old label was wrong
      // about that, and with a Runners column beside it "unrestricted" would
      // now also mean two different things on one row.
      cell: (row) =>
        (row.agencies ?? []).length === 0 ? (
          <span
            title="In no agency — only a general-pool runner (one in no agency) may run this scope's jobs."
            style={{ color: c.textSec, fontSize: c.fontXs, fontStyle: "italic", cursor: "help" }}
          >
            general pool
          </span>
        ) : (
          <div style={{ display: "flex", flexWrap: "wrap", gap: 4 }}>
            {(row.agencies ?? []).map((a) => (
              <span
                key={a.id}
                style={{ padding: "1px 7px", borderRadius: c.radiusChip, fontSize: c.fontXs, fontFamily: c.mono, background: c.panel2, border: `1px solid ${c.border}`, color: c.text, whiteSpace: "nowrap" }}
              >
                {a.name}
              </span>
            ))}
          </div>
        ),
    },
    {
      key: "runners",
      label: "Runners",
      width: COL_W.runners,
      // SB — which runners this scope is bound to. Beside Agencies because the
      // two answer the same question at different grain: which department's
      // runners, then which of those can actually reach these hosts.
      cell: (row) => <BoundRunnersCell bound={row.boundRunners} />,
    },
    {
      key: "updated",
      label: "Updated",
      sortKey: "updated",
      width: COL_W.updated,
      fixed: true,
      tdStyle: { textAlign: "right", color: c.textSec, fontSize: c.fontSm, fontFamily: c.mono, whiteSpace: "nowrap" },
      cell: (row) => fmtDate(row.lastChangedAt),
    },
    {
      key: "actions",
      label: "",
      menuLabel: "Actions",
      width: COL_W.actions,
      fixed: true,
      pin: "last",
      cell: (row) => (
        <span onClick={(e) => e.stopPropagation()}>
          <div style={{ display: "flex", gap: 6, justifyContent: "flex-end" }}>
            {row.source === "cronomicon" ? (
              canEdit ? <Btn onClick={() => setEditing(row)}>Edit</Btn> : null
            ) : row.gitlabUrl ? (
              <a
                href={row.gitlabUrl}
                target="_blank"
                rel="noreferrer"
                title="Git-source scopes are version-controlled — edit the inventory file in GitLab."
                style={{
                  display: "inline-flex",
                  alignItems: "center",
                  padding: "6px 12px",
                  border: `1px solid ${c.border}`,
                  borderRadius: c.radiusChip,
                  background: c.panel2,
                  color: c.textSec,
                  fontSize: c.fontSm,
                  fontWeight: 600,
                  textDecoration: "none",
                }}
              >
                Edit in GitLab ↗
              </a>
            ) : null}
          </div>
        </span>
      ),
    },
  ]);

  const doDelete = async () => {
    if (deleting?.id == null) return;
    setBusy(true);
    const { error: e } = await api.DELETE("/scopes/{scopeId}", {
      params: { path: { scopeId: deleting.id }, header: csrfHeader },
    });
    setBusy(false);
    setNotice(
      e
        ? { kind: "error", text: `Delete failed: ${errMsg(e)}` }
        : { kind: "info", text: `Scope deleted: ${deleting.scope}` },
    );
    setDeleting(null);
    setExpanded(null);
    if (!e) refetch();
  };

  return (
    <div>
      {notice && (
        <Notice kind={notice.kind} onDismiss={() => setNotice(null)}>
          {notice.text}
        </Notice>
      )}
      {/* SB — runner pins that could not become a scope binding. ConfigureApp
          only, like the route behind it; and re-read on every list load, since
          binding a scope is what resolves a group. */}
      {canEdit && <BindingNotices dep={dep} onChanged={refetch} />}
      <div style={{ display: "flex", gap: 8, marginBottom: 16 }}>
        <div style={{ flex: 1 }}>
          <SearchBar value={search} onChange={setSearch} placeholder="Search scopes by name or description..." />
        </div>
        <TagFilterSelect items={scopes} selected={tagFilter} onChange={setTagFilter} getTags={(s) => tagState.tagsFor(s)} matchMode={tagMatch} onMatchModeChange={setTagMatch} />
        <ColumnsMenu cols={cols} cw={cw} />
        {canEdit && (
          <>
            <Btn onClick={resync} disabled={resyncing}>
              {resyncing ? "Re-syncing…" : "↻ Re-sync from GitLab"}
            </Btn>
            <Btn primary onClick={() => setAdding(true)}>
              + Add Scope
            </Btn>
          </>
        )}
      </div>

      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {error && <div style={{ color: c.danger }}>Error: {error}</div>}
      {/* VU-14 — "No scopes match" was shown whether or not anything was typed.
          An empty catalog offers Add Scope (the same handler as the toolbar, and
          hidden for a caller without ConfigureApp, who cannot create one); a
          filtered-empty list offers the search and the tag filter back. */}
      {!loading && !error && filtered.length === 0 && (
        <div style={{ color: c.textSec }}>
          {scopes.length === 0 ? (
            <>
              <div>No scopes yet. Git-source scopes appear after the first GitLab sync of an inventories/ directory.</div>
              {canEdit && (
                <div style={{ marginTop: 12 }}>
                  <Btn small onClick={() => setAdding(true)}>
                    Add a scope
                  </Btn>
                </div>
              )}
            </>
          ) : (
            <>
              <div>{search.trim() && tagFilter.length === 0 ? `No scopes match “${search.trim()}”.` : "No scopes match the current filters."}</div>
              <div style={{ marginTop: 12 }}>
                <Btn small onClick={clearFilters}>
                  {tagFilter.length === 0 ? "Clear search" : "Clear filters"}
                </Btn>
              </div>
            </>
          )}
        </div>
      )}
      {filtered.length > 0 && (
        <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontBody, tableLayout: "fixed" }}>
          <thead>
            <TableHead
              columns={cols.visible}
              sort={sort}
              cw={cw}
              trStyle={{ color: c.textSec, textAlign: "left", borderBottom: `1px solid ${c.border}` }}
            />
          </thead>
          <tbody>
            {sort.sorted.map((s) => {
              const isExp = expanded === s.id;
              const isLocal = s.source === "cronomicon";
              const pragmaErrors = s.pragmaErrors ?? [];
              return (
                <Fragment key={s.id ?? s.scope}>
                  <HoverTr
                    onClick={() => setExpanded(isExp ? null : (s.id ?? null))}
                    tint={isExp ? c.primaryBg : undefined}
                    hoverTint={isExp ? c.primaryBg : c.panelHover}
                    style={{ borderBottom: `1px solid ${c.border}` }}
                  >
                    {renderCells(cols.visible, s, {})}
                  </HoverTr>
                  {isExp && (
                    <tr style={{ borderBottom: `1px solid ${c.border}`, background: c.primaryBg }}>
                      <td style={tdStyle()}></td>
                      <td colSpan={cols.visible.length - 1} style={{ ...tdStyle(), paddingBottom: 18 }}>
                        {/* EP-4 Shape B — the panel renders list-row data, so its
                            Refresh drives the view's list reload. `refetch` here is a
                            pure dep bump in Scopes.tsx (it does not collapse the row). */}
                        <DetailPanel also={refetch}>
                        {pragmaErrors.length > 0 && (
                          <div
                            style={{
                              marginBottom: 12,
                              padding: "8px 12px",
                              border: `1px solid ${c.warning}40`,
                              borderLeft: `3px solid ${c.warning}`,
                              borderRadius: c.radiusSurface,
                              fontSize: c.fontSm,
                              color: c.warning,
                            }}
                          >
                            <strong>Pragma error{pragmaErrors.length === 1 ? "" : "s"}:</strong>{" "}
                            {pragmaErrors.map((err, i) => (
                              <span key={i} style={{ marginRight: 12 }}>
                                {err.line != null && <code style={{ fontFamily: c.mono, fontSize: c.fontXs }}>line {err.line}</code>}{" "}
                                {err.message}
                              </span>
                            ))}
                          </div>
                        )}
                        <div style={{ display: "grid", gridTemplateColumns: "140px 1fr", gap: "6px 16px", fontSize: c.fontSm }}>
                          {s.owner && (
                            <>
                              <span style={{ color: c.textSec }}>Owner</span>
                              <span>{s.owner}</span>
                            </>
                          )}
                          {s.name && (
                            <>
                              <span style={{ color: c.textSec }}>Inventory file</span>
                              <span style={{ fontFamily: c.mono, fontSize: c.fontSm }}>{s.name}</span>
                            </>
                          )}
                          {s.sidecarPath && (
                            <>
                              <span style={{ color: c.textSec }}>Sidecar</span>
                              <span style={{ fontFamily: c.mono, fontSize: c.fontSm }}>{s.sidecarPath}</span>
                            </>
                          )}
                        </div>
                        {/* T3.8 — a scope's FULL agency membership. The selector
                            below is the 1:1 affordance this view has always had and
                            still covers the common case; when a scope belongs to
                            SEVERAL agencies it would silently truncate the set, so
                            it is replaced by a read-only summary that names them and
                            points at the matrix. */}
                        {s.id != null && (s.agencies?.length ?? 0) > 1 && (
                          <div style={{ marginTop: 12, display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                            <span style={{ ...labelStyle(), margin: 0 }}>Agencies</span>
                            {(s.agencies ?? []).map((a) => (
                              <span
                                key={a.id}
                                style={{ padding: "2px 9px", borderRadius: c.radiusChip, fontSize: c.fontXs, fontFamily: c.mono, background: `${c.primary}1a`, border: `1px solid ${c.primary}55`, color: c.text }}
                              >
                                {a.name}
                              </span>
                            ))}
                            <span style={{ color: c.textSec, fontSize: c.fontSm }}>
                              This scope belongs to several isolation zones — a runner in <em>any</em> of them can execute
                              its jobs. Edit the set on the <strong>Agencies</strong> tab (expand the agency); the picker
                              here would keep only one.
                            </span>
                          </div>
                        )}
                        {canEdit && s.id != null && (s.agencies?.length ?? 0) <= 1 && (
                          <div style={{ marginTop: 12, display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
                            <span style={{ ...labelStyle(), margin: 0 }}>Agency</span>
                            <select
                              value={s.agencies?.[0]?.id ?? ""}
                              onChange={async (e) => {
                                const agencyId = e.target.value || null;
                                const { error: er } = await api.PUT("/scopes/{scopeId}/agency", {
                                  params: { path: { scopeId: s.id! }, header: csrfHeader },
                                  body: { agencyId },
                                });
                                if (er) setNotice({ kind: "error", text: `Agency update failed: ${errMsg(er)}` });
                                else {
                                  setNotice({ kind: "info", text: `Agency updated: ${s.scope}` });
                                  refetch();
                                }
                              }}
                              style={{ ...inputStyle(), cursor: "pointer", maxWidth: 260 }}
                            >
                              <option value="">— None (general pool) —</option>
                              {agencies.map((a) => (
                                <option key={a.id} value={a.id}>
                                  {a.name}
                                </option>
                              ))}
                            </select>
                            <span style={{ color: c.textSec, fontSize: c.fontSm }}>
                              Network-isolation zone — only a runner in this agency can execute this scope's jobs, and
                              only secrets and keys in it are injectable.
                            </span>
                          </div>
                        )}
                        {/* SB — the runners this scope is bound to, directly under
                            its agency: department first, then which of that
                            department's runners can reach these hosts. Shown to
                            everyone who can see the scope; changed only with
                            ConfigureApp. */}
                        {s.id != null && (
                          <ScopeRunnersField
                            scope={s}
                            canEdit={canEdit}
                            onSaved={(text) => {
                              setNotice({ kind: "info", text });
                              refetch();
                            }}
                          />
                        )}
                        {/* Tags are shown to everyone who can see the scope and
                            edited only with ConfigureApp, like every other scope
                            write. A missing permission is not a precondition the
                            reader can fix, so the editor goes read-only rather
                            than disabled-with-a-reason. */}
                        {s.id != null && (
                          <div style={{ marginTop: 12 }}>
                            <div style={{ ...labelStyle(), marginBottom: 6 }}>
                              Tags{tagState.tagsFor(s).length ? ` (${tagState.tagsFor(s).length})` : ""}
                            </div>
                            {canEdit || tagState.tagsFor(s).length > 0 ? (
                              <TagEditor tags={tagState.tagsFor(s)} onChange={(next) => tagState.save(s, next)} disabled={!canEdit} />
                            ) : (
                              <span style={{ color: c.textSec, fontSize: c.fontSm, fontStyle: "italic" }}>No tags</span>
                            )}
                            {tagState.errors[String(s.id)] && (
                              <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 6 }}>{tagState.errors[String(s.id)]}</div>
                            )}
                          </div>
                        )}
                        {canEdit && s.id != null && <InventoryPanel scopeId={s.id} />}
                        {isLocal && (s.hosts?.length ?? 0) > 0 && (
                          <div style={{ marginTop: 12 }}>
                            <div style={{ ...labelStyle(), marginBottom: 6 }}>Hosts ({s.hosts!.length})</div>
                            <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
                              {s.hosts!.map((h) => (
                                <code
                                  key={h}
                                  style={{
                                    padding: "3px 8px",
                                    background: c.panel2,
                                    border: `1px solid ${c.border}`,
                                    borderRadius: c.radiusChip,
                                    fontSize: c.fontXs,
                                    fontFamily: c.mono,
                                    color: c.textSec,
                                  }}
                                >
                                  {h}
                                </code>
                              ))}
                            </div>
                          </div>
                        )}
                        {/* Cronomicon-authored scopes only, and only for callers who
                            may configure the app — every other mutation control in
                            this view is canEdit-gated, and without it a viewer saw a
                            Delete button that could only ever 403. Quiet danger here;
                            the solid fill is reserved for the confirm (VC.6). */}
                        {isLocal && canEdit && (
                          <div style={{ display: "flex", justifyContent: "flex-end", marginTop: 12 }}>
                            <Btn dangerQuiet onClick={() => setDeleting(s)}>
                              Delete Scope
                            </Btn>
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
      )}

      <div style={{ marginTop: 12, fontSize: c.fontXs, color: c.textSec, display: "flex", gap: 6, alignItems: "center" }}>
        <span style={{ color: c.warning }}>⚠</span>
        Scope names are stable identifiers referenced by jobs, workflows, env vars, secrets and schedules. Renaming does
        not cascade — references must be updated manually.
      </div>

      {(adding || editing) && (
        <ScopeFormModal
          initial={editing}
          existing={scopes}
          onClose={() => {
            setAdding(false);
            setEditing(null);
          }}
          onSaved={(text, broken) => {
            setAdding(false);
            setEditing(null);
            const brokenText =
              broken && broken.length > 0
                ? ` Broken references: ${broken.map((b) => `${b.entity ?? "?"}: ${b.name ?? "?"}`).join(", ")} — update them manually.`
                : "";
            setNotice({ kind: broken && broken.length > 0 ? "error" : "info", text: text + brokenText });
            refetch();
          }}
        />
      )}

      {deleting && (
        <ConfirmDialog
          title="Delete Scope"
          message={
            <>
              Delete <strong>{deleting.scope}</strong>? Jobs, workflows, env vars, secrets, schedules, or role access
              rules that reference this scope by name may fail to resolve until updated. This cannot be undone.
            </>
          }
          confirmLabel="Delete Scope"
          busy={busy}
          onConfirm={doDelete}
          onCancel={() => setDeleting(null)}
        />
      )}
    </div>
  );
}

// InventoryPanel fetches a scope's inventory + advisory parsed projection and
// renders the group tree, host_vars, and the loud "preview unavailable" state.
// Mounted only inside an expanded row, so the GET fires lazily on expand.
function InventoryPanel({ scopeId }: { scopeId: string }) {
  const [doc, setDoc] = useState<InventoryDoc | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  // M5 authoring state (cronomicon scopes).
  const [editing, setEditing] = useState(false);
  const [rawText, setRawText] = useState("");
  const [busy, setBusy] = useState(false);
  const [saveErr, setSaveErr] = useState<string | null>(null);
  const [lineErrs, setLineErrs] = useState<{ line?: number; message?: string }[]>([]);
  const [note, setNote] = useState<string | null>(null);

  const load = () =>
    api.GET("/scopes/{scopeId}/inventory", { params: { path: { scopeId } } }).then(({ data, error: e }) => {
      setLoading(false);
      if (e) setErr(errMsg(e));
      else setDoc(data as InventoryDoc);
    });
  useEffect(() => {
    let live = true;
    setLoading(true);
    setErr(null);
    api.GET("/scopes/{scopeId}/inventory", { params: { path: { scopeId } } }).then(({ data, error: e }) => {
      if (!live) return;
      setLoading(false);
      if (e) setErr(errMsg(e));
      else setDoc(data as InventoryDoc);
    });
    return () => {
      live = false;
    };
  }, [scopeId]);

  if (loading) return <InlineLoading what="inventory" />;
  if (err) return <div style={{ marginTop: 14, color: c.danger, fontSize: c.fontSm }}>Inventory: {err}</div>;
  if (!doc) return null;
  const editable = !!doc.editable; // cronomicon-source — operator can author in-app
  // A git scope with no inventory has nothing to show; an cronomicon scope always
  // offers the authoring affordance even when empty.
  if (!editable && !doc.hasInventory) return null;

  const save = async () => {
    setBusy(true);
    setSaveErr(null);
    setLineErrs([]);
    setNote(null);
    const { error: e } = await api.PUT("/scopes/{scopeId}/inventory", {
      params: { path: { scopeId }, header: csrfHeader },
      body: { raw: rawText, format: "ini" },
    });
    setBusy(false);
    if (e) {
      const body = e as { code?: string; message?: string; errors?: { line?: number; message?: string }[] };
      if (body.code === "inventory_secret_rejected" && body.errors?.length) setLineErrs(body.errors);
      else setSaveErr(body.message ?? errMsg(e));
      return;
    }
    setEditing(false);
    setNote("Inventory saved.");
    await load();
  };
  const importHosts = async () => {
    setBusy(true);
    setNote(null);
    setSaveErr(null);
    const { data, error: e } = await api.POST("/scopes/{scopeId}/inventory/import-hosts", {
      params: { path: { scopeId }, header: csrfHeader },
      body: { overwrite: true },
    });
    setBusy(false);
    if (e) {
      setSaveErr(errMsg(e));
      return;
    }
    const r = data as { created?: number; updated?: number; skipped?: string[] };
    setNote(`Imported to SSH inventory: ${r.created ?? 0} created, ${r.updated ?? 0} updated, ${r.skipped?.length ?? 0} skipped.`);
  };
  const onUpload = (file: File) => {
    file.text().then((t) => {
      setRawText(t);
      setEditing(true);
    });
  };

  const chip = (text: string) => (
    <code
      key={text}
      style={{
        padding: "2px 7px",
        background: c.panel2,
        border: `1px solid ${c.border}`,
        borderRadius: c.radiusChip,
        fontSize: c.fontXs,
        fontFamily: c.mono,
        color: c.textSec,
      }}
    >
      {text}
    </code>
  );
  const varsLine = (vars?: Record<string, string>) =>
    vars && Object.keys(vars).length > 0
      ? Object.entries(vars)
          .map(([k, v]) => `${k}=${v}`)
          .join("  ")
      : "";

  const groups = doc.projection?.groups ?? [];
  const hostsWithVars = (doc.projection?.hosts ?? []).filter((h) => varsLine(h.vars) !== "");

  const noticeStyle = (kind: "info" | "danger"): React.CSSProperties => ({
    padding: "8px 12px",
    borderRadius: c.radiusSurface,
    fontSize: c.fontSm,
    marginBottom: 8,
    border: `1px solid ${kind === "danger" ? c.danger : c.border}40`,
    color: kind === "danger" ? c.danger : c.textSec,
  });

  return (
    <div style={{ marginTop: 14 }}>
      <div style={{ ...labelStyle(), marginBottom: 8, display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        Ansible inventory
        {doc.hasInventory && (
          <span
            title="The parsed view is a navigational aid. Execution always uses the raw inventory file via ansible -i."
            style={{ fontSize: c.fontXs, fontWeight: 500, color: c.textSec, border: `1px solid ${c.border}`, borderRadius: c.radiusChip, padding: "1px 6px" }}
          >
            advisory — not authoritative
          </span>
        )}
        {editable && !editing && (
          <div style={{ marginLeft: "auto", display: "flex", gap: 6, alignItems: "center" }}>
            <Btn
              onClick={() => {
                setRawText(doc.raw ?? "");
                setEditing(true);
                setLineErrs([]);
                setSaveErr(null);
              }}
            >
              {doc.hasInventory ? "Edit" : "Add inventory"}
            </Btn>
            <label
              style={{ fontSize: c.fontSm, padding: "6px 12px", border: `1px solid ${c.border}`, borderRadius: c.radiusChip, color: c.textSec, cursor: "pointer", background: c.panel2 }}
            >
              Upload
              <input type="file" accept=".ini,.txt" style={{ display: "none" }} onChange={(e) => e.target.files?.[0] && onUpload(e.target.files[0])} />
            </label>
            {doc.hasInventory && doc.parseStatus !== "unavailable" && (
              <Btn onClick={importHosts} disabled={busy}>
                Import hosts → SSH
              </Btn>
            )}
          </div>
        )}
      </div>

      {note && <div style={noticeStyle("info")}>{note}</div>}
      {saveErr && <div style={noticeStyle("danger")}>{saveErr}</div>}

      {editing ? (
        <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
          <textarea
            value={rawText}
            onChange={(e) => setRawText(e.target.value)}
            rows={12}
            placeholder={"[web]\nweb1 ansible_host=10.0.0.1 ansible_user=deploy\n\n[web:vars]\ncronomicon_auth_key_env_var=DEPLOY_KEY"}
            style={{ ...inputStyle(), fontFamily: c.mono, fontSize: c.fontSm, resize: "vertical" }}
          />
          {lineErrs.length > 0 && (
            <div style={noticeStyle("danger")}>
              <strong>Secret-bearing inventory rejected.</strong> Reference secrets by env-var NAME, e.g.{" "}
              <code style={{ fontFamily: c.mono, fontSize: c.fontXs }}>{"ansible_become_pass=\"{{ lookup('env','NAME') }}\""}</code>.
              {lineErrs.map((le, i) => (
                <div key={i} style={{ fontFamily: c.mono, fontSize: c.fontXs, marginTop: 2 }}>
                  {le.line != null ? `line ${le.line}: ` : ""}
                  {le.message}
                </div>
              ))}
            </div>
          )}
          <div style={{ display: "flex", gap: 6 }}>
            <Btn primary onClick={save} disabled={busy}>
              {busy ? "Saving…" : "Save inventory"}
            </Btn>
            <Btn
              onClick={() => {
                setEditing(false);
                setLineErrs([]);
                setSaveErr(null);
              }}
            >
              Cancel
            </Btn>
          </div>
          <div style={{ fontSize: c.fontXs, color: c.textSec }}>
            INI format only. Secret <em>values</em> are rejected — reference keys by env-var NAME. After saving, “Import hosts → SSH” materializes dialable hosts.
          </div>
        </div>
      ) : !doc.hasInventory ? (
        // VU-14 — this branch only renders for an editable scope, so the header's
        // "Add inventory" is always present; name it rather than repeat it.
        <div style={{ fontSize: c.fontSm, color: c.textSec }}>
          No inventory yet — use <strong>Add inventory</strong> above to target this scope with ansible and import its
          hosts into the SSH registry.
        </div>
      ) : doc.parseStatus === "unavailable" ? (
        <div
          style={{
            padding: "8px 12px",
            border: `1px solid ${c.warning}40`,
            borderLeft: `3px solid ${c.warning}`,
            borderRadius: c.radiusSurface,
            fontSize: c.fontSm,
            color: c.warning,
          }}
        >
          <strong>Preview unavailable.</strong>{" "}
          {doc.parseReason || "the inventory uses a construct outside the supported preview subset"}
          {doc.parseLine ? (
            <>
              {" "}
              (line <code style={{ fontFamily: c.mono, fontSize: c.fontXs }}>{doc.parseLine}</code>)
            </>
          ) : null}
          .
          <span style={{ color: c.textSec, marginLeft: 6 }}>
            The raw inventory still ships to <code style={{ fontFamily: c.mono, fontSize: c.fontXs }}>ansible -i</code>{" "}
            unchanged.
          </span>
        </div>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
          {groups.length === 0 && (
            <div style={{ fontSize: c.fontSm, color: c.textSec }}>No groups — flat host list (see Hosts above).</div>
          )}
          {groups.map((g) => (
            <div key={g.name} style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, padding: "8px 10px" }}>
              <div style={{ fontSize: c.fontSm, fontWeight: 600, color: c.text, marginBottom: (g.hosts?.length ?? 0) > 0 ? 6 : 0 }}>
                <span style={{ fontFamily: c.mono }}>[{g.name}]</span>
                {g.children && g.children.length > 0 && (
                  <span style={{ fontWeight: 400, color: c.textSec, marginLeft: 8, fontSize: c.fontXs }}>
                    children: {g.children.join(", ")}
                  </span>
                )}
              </div>
              {(g.hosts ?? []).length > 0 && (
                <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>{(g.hosts ?? []).map((h) => chip(h))}</div>
              )}
              {varsLine(g.vars) && (
                <div style={{ fontSize: c.fontXs, color: c.textSec, fontFamily: c.mono, marginTop: 6 }}>{varsLine(g.vars)}</div>
              )}
            </div>
          ))}
          {hostsWithVars.length > 0 && (
            <div>
              <div style={{ fontSize: c.fontXs, color: c.textSec, marginBottom: 4 }}>Host vars</div>
              <div style={{ display: "flex", flexDirection: "column", gap: 3 }}>
                {hostsWithVars.map((h) => (
                  <div key={h.name} style={{ fontSize: c.fontXs, fontFamily: c.mono, color: c.textSec }}>
                    <span style={{ color: c.text }}>{h.name}</span> {varsLine(h.vars)}
                  </div>
                ))}
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function ScopeFormModal({
  initial,
  existing,
  onClose,
  onSaved,
}: {
  initial: ScopeRow | null;
  existing: ScopeRow[];
  onClose: () => void;
  onSaved: (msg: string, broken?: BrokenRef[]) => void;
}) {
  const isEdit = initial != null;
  const [name, setName] = useState(initial?.scope ?? "");
  const [description, setDescription] = useState(initial?.description ?? "");
  const [hostsText, setHostsText] = useState((initial?.hosts ?? []).join("\n"));
  const [formError, setFormError] = useState("");
  const [busy, setBusy] = useState(false);

  const [mode, setMode] = useState<"hosts" | "inventory">(initial?.hasInventory ? "inventory" : "hosts");
  const [rawInventory, setRawInventory] = useState("");
  const [lineErrs, setLineErrs] = useState<{ line?: number; message?: string }[]>([]);

  useEffect(() => {
    if (isEdit && initial?.hasInventory && initial.id != null) {
      setBusy(true);
      api.GET("/scopes/{scopeId}/inventory", { params: { path: { scopeId: initial.id } } }).then(({ data, error }) => {
        setBusy(false);
        if (!error && data) {
          const doc = data as InventoryDoc;
          setRawInventory(doc.raw ?? "");
        }
      });
    }
  }, [isEdit, initial]);

  const renameDiffers = isEdit && name.trim() !== initial.scope;

  const save = async () => {
    const scopeName = name.trim();
    const hosts = mode === "hosts"
      ? hostsText
          .split("\n")
          .map((h) => h.trim())
          .filter(Boolean)
      : [];
    const rawInv = mode === "inventory" ? rawInventory : "";

    if (!scopeName) {
      setFormError("Scope name is required.");
      return;
    }
    if (!/^[A-Za-z0-9_-]+$/.test(scopeName)) {
      setFormError("Scope names must be alphanumeric (dashes/underscores allowed).");
      return;
    }
    const dup = existing.some((s) => s.id !== initial?.id && s.scope.toLowerCase() === scopeName.toLowerCase());
    if (dup) {
      setFormError(`A scope named '${scopeName}' already exists.`);
      return;
    }
    // Hosts may be empty (M5): an inventory scope starts empty and its membership
    // comes from the inventory authored in the expanded row.
    setBusy(true);
    setFormError("");
    setLineErrs([]);
    const body = { scope: scopeName, description, hosts, rawInventory: rawInv };
    if (isEdit && initial.id != null) {
      const { data, error: e } = await api.PATCH("/scopes/{scopeId}", {
        params: { path: { scopeId: initial.id }, header: csrfHeader },
        body,
      });
      setBusy(false);
      if (e) {
        const errBody = e as { code?: string; message?: string; errors?: { line?: number; message?: string }[] };
        if (errBody.code === "inventory_secret_rejected" && errBody.errors?.length) {
          setLineErrs(errBody.errors);
        } else {
          setFormError(errBody.message ?? errMsg(e));
        }
      } else {
        onSaved(`Scope '${scopeName}' updated.`, (data as { brokenReferences?: BrokenRef[] })?.brokenReferences);
      }
    } else {
      const { error: e } = await api.POST("/scopes", {
        params: { header: csrfHeader },
        body,
      });
      setBusy(false);
      if (e) {
        const errBody = e as { code?: string; message?: string; errors?: { line?: number; message?: string }[] };
        if (errBody.code === "inventory_secret_rejected" && errBody.errors?.length) {
          setLineErrs(errBody.errors);
        } else {
          setFormError(errBody.message ?? errMsg(e));
        }
      } else {
        onSaved(`Scope '${scopeName}' created.`);
      }
    }
  };

  return (
    <Modal
      title={isEdit ? "Edit Scope" : "Add Scope"}
      onClose={onClose}
      /* FX-10 — the actions and the error that explains a disabled Save are pinned
         below the scrolling body, so neither can slide under the fold on a short
         viewport. Everything that grows stays in the body. */
      footer={
        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          {formError && <div style={{ fontSize: c.fontSm, color: c.danger }}>{formError}</div>}
          <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
            <Btn onClick={onClose} disabled={busy}>
              Cancel
            </Btn>
            <Btn primary onClick={save} disabled={busy}>
              {busy ? "Saving…" : isEdit ? "Save Changes" : "Create Scope"}
            </Btn>
          </div>
        </div>
      }
    >
      <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        <div>
          <label style={labelStyle()}>Name *</label>
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. Edge-Lab" style={inputStyle()} />
          {renameDiffers && (
            <div
              style={{
                marginTop: 6,
                padding: "8px 12px",
                border: `1px solid ${c.warning}40`,
                borderRadius: c.radiusSurface,
                color: c.warning,
                fontSize: c.fontXs,
                lineHeight: 1.5,
              }}
            >
              <strong>⚠ Heads up:</strong> Renaming this scope does <strong>not</strong> automatically update references
              to the current name (<code style={{ fontFamily: c.mono, fontSize: c.fontXs }}>{initial.scope}</code>). Jobs,
              workflows, env vars, secrets, schedules and role access rules that target this scope by name may fail to
              resolve until updated manually.
            </div>
          )}
        </div>
        <div>
          <label style={labelStyle()}>Description</label>
          <input
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder="Short description of what this scope targets"
            style={inputStyle()}
          />
        </div>
        <div>
          <label style={labelStyle()}>Hosts Input Mode</label>
          <div style={{ display: "flex", gap: 1 }}>
            <button
              type="button"
              onClick={() => setMode("hosts")}
              style={{
                flex: 1,
                padding: "8px 12px",
                border: `1px solid ${c.border}`,
                borderRight: "none",
                borderTopLeftRadius: c.radiusChip,
                borderBottomLeftRadius: c.radiusChip,
                background: mode === "hosts" ? `${c.primary}1a` : "transparent",
                color: mode === "hosts" ? c.primary : c.textSec,
                borderColor: mode === "hosts" ? c.primary : c.border,
                fontWeight: 600,
                fontSize: c.fontSm,
                cursor: "pointer",
                outline: "none",
              }}
            >
              Hosts List
            </button>
            <button
              type="button"
              onClick={() => setMode("inventory")}
              style={{
                flex: 1,
                padding: "8px 12px",
                border: `1px solid ${c.border}`,
                borderTopRightRadius: c.radiusChip,
                borderBottomRightRadius: c.radiusChip,
                background: mode === "inventory" ? `${c.primary}1a` : "transparent",
                color: mode === "inventory" ? c.primary : c.textSec,
                borderColor: mode === "inventory" ? c.primary : c.border,
                fontWeight: 600,
                fontSize: c.fontSm,
                cursor: "pointer",
                outline: "none",
              }}
            >
              Ansible INI Inventory
            </button>
          </div>
        </div>
        {mode === "hosts" ? (
          <div>
            <label style={labelStyle()}>Hosts (one per line)</label>
            <textarea
              value={hostsText}
              onChange={(e) => setHostsText(e.target.value)}
              placeholder={"host1.internal\nhost2.internal"}
              rows={6}
              style={{ ...inputStyle(), fontFamily: c.mono, fontSize: c.fontSm, resize: "vertical" }}
            />
            <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4 }}>
              Hosts the scope expands to at run time. Empty lines are ignored.
            </div>
          </div>
        ) : (
          <div>
            <label style={labelStyle()}>Ansible INI Inventory</label>
            <textarea
              value={rawInventory}
              onChange={(e) => setRawInventory(e.target.value)}
              placeholder={"[web]\nweb1.internal\nweb2.internal\n\n[web:vars]\nsome_var=some_val"}
              rows={6}
              style={{ ...inputStyle(), fontFamily: c.mono, fontSize: c.fontSm, resize: "vertical" }}
            />
            <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4 }}>
              INI format only. Secret <em>values</em> are rejected — reference keys by env-var NAME.
            </div>
            {lineErrs.length > 0 && (
              <div
                style={{
                  padding: "8px 12px",
                  borderRadius: c.radiusSurface,
                  fontSize: c.fontSm,
                  marginTop: 8,
                  border: `1px solid ${c.danger}40`,
                  color: c.danger,
                }}
              >
                <strong>Secret-bearing inventory rejected.</strong> Reference secrets by env-var NAME, e.g.{" "}
                <code style={{ fontFamily: c.mono, fontSize: c.fontXs }}>{"ansible_become_pass=\"{{ lookup('env','NAME') }}\""}</code>.
                {lineErrs.map((le, i) => (
                  <div key={i} style={{ fontFamily: c.mono, fontSize: c.fontXs, marginTop: 2 }}>
                    {le.line != null ? `line ${le.line}: ` : ""}
                    {le.message}
                  </div>
                ))}
              </div>
            )}
          </div>
        )}
      </div>
    </Modal>
  );
}

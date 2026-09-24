import { Fragment, useState } from "react";
import { api } from "../../api/client";
import { useGet, rows, useColumnWidths, useInlineTags, useTableSort } from "../../hooks";
import { ColumnsMenu, TableHead, renderCells, useTableColumns } from "../../components/table";
import { CreationAgencyPicker, useCreationAgencies } from "../../components/CreationAgencyPicker";
import { type SortColumn } from "../../utils/sort";
import { c } from "../../theme";
import { DetailPanel, EmptyCell, InlineTags, SkeletonRows, TagEditor, TagFilterSelect, matchesTags } from "../../components/ui";
import {
  Btn,
  Chevron,
  ConfirmDialog,
  DetailGrid,
  DetailLabel,
  DetailRow,
  Modal,
  NamespaceHelp,
  Notice,
  ReferenceField,
  ScopeFilterSelect,
  SearchBar,
  UsageCell,
  csrfHeader,
  detailBoxStyle,
  errMsg,
  fmtDate,
  inputStyle,
  labelStyle,
  ALL_SCOPE_OPTION,
  matchesScope,
  scopeCell,
  scopeForWrite,
  AmbiguityBadge,
  OwnerChip,
  ShadowBadge,
  useAmbiguityWarnings,
  useReferenceUsage,
  useShadowWarnings,
  valueBoxStyle,
} from "./ui";
import { AgencyCell, useEntityAgencies } from "./ui";

interface EnvVarRow {
  // UUIDv7 (K-3) — this said `number` on the strength of an openapi.yaml
  // declaration that had always been wrong.
  id?: string;
  key: string;
  reference?: string;
  value?: string;
  scope?: string;
  description?: string;
  tags?: string[];
  ownerAgency?: string; // RA-15 — the owning department's NAME; "" when shared.
  createdBy?: string;
  lastModifiedAt?: string;
}

// Default column widths (px) for table-layout:fixed before the user drags
// (V1.1-7). Stored overrides come from useColumnWidths. Every column has an
// entry — resizable (key/scope/description) and fixed (toggle/actions).
const COL_W: Record<string, number> = {
  toggle: 44,
  key: 240,
  scope: 130,
  usedBy: 120,
  agencies: 150,
  description: 200,
  tags: 150,
  actions: 140,
};

export function VariablesTab({ scopeNames, canEdit }: { scopeNames: string[]; canEdit: boolean }) {
  const [dep, setDep] = useState(0);
  const cw = useColumnWidths("envvars-variables");
  const { data, error, loading } = useGet<unknown>(() => api.GET("/env-vars"), [dep]);
  const items = rows<EnvVarRow>(data);

  // T1.9 — binding counts, so an admin can see what a variable is wired into
  // before editing its scope (the edit that silently strands every binding).
  const usage = useReferenceUsage(dep);
  // RB-22 — per-row agency membership; see SecretsTab.
  const entityAgencies = useEntityAgencies("env-var", dep);

  // RA-10 — a scoped row that shadows a global row of the same key. Worth saying
  // where the operator is looking at the two rows, because the list alone cannot
  // tell them apart: both render as an ordinary row, and only the scoped one is
  // ever injected for runs in its scope.
  const shadows = useShadowWarnings(dep);

  // RA-18 — a name several departments own. Not an error (each department's own
  // runs resolve their own row) but a run spanning them fails closed, and that
  // refusal reads as breakage unless it was seen here first.
  const ambiguities = useAmbiguityWarnings(dep);

  const [search, setSearch] = useState("");
  const [scopeFilter, setScopeFilter] = useState("");
  const [tagFilter, setTagFilter] = useState<string[]>([]);
  const [tagMatch, setTagMatch] = useState<"any" | "all">("any");
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<EnvVarRow | null>(null);
  const [deleting, setDeleting] = useState<EnvVarRow | null>(null);
  const [expanded, setExpanded] = useState<EnvVarRow["id"] | null>(null);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<{ kind: "info" | "error"; text: string } | null>(null);

  // Operator-authored tags (migration 470) — optimistic edits via the dedicated
  // PUT /env-var-tags/{id} endpoint; override map resets on every refetch (dep).
  const tagState = useInlineTags<EnvVarRow>(
    dep,
    (v) => String(v.id),
    (v) => v.tags,
    (v, next) =>
      api.PUT("/env-var-tags/{envVarId}", { params: { path: { envVarId: v.id! }, header: csrfHeader }, body: { tags: next } }),
  );

  // EP-4 — two meanings that used to share one function. `reloadList` re-reads
  // the rows and nothing else; `refetch` additionally collapses the expansion,
  // which is right after a create/delete (the row you were looking at may be
  // gone) and WRONG for the panel's own Refresh button — that would collapse
  // the very panel it was asked to refresh.
  const reloadList = () => setDep((n) => n + 1);
  const refetch = () => {
    setExpanded(null);
    reloadList();
  };

  const filtered = items.filter(
    (v) =>
      (!search ||
        v.key.toLowerCase().includes(search.toLowerCase()) ||
        (v.description || "").toLowerCase().includes(search.toLowerCase())) &&
      matchesScope(v.scope, scopeFilter) &&
      matchesTags(tagState.tagsFor(v), tagFilter, tagMatch),
  );

  // VU-14 — hands every narrowing control back from the filtered-empty state.
  const clearFilters = () => {
    setSearch("");
    setScopeFilter("");
    setTagFilter([]);
  };

  // Sortable columns (TS-12, the sorting-update plan) — declared in-component
  // because Used by reads the usage hook (total binding count, jobs + scripts).
  // Description, Tags, and the expand/actions columns stay unsortable.
  const sortCols: SortColumn<EnvVarRow>[] = [
    { key: "key", get: (v) => v.key },
    { key: "scope", get: (v) => v.scope },
    {
      key: "usedBy",
      get: (v) => {
        const u = usage.usageFor("var", v.key);
        return u.jobs + u.scripts;
      },
      type: "number",
    },
  ];
  const sort = useTableSort(filtered, sortCols, { key: "key", dir: "asc" }, { tableId: "envvars-variables" });

  // Sortable resizable header: the drag handle stops propagation, so resizing
  // never toggles the sort. Active column reads c.text, inactive c.textSec.
  // CO-4 — the column spec replaces this tab's own sortHead helper. Built in
  // render: cells read `c.*` and close over `expanded`, the usage/shadow/tag
  // resolvers, and canEdit.
  const cols = useTableColumns<EnvVarRow>("envvars-variables", [
    {
      key: "toggle",
      label: "",
      menuLabel: "Expand",
      width: COL_W.toggle,
      fixed: true,
      // CO-Q4 — the disclosure control opens this table, so it pins first.
      pin: "first",
      tdStyle: { width: 36, textAlign: "center" },
      cell: (v) => <Chevron open={v.id != null && expanded === v.id} />,
    },
    {
      key: "key",
      label: "Key",
      sortKey: "key",
      width: COL_W.key,
      tdStyle: { fontFamily: c.mono, fontWeight: 600, fontSize: c.fontSm },
      cell: (v) => (
        // Flex-wrap, not inline: JSX strips the newline between the key and its
        // chips, so there is no break opportunity and two badges spill out of
        // the fixed-width column onto Scope.
        <div style={{ display: "flex", flexWrap: "wrap", alignItems: "center", gap: 6, rowGap: 4 }}>
          <span>{v.key}</span>
          <OwnerChip owner={v.ownerAgency} />
          <ShadowBadge shadow={shadows.shadowFor("var", v.key, v.scope)} />
          <AmbiguityBadge ambiguity={ambiguities.ambiguityFor("var", v.key, v.scope)} />
        </div>
      ),
    },
    {
      key: "scope",
      label: "Scope",
      sortKey: "scope",
      width: COL_W.scope,
      // T1.8 — an unscoped row is GLOBAL, and a global variable injects into
      // every scope. A blank cell said neither.
      cell: (v) => (
        <span style={{ color: v.scope ? c.text : c.textSec, fontStyle: v.scope ? "normal" : "italic" }}>{scopeCell(v.scope)}</span>
      ),
    },
    { key: "usedBy", label: "Used by", sortKey: "usedBy", width: COL_W.usedBy, cell: (v) => <UsageCell {...usage.usageFor("var", v.key)} /> },
    { key: "agencies", label: "Agencies", width: COL_W.agencies, cell: (v) => <AgencyCell names={v.id ? entityAgencies[v.id] : undefined} /> },
    { key: "description", label: "Description", width: COL_W.description, tdStyle: { color: c.textSec, fontSize: c.fontSm }, cell: (v) => v.description || <EmptyCell /> },
    { key: "tags", label: "Tags", width: COL_W.tags, cell: (v) => <InlineTags tags={tagState.tagsFor(v)} max={2} /> },
    {
      key: "actions",
      label: "",
      menuLabel: "Actions",
      width: COL_W.actions,
      fixed: true,
      pin: "last",
      cell: (v) => (
        <span onClick={(e) => e.stopPropagation()}>
          {canEdit && (
            <div style={{ display: "flex", gap: 6, justifyContent: "flex-end" }}>
              <Btn onClick={() => setEditing(v)}>Edit</Btn>
              <Btn dangerQuiet onClick={() => setDeleting(v)}>
                Delete
              </Btn>
            </div>
          )}
        </span>
      ),
    },
  ]);

  const doDelete = async () => {
    if (deleting?.id == null) return;
    setBusy(true);
    const { error: e } = await api.DELETE("/env-vars/{envVarId}", {
      params: { path: { envVarId: deleting.id }, header: csrfHeader },
    });
    setBusy(false);
    setNotice(e ? { kind: "error", text: `Delete failed: ${errMsg(e)}` } : { kind: "info", text: `Variable deleted: ${deleting.key}` });
    setDeleting(null);
    if (!e) refetch();
  };

  return (
    <div>
      {notice && (
        <Notice kind={notice.kind} onDismiss={() => setNotice(null)}>
          {notice.text}
        </Notice>
      )}
      <div style={{ display: "flex", gap: 8, marginBottom: 16 }}>
        <div style={{ flex: 1 }}>
          <SearchBar value={search} onChange={setSearch} placeholder="Search variables by key or description..." />
        </div>
        <ScopeFilterSelect scopes={scopeNames.filter((s) => s !== ALL_SCOPE_OPTION)} value={scopeFilter} onChange={setScopeFilter} />
        <TagFilterSelect items={items} selected={tagFilter} onChange={setTagFilter} getTags={(v) => tagState.tagsFor(v)} matchMode={tagMatch} onMatchModeChange={setTagMatch} />
        <ColumnsMenu cols={cols} cw={cw} />
        {canEdit && (
          <Btn primary onClick={() => setAdding(true)}>
            + Add Variable
          </Btn>
        )}
      </div>

      <NamespaceHelp>
        Reference a variable in a script or inventory as{" "}
        <code style={{ fontFamily: c.mono }}>AMADEUS_VAR_&lt;key&gt;</code> — it resolves to the variable's plaintext value
        at run time. Rows keep their bare key; the prefix is added only when you reference it.
      </NamespaceHelp>

      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {error && <div style={{ color: c.danger }}>Error: {error}</div>}
      {/* VU-14 — "No variables found. Add one above." was shown to a viewer with no
          Add button and to anyone whose scope/tag filter simply excluded everything.
          Split the two, and carry the control that resolves each. */}
      {!loading && !error && filtered.length === 0 && (
        <div style={{ color: c.textSec }}>
          {items.length === 0 ? (
            <>
              <div>No variables yet. A variable is plaintext configuration a run can read as AMADEUS_VAR_&lt;key&gt;.</div>
              {canEdit && (
                <div style={{ marginTop: 12 }}>
                  <Btn small onClick={() => setAdding(true)}>
                    Add a variable
                  </Btn>
                </div>
              )}
            </>
          ) : (
            <>
              <div>No variables match the current search, scope, or tags.</div>
              <div style={{ marginTop: 12 }}>
                <Btn small onClick={clearFilters}>
                  Clear filters
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
            {sort.sorted.map((v) => {
              const isExp = v.id != null && expanded === v.id;
              const toggle = () => setExpanded(isExp ? null : (v.id ?? null));
              const rowBg = isExp ? c.primaryBg : "transparent";
              return (
                <Fragment key={v.id ?? v.key}>
                  <tr
                    onClick={toggle}
                    onMouseEnter={(e) => { if (!isExp) e.currentTarget.style.background = c.panelHover; }}
                    onMouseLeave={(e) => { e.currentTarget.style.background = rowBg; }}
                    style={{ borderBottom: isExp ? "none" : `1px solid ${c.border}`, background: rowBg, cursor: "pointer", transition: "background 0.15s" }}
                  >
                    {renderCells(cols.visible, v, { rowStyle: { borderBottom: "none" } })}
                  </tr>
                  {isExp && (
                    <tr style={{ background: rowBg, borderBottom: `1px solid ${c.border}` }}>
                      <td style={{ borderBottom: "none" }} />
                      <td colSpan={cols.visible.length - 1} style={{ padding: "2px 16px 16px", borderBottom: "none" }} onClick={(e) => e.stopPropagation()}>
                        {/* EP-4 Shape B — this panel renders the LIST ROW, so
                            "refresh this panel" means reload the list. Uses
                            reloadList, not refetch: refetch also collapses the
                            expansion, which would close the panel the button
                            was asked to refresh. */}
                        <DetailPanel also={reloadList}>
                        <DetailGrid>
                          <div>
                            <DetailLabel>Value</DetailLabel>
                            <div style={{ ...valueBoxStyle(), fontFamily: c.mono, fontSize: c.fontSm, color: v.value ? c.text : c.textMuted }}>
                              {v.value || "—"}
                            </div>
                          </div>
                          <div>
                            <DetailLabel>Details</DetailLabel>
                            <div style={detailBoxStyle()}>
                              <DetailRow label="Scope" value={v.scope || "global (injects into every scope)"} />
                              <DetailRow
                                label="Used by"
                                value={<UsageCell {...usage.usageFor("var", v.key)} />}
                              />
                              <DetailRow label="Description" value={v.description || "—"} />
                              <DetailRow label="Last Modified" value={fmtDate(v.lastModifiedAt)} />
                              <DetailRow label="Created By" value={v.createdBy || "—"} last />
                            </div>
                          </div>
                        </DetailGrid>
                        </DetailPanel>
                        <ReferenceField reference={v.reference} semantics="Resolves to the variable's plaintext value at run time (log-safe)." />
                        <div style={{ marginTop: 12 }}>
                          <DetailLabel>Tags</DetailLabel>
                          <TagEditor tags={tagState.tagsFor(v)} onChange={(next) => tagState.save(v, next)} disabled={!canEdit} />
                          {tagState.errors[String(v.id)] && (
                            <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 6 }}>{tagState.errors[String(v.id)]}</div>
                          )}
                        </div>
                      </td>
                    </tr>
                  )}
                </Fragment>
              );
            })}
          </tbody>
        </table>
      )}

      {(adding || editing) && (
        <VarFormModal
          initial={editing}
          scopeNames={scopeNames}
          onClose={() => {
            setAdding(false);
            setEditing(null);
          }}
          onSaved={(text) => {
            setAdding(false);
            setEditing(null);
            setNotice({ kind: "info", text });
            refetch();
          }}
        />
      )}

      {deleting && (
        <ConfirmDialog
          title="Delete Variable"
          message={
            <>
              Are you sure you want to delete <code style={{ fontFamily: c.mono }}>{deleting.key}</code>? This cannot be undone.
            </>
          }
          confirmLabel="Delete"
          busy={busy}
          onConfirm={doDelete}
          onCancel={() => setDeleting(null)}
        />
      )}
    </div>
  );
}

function VarFormModal({
  initial,
  scopeNames,
  onClose,
  onSaved,
}: {
  initial: EnvVarRow | null;
  scopeNames: string[];
  onClose: () => void;
  onSaved: (msg: string) => void;
}) {
  const isEdit = initial != null;
  const [key, setKey] = useState(initial?.key ?? "");
  // A global row has no scope, so it initializes to the "All" sentinel and
  // round-trips back to global on save (scopeForWrite).
  const [scope, setScope] = useState(initial?.scope || ALL_SCOPE_OPTION);
  const [value, setValue] = useState(initial?.value ?? "");
  const [description, setDescription] = useState(initial?.description ?? "");
  const [keyError, setKeyError] = useState("");
  const [formError, setFormError] = useState("");
  const [busy, setBusy] = useState(false);
  // RF-Q2(a): the create-time agency binding, shown only to restricted callers.
  const agencyPick = useCreationAgencies(isEdit);

  const onKeyChange = (raw: string) => {
    const up = raw.toUpperCase().replace(/[^A-Z0-9_]/g, "");
    setKey(up);
    setKeyError(raw !== up && raw.length > 0 ? "Only A–Z, 0–9 and _ are allowed" : "");
  };

  const save = async () => {
    if (!key) {
      setKeyError("Key is required");
      return;
    }
    if (agencyPick.blockedReason) {
      setFormError(agencyPick.blockedReason);
      return;
    }
    setBusy(true);
    const body = { key, value, scope: scopeForWrite(scope), description, ...agencyPick.body };
    const res =
      isEdit && initial.id != null
        ? await api.PUT("/env-vars/{envVarId}", { params: { path: { envVarId: initial.id }, header: csrfHeader }, body })
        : await api.POST("/env-vars", { params: { header: csrfHeader }, body });
    setBusy(false);
    if (res.error) setFormError(errMsg(res.error));
    else onSaved(`Variable ${isEdit ? "updated" : "created"}: ${key}`);
  };

  return (
    <Modal
      title={isEdit ? "Edit Variable" : "New Variable"}
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
            <Btn primary onClick={save} disabled={busy || !!agencyPick.blockedReason}>
              {busy ? "Saving…" : agencyPick.blockedReason ? agencyPick.blockedReason : isEdit ? "Save Changes" : "Save Variable"}
            </Btn>
          </div>
        </div>
      }
    >
      <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        <div>
          <label style={labelStyle()}>Key *</label>
          {isEdit ? (
            <>
              <code style={{ display: "block", padding: "8px 12px", background: c.bg, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, fontFamily: c.mono, fontSize: c.fontSm }}>
                {key}
              </code>
          {/* RA-18 (display-only, per RA-Q22) — the owning department, beside the KEY
              because that is what it qualifies: which row a department's run resolves
              is a property of this row's identity, not a field to fill in.
              Deliberately NOT appended at the bottom of the form — this modal's body
              is already at its height, so a block added there lands exactly at the
              fold and renders shaved behind the pinned footer. tsc and the unit tests
              cannot see that; screenshotting the real dialog can. Read-only because
              ownership is fixed at creation and there is no transfer action, so an
              editable-looking control would promise what the API cannot do. */}
          {isEdit && initial?.ownerAgency && (
            <div style={{ display: "flex", alignItems: "center", gap: 6, flexWrap: "wrap", marginTop: 6, fontSize: c.fontXs, color: c.textMuted }}>
              <span>Owned by</span>
              <OwnerChip owner={initial.ownerAgency} />
              <span>&middot; set at creation</span>
            </div>
          )}
            </>
          ) : (
            <>
              <input
                value={key}
                onChange={(e) => onKeyChange(e.target.value)}
                placeholder="MY_VARIABLE_NAME"
                style={{ ...inputStyle(), fontFamily: c.mono, borderColor: keyError ? c.danger : c.borderStrong }}
              />
              {keyError && <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 4 }}>{keyError}</div>}
              <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4 }}>Uppercase A–Z, 0–9 and _ only</div>
            </>
          )}
        </div>
        <div>
          <label style={labelStyle()}>Scope / Target *</label>
          <select value={scope} onChange={(e) => setScope(e.target.value)} style={{ ...inputStyle(), cursor: "pointer" }}>
            {scopeNames.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </div>
        <div>
          <label style={labelStyle()}>Value</label>
          <textarea
            value={value}
            onChange={(e) => setValue(e.target.value)}
            placeholder="Enter value, or paste a multi-line key (newlines preserved)…"
            spellCheck={false}
            rows={value.includes("\n") ? 8 : 1}
            style={{ ...inputStyle(), fontFamily: value.includes("\n") ? c.mono : "inherit", fontSize: c.fontSm, lineHeight: 1.5, resize: "vertical", minHeight: 38 }}
          />
        </div>
        <div>
          <label style={labelStyle()}>Description</label>
          <input
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder="Optional — what does this variable do?"
            style={inputStyle()}
          />
        </div>
        {/* RA-18 (partial, per RA-Q22) — the OWNER, shown but not settable. Ownership
            decides which row a department's run resolves, so a form that never named
            it left the most consequential property of the row invisible at exactly
            the moment someone was reasoning about it. It is display-only on purpose:
            ownership is fixed at creation and there is no transfer action yet, so an
            editable-looking control would promise something the API cannot do. */}
        <CreationAgencyPicker
          label="variable"
          required={agencyPick.required}
          agencies={agencyPick.agencies}
          selected={agencyPick.selected}
          setSelected={agencyPick.setSelected}
        />
      </div>
    </Modal>
  );
}

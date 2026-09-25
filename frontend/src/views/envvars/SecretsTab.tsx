import { Fragment, useEffect, useState } from "react";
import { api, fetchCapabilities } from "../../api/client";
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

type SecretSource = "stored" | "vault";

interface EnvSecretRow {
  id?: string; // UUIDv7 (K-3)
  key: string;
  reference?: string;
  source?: SecretSource;
  scope?: string;
  description?: string;
  vaultPath?: string | null;
  tags?: string[];
  ownerAgency?: string; // RA-15 — the owning department's NAME; "" when shared.
  createdBy?: string;
  lastModifiedAt?: string;
}

const MASK = "••••••••••••••••";

// Default column widths (px) so table-layout:fixed has a sensible starting point
// before the user drags (V1.1-7). Stored overrides come from useColumnWidths.
const COL_W: Record<string, number> = {
  expand: 44,
  key: 240,
  scope: 130,
  usedBy: 120,
  agencies: 150,
  source: 110,
  description: 180,
  tags: 150,
  actions: 150,
};

export function SecretsTab({ scopeNames, canEdit }: { scopeNames: string[]; canEdit: boolean }) {
  const [dep, setDep] = useState(0);
  const { data, error, loading } = useGet<unknown>(() => api.GET("/env-secrets"), [dep]);
  const items = rows<EnvSecretRow>(data);

  // Vault-source secrets are only offered when Vault is actually configured
  // (C.3); otherwise the local-KEK "stored" path is the only active option.
  const [vaultEnabled, setVaultEnabled] = useState(false);
  useEffect(() => {
    let cancelled = false;
    fetchCapabilities().then((caps) => {
      if (!cancelled) setVaultEnabled(caps.vault);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  // T1.9 — binding counts, so an admin can see what a secret is wired into before
  // editing its scope (the edit that silently strands every binding).
  const usage = useReferenceUsage(dep);

  // RA-10 — a scoped secret shadowing a global one of the same key. Highest
  // consequence of the three kinds: the scoped row is what actually gets injected,
  // and an unmembered one is injected for EVERY department's runs in that scope.
  const shadows = useShadowWarnings(dep);
  const ambiguities = useAmbiguityWarnings(dep); // RA-18 — see VariablesTab

  const [search, setSearch] = useState("");
  const [scopeFilter, setScopeFilter] = useState("");
  const [tagFilter, setTagFilter] = useState<string[]>([]);
  const [tagMatch, setTagMatch] = useState<"any" | "all">("any");
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<EnvSecretRow | null>(null);
  const [deleting, setDeleting] = useState<EnvSecretRow | null>(null);
  const [migrating, setMigrating] = useState<EnvSecretRow | null>(null);
  const [expanded, setExpanded] = useState<EnvSecretRow["id"] | null>(null);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<{ kind: "info" | "error"; text: string } | null>(null);
  // Transient client-side cache of revealed plaintext, keyed by secret id.
  // Held only in component state — cleared on Hide, tab switch, or unmount.
  const [revealed, setRevealed] = useState<Record<string, string>>({});
  const [revealBusy, setRevealBusy] = useState<string | null>(null);
  const cw = useColumnWidths("envvars-secrets");
  // RB-22 — each row shows its full agency membership; the matrix that used to
  // answer this is gone. `dep` reuses the row refetch, so a membership edit made
  // in the Agencies tab shows up here on the same refresh cadence as everything.
  const entityAgencies = useEntityAgencies("secret", dep);

  // Operator-authored tags (migration 470) — plaintext metadata, never the
  // encrypted value; optimistic edits via PUT /env-secret-tags/{id}.
  const tagState = useInlineTags<EnvSecretRow>(
    dep,
    (v) => String(v.id),
    (v) => v.tags,
    (v, next) =>
      api.PUT("/env-secret-tags/{secretId}", { params: { path: { secretId: v.id! }, header: csrfHeader }, body: { tags: next } }),
  );

  // EP-4 — two meanings that used to share one function. `reloadList` re-reads
  // the rows and nothing else; `refetch` additionally collapses the expansion,
  // which is right after a create/delete (the row you were looking at may be
  // gone) and WRONG for the panel's own Refresh button — that would collapse
  // the very panel it was asked to refresh.
  const reloadList = () => {
    setRevealed({});
    setDep((n) => n + 1);
  };
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

  // Sortable columns (TS-13, the sorting-update plan) — in-component because
  // Used by reads the usage hook (total binding count, jobs + scripts).
  // Description, Tags, and the expand/actions columns stay unsortable.
  const sortCols: SortColumn<EnvSecretRow>[] = [
    { key: "key", get: (v) => v.key },
    { key: "scope", get: (v) => v.scope },
    {
      key: "usedBy",
      get: (v) => {
        const u = usage.usageFor("secret", v.key);
        return u.jobs + u.scripts;
      },
      type: "number",
    },
    { key: "source", get: (v) => v.source },
  ];
  const sort = useTableSort(filtered, sortCols, { key: "key", dir: "asc" }, { tableId: "envvars-secrets" });

  // Sortable resizable header: the drag handle stops propagation, so resizing
  // never toggles the sort. Active column reads c.text, inactive c.textSec.
  // CO-4 — the column spec replaces this tab's sortHead helper. Built in render:
  // cells read `c.*` and close over `expanded`, the usage/shadow resolvers and
  // canEdit.
  const cols = useTableColumns<EnvSecretRow>("envvars-secrets", [
    {
      key: "expand",
      label: "",
      menuLabel: "Expand",
      width: COL_W.expand,
      fixed: true,
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
          <ShadowBadge shadow={shadows.shadowFor("secret", v.key, v.scope)} />
          <AmbiguityBadge ambiguity={ambiguities.ambiguityFor("secret", v.key, v.scope)} />
        </div>
      ),
    },
    {
      key: "scope",
      label: "Scope",
      sortKey: "scope",
      width: COL_W.scope,
      // T1.8 — an unscoped row is GLOBAL, and a global secret is injectable from
      // every scope. A blank cell said neither.
      cell: (v) => (
        <span style={{ color: v.scope ? c.text : c.textSec, fontStyle: v.scope ? "normal" : "italic" }}>{scopeCell(v.scope)}</span>
      ),
    },
    { key: "usedBy", label: "Used by", sortKey: "usedBy", width: COL_W.usedBy, cell: (v) => <UsageCell {...usage.usageFor("secret", v.key)} /> },
    { key: "agencies", label: "Agencies", width: COL_W.agencies, cell: (v) => <AgencyCell names={v.id ? entityAgencies[v.id] : undefined} /> },
    {
      key: "source",
      label: "Source",
      sortKey: "source",
      width: COL_W.source,
      fixed: true,
      cell: (v) => {
        const isVault = v.source === "vault";
        return (
          <span
            style={{
              display: "inline-flex",
              padding: "2px 8px",
              borderRadius: c.radiusChip,
              fontSize: c.fontXs,
              fontWeight: 600,
              background: isVault ? `${c.primary}22` : c.panel2,
              color: isVault ? c.primary : c.textSec,
            }}
          >
            {isVault ? "Vault" : "Stored"}
          </span>
        );
      },
    },
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

  const toggleReveal = async (row: EnvSecretRow) => {
    if (row.id == null) return;
    const id = row.id;
    if (revealed[id] !== undefined) {
      setRevealed((r) => {
        const next = { ...r };
        delete next[id];
        return next;
      });
      return;
    }
    setRevealBusy(id);
    // POST (not GET) — every reveal is audit-logged server-side. Value is never logged here.
    const { data: d, error: e } = await api.POST("/env-secrets/{secretId}/reveal", {
      params: { path: { secretId: id }, header: csrfHeader },
    });
    setRevealBusy(null);
    if (e) setNotice({ kind: "error", text: `Reveal failed: ${errMsg(e)}` });
    else setRevealed((r) => ({ ...r, [id]: (d as { value?: string })?.value ?? "" }));
  };

  const doDelete = async () => {
    if (deleting?.id == null) return;
    setBusy(true);
    const { error: e } = await api.DELETE("/env-secrets/{secretId}", {
      params: { path: { secretId: deleting.id }, header: csrfHeader },
    });
    setBusy(false);
    setNotice(e ? { kind: "error", text: `Delete failed: ${errMsg(e)}` } : { kind: "info", text: `Secret deleted: ${deleting.key}` });
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
          <SearchBar value={search} onChange={setSearch} placeholder="Search secrets by key or description..." />
        </div>
        <ScopeFilterSelect scopes={scopeNames.filter((s) => s !== ALL_SCOPE_OPTION)} value={scopeFilter} onChange={setScopeFilter} />
        <TagFilterSelect items={items} selected={tagFilter} onChange={setTagFilter} getTags={(v) => tagState.tagsFor(v)} matchMode={tagMatch} onMatchModeChange={setTagMatch} />
        <ColumnsMenu cols={cols} cw={cw} />
        {canEdit && (
          <Btn primary onClick={() => setAdding(true)}>
            + Add Secret
          </Btn>
        )}
      </div>

      <NamespaceHelp>
        Reference a secret in a script or inventory as{" "}
        <code style={{ fontFamily: c.mono }}>CRONOMICON_SECRET_&lt;key&gt;</code> — it resolves to the secret value at run
        time and is always redacted in logs. This works whether the secret is stored in Cronomicon or Vault-backed; rows keep
        their bare key.
      </NamespaceHelp>

      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {error && <div style={{ color: c.danger }}>Error: {error}</div>}
      {/* VU-14 — "Add one above" was also shown to a viewer with no Add button and
          whenever a filter merely excluded every row. Two states, two next steps. */}
      {!loading && !error && filtered.length === 0 && (
        <div style={{ color: c.textSec }}>
          {items.length === 0 ? (
            <>
              <div>No secrets yet. A secret is injected as CRONOMICON_SECRET_&lt;key&gt; and always redacted in logs.</div>
              {canEdit && (
                <div style={{ marginTop: 12 }}>
                  <Btn small onClick={() => setAdding(true)}>
                    Add a secret
                  </Btn>
                </div>
              )}
            </>
          ) : (
            <>
              <div>No secrets match the current search, scope, or tags.</div>
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
              const isVault = v.source === "vault";
              const shown = v.id != null ? revealed[v.id] : undefined;
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
                            <DetailLabel
                              action={
                                !isVault && canEdit ? (
                                  <span
                                    onClick={() => toggleReveal(v)}
                                    style={{ cursor: "pointer", color: c.primary, fontSize: c.fontSm, whiteSpace: "nowrap", flexShrink: 0 }}
                                  >
                                    {revealBusy === v.id ? "…" : shown !== undefined ? "Hide" : "Reveal"}
                                  </span>
                                ) : undefined
                              }
                            >
                              {isVault ? "Vault Reference" : "Secret Value"}
                            </DetailLabel>
                            <div
                              style={{
                                ...valueBoxStyle(),
                                fontFamily: c.mono,
                                fontSize: isVault ? c.fontXs : shown !== undefined ? c.fontSm : c.fontXs,
                                color: isVault ? c.primary : shown !== undefined ? c.text : c.textMuted,
                                letterSpacing: !isVault && shown === undefined ? 2 : 0,
                              }}
                              title={isVault ? "Vault reference — resolved at run time" : undefined}
                            >
                              {isVault ? v.vaultPath || "—" : shown !== undefined ? shown : MASK}
                            </div>
                          </div>
                          <div>
                            <DetailLabel>Details</DetailLabel>
                            <div style={detailBoxStyle()}>
                              <DetailRow label="Scope" value={v.scope || "global (injectable from every scope)"} />
                              <DetailRow label="Used by" value={<UsageCell {...usage.usageFor("secret", v.key)} />} />
                              <DetailRow label="Description" value={v.description || "—"} />
                              <DetailRow
                                label="Source"
                                value={
                                  <span
                                    style={{
                                      display: "inline-flex",
                                      padding: "2px 7px",
                                      borderRadius: c.radiusChip,
                                      fontSize: c.fontXs,
                                      fontWeight: 600,
                                      background: isVault ? `${c.primary}22` : c.panel2,
                                      color: isVault ? c.primary : c.textMuted,
                                    }}
                                  >
                                    {isVault ? "Vault reference" : "Stored in Cronomicon"}
                                  </span>
                                }
                              />
                              <DetailRow label={isVault ? "Last Resolved" : "Last Rotated"} value={fmtDate(v.lastModifiedAt)} />
                              <DetailRow label="Created By" value={v.createdBy || "—"} last />
                              {!isVault && vaultEnabled && (
                                <div style={{ marginTop: 10, paddingTop: 10, borderTop: `1px solid ${c.borderLight}`, display: "flex", alignItems: "center", gap: 10 }}>
                                  <Btn onClick={() => setMigrating(v)}>Migrate to Vault</Btn>
                                  <span style={{ fontSize: c.fontXs, color: c.textMuted }}>Converts this entry to a vault reference</span>
                                </div>
                              )}
                            </div>
                          </div>
                        </DetailGrid>
                        </DetailPanel>
                        <ReferenceField reference={v.reference} semantics="Resolves to the secret value at run time (always redacted in logs)." />
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
        <SecretFormModal
          initial={editing}
          scopeNames={scopeNames}
          vaultEnabled={vaultEnabled}
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
          title="Delete Secret"
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

      {migrating && (
        <MigrateModal
          row={migrating}
          onClose={() => setMigrating(null)}
          onDone={(text, kind) => {
            setMigrating(null);
            setNotice({ kind, text });
            if (kind === "info") refetch();
          }}
        />
      )}
    </div>
  );
}

function SecretFormModal({
  initial,
  scopeNames,
  vaultEnabled,
  onClose,
  onSaved,
}: {
  initial: EnvSecretRow | null;
  scopeNames: string[];
  vaultEnabled: boolean;
  onClose: () => void;
  onSaved: (msg: string) => void;
}) {
  const isEdit = initial != null;
  const [key, setKey] = useState(initial?.key ?? "");
  const [source, setSource] = useState<SecretSource>(initial?.source ?? "stored");
  // A global row has no scope, so it initializes to the "All" sentinel and
  // round-trips back to global on save (scopeForWrite).
  const [scope, setScope] = useState(initial?.scope || ALL_SCOPE_OPTION);
  const [value, setValue] = useState("");
  const [vaultPath, setVaultPath] = useState(initial?.vaultPath ?? "");
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
    if (source === "vault" && !vaultPath.trim()) {
      setFormError("Vault path is required for vault-source secrets");
      return;
    }
    if (source === "stored" && !isEdit && !value) {
      setFormError("Secret value is required for stored secrets");
      return;
    }
    if (agencyPick.blockedReason) {
      setFormError(agencyPick.blockedReason);
      return;
    }
    setBusy(true);
    const body = {
      key,
      source,
      scope: scopeForWrite(scope),
      description,
      ...(source === "stored" && value !== "" ? { value } : {}),
      ...(source === "vault" ? { vaultPath: vaultPath.trim() } : {}),
      ...agencyPick.body,
    };
    const res =
      isEdit && initial.id != null
        ? await api.PUT("/env-secrets/{secretId}", { params: { path: { secretId: initial.id }, header: csrfHeader }, body })
        : await api.POST("/env-secrets", { params: { header: csrfHeader }, body });
    setBusy(false);
    if (res.error) setFormError(errMsg(res.error));
    else onSaved(`Secret ${isEdit ? "updated" : "created"}: ${key}`);
  };

  return (
    <Modal
      title={isEdit ? "Edit Secret" : "New Secret"}
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
              {busy ? "Saving…" : agencyPick.blockedReason ? agencyPick.blockedReason : isEdit ? "Save Changes" : "Save Secret"}
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
                placeholder="MY_SECRET_NAME"
                style={{ ...inputStyle(), fontFamily: c.mono, borderColor: keyError ? c.danger : c.borderStrong }}
              />
              {keyError && <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 4 }}>{keyError}</div>}
              <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4 }}>Uppercase A–Z, 0–9 and _ only</div>
            </>
          )}
        </div>
        {!isEdit && (
          <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
            <span style={{ fontSize: c.fontSm, color: c.textSec, fontWeight: 500 }}>Source:</span>
            <div style={{ display: "flex", borderRadius: c.radiusChip, overflow: "hidden", border: `1px solid ${c.border}` }}>
              {(
                [
                  { v: "stored", l: "Stored in Cronomicon" },
                  // Vault option only appears when Vault is configured (C.3).
                  ...(vaultEnabled ? [{ v: "vault", l: "Vault reference" }] : []),
                ] as { v: SecretSource; l: string }[]
              ).map((opt, i) => (
                <div
                  key={opt.v}
                  onClick={() => {
                    setSource(opt.v);
                    setValue("");
                    setVaultPath("");
                  }}
                  style={{
                    padding: "6px 12px",
                    cursor: "pointer",
                    fontSize: c.fontSm,
                    fontWeight: source === opt.v ? 600 : 400,
                    userSelect: "none",
                    background: source === opt.v ? c.primary : "transparent",
                    color: source === opt.v ? c.onSolid : c.textSec,
                    borderRight: i === 0 ? `1px solid ${c.border}` : "none",
                  }}
                >
                  {opt.l}
                </div>
              ))}
            </div>
          </div>
        )}
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
        {source === "vault" ? (
          <div>
            <label style={labelStyle()}>Vault Path *</label>
            <input
              value={vaultPath}
              onChange={(e) => setVaultPath(e.target.value)}
              placeholder="secret/data/myapp#KEY_NAME"
              style={{ ...inputStyle(), fontFamily: c.mono, fontSize: c.fontSm }}
            />
            <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4 }}>Format: engine/data/path#FIELD_NAME</div>
          </div>
        ) : (
          <div>
            <label style={labelStyle()}>{isEdit ? "New Secret Value" : "Secret Value *"}</label>
            <textarea
              value={value}
              onChange={(e) => setValue(e.target.value)}
              placeholder={isEdit ? "Leave blank to keep current value" : "Paste a value, or a full PEM private key (multi-line OK)"}
              spellCheck={false}
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              rows={value.includes("\n") ? 10 : 3}
              style={{ ...inputStyle(), fontFamily: c.mono, fontSize: c.fontSm, lineHeight: 1.5, resize: "vertical", minHeight: 38 }}
            />
            <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4 }}>
              Encrypted at rest; never echoed back by the API. Multi-line values (e.g. PEM/OpenSSH private keys) are preserved exactly — paste the whole key including the BEGIN/END lines.
            </div>
          </div>
        )}
        <div>
          <label style={labelStyle()}>Description</label>
          <input
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder="Optional — what does this secret do?"
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
          label="secret"
          required={agencyPick.required}
          agencies={agencyPick.agencies}
          selected={agencyPick.selected}
          setSelected={agencyPick.setSelected}
        />
      </div>
    </Modal>
  );
}

function MigrateModal({
  row,
  onClose,
  onDone,
}: {
  row: EnvSecretRow;
  onClose: () => void;
  onDone: (msg: string, kind: "info" | "error") => void;
}) {
  const [vaultPath, setVaultPath] = useState(`secret/data/cronomicon#${row.key}`);
  const [busy, setBusy] = useState(false);
  const [formError, setFormError] = useState("");

  const migrate = async () => {
    if (row.id == null) return;
    if (!vaultPath.trim()) {
      setFormError("Vault path is required");
      return;
    }
    setBusy(true);
    const { error: e } = await api.POST("/env-secrets/{secretId}/migrate-to-vault", {
      params: { path: { secretId: row.id }, header: csrfHeader },
      body: { vaultPath: vaultPath.trim() },
    });
    setBusy(false);
    if (e) onDone(`Migrate failed: ${errMsg(e)}`, "error");
    else onDone(`${row.key} migrated to Vault`, "info");
  };

  return (
    <Modal
      title="Migrate secret to Vault?"
      onClose={onClose}
      footer={
        <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
          {formError && <div style={{ fontSize: c.fontSm, color: c.danger }}>{formError}</div>}
          <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
            <Btn onClick={onClose} disabled={busy}>
              Cancel
            </Btn>
            <Btn primary onClick={migrate} disabled={busy}>
              {busy ? "Migrating…" : "Migrate to Vault"}
            </Btn>
          </div>
        </div>
      }
    >
      <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6, marginBottom: 14 }}>
        This moves <code style={{ fontFamily: c.mono }}>{row.key}</code> to the Vault path below and removes the stored
        value from Cronomicon. The vault reference will resolve at run time. There is no undo.
      </div>
      <div style={{ marginBottom: 14 }}>
        <label style={labelStyle()}>Vault Path *</label>
        <input
          value={vaultPath}
          onChange={(e) => setVaultPath(e.target.value)}
          style={{ ...inputStyle(), fontFamily: c.mono, fontSize: c.fontSm }}
        />
        <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4 }}>Format: engine/data/path#FIELD_NAME</div>
      </div>
    </Modal>
  );
}

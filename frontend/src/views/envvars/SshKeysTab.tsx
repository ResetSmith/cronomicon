import { Fragment, useState } from "react";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { useGet, rows, useColumnWidths, useInlineTags, useTableSort } from "../../hooks";
import { ColumnsMenu, TableHead, renderCells, useTableColumns } from "../../components/table";
import { CreationAgencyPicker, useCreationAgencies } from "../../components/CreationAgencyPicker";
import { type SortColumn } from "../../utils/sort";
import { c } from "../../theme";
import { pubKeyFilename, toOpenSSH, toRFC4716 } from "../../utils/sshpubkey";
import { CopyButton, CopyText, DetailPanel, EmptyCell, InlineTags, SkeletonRows, TagEditor, TagFilterSelect, matchesTags } from "../../components/ui";
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
  SearchBar,
  UsageCell,
  csrfHeader,
  detailBoxStyle,
  errMsg,
  fmtDate,
  inputStyle,
  labelStyle,
  useReferenceUsage,
  valueBoxStyle,
} from "./ui";
import { AgencyCell, useEntityAgencies } from "./ui";

type Credential = components["schemas"]["SshCredential"];
type CredentialInput = components["schemas"]["SshCredentialInput"];

// Default column widths (px) for table-layout:fixed before the user drags.
const COL_W: Record<string, number> = {
  expand: 44,
  label: 200,
  type: 120,
  fingerprint: 260,
  source: 90,
  usedBy: 90,
  boundBy: 110,
  agencies: 150,
  tags: 150,
  actions: 150,
};

// Copy text to the clipboard, best-effort.
function copy(text: string, ok: () => void) {
  navigator.clipboard?.writeText(text).then(ok, () => {});
}

// shortFp trims the SHA256: prefix display so the column doesn't dominate.
function shortFp(fp?: string | null): string {
  if (!fp) return "—";
  return fp.length > 30 ? fp.slice(0, 30) + "…" : fp;
}

/**
 * PubKeyDownload saves the public key as a file, in either of the two encodings
 * SSH implementations ask for. Both carry the SAME key: OpenSSH is the
 * authorized_keys line, RFC 4716 ("SSH2") is the wrapped form some enterprise
 * stacks — Tectia, certain SFTP appliances — require instead. The conversion is
 * text reshaping done here in the client, so no private material is involved and
 * the server is not asked for anything.
 *
 * Only rendered where a public key exists; a Vault-source credential has none
 * stored (it resolves at run time), so it has nothing to download.
 */
function PubKeyDownload({ publicKey, label, onError }: { publicKey: string; label: string; onError: (text: string) => void }) {
  const save = (format: "openssh" | "rfc4716") => {
    let text: string;
    try {
      text = format === "rfc4716" ? toRFC4716(publicKey, label) : toOpenSSH(publicKey);
    } catch {
      onError("This key's stored public material is not a readable OpenSSH public key, so it cannot be downloaded.");
      return;
    }
    const url = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
    const link = document.createElement("a");
    link.href = url;
    link.download = pubKeyFilename(label, format);
    link.click();
    URL.revokeObjectURL(url);
  };
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 8, marginTop: 8, flexWrap: "wrap" }}>
      <span style={{ fontSize: c.fontXs, color: c.textSec }}>Download as</span>
      <Btn small onClick={() => save("openssh")} ariaLabel={`Download OpenSSH format public key for ${label}`}>
        OpenSSH
      </Btn>
      <Btn small onClick={() => save("rfc4716")} ariaLabel={`Download RFC 4716 format public key for ${label}`}>
        RFC 4716
      </Btn>
    </div>
  );
}

export function SshKeysTab({ canEdit }: { canEdit: boolean }) {
  const [dep, setDep] = useState(0);
  // RB-22 — per-row agency membership; see SecretsTab.
  const entityAgencies = useEntityAgencies("ssh-credential", dep);
  const { data, error, loading } = useGet<unknown>(() => api.GET("/ssh/credentials"), [dep]);
  const creds = rows<Credential>(data);
  // Host/bastion lists give the used-by counts client-side (no N+1 /usage calls).
  const hostsQ = useGet<unknown>(() => api.GET("/ssh/hosts"), [dep]);
  const bastionsQ = useGet<unknown>(() => api.GET("/ssh/bastions"), [dep]);
  const hosts = rows<{ authCredentialId?: string | null }>(hostsQ.data);
  const bastions = rows<{ authCredentialId?: string | null }>(bastionsQ.data);
  const usedBy = (id: string) =>
    hosts.filter((h) => h.authCredentialId === id).length + bastions.filter((b) => b.authCredentialId === id).length;
  // T1.9 — distinct from "Used By" above: that counts hosts/bastions this key
  // AUTHENTICATES; this counts jobs/scripts that BIND it as CRONOMICON_KEY_<label>.
  // The two answer different questions and a key can have one without the other.
  const usage = useReferenceUsage(dep);

  const [search, setSearch] = useState("");
  const [tagFilter, setTagFilter] = useState<string[]>([]);
  const [tagMatch, setTagMatch] = useState<"any" | "all">("any");
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<Credential | null>(null);
  const [deleting, setDeleting] = useState<Credential | null>(null);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<{ kind: "info" | "error"; text: string } | null>(null);
  const cw = useColumnWidths("envvars-sshkeys");

  // Operator-authored tags (migration 470) — plaintext metadata beside the sealed
  // key; optimistic edits via PUT /ssh-credential-tags/{id}.
  const tagState = useInlineTags<Credential>(
    dep,
    (v) => v.id,
    (v) => v.tags,
    (v, next) =>
      api.PUT("/ssh-credential-tags/{credentialId}", { params: { path: { credentialId: v.id }, header: csrfHeader }, body: { tags: next } }),
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

  const filtered = creds.filter(
    (v) =>
      (!search ||
        v.label.toLowerCase().includes(search.toLowerCase()) ||
        (v.fingerprint || "").toLowerCase().includes(search.toLowerCase()) ||
        (v.description || "").toLowerCase().includes(search.toLowerCase())) &&
      matchesTags(tagState.tagsFor(v), tagFilter, tagMatch),
  );

  // VU-14 — hands the search + tag filter back from the filtered-empty state.
  const clearFilters = () => {
    setSearch("");
    setTagFilter([]);
  };

  // Sortable columns (TS-14, the sorting-update plan) — in-component because
  // Used By counts hosts/bastions (usedBy) and Bound By counts job/script
  // bindings (usage hook); both are counts, so type "number". Fingerprint,
  // Tags, and the expand/actions columns stay unsortable.
  const sortCols: SortColumn<Credential>[] = [
    { key: "label", get: (v) => v.label },
    { key: "type", get: (v) => v.keyType },
    { key: "source", get: (v) => v.source },
    { key: "usedBy", get: (v) => usedBy(v.id), type: "number" },
    {
      key: "boundBy",
      get: (v) => {
        const u = usage.usageFor("key", v.label);
        return u.jobs + u.scripts;
      },
      type: "number",
    },
  ];
  const sort = useTableSort(filtered, sortCols, { key: "label", dir: "asc" }, { tableId: "envvars-sshkeys" });

  // Fixed-width sortable header (hybrid policy): a plain <th> carrying the sort
  // toggle, no resize handle. Active column reads c.text, inactive c.textSec.
  // CO-4 — the column spec replaces this tab's sortHeadFixed helper.
  const cols = useTableColumns<Credential>("envvars-sshkeys", [
    {
      key: "expand",
      label: "",
      menuLabel: "Expand",
      width: COL_W.expand,
      fixed: true,
      pin: "first",
      tdStyle: { width: 36, textAlign: "center" },
      cell: (v) => <Chevron open={expanded === v.id} />,
    },
    { key: "label", label: "Label", sortKey: "label", width: COL_W.label, tdStyle: { fontWeight: 600, fontSize: c.fontSm }, cell: (v) => v.label },
    { key: "type", label: "Type", sortKey: "type", width: COL_W.type, fixed: true, tdStyle: { color: c.textSec, fontFamily: c.mono, fontSize: c.fontSm }, cell: (v) => v.keyType || <EmptyCell /> },
    {
      key: "fingerprint",
      label: "Fingerprint",
      width: COL_W.fingerprint,
      tdStyle: { fontFamily: c.mono, fontSize: c.fontXs, color: c.textSec },
      cell: (v) => <span title={v.fingerprint || ""}>{shortFp(v.fingerprint)}</span>,
    },
    {
      key: "source",
      label: "Source",
      sortKey: "source",
      width: COL_W.source,
      fixed: true,
      cell: (v) => {
        const isVault = v.source === "vault";
        return (
          <span style={{ display: "inline-flex", padding: "2px 8px", borderRadius: c.radiusChip, fontSize: c.fontXs, fontWeight: 600, background: isVault ? `${c.primary}22` : c.panel2, color: isVault ? c.primary : c.textSec }}>
            {isVault ? "Vault" : "Stored"}
          </span>
        );
      },
    },
    {
      key: "usedBy",
      label: "Used By",
      sortKey: "usedBy",
      width: COL_W.usedBy,
      fixed: true,
      cell: (v) => {
        const uses = usedBy(v.id);
        return <span style={{ color: uses > 0 ? c.text : c.textSec }}>{uses}</span>;
      },
    },
    { key: "boundBy", label: "Bound By", sortKey: "boundBy", width: COL_W.boundBy, fixed: true, cell: (v) => <UsageCell {...usage.usageFor("key", v.label)} /> },
    { key: "agencies", label: "Agencies", width: COL_W.agencies, cell: (v) => <AgencyCell names={v.id ? entityAgencies[v.id] : undefined} /> },
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

  const doDelete = async (force: boolean) => {
    if (!deleting) return;
    setBusy(true);
    const { error: e } = await api.DELETE("/ssh/credentials/{credentialId}", {
      params: { path: { credentialId: deleting.id }, query: { force }, header: csrfHeader },
    });
    setBusy(false);
    setNotice(e ? { kind: "error", text: `Delete failed: ${errMsg(e)}` } : { kind: "info", text: `SSH key deleted: ${deleting.label}` });
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
          <SearchBar value={search} onChange={setSearch} placeholder="Search SSH keys by label, fingerprint or description..." />
        </div>
        <TagFilterSelect items={creds} selected={tagFilter} onChange={setTagFilter} getTags={(v) => tagState.tagsFor(v)} matchMode={tagMatch} onMatchModeChange={setTagMatch} />
        <ColumnsMenu cols={cols} cw={cw} />
        {canEdit && (
          <Btn primary onClick={() => setAdding(true)}>
            + Add SSH Key
          </Btn>
        )}
      </div>

      <NamespaceHelp>
        Reference a key in a script or inventory as{" "}
        <code style={{ fontFamily: c.mono }}>CRONOMICON_KEY_&lt;label&gt;</code> — unlike the other tabs it resolves to a{" "}
        <strong>key-file path</strong> on the executing host (use it where a tool expects a key file, e.g.{" "}
        <code style={{ fontFamily: c.mono }}>ssh -i</code>), not the key bytes. Labels are POSIX identifiers so the
        reference is a valid env-var name.
      </NamespaceHelp>

      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {error && <div style={{ color: c.danger }}>Error: {error}</div>}
      {/* VU-14 — the "yet" copy was also what a no-match search returned, and the
          "Add one above" it offered is hidden without ConfigureApp. */}
      {!loading && !error && filtered.length === 0 && (
        <div style={{ color: c.textSec }}>
          {creds.length === 0 ? (
            <>
              <div>
                No SSH key credentials yet. Once one exists, attach it to a host or bastion under Settings → SSH
                Targets.
              </div>
              {canEdit && (
                <div style={{ marginTop: 12 }}>
                  <Btn small onClick={() => setAdding(true)}>
                    Add an SSH key
                  </Btn>
                </div>
              )}
            </>
          ) : (
            <>
              <div>No SSH keys match the current search or tags.</div>
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
              const isExp = expanded === v.id;
              const toggle = () => setExpanded(isExp ? null : v.id);
              const rowBg = isExp ? c.primaryBg : "transparent";
              const uses = usedBy(v.id);
              return (
                <Fragment key={v.id}>
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
                                v.publicKey ? (
                                  <CopyButton
                                    text={v.publicKey}
                                    variant="text"
                                    ariaLabel="Copy public key to clipboard"
                                    onCopied={() => setNotice({ kind: "info", text: "Public key copied" })}
                                    style={{ fontSize: c.fontSm, flexShrink: 0 }}
                                  />
                                ) : undefined
                              }
                            >
                              Public Key
                            </DetailLabel>
                            <div style={{ ...valueBoxStyle(), fontFamily: c.mono, fontSize: c.fontXs, color: v.publicKey ? c.text : c.textMuted, lineHeight: 1.5 }}>
                              {v.publicKey || (isVault ? "(resolved from Vault at run time)" : "—")}
                            </div>
                            <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4 }}>Add this line to the target account's authorized_keys.</div>
                            {v.publicKey && (
                              <PubKeyDownload publicKey={v.publicKey} label={v.label} onError={(text) => setNotice({ kind: "error", text })} />
                            )}
                          </div>
                          <div>
                            <DetailLabel>Details</DetailLabel>
                            <div style={detailBoxStyle()}>
                              <DetailRow label="Key Type" value={v.keyType || "—"} />
                              <DetailRow
                                label="Fingerprint"
                                value={
                                  v.fingerprint ? (
                                    <CopyText
                                      text={v.fingerprint}
                                      style={{ fontFamily: c.mono, fontSize: c.fontXs, color: c.primary }}
                                    />
                                  ) : (
                                    "—"
                                  )
                                }
                              />
                              <DetailRow label="Description" value={v.description || "—"} />
                              <DetailRow label="Used By" value={`${uses} host(s) / bastion(s)`} />
                              <DetailRow label="Bound By" value={<UsageCell {...usage.usageFor("key", v.label)} />} />
                              <DetailRow label="Last Rotated" value={fmtDate(v.lastModifiedAt)} />
                              <DetailRow label="Created By" value={v.createdBy || "—"} last />
                            </div>
                          </div>
                        </DetailGrid>
                        </DetailPanel>
                        <ReferenceField reference={v.reference} semantics="Resolves to a key-file path on the executing host (not the key bytes)." />
                        <div style={{ marginTop: 12 }}>
                          <DetailLabel>Tags</DetailLabel>
                          <TagEditor tags={tagState.tagsFor(v)} onChange={(next) => tagState.save(v, next)} disabled={!canEdit} />
                          {tagState.errors[v.id] && (
                            <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 6 }}>{tagState.errors[v.id]}</div>
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
        <SshKeyFormModal
          initial={editing}
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
          title="Delete SSH Key"
          message={
            usedBy(deleting.id) > 0 ? (
              <>
                <strong>{deleting.label}</strong> is referenced by <strong>{usedBy(deleting.id)}</strong> host(s)/bastion(s). Deleting it will detach
                it from them (they fall back to no key until reassigned). This cannot be undone.
              </>
            ) : (
              <>
                Delete <code style={{ fontFamily: c.mono }}>{deleting.label}</code>? This cannot be undone.
              </>
            )
          }
          confirmLabel={usedBy(deleting.id) > 0 ? "Force Delete" : "Delete"}
          busy={busy}
          onConfirm={() => doDelete(usedBy(deleting.id) > 0)}
          onCancel={() => setDeleting(null)}
        />
      )}
    </div>
  );
}

function SshKeyFormModal({
  initial,
  onClose,
  onSaved,
}: {
  initial: Credential | null;
  onClose: () => void;
  onSaved: (msg: string) => void;
}) {
  const isEdit = initial != null;
  const [label, setLabel] = useState(initial?.label ?? "");
  const [description, setDescription] = useState(initial?.description ?? "");
  const [material, setMaterial] = useState("");
  const [formError, setFormError] = useState("");
  const [busy, setBusy] = useState(false);
  // RF-Q2(a): the create-time agency binding, shown only to restricted callers.
  const agencyPick = useCreationAgencies(isEdit);
  // After a successful create/rotate, show the derived fingerprint + public key.
  const [result, setResult] = useState<Credential | null>(null);

  const save = async () => {
    if (!label.trim()) {
      setFormError("Label is required");
      return;
    }
    if (!isEdit && !material.trim()) {
      setFormError("Paste the private key");
      return;
    }
    if (agencyPick.blockedReason) {
      setFormError(agencyPick.blockedReason);
      return;
    }
    setBusy(true);
    setFormError("");
    const body: CredentialInput = {
      label: label.trim(),
      description: description || null,
      source: "stored",
      ...(material.trim() ? { material } : {}),
      ...agencyPick.body,
    };
    const res =
      isEdit && initial
        ? await api.PUT("/ssh/credentials/{credentialId}", { params: { path: { credentialId: initial.id }, header: csrfHeader }, body })
        : await api.POST("/ssh/credentials", { params: { header: csrfHeader }, body });
    setBusy(false);
    if (res.error) {
      setFormError(errMsg(res.error));
      return;
    }
    const saved = res.data as Credential;
    // Show the derived material when we have a public key to surface (rotation
    // with new material, or a create). A metadata-only edit closes directly.
    if (saved?.publicKey && material.trim()) {
      setResult(saved);
    } else {
      onSaved(`SSH key ${isEdit ? "updated" : "created"}: ${label.trim()}`);
    }
  };

  if (result) {
    return (
      <Modal
        title="SSH key saved"
        onClose={() => onSaved(`SSH key ${isEdit ? "updated" : "created"}: ${result.label}`)}
        /* FX-10 — the footer expression lives inside this branch, where `result`
           is still narrowed to non-null by the guard above. */
        footer={
          <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
            <Btn onClick={() => copy(result.publicKey ?? "", () => {})}>Copy public key</Btn>
            <Btn primary onClick={() => onSaved(`SSH key ${isEdit ? "updated" : "created"}: ${result.label}`)}>
              Done
            </Btn>
          </div>
        }
      >
        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          <div style={{ fontSize: c.fontSm, color: c.textSec }}>
            Validated and encrypted at rest. Add the public key below to each target account's <code style={{ fontFamily: c.mono }}>authorized_keys</code>.
          </div>
          <div>
            <label style={labelStyle()}>Key Type · Fingerprint</label>
            <div style={{ ...detailBoxStyle(), fontFamily: c.mono, fontSize: c.fontSm }}>
              {result.keyType || "—"} · {result.fingerprint || "—"}
            </div>
          </div>
          <div>
            <label style={labelStyle()}>Public Key</label>
            <textarea
              readOnly
              value={result.publicKey ?? ""}
              rows={3}
              onFocus={(e) => e.currentTarget.select()}
              style={{ ...inputStyle(), fontFamily: c.mono, fontSize: c.fontXs, lineHeight: 1.5, resize: "vertical" }}
            />
            {result.publicKey && <PubKeyDownload publicKey={result.publicKey} label={result.label} onError={setFormError} />}
            {formError && <div style={{ fontSize: c.fontSm, color: c.danger, marginTop: 8 }}>{formError}</div>}
          </div>
        </div>
      </Modal>
    );
  }

  return (
    <Modal
      title={isEdit ? "Edit SSH Key" : "New SSH Key"}
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
              {busy ? "Saving…" : agencyPick.blockedReason ? agencyPick.blockedReason : isEdit ? "Save Changes" : "Save SSH Key"}
            </Btn>
          </div>
        </div>
      }
    >
      <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        <div>
          <label style={labelStyle()}>Label *</label>
          <input value={label} onChange={(e) => setLabel(e.target.value)} placeholder="prod-deploy-ed25519" style={inputStyle()} />
        </div>
        <div>
          <label style={labelStyle()}>Description</label>
          <input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="Optional — what is this key for?" style={inputStyle()} />
        </div>
        <div>
          <label style={labelStyle()}>{isEdit ? "New Private Key (rotate)" : "Private Key *"}</label>
          <textarea
            value={material}
            onChange={(e) => setMaterial(e.target.value)}
            placeholder={isEdit ? "Leave blank to keep the current key" : "-----BEGIN OPENSSH PRIVATE KEY-----\n…paste the whole unencrypted key…\n-----END OPENSSH PRIVATE KEY-----"}
            spellCheck={false}
            autoComplete="off"
            autoCapitalize="off"
            autoCorrect="off"
            rows={material.includes("\n") ? 10 : 4}
            style={{ ...inputStyle(), fontFamily: c.mono, fontSize: c.fontSm, lineHeight: 1.5, resize: "vertical", minHeight: 80 }}
          />
          <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4 }}>
            Validated on save and encrypted at rest; never echoed back. Paste the whole unencrypted PEM/OpenSSH key including the BEGIN/END lines. Passphrase-protected keys are not supported.
          </div>
        </div>
        <CreationAgencyPicker
          label="SSH key"
          required={agencyPick.required}
          agencies={agencyPick.agencies}
          selected={agencyPick.selected}
          setSelected={agencyPick.setSelected}
        />
      </div>
    </Modal>
  );
}

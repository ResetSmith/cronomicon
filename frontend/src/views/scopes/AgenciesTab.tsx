import { Fragment, useState } from "react";
import { api } from "../../api/client";
import { useGet, rows, useTableSort } from "../../hooks";
import { type SortColumn } from "../../utils/sort";
import { c } from "../../theme";
import { DOC_LINKS } from "../../components/docLinks";
import { DetailPanel, InlineLoading, SkeletonRows, SortableLabel, DocLink } from "../../components/ui";
import { RefreshScope } from "../../components/RefreshScope";
import {
  Btn,
  Chevron,
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
  thStyle,
} from "../envvars/ui";

interface AgencyRow {
  id?: string;
  name: string;
  description?: string | null;
  onlineRunnerCount?: number; // M4 coverage — 0 means jobs bound here will wait
  createdBy?: string;
  lastModifiedAt?: string;
}

// Sortable columns (Phase 2, the sorting-update plan §3.2), driven by
// useTableSort. Online runners sorts by the same count the cell shows (missing
// counts render as 0, so they sort as 0 too); Description and the expand /
// actions columns stay unsortable.
const SORT_COLS: SortColumn<AgencyRow>[] = [
  { key: "name", get: (a) => a.name, type: "text" },
  { key: "runners", get: (a) => a.onlineRunnerCount ?? 0, type: "number" },
  { key: "modified", get: (a) => a.lastModifiedAt, type: "date" },
];

// AgenciesTab — the agency (network-isolation zone) catalog (agency-support.md M1).
// Operator-managed registry. Since RB-22 each row EXPANDS INTO AN EDITOR for what
// the agency contains — the job the deleted Membership matrix used to do, done
// agency-first so the thing being edited stays named on screen.
//
// T4.2/T4.3: each row expands into what the agency actually CONTAINS, and states
// the trap the originating investigation hit — an agency with no online runner
// queues every run targeting it, forever. That was previously visible only on an
// individual run, which is exactly why it cost someone a debugging session.
export function AgenciesTab({ canEdit }: { canEdit: boolean }) {
  const [dep, setDep] = useState(0);
  const { data, error, loading } = useGet<unknown>(() => api.GET("/agencies"), [dep]);
  const items = rows<AgencyRow>(data);

  const [search, setSearch] = useState("");
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<AgencyRow | null>(null);
  const [deleting, setDeleting] = useState<AgencyRow | null>(null);
  const [busy, setBusy] = useState(false);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [notice, setNotice] = useState<{ kind: "info" | "error"; text: string } | null>(null);

  const refetch = () => {
    setExpanded(null);
    setDep((n) => n + 1);
  };

  const filtered = items.filter(
    (a) =>
      !search ||
      a.name.toLowerCase().includes(search.toLowerCase()) ||
      (a.description || "").toLowerCase().includes(search.toLowerCase()),
  );

  // Sort after the search filter (§3.1).
  const sort = useTableSort(filtered, SORT_COLS, { key: "name", dir: "asc" }, { tableId: "agencies" });

  const doDelete = async () => {
    if (deleting?.id == null) return;
    setBusy(true);
    const { error: e } = await api.DELETE("/agencies/{agencyId}", {
      params: { path: { agencyId: deleting.id }, header: csrfHeader },
    });
    setBusy(false);
    setNotice(
      e
        ? { kind: "error", text: `Delete failed: ${errMsg(e)}` }
        : { kind: "info", text: `Agency deleted: ${deleting.name}` },
    );
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
      <div style={{ marginBottom: 12, color: c.textSec, fontSize: c.fontSm, lineHeight: 1.55 }}>
        Agencies are network-isolation zones, and the single membership axis for scopes, secrets, variables, SSH
        keys and runners. A run goes only to a runner sharing one of its scope's agencies, and injects only
        references whose agencies overlap. <strong>Expand a row to see and edit what it contains.</strong> To go
        the other way — which agencies hold one secret, variable, key, scope or runner — that entity's own
        catalog row carries an Agencies column.{" "}
        <DocLink href={DOC_LINKS.agencies}>How membership and ownership work</DocLink>
      </div>
      <div style={{ display: "flex", gap: 8, marginBottom: 16 }}>
        <div style={{ flex: 1 }}>
          <SearchBar value={search} onChange={setSearch} placeholder="Search agencies by name or description..." />
        </div>
        {canEdit && (
          <Btn primary onClick={() => setAdding(true)}>
            + Add Agency
          </Btn>
        )}
      </div>

      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {error && <div style={{ color: c.danger }}>Error: {error}</div>}
      {/* VU-14 — "No agencies yet." was also what a no-match search returned. The
          two states now differ, and each carries the control that resolves it:
          Add Agency (the same handler as the toolbar, hidden without ConfigureApp)
          or the search back. */}
      {!loading && !error && filtered.length === 0 && (
        <div style={{ color: c.textSec }}>
          {items.length === 0 ? (
            <>
              <div>No agencies yet. Until one exists, every scope, credential and runner is unrestricted.</div>
              {canEdit && (
                <div style={{ marginTop: 12 }}>
                  <Btn small onClick={() => setAdding(true)}>
                    Add an agency
                  </Btn>
                </div>
              )}
            </>
          ) : (
            <>
              <div>No agencies match “{search.trim()}”.</div>
              <div style={{ marginTop: 12 }}>
                <Btn small onClick={() => setSearch("")}>
                  Clear search
                </Btn>
              </div>
            </>
          )}
        </div>
      )}
      {filtered.length > 0 && (
        <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontBody }}>
          <thead>
            <tr style={{ color: c.textSec, textAlign: "left", borderBottom: `1px solid ${c.border}` }}>
              <th style={{ ...thStyle(), width: 44 }}></th>
              <th
                onClick={() => sort.toggle("name")}
                title="Sort by Name"
                aria-sort={sort.ariaSort("name")}
                style={{ ...thStyle(), cursor: "pointer", color: sort.isActive("name") ? c.text : c.textSec }}
              >
                <SortableLabel label="Name" active={sort.isActive("name")} dir={sort.sortDir} />
              </th>
              <th style={thStyle()}>Description</th>
              <th
                onClick={() => sort.toggle("runners")}
                title="Sort by Online runners"
                aria-sort={sort.ariaSort("runners")}
                style={{ ...thStyle(), cursor: "pointer", color: sort.isActive("runners") ? c.text : c.textSec }}
              >
                <SortableLabel label="Online runners" active={sort.isActive("runners")} dir={sort.sortDir} />
              </th>
              <th
                onClick={() => sort.toggle("modified")}
                title="Sort by Last Modified"
                aria-sort={sort.ariaSort("modified")}
                style={{ ...thStyle(), cursor: "pointer", color: sort.isActive("modified") ? c.text : c.textSec }}
              >
                <SortableLabel label="Last Modified" active={sort.isActive("modified")} dir={sort.sortDir} />
              </th>
              <th style={thStyle()}></th>
            </tr>
          </thead>
          <tbody>
            {sort.sorted.map((a) => {
              const isExp = a.id != null && expanded === a.id;
              const noRunners = (a.onlineRunnerCount ?? 0) === 0;
              return (
                <Fragment key={a.id ?? a.name}>
                  <tr
                    onClick={() => setExpanded(isExp ? null : (a.id ?? null))}
                    style={{ borderBottom: isExp ? "none" : `1px solid ${c.border}`, cursor: "pointer", background: isExp ? c.primaryBg : "transparent" }}
                  >
                    <td style={{ ...tdStyle(), borderBottom: "none", textAlign: "center" }}>
                      <Chevron open={isExp} />
                    </td>
                    <td style={{ ...tdStyle(), borderBottom: "none", fontWeight: 600 }}>{a.name}</td>
                    <td style={{ ...tdStyle(), borderBottom: "none", color: c.textSec, fontSize: c.fontSm }}>{a.description || "—"}</td>
                    <td style={{ ...tdStyle(), borderBottom: "none", fontSize: c.fontSm }}>
                      <span style={{ fontWeight: 600, color: noRunners ? c.warning : c.text }}>
                        {a.onlineRunnerCount ?? 0}
                      </span>
                      {noRunners && (
                        <span style={{ color: c.warning, fontSize: c.fontXs, marginLeft: 6 }}>runs will queue</span>
                      )}
                    </td>
                    <td style={{ ...tdStyle(), borderBottom: "none", color: c.textSec, fontSize: c.fontSm }}>{fmtDate(a.lastModifiedAt)}</td>
                    <td style={{ ...tdStyle(), borderBottom: "none" }} onClick={(e) => e.stopPropagation()}>
                      {canEdit && (
                        <div style={{ display: "flex", gap: 6, justifyContent: "flex-end" }}>
                          <Btn onClick={() => setEditing(a)}>Edit</Btn>
                          <Btn danger onClick={() => setDeleting(a)}>
                            Delete
                          </Btn>
                        </div>
                      )}
                    </td>
                  </tr>
                  {isExp && a.id != null && (
                    <tr style={{ background: c.primaryBg, borderBottom: `1px solid ${c.border}` }}>
                      <td style={{ borderBottom: "none" }} />
                      <td colSpan={5} style={{ padding: "2px 16px 16px", borderBottom: "none" }} onClick={(e) => e.stopPropagation()}>
                        <RefreshScope>
                        <AgencyDetailPanel agencyId={a.id} canEdit={canEdit} onMembersChanged={() => setDep((n) => n + 1)} />
                        </RefreshScope>
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
        <AgencyFormModal
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
          title="Delete Agency"
          message={
            <>
              Delete agency <code style={{ fontFamily: c.mono }}>{deleting.name}</code>? A scope still bound
              to it will block deletion.
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

function AgencyFormModal({
  initial,
  onClose,
  onSaved,
}: {
  initial: AgencyRow | null;
  onClose: () => void;
  onSaved: (msg: string) => void;
}) {
  const isEdit = initial != null;
  const [name, setName] = useState(initial?.name ?? "");
  const [description, setDescription] = useState(initial?.description ?? "");
  const [formError, setFormError] = useState("");
  const [busy, setBusy] = useState(false);

  const save = async () => {
    if (!name.trim()) {
      setFormError("Name is required");
      return;
    }
    setBusy(true);
    const body = { name: name.trim(), description: description || null };
    const res =
      isEdit && initial.id != null
        ? await api.PUT("/agencies/{agencyId}", { params: { path: { agencyId: initial.id }, header: csrfHeader }, body })
        : await api.POST("/agencies", { params: { header: csrfHeader }, body });
    setBusy(false);
    if (res.error) setFormError(errMsg(res.error));
    else onSaved(`Agency ${isEdit ? "updated" : "created"}: ${name.trim()}`);
  };

  return (
    <Modal
      title={isEdit ? "Edit Agency" : "New Agency"}
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
              {busy ? "Saving…" : isEdit ? "Save Changes" : "Save Agency"}
            </Btn>
          </div>
        </div>
      }
    >
      <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        <div>
          <label style={labelStyle()}>Name *</label>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="e.g. agency-alpha"
            style={inputStyle()}
          />
        </div>
        <div>
          <label style={labelStyle()}>Description</label>
          <input
            value={description ?? ""}
            onChange={(e) => setDescription(e.target.value)}
            placeholder="Optional — which network / tenant is this?"
            style={inputStyle()}
          />
        </div>
      </div>
    </Modal>
  );
}

// AgencyDetailPanel answers "what is in this agency, and will anything actually
// run here?" (T4.2/T4.3) — and since RB-22, edits the answer in place.
//
// This panel replaced the Membership matrix tab. The matrix was rows × agency-
// columns over EVERY isolated entity, and at two dozen departments its name cell
// scrolled away while you ticked checkboxes — the same horizontal wall as the
// deleted Scope Restrictions grid, with far more rows. Working agency-first keeps
// both name and membership on screen at once, because the question an operator
// actually brings is "what does Tax contain?", not "show me everything times
// everything".
//
// Writes go through PUT /agencies/{id}/members, the agency-scoped setter: it
// touches only this agency's rows, so two admins editing two different
// departments cannot race on the same entity — the lost-update hazard a
// read-modify-write through the entity-centric setters would reintroduce.
function AgencyDetailPanel({ agencyId, canEdit, onMembersChanged }: {
  agencyId: string;
  canEdit: boolean;
  onMembersChanged?: () => void;
}) {
  interface Member {
    kind: string;
    id: string;
    name: string;
    scope?: string;
    status?: string;
  }
  interface Detail {
    members?: Member[];
    onlineRunners?: number;
    queuedRuns?: number;
  }
  // Candidates for the add-picker come from the same matrix endpoint the deleted
  // tab used — the UI went, the data source stayed. Row names are needed for the
  // picker; the matrix is the one endpoint that has every kind's catalog with
  // names in a single fetch.
  interface MatrixRowT {
    kind: string;
    id: string;
    name: string;
    scope?: string;
    agencyIds?: string[];
  }
  const { data, error, loading } = useGet<Detail>(
    () => api.GET("/agencies/{agencyId}", { params: { path: { agencyId } } }),
    [agencyId],
  );
  const { data: matrixData } = useGet<{ rows?: MatrixRowT[] }>(
    () => (canEdit ? api.GET("/agency-matrix") : Promise.resolve({ data: { rows: [] } } as never)),
    [agencyId, canEdit],
  );
  // Server truth after a save — the endpoint returns the resulting AgencyDetail,
  // so the panel re-renders from the response without a second fetch.
  const [saved, setSaved] = useState<Detail | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  if (loading) return <InlineLoading />;
  if (error) return <div style={{ fontSize: c.fontSm, color: c.danger }}>Could not load this agency: {error}</div>;

  const detail = saved ?? data;
  const members = detail?.members ?? [];
  const online = detail?.onlineRunners ?? 0;
  const queued = detail?.queuedRuns ?? 0;
  const groups: { kind: string; label: string }[] = [
    { kind: "scope", label: "Scopes" },
    { kind: "secret", label: "Secrets" },
    { kind: "env-var", label: "Variables" },
    { kind: "ssh-credential", label: "SSH Keys" },
    { kind: "runner", label: "Runners" },
  ];

  const save = async (desired: { kind: string; id: string }[], busyKey: string) => {
    setBusy(busyKey);
    setErr(null);
    type MemberRef = { kind: "scope" | "secret" | "env-var" | "ssh-credential" | "runner"; id: string };
    const { data: next, error: e } = await api.PUT("/agencies/{agencyId}/members", {
      params: { path: { agencyId }, header: csrfHeader },
      body: { members: desired as MemberRef[] },
    });
    setBusy(null);
    if (e) {
      setErr(errMsg(e));
      return;
    }
    setSaved(next as Detail);
    // The parent's runner-count column derives from membership; nudge it.
    onMembersChanged?.();
  };

  const remove = (m: Member) =>
    save(
      members.filter((x) => !(x.kind === m.kind && x.id === m.id)).map((x) => ({ kind: x.kind, id: x.id })),
      `${m.kind}-${m.id}`,
    );
  const add = (kind: string, id: string) =>
    save(
      [...members.map((x) => ({ kind: x.kind, id: x.id })), { kind, id }],
      `add-${kind}`,
    );

  const candidates = (kind: string): MatrixRowT[] => {
    const mine = new Set(members.filter((m) => m.kind === kind).map((m) => m.id));
    return (matrixData?.rows ?? []).filter((r) => r.kind === kind && !mine.has(r.id));
  };

  return (
    // EP-4 Shape A — the agency detail plus the runner-agency matrix behind it.
    <DetailPanel style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      {err && (
        <Notice kind="error" onDismiss={() => setErr(null)}>
          {err}
        </Notice>
      )}
      {online === 0 && (
        <div
          style={{
            fontSize: c.fontSm,
            color: c.text,
            background: c.warningBg,
            border: `1px solid ${c.warning}55`,
            borderRadius: c.radiusSurface,
            padding: "8px 12px",
            lineHeight: 1.55,
          }}
        >
          ⚠ <strong>No online runner serves this agency.</strong> Every run whose scope belongs here stays{" "}
          <em>queued indefinitely</em> — it is not failed, so nothing alerts, and the only other place this shows
          is one run's status line at a time.{" "}
          {queued > 0 ? (
            <>
              <strong>
                {queued} run{queued === 1 ? " is" : "s are"} waiting right now.
              </strong>
            </>
          ) : (
            <>Nothing is waiting yet.</>
          )}{" "}
          Bring a runner online in this agency, or move the scope out of it.
        </div>
      )}
      {online > 0 && (
        <div style={{ fontSize: c.fontSm, color: c.textSec }}>
          <strong style={{ color: c.text }}>{online}</strong> online runner{online === 1 ? "" : "s"} serve this
          agency
          {queued > 0 ? `, with ${queued} run${queued === 1 ? "" : "s"} queued.` : "; nothing is waiting."}
        </div>
      )}

      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(220px, 1fr))", gap: 12 }}>
        {groups.map((g) => {
          const mine = members.filter((m) => m.kind === g.kind);
          const avail = canEdit ? candidates(g.kind) : [];
          return (
            <div key={g.kind}>
              <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 700, color: c.textMuted, textTransform: "uppercase", letterSpacing: 0.7, marginBottom: 6 }}>
                {g.label} ({mine.length})
              </div>
              {mine.length === 0 && !canEdit && <div style={{ fontSize: c.fontSm, color: c.textMuted }}>—</div>}
              {mine.length > 0 && (
                <div style={{ display: "flex", flexWrap: "wrap", gap: 5, marginBottom: canEdit ? 6 : 0 }}>
                  {mine.map((m) => (
                    <span
                      key={`${m.kind}-${m.id}`}
                      title={m.scope ? `scope ${m.scope}` : m.status ? `status ${m.status}` : undefined}
                      style={{
                        padding: "2px 8px",
                        borderRadius: c.radiusChip,
                        fontSize: c.fontXs,
                        fontFamily: c.mono,
                        background: c.panel2,
                        border: `1px solid ${c.border}`,
                        color: m.status && m.status !== "online" ? c.textMuted : c.text,
                        display: "inline-flex",
                        alignItems: "center",
                        gap: 5,
                      }}
                    >
                      {m.name}
                      {m.scope && <span style={{ color: c.textSec }}>@{m.scope}</span>}
                      {canEdit && (
                        <button
                          onClick={() => remove(m)}
                          disabled={busy != null}
                          aria-label={`Remove ${m.name} from this agency`}
                          title={`Remove ${m.name} from this agency`}
                          style={{
                            border: "none",
                            background: "transparent",
                            color: busy === `${m.kind}-${m.id}` ? c.textMuted : c.textSec,
                            cursor: busy != null ? "default" : "pointer",
                            padding: 0,
                            fontSize: c.fontXs,
                            lineHeight: 1,
                          }}
                        >
                          ✕
                        </button>
                      )}
                    </span>
                  ))}
                </div>
              )}
              {canEdit && (
                <select
                  value=""
                  disabled={busy != null || avail.length === 0}
                  aria-label={`Add a ${g.label.replace(/s$/, "").toLowerCase()} to this agency`}
                  onChange={(e) => {
                    if (e.target.value) add(g.kind, e.target.value);
                  }}
                  style={{
                    ...inputStyle(),
                    width: "100%",
                    fontSize: c.fontXs,
                    padding: "3px 6px",
                    cursor: avail.length === 0 ? "default" : "pointer",
                    color: c.textSec,
                  }}
                >
                  <option value="">{avail.length === 0 ? "Nothing to add" : "+ Add…"}</option>
                  {avail.map((r) => (
                    <option key={r.id} value={r.id}>
                      {r.name}
                      {r.scope ? ` @${r.scope}` : ""}
                    </option>
                  ))}
                </select>
              )}
            </div>
          );
        })}
      </div>
      <div style={{ fontSize: c.fontXs, color: c.textMuted }}>
        A scope, secret, variable or key that appears in NO agency is unrestricted, not orphaned — it stays
        reachable from everywhere. Removing an entity from the agency that <em>owns</em> it is refused; transfer
        ownership first.
      </div>
    </DetailPanel>
  );
}

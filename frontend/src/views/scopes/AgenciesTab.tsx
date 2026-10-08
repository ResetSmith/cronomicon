import { Fragment, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../../api/client";
import { GLOBAL_AGENCY } from "../../api/access";
import { globalOnly } from "../../api/globalAdmin";
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
// `canEdit` is configureApp SOMEWHERE and decides whether the mutation controls
// exist. `globalAdmin` (GC, v2.2.2) is configureApp on every agency, which is
// what the agency CATALOG now needs — an agency is the unit access is divided
// by, so creating, renaming or deleting one, and moving a SCOPE into or out of
// one, is not something an administrator of a single agency may do. Those
// controls stay, disabled with the reason. Secret, variable, key and runner
// membership is unchanged: still judged per agency by the server.
export function AgenciesTab({
  canEdit,
  globalAdmin,
  envGlobal = globalAdmin,
}: {
  canEdit: boolean;
  /** A global administrator for configureApp: the catalog, Vault paths, and taking a scope or key out of Global. */
  globalAdmin: boolean;
  /** A global administrator for manageEnvVars: taking a secret or variable out of Global. */
  envGlobal?: boolean;
}) {
  const globalWhy = globalOnly(globalAdmin);
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
        An agency is a department&rsquo;s isolation zone. Every scope, secret, variable, SSH key, host record and
        runner belongs to exactly one, and <strong>Global</strong> holds what belongs to no department: Global&rsquo;s
        rows are every agency&rsquo;s to use and a global administrator&rsquo;s to change. A run goes only to a runner
        that serves its scope&rsquo;s agency, and receives only that agency&rsquo;s secrets and keys, or Global&rsquo;s.{" "}
        <strong>Expand a row to see and edit what it contains.</strong> To go the other way — which agency one secret,
        variable, key, scope or runner belongs to — that row&rsquo;s own catalog carries an Agency column.{" "}
        <DocLink href={DOC_LINKS.agencies}>How membership and ownership work</DocLink>
      </div>
      <div style={{ display: "flex", gap: 8, marginBottom: 16 }}>
        <div style={{ flex: 1 }}>
          <SearchBar value={search} onChange={setSearch} placeholder="Search agencies by name or description..." />
        </div>
        {canEdit && (
          <Btn primary onClick={() => setAdding(true)} disabled={!!globalWhy} title={globalWhy || undefined}>
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
                  <Btn small onClick={() => setAdding(true)} disabled={!!globalWhy} title={globalWhy || undefined}>
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
                          <Btn onClick={() => setEditing(a)} disabled={!!globalWhy} title={globalWhy || undefined}>Edit</Btn>
                          <Btn danger onClick={() => setDeleting(a)} disabled={!!globalWhy} title={globalWhy || undefined}>
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
                        <AgencyDetailPanel agencyId={a.id} canEdit={canEdit} globalAdmin={globalAdmin} envGlobal={envGlobal} onMembersChanged={() => setDep((n) => n + 1)} />
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
function AgencyDetailPanel({ agencyId, canEdit, globalAdmin, envGlobal, onMembersChanged }: {
  agencyId: string;
  canEdit: boolean;
  envGlobal: boolean;
  /** A global administrator assigns an agency its Vault paths (LR-80). Moving a
   *  scope is no longer theirs alone: since v2.3.0 it follows the same rule as
   *  a secret or a key (authority over both agencies), which the server checks. */
  globalAdmin: boolean;
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

  // What can be ADDED here is what is Global's (one agency per row since
  // v2.3.0): the add takes it out of Global. A row that is another agency's is
  // moved on the row itself, not from this list, and would be refused.
  const candidates = (kind: string): MatrixRowT[] => {
    const mine = new Set(members.filter((m) => m.kind === kind).map((m) => m.id));
    return (matrixData?.rows ?? []).filter(
      (r) => r.kind === kind && !mine.has(r.id) && (r.agencyIds ?? []).every((a) => a === GLOBAL_AGENCY),
    );
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
          <Link to="/runners" style={{ color: c.primary }}>
            Enrol a runner for this agency
          </Link>{" "}
          (Runners → Add Runner, with this agency as its owner), or bring one of its runners back online.
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
          // Runners are not members to add or remove here (v2.3.0, MA-11): an
          // agent serves the agency whose token it enrolled with. The list is
          // shown, and the Runners page is where one is enrolled or removed.
          const readOnly = g.kind === "runner";
          // ADDING here takes one of Global's rows out of Global, and Global is
          // a global administrator's on either side of a move (requireMove): for
          // anyone else the add is always refused, so it is disabled with why.
          const kindGlobal = g.kind === "secret" || g.kind === "env-var" ? envGlobal : globalAdmin;
          const why = kindGlobal
            ? ""
            : "Adding one of Global's here takes it out of Global — only a global administrator (a role on every agency) can do that.";
          // REMOVING is possible only for a row still in several agencies (from
          // before 2.3.0): a row in one agency cannot be left in none, and one
          // this agency owns is moved, not removed. So the control exists only
          // where there is something it can do.
          const removable = (m: Member) =>
            ((matrixData?.rows ?? []).find((r) => r.kind === m.kind && r.id === m.id)?.agencyIds ?? []).length > 1;
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
                      {canEdit && !readOnly && removable(m) && (
                        <button
                          onClick={() => remove(m)}
                          disabled={busy != null}
                          aria-label={`Remove ${m.name} from this agency`}
                          title={`${m.name} is still in several agencies. Remove it from this one.`}
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
              {readOnly && (
                <div style={{ fontSize: c.fontXs, color: c.textMuted }}>
                  The runners that serve this agency.{" "}
                  <Link to="/runners" style={{ color: c.primary }}>
                    Enrol or remove one on Runners
                  </Link>
                  .
                </div>
              )}
              {canEdit && !readOnly && (
                <select
                  value=""
                  disabled={busy != null || avail.length === 0 || !!why}
                  title={why || undefined}
                  aria-label={`Add a ${g.label.replace(/s$/, "").toLowerCase()} to this agency`}
                  onChange={(e) => {
                    if (e.target.value) add(g.kind, e.target.value);
                  }}
                  style={{
                    ...inputStyle(),
                    width: "100%",
                    fontSize: c.fontXs,
                    padding: "3px 6px",
                    cursor: why ? "not-allowed" : avail.length === 0 ? "default" : "pointer",
                    color: c.textSec,
                  }}
                >
                  <option value="">{avail.length === 0 ? "Nothing of Global's to add" : "+ Add one of Global's…"}</option>
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
        A scope, secret, variable and key belongs to exactly one agency. A global administrator can add one of Global's
        here, which makes this agency its owner. To move one between agencies, or back to Global, change its agency on
        the row itself: the Agency select on a scope, and Move to another agency on a secret, variable or SSH key.
      </div>
      {agencyId !== GLOBAL_AGENCY && <VaultPathsEditor agencyId={agencyId} globalAdmin={globalAdmin} />}
    </DetailPanel>
  );
}

// VaultPathsEditor — the Vault path prefixes assigned to one agency (LR-80).
//
// The installation has one Vault connection. A global administrator divides it
// here: an agency's administrators may then name, for the secrets and keys
// their agency owns, any path inside one of these prefixes and none outside.
// An agency with no prefix can hold no Vault-backed row. Global has no list:
// its rows are a global administrator's and have no path limit.
//
// Read by whoever writes Vault-backed rows for the agency; edited by a global
// administrator only. For anyone else the list is absent (the read is refused),
// and that is irrelevance, not a precondition: nothing is shown.
function VaultPathsEditor({ agencyId, globalAdmin }: { agencyId: string; globalAdmin: boolean }) {
  const [dep, setDep] = useState(0);
  const q = useGet<{ prefixes?: string[] }>(
    () => api.GET("/agencies/{agencyId}/vault-prefixes", { params: { path: { agencyId } } }),
    [agencyId, dep],
  );
  const [draft, setDraft] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const prefixes = q.data?.prefixes ?? [];
  useEffect(() => {
    setDraft(null);
    setErr(null);
  }, [agencyId]);
  if (q.loading) return <InlineLoading what="Vault paths" />;
  if (q.error) return null;
  const why = globalOnly(globalAdmin, "Only a global administrator (a role on every agency) can assign an agency its Vault paths.");

  const save = async () => {
    setBusy(true);
    setErr(null);
    const next = (draft ?? "")
      .split("\n")
      .map((l) => l.trim())
      .filter(Boolean);
    const { error } = await api.PUT("/agencies/{agencyId}/vault-prefixes", {
      params: { path: { agencyId }, header: csrfHeader },
      body: { prefixes: next },
    });
    setBusy(false);
    if (error) {
      setErr(errMsg(error));
      return;
    }
    setDraft(null);
    setDep((n) => n + 1);
  };

  return (
    <div style={{ borderTop: `1px solid ${c.borderLight}`, paddingTop: 10 }}>
      <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 700, color: c.textMuted, textTransform: "uppercase", letterSpacing: 0.7, marginBottom: 6 }}>
        Vault paths ({prefixes.length})
      </div>
      {draft === null ? (
        <>
          {prefixes.length === 0 ? (
            <div style={{ fontSize: c.fontSm, color: c.textSec }}>
              None assigned. This agency cannot create a Vault-backed secret or key, and any it already owns keep working but
              cannot be edited until a path that covers them is assigned.
            </div>
          ) : (
            <div style={{ display: "flex", flexWrap: "wrap", gap: 5 }}>
              {prefixes.map((p) => (
                <code key={p} style={{ fontFamily: c.mono, fontSize: c.fontXs, padding: "2px 8px", borderRadius: c.radiusChip, background: c.panel2, border: `1px solid ${c.border}`, color: c.text }}>
                  {p}
                </code>
              ))}
            </div>
          )}
          <div style={{ display: "flex", alignItems: "center", gap: 10, marginTop: 8, flexWrap: "wrap" }}>
            <Btn small onClick={() => setDraft(prefixes.join("\n"))} disabled={!!why} title={why || undefined}>
              Edit Vault paths
            </Btn>
            <span style={{ fontSize: c.fontXs, color: c.textMuted }}>
              {why ||
                "The agency's administrators may name any path inside one of these, for the secrets and keys the agency owns."}
            </span>
          </div>
        </>
      ) : (
        <>
          <label style={{ ...labelStyle(), display: "block" }}>
            One path prefix per line
            <textarea
              aria-label="Vault path prefixes, one per line"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              rows={Math.max(3, draft.split("\n").length + 1)}
              spellCheck={false}
              placeholder={"secret/data/tax\nkv/tax"}
              style={{ ...inputStyle(), fontFamily: c.mono, fontSize: c.fontSm, marginTop: 4, resize: "vertical" }}
            />
          </label>
          <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4, lineHeight: 1.5 }}>
            A prefix covers the paths whose first segments are its own: <code style={{ fontFamily: c.mono }}>secret/data/tax</code>{" "}
            covers <code style={{ fontFamily: c.mono }}>secret/data/tax/db</code> and not{" "}
            <code style={{ fontFamily: c.mono }}>secret/data/tax-audit/db</code>. Paths are case-sensitive. Saving replaces the list;
            an empty list means this agency can name no Vault path.
          </div>
          {err && (
            <div role="alert" style={{ fontSize: c.fontSm, color: c.danger, marginTop: 6 }}>
              {err}
            </div>
          )}
          <div style={{ display: "flex", gap: 8, marginTop: 8 }}>
            <Btn small primary onClick={save} disabled={busy}>
              {busy ? "Saving…" : "Save Vault paths"}
            </Btn>
            <Btn small onClick={() => setDraft(null)} disabled={busy}>
              Cancel
            </Btn>
          </div>
        </>
      )}
    </div>
  );
}

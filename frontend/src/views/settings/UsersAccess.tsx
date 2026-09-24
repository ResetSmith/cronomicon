import { useEffect, useState } from "react";
import { api, fetchCapabilities } from "../../api/client";
import type { components } from "../../api/schema";
import { useGet, rows, useColumnWidths, useTableSort } from "../../hooks";
import { ColumnsMenu, TableHead, renderCells, useTableColumns } from "../../components/table";
import { type SortColumn } from "../../utils/sort";
import { c } from "../../theme";
import { DOC_LINKS } from "../../components/docLinks";
import { AlertBanner, ConfirmDialog, SearchBar, SkeletonRows, SortableLabel, DocLink } from "../../components/ui";
import { Btn, Card, csrfHeader, errMsg, fmtDateTime, inputStyle, tdStyle, thStyle } from "./ui";

type RecentLogin = components["schemas"]["RecentLogin"];
type Role = components["schemas"]["Role"];

type RoleInput = components["schemas"]["RoleInput"];
type AccessGrant = components["schemas"]["AccessGrant"];
type Agency = components["schemas"]["Agency"];
type RolePermissions = components["schemas"]["RolePermissions"];

// Roles are DATA as of v0.56.1 (RB-6/RB-7) — a table, not a compiled-in list — so
// every role-consuming control fetches GET /roles. The hardcoded
// ["Admin","Approver","Operator","Viewer"] that used to live here was pinned to a
// frozen OpenAPI enum, so a custom role could not appear in the mapping dropdown
// even though the page was already fetching /roles elsewhere — two disagreeing
// sources of truth for the same question.
const useRoles = (bump?: number) => {
  const q = useGet<unknown>(() => api.GET("/roles"), [bump]);
  return { roles: rows<Role>(q.data), loading: q.loading, error: q.error };
};

// The seven permissions, in wire order, with the labels the editor renders. Kept
// beside the type so adding one to the contract surfaces here as a missing entry
// rather than silently disappearing from the UI.
const PERMISSION_LABELS: { key: keyof RolePermissions; label: string; hint: string }[] = [
  { key: "triggerJobs", label: "Trigger jobs", hint: "Run jobs and workflows, within the scopes this role is granted." },
  { key: "killJobs", label: "Kill jobs", hint: "Stop, pause and resume runs, within the scopes this role is granted." },
  { key: "manageEnvVars", label: "Manage variables & secrets", hint: "Create, edit and reveal variables and secrets." },
  { key: "publishSchedule", label: "Publish to GitLab", hint: "Publish job and schedule definitions to the shared repository." },
  { key: "configureApp", label: "Configure application", hint: "Settings, integrations, scopes, runners and SSH configuration." },
  { key: "manageRoles", label: "Manage roles & access", hint: "This screen: roles and access grants." },
  // AF-2. The hint states BOTH halves of the rule, because the permission is
  // narrower than its name suggests: it is agency-bound, and it stops at the
  // shared objects (schedules, calendars, the recycle bin) that stay admin-only.
  {
    key: "compose",
    label: "Create jobs & workflows",
    hint:
      "Author jobs and workflows within the agencies this role is granted. Global (All) definitions, " +
      "reusable schedules, calendars and the recycle bin stay admin-only.",
  },
];

const EMPTY_PERMS: RolePermissions = {
  triggerJobs: false,
  killJobs: false,
  manageEnvVars: false,
  publishSchedule: false,
  configureApp: false,
  manageRoles: false,
  compose: false,
};

// Default column widths (px) for table-layout:fixed before the user drags
// (V1.1-7). Stored overrides come from useColumnWidths.
const RECENT_LOGINS_COL_W: Record<string, number> = {
  user: 220,
  adGroups: 200,
  grants: 260,
  firstSeen: 150,
  lastSeen: 150,
};

// LB11: cap the AD-group pills shown per row so a user in many groups doesn't
// blow up the row height; the rest collapse behind a "+N more" toggle.
const AD_GROUPS_COLLAPSED = 3;

// RB-21 — one (role, where) pair per grant. This replaced a single "Resolved
// Role" chip in v0.57.8: under multi-grant that chip was false, not merely terse.
// "operator" over a user who is operator on Finance and viewer on Tax reads as
// unrestricted operator, on the one screen an auditor uses to answer exactly that.
type LoginGrant = NonNullable<RecentLogin["grants"]>[number];

const grantWhere = (g: LoginGrant): string =>
  g.allScopes ? "All scopes" : (g.agencyName ?? "—");

// Sorts on the rendered summary rather than a hidden precedence rank: the column
// shows a list, and sorting a list by something the reader cannot see is worse
// than not sorting it. Users with no grants sort last under asc — they are the
// exceptions worth finding, and an empty string would bury them at the top.
const grantSortKey = (u: RecentLogin): string =>
  (u.grants ?? []).map((g) => `${g.role}@${grantWhere(g)}`).join(", ") || "\uffff";

// Sortable columns (the sorting-update plan §3.2). AD Groups is multi-value,
// so it stays unsortable; User sorts by the displayed value (name, else email).
const LOGIN_SORT_COLS: SortColumn<RecentLogin>[] = [
  { key: "user", get: (u) => u.name || u.email, type: "text" },
  { key: "grants", get: grantSortKey, type: "text" },
  { key: "firstSeen", get: (u) => u.firstSeenAt, type: "date" },
  { key: "lastSeen", get: (u) => u.lastSeenAt, type: "date" },
];

// RF-18 — the grants table is the one surface that decides access, and it was the
// only one on this page with no sort, no search and no filters while both its
// neighbours had all three. It also grows fastest: ~one row per group per
// department, so the matrix it replaced traded a 4,000px-wide grid for a list
// nobody could find anything in.
//
// "Where" sorts on the rendered text so All-scopes grants group together rather
// than scattering by a null agency id — the reading order matches the screen.
const GRANT_SORT_COLS: SortColumn<AccessGrant>[] = [
  { key: "adGroup", get: (g) => g.adGroup, type: "text" },
  { key: "role", get: (g) => g.role, type: "text" },
  { key: "where", get: (g) => (g.allScopes ? "All scopes" : g.agencyName || g.agencyId || ""), type: "text" },
];

// The Honest View (A3.2) — users appear only after their first successful OIDC
// round-trip; there is no directory enumeration in v1. Read-only.
export function UsersAccessSection() {
  const { data, error, loading } = useGet<unknown>(() => api.GET("/recent-logins"));
  const items = rows<RecentLogin>(data);
  const cw = useColumnWidths("recent-logins");
  const sort = useTableSort(items, LOGIN_SORT_COLS, { key: "user", dir: "asc" }, { tableId: "recent-logins" });
  // LB11: per-row expand state for the AD-groups "+N more" toggle (Set so several
  // rows can be open at once on this read-only browse table).
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const toggle = (rowKey: string) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      next.has(rowKey) ? next.delete(rowKey) : next.add(rowKey);
      return next;
    });
  // LB15: session-only dismissal of the Honest View notice (mirrors the LB5 pattern).
  const [noteDismissed, setNoteDismissed] = useState(false);

  // CO-4 — the column spec, built in render (it closes over `expanded`/`toggle`
  // and reads `c.*`). The row carries `rowKey` alongside the login because the
  // AD-groups cell needs a stable per-row identity for its expand state, and the
  // fallback for a login with no email is its INDEX — which a cell cannot see.
  const cols = useTableColumns<{ u: RecentLogin; rowKey: string }>("recent-logins", [
    {
      key: "user",
      label: "User",
      sortKey: "user",
      width: RECENT_LOGINS_COL_W.user,
      pin: "first",
      cell: ({ u }) => (
        <>
          <div style={{ fontWeight: 600 }}>{u.name || u.email || "—"}</div>
          {u.name && u.email && <div style={{ fontSize: c.fontSm, color: c.textSec, fontFamily: c.mono }}>{u.email}</div>}
        </>
      ),
    },
    {
      key: "adGroups",
      label: "AD Groups",
      width: RECENT_LOGINS_COL_W.adGroups,
      cell: ({ u, rowKey }) => {
        const groups = u.adGroups ?? [];
        const isExp = expanded.has(rowKey);
        const shown = isExp ? groups : groups.slice(0, AD_GROUPS_COLLAPSED);
        const hiddenCount = groups.length - shown.length;
        return (
          <div style={{ display: "flex", gap: 4, flexWrap: "wrap", alignItems: "center" }}>
            {shown.map((g) => (
              <span
                key={g}
                style={{
                  padding: "2px 8px",
                  borderRadius: c.radiusChip,
                  fontSize: c.fontXs,
                  fontWeight: 600,
                  background: c.panel2,
                  color: c.textSec,
                  border: `1px solid ${c.border}`,
                  fontFamily: c.mono,
                }}
              >
                {g}
              </span>
            ))}
            {groups.length > AD_GROUPS_COLLAPSED && (
              <span
                role="button"
                tabIndex={0}
                onClick={() => toggle(rowKey)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" || e.key === " ") {
                    e.preventDefault();
                    toggle(rowKey);
                  }
                }}
                style={{
                  padding: "2px 8px",
                  borderRadius: c.radiusChip,
                  fontSize: c.fontXs,
                  fontWeight: 600,
                  background: "transparent",
                  color: c.primary,
                  border: `1px solid ${c.primary}40`,
                  cursor: "pointer",
                  fontFamily: c.mono,
                }}
              >
                {isExp ? "Show less" : `+${hiddenCount} more`}
              </span>
            )}
            {groups.length === 0 && <span style={{ color: c.textSec }}>—</span>}
          </div>
        );
      },
    },
    {
      key: "grants",
      label: "Access",
      sortKey: "grants",
      width: RECENT_LOGINS_COL_W.grants,
      cell: ({ u }) =>
        (u.grants ?? []).length === 0 ? (
          // Says the thing rather than showing a dash. Fail-closed means this
          // person can do NOTHING, which is a real finding — usually a grant
          // nobody wrote — and "—" reads as missing data instead.
          <span style={{ color: c.textSec, fontSize: c.fontSm, fontStyle: "italic" }}>No access</span>
        ) : (
          <div style={{ display: "flex", flexWrap: "wrap", gap: 4 }}>
            {(u.grants ?? []).map((g, gi) => (
              <span
                key={`${g.role}-${g.agencyName ?? "*"}-${gi}`}
                title={g.allScopes ? `${g.role} everywhere — unrestricted` : `${g.role} on ${grantWhere(g)} only`}
                style={{
                  padding: "2px 9px",
                  // A role, not a status — chip shape (B-3/VU-17).
                  borderRadius: c.radiusChip,
                  fontSize: c.fontXs,
                  fontWeight: 600,
                  whiteSpace: "nowrap",
                  // Unrestricted grants are styled apart because they are the
                  // rows worth auditing first; a uniform chip hides the
                  // difference that matters most.
                  background: g.allScopes ? `${c.warning}24` : `${c.primary}24`,
                  color: g.allScopes ? c.warning : c.primary,
                }}
              >
                {g.role}
                <span style={{ opacity: 0.7, fontWeight: 500 }}> · {grantWhere(g)}</span>
              </span>
            ))}
          </div>
        ),
    },
    {
      key: "firstSeen",
      label: "First Seen",
      sortKey: "firstSeen",
      width: RECENT_LOGINS_COL_W.firstSeen,
      fixed: true,
      tdStyle: { color: c.textSec, fontSize: c.fontSm, whiteSpace: "nowrap" },
      cell: ({ u }) => fmtDateTime(u.firstSeenAt),
    },
    {
      key: "lastSeen",
      label: "Last Seen",
      sortKey: "lastSeen",
      width: RECENT_LOGINS_COL_W.lastSeen,
      fixed: true,
      tdStyle: { color: c.textSec, fontSize: c.fontSm, whiteSpace: "nowrap" },
      cell: ({ u }) => fmtDateTime(u.lastSeenAt),
    },
  ]);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      {!noteDismissed && (
        <AlertBanner type="warning" onDismiss={() => setNoteDismissed(true)}>
          <strong>The Honest View</strong> — this list shows only users who have actually signed in via OIDC; Cronomicon does
          not enumerate the directory. <strong>Access</strong> shows each person's grants as they resolve at login — the
          role <em>and</em> the department it applies to, because one without the other is not an answer.
        </AlertBanner>
      )}

      <Card title="Recent Logins" noPad>
        {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
        {error && <div style={{ color: c.danger, padding: 16 }}>Error: {error}</div>}
        {!loading && !error && items.length === 0 && (
          <div style={{ color: c.textSec, padding: 16 }}>No logins recorded yet. Users appear after their first successful sign-in.</div>
        )}
        {!loading && !error && items.length > 0 && (
          <div style={{ overflowX: "auto" }}>
            {/* CO-4 — this replaces a hand-rolled "Reset columns" button that
                appeared only once a width had been dragged, and reset widths
                alone. ColumnsMenu is always present, arranges as well as resets,
                and clears both preferences together. */}
            <div style={{ display: "flex", justifyContent: "flex-end", padding: "8px 16px 0" }}>
              <ColumnsMenu cols={cols} cw={cw} />
            </div>
            <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm, tableLayout: "fixed" }}>
              <thead>
                <TableHead columns={cols.visible} sort={sort} cw={cw} />
              </thead>
              <tbody>
                {sort.sorted.map((u, i) => (
                  <tr key={u.email ?? i} style={{ borderBottom: `1px solid ${c.border}` }}>
                    {renderCells(cols.visible, { u, rowKey: u.email ?? String(i) })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      <RolesCard />
      <AccessGrantsCard />
    </div>
  );
}


// AccessGrantsCard — RB-20. THE authorization surface as of v0.56.5.
//
// A grant is `AD group → role → where`, and `where` is an agency or "all scopes".
// It replaces the two-axis model below, whose independently-unioned halves produced
// the cross-product leak: a user who was a viewer on Tax and an operator on Finance
// resolved to {trigger} over {tax, finance} and could therefore trigger TAX jobs.
// A grant keeps the association those two flat lists destroy — THIS role, HERE.
//
// It is a LIST, not a matrix, deliberately: the role × scope grid it replaces grows
// horizontally with every scope, and at two dozen departments becomes a
// 4,000px-wide wall. Grants grow vertically and scroll.
function AccessGrantsCard() {
  const [bump, setBump] = useState(0);
  const grantsQ = useGet<unknown>(() => api.GET("/access-grants"), [bump]);
  const agenciesQ = useGet<unknown>(() => api.GET("/agencies"));
  const { roles } = useRoles();
  const grants = rows<AccessGrant>(grantsQ.data);
  const agencies = rows<Agency>(agenciesQ.data);

  const [adGroup, setAdGroup] = useState("");
  const [role, setRole] = useState("");
  const [where, setWhere] = useState(""); // agency id, or "*" for all scopes
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<AccessGrant | null>(null);
  // AF-3 — only an unrestricted administrator may grant "everywhere".
  const [canGrantEverywhere, setCanGrantEverywhere] = useState(false);
  useEffect(() => {
    fetchCapabilities().then((caps) => setCanGrantEverywhere(caps.unrestricted));
  }, []);

  // RF-18 — find/filter/sort. Search matches the group AND the where, because
  // "who can touch Tax?" and "what does SG-Ops hold?" are the two questions this
  // table exists to answer.
  const [search, setSearch] = useState("");
  const [roleFilter, setRoleFilter] = useState("");
  const [agencyFilter, setAgencyFilter] = useState("");
  const visible = grants.filter((g) => {
    const q = search.trim().toLowerCase();
    const whereText = g.allScopes ? "all scopes" : (g.agencyName || g.agencyId || "").toLowerCase();
    if (q && !(g.adGroup ?? "").toLowerCase().includes(q) && !whereText.includes(q)) return false;
    if (roleFilter && g.role !== roleFilter) return false;
    // "*" filters to the unrestricted grants — the ones worth auditing first,
    // since they are the only rows that reach every department.
    if (agencyFilter === "*" && !g.allScopes) return false;
    if (agencyFilter && agencyFilter !== "*" && g.agencyId !== agencyFilter) return false;
    return true;
  });
  const gsort = useTableSort(visible, GRANT_SORT_COLS, { key: "adGroup", dir: "asc" }, { tableId: "access-grants" });

  useEffect(() => {
    if (!role && roles.length > 0) setRole(roles.find((r) => r.name === "operator")?.name ?? roles[0].name ?? "");
  }, [roles, role]);

  async function add() {
    if (!adGroup.trim() || !role || !where) return;
    setBusy(true);
    setErr(null);
    const body = where === "*"
      ? { adGroup: adGroup.trim(), role, allScopes: true }
      : { adGroup: adGroup.trim(), role, agencyId: where };
    const { error: e } = await api.POST("/access-grants", { params: { header: csrfHeader }, body });
    setBusy(false);
    if (e) setErr(errMsg(e));
    else {
      setAdGroup("");
      setBump((b) => b + 1);
    }
  }

  async function remove(g: AccessGrant) {
    setBusy(true);
    setErr(null);
    const { error: e } = await api.DELETE("/access-grants/{grantId}", {
      params: { path: { grantId: g.id ?? "" }, header: csrfHeader },
    });
    setBusy(false);
    setDeleting(null);
    if (e) setErr(errMsg(e));
    else setBump((b) => b + 1);
  }

  return (
    <Card title="Access Grants" noPad>
      <div style={{ padding: 16, borderBottom: `1px solid ${c.border}`, color: c.textSec, fontSize: c.fontSm }}>
        Who may do what, <strong>and where</strong>. A grant pairs an AD group with a role and the
        agency it applies to, so "Operator, but only for Tax" is one row. This is the only surface
        that decides access. <DocLink href={DOC_LINKS.usersAccess}>How access resolves</DocLink>
      </div>

      <div style={{ display: "flex", gap: 8, alignItems: "center", padding: 16, borderBottom: `1px solid ${c.border}`, flexWrap: "wrap" }}>
        <input value={adGroup} onChange={(e) => setAdGroup(e.target.value)} placeholder="SG-Cronomicon-Tax" style={{ ...inputStyle(), flex: 1, minWidth: 200 }} />
        <select value={role} onChange={(e) => setRole(e.target.value)} style={{ ...inputStyle(), width: 150, cursor: "pointer" }}>
          {roles.map((r) => <option key={r.name} value={r.name ?? ""}>{r.name}</option>)}
        </select>
        <select value={where} onChange={(e) => setWhere(e.target.value)} style={{ ...inputStyle(), width: 190, cursor: "pointer" }}>
          <option value="">Where…</option>
          {/* AF-3 — "everywhere" is not a departmental administrator's to give,
              so it is not offered to one (the server refuses it either way). The
              named agencies stay listed for everyone: a delegate picking one
              outside their own gets a 403 that says which rule they hit, and
              filtering the list here would need the server to publish the
              caller's agency set for what a clear refusal already handles. */}
          {canGrantEverywhere && <option value="*">All scopes (unrestricted)</option>}
          {agencies.map((a) => <option key={a.id} value={a.id ?? ""}>{a.name}</option>)}
        </select>
        <Btn primary onClick={add} disabled={busy || !adGroup.trim() || !role || !where}>+ Add</Btn>
      </div>

      {err && <div style={{ padding: 16 }}><AlertBanner type="danger">{err}</AlertBanner></div>}

      {/* RF-18 — find/filter, shown only once the list is long enough to need it:
          on a single-department install the controls would be noise. */}
      {grants.length > 5 && (
        <div style={{ display: "flex", gap: 8, alignItems: "center", padding: "12px 16px", borderBottom: `1px solid ${c.border}`, flexWrap: "wrap" }}>
          <SearchBar value={search} onChange={setSearch} placeholder="Search group or where…" style={{ flex: 1, minWidth: 200 }} />
          <select value={roleFilter} onChange={(e) => setRoleFilter(e.target.value)} style={{ ...inputStyle(), width: 150, cursor: "pointer" }}>
            <option value="">All roles</option>
            {roles.map((r) => <option key={r.name} value={r.name ?? ""}>{r.name}</option>)}
          </select>
          <select value={agencyFilter} onChange={(e) => setAgencyFilter(e.target.value)} style={{ ...inputStyle(), width: 190, cursor: "pointer" }}>
            <option value="">All agencies</option>
            <option value="*">All scopes (unrestricted)</option>
            {agencies.map((a) => <option key={a.id} value={a.id ?? ""}>{a.name}</option>)}
          </select>
          {visible.length !== grants.length && (
            <span style={{ color: c.textSec, fontSize: c.fontSm }}>{visible.length} of {grants.length}</span>
          )}
        </div>
      )}

      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr>
            {([["adGroup", "AD Group"], ["role", "Role"], ["where", "Where"]] as const).map(([key, label]) => (
              <th
                key={key}
                onClick={() => gsort.toggle(key)}
                title={`Sort by ${label}`}
                aria-sort={gsort.ariaSort(key)}
                style={{ ...thStyle(), cursor: "pointer", color: gsort.isActive(key) ? c.text : undefined }}
              >
                <SortableLabel label={label} active={gsort.isActive(key)} dir={gsort.sortDir} />
              </th>
            ))}
            <th style={{ ...thStyle(), width: 120, textAlign: "right" }}>Actions</th>
          </tr>
        </thead>
        <tbody>
          {grantsQ.loading && <SkeletonRows rows={4} cols={4} />}
          {!grantsQ.loading && grants.length > 0 && visible.length === 0 && (
            <tr><td style={tdStyle()} colSpan={4}>No grants match this filter.</td></tr>
          )}
          {!grantsQ.loading && grants.length === 0 && (
            <tr>
              <td style={tdStyle()} colSpan={4}>
                No grants. <strong>Nobody has any access</strong> until one exists — a grant is the
                only thing that confers it.
              </td>
            </tr>
          )}
          {!grantsQ.loading && gsort.sorted.map((g) => (
            <tr key={g.id}>
              <td style={{ ...tdStyle(), fontFamily: c.mono }}>{g.adGroup}</td>
              <td style={tdStyle()}>{g.role}</td>
              <td style={tdStyle()}>
                <span style={{ fontSize: c.fontXs, padding: "2px 8px", borderRadius: c.radiusChip, border: `1px solid ${c.border}`, color: g.allScopes ? c.warning : c.textSec }}>
                  {g.allScopes ? "All scopes" : (g.agencyName || g.agencyId)}
                </span>
              </td>
              <td style={{ ...tdStyle(), textAlign: "right" }}>
                <Btn small danger onClick={() => setDeleting(g)}>Remove</Btn>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {deleting && (
        <ConfirmDialog
          title="Remove this grant?"
          danger
          busy={busy}
          message={
            <>
              Members of <strong style={{ fontFamily: c.mono }}>{deleting.adGroup}</strong> lose{" "}
              <strong>{deleting.role}</strong> on{" "}
              <strong>{deleting.allScopes ? "all scopes" : (deleting.agencyName || deleting.agencyId)}</strong>.
              Anyone whose access came only from this grant keeps none at all.
              <div style={{ marginTop: 10 }}>
                This also <strong>signs out every other operator</strong>.
              </div>
            </>
          }
          confirmLabel={busy ? "Removing…" : "Remove grant"}
          onCancel={() => setDeleting(null)}
          onConfirm={() => remove(deleting)}
        />
      )}
    </Card>
  );
}

// RolesCard — the role catalog (RB-11). Roles are pure PERMISSION TEMPLATES and
// carry no scope: a role says what you may DO, the scope-restriction matrix below
// says where. That separation is why Tax needs zero new roles — two grants against
// the existing operator and viewer — rather than one role per department per level.
function RolesCard() {
  const [bump, setBump] = useState(0);
  const { roles, loading, error } = useRoles(bump);
  // AF-3 — a role TEMPLATE is shared by every agency, so editing one is
  // unrestricted-only (the server refuses otherwise, AF3-D1a). A departmental
  // administrator still reads this list — it is how they know what the roles
  // they grant actually carry — but is not offered writes that would 403.
  const [canEditTemplates, setCanEditTemplates] = useState(false);
  useEffect(() => {
    fetchCapabilities().then((caps) => setCanEditTemplates(caps.unrestricted));
  }, []);
  const [editing, setEditing] = useState<Role | null>(null);
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<Role | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const blank: Role = { name: "", description: "", rank: 1, permissions: { ...EMPTY_PERMS } };

  async function save(draft: Role, isNew: boolean) {
    setBusy(true);
    setErr(null);
    const body: RoleInput = {
      name: draft.name ?? "",
      description: draft.description ?? "",
      rank: draft.rank ?? 0,
      permissions: draft.permissions ?? { ...EMPTY_PERMS },
    };
    const { error: e } = isNew
      ? await api.POST("/roles", { params: { header: csrfHeader }, body })
      : await api.PUT("/roles/{roleName}", { params: { path: { roleName: draft.name ?? "" }, header: csrfHeader }, body });
    setBusy(false);
    if (e) {
      setErr(errMsg(e));
      return;
    }
    setEditing(null);
    setCreating(false);
    setBump((b) => b + 1);
  }

  async function remove(role: Role) {
    setBusy(true);
    setErr(null);
    const { error: e } = await api.DELETE("/roles/{roleName}", { params: { path: { roleName: role.name ?? "" }, header: csrfHeader } });
    setBusy(false);
    setDeleting(null);
    if (e) setErr(errMsg(e));
    else setBump((b) => b + 1);
  }

  const held = (r: Role) => PERMISSION_LABELS.filter((p) => r.permissions?.[p.key]).map((p) => p.label);

  return (
    <Card title="Roles" noPad>
      <div style={{ padding: 16, borderBottom: `1px solid ${c.border}`, display: "flex", alignItems: "center", gap: 12, flexWrap: "wrap" }}>
        <div style={{ color: c.textSec, fontSize: c.fontSm, flex: 1, minWidth: 280 }}>
          What each role may <strong>do</strong>. <em>Where</em> it may do it is an Access Grant below —
          roles are reusable permission templates and carry no scope of their own.
        </div>
        {canEditTemplates ? (
          <Btn primary onClick={() => { setCreating(true); setEditing(blank); }}>+ New role</Btn>
        ) : (
          <span style={{ fontSize: c.fontXs, color: c.textSec, maxWidth: "42ch" }}>
            Roles are shared by every agency, so only an unrestricted administrator changes them. You can grant
            and revoke these roles for your agencies below.
          </span>
        )}
      </div>

      {err && <div style={{ padding: 16 }}><AlertBanner type="danger">{err}</AlertBanner></div>}
      {error && <div style={{ padding: 16 }}><AlertBanner type="danger">{errMsg(error)}</AlertBanner></div>}

      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr>
            <th style={thStyle()}>Role</th>
            <th style={thStyle()}>Permissions</th>
            <th style={{ ...thStyle(), width: 170, textAlign: "right" }}>Actions</th>
          </tr>
        </thead>
        <tbody>
          {loading && <SkeletonRows rows={4} cols={3} />}
          {!loading && roles.length === 0 && (
            <tr><td style={tdStyle()} colSpan={3}>No roles are defined.</td></tr>
          )}
          {!loading && roles.map((r) => (
            <tr key={r.name}>
              <td style={tdStyle()}>
                <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                  <strong style={{ fontFamily: "monospace" }}>{r.name}</strong>
                  {r.builtin && (
                    <span title="Ships with Cronomicon. Editable, but cannot be deleted." style={{ fontSize: c.fontXs, textTransform: "uppercase", letterSpacing: 0.6, padding: "2px 6px", borderRadius: c.radiusChip, border: `1px solid ${c.border}`, color: c.textSec }}>
                      built-in
                    </span>
                  )}
                </div>
                {r.description && <div style={{ color: c.textSec, fontSize: c.fontSm, marginTop: 4 }}>{r.description}</div>}
              </td>
              <td style={tdStyle()}>
                {held(r).length === 0
                  // Deliberate and worth stating: viewer holds nothing after
                  // v0.56.1. Its only permission was viewDashboard, which gated
                  // nothing. Read access comes from scopes, not from a verb.
                  ? <span style={{ color: c.textSec }}>No permissions — read-only. Visibility comes from its granted scopes.</span>
                  : <span style={{ fontSize: c.fontSm }}>{held(r).join(" · ")}</span>}
              </td>
              <td style={{ ...tdStyle(), textAlign: "right", whiteSpace: "nowrap" }}>
                {canEditTemplates && (
                  <>
                    <Btn small onClick={() => { setCreating(false); setEditing(r); }}>Edit</Btn>{" "}
                  </>
                )}
                {/* Built-ins have no Delete at all rather than a disabled one: the
                    disabled state does not read as disabled at this size, so an
                    inert red Delete beside `admin` looks like a bug the first time
                    someone clicks it. Absence is unambiguous, and the badge beside
                    the name already says why. */}
                {canEditTemplates && !r.builtin && <Btn small danger onClick={() => setDeleting(r)}>Delete</Btn>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {editing && (
        <RoleEditor
          role={editing}
          isNew={creating}
          busy={busy}
          onCancel={() => { setEditing(null); setCreating(false); setErr(null); }}
          onSave={(d) => save(d, creating)}
        />
      )}

      {deleting && (
        <ConfirmDialog
          title={`Delete role ${deleting.name}?`}
          danger
          busy={busy}
          message={
            <>
              Any access grant still using <strong style={{ fontFamily: c.mono }}>{deleting.name}</strong> blocks
              this delete — remove those grants first.
              <div style={{ marginTop: 10 }}>
                Its scope grants are removed with it, and every other operator is <strong>signed out</strong>.
              </div>
            </>
          }
          confirmLabel={busy ? "Deleting…" : "Delete role"}
          onCancel={() => setDeleting(null)}
          onConfirm={() => remove(deleting)}
        />
      )}
    </Card>
  );
}

// RoleEditor — name, description, rank, and the six permission checkboxes.
function RoleEditor({ role, isNew, busy, onCancel, onSave }: {
  role: Role; isNew: boolean; busy: boolean; onCancel: () => void; onSave: (r: Role) => void;
}) {
  const [draft, setDraft] = useState<Role>(role);
  const perms = draft.permissions ?? EMPTY_PERMS;
  const setPerm = (k: keyof RolePermissions, v: boolean) =>
    setDraft((d) => ({ ...d, permissions: { ...(d.permissions ?? EMPTY_PERMS), [k]: v } }));

  // The lockout floor, mirrored client-side so the box explains itself rather than
  // producing a 409 the user cannot interpret. The server enforces it regardless.
  const adminLock = draft.name === "admin";

  return (
    <div style={{ padding: 16, borderTop: `1px solid ${c.border}`, background: c.panel2 }}>
      <div style={{ display: "flex", gap: 12, flexWrap: "wrap", marginBottom: 12 }}>
        <label style={{ flex: 1, minWidth: 200 }}>
          <div style={{ color: c.textSec, fontSize: c.fontSm, marginBottom: 4 }}>Name</div>
          <input
            value={draft.name ?? ""}
            disabled={!isNew}
            onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))}
            placeholder="tax-operator"
            style={{ ...inputStyle(), width: "100%", fontFamily: "monospace" }}
          />
          {isNew && <div style={{ color: c.textSec, fontSize: c.fontSm, marginTop: 4 }}>Stored lowercase. Renaming later is not supported.</div>}
        </label>
        <label style={{ flex: 2, minWidth: 240 }}>
          <div style={{ color: c.textSec, fontSize: c.fontSm, marginBottom: 4 }}>Description</div>
          <input
            value={draft.description ?? ""}
            onChange={(e) => setDraft((d) => ({ ...d, description: e.target.value }))}
            style={{ ...inputStyle(), width: "100%" }}
          />
        </label>
        <label style={{ width: 110 }}>
          <div style={{ color: c.textSec, fontSize: c.fontSm, marginBottom: 4 }}>Rank</div>
          <input
            type="number"
            value={draft.rank ?? 0}
            onChange={(e) => setDraft((d) => ({ ...d, rank: Number(e.target.value) }))}
            style={{ ...inputStyle(), width: "100%" }}
          />
        </label>
      </div>

      <div style={{ color: c.textSec, fontSize: c.fontSm, marginBottom: 8 }}>Permissions</div>
      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(280px, 1fr))", gap: 8, marginBottom: 12 }}>
        {PERMISSION_LABELS.map((p) => {
          const locked = adminLock && p.key === "manageRoles";
          return (
            <label key={p.key} title={p.hint} style={{ display: "flex", gap: 8, alignItems: "flex-start", cursor: locked ? "not-allowed" : "pointer", opacity: locked ? 0.7 : 1 }}>
              <input
                type="checkbox"
                checked={!!perms[p.key]}
                disabled={locked}
                onChange={(e) => setPerm(p.key, e.target.checked)}
                style={{ marginTop: 2 }}
              />
              <span>
                <span>{p.label}</span>
                <div style={{ color: c.textSec, fontSize: c.fontSm }}>
                  {locked ? "The admin role cannot give this up — nothing could restore it." : p.hint}
                </div>
              </span>
            </label>
          );
        })}
      </div>

      <div style={{ display: "flex", gap: 8 }}>
        <Btn primary disabled={busy || !(draft.name ?? "").trim()} onClick={() => onSave(draft)}>
          {busy ? "Saving…" : isNew ? "Create role" : "Save changes"}
        </Btn>
        <Btn onClick={onCancel} disabled={busy}>Cancel</Btn>
        <div style={{ color: c.textSec, fontSize: c.fontSm, alignSelf: "center" }}>
          Saving signs out every other operator (their permissions may have changed).
        </div>
      </div>
    </div>
  );
}

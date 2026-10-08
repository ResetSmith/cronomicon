import { Fragment, useState } from "react";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { useGet, rows, useColumnWidths, useTableSort } from "../../hooks";
import { ColumnsMenu, TableHead, renderCells, useTableColumns } from "../../components/table";
import { type SortColumn } from "../../utils/sort";
import { c } from "../../theme";
import { SkeletonRows } from "../../components/ui";
import { GLOBAL_AGENCY, agenciesFor, useMyAccess, type AgencyRef } from "../../api/access";
import { Btn, Card, StatusBadge, csrfHeader, errMsg, fmtDateTime, inputStyle } from "./ui";

type SshHost = components["schemas"]["SshHost"];
type SshHostInput = components["schemas"]["SshHostInput"];
type SshBastion = components["schemas"]["SshBastion"];
type SshBastionInput = components["schemas"]["SshBastionInput"];
type Credential = components["schemas"]["SshCredential"];

interface HostForm {
  hostname: string;
  address: string;
  port: number;
  os: "Linux" | "Windows";
  via: string;
  authKeyEnvVar: string;
  authCredentialId: string;
  user: string;
  /** The agency that owns a hand-written record (LR-69). Chosen on Add; fixed afterwards. */
  ownerAgency: string;
}

const EMPTY_HOST: HostForm = { hostname: "", address: "", port: 22, os: "Linux", via: "", authKeyEnvVar: "", authCredentialId: "", user: "", ownerAgency: "" };

function hostToForm(h: SshHost): HostForm {
  return {
    hostname: h.hostname,
    address: h.address ?? "",
    port: h.port ?? 22,
    os: h.os ?? "Linux",
    via: h.via ?? "",
    authKeyEnvVar: h.authKeyEnvVar ?? "",
    authCredentialId: h.authCredentialId ?? "",
    user: h.user ?? "",
    ownerAgency: h.ownerAgency ?? "",
  };
}

// The owner is sent when a record is CREATED. An edit leaves it out: giving a
// record to another agency is a move, and this form does not make one.
function hostToInput(f: HostForm, withOwner = false): SshHostInput {
  return {
    ...(withOwner && f.ownerAgency ? { ownerAgency: f.ownerAgency } : {}),
    hostname: f.hostname,
    address: f.address,
    port: f.port,
    os: f.os,
    via: f.via || null,
    authKeyEnvVar: f.authKeyEnvVar,
    // An FK column: empty MUST be null, never "" (no credential has id "").
    authCredentialId: f.authCredentialId || null,
    user: f.user,
  };
}

interface BastionForm {
  name: string;
  address: string;
  port: number;
  username: string;
  authKeyEnvVar: string;
  authCredentialId: string;
  zone: string;
  ownerAgency: string;
}

const EMPTY_BASTION: BastionForm = { name: "", address: "", port: 22, username: "jump", authKeyEnvVar: "", authCredentialId: "", zone: "", ownerAgency: "" };

function bastionToForm(b: SshBastion): BastionForm {
  return {
    name: b.name,
    address: b.address,
    port: b.port ?? 22,
    username: b.username ?? "",
    authKeyEnvVar: b.authKeyEnvVar ?? "",
    authCredentialId: b.authCredentialId ?? "",
    zone: b.zone ?? "",
    ownerAgency: b.ownerAgency ?? "",
  };
}

function bastionToInput(f: BastionForm, withOwner = false): SshBastionInput {
  return {
    ...(withOwner && f.ownerAgency ? { ownerAgency: f.ownerAgency } : {}),
    name: f.name,
    address: f.address,
    port: f.port,
    username: f.username,
    authKeyEnvVar: f.authKeyEnvVar,
    authCredentialId: f.authCredentialId || null,
    zone: f.zone,
  };
}

const smallInput = (extra?: React.CSSProperties): React.CSSProperties => ({ ...inputStyle(), fontSize: c.fontSm, padding: "5px 8px", ...extra });

// shortFp trims a SHA256:… fingerprint to a compact tail for the table cell; the
// full value stays in the title for out-of-band comparison.
function shortFp(fp?: string): string {
  if (!fp) return "";
  const body = fp.replace(/^SHA256:/, "");
  return body.length > 12 ? "…" + body.slice(-12) : body;
}

// HostKeyBadge shows the host key the LOCAL RUNNER trusts for a target's or a
// bastion's address (FU-1; 2.3.0). The server connects only to a host with a
// key an operator approved for the local runner — it captures nothing on first
// connect any more — so this is the answer to "can the server reach this host":
// approved ⇒ green lock + short fingerprint (hover for the full SHA256 to
// compare out-of-band); none ⇒ amber, with where the key is approved. An agent
// has its own trusted keys (Runners → the runner → Host keys); this badge says
// nothing about them.
function HostKeyBadge({ pinned, fingerprint, keyType }: { pinned?: boolean; fingerprint?: string; keyType?: string }) {
  if (pinned) {
    return (
      <span
        title={`The local runner trusts this host key${keyType ? ` (${keyType})` : ""}. Compare this fingerprint out-of-band:\n${fingerprint ?? "(unparseable)"}`}
        style={{ display: "inline-flex", alignItems: "center", gap: 4, fontSize: c.fontXs, color: c.success, fontFamily: c.mono }}
      >
        <span aria-hidden>🔒</span>
        {fingerprint ? shortFp(fingerprint) : "approved"}
      </span>
    );
  }
  return (
    <span
      title={
        "No host key is approved for the local runner at this address, so the server will not connect to it: a run it takes fails for this host (host_key_unverified). " +
        "Scan the host and approve its key under Runners → Local runner → Host keys. Nothing is captured on first connect."
      }
      style={{ fontSize: c.fontXs, color: c.warning }}
    >
      ⚠ no approved key
    </span>
  );
}

// KeyPicker chooses how a host/bastion's key is resolved. Two permanent,
// coequal mechanisms (SK.17): a first-class **SSH key credential** (preferred for
// app-managed hosts the in-app executor dials) OR a key **by name** — required for
// runner-executed hosts (the runner agent resolves the name to a key it holds
// locally; credential material never crosses the wire) and for inventory/git-
// imported hosts (which name keys in their Ansible vars). Selecting one clears the
// other; an empty selection sends neither.
function KeyPicker({
  credentialId,
  keyEnvVar,
  set,
  creds,
}: {
  credentialId: string;
  keyEnvVar: string;
  set: (p: { authCredentialId?: string; authKeyEnvVar?: string }) => void;
  creds: Credential[];
}) {
  const BY_NAME = "__byname__";
  const sel = credentialId ? credentialId : keyEnvVar ? BY_NAME : "";
  return (
    <span style={{ display: "inline-flex", gap: 6, alignItems: "center" }}>
      <select
        value={sel}
        onChange={(e) => {
          const v = e.target.value;
          if (v === BY_NAME) set({ authCredentialId: "", authKeyEnvVar: keyEnvVar || "" });
          else if (v === "") set({ authCredentialId: "", authKeyEnvVar: "" });
          else set({ authCredentialId: v, authKeyEnvVar: "" });
        }}
        style={smallInput({ cursor: "pointer", width: 165 })}
        title="SSH key: a credential (Env Vars → SSH Keys), or a key resolved by name (runner / inventory hosts)"
      >
        <option value="">— No key —</option>
        {creds.map((cr) => (
          <option key={cr.id} value={cr.id}>
            {cr.label}
            {cr.keyType ? ` · ${cr.keyType}` : ""}
          </option>
        ))}
        <option value={BY_NAME}>Key by name (runner / inventory)…</option>
      </select>
      {sel === BY_NAME && (
        <input
          value={keyEnvVar}
          onChange={(e) => set({ authKeyEnvVar: e.target.value })}
          placeholder="SSH_KEY_ENV_VAR"
          style={smallInput({ width: 130, fontFamily: c.mono, fontSize: c.fontXs })}
        />
      )}
    </span>
  );
}

// Default column widths (px) for table-layout:fixed (V1.1-7). Stored overrides
// come from useColumnWidths. One entry per column — resizable and fixed alike.
const HOST_COL_W: Record<string, number> = {
  hostname: 160,
  address: 150,
  os: 70,
  via: 120,
  authKey: 150,
  user: 90,
  owner: 110,
  status: 100,
  lastChecked: 130,
  actions: 200,
};

const BASTION_COL_W: Record<string, number> = {
  name: 160,
  address: 150,
  port: 70,
  user: 90,
  authKey: 150,
  zone: 100,
  owner: 110,
  status: 100,
  lastChecked: 130,
  actions: 200,
};

// Status rank (the sorting-update plan §3.2): worst first when ascending, per
// TS-Q5. `reachable` and `verified` share a rank ON PURPOSE — v0.55.8 gave the
// two good outcomes equal weight — so the tiebreak falls through to text order.
// A status outside the map (or missing) sorts last automatically.
const SSH_STATUS_RANK: Record<string, number> = { cred_error: 0, conn_error: 0, unverified: 1, reachable: 2, verified: 2 };

// CO-4 — this file's own sortableTh / sortableResizableTh pair is gone. They
// were a THIRD copy of the same two closures (the comment above them named
// ExecutionsTab as the source), which is precisely the drift <TableHead> exists
// to end.

// `canWrite` — the caller is a global administrator for configureApp.
//
// A bastion and a hand-written host record belong to ONE agency (v2.3.0,
// LR-69): its administrators create and change them, and Global's are a global
// administrator's. Until 2.3.0 both were a global administrator's alone, since
// neither had an owner. So:
//
//   - "+ Add" asks whose the record is, offering the agencies the caller
//     administers (Global too, for a global administrator);
//   - a record another agency owns has its actions disabled, with whose it is;
//   - a host IMPORTED for a scope follows that scope's gate, which the server
//     judges; this view does not guess, and its actions stay live.
export function SshTargetsSection({ canWrite }: { canWrite: boolean }) {
  // LR-69 (v2.3.0) — a bastion and a hand-written host record belong to ONE
  // agency. Its administrators create and change them; Global's are a global
  // administrator's. `owners` is what this caller may create a record for.
  const access = useMyAccess();
  const catalogQ = useGet<unknown>(() => api.GET("/agencies"), []);
  const catalog = rows<{ id: string; name: string }>(catalogQ.data);
  const owners: AgencyRef[] = canWrite
    ? [{ id: GLOBAL_AGENCY, name: "Global" }, ...catalog.filter((a) => a.id !== GLOBAL_AGENCY)]
    : agenciesFor(access ?? null, "configureApp");
  const mine = new Set(owners.map((a) => a.id));
  // Whether the caller's agencies are KNOWN. While /me/access is loading, or
  // if it cannot be read, nothing here claims a record is somebody else's or
  // that there is no agency to create one for: the controls stay live and the
  // server decides, as it does on the Runners page.
  const known = canWrite || access != null;
  const ownerName = (id?: string | null, name?: string | null) =>
    name || owners.find((a) => a.id === id)?.name || catalog.find((a) => a.id === id)?.name || (id === GLOBAL_AGENCY || !id ? "Global" : id);
  // Why "+ Add" is disabled: the caller administers no agency to create one for.
  const addWhy = known && owners.length === 0 ? "You do not administer an agency, so there is none to create this for." : "";
  // Why a record's actions are disabled: it is another owner's. A caller on an
  // older server (no owner on the row) is not second-guessed: the server decides.
  const ownedWhy = (owner: string | undefined | null, name: string | undefined | null, what: string): string => {
    if (!known || !owner || mine.has(owner)) return "";
    return owner === GLOBAL_AGENCY
      ? `This ${what} is Global's — only a global administrator (a role on every agency) can change it.`
      : `This ${what} belongs to ${ownerName(owner, name)} — only that agency's administrators can change it.`;
  };
  const defaultOwner = canWrite ? GLOBAL_AGENCY : owners.length === 1 ? owners[0].id : "";
  const [bump, setBump] = useState(0);
  const hostsQ = useGet<unknown>(() => api.GET("/ssh/hosts"), [bump]);
  const bastionsQ = useGet<unknown>(() => api.GET("/ssh/bastions"), [bump]);
  const hosts = rows<SshHost>(hostsQ.data);
  const bastions = rows<SshBastion>(bastionsQ.data);
  const credsQ = useGet<unknown>(() => api.GET("/ssh/credentials"), [bump]);
  const creds = rows<Credential>(credsQ.data);
  const credById = new Map(creds.map((cr) => [cr.id, cr]));

  const [actionErr, setActionErr] = useState<string | null>(null);
  const [actionOk, setActionOk] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [testingId, setTestingId] = useState<number | string | null>(null);

  const hostCw = useColumnWidths("ssh-hosts");
  const bastionCw = useColumnWidths("ssh-bastions");

  // host state
  const [hostSearch, setHostSearch] = useState("");
  const [showAddHost, setShowAddHost] = useState(false);
  const [addHostForm, setAddHostForm] = useState<HostForm>({ ...EMPTY_HOST });
  const [editHostId, setEditHostId] = useState<string | null>(null);
  const [editHostForm, setEditHostForm] = useState<HostForm>({ ...EMPTY_HOST });

  // bastion state
  const [showAddBastion, setShowAddBastion] = useState(false);
  const [addBastionForm, setAddBastionForm] = useState<BastionForm>({ ...EMPTY_BASTION });
  const [editBastionId, setEditBastionId] = useState<string | null>(null);
  const [editBastionForm, setEditBastionForm] = useState<BastionForm>({ ...EMPTY_BASTION });

  const done = (e: unknown) => {
    setBusy(false);
    if (e) {
      setActionErr(errMsg(e));
      return false;
    }
    setBump((b) => b + 1);
    return true;
  };

  async function createHost() {
    if (!addHostForm.hostname.trim()) {
      setActionErr("Hostname is required.");
      return;
    }
    setBusy(true);
    setActionErr(null);
    const owner = addHostForm.ownerAgency || defaultOwner;
    if (!owner && known) {
      setBusy(false);
      setActionErr("Choose the agency that owns this host record.");
      return;
    }
    const { error } = await api.POST("/ssh/hosts", { params: { header: csrfHeader }, body: hostToInput({ ...addHostForm, ownerAgency: owner }, true) });
    if (done(error)) {
      setShowAddHost(false);
      setAddHostForm({ ...EMPTY_HOST });
    }
  }

  async function saveHost(id: string) {
    setBusy(true);
    setActionErr(null);
    const { error } = await api.PUT("/ssh/hosts/{hostId}", {
      params: { path: { hostId: id }, header: csrfHeader },
      body: hostToInput(editHostForm),
    });
    if (done(error)) setEditHostId(null);
  }

  async function removeHost(h: SshHost) {
    if (h.id == null) return;
    if (!window.confirm(`Remove ${h.hostname} from the host registry?`)) return;
    setBusy(true);
    setActionErr(null);
    const { error } = await api.DELETE("/ssh/hosts/{hostId}", { params: { path: { hostId: h.id }, header: csrfHeader } });
    done(error);
  }

  async function createBastion() {
    if (!addBastionForm.name.trim() || !addBastionForm.address.trim()) {
      setActionErr("Bastion name and address are required.");
      return;
    }
    setBusy(true);
    setActionErr(null);
    const owner = addBastionForm.ownerAgency || defaultOwner;
    if (!owner && known) {
      setBusy(false);
      setActionErr("Choose the agency that owns this bastion.");
      return;
    }
    const { error } = await api.POST("/ssh/bastions", { params: { header: csrfHeader }, body: bastionToInput({ ...addBastionForm, ownerAgency: owner }, true) });
    if (done(error)) {
      setShowAddBastion(false);
      setAddBastionForm({ ...EMPTY_BASTION });
    }
  }

  async function saveBastion(id: string) {
    setBusy(true);
    setActionErr(null);
    const { error } = await api.PUT("/ssh/bastions/{bastionId}", {
      params: { path: { bastionId: id }, header: csrfHeader },
      body: bastionToInput(editBastionForm),
    });
    if (done(error)) setEditBastionId(null);
  }

  async function removeBastion(b: SshBastion) {
    if (b.id == null) return;
    const affected = hosts.filter((h) => h.via === b.name).length;
    const warn = affected > 0 ? ` ${affected} host(s) route through this bastion.` : "";
    if (!window.confirm(`Remove bastion ${b.name}?${warn}`)) return;
    setBusy(true);
    setActionErr(null);
    const { error } = await api.DELETE("/ssh/bastions/{bastionId}", { params: { path: { bastionId: b.id }, header: csrfHeader } });
    done(error);
  }

  async function testHost(h: SshHost) {
    if (h.id == null) return;
    setTestingId(h.id);
    setActionErr(null);
    setActionOk(null);
    const { data, error } = await api.POST("/ssh/hosts/{hostId}/test", {
      params: { path: { hostId: h.id }, header: csrfHeader },
    });
    setTestingId(null);
    if (error) {
      setActionErr(errMsg(error));
      return;
    }
    // Surface the outcome inline: a verified test confirms WHICH key authenticated
    // (the message carries the resolved key fingerprint, SK.12); reachable (TT) is
    // the keyless tier's good outcome — endpoint answered, host key verified, auth
    // untested; a failure shows why, an unapproved host key included (a test
    // verifies and never captures). FU-1: also show the fingerprint of the key
    // the local runner trusts, so the operator can compare it out-of-band.
    if (data?.status === "verified" || data?.status === "reachable") {
      const fp = data.hostKeyFingerprint ? ` — host key ${data.hostKeyFingerprint}` : "";
      setActionOk(`${h.hostname}: ${data.message ?? "connection verified"}${fp}`);
    } else if (data?.message) setActionErr(`${h.hostname}: ${data.message}`);
    setBump((b) => b + 1); // re-pull list so status + Last Checked + the host-key badge reflect server truth
  }

  async function testBastion(b: SshBastion) {
    if (b.id == null) return;
    setTestingId(b.id);
    setActionErr(null);
    setActionOk(null);
    const { data, error } = await api.POST("/ssh/bastions/{bastionId}/test", {
      params: { path: { bastionId: b.id }, header: csrfHeader },
    });
    setTestingId(null);
    if (error) {
      setActionErr(errMsg(error));
      return;
    }
    if (data?.status === "verified" || data?.status === "reachable") {
      const fp = data.hostKeyFingerprint ? ` — host key ${data.hostKeyFingerprint}` : "";
      setActionOk(`${b.name}: ${data.message ?? "connection verified"}${fp}`);
    } else if (data?.message) setActionErr(`${b.name}: ${data.message}`);
    setBump((bp) => bp + 1); // re-pull list so status + Last Checked + the host-key badge reflect server truth
  }

  const filteredHosts = hosts.filter(
    (h) =>
      !hostSearch ||
      h.hostname.toLowerCase().includes(hostSearch.toLowerCase()) ||
      (h.address ?? "").toLowerCase().includes(hostSearch.toLowerCase()),
  );

  // Sortable columns (the sorting-update plan §3.2). Built inside the component
  // because Auth Key sorts by its DISPLAYED value: the credential's label when
  // the id resolves via credById, else the raw env-var name (matching the cell).
  const authKeyDisplay = (credId?: string | null, envVar?: string | null): string =>
    (credId ? credById.get(credId)?.label : undefined) ?? envVar ?? "";
  const hostSortCols: SortColumn<SshHost>[] = [
    { key: "hostname", get: (h) => h.hostname, type: "text" },
    { key: "address", get: (h) => h.address, type: "text" },
    { key: "os", get: (h) => h.os, type: "text" },
    { key: "via", get: (h) => h.via, type: "text" },
    { key: "authKey", get: (h) => authKeyDisplay(h.authCredentialId, h.authKeyEnvVar), type: "text" },
    { key: "user", get: (h) => h.user, type: "text" },
    { key: "owner", get: (h) => (h.scopeId ? "" : ownerName(h.ownerAgency, h.ownerAgencyName)), type: "text" },
    { key: "status", get: (h) => h.status, type: "rank", rank: SSH_STATUS_RANK },
    { key: "lastChecked", get: (h) => h.lastCheckedAt, type: "date" },
  ];
  const bastionSortCols: SortColumn<SshBastion>[] = [
    { key: "name", get: (b) => b.name, type: "text" },
    { key: "address", get: (b) => b.address, type: "text" },
    { key: "port", get: (b) => b.port, type: "number" },
    { key: "user", get: (b) => b.username, type: "text" },
    { key: "authKey", get: (b) => authKeyDisplay(b.authCredentialId, b.authKeyEnvVar), type: "text" },
    { key: "zone", get: (b) => b.zone, type: "text" },
    { key: "owner", get: (b) => ownerName(b.ownerAgency, b.ownerAgencyName), type: "text" },
    { key: "status", get: (b) => b.status, type: "rank", rank: SSH_STATUS_RANK },
    { key: "lastChecked", get: (b) => b.lastCheckedAt, type: "date" },
  ];
  // Sort after the search filter, before render (there is no paging here).
  const hostSort = useTableSort(filteredHosts, hostSortCols, { key: "hostname", dir: "asc" }, { tableId: "ssh-hosts" });
  const bastionSort = useTableSort(bastions, bastionSortCols, { key: "name", dir: "asc" }, { tableId: "ssh-bastions" });

  // CO-4 — the two column specs. Built in render: cells read `c.*` and close
  // over the edit/test state and the credential lookup.
  const hostCols = useTableColumns<SshHost>("ssh-hosts", [
    {
      key: "hostname",
      label: "Hostname",
      sortKey: "hostname",
      width: HOST_COL_W.hostname,
      pin: "first",
      tdStyle: { fontWeight: 600 },
      cell: (h) => (
        <>
          {h.hostname}
          {h.source === "git" && (
            <span
              title="Imported from a git inventory by sync — read-only. Author an Cronomicon host with the same name to override its connection details."
              style={{ marginLeft: 6, fontSize: c.fontXs, fontWeight: 500, color: c.textSec, border: `1px solid ${c.border}`, borderRadius: c.radiusChip, padding: "1px 5px" }}
            >
              git
            </span>
          )}
        </>
      ),
    },
    {
      key: "address",
      label: "Address",
      sortKey: "address",
      width: HOST_COL_W.address,
      cell: (h) => <code style={{ fontSize: c.fontSm, fontFamily: c.mono, color: h.address ? c.text : c.textSec }}>{h.address || "—"}</code>,
    },
    { key: "os", label: "OS", sortKey: "os", width: HOST_COL_W.os, fixed: true, tdStyle: { color: c.textSec }, cell: (h) => h.os ?? "—" },
    {
      key: "via",
      label: "Connect Via",
      sortKey: "via",
      width: HOST_COL_W.via,
      fixed: true,
      cell: (h) => (h.via ? <span style={{ color: c.primary, fontWeight: 500 }}>{h.via}</span> : <span style={{ color: c.textSec }}>Direct</span>),
    },
    {
      key: "authKey",
      label: "Auth Key",
      sortKey: "authKey",
      width: HOST_COL_W.authKey,
      cell: (h) => {
        const keyCred = h.authCredentialId ? credById.get(h.authCredentialId) : undefined;
        return keyCred ? (
          <span style={{ fontSize: c.fontSm, fontWeight: 500, color: c.primary }} title={keyCred.fingerprint || keyCred.label}>
            {keyCred.label}
          </span>
        ) : (
          <code style={{ fontSize: c.fontXs, fontFamily: c.mono, color: h.authKeyEnvVar ? c.text : c.textSec }}>{h.authKeyEnvVar || "—"}</code>
        );
      },
    },
    { key: "user", label: "User", sortKey: "user", width: HOST_COL_W.user, fixed: true, tdStyle: { color: c.textSec }, cell: (h) => h.user || "—" },
    {
      key: "owner",
      label: "Owner",
      sortKey: "owner",
      width: HOST_COL_W.owner,
      tdStyle: { color: c.textSec },
      // A hand-written record has an owner; one imported for a scope belongs to
      // that scope, and says so instead.
      cell: (h) => (h.scopeId ? <span title="Imported for a scope: it follows that scope's agency.">its scope's</span> : ownerName(h.ownerAgency, h.ownerAgencyName)),
    },
    {
      key: "status",
      label: "Status",
      sortKey: "status",
      width: HOST_COL_W.status,
      fixed: true,
      cell: (h) => (
        <div style={{ display: "flex", flexDirection: "column", gap: 3, alignItems: "flex-start" }}>
          <StatusBadge status={h.status} />
          <HostKeyBadge pinned={h.hostKeyPinned} fingerprint={h.hostKeyFingerprint} keyType={h.hostKeyType} />
        </div>
      ),
    },
    {
      key: "lastChecked",
      label: "Last Checked",
      sortKey: "lastChecked",
      width: HOST_COL_W.lastChecked,
      fixed: true,
      tdStyle: { color: c.textSec, fontSize: c.fontSm, whiteSpace: "nowrap" },
      cell: (h) => fmtDateTime(h.lastCheckedAt),
    },
    {
      key: "actions",
      label: "",
      menuLabel: "Actions",
      width: HOST_COL_W.actions,
      fixed: true,
      pin: "last",
      tdStyle: { whiteSpace: "nowrap" },
      cell: (h) => {
        const editing = editHostId === (h.id ?? "");
        const isGit = h.source === "git";
        // A hand-written record (no scope) is its owner's. One imported for a
        // scope follows the scope's gate, which the server judges: left live.
        const why = h.scopeId ? "" : ownedWhy(h.ownerAgency, h.ownerAgencyName, "host record");
        return (
          <div style={{ display: "flex", gap: 4 }}>
            {editing ? (
              <>
                <Btn primary onClick={() => h.id != null && saveHost(h.id)} disabled={busy}>
                  Save
                </Btn>
                <Btn onClick={() => setEditHostId(null)}>Cancel</Btn>
              </>
            ) : (
              <>
                <Btn onClick={() => testHost(h)} disabled={testingId === h.id || busy || !!why} title={why || undefined}>
                  {testingId === h.id ? "Testing…" : "Test"}
                </Btn>
                {isGit ? (
                  <span style={{ fontSize: c.fontXs, color: c.textSec, alignSelf: "center" }}>imported (read-only)</span>
                ) : (
                  <>
                    <Btn
                      disabled={!!why}
                      title={why || undefined}
                      onClick={() => {
                        setEditHostId(h.id ?? null);
                        setEditHostForm(hostToForm(h));
                        setActionErr(null);
                      }}
                    >
                      Edit
                    </Btn>
                    <Btn danger onClick={() => removeHost(h)} disabled={busy || !!why} title={why || undefined}>
                      Remove
                    </Btn>
                  </>
                )}
              </>
            )}
          </div>
        );
      },
    },
  ]);

  const bastionCols = useTableColumns<SshBastion>("ssh-bastions", [
    { key: "name", label: "Name", sortKey: "name", width: BASTION_COL_W.name, pin: "first", tdStyle: { fontWeight: 600 }, cell: (b) => b.name },
    {
      key: "address",
      label: "Address",
      sortKey: "address",
      width: BASTION_COL_W.address,
      cell: (b) => <code style={{ fontSize: c.fontSm, fontFamily: c.mono }}>{b.address}</code>,
    },
    { key: "port", label: "Port", sortKey: "port", width: BASTION_COL_W.port, fixed: true, tdStyle: { color: c.textSec }, cell: (b) => b.port ?? 22 },
    { key: "user", label: "User", sortKey: "user", width: BASTION_COL_W.user, fixed: true, tdStyle: { color: c.textSec }, cell: (b) => b.username || "—" },
    {
      key: "authKey",
      label: "Auth Key",
      sortKey: "authKey",
      width: BASTION_COL_W.authKey,
      cell: (b) => {
        const keyCred = b.authCredentialId ? credById.get(b.authCredentialId) : undefined;
        return keyCred ? (
          <span style={{ fontSize: c.fontSm, fontWeight: 500, color: c.primary }} title={keyCred.fingerprint || keyCred.label}>
            {keyCred.label}
          </span>
        ) : (
          <code style={{ fontSize: c.fontXs, fontFamily: c.mono, color: b.authKeyEnvVar ? c.text : c.textSec }}>{b.authKeyEnvVar || "—"}</code>
        );
      },
    },
    {
      key: "zone",
      label: "Zone",
      sortKey: "zone",
      width: BASTION_COL_W.zone,
      fixed: true,
      cell: (b) =>
        b.zone ? (
          <span style={{ padding: "2px 8px", borderRadius: c.radiusChip, fontSize: c.fontXs, fontWeight: 600, background: `${c.primary}24`, color: c.primary }}>
            {b.zone}
          </span>
        ) : (
          <span style={{ color: c.textSec }}>—</span>
        ),
    },
    {
      key: "owner",
      label: "Owner",
      sortKey: "owner",
      width: BASTION_COL_W.owner,
      tdStyle: { color: c.textSec },
      cell: (b) => ownerName(b.ownerAgency, b.ownerAgencyName),
    },
    {
      key: "status",
      label: "Status",
      sortKey: "status",
      width: BASTION_COL_W.status,
      fixed: true,
      cell: (b) => (
        <div style={{ display: "flex", flexDirection: "column", gap: 3, alignItems: "flex-start" }}>
          <StatusBadge status={b.status} />
          <HostKeyBadge pinned={b.hostKeyPinned} fingerprint={b.hostKeyFingerprint} keyType={b.hostKeyType} />
        </div>
      ),
    },
    {
      key: "lastChecked",
      label: "Last Checked",
      sortKey: "lastChecked",
      width: BASTION_COL_W.lastChecked,
      fixed: true,
      tdStyle: { color: c.textSec, fontSize: c.fontSm, whiteSpace: "nowrap" },
      cell: (b) => fmtDateTime(b.lastCheckedAt),
    },
    {
      key: "actions",
      label: "",
      menuLabel: "Actions",
      width: BASTION_COL_W.actions,
      fixed: true,
      pin: "last",
      tdStyle: { whiteSpace: "nowrap" },
      cell: (b) => {
        const editing = editBastionId === (b.id ?? "");
        const why = ownedWhy(b.ownerAgency, b.ownerAgencyName, "bastion");
        return (
          <div style={{ display: "flex", gap: 4 }}>
            {editing ? (
              <>
                <Btn primary onClick={() => b.id != null && saveBastion(b.id)} disabled={busy}>
                  Save
                </Btn>
                <Btn onClick={() => setEditBastionId(null)}>Cancel</Btn>
              </>
            ) : (
              <>
                <Btn onClick={() => testBastion(b)} disabled={testingId === b.id || busy || !!why} title={why || undefined}>
                  {testingId === b.id ? "Testing…" : "Test"}
                </Btn>
                <Btn
                  disabled={!!why}
                  title={why || undefined}
                  onClick={() => {
                    setEditBastionId(b.id ?? null);
                    setEditBastionForm(bastionToForm(b));
                    setActionErr(null);
                  }}
                >
                  Edit
                </Btn>
                <Btn danger onClick={() => removeBastion(b)} disabled={busy || !!why} title={why || undefined}>
                  Remove
                </Btn>
              </>
            )}
          </div>
        );
      },
    },
  ]);

  const verified = hosts.filter((h) => h.status === "verified").length;
  const reachable = hosts.filter((h) => h.status === "reachable").length;
  const errored = hosts.filter((h) => h.status === "cred_error" || h.status === "conn_error").length;
  const unverified = hosts.filter((h) => h.status === "unverified" || !h.status).length;

  const viaOptions = [{ v: "", l: "Direct" }, ...bastions.map((b) => ({ v: b.name, l: b.name }))];

  // The keys a record may name: its owner's, and Global's (LR-72). Anything
  // else would be refused, so it is not offered. The key a record already
  // names stays listed, so an edit shows what is there.
  const keysFor = (owner: string, current: string) => {
    const name = ownerName(owner || defaultOwner);
    return creds.filter((cr) => cr.id === current || !cr.ownerAgency || cr.ownerAgency === "Global" || cr.ownerAgency === name);
  };
  const ownerPick = (value: string, set: (id: string) => void, what: string) => (
    <select
      aria-label={`Agency that owns this ${what}`}
      value={value || defaultOwner}
      disabled={owners.length <= 1}
      onChange={(e) => set(e.target.value)}
      title={`The agency that owns this ${what}: its administrators change it, and its runs (and no other agency's) use it. Global's is used by every agency.`}
      style={smallInput({ cursor: owners.length <= 1 ? "default" : "pointer" })}
    >
      {!canWrite && owners.length > 1 && <option value="">Owner…</option>}
      {owners.map((a) => (
        <option key={a.id} value={a.id}>
          {a.name}
        </option>
      ))}
    </select>
  );

  const hostFields = (form: HostForm, set: (p: Partial<HostForm>) => void, isAdd: boolean) => (
    <>
      {isAdd && ownerPick(form.ownerAgency, (id) => set({ ownerAgency: id, authCredentialId: "" }), "host record")}
      {isAdd && (
        <input value={form.hostname} onChange={(e) => set({ hostname: e.target.value })} placeholder="Hostname" style={smallInput({ width: 130 })} />
      )}
      <input value={form.address} onChange={(e) => set({ address: e.target.value })} placeholder="IP or FQDN" style={smallInput({ width: 130 })} />
      <input
        type="number"
        value={form.port}
        onChange={(e) => set({ port: Number(e.target.value) || 22 })}
        placeholder="Port"
        style={smallInput({ width: 60 })}
      />
      <select value={form.os} onChange={(e) => set({ os: e.target.value as HostForm["os"] })} style={smallInput({ cursor: "pointer" })}>
        <option value="Linux">Linux</option>
        <option value="Windows">Windows</option>
      </select>
      <select value={form.via} onChange={(e) => set({ via: e.target.value })} style={smallInput({ cursor: "pointer" })}>
        {viaOptions.map((o) => (
          <option key={o.v} value={o.v}>
            {o.l}
          </option>
        ))}
      </select>
      <KeyPicker credentialId={form.authCredentialId} keyEnvVar={form.authKeyEnvVar} set={set} creds={keysFor(form.ownerAgency, form.authCredentialId)} />
      <input value={form.user} onChange={(e) => set({ user: e.target.value })} placeholder="user" style={smallInput({ width: 80 })} />
    </>
  );

  const bastionFields = (form: BastionForm, set: (p: Partial<BastionForm>) => void, isAdd: boolean) => (
    <>
      {isAdd && ownerPick(form.ownerAgency, (id) => set({ ownerAgency: id, authCredentialId: "" }), "bastion")}
      {isAdd && (
        <input value={form.name} onChange={(e) => set({ name: e.target.value })} placeholder="Name (e.g. bastion-prod)" style={smallInput({ width: 150 })} />
      )}
      <input value={form.address} onChange={(e) => set({ address: e.target.value })} placeholder="Address" style={smallInput({ width: 130 })} />
      <input
        type="number"
        value={form.port}
        onChange={(e) => set({ port: Number(e.target.value) || 22 })}
        placeholder="Port"
        style={smallInput({ width: 60 })}
      />
      <input value={form.username} onChange={(e) => set({ username: e.target.value })} placeholder="User" style={smallInput({ width: 80 })} />
      <KeyPicker credentialId={form.authCredentialId} keyEnvVar={form.authKeyEnvVar} set={set} creds={keysFor(form.ownerAgency, form.authCredentialId)} />
      <input value={form.zone} onChange={(e) => set({ zone: e.target.value })} placeholder="Zone" style={smallInput({ width: 90 })} />
    </>
  );

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      {/* Summary tiles */}
      <div style={{ display: "flex", gap: 10, flexWrap: "wrap" }}>
        {[
          { l: "Total", v: hosts.length, color: c.text },
          { l: "Verified", v: verified, color: c.success },
          { l: "Reachable", v: reachable, color: c.success },
          { l: "Errors", v: errored, color: c.danger },
          { l: "Unverified", v: unverified, color: c.textSec },
        ].map((s) => (
          <div
            key={s.l}
            style={{ flex: "1 1 90px", padding: "10px 14px", background: c.panel, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface }}
          >
            <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, color: c.textSec, fontWeight: 600, textTransform: "uppercase", letterSpacing: 0.7, marginBottom: 3 }}>
              {s.l}
            </div>
            <div style={{ fontSize: c.fontTitle, fontWeight: 700, color: s.color }}>{s.v}</div>
          </div>
        ))}
      </div>

      {actionErr && <div style={{ color: c.danger, fontSize: c.fontSm }}>Error: {actionErr}</div>}
      {actionOk && <div style={{ color: c.success, fontSize: c.fontSm }}>✓ {actionOk}</div>}

      {/* Host registry */}
      <Card
        title="Host Registry"
        noPad
        action={
          <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
            <input
              value={hostSearch}
              onChange={(e) => setHostSearch(e.target.value)}
              placeholder="Search hosts…"
              style={smallInput({ width: 160 })}
            />
            <ColumnsMenu cols={hostCols} cw={hostCw} />
            <Btn
              primary
              disabled={!!addWhy}
              title={addWhy || undefined}
              onClick={() => {
                setShowAddHost((s) => !s);
                setActionErr(null);
              }}
            >
              {showAddHost ? "Cancel" : "+ Add Host"}
            </Btn>
          </div>
        }
      >
        {showAddHost && (
          <div style={{ padding: 14, borderBottom: `1px solid ${c.border}`, display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center", background: `${c.primary}0d` }}>
            {hostFields(addHostForm, (p) => setAddHostForm((f) => ({ ...f, ...p })), true)}
            <Btn primary onClick={createHost} disabled={busy}>
              Add
            </Btn>
          </div>
        )}
        {hostsQ.loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
        {hostsQ.error && <div style={{ color: c.danger, padding: 16 }}>Error: {hostsQ.error}</div>}
        {!hostsQ.loading && !hostsQ.error && filteredHosts.length === 0 && (
          <div style={{ color: c.textSec, padding: 16 }}>
            {hosts.length === 0
              ? "No hosts registered yet. Choose + Add Host above to register the first one."
              : `No hosts match “${hostSearch}”. Clear the search to see all ${hosts.length}.`}
          </div>
        )}
        {!hostsQ.loading && !hostsQ.error && filteredHosts.length > 0 && (
          <div style={{ overflowX: "auto" }}>
            <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm, tableLayout: "auto", minWidth: 1170 }}>
              <thead>
                <TableHead columns={hostCols.visible} sort={hostSort} cw={hostCw} />
              </thead>
              <tbody>
                {hostSort.sorted.map((h) => {
                  // isGit/keyCred moved INTO their column cells (CO-4).
                  const editing = editHostId === (h.id ?? "");
                  return (
                    <Fragment key={h.id ?? h.hostname}>
                      <tr style={{ borderBottom: `1px solid ${c.border}`, background: editing ? `${c.primary}0d` : "transparent" }}>
                        {renderCells(hostCols.visible, h, {})}
                      </tr>
                      {editing && (
                        <tr style={{ borderBottom: `1px solid ${c.border}` }}>
                          <td colSpan={hostCols.visible.length} style={{ padding: "10px 14px", background: `${c.primary}0d` }}>
                            <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}>
                              {hostFields(editHostForm, (p) => setEditHostForm((f) => ({ ...f, ...p })), false)}
                            </div>
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {/* Bastions */}
      <Card
        title="Bastion Configuration"
        noPad
        action={
          <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
            <ColumnsMenu cols={bastionCols} cw={bastionCw} />
            <Btn
              primary
              disabled={!!addWhy}
              title={addWhy || undefined}
              onClick={() => {
                setShowAddBastion((s) => !s);
                setActionErr(null);
              }}
            >
              {showAddBastion ? "Cancel" : "+ Add Bastion"}
            </Btn>
          </div>
        }
      >
        {showAddBastion && (
          <div style={{ padding: 14, borderBottom: `1px solid ${c.border}`, display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center", background: `${c.primary}0d` }}>
            {bastionFields(addBastionForm, (p) => setAddBastionForm((f) => ({ ...f, ...p })), true)}
            <Btn primary onClick={createBastion} disabled={busy}>
              Add
            </Btn>
          </div>
        )}
        {bastionsQ.loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
        {bastionsQ.error && <div style={{ color: c.danger, padding: 16 }}>Error: {bastionsQ.error}</div>}
        {!bastionsQ.loading && !bastionsQ.error && bastions.length === 0 && (
          <div style={{ color: c.textSec, padding: 16 }}>
            No bastions configured, so hosts connect directly. Choose + Add Bastion above to route them through a jump
            host instead.
          </div>
        )}
        {!bastionsQ.loading && !bastionsQ.error && bastions.length > 0 && (
          <div style={{ overflowX: "auto" }}>
            <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm, tableLayout: "auto", minWidth: 1150 }}>
              <thead>
                <TableHead columns={bastionCols.visible} sort={bastionSort} cw={bastionCw} />
              </thead>
              <tbody>
                {bastionSort.sorted.map((b) => {
                  const editing = editBastionId === (b.id ?? "");
                  return (
                    <Fragment key={b.id ?? b.name}>
                      <tr style={{ borderBottom: `1px solid ${c.border}`, background: editing ? `${c.primary}0d` : "transparent" }}>
                        {renderCells(bastionCols.visible, b, {})}
                      </tr>
                      {editing && (
                        <tr style={{ borderBottom: `1px solid ${c.border}` }}>
                          <td colSpan={bastionCols.visible.length} style={{ padding: "10px 14px", background: `${c.primary}0d` }}>
                            <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}>
                              {bastionFields(editBastionForm, (p) => setEditBastionForm((f) => ({ ...f, ...p })), false)}
                            </div>
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </div>
  );
}

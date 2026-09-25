import { Fragment, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api, csrfHeader, errMsg, fetchCapabilities, fetchVersion, type BuildInfo } from "../api/client";
import { useGet, rows, paged, useColumnWidths, useTableSort, useToast } from "../hooks";
import { ColumnsMenu, TableHead, renderCells, useTableColumns } from "../components/table";
import { RefreshScope } from "../components/RefreshScope";
import { AlertBanner, Badge, Btn, ConfirmDialog, CopyButton, CopyText, EmptyCell, Field, HoverTr, InlineLoading, Modal, RefreshButton, Rule, Section, SkeletonRows, StatTile, TableSurface, TagEditor, Toast, SortableLabel, TypeBadge, statusLabel, usePager } from "../components/ui";
import { RecentRuns, DurationTrend } from "../components/RecentRuns";
import { TraceId } from "./history/shared";
import type { components } from "../api/schema";
import { c } from "../theme";
import { clampPage, sliceForPage } from "../utils/pager";
import { type SortColumn } from "../utils/sort";
import { fmtInAppZone, fmtDuration } from "../utils/datetime";
import { scrollIntoViewRespectingMotion } from "../utils/motion";
import {
  RUNNER_INSTALL_SCRIPT_PATH,
  installOneLiner,
  installTwoStep,
  personalizedInstallOneLiner,
  personalizedInstallTwoStep,
} from "./runner-install-cmd";
import { AGENT_BIN_PATH, upgradeCommand } from "./runner-upgrade-cmd";
import {
  ENV_EXAMPLE_PATH,
  RUN_TYPES,
  FAT_RUN_TYPES,
  generateRunnerEnv,
  provisionOneLiner,
  provisionDockerRun,
  type ProvisionOptions,
} from "./runner-provision";

// Ported from the prototype's RunnersSection (cronomicon-settings.jsx).
// GET /runners, GET|POST /runners/registration-tokens (single-use, Phase 7),
// DELETE /runners/registration-tokens/{id}, POST /runners/{id}/drain,
// POST /runners/{id}/resync, DELETE /runners/{id}.

interface Runner {
  // Runner ids are UUIDv7 TEXT keys (runners.id) — a string, despite the earlier
  // openapi mislabel that typed them integer. Fixed spec-side in v0.47.20.
  id?: string;
  name: string;
  status?: "online" | "offline" | "degraded" | "draining" | string;
  os?: string;
  capabilities?: string[];
  inventory?: "cronomicon" | "local" | string;
  load?: number;
  maxConcurrent?: number;
  version?: string;
  // Wire-protocol version declared at the last (re-)registration. Display
  // only: the server's floor tracks the current protocol, so every registered
  // runner speaks it (DD Phase B) — no per-action gate is needed here.
  protocolVersion?: number;
  lastHeartbeatAt?: string | null;
  registeredAt?: string;
  agencies?: { id: string; name: string }[]; // network-isolation membership (M2)
  // DR-7 — a prior placement offered for confirmation, present ONLY on a runner
  // in no agency whose name matches a retained snapshot. NEVER auto-applied: the
  // name is self-declared by the agent, so healing on it alone would let any
  // agent inherit another runner's placement by claiming its name.
  placementSuggestion?: {
    historyId: number;
    previousRunnerId: string;
    agencies: { id: string; name: string }[];
    tags: string[];
    deregisteredAt: string;
    deregisteredVia: "operator" | "reaper";
    previousClientIp?: string | null;
    currentClientIp?: string | null;
    clientIpMatches: boolean;
  } | null;
  // ID of the registration token that created this runner (provenance, A6.1);
  // cross-links to its row in the token table below. null for legacy runners.
  registrationTokenId?: string | null;
  // Operator-authored tags (migration 580); SQLite-only, edited inline from the
  // expanded row. [] when none.
  tags?: string[];
  // Detected-toolchain detail (RX.7); display-only. null for a pre-Phase-4 agent.
  toolchains?: {
    ansibleCore?: string;
    collections?: Record<string, string>;
    checkout?: boolean;
    vault?: boolean;
    sandboxed?: boolean;
    // Credential NAMES the runner can resolve to a local key (R6/Phase 4);
    // display-only, names only. Absent until a post-Phase-4 agent re-declares.
    keyNames?: string[];
  } | null;
  // Server-managed operational overrides (Phase 4). settingsVersion >
  // settingsAckedVersion ⇒ a change is still propagating to the agent.
  managedSettings?: ManagedSettings | null;
  settingsVersion?: number;
  settingsAckedVersion?: number;
  // Operator trust flag (migration 600, P1.4/P1.8): when true this runner may claim
  // runs that inject reference bindings and receive resolved secret VALUES in its
  // manifest. Defaults false; set only via the operator settings API.
  allowSecretInjection?: boolean;
}

// RunTypeName is the closed run-type vocabulary (matches the openapi RunType).
type RunTypeName = (typeof RUN_TYPES)[number];

// ManagedSettings is the tri-state override set edited in the Settings drawer.
// A field present overrides the runner's local value; absent = no server opinion.
interface ManagedSettings {
  maxConcurrent?: number;
  sandboxMemoryMax?: string;
  sandboxCpuQuota?: string;
  sandboxTasksMax?: string;
  allowCheckout?: boolean;
  checkoutRepos?: string[];
  capabilityMask?: RunTypeName[];
}

// capabilityHint explains a NON-run-type capability token (RA-20d). Run types
// (bash, ansible, …) are self-explanatory; the requirement tokens are not, and they
// are exactly the ones that decide whether a departmental run can be claimed at all.
// A chip an operator cannot interpret is a chip that does not help them fix the
// queued run that sent them here.
function capabilityHint(cap: string): string | undefined {
  if (cap.startsWith("collection:")) {
    return `Ansible collection ${cap.slice("collection:".length)} is installed — a job requiring it can be claimed here.`;
  }
  switch (cap) {
    case "become-file":
      return "Supports --become-password-file (ansible-core ≥ 2.12), so this runner can claim a job that names a become password. A runner without it makes those runs wait rather than run without escalation.";
    case "vault":
      return "A vault password file is configured, so this runner can claim a job declaring requires: [vault].";
    case "checkout":
      return "Checkout projects are enabled (-allow-checkout), so this runner can materialize a playbook repo at a pinned commit.";
    default:
      return undefined;
  }
}

// toolchainSummary renders the compact detail line under a runner's capability
// badges (ansible-core version, collection count, checkout/vault flags).
function toolchainSummary(tc: Runner["toolchains"]): string {
  if (!tc) return "";
  const parts: string[] = [];
  if (tc.ansibleCore) parts.push(`ansible-core ${tc.ansibleCore}`);
  const n = tc.collections ? Object.keys(tc.collections).length : 0;
  if (n > 0) parts.push(`${n} collection${n === 1 ? "" : "s"}`);
  if (tc.checkout) parts.push("checkout");
  if (tc.vault) parts.push("vault");
  if (tc.checkout) parts.push(tc.sandboxed ? "sandboxed" : "unsandboxed");
  return parts.join(" · ");
}

type Run = components["schemas"]["Run"];

// A pill for the health-at-a-glance line. `tone` picks the accent colour.
// Chip shape, not the round status pill (VU-17): three of its four uses carry a
// measurement (heartbeat, load, sandbox mode), and the runner's actual status
// pill is the one in the table row.
function HealthPill({ label, value, tone }: { label: string; value: string; tone: string }) {
  return (
    <span style={{ display: "inline-flex", alignItems: "center", gap: 6, padding: "4px 10px", borderRadius: c.radiusChip, fontSize: c.fontXs, fontWeight: 600, background: `${tone}1f`, color: tone }}>
      <span style={{ color: c.textMuted, fontFamily: c.sansCond, fontWeight: 600, textTransform: "uppercase", letterSpacing: 0.7, fontSize: c.fontXs }}>{label}</span>
      {value}
    </span>
  );
}

// PlacementOffer surfaces a prior placement for an operator to confirm (DR-7).
//
// Deliberately NOT pre-selected and NOT auto-applied. The name this match is
// keyed on is self-declared by the agent at registration, so a silent heal would
// turn an enrollment credential into a placement credential. The operator is the
// control, which only works if they can see what the match actually rests on —
// hence every signal is shown, labelled observed or self-declared.
function PlacementOffer({ runner, onSaved }: { runner: Runner; onSaved: () => void }) {
  const sugg = runner.placementSuggestion;
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  if (!sugg) return null;

  const restore = async () => {
    setBusy(true);
    setErr(null);
    const { error } = await api.POST("/runners/{runnerId}/placement", {
      params: { path: { runnerId: runner.id ?? "" } },
      body: { historyId: sugg.historyId },
    });
    setBusy(false);
    if (error) {
      setErr(errMsg(error));
      return;
    }
    onSaved();
  };

  // DRF-3: Dismiss is a server-side decision on the SNAPSHOT — it withdraws the
  // offer from every runner and every session, not just this render. The old
  // useState version came back on every reload, which is the word without the
  // behaviour.
  const dismiss = async () => {
    setBusy(true);
    setErr(null);
    const { error } = await api.POST("/runners/{runnerId}/placement/dismiss", {
      params: { path: { runnerId: runner.id ?? "" } },
      body: { historyId: sugg.historyId },
    });
    setBusy(false);
    if (error) {
      setErr(errMsg(error));
      return;
    }
    onSaved();
  };

  const when = sugg.deregisteredAt ? fmtInAppZone(sugg.deregisteredAt) : "";
  return (
    <div style={{ background: c.panel, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, padding: 12, marginBottom: 10 }}>
      <div style={{ fontFamily: c.sansCond, textTransform: "uppercase", fontSize: c.fontXs, letterSpacing: 0.5, color: c.textSec, marginBottom: 6 }}>
        Previous placement found
      </div>
      <p style={{ margin: "0 0 8px", fontSize: c.fontSm, color: c.text }}>
        A runner named <strong>{runner.name}</strong> was removed {when ? <>on {when}</> : "previously"}
        {sugg.deregisteredVia === "reaper" ? " by the offline sweep" : " by an operator"} while placed in{" "}
        {sugg.agencies.map((a) => a.name).join(", ")}
        {sugg.tags.length > 0 ? <> with tags {sugg.tags.join(", ")}</> : null}.
      </p>
      <div style={{ fontSize: c.fontXs, color: c.textSec, marginBottom: 8, lineHeight: 1.6 }}>
        <div>
          Name <code style={{ fontFamily: c.mono }}>{runner.name}</code> — matched,{" "}
          <em>self-declared by the agent</em>
        </div>
        <div>
          Source address{" "}
          {sugg.clientIpMatches ? (
            <>
              <code style={{ fontFamily: c.mono }}>{sugg.currentClientIp}</code> — matched, <em>observed</em>
            </>
          ) : (
            <>
              differs: was <code style={{ fontFamily: c.mono }}>{sugg.previousClientIp ?? "unknown"}</code>, now{" "}
              <code style={{ fontFamily: c.mono }}>{sugg.currentClientIp ?? "unknown"}</code> — <em>observed</em>
            </>
          )}
        </div>
      </div>
      {!sugg.clientIpMatches && (
        <p style={{ margin: "0 0 8px", fontSize: c.fontXs, color: c.warning }}>
          Only the self-declared name matches. Confirm this is the same machine before restoring.
        </p>
      )}
      {err && <p style={{ margin: "0 0 8px", fontSize: c.fontXs, color: c.danger }}>{err}</p>}
      <div style={{ display: "flex", gap: 8 }}>
        <Btn small onClick={restore} disabled={busy}>
          {busy ? "Restoring…" : "Restore placement"}
        </Btn>
        <Btn small onClick={dismiss} disabled={busy}>
          Dismiss
        </Btn>
      </div>
    </div>
  );
}

// GroupEditor edits a runner's agency ("group") membership inline (0..many),
// relocated from the former standalone Agency Membership matrix. Each add/remove
// commits immediately via PUT /runner-agencies with a SINGLE-runner body — the
// endpoint replaces only the posted runners' sets, so this never disturbs any
// other runner. Optimistic: reverts on error.
function GroupEditor({ runner, available, onSaved }: { runner: Runner; available: { id: string; name: string }[]; onSaved: () => void }) {
  const [ids, setIds] = useState<string[]>((runner.agencies ?? []).map((a) => a.id));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  // Re-seed from the server's view whenever the list refetches (post-save).
  useEffect(() => {
    setIds((runner.agencies ?? []).map((a) => a.id));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [JSON.stringify((runner.agencies ?? []).map((a) => a.id))]);

  const nameOf = (id: string) => available.find((a) => a.id === id)?.name ?? id;
  const unassigned = available.filter((a) => !ids.includes(a.id));

  const commit = async (next: string[]) => {
    const prev = ids;
    setIds(next); // optimistic
    setBusy(true);
    setErr(null);
    const { error } = await api.PUT("/runner-agencies", {
      params: { header: csrfHeader },
      body: [{ runnerId: String(runner.id), agencyIds: next }],
    });
    setBusy(false);
    if (error) {
      setIds(prev);
      setErr(errMsg(error));
    } else {
      onSaved();
    }
  };

  if (available.length === 0) {
    // VU-14 — the pointer was stale: the Agencies catalog moved off Env Vars onto
    // the Scopes page (SC2), so the old copy named a tab that no longer exists.
    return (
      <span style={{ display: "inline-flex", alignItems: "center", gap: 10, flexWrap: "wrap", color: c.textMuted }}>
        No groups defined yet — a runner with no group serves the general pool.
        <Link to="/scopes?tab=agencies" style={{ textDecoration: "none" }}>
          <Btn small>Manage agencies</Btn>
        </Link>
      </span>
    );
  }
  return (
    <div>
      <div style={{ display: "flex", gap: 6, flexWrap: "wrap", alignItems: "center" }}>
        {ids.length === 0 && <span style={{ color: c.textMuted }}>General pool (no group binding)</span>}
        {ids.map((id) => (
          <span key={id} style={{ display: "inline-flex", alignItems: "center", gap: 5, padding: "3px 8px", borderRadius: c.radiusChip, fontSize: c.fontXs, fontWeight: 600, background: `${c.primary}1f`, color: c.primary, border: `1px solid ${c.primary}30` }}>
            {nameOf(id)}
            <button
              onClick={() => commit(ids.filter((x) => x !== id))}
              disabled={busy}
              title="Remove from group"
              style={{ background: "none", border: "none", padding: 0, margin: 0, color: c.primary, cursor: busy ? "default" : "pointer", fontSize: c.fontSm, lineHeight: 1 }}
            >
              ×
            </button>
          </span>
        ))}
        {unassigned.length > 0 && (
          <select
            value=""
            disabled={busy}
            onChange={(e) => { if (e.target.value) void commit([...ids, e.target.value]); }}
            style={{ fontSize: c.fontSm, padding: "3px 6px", borderRadius: c.radiusChip, background: c.panelInput, color: c.text, border: `1px solid ${c.borderStrong}`, cursor: "pointer" }}
          >
            <option value="">+ Add group…</option>
            {unassigned.map((a) => (
              <option key={a.id} value={a.id}>{a.name}</option>
            ))}
          </select>
        )}
      </div>
      {err && <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 4 }}>Save failed: {err}</div>}
    </div>
  );
}

// RunnerTagsEditor edits a runner's operator-authored tags inline (like the Jobs
// tag editor), committing each change via PUT /runner-tags/{id}. Optimistic;
// reverts on error (e.g. a server-side cap 422).
function RunnerTagsEditor({ runner, onSaved }: { runner: Runner; onSaved: () => void }) {
  const [tags, setTags] = useState<string[]>(runner.tags ?? []);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    setTags(runner.tags ?? []);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [JSON.stringify(runner.tags ?? [])]);

  const save = async (next: string[]) => {
    const prev = tags;
    setTags(next); // optimistic
    setErr(null);
    const { error } = await api.PUT("/runner-tags/{runnerId}", {
      params: { path: { runnerId: String(runner.id) }, header: csrfHeader },
      body: { tags: next },
    });
    if (error) {
      setTags(prev);
      setErr(errMsg(error));
    } else {
      onSaved();
    }
  };

  return (
    <div>
      <TagEditor tags={tags} onChange={save} />
      {err && <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 4 }}>Save failed: {err}</div>}
    </div>
  );
}

// SecretInjectionEditor toggles a runner's secret-injection trust flag (P1.8,
// PUT /runners/{id}/secret-injection). The "on" state is warning-toned because it
// widens the runner's blast radius (it receives decrypted secret values). Disabled
// (and explained via title) when the caller lacks ConfigureApp.
function SecretInjectionEditor({ runner, canConfig, onSaved }: { runner: Runner; canConfig: boolean; onSaved: () => void }) {
  const [on, setOn] = useState<boolean>(!!runner.allowSecretInjection);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    setOn(!!runner.allowSecretInjection);
  }, [runner.allowSecretInjection]);

  const toggle = async () => {
    if (!canConfig || busy || runner.id == null) return;
    const prev = on;
    const next = !on;
    setOn(next); // optimistic
    setErr(null);
    setBusy(true);
    const { error } = await api.PUT("/runners/{id}/secret-injection", {
      params: { path: { id: String(runner.id) }, header: csrfHeader },
      body: { allow: next },
    });
    setBusy(false);
    if (error) {
      setOn(prev);
      setErr(errMsg(error));
    } else {
      onSaved();
    }
  };

  return (
    <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
      <div
        onClick={toggle}
        title={canConfig ? undefined : "Requires the Configure App permission"}
        style={{
          width: 36,
          height: 20,
          // Geometry, not chip vocabulary (VU-6): the track has to stay fully
          // round to match the 50% knob riding inside it.
          borderRadius: c.radiusPill,
          background: on ? c.warning : c.border,
          padding: 2,
          cursor: canConfig ? "pointer" : "not-allowed",
          opacity: canConfig ? 1 : 0.5,
          transition: "background 0.2s",
          flexShrink: 0,
        }}
      >
        <div
          style={{
            width: 16,
            height: 16,
            borderRadius: "50%",
            background: "#fff",
            transform: `translateX(${on ? 16 : 0}px)`,
            transition: "transform 0.2s",
          }}
        />
      </div>
      <span style={{ fontSize: c.fontSm, color: on ? c.warning : c.textSec, fontWeight: 600 }}>{on ? "Enabled" : "Disabled"}</span>
      {err && <span style={{ fontSize: c.fontXs, color: c.danger }}>Save failed: {err}</span>}
    </div>
  );
}

// RunnerDetail is the expanded-row body (Phase 3): health-at-a-glance, a field
// grid, registration provenance (cross-linked to the creating token), a
// read-only managed-settings summary, an agent version/upgrade hint, the
// currently-running list, reliability mini-stats, and the recent-jobs history.
// It fetches the runner's recent job runs on mount (i.e. on expand).
function RunnerDetail({
  runner,
  serverVersion,
  availableAgencies,
  canConfig,
  actions,
  onEditSettings,
  onScrollToToken,
  onSaved,
}: {
  runner: Runner;
  serverVersion?: string | null;
  availableAgencies: { id: string; name: string }[];
  canConfig: boolean;
  actions?: React.ReactNode;
  onEditSettings: () => void;
  onScrollToToken: (tokenId: string) => void;
  onSaved: () => void;
}) {
  // Server-side paging (PP-H7 pattern, as History › Executions): fetch only the
  // current page so the runner's full job history stays reachable past the former
  // 20-row cap.
  const runsPager = usePager();
  const runsQ = useGet<unknown>(
    () => api.GET("/runs", { params: { query: { runnerId: runner.id!, kind: "job", page: runsPager.page + 1, pageSize: runsPager.pageSize } } }),
    [runner.id, runsPager.page, runsPager.pageSize],
  );
  const runsPg = paged<Run>(runsQ.data);
  const runs = runsPg.items;
  // Reliability, the trend and "Currently running" describe the MOST RECENT runs
  // — pin them to page 1's rows while the operator browses older pages, rather
  // than silently recomputing "recent" over page N.
  const [recent, setRecent] = useState<Run[]>([]);
  useEffect(() => {
    if (runsPager.page === 0 && !runsQ.loading) setRecent(runs);
    // runs is derived from runsQ.data; the data object is its stable identity.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runsQ.data, runsQ.loading, runsPager.page]);

  // Reliability/throughput over the most recent page of job runs.
  const terminal = recent.filter((r) => r.status !== "running" && r.status !== "queued");
  const successes = terminal.filter((r) => r.status === "success").length;
  const successRate = terminal.length ? Math.round((successes / terminal.length) * 100) : null;
  const durs = terminal.filter((r) => r.durationMs != null).map((r) => r.durationMs as number);
  const avgDur = durs.length ? Math.round(durs.reduce((a, b) => a + b, 0) / durs.length) : null;
  const inflight = recent.filter((r) => r.status === "running" || r.status === "queued");

  const tc = runner.toolchains;
  const load = runner.load ?? 0;
  const max = runner.maxConcurrent ?? 0;
  const sc = statusColor(runner.status);
  const sandboxed = tc == null || tc.sandboxed == null ? null : tc.sandboxed;
  const ms = runner.managedSettings ?? null;
  const pendingAck = (runner.settingsVersion ?? 0) > (runner.settingsAckedVersion ?? 0);
  const behind = !!(runner.version && serverVersion && runner.version !== serverVersion);
  const caps = runner.capabilities ?? [];
  const keyNames = tc?.keyNames ?? [];

  // Overview facts, populated-only (R5): a label over "—" costs the same
  // attention as a real fact, so empties collapse into one muted line below the
  // grid instead of holding grid cells. Toolchains and status/load are absent on
  // purpose — the compact row directly above already shows them (R4); the health
  // pills own status/load.
  const upgradeHow = `Run "Copy upgrade command" (top right) as root on ${runner.name}. It downloads this server's agent binary for the host's architecture, verifies it against SHA256SUMS, swaps ${AGENT_BIN_PATH} (keeping the old one at .prev) and restarts the service. Registration, id and config are untouched — a checksum mismatch aborts before anything is replaced.`;
  const overview: { label: string; value: React.ReactNode; mono?: boolean; wide?: boolean; present: boolean }[] = [
    { label: "Runner ID", mono: true, wide: true, present: !!runner.id, value: runner.id ? <CopyText text={runner.id} /> : null },
    { label: "OS", present: !!runner.os, value: runner.os },
    { label: "Inventory", present: true, value: runner.inventory ?? "cronomicon" },
    {
      label: "Version",
      present: !!runner.version,
      value: (
        <span style={{ display: "inline-flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
          <span style={{ fontFamily: c.mono }}>v{runner.version}</span>
          {behind ? (
            <span
              title={upgradeHow}
              style={{ display: "inline-flex", padding: "2px 8px", borderRadius: c.radiusChip, fontSize: c.fontXs, fontWeight: 700, background: `${c.warning}1f`, color: c.warning, border: `1px solid ${c.warning}40`, cursor: "help" }}
            >
              Upgrade available → v{serverVersion}
            </span>
          ) : runner.version && serverVersion ? (
            <span style={{ fontSize: c.fontXs, color: c.success }}>✓ up to date</span>
          ) : null}
        </span>
      ),
    },
    {
      label: "Key names",
      present: keyNames.length > 0,
      value: (
        <span
          title="Local credential names this runner can resolve to a private key (from its key-map / key-dir). Names only — never key material. A host's authKeyEnvVar must match one of these for this runner to authenticate."
          style={{ display: "inline-flex", gap: 4, flexWrap: "wrap", cursor: "help" }}
        >
          {keyNames.map((n) => (
            <span key={n} style={{ fontFamily: c.mono, fontSize: c.fontXs, padding: "1px 6px", borderRadius: c.radiusChip, background: c.panel2, color: c.textSec, border: `1px solid ${c.border}` }}>
              {n}
            </span>
          ))}
        </span>
      ),
    },
  ];
  const missing = overview.filter((f) => !f.present).map((f) => f.label);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 20, marginTop: 4, fontSize: c.fontSm }}>
      {/* Top strip (R6): health at a glance on the left, the per-runner action
          bar on the right — one consistent home for every action. */}
      <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}>
        <HealthPill label="Status" value={runner.status === "draining" ? `Draining (${load} active)` : (runner.status ?? "unknown")} tone={sc} />
        <HealthPill label="Heartbeat" value={timeAgo(runner.lastHeartbeatAt)} tone={runner.status === "offline" ? c.danger : c.textSec} />
        <HealthPill label="Load" value={`${load}/${max}`} tone={max > 0 && load >= max ? c.danger : c.textSec} />
        <HealthPill
          label="Sandbox"
          value={sandboxed == null ? "unknown" : sandboxed ? "Sandboxed" : "Unsandboxed"}
          tone={sandboxed == null ? c.textSec : sandboxed ? c.success : c.warning}
        />
        {/* EP-4 Shape A — Refresh joins the existing per-runner action bar and
            leads it, so the non-destructive control is not beside Deregister.
            The span is unconditional now (Refresh is always offered) where the
            actions themselves stay gated. */}
        <span style={{ marginLeft: "auto", display: "inline-flex", gap: 6, flexWrap: "wrap" }}>
          <RefreshButton />
          {actions}
        </span>
      </div>

      {/* R2 — two columns at wide viewports: configuration (left), activity
          (right). Flex-basis wrap collapses this to one column when narrow. */}
      <div style={{ display: "flex", gap: 36, flexWrap: "wrap", alignItems: "flex-start" }}>
        <div style={{ flex: "2 1 420px", minWidth: 0, display: "flex", flexDirection: "column", gap: 22 }}>
          {/* Overview facts — populated fields only (R5), in an aligned grid so
              labels form scanning columns. FX-14: Protocol and Registered stay in
              the Registration section, the one place that carries them. */}
          <div>
            <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(170px, 1fr))", gap: "14px 24px" }}>
              {overview.filter((f) => f.present).map((f) => (
                <div key={f.label} style={f.wide ? { gridColumn: "span 2" } : undefined}>
                  <Field label={f.label} value={f.value} mono={f.mono} />
                </div>
              ))}
            </div>
            {missing.length > 0 && (
              <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 10 }}>Not recorded: {missing.join(" · ")}</div>
            )}
          </div>

          {/* Registration provenance */}
          <Section title="Registration">
            <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(170px, 1fr))", gap: "14px 24px" }}>
              <Field label="Registered at" value={runner.registeredAt ? fmtDate(runner.registeredAt) : "—"} mono />
              <Field label="Protocol version" value={runner.protocolVersion != null ? `v${runner.protocolVersion}` : "—"} mono />
              <Field
                label="Created by token"
                value={
                  runner.registrationTokenId ? (
                    <button
                      onClick={() => onScrollToToken(runner.registrationTokenId!)}
                      style={{ background: "none", border: "none", padding: 0, color: c.primary, cursor: "pointer", fontSize: c.fontSm, textDecoration: "underline", fontFamily: c.mono }}
                      title="Jump to this token's row in the registration-token table"
                    >
                      Token #{runner.registrationTokenId}
                    </button>
                  ) : (
                    <span style={{ color: c.textMuted }}>— (pre-provenance runner)</span>
                  )
                }
              />
            </div>
          </Section>

          {/* Managed settings summary (read-only) + Edit. The former standalone
              "Agent version" section is folded into the overview's Version field
              (R4) — the upgrade chip carries the how-to as its tooltip. */}
          <Section
            title={
              <>
                Managed settings
                {pendingAck && <span title="A change is still propagating to the agent" style={{ marginLeft: 6, color: c.warning }}>● pending</span>}
              </>
            }
            info="Server-managed overrides pushed to the agent: max concurrent jobs, sandbox caps, checkout policy and capability mask. When none are set the runner uses its local config."
            actions={<Btn small onClick={onEditSettings}>⚙ Edit</Btn>}
          >
            {ms == null ? (
              <span style={{ color: c.textMuted }}>No server-managed overrides — the runner uses its local config.</span>
            ) : (
              <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(170px, 1fr))", gap: "14px 24px" }}>
                <Field label="Max concurrent" value={ms.maxConcurrent != null ? String(ms.maxConcurrent) : "—"} mono />
                <Field label="Capability mask" value={ms.capabilityMask && ms.capabilityMask.length ? ms.capabilityMask.join(", ") : "—"} />
                <Field
                  label="Sandbox caps"
                  value={[ms.sandboxMemoryMax && `mem ${ms.sandboxMemoryMax}`, ms.sandboxCpuQuota && `cpu ${ms.sandboxCpuQuota}`, ms.sandboxTasksMax && `tasks ${ms.sandboxTasksMax}`].filter(Boolean).join(" · ") || "—"}
                  mono
                />
                <Field
                  label="Checkout policy"
                  value={ms.allowCheckout == null ? "—" : ms.allowCheckout ? (ms.checkoutRepos && ms.checkoutRepos.length ? `allowed (${ms.checkoutRepos.length} repo${ms.checkoutRepos.length === 1 ? "" : "s"})` : "allowed") : "denied"}
                />
              </div>
            )}
          </Section>

          {/* Full capabilities (incl. collection: tokens hidden in the compact row) */}
          <Section title={`Capabilities (${caps.length})`}>
            {caps.length === 0 ? (
              <span style={{ color: c.textMuted }}>None declared</span>
            ) : (
              <div style={{ display: "flex", gap: 4, flexWrap: "wrap" }}>
                {caps.map((cap) => (
                  <TypeBadge key={cap} type={cap} withLabel title={capabilityHint(cap)} />
                ))}
              </div>
            )}
          </Section>

          {/* Groups (agencies) — editable inline, 0..many (relocated from the
              former standalone Agency Membership matrix). */}
          <Section
            title="Groups"
            info={
              <>
                Groups are network-isolation <strong>agencies</strong> (managed on the Scopes page). A runner in one
                or more groups only claims runs targeting those groups; a runner in none serves the general pool.
              </>
            }
          >
            <PlacementOffer runner={runner} onSaved={onSaved} />
            <GroupEditor runner={runner} available={availableAgencies} onSaved={onSaved} />
          </Section>

          {/* Tags — operator-authored, editable inline like other catalog items */}
          <Section
            title={`Tags${(runner.tags ?? []).length ? ` (${(runner.tags ?? []).length})` : ""}`}
            info="Tags are operator-authored and stored in Cronomicon only — free-form labels for grouping and filtering runners."
          >
            <RunnerTagsEditor runner={runner} onSaved={onSaved} />
          </Section>

          {/* Secret injection — operator trust flag (P1.8). A flagged runner may
              claim reference-bearing runs and receives resolved secret VALUES in
              its manifest. Education lives behind the ⓘ; the consequence warning
              stays inline, but only while the flag is actually on. */}
          <Section
            title="Secret injection"
            info={
              <>
                When enabled, this runner may claim runs whose job or script declares reference bindings, receiving
                the resolved <strong>secret values</strong> in its manifest. Runs that inject references are never
                dispatched to a runner without this flag.
              </>
            }
          >
            <SecretInjectionEditor runner={runner} canConfig={canConfig} onSaved={onSaved} />
            {!!runner.allowSecretInjection && (
              <div style={{ fontSize: c.fontXs, color: c.warning, background: c.warningBg, border: `1px solid ${c.warning}30`, borderRadius: c.radiusSurface, padding: "8px 10px", marginTop: 8, maxWidth: "70ch" }}>
                ⚠ This runner receives resolved secret values in its manifests. Keep this enabled only for runners you
                trust with the secrets in the scopes it serves.
              </div>
            )}
          </Section>
        </div>

        {/* Activity column — what has this runner been doing. */}
        <div style={{ flex: "1.9 1 430px", minWidth: 0, display: "flex", flexDirection: "column", gap: 22 }}>
          {inflight.length > 0 && (
            <Section title={`Currently running (${inflight.length})`}>
              {/* R7 — hairline rows, the same frameless treatment as every other
                  list inside the detail (was: per-row tinted cards). */}
              <div style={{ display: "flex", flexDirection: "column" }}>
                {inflight.map((r, i) => (
                  <Fragment key={r.traceId}>
                    {i > 0 && <Rule />}
                    <div style={{ display: "flex", alignItems: "center", gap: 10, padding: "7px 2px", flexWrap: "wrap" }}>
                      <TraceId id={r.traceId} color={c.accent} />
                      {r.status && <Badge status={r.status} label={statusLabel(r.status)} />}
                      <span style={{ color: c.textSec }}>{r.jobName ?? "—"}</span>
                      <span style={{ color: c.textMuted, marginLeft: "auto" }}>started {timeAgo(r.startedAt)}</span>
                    </div>
                  </Fragment>
                ))}
              </div>
            </Section>
          )}

          {recent.length > 0 && (
            <Section title="Reliability">
              <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(120px, 1fr))", gap: "14px 24px", marginBottom: 12 }}>
                <Field label="Recent job runs" value={String(recent.length)} mono />
                <Field label="Success rate" value={successRate == null ? "—" : `${successRate}% (${successes}/${terminal.length})`} mono />
                <Field label="Avg duration" value={avgDur == null ? "—" : fmtDuration(avgDur)} mono />
              </div>
              <DurationTrend runs={recent} />
            </Section>
          )}

          <Section title="Recent jobs">
            <RecentRuns
              runs={runs}
              loading={runsQ.loading}
              error={runsQ.error}
              emptyText="This runner hasn't executed any jobs yet."
              pager={runsPager}
              page={runsPager.page}
              total={runsPg.totalItems}
            />
          </Section>
        </div>
      </div>
    </div>
  );
}

// Single-use registration token metadata (Phase 7). Plaintext is never in
// list responses — only the mint response carries it, once.
interface RegTokenInfo {
  id: number;
  label?: string | null;
  createdBy?: string;
  createdAt?: string;
  expiresAt?: string;
  revokedAt?: string | null;
  usedAt?: string | null;
  usedByRunnerId?: string | null;
  usedByRunnerName?: string | null;
  status?: "active" | "used" | "expired" | "revoked" | string;
}

interface MintedToken extends RegTokenInfo {
  token?: string;
}

// Registration-token table: sortable columns + client-side pagination
// (v0.47.18; comparators/toggle moved into useTableSort — TS-6,
// the sorting-update plan). Status sort order: live/actionable first
// (pending → active), then terminal (expired → revoked). Drives both the
// "Status" column sort and the default tiebreak chain.
const TOK_STATUS_RANK: Record<string, number> = { pending: 0, active: 1, expired: 2, revoked: 3 };
const TOK_PAGE_SIZE = 10;

const TOK_SORT_COLS: SortColumn<RegTokenInfo>[] = [
  { key: "status", get: (tk) => tk.status, type: "rank", rank: TOK_STATUS_RANK },
  { key: "usedBy", get: (tk) => tk.usedByRunnerName ?? tk.usedByRunnerId, type: "text" },
  { key: "label", get: (tk) => tk.label, type: "text" },
  { key: "createdAt", get: (tk) => tk.createdAt, type: "date" },
  { key: "expiresAt", get: (tk) => tk.expiresAt, type: "date" },
];

// Default column widths (px) for the resizable runner registry table (V1.1-7).
// Runner (multi-line name + version subtext) and Capabilities (flex-wrap badge
// list) are resizable and seeded wide so they don't crush; the rest are fixed.
const COL_W: Record<string, number> = {
  runner: 200,
  status: 130,
  os: 100,
  capabilities: 200,
  agencies: 150,
  load: 100,
  lastSeen: 180,
  // Compact row now carries only Test + Deregister (the rest moved into the
  // expanded detail's action strip, V1.1 / v0.47.20), so this can be narrower.
  actions: 160,
};

// Runner-registry sort (the sorting-update plan §3.2). Status ranks worst
// first when ascending (TS-Q5): offline < draining < degraded < online — a
// value outside the map (unknown status) sorts last automatically. Load sorts
// by the current load count exactly as displayed (`load ?? 0`); Capabilities
// and Agencies are multi-value and stay unsortable.
const RUNNER_STATUS_RANK: Record<string, number> = { offline: 0, draining: 1, degraded: 2, online: 3 };
const RUNNER_SORT_COLS: SortColumn<Runner>[] = [
  { key: "runner", get: (r) => r.name, type: "text" },
  { key: "status", get: (r) => r.status, type: "rank", rank: RUNNER_STATUS_RANK },
  { key: "os", get: (r) => r.os, type: "text" },
  { key: "load", get: (r) => r.load ?? 0, type: "number" },
  { key: "lastSeen", get: (r) => r.lastHeartbeatAt, type: "date" },
];

const statusColor = (status?: string): string => {
  switch (status) {
    case "online":
      return c.success;
    case "draining":
    case "degraded":
      return c.warning;
    default:
      return c.textSec; // offline / unknown — grey
  }
};

// FX-7 — "is there an agent on the other end to answer?", which is the actual
// precondition for Resync / Scan keys / Drain. `degraded` still heartbeats and
// still holds work, so it belongs here; `draining` is already on its way out and
// `offline` has nobody to ask, so for both of them these actions are irrelevant
// rather than merely unavailable — and irrelevance is the one case that hides.
const isReachable = (status?: string): boolean => status === "online" || status === "degraded";

function timeAgo(iso?: string | null): string {
  if (!iso) return "never";
  const ms = Date.now() - new Date(iso).getTime();
  if (Number.isNaN(ms)) return iso;
  const s = Math.floor(ms / 1000);
  if (s < 10) return "just now";
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

// Absolute app-zone datetime for registration-token expiry/issue stamps (§5.2).
// Distinct from the relative timeAgo above, which stays zone-agnostic.
function fmtDate(iso?: string): string {
  return fmtInAppZone(iso);
}

// RU1 — the upgrade command targets THIS server (the host already trusts it for
// registration and polling, and it is what publishes /agents/). A function, not a
// module const, so it is not evaluated at import time in a non-browser context.
function runnerOrigin(): string {
  return typeof window !== "undefined" ? window.location.origin : "https://cronomicon.example.com";
}


export function Runners() {
  const [refresh, setRefresh] = useState(0);
  const [tokenV, setTokenV] = useState(0);
  const [canConfig, setCanConfig] = useState(false);
  useEffect(() => {
    fetchCapabilities().then((caps) => setCanConfig(caps.configureApp));
  }, []);

  const list = useGet<unknown>(() => api.GET("/runners"), [refresh]);
  const runners = rows<Runner>(list.data);
  const cw = useColumnWidths("runners");
  // Registry sort (the sorting-update plan §3.2) — no filter/pager on this
  // table, so the sorted array feeds the render directly.
  const runnerSort = useTableSort(runners, RUNNER_SORT_COLS, { key: "runner", dir: "asc" }, { tableId: "runners" });

  // CO-4 — the column spec. Built in render: cells read `c.*` and close over
  // `expandedRunner`, `testingId`, `busy` and the row actions.
  const cols = useTableColumns<Runner>("runners", [
    {
      key: "runner",
      label: "Runner",
      sortKey: "runner",
      width: COL_W.runner,
      pin: "first",
      cell: (r) => (
        <>
          <div style={{ display: "flex", alignItems: "center", gap: 6 }}>
            <span style={{ fontWeight: 600 }}>{r.name}</span>
            {(r.inventory === "cronomicon" || r.inventory === "local") && <InventoryChip mode={r.inventory} />}
          </div>
          {r.version && <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 1 }}>v{r.version}</div>}
          {(r.tags ?? []).length > 0 && (
            <div style={{ display: "flex", gap: 3, flexWrap: "wrap", marginTop: 4 }}>
              {(r.tags ?? []).map((t) => (
                <span key={t} style={{ fontSize: c.fontXs, fontWeight: 600, padding: "1px 6px", borderRadius: c.radiusChip, background: `${c.info}1a`, color: c.info, border: `1px solid ${c.info}30` }}>
                  {t}
                </span>
              ))}
            </div>
          )}
        </>
      ),
    },
    {
      key: "status",
      label: "Status",
      sortKey: "status",
      width: COL_W.status,
      fixed: true,
      cell: (r) => {
        const sc = statusColor(r.status);
        return (
          <span
            style={{
              display: "inline-flex",
              alignItems: "center",
              gap: 5,
              padding: "3px 9px",
              borderRadius: c.radiusPill,
              fontSize: c.fontXs,
              fontWeight: 600,
              background: `${sc}1f`,
              color: sc,
            }}
          >
            <span style={{ width: 6, height: 6, borderRadius: "50%", background: sc }} />
            {r.status === "draining" ? `Draining (${r.load ?? 0} active)` : statusLabel(r.status)}
          </span>
        );
      },
    },
    { key: "os", label: "OS", sortKey: "os", width: COL_W.os, fixed: true, tdStyle: { color: c.textSec, fontSize: c.fontSm }, cell: (r) => r.os ?? <EmptyCell /> },
    {
      key: "capabilities",
      label: "Capabilities",
      width: COL_W.capabilities,
      cell: (r) => (
        <>
          <div style={{ display: "flex", gap: 4, flexWrap: "wrap" }}>
            {(r.capabilities ?? [])
              .filter((cap) => !cap.startsWith("collection:"))
              .map((cap) => (
                <TypeBadge key={cap} type={cap} withLabel title={capabilityHint(cap)} />
              ))}
          </div>
          {toolchainSummary(r.toolchains) && (
            <div style={{ color: c.textMuted, fontSize: c.fontXs, marginTop: 3 }}>{toolchainSummary(r.toolchains)}</div>
          )}
        </>
      ),
    },
    {
      key: "agencies",
      label: "Agencies",
      width: COL_W.agencies,
      fixed: true,
      cell: (r) =>
        (r.agencies ?? []).length === 0 ? (
          // RB-22 — say what the empty set MEANS. A runner in no agency serves
          // the GENERAL POOL: every department's unscoped and system work. That
          // is more reach than a departmental runner, and "—" reads as missing.
          <span
            title={
              r.placementSuggestion
                ? "In no agency — serving the general pool. A previous placement was found; expand the row to review and restore it."
                : "In no agency — serves the general pool, so it may claim every department's unscoped and system work."
            }
            style={{ color: c.textSec, fontSize: c.fontXs, fontStyle: "italic", cursor: "help" }}
          >
            general pool
            {r.placementSuggestion ? (
              <span style={{ color: c.warning, fontStyle: "normal", fontWeight: 600 }}> · was placed</span>
            ) : null}
          </span>
        ) : (
          <div style={{ display: "flex", gap: 4, flexWrap: "wrap" }}>
            {(r.agencies ?? []).map((a) => (
              <span
                key={a.id}
                style={{ display: "inline-flex", padding: "2px 7px", borderRadius: c.radiusChip, fontSize: c.fontXs, fontWeight: 600, background: `${c.primary}1f`, color: c.primary, border: `1px solid ${c.primary}20` }}
              >
                {a.name}
              </span>
            ))}
          </div>
        ),
    },
    {
      key: "load",
      label: "Load",
      sortKey: "load",
      width: COL_W.load,
      fixed: true,
      tdStyle: { textAlign: "right" },
      cell: (r) => {
        const load = r.load ?? 0;
        const max = r.maxConcurrent ?? 0;
        return (
          <span style={{ fontSize: c.fontSm }}>
            <span style={{ fontWeight: 600, color: max > 0 && load >= max ? c.danger : c.text }}>{load}</span>
            <span style={{ color: c.textSec }}>/{max}</span>
          </span>
        );
      },
    },
    {
      key: "lastSeen",
      label: "Last Seen",
      sortKey: "lastSeen",
      width: COL_W.lastSeen,
      fixed: true,
      tdStyle: { fontSize: c.fontSm, fontFamily: c.mono, whiteSpace: "nowrap" },
      cell: (r) => (
        <span
          title={r.lastHeartbeatAt ? timeAgo(r.lastHeartbeatAt) : "never"}
          style={{ color: r.status === "offline" ? c.danger : c.textSec }}
        >
          {r.lastHeartbeatAt ? fmtDate(r.lastHeartbeatAt) : "never"}
        </span>
      ),
    },
    {
      key: "actions",
      label: "",
      menuLabel: "Actions",
      width: COL_W.actions,
      fixed: true,
      pin: "last",
      cell: (r) => (
        // Compact row keeps only Test + Deregister; the rest moved into the
        // expanded detail (v0.47.20). stopPropagation so a button click doesn't
        // also toggle the row.
        <div onClick={(e) => e.stopPropagation()} style={{ display: "flex", gap: 4, justifyContent: "flex-end" }}>
          <Btn small onClick={() => testRunner(r)} disabled={testingId === r.id || busy}>
            {testingId === r.id ? "Testing…" : "Test"}
          </Btn>
          <Btn small dangerQuiet onClick={() => setDeregTarget(r)} disabled={busy}>
            Deregister
          </Btn>
        </div>
      ),
    },
  ]);

  // All defined agencies ("groups"), for the inline group editor in each
  // expanded runner row (replaces the standalone Agency Membership matrix).
  const agenciesQ = useGet<unknown>(() => api.GET("/agencies"), [refresh]);
  const availableAgencies = rows<{ id: string; name: string }>(agenciesQ.data);

  const tokList = useGet<unknown>(() => api.GET("/runners/registration-tokens"), [tokenV]);
  const regTokens = rows<RegTokenInfo>(tokList.data);

  // Token table sort + pagination (v0.47.18). Default: Status, then Created
  // (newest first), then Label. Each column header toggles/selects the sort; the
  // list pages 10 at a time client-side. A sort change snaps back to page 1.
  const [tokPage, setTokPage] = useState(0);
  const tokSort = useTableSort(regTokens, TOK_SORT_COLS, { key: "status", dir: "asc" }, {
    tableId: "runner-reg-tokens",
    tiebreak: [
      { key: "status", dir: "asc" },
      { key: "createdAt", dir: "desc" },
      { key: "label", dir: "asc" },
    ],
    onChange: () => setTokPage(0),
  });
  const sortedTokens = tokSort.sorted;
  const tokPageClamped = clampPage(tokPage, sortedTokens.length, TOK_PAGE_SIZE);
  const tokPageItems = sliceForPage(sortedTokens, tokPageClamped, TOK_PAGE_SIZE);

  const [tokenRevealed, setTokenRevealed] = useState(false);
  const [showTwoStep, setShowTwoStep] = useState(false);
  // Add Runner one-click flow (Phase 2): the headline path — mint, then paste
  // one baked-in line on the host.
  const [addOpen, setAddOpen] = useState(false);
  const [addShowTwoStep, setAddShowTwoStep] = useState(false);
  const [mintLabel, setMintLabel] = useState("");
  // The plaintext token is returned by the mint POST only once; the list never
  // carries it. Hold the freshly minted token locally so it stays copyable.
  const [minted, setMinted] = useState<MintedToken | null>(null);

  const [drainTarget, setDrainTarget] = useState<Runner | null>(null);
  const [resyncTarget, setResyncTarget] = useState<Runner | null>(null);
  const [deregTarget, setDeregTarget] = useState<Runner | null>(null);
  const [settingsTarget, setSettingsTarget] = useState<Runner | null>(null);
  const [scanTarget, setScanTarget] = useState<Runner | null>(null);
  const [busy, setBusy] = useState(false);
  const [testingId, setTestingId] = useState<string | null>(null);
  // Which runner row is expanded to its detail sub-row (keyed by the UUID id).
  const [expandedRunner, setExpandedRunner] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  // FX-16 — the shared toast + its timer, replacing a hand-rolled success
  // banner the operator had to dismiss by hand.
  const [actionOk, setActionOk] = useToast();

  // Server build version, for the per-runner agent upgrade hint (the server and
  // its bundled agent ship from the same release, so this is "latest available").
  const [serverBuild, setServerBuild] = useState<BuildInfo | null>(null);
  useEffect(() => {
    void fetchVersion().then(setServerBuild);
  }, []);

  // Scroll+flash the registration-token row a runner's provenance link points at.
  const scrollToToken = (tokenId: string) => {
    const el = document.getElementById(`regtoken-${tokenId}`);
    if (!el) return;
    // H-2: the one motion the stylesheet cannot reach. An explicit behavior
    // argument beats the scroll-behavior property, so a reduce-preferring viewer
    // would get the smooth scroll anyway; the helper asks first and jumps
    // instead. The row still ends up centred either way.
    scrollIntoViewRespectingMotion(el, { block: "center" });
    const prev = el.style.backgroundColor;
    el.style.transition = "background-color 0.4s";
    el.style.backgroundColor = c.primaryBg;
    setTimeout(() => {
      el.style.backgroundColor = prev;
    }, 1600);
  };

  const refetchList = () => setRefresh((n) => n + 1);

  // Always start the Add Runner flow at the mint step: clear any previously
  // minted token so the modal never opens onto a stale (possibly already
  // consumed — tokens are single-use) command. `minted` is shared with the
  // Register/Provision panels, so this also clears their revealed token, which
  // is the correct default for "add a NEW runner".
  const openAddRunner = () => {
    setMinted(null);
    setMintLabel("");
    setTokenRevealed(false);
    setAddShowTwoStep(false);
    setAddOpen(true);
  };

  const drain = async (r: Runner) => {
    if (r.id == null) return;
    setBusy(true);
    setActionError(null);
    const { error } = await api.POST("/runners/{runnerId}/drain", {
      params: { path: { runnerId: r.id }, header: csrfHeader },
    });
    setBusy(false);
    setDrainTarget(null);
    if (error) {
      setActionError(`Drain ${r.name}: ${errMsg(error)}`);
    } else {
      refetchList(); // shows draining immediately…
      setTimeout(refetchList, 5000); // ...and catches the draining → offline transition
    }
  };

  const resync = async (r: Runner) => {
    if (r.id == null) return;
    setBusy(true);
    setActionError(null);
    const { error } = await api.POST("/runners/{runnerId}/resync", {
      params: { path: { runnerId: r.id }, header: csrfHeader },
    });
    setBusy(false);
    setResyncTarget(null);
    if (error) {
      setActionError(`Resync ${r.name}: ${errMsg(error)}`);
    } else {
      refetchList();
      setTimeout(refetchList, 5000); // catch the re-declared row after the runner's next poll
    }
  };

  const deregister = async (r: Runner) => {
    if (r.id == null) return;
    setBusy(true);
    setActionError(null);
    const { error } = await api.DELETE("/runners/{runnerId}", {
      params: { path: { runnerId: r.id }, header: csrfHeader },
    });
    setBusy(false);
    setDeregTarget(null);
    if (error) setActionError(`Deregister ${r.name}: ${errMsg(error)}`);
    else refetchList();
  };

  const testRunner = async (r: Runner) => {
    if (r.id == null) return;
    setTestingId(r.id);
    setActionError(null);
    const { data, error } = await api.POST("/runners/{runnerId}/test", {
      params: { path: { runnerId: r.id }, header: csrfHeader },
    });
    setTestingId(null);
    if (error) {
      setActionError(`${r.name}: ${errMsg(error)}`);
      return;
    }
    if (data?.status === "verified") {
      setActionOk(`${r.name}: ${data.message ?? "connection verified"}`);
    } else if (data?.message) {
      setActionError(`${r.name}: ${data.message}`);
    }
    refetchList();
  };

  const mint = async () => {
    setBusy(true);
    setActionError(null);
    const { data, error } = await api.POST("/runners/registration-tokens", {
      params: { header: csrfHeader },
      body: mintLabel.trim() ? { label: mintLabel.trim() } : {},
    });
    setBusy(false);
    if (error) {
      setActionError(`Mint token: ${errMsg(error)}`);
    } else {
      // The mint response is the ONLY carrier of the plaintext; keep it so
      // Reveal/Copy and the install command work until the page unloads.
      setMinted((data as MintedToken) ?? null);
      setMintLabel("");
      setTokenRevealed(false);
      setTokenV((n) => n + 1); // refresh the list
    }
  };

  const revokeTok = async (tk: RegTokenInfo) => {
    setBusy(true);
    setActionError(null);
    const { error } = await api.DELETE("/runners/registration-tokens/{tokenId}", {
      params: { path: { tokenId: tk.id }, header: csrfHeader },
    });
    setBusy(false);
    if (error) {
      setActionError(`Revoke token${tk.label ? ` ${tk.label}` : ""}: ${errMsg(error)}`);
    } else {
      if (minted?.id === tk.id) setMinted(null); // its command is no longer usable
      setTokenV((n) => n + 1);
    }
  };

  const online = runners.filter((r) => r.status === "online").length;
  const degraded = runners.filter((r) => r.status === "degraded").length;
  const draining = runners.filter((r) => r.status === "draining").length;
  const offline = runners.filter((r) => r.status === "offline").length;

  const token = minted?.token ?? "";
  // LB8: a usable, copyable plaintext exists only when freshly minted — the
  // list never carries plaintext — so gate every copyable affordance on this.
  const revealable = minted != null && token !== "";
  const origin = typeof window !== "undefined" ? window.location.origin : "https://cronomicon.example.com";

  const tiles = [
    { l: "Total", v: runners.length, color: c.text },
    { l: "Online", v: online, color: c.success },
    { l: "Degraded", v: degraded, color: c.warning },
    { l: "Draining", v: draining, color: c.warning },
    { l: "Offline", v: offline, color: c.textSec },
  ].filter((s) => s.l !== "Draining" || s.v > 0);

  return (
    <div>
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: 16, gap: 8, flexWrap: "wrap" }}>
        <div style={{ display: "flex", gap: 8 }}>
          <a href="/runner-install.html" target="_blank" rel="noopener noreferrer" style={topLinkBtnStyle()}>
            Install Guide
          </a>
          <a href="/runner-manage.html" target="_blank" rel="noopener noreferrer" style={topLinkBtnStyle()}>
            Config Guide
          </a>
          <a href="/runner-security.html" target="_blank" rel="noopener noreferrer" style={topLinkBtnStyle()}>
            Security Guide
          </a>
        </div>
        <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
          <Btn primary onClick={openAddRunner}>
            + Add Runner
          </Btn>
          <a
            href={RUNNER_INSTALL_SCRIPT_PATH}
            download
            style={topDownloadBtnStyle()}
            title="Download the runner install script (served by this app)"
          >
            <svg width={14} height={14} viewBox="0 0 18 18" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
              <path d="M9 2.5v8.5M5.5 7.5L9 11l3.5-3.5M3.5 14.5h11" />
            </svg>
            Download
          </a>
        </div>
      </div>

      {actionError && (
        <AlertBanner type="danger" onDismiss={() => setActionError(null)}>
          {actionError}
        </AlertBanner>
      )}

      {/* FX-16 — success is a toast, not a banner: a banner is for a condition
          that persists and needs dismissing, and "connection verified" is neither.
          Errors keep the banner below, which is what banners are for. */}
      <Toast message={actionOk} />

      {list.loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {list.error && <div style={{ color: c.danger }}>Error: {list.error}</div>}

      {!list.loading && !list.error && (
        <>
          {/* Status tiles — shared StatTile with a status tint (VC.5) */}
          <div style={{ display: "flex", gap: 10, marginBottom: 16 }}>
            {tiles.map((s) => (
              <StatTile
                key={s.l}
                label={s.l}
                value={s.v}
                color={s.color}
                tint={s.color === c.text ? undefined : `${s.color}14`}
              />
            ))}
          </div>

          {/* Runner registry — the heading keeps its own line and TableSurface
              supplies the rule under it, so the table is bounded without the
              enclosing box it used to sit in (VU-5). */}
          <div style={{ marginBottom: 16 }}>
            <div style={{ ...cardTitle(), borderBottom: "none", padding: "10px 0", display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8 }}>
              <span>Runner Registry</span>
            <ColumnsMenu cols={cols} cw={cw} />
            </div>
            <TableSurface>
                <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm, tableLayout: "fixed" }}>
                  <thead>
                    <TableHead columns={cols.visible} sort={runnerSort} cw={cw} thStyle={th} />
                  </thead>
                  <tbody>
                    {runners.length === 0 ? (
                      <tr>
                        <td
                          colSpan={cols.visible.length}
                          style={{ color: c.textSec, padding: "20px 14px", textAlign: "center", fontSize: c.fontSm }}
                        >
                          No runners registered —{" "}
                          <button
                            onClick={openAddRunner}
                            style={{ background: "none", border: "none", padding: 0, color: c.primary, cursor: "pointer", fontSize: c.fontSm, textDecoration: "underline", fontFamily: "inherit" }}
                          >
                            Add Runner
                          </button>{" "}
                          to install one.
                        </td>
                      </tr>
                    ) : (
                      runnerSort.sorted.map((r) => {
                      // sc/load/max moved INTO their column cells (CO-4) — each
                      // was used by exactly one of them.
                      const isExp = expandedRunner === r.id;
                      return (
                        <Fragment key={r.id ?? r.name}>
                        <HoverTr
                          onClick={() => setExpandedRunner(isExp ? null : (r.id ?? null))}
                          tint={isExp ? c.primaryBg : undefined}
                          hoverTint={isExp ? c.primaryBg : c.panelHover}
                          style={{ borderBottom: isExp ? "none" : `1px solid ${c.border}` }}
                          role="button"
                          tabIndex={0}
                          ariaExpanded={isExp}
                          ariaLabel={`Runner ${r.name} — ${isExp ? "collapse" : "expand"} details`}
                          onKeyDown={(e) => {
                            // Only the row itself toggles: ignore key events bubbling up
                            // from the focusable Test/Deregister buttons inside it.
                            if (e.target !== e.currentTarget) return;
                            if (e.key === "Enter" || e.key === " ") {
                              e.preventDefault();
                              setExpandedRunner(isExp ? null : (r.id ?? null));
                            }
                          }}
                        >
                          {renderCells(cols.visible, r, { base: td })}
                        </HoverTr>
                        {isExp && (
                          <tr>
                            <td
                              colSpan={cols.visible.length}
                              style={{ padding: "14px 18px", background: c.primaryBg, borderBottom: `1px solid ${c.border}` }}
                            >
                              {/* Actions relocated out of the compact row (v0.47.20):
                                  Resync, Scan keys, Drain, Copy upgrade — shown only once
                                  the runner is expanded. R6: they render top-right of the
                                  detail's health strip, the one consistent action home.
                                  ⚙ Settings moved onto the Managed settings section header
                                  (the thing it edits), which also carries the pending dot. */}
                              <RefreshScope>
                              <RunnerDetail
                                runner={r}
                                serverVersion={serverBuild?.version}
                                availableAgencies={availableAgencies}
                                canConfig={canConfig}
                                onEditSettings={() => setSettingsTarget(r)}
                                onScrollToToken={scrollToToken}
                                onSaved={refetchList}
                                actions={
                                  <>
                                {/* FX-7 — these three are gated on the runner being
                                    REACHABLE, not on it being healthy. They were gated on
                                    `online`, which took Resync, Scan keys and Drain away
                                    from a degraded runner — the one state where an
                                    operator most needs them, since a degraded runner still
                                    heartbeats and still holds work. Offline keeps hiding
                                    them: there is nothing there to answer.

                                    House rule this establishes: a precondition DISABLES
                                    with an explanation; only irrelevance hides. (The
                                    per-button protocol gates that once sat here went with
                                    the server's protocol floor, v1.5.40.) */}
                                {isReachable(r.status) && (
                                  <Btn
                                    small
                                    onClick={() => setResyncTarget(r)}
                                    disabled={busy}
                                    title="Force a re-declare of this runner's current local config now (config changes usually propagate automatically on the next poll after a restart)"
                                  >
                                    Resync
                                  </Btn>
                                )}
                                {isReachable(r.status) && (
                                  <Btn
                                    small
                                    onClick={() => setScanTarget(r)}
                                    disabled={busy}
                                    title="Scan a target's SSH host key so you can approve it (human-approved TOFU)"
                                  >
                                    Scan keys
                                  </Btn>
                                )}
                                {isReachable(r.status) && (
                                  <Btn small onClick={() => setDrainTarget(r)} disabled={busy}>
                                    Drain
                                  </Btn>
                                )}
                                {/* FX-4 — the upgrade command belongs with the other
                                    per-runner actions, not below the fold in the version
                                    section. Always offered: it is also how you reinstall
                                    the current version, and `behind` compares version
                                    STRINGS, so a locally-built agent reads "up to date"
                                    while speaking an older protocol. */}
                                <CopyButton
                                  text={upgradeCommand(runnerOrigin())}
                                  label="Copy upgrade command"
                                  title={`Copy the one-line root command that upgrades (or reinstalls) the agent on ${r.name}`}
                                  ariaLabel={`Copy upgrade command for ${r.name}`}
                                />
                                  </>
                                }
                              />
                              </RefreshScope>
                            </td>
                          </tr>
                        )}
                        </Fragment>
                      );
                    })
                    )}
                  </tbody>
                </table>
              </TableSurface>
          </div>

          {canConfig && <PendingHostKeys refreshKey={refresh} />}
        </>
      )}

      {/* Register new runner (single-use tokens, Phase 7) */}
      <div style={{ background: c.panel, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, overflow: "hidden" }}>
        <div style={cardTitle()}>Register New Runner</div>
        <div style={{ padding: 14 }}>
          <div style={{ fontSize: c.fontSm, color: c.textSec, marginBottom: 14 }}>
            For most installs, use <strong>+ Add Runner</strong> above — mint and copy a one-line command in one
            step. This panel is the token audit trail and the manual command builder. Mint a{" "}
            <strong>single-use</strong> registration token per install (24-hour expiry, shown once). Each token dies
            on its first successful registration and records which runner consumed it. Revoking an unused token does
            not affect registered runners — they hold long-lived API keys; to revoke a runner,{" "}
            <strong>Deregister</strong> it above.
          </div>

          <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
            {/* Mint row */}
            <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
              <input
                value={mintLabel}
                onChange={(e) => setMintLabel(e.target.value)}
                placeholder="Label (optional — e.g. the intended runner name)"
                maxLength={120}
                style={{
                  flex: 1,
                  padding: "7px 10px",
                  background: c.panel2,
                  border: `1px solid ${c.borderStrong}`,
                  borderRadius: c.radiusChip,
                  fontSize: c.fontSm,
                  color: c.text,
                }}
              />
              <button style={btnStyle("primary")} onClick={mint} disabled={busy}>
                {busy ? "Minting…" : "Mint token"}
              </button>
            </div>

            {/* Freshly minted token + install command */}
            {minted && (
              <>
                <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                  <div style={fieldLabel()}>Token</div>
                  <code
                    style={{
                      flex: 1,
                      padding: "7px 10px",
                      background: c.panel2,
                      border: `1px solid ${c.border}`,
                      borderRadius: c.radiusChip,
                      fontSize: c.fontSm,
                      fontFamily: c.mono,
                      letterSpacing: tokenRevealed ? 0 : 3,
                      overflow: "hidden",
                      textOverflow: "ellipsis",
                      whiteSpace: "nowrap",
                    }}
                  >
                    {tokenRevealed ? token : "•".repeat(40)}
                  </code>
                  <button style={btnStyle()} onClick={() => setTokenRevealed((v) => !v)} disabled={!revealable}>
                    {tokenRevealed ? "Hide" : "Reveal"}
                  </button>
                  <CopyButton text={token} disabled={!revealable} ariaLabel="Copy registration token to clipboard" />
                </div>
                <div style={{ display: "flex", alignItems: "center", gap: 8, fontSize: c.fontSm, color: c.textSec }}>
                  <div style={fieldLabel()}>Expires</div>
                  <span>{fmtDate(minted.expiresAt)}</span>
                  {minted.label && (
                    <span style={{ fontSize: c.fontXs }}>
                      Label: <strong>{minted.label}</strong>
                    </span>
                  )}
                  <span style={{ fontSize: c.fontXs }}>
                    Shown once — it is stored only as a hash and cannot be re-revealed later.
                  </span>
                </div>
              </>
            )}
            {minted && (
              <div>
                <div style={{ ...fieldLabel(), minWidth: undefined, marginBottom: 6 }}>Install Command</div>
                <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
                  <code
                    style={{
                      flex: 1,
                      padding: "8px 12px",
                      background: c.panel2,
                      border: `1px solid ${c.border}`,
                      borderRadius: c.radiusSurface,
                      fontSize: c.fontSm,
                      fontFamily: c.mono,
                      color: c.textSec,
                      wordBreak: "break-all",
                    }}
                  >
                    {installOneLiner(origin, revealable && tokenRevealed ? token : "<TOKEN>")}
                  </code>
                  <CopyButton text={installOneLiner(origin, token)} ariaLabel="Copy the install command to clipboard" />
                </div>
                <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 6, lineHeight: 1.5 }}>
                  Run on the target host. <code>--download</code> fetches the agent binary from this server with
                  checksum verification (deployments without bundled binaries fall back to a local{" "}
                  <code>cronomicon-runner</code> / <code>-b &lt;path&gt;</code>). Capabilities are auto-detected from
                  the host's toolchains at startup; add <code>-c</code> only to narrow them — see the Install
                  Guide.{" "}
                  <button
                    onClick={() => setShowTwoStep((v) => !v)}
                    style={{
                      background: "none",
                      border: "none",
                      padding: 0,
                      color: c.primary,
                      cursor: "pointer",
                      fontSize: c.fontXs,
                      textDecoration: "underline",
                    }}
                  >
                    {showTwoStep ? "Hide the download-inspect-run variant" : "Prefer not to pipe curl into sudo?"}
                  </button>
                </div>
                {showTwoStep && (
                  <div style={{ display: "flex", gap: 8, alignItems: "center", marginTop: 6 }}>
                    <code
                      style={{
                        flex: 1,
                        padding: "8px 12px",
                        background: c.panel2,
                        border: `1px solid ${c.border}`,
                        borderRadius: c.radiusSurface,
                        fontSize: c.fontSm,
                        fontFamily: c.mono,
                        color: c.textSec,
                        whiteSpace: "pre-wrap",
                        wordBreak: "break-all",
                      }}
                    >
                      {installTwoStep(origin, revealable && tokenRevealed ? token : "<TOKEN>")}
                    </code>
                    <CopyButton text={installTwoStep(origin, token)} ariaLabel="Copy the install command to clipboard" />
                  </div>
                )}
              </div>
            )}

            {/* Token list: label, created, expires, status + used-by audit trail */}
            {tokList.loading && <InlineLoading what="tokens" />}
            {tokList.error && (
              <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
                <div style={{ color: c.danger, fontSize: c.fontSm }}>Error loading tokens: {tokList.error}</div>
                <button style={btnStyle()} onClick={() => setTokenV((n) => n + 1)} disabled={busy}>
                  Retry
                </button>
              </div>
            )}
            {!tokList.loading && !tokList.error && regTokens.length > 0 && (
              <div style={{ overflowX: "auto" }}>
                <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm }}>
                  <thead>
                    <tr>
                      {(
                        [
                          ["Status", "status"],
                          ["Used by", "usedBy"],
                          ["Label", "label"],
                          ["Created", "createdAt"],
                          ["Expires", "expiresAt"],
                        ] as [string, string][]
                      ).map(([labelText, key]) => {
                        const active = tokSort.isActive(key);
                        return (
                          <th
                            key={key}
                            onClick={() => tokSort.toggle(key)}
                            aria-sort={tokSort.ariaSort(key)}
                            title={`Sort by ${labelText}`}
                            style={{
                              textAlign: "left",
                              padding: "6px 8px",
                              borderBottom: `1px solid ${c.border}`,
                              color: active ? c.text : c.textSec,
                              fontWeight: 600,
                              cursor: "pointer",
                              userSelect: "none",
                            }}
                          >
                            <SortableLabel label={labelText} active={active} dir={tokSort.sortDir} />
                          </th>
                        );
                      })}
                      <th
                        style={{
                          padding: "6px 8px",
                          borderBottom: `1px solid ${c.border}`,
                        }}
                      ></th>
                    </tr>
                  </thead>
                  <tbody>
                    {tokPageItems.map((tk) => {
                      const tdStyle = {
                        padding: "6px 8px",
                        borderBottom: `1px solid ${c.border}`,
                        color: c.textSec,
                        whiteSpace: "nowrap" as const,
                      };

                      const statusColor =
                        tk.status === "pending"
                          ? c.success
                          : tk.status === "active"
                            ? c.text
                            : tk.status === "expired"
                              ? c.warning
                              : c.textSec;
                      const tokenStatusLabel =
                        tk.status === "active"
                          ? "Active"
                          : tk.status === "pending"
                            ? "Pending"
                            : tk.status === "expired"
                              ? "Expired"
                              : tk.status === "revoked"
                                ? "Revoked"
                                : (tk.status ?? "—");
                      // "Used by" is only meaningful once a token has been consumed
                      // (status active); the consuming runner may since be deregistered.
                      const usedBy =
                        tk.status === "active"
                          ? (tk.usedByRunnerName ?? tk.usedByRunnerId ?? "a deregistered runner")
                          : null;
                      return (
                        <tr key={tk.id} id={`regtoken-${tk.id}`}>
                          <td style={{ ...tdStyle, color: statusColor }}>{tokenStatusLabel}</td>
                          <td style={tdStyle}>{usedBy ?? <span style={{ color: c.textSec }}>—</span>}</td>
                          <td style={{ ...tdStyle, color: c.text }}>{tk.label || <span style={{ color: c.textSec }}>—</span>}</td>
                          <td style={tdStyle}>{fmtDate(tk.createdAt)}</td>
                          <td style={tdStyle}>{fmtDate(tk.expiresAt)}</td>
                          <td style={{ ...tdStyle, textAlign: "right" }}>
                            {tk.status === "pending" && (
                              <Btn
                                small
                                dangerQuiet
                                onClick={() => revokeTok(tk)}
                                disabled={busy}
                                title="Revoke this unused token. Does not affect registered runners."
                              >
                                Revoke
                              </Btn>
                            )}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
                {sortedTokens.length > TOK_PAGE_SIZE && (
                  <div
                    style={{
                      display: "flex",
                      justifyContent: "space-between",
                      alignItems: "center",
                      marginTop: 8,
                      fontSize: c.fontSm,
                      color: c.textSec,
                    }}
                  >
                    <span>
                      Showing {tokPageClamped * TOK_PAGE_SIZE + 1}–
                      {Math.min(sortedTokens.length, (tokPageClamped + 1) * TOK_PAGE_SIZE)} of {sortedTokens.length} tokens
                    </span>
                    <div style={{ display: "flex", gap: 6 }}>
                      <Btn small onClick={() => setTokPage(Math.max(0, tokPageClamped - 1))} disabled={tokPageClamped === 0}>
                        ← Prev
                      </Btn>
                      <Btn
                        small
                        onClick={() => setTokPage(tokPageClamped + 1)}
                        disabled={(tokPageClamped + 1) * TOK_PAGE_SIZE >= sortedTokens.length}
                      >
                        Next →
                      </Btn>
                    </div>
                  </div>
                )}
              </div>
            )}
            {/* VU-14 — the empty token list carries the mint control itself (the same
                handler as the row above), so the sentence is not the whole answer. */}
            {!tokList.loading && !tokList.error && regTokens.length === 0 && !minted && (
              <div style={{ fontSize: c.fontSm, color: c.textSec, display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
                No registration tokens yet — a runner needs one to register.
                <Btn small onClick={mint} disabled={busy}>
                  {busy ? "Minting…" : "Mint a token"}
                </Btn>
              </div>
            )}
          </div>
        </div>
      </div>

      {/* Provision a Runner (Phase 6): choices → runner.env + one-liner + docker run */}
      <ProvisionPanel origin={origin} token={minted?.token ?? "<TOKEN>"} serverVersion={serverBuild?.version} />

      {/* Add Runner — the one-click headline path (Phase 2, D2). Mint a token,
          then paste one baked-in line on the host. */}
      {addOpen && (
        <Modal
          title="Add a Runner"
          wide
          onClose={() => setAddOpen(false)}
          /* FX-10 — only the revealed state has a trailing bar; before the token
             is minted the primary action sits inline beside its label input,
             which is where it belongs. Hence a per-branch footer rather than one
             bar that would render empty half the time. */
          footer={
            revealable ? (
              <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 8 }}>
                <button
                  onClick={() => { setMinted(null); setMintLabel(""); setAddShowTwoStep(false); }}
                  style={{ background: "none", border: "none", padding: 0, color: c.textSec, cursor: "pointer", fontSize: c.fontSm, textDecoration: "underline" }}
                >
                  Add another runner
                </button>
                <Btn primary onClick={() => setAddOpen(false)}>Done</Btn>
              </div>
            ) : undefined
          }
        >
          {!revealable ? (
            <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
              <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
                Mint a <strong>single-use</strong> registration token, then paste one line on the runner host.
                The server URL, token, and agent-binary download are baked into the script — no flags to carry,
                and capabilities auto-detect from the host's toolchains.
              </div>
              <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                <input
                  value={mintLabel}
                  onChange={(e) => setMintLabel(e.target.value)}
                  placeholder="Label (optional — e.g. the intended runner name)"
                  maxLength={120}
                  style={{ flex: 1, padding: "7px 10px", background: c.panel2, border: `1px solid ${c.borderStrong}`, borderRadius: c.radiusChip, fontSize: c.fontSm, color: c.text }}
                />
                <Btn primary onClick={mint} disabled={busy}>
                  {busy ? "Minting…" : "Mint & build command"}
                </Btn>
              </div>
              <div style={{ fontSize: c.fontXs, color: c.textSec }}>
                The host only needs outbound HTTPS to <code>{origin}</code> — the agent polls the server, so no
                inbound ports are opened.
              </div>
            </div>
          ) : (
            <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
              <div style={{ fontSize: c.fontSm, color: c.textSec }}>
                Expires in 24h · single-use · one token per runner
                {minted.label && (
                  <>
                    {" · "}Label: <strong>{minted.label}</strong>
                  </>
                )}
              </div>
              <div>
                <div style={{ ...fieldLabel(), minWidth: undefined, marginBottom: 6 }}>Run this on the runner host (as root)</div>
                <div style={{ display: "flex", gap: 8, alignItems: "flex-start" }}>
                  <code style={{ flex: 1, padding: "10px 12px", background: c.panel2, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, fontSize: c.fontSm, fontFamily: c.mono, color: c.text, wordBreak: "break-all" }}>
                    {personalizedInstallOneLiner(origin, token)}
                  </code>
                  <CopyButton text={personalizedInstallOneLiner(origin, token)} ariaLabel="Copy the install command to clipboard" />
                </div>
                <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 6, lineHeight: 1.5 }}>
                  Thirty seconds later the runner appears in the registry above with its detected run-types. The
                  agent binary is fetched from this server and checksum-verified.{" "}
                  <button
                    onClick={() => setAddShowTwoStep((v) => !v)}
                    style={{ background: "none", border: "none", padding: 0, color: c.primary, cursor: "pointer", fontSize: c.fontXs, textDecoration: "underline" }}
                  >
                    {addShowTwoStep ? "Hide the download-inspect-run variant" : "Prefer not to pipe curl into sudo?"}
                  </button>
                </div>
                {addShowTwoStep && (
                  <div style={{ display: "flex", gap: 8, alignItems: "flex-start", marginTop: 8 }}>
                    <code style={{ flex: 1, padding: "10px 12px", background: c.panel2, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, fontSize: c.fontSm, fontFamily: c.mono, color: c.text, whiteSpace: "pre-wrap", wordBreak: "break-all" }}>
                      {personalizedInstallTwoStep(origin, token)}
                    </code>
                    <CopyButton text={personalizedInstallTwoStep(origin, token)} ariaLabel="Copy the install command to clipboard" />
                  </div>
                )}
              </div>
            </div>
          )}
        </Modal>
      )}

      {/* Confirm dialogs */}
      {drainTarget && (
        <ConfirmDialog
          title={`Drain ${drainTarget.name}`}
          message={
            (drainTarget.load ?? 0) > 0
              ? `${drainTarget.name} will finish ${drainTarget.load} active job${(drainTarget.load ?? 0) !== 1 ? "s" : ""} then stop accepting new jobs. When the active count reaches 0 it goes offline.`
              : `${drainTarget.name} will stop accepting new jobs immediately and go offline.`
          }
          confirmLabel="Drain Runner"
          danger={false}
          busy={busy}
          onConfirm={() => drain(drainTarget)}
          onCancel={() => setDrainTarget(null)}
        />
      )}
      {resyncTarget && (
        <ConfirmDialog
          title={`Resync ${resyncTarget.name}`}
          message={`${resyncTarget.name} will re-declare its current local config (name, capabilities, version, inventory, toolchains) on its next poll — same identity and API key, no deregistration. Active jobs are unaffected. (Config changes usually propagate automatically after a restart; Resync forces it now.)`}
          confirmLabel="Resync"
          danger={false}
          busy={busy}
          onConfirm={() => resync(resyncTarget)}
          onCancel={() => setResyncTarget(null)}
        />
      )}
      {deregTarget && (
        <ConfirmDialog
          title={`Deregister ${deregTarget.name}`}
          message={
            deregTarget.status === "offline"
              ? `${deregTarget.name} will be permanently removed from the runner registry and its API key revoked.`
              : `${deregTarget.name} is currently ${deregTarget.status} with ${deregTarget.load ?? 0} active job${(deregTarget.load ?? 0) !== 1 ? "s" : ""}. Force-deregistering revokes its API key immediately and cannot be undone.`
          }
          confirmLabel="Deregister"
          danger
          busy={busy}
          onConfirm={() => deregister(deregTarget)}
          onCancel={() => setDeregTarget(null)}
        />
      )}
      {settingsTarget && (
        <RunnerSettingsDrawer
          runner={settingsTarget}
          onClose={() => setSettingsTarget(null)}
          onSaved={() => {
            setSettingsTarget(null);
            refetchList();
            setTimeout(refetchList, 5000); // catch the ack after the runner's next poll
          }}
        />
      )}
      {scanTarget && (
        <ScanHostKeysModal runner={scanTarget} onClose={() => setScanTarget(null)} />
      )}
    </div>
  );
}

// ScanHostKeysModal queues a host-key scan for a runner (Phase 5). The operator
// enters the targets; the runner's next poll scans them and uploads what it saw
// to the Pending host-key approvals section.
function ScanHostKeysModal({ runner, onClose }: { runner: Runner; onClose: () => void }) {
  const [hosts, setHosts] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [done, setDone] = useState(false);

  const submit = async () => {
    if (runner.id == null) return;
    const list = hosts.split(/[\s,]+/).map((h) => h.trim()).filter(Boolean);
    if (list.length === 0) {
      setErr("enter at least one host");
      return;
    }
    setBusy(true);
    setErr(null);
    const { error } = await api.POST("/runners/{runnerId}/keyscan", {
      params: { path: { runnerId: runner.id }, header: csrfHeader },
      body: { hosts: list },
    });
    setBusy(false);
    if (error) {
      setErr(errMsg(error));
      return;
    }
    setDone(true);
  };

  return (
    <Modal
      title={`Scan host keys — ${runner.name}`}
      onClose={onClose}
      /* FX-10 — the body is a two-branch ternary, so the footer is one too: each
         state has its own action, and the error belongs with the button it
         explains. The Done button was nested inside the prose block; pinning it
         also takes it out of a paragraph it was never part of. */
      footer={
        done ? (
          <div style={{ display: "flex", justifyContent: "flex-end" }}>
            <Btn primary onClick={onClose}>Done</Btn>
          </div>
        ) : (
          <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
            {err && <div style={{ color: c.danger, fontSize: c.fontSm }}>{err}</div>}
            <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
              <Btn onClick={onClose} disabled={busy}>Cancel</Btn>
              <Btn primary onClick={submit} disabled={busy}>{busy ? "Queuing…" : "Queue scan"}</Btn>
            </div>
          </div>
        )
      }
    >
      {done ? (
        <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
          Scan queued. On its next poll the runner scans these targets and uploads their keys to{" "}
          <strong>Pending host-key approvals</strong> above. Compare each fingerprint out-of-band, then approve.
        </div>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
          <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
            Enter the targets to scan (one per line, or comma/space separated) as <code>host</code> or{" "}
            <code>host:port</code>. The runner captures each host's SSH key from its own vantage — you approve
            them next. This is <strong>human-approved TOFU</strong>: compare the SHA256 fingerprint out-of-band
            before approving.
          </div>
          <textarea
            style={{
              width: "100%",
              minHeight: 80,
              padding: "8px 10px",
              background: c.panel2,
              border: `1px solid ${c.borderStrong}`,
              borderRadius: c.radiusChip,
              fontSize: c.fontSm,
              fontFamily: c.mono,
              color: c.text,
              resize: "vertical",
            }}
            value={hosts}
            onChange={(e) => setHosts(e.target.value)}
            placeholder={"web01\ndb01:2222"}
          />
        </div>
      )}
    </Modal>
  );
}

// PendingHostKeys renders the scanned host keys awaiting approval (Phase 5),
// with full SHA256 fingerprints for out-of-band comparison and approve/reject.
function PendingHostKeys({ refreshKey }: { refreshKey: number }) {
  const [rows, setRows] = useState<PendingHostKeyRow[]>([]);
  const [v, setV] = useState(0);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    api.GET("/runners/host-keys/pending").then(({ data, error }) => {
      if (!live) return;
      if (error) setErr(errMsg(error));
      else setRows((data as PendingHostKeyRow[]) ?? []);
    });
    return () => {
      live = false;
    };
  }, [refreshKey, v]);

  const resolve = async (id: string, action: "approve" | "reject") => {
    setBusyId(id);
    setErr(null);
    const { error } = await api.POST("/runners/host-keys/{keyId}/resolve", {
      params: { path: { keyId: id }, header: csrfHeader },
      body: { action },
    });
    setBusyId(null);
    if (error) setErr(errMsg(error));
    else setV((n) => n + 1);
  };

  if (rows.length === 0) return null; // nothing pending — stay out of the way

  return (
    <div style={{ background: c.panel, border: `1px solid ${c.warning}`, borderRadius: c.radiusSurface, marginBottom: 16, overflow: "hidden" }}>
      <div style={{ ...cardTitle(), display: "flex", alignItems: "center", gap: 8 }}>
        <span>Pending host-key approvals</span>
        <span style={{ fontSize: c.fontXs, color: c.warning }}>● {rows.length} awaiting review</span>
      </div>
      <div style={{ padding: 14, display: "flex", flexDirection: "column", gap: 10 }}>
        <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
          These SSH host keys were scanned by a runner and are awaiting your approval. <strong>Compare the
          SHA256 fingerprint out-of-band</strong> (against the host itself) before approving — approving without
          that check is still trust-on-first-use. Approving appends the key to the runner's <code>known_hosts</code>{" "}
          on its next poll; the failed SSH run can then be retried.
        </div>
        {err && <div style={{ color: c.danger, fontSize: c.fontSm }}>{err}</div>}
        {/* One framing layer, not two (VU-5): inside an already-bordered panel the
            per-key boxes were a nested frame, so the rows separate on a hairline
            and their own leading instead. */}
        {rows.map((k, i) => (
          <Fragment key={k.id}>
            {i > 0 && <Rule />}
          <div
            style={{
              display: "flex",
              alignItems: "center",
              gap: 12,
              flexWrap: "wrap",
              padding: "8px 0",
            }}
          >
            <div style={{ flex: 1, minWidth: 260 }}>
              <div style={{ fontSize: c.fontSm, color: c.text }}>
                <strong>{k.host}</strong> <span style={{ color: c.textSec }}>({k.keyType})</span>
                <span style={{ color: c.textSec }}> · {k.runnerName}</span>
              </div>
              <div style={{ fontSize: c.fontXs, fontFamily: c.mono, color: c.textSec, wordBreak: "break-all" }}>
                {k.fingerprint}
              </div>
            </div>
            <Btn small primary disabled={busyId === k.id} onClick={() => resolve(k.id, "approve")}>
              Approve
            </Btn>
            <Btn small dangerQuiet disabled={busyId === k.id} onClick={() => resolve(k.id, "reject")}>
              Reject
            </Btn>
          </div>
          </Fragment>
        ))}
      </div>
    </div>
  );
}

interface PendingHostKeyRow {
  id: string;
  runnerId: string;
  runnerName: string;
  host: string;
  keyType: string;
  fingerprint: string;
  scannedAt: string;
}

// RunnerSettingsDrawer edits a runner's server-managed operational overrides
// (Phase 4) and PATCHes them. Each override is optional: blank/inherit means
// "no server opinion — use the runner's own local value". The change rides the
// runner's next poll and applies in-memory (no restart).
function RunnerSettingsDrawer({
  runner,
  onClose,
  onSaved,
}: {
  runner: Runner;
  onClose: () => void;
  onSaved: () => void;
}) {
  const ms = runner.managedSettings ?? {};
  const [maxJobs, setMaxJobs] = useState(ms.maxConcurrent != null ? String(ms.maxConcurrent) : "");
  const [sbMem, setSbMem] = useState(ms.sandboxMemoryMax ?? "");
  const [sbCpu, setSbCpu] = useState(ms.sandboxCpuQuota ?? "");
  const [sbTasks, setSbTasks] = useState(ms.sandboxTasksMax ?? "");
  const [checkout, setCheckout] = useState<"inherit" | "on" | "off">(
    ms.allowCheckout == null ? "inherit" : ms.allowCheckout ? "on" : "off",
  );
  const [repos, setRepos] = useState((ms.checkoutRepos ?? []).join("\n"));
  const [mask, setMask] = useState<RunTypeName[]>(ms.capabilityMask ?? []);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const pending = (runner.settingsVersion ?? 0) > (runner.settingsAckedVersion ?? 0);

  const build = (): ManagedSettings => {
    const body: ManagedSettings = {};
    if (maxJobs.trim()) body.maxConcurrent = Number(maxJobs);
    if (sbMem.trim()) body.sandboxMemoryMax = sbMem.trim();
    if (sbCpu.trim()) body.sandboxCpuQuota = sbCpu.trim();
    if (sbTasks.trim()) body.sandboxTasksMax = sbTasks.trim();
    if (checkout !== "inherit") body.allowCheckout = checkout === "on";
    const repoList = repos.split("\n").map((s) => s.trim()).filter(Boolean);
    if (repoList.length) body.checkoutRepos = repoList;
    if (mask.length) body.capabilityMask = mask;
    return body;
  };

  const save = async (clear = false) => {
    if (runner.id == null) return;
    setBusy(true);
    setErr(null);
    const { error } = await api.PATCH("/runners/{runnerId}/settings", {
      params: { path: { runnerId: runner.id }, header: csrfHeader },
      body: clear ? {} : build(),
    });
    setBusy(false);
    if (error) {
      setErr(errMsg(error));
      return;
    }
    onSaved();
  };

  const inputStyle: React.CSSProperties = {
    padding: "6px 9px",
    background: c.panel2,
    border: `1px solid ${c.borderStrong}`,
    borderRadius: c.radiusChip,
    fontSize: c.fontSm,
    color: c.text,
    fontFamily: "inherit",
  };
  const localHint = (t: React.ReactNode) => (
    <span style={{ fontSize: c.fontXs, color: c.textSec, marginLeft: 8 }}>{t}</span>
  );
  const row = (label: string, control: React.ReactNode, hint?: React.ReactNode) => (
    <div style={{ marginBottom: 12 }}>
      <div style={{ display: "flex", alignItems: "center", flexWrap: "wrap", gap: 6 }}>
        <div style={{ ...fieldLabel(), minWidth: 120 }}>{label}</div>
        {control}
      </div>
      {hint && <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4, paddingLeft: 126 }}>{hint}</div>}
    </div>
  );

  return (
    <Modal
      title={`Managed settings — ${runner.name}`}
      wide
      onClose={onClose}
      /* FX-10 — this one keeps its space-between shape: "Clear all overrides" is
         a destructive-ish action that belongs opposite Save, not stacked with it. */
      footer={
        <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
          {err && <div style={{ color: c.danger, fontSize: c.fontSm }}>{err}</div>}
          <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 8 }}>
            <button
              onClick={() => save(true)}
              disabled={busy}
              style={{ background: "none", border: "none", padding: 0, color: c.textSec, cursor: "pointer", fontSize: c.fontSm, textDecoration: "underline" }}
            >
              Clear all overrides
            </button>
            <div style={{ display: "flex", gap: 8 }}>
              <Btn onClick={onClose} disabled={busy}>
                Cancel
              </Btn>
              <Btn primary onClick={() => save(false)} disabled={busy}>
                {busy ? "Saving…" : "Save"}
              </Btn>
            </div>
          </div>
        </div>
      }
    >
      <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6, marginBottom: 14 }}>
        These <strong>override</strong> the runner's local config and take effect on its{" "}
        <strong>next poll</strong> — no SSH, no restart, no env edit. Leave a field blank (or{" "}
        <em>inherit</em>) to keep the runner's own value.
        {pending && (
          <div style={{ marginTop: 6, color: c.warning }}>
            ● A previous change (v{runner.settingsVersion}) is still propagating — the agent last acked v
            {runner.settingsAckedVersion ?? 0}.
          </div>
        )}
      </div>

      {row(
        "Max jobs",
        <input
          style={{ ...inputStyle, width: 90 }}
          value={maxJobs}
          onChange={(e) => setMaxJobs(e.target.value.replace(/[^0-9]/g, ""))}
          placeholder="inherit"
        />,
        <>
          Caps simultaneously-executing runs. Runner's declared value: <strong>{runner.maxConcurrent ?? 5}</strong>.
        </>,
      )}

      {row(
        "Sandbox caps",
        <>
          <input style={{ ...inputStyle, width: 100 }} value={sbMem} onChange={(e) => setSbMem(e.target.value)} placeholder="memory (2G)" />
          <input style={{ ...inputStyle, width: 100 }} value={sbCpu} onChange={(e) => setSbCpu(e.target.value)} placeholder="cpu (150%)" />
          <input style={{ ...inputStyle, width: 100 }} value={sbTasks} onChange={(e) => setSbTasks(e.target.value)} placeholder="tasks (512)" />
        </>,
        <>Per-run cgroup MemoryMax / CPUQuota / TasksMax. Blank ⇒ the runner's own value (or uncapped).</>,
      )}

      {row(
        "Checkout",
        <>
          <select style={inputStyle} value={checkout} onChange={(e) => setCheckout(e.target.value as typeof checkout)}>
            <option value="inherit">inherit (runner's value)</option>
            <option value="on">force on</option>
            <option value="off">force off</option>
          </select>
          {localHint(<>runner reports checkout {runner.toolchains?.checkout ? "on" : "off"}</>)}
        </>,
        <>Whether ansible jobs may check out a pinned playbook project. The deploy token stays runner-local regardless.</>,
      )}

      {row(
        "Checkout repos",
        <textarea
          style={{ ...inputStyle, width: 360, height: 60, resize: "vertical" }}
          value={repos}
          onChange={(e) => setRepos(e.target.value)}
          placeholder={"one clone URL per line\n(blank ⇒ runner's own allowlist)"}
        />,
        <>The allowlist of repo clone URLs this runner may check out — one per line. A repo outside it is refused.</>,
      )}

      {row(
        "Capability mask",
        <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
          {RUN_TYPES.map((rt) => {
            const on = mask.includes(rt);
            const has = (runner.capabilities ?? []).includes(rt);
            return (
              <button
                key={rt}
                onClick={() => setMask(on ? mask.filter((x) => x !== rt) : [...mask, rt])}
                disabled={!has}
                title={has ? "Click to REMOVE this run-type from the runner's claim set" : "Runner does not declare this run-type"}
                style={{ ...btnStyle(on ? "danger" : "default"), opacity: has ? (on ? 1 : 0.7) : 0.35 }}
              >
                {on ? `− ${rt}` : rt}
              </button>
            );
          })}
        </div>,
        <>Subtract-only: masked run-types are removed from what this runner will claim (server-enforced). Use it to quiesce a run-type without touching the host.</>,
      )}

    </Modal>
  );
}

// ── Provision a Runner (Phase 6) ─────────────────────────────────────────────

// ProvisionPanel turns one set of choices into three copy-paste artifacts: a
// complete annotated runner.env (patched into the verbatim
// cronomicon-runner.env.example — single-sourced, fetched from the app), the
// matching runner-install.sh one-liner (Phase-1 flags included), and the
// docker run variant (slim/fat derived from the capability pick). Pure
// frontend; the generators live in runner-provision.ts with unit tests.
type ProvisionProfile = "ssh" | "ansible" | "custom";

function ProvisionPanel({
  origin,
  token,
  serverVersion,
}: {
  origin: string;
  token: string;
  serverVersion?: string;
}) {
  const [open, setOpen] = useState(false);
  // Phase 6: a profile is a visibility preset over the same fields. "SSH task
  // runner" surfaces key/known_hosts custody; "Ansible control node" surfaces
  // the checkout/vault secret files; "Custom" shows everything.
  const [profile, setProfile] = useState<ProvisionProfile>("ssh");
  const [sshExpanded, setSshExpanded] = useState(false);
  const showSSHCustody = profile !== "ansible" || sshExpanded;
  const showAnsibleSecrets = profile !== "ssh";
  const [example, setExample] = useState<string | null>(null);
  const [exampleErr, setExampleErr] = useState<string | null>(null);

  const [name, setName] = useState("");
  // Default: no explicit capabilities — the agent auto-detects the host's
  // toolchains at startup (D1: 1B). The chips only appear under an explicit
  // "Override" toggle, and narrow what the runner claims.
  const [capsOverride, setCapsOverride] = useState(false);
  const [caps, setCaps] = useState<string[]>([]);
  const [inventory, setInventory] = useState<"cronomicon" | "local">("cronomicon");
  const [localInventorySrc, setLocalInventorySrc] = useState("");
  const [knownHostsSrc, setKnownHostsSrc] = useState("");
  const [keyMode, setKeyMode] = useState<"none" | "key-dir" | "key-map">("none");
  const [keyDirSrc, setKeyDirSrc] = useState("");
  const [keyMapSpec, setKeyMapSpec] = useState("");
  const [caCertSrc, setCaCertSrc] = useState("");
  // Max jobs, sandbox caps, and checkout policy moved to the per-runner Settings
  // drawer (Phase 4). The helper keeps only the install-time secret-file inputs.
  const [checkoutTokenFile, setCheckoutTokenFile] = useState("");
  const [vaultPasswordFile, setVaultPasswordFile] = useState("");

  // The verbatim env skeleton, fetched once on first expand (same bytes the
  // manuals plugin publishes into dist from backend/deploy).
  useEffect(() => {
    if (!open || example != null || exampleErr != null) return;
    fetch(ENV_EXAMPLE_PATH)
      .then((r) => (r.ok ? r.text() : Promise.reject(new Error(`HTTP ${r.status}`))))
      .then(setExample)
      .catch((e) => setExampleErr(String(e)));
  }, [open, example, exampleErr]);

  const opts: ProvisionOptions = {
    origin,
    token,
    name: name.trim(),
    capabilities: capsOverride ? caps : [],
    inventory,
    localInventorySrc: localInventorySrc.trim() || undefined,
    knownHostsSrc: knownHostsSrc.trim() || undefined,
    keyMode,
    keyDirSrc: keyDirSrc.trim() || undefined,
    keyMapSpec: keyMapSpec.trim() || undefined,
    caCertSrc: caCertSrc.trim() || undefined,
    // Max jobs, sandbox caps, and checkout POLICY (allow + repos) are now
    // server-managed per-runner (the ⚙ Settings drawer), not set at install.
    // Only the secret-file installs remain here (Phase 3).
    checkout: false,
    checkoutTokenFile: checkoutTokenFile.trim() || undefined,
    vaultPasswordFile: vaultPasswordFile.trim() || undefined,
  };

  // An override with nothing picked is the only invalid capability state —
  // auto-detect (no override) always generates.
  const capsValid = !capsOverride || caps.length > 0;
  let envText = "";
  let envErr: string | null = exampleErr;
  if (example != null && capsValid) {
    try {
      envText = generateRunnerEnv(example, opts);
    } catch (e) {
      envErr = String(e);
    }
  }
  const oneLiner = capsValid ? provisionOneLiner(opts) : "";
  const dockerCmd = capsValid ? provisionDockerRun(opts, serverVersion) : "";

  const inputStyle: React.CSSProperties = {
    padding: "6px 9px",
    background: c.panel2,
    border: `1px solid ${c.borderStrong}`,
    borderRadius: c.radiusChip,
    fontSize: c.fontSm,
    color: c.text,
    fontFamily: "inherit",
  };
  const rowStyle: React.CSSProperties = { display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" };
  // Explanatory caption under each option row, indented to align with the
  // controls (fieldLabel minWidth 76 + row gap 8).
  const hintStyle: React.CSSProperties = {
    fontSize: c.fontXs,
    color: c.textSec,
    lineHeight: 1.55,
    marginTop: 4,
    paddingLeft: 84,
    maxWidth: 900,
  };
  const artifactStyle: React.CSSProperties = {
    margin: 0,
    padding: "8px 12px",
    background: c.panel2,
    border: `1px solid ${c.border}`,
    borderRadius: c.radiusSurface,
    fontSize: c.fontXs,
    fontFamily: c.mono,
    color: c.textSec,
    whiteSpace: "pre-wrap",
    wordBreak: "break-all",
    maxHeight: 260,
    overflow: "auto",
    flex: 1,
  };

  const artifact = (
    label: string,
    text: string,
    note?: React.ReactNode,
  ) => (
    <div>
      <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 6 }}>
        <div style={{ ...fieldLabel(), minWidth: undefined }}>{label}</div>
        <CopyButton text={text} disabled={!text} ariaLabel={`Copy ${label} to clipboard`} />
      </div>
      <div style={{ display: "flex" }}>
        <pre style={artifactStyle}>{text}</pre>
      </div>
      {note && <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4, lineHeight: 1.5 }}>{note}</div>}
    </div>
  );

  return (
    <div style={{ background: c.panel, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, overflow: "hidden" }}>
      <div style={{ ...cardTitle(), display: "flex", alignItems: "center", justifyContent: "space-between" }}>
        <span>Advanced: pre-authored install</span>
        <button style={btnStyle()} onClick={() => setOpen((v) => !v)}>
          {open ? "Hide" : "Open the helper"}
        </button>
      </div>
      {open && (
        <div style={{ padding: 14, display: "flex", flexDirection: "column", gap: 12 }}>
          <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
            <strong style={{ color: c.text }}>Most installs just use ＋ Add Runner</strong> (top of the view) — one
            click, one line on the host, everything else auto-detected or tuned on the runner's row afterward. This
            helper is for <strong>pre-authoring</strong> a runner's install (config management, containers, or a
            fully-specified one-liner) when you want to lock down every choice up front.
            <div style={{ marginTop: 8 }}>Pick a profile to show just the fields it needs:</div>
            <div style={{ display: "flex", gap: 8, marginTop: 8, flexWrap: "wrap" }}>
              {(
                [
                  ["ssh", "SSH task runner", "bash/perl/python over SSH — key & known_hosts custody"],
                  ["ansible", "Ansible control node", "runs ansible locally — checkout/vault secret files"],
                  ["custom", "Custom", "every field"],
                ] as [ProvisionProfile, string, string][]
              ).map(([p, label, sub]) => (
                <button
                  key={p}
                  onClick={() => setProfile(p)}
                  title={sub}
                  style={{ ...btnStyle(profile === p ? "primary" : "default"), opacity: profile === p ? 1 : 0.75 }}
                >
                  {label}
                </button>
              ))}
            </div>
          </div>
          <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
            The workflow:
            <ol style={{ margin: "6px 0 0", paddingLeft: 22, display: "flex", flexDirection: "column", gap: 3 }}>
              <li>
                <strong style={{ color: c.text }}>Mint a registration token</strong> in the panel above
                (single-use, 24-hour expiry, one token per runner).{" "}
                {token === "<TOKEN>" ? (
                  <span style={{ color: c.warning }}>
                    No token minted this session — the artifacts below carry a <code>&lt;TOKEN&gt;</code>{" "}
                    placeholder you must replace before running them.
                  </span>
                ) : (
                  <>Your minted token is already filled in below.</>
                )}
              </li>
              <li>
                <strong style={{ color: c.text }}>Describe the runner</strong> with the options below — each is
                explained inline, and the{" "}
                <a href="/runner-install.html" target="_blank" rel="noopener noreferrer" style={{ color: c.primary }}>
                  Install Guide
                </a>{" "}
                and{" "}
                <a href="/runner-security.html" target="_blank" rel="noopener noreferrer" style={{ color: c.primary }}>
                  Security Guide
                </a>{" "}
                cover the background in depth.
              </li>
              <li>
                <strong style={{ color: c.text }}>Copy ONE artifact</strong> — they are three alternative install
                paths for the same choices, not sequential steps: the one-liner (systemd service on a Linux
                host), the <code>runner.env</code> (manual install or config management), or the{" "}
                <code>docker run</code> (container).
              </li>
              <li>
                <strong style={{ color: c.text }}>Run it on the runner host</strong> as root. The host only
                needs outbound HTTPS to <code>{origin}</code> — the agent polls the server, so no inbound ports
                are opened.
              </li>
              <li>
                <strong style={{ color: c.text }}>Verify</strong> — within about a minute the runner appears in
                the Runner Registry above; press <strong>Test</strong> on its row to confirm it is checking in.
              </li>
            </ol>
          </div>

          <div>
            <div style={rowStyle}>
              <div style={fieldLabel()}>Name</div>
              <input
                style={{ ...inputStyle, width: 200 }}
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="$(hostname)"
              />
            </div>
            <div style={hintStyle}>
              The name shown in the registry — it must stay <strong>stable across restarts</strong> (the saved
              identity file re-pairs to the same registry row by it). Empty ⇒ the host's hostname.{" "}
              <strong>Max jobs, sandbox caps, and checkout policy are now set per-runner on its row</strong>{" "}
              (the <strong>⚙ Settings</strong> button) after it registers — and take effect on the next poll, no
              reinstall (v0.47.12).
            </div>
          </div>

          <div>
            <div style={rowStyle}>
              <div style={fieldLabel()}>Capabilities</div>
              <button
                onClick={() => setCapsOverride(false)}
                style={{ ...btnStyle(capsOverride ? "default" : "primary"), opacity: capsOverride ? 0.7 : 1 }}
              >
                auto-detect on the host ★
              </button>
              <button
                onClick={() => setCapsOverride(true)}
                style={{ ...btnStyle(capsOverride ? "primary" : "default"), opacity: capsOverride ? 1 : 0.7 }}
              >
                override
              </button>
              {capsOverride &&
                RUN_TYPES.map((rt) => {
                  const on = caps.includes(rt);
                  const fat = FAT_RUN_TYPES.has(rt);
                  return (
                    <button
                      key={rt}
                      onClick={() => setCaps(on ? caps.filter((x) => x !== rt) : [...caps, rt])}
                      title={fat ? "Local toolchain — needs the fat image / an ansible-capable host" : "SSH-onward"}
                      style={{
                        ...btnStyle(on ? "primary" : "default"),
                        opacity: on ? 1 : 0.7,
                        borderStyle: fat ? "dashed" : "solid",
                      }}
                    >
                      {rt}
                    </button>
                  );
                })}
              {capsOverride && caps.length === 0 && (
                <span style={{ fontSize: c.fontXs, color: c.danger }}>pick at least one run-type</span>
              )}
            </div>
            <div style={hintStyle}>
              The run-types this runner claims — jobs of other types are never offered to it.{" "}
              <strong>Auto-detect</strong> (recommended): the agent probes the host's toolchains at startup and
              claims what it finds — install a toolchain later and a restart picks it up. <strong>Override</strong>{" "}
              to narrow the claim set (e.g. the host has python but must not run python jobs). Solid buttons
              (bash, perl, powershell, python) execute <strong>over SSH on the target hosts</strong>; the runner
              needs no local toolchain (slim container image). Dashed buttons (ansible, terraform) run{" "}
              <strong>local toolchains on the runner itself</strong> — use the fat image, or a host with the
              tooling installed.
            </div>
          </div>

          <div>
            <div style={rowStyle}>
              <div style={fieldLabel()}>Inventory</div>
              <select
                style={inputStyle}
                value={inventory}
                onChange={(e) => setInventory(e.target.value as "cronomicon" | "local")}
              >
                <option value="cronomicon">cronomicon — server resolves targets</option>
                <option value="local">local — runner resolves its own inventory</option>
              </select>
              {inventory === "local" && (
                <>
                  <input
                    style={{ ...inputStyle, width: 280 }}
                    value={localInventorySrc}
                    onChange={(e) => setLocalInventorySrc(e.target.value)}
                    placeholder="inventory.json path on the installing host"
                  />
                  {!localInventorySrc.trim() && (
                    <span style={{ fontSize: c.fontXs, color: c.warning }}>
                      required — the installer rejects --inventory local without it
                    </span>
                  )}
                </>
              )}
            </div>
            <div style={hintStyle}>
              Who turns a job's scope into concrete hosts. <strong>cronomicon</strong> (the default): the server
              resolves targets and ships them with each job — pick this unless you know otherwise.{" "}
              <strong>local</strong>: the runner resolves scopes against its own <code>inventory.json</code> —
              for network segments only the runner can see (the server never learns those hosts). Local mode
              needs the inventory file's path on the installing host; the installer copies it into place.
            </div>
          </div>

          {profile === "ansible" && !sshExpanded && (
            <button
              onClick={() => setSshExpanded(true)}
              style={{ alignSelf: "flex-start", background: "none", border: "none", padding: 0, color: c.primary, cursor: "pointer", fontSize: c.fontSm, textDecoration: "underline" }}
            >
              ＋ SSH custody (keys / known_hosts) — advanced
            </button>
          )}
          {showSSHCustody && (
          <div>
            <div style={rowStyle}>
              <div style={fieldLabel()}>SSH custody</div>
              <input
                style={{ ...inputStyle, width: 250 }}
                value={knownHostsSrc}
                onChange={(e) => setKnownHostsSrc(e.target.value)}
                placeholder="known_hosts file to install (path on host)"
              />
              <select style={inputStyle} value={keyMode} onChange={(e) => setKeyMode(e.target.value as typeof keyMode)}>
                <option value="none">no keys via installer</option>
                <option value="key-dir">key directory</option>
                <option value="key-map">key map (NAME=path,…)</option>
              </select>
              {keyMode === "key-dir" && (
                <input
                  style={{ ...inputStyle, width: 220 }}
                  value={keyDirSrc}
                  onChange={(e) => setKeyDirSrc(e.target.value)}
                  placeholder="directory of key files"
                />
              )}
              {keyMode === "key-map" && (
                <input
                  style={{ ...inputStyle, width: 300 }}
                  value={keyMapSpec}
                  onChange={(e) => setKeyMapSpec(e.target.value)}
                  placeholder="PROD_KEY=/path/prod.pem,DB_KEY=/path/db.pem"
                />
              )}
              <input
                style={{ ...inputStyle, width: 220 }}
                value={caCertSrc}
                onChange={(e) => setCaCertSrc(e.target.value)}
                placeholder="private CA bundle (PEM), optional"
              />
            </div>
            <div style={hintStyle}>
              The runner holds its <strong>own</strong> SSH credentials — Cronomicon never ships key bytes. All
              paths here are on the installing host; the installer copies the files into the standard
              locations. <strong>known_hosts</strong> is required before this runner's first SSH job: without it
              the agent refuses to connect (strict host-key verification, no trust-on-first-use). Provide keys
              as a <strong>directory</strong> (a target's key name matches a <code>&lt;NAME&gt;.pem/.key</code>{" "}
              file) or an explicit <strong>NAME=path map</strong>; "no keys via installer" is fine when keys are
              staged separately or supplied via an ssh-agent. The <strong>CA bundle</strong> is only needed when
              the Cronomicon server's TLS cert is signed by a private CA.
            </div>
          </div>
          )}

          {showAnsibleSecrets && (
          <div>
            <div style={rowStyle}>
              <div style={fieldLabel()}>Ansible secrets</div>
              <input
                style={{ ...inputStyle, width: 280 }}
                value={checkoutTokenFile}
                onChange={(e) => setCheckoutTokenFile(e.target.value)}
                placeholder="deploy-token file to install (path on this host)"
              />
              <input
                style={{ ...inputStyle, width: 250 }}
                value={vaultPasswordFile}
                onChange={(e) => setVaultPasswordFile(e.target.value)}
                placeholder="vault password file to install (path on this host)"
              />
            </div>
            <div style={hintStyle}>
              Leave blank for non-ansible runners. These are <strong>source files on this host</strong> the
              installer copies to <code>/etc/cronomicon-runner/</code> at <code>0640</code> — the server never ships
              secret bytes. The checkout <strong>policy</strong> (allow-checkout + the repo allowlist), max jobs,
              and sandbox caps are no longer set here: enable and tune them on the runner's row (
              <strong>⚙ Settings</strong>) once it registers, and they apply on the next poll (v0.47.12). Register
              the vault password as an Cronomicon stored secret too, so log redaction masks it.
            </div>
          </div>
          )}

          {envErr && (
            <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
              <div style={{ color: c.danger, fontSize: c.fontSm }}>Could not load the env template: {envErr}</div>
              <button
                style={btnStyle()}
                onClick={() => {
                  setExampleErr(null); // the fetch effect re-runs on the cleared error
                  setExample(null);
                }}
              >
                Retry
              </button>
            </div>
          )}

          {capsValid && (
            <>
              <div style={{ borderTop: `1px solid ${c.border}`, paddingTop: 12, fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
                <strong style={{ color: c.text }}>Copy ONE of the artifacts below</strong> — whichever matches how
                you're installing. All three encode the same choices; they update live as you edit the options
                above.
              </div>
              {artifact("Install one-liner — systemd service on a Linux host (recommended)",
                oneLiner,
                <>
                  Run as root on the runner host: downloads the agent binary from this server, creates the
                  service user and <code>cronomicon-runner</code> systemd unit, installs any referenced files
                  (known_hosts, keys, inventory, checkout token, vault password) into the standard paths, writes
                  the env, and starts the agent. Max jobs, sandbox caps, and checkout policy are then tuned on the
                  runner's row — no env merge, no restart.
                </>,
              )}
              {artifact("runner.env — manual install or config management",
                envText,
                <>
                  The annotated template with your choices applied — for hand-rolled installs or pushing via
                  Ansible/Puppet. Place it at <code>/etc/cronomicon-runner/runner.env</code> (mode 0640) and stage
                  any referenced files at the standard paths it names; the Install Guide's manual section covers
                  the service unit.
                </>,
              )}
              {artifact("docker run — container deployment",
                dockerCmd,
                <>
                  {capsOverride ? (
                    <>Image is derived from the capability pick ({dockerCmd.includes("-fat:") ? "fat" : "slim"});</>
                  ) : (
                    <>
                      With auto-detect the agent claims the image's toolchains (slim shown; switch to{" "}
                      <code>cronomicon-runner-fat</code> for ansible/terraform);
                    </>
                  )}{" "}
                  identity and keys persist on the named volume. Place any referenced files (known_hosts, keys,
                  inventory, tokens) onto the volume before starting.
                </>,
              )}
              <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
                <strong style={{ color: c.text }}>After the install:</strong> the agent registers itself with the
                token, then polls every 60 seconds. It appears in the Runner Registry above within about a
                minute — press <strong>Test</strong> on its row to confirm, then run a job scoped to one of its
                capabilities. If it never appears, check <code>journalctl -u cronomicon-runner</code> (or{" "}
                <code>docker logs</code>) on the host; the usual causes are an expired/used token or the host
                not reaching <code>{origin}</code>.
              </div>
            </>
          )}
        </div>
      )}
    </div>
  );
}

// ── local UI bits ────────────────────────────────────────────────────────────


// Inventory-canonicality label (D8 / R7.4). `cronomicon` — Cronomicon resolves the
// scope→hosts targets and ships them in the manifest. `local` — the runner
// resolves hosts against its own inventory (network-isolated segments). Small,
// subtle chip matching the capability-tag styling above.
function InventoryChip({ mode }: { mode: "cronomicon" | "local" }) {
  const col = mode === "local" ? "#8466c4" : c.info;
  return (
    <span
      title={
        mode === "local"
          ? "Runner-local inventory: this runner resolves scope hosts against its own inventory (network-isolated segments)."
          : "Cronomicon-canonical inventory: Cronomicon resolves scope hosts and ships fully-resolved targets to this runner."
      }
      style={{
        display: "inline-flex",
        padding: "1px 6px",
        borderRadius: c.radiusChip,
        fontSize: c.fontXs,
        fontWeight: 600,
        background: `${col}1a`,
        color: col,
        border: `1px solid ${col}30`,
        letterSpacing: 0.3,
        whiteSpace: "nowrap",
      }}
    >
      {mode === "local" ? "Local inventory" : "Cronomicon inventory"}
    </span>
  );
}

function btnStyle(kind: "default" | "primary" | "danger" = "default"): React.CSSProperties {
  const color = kind === "danger" ? c.danger : kind === "primary" ? c.primary : c.text;
  return {
    padding: "4px 10px",
    fontSize: c.fontSm,
    fontWeight: 600,
    fontFamily: "inherit",
    background: kind === "default" ? c.panel2 : `${color}1f`,
    color,
    border: `1px solid ${kind === "default" ? c.border : color}`,
    borderRadius: c.radiusChip,
    cursor: "pointer",
    whiteSpace: "nowrap",
  };
}

// Top-bar buttons (functions so they re-read `c` on theme toggle). The guide
// links are secondary; Download uses the solid-primary fill of the "+ Create"
// buttons on other pages.
function topLinkBtnStyle(): React.CSSProperties {
  return {
    display: "inline-flex",
    alignItems: "center",
    gap: 6,
    padding: "8px 14px",
    fontSize: c.fontSm,
    fontWeight: 600,
    fontFamily: "inherit",
    textDecoration: "none",
    background: c.panel2,
    color: c.text,
    border: `1px solid ${c.border}`,
    borderRadius: c.radiusChip,
    cursor: "pointer",
    whiteSpace: "nowrap",
  };
}

function topDownloadBtnStyle(): React.CSSProperties {
  return {
    display: "inline-flex",
    alignItems: "center",
    gap: 7,
    padding: "8px 14px",
    fontSize: c.fontSm,
    fontWeight: 600,
    fontFamily: "inherit",
    textDecoration: "none",
    background: c.primary,
    color: c.onSolid,
    border: "none",
    borderRadius: c.radiusChip,
    cursor: "pointer",
    whiteSpace: "nowrap",
  };
}

// Token-bearing styles are FUNCTIONS so they re-read `c` on theme toggle.
const cardTitle = (): React.CSSProperties => ({
  padding: "10px 14px",
  borderBottom: `1px solid ${c.border}`,
  fontSize: c.fontSm,
  fontWeight: 600,
});

const fieldLabel = (): React.CSSProperties => ({
  fontSize: c.fontXs,
  fontFamily: c.sansCond,
  fontWeight: 600,
  color: c.textMuted,
  textTransform: "uppercase",
  letterSpacing: 0.7,
  minWidth: 76,
});

// Converged onto the canonical thStyle (condensed uppercase at fontXs, 12px 16px,
// textMuted).
const th = (): React.CSSProperties => ({
  textAlign: "left",
  padding: "12px 16px",
  borderBottom: `1px solid ${c.border}`,
  color: c.textMuted,
  fontFamily: c.sansCond,
  fontWeight: 600,
  fontSize: c.fontXs,
  textTransform: "uppercase",
  letterSpacing: 0.7,
  whiteSpace: "nowrap",
});

const td: React.CSSProperties = { padding: "10px 16px" };

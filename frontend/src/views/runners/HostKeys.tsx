import { Fragment, useEffect, useMemo, useRef, useState, type CSSProperties, type ReactNode } from "react";
import { api, csrfHeader, errMsg } from "../../api/client";
import { agenciesFor, useMyAccess } from "../../api/access";
import { useGet, rows } from "../../hooks";
import type { components } from "../../api/schema";
import { c } from "../../theme";
import { fmtInAppZone } from "../../utils/datetime";
import {
  AlertBanner,
  Btn,
  ConfirmDialog,
  CopyButton,
  EmptyCell,
  ExpandChevron,
  InlineLoading,
  Modal,
  Rule,
  SectionLabel,
  Select,
  TabBar,
  tdStyle,
  thStyle,
} from "../../components/ui";

// Runner host keys — the UI (SB band).
//
// A runner refuses any host that is not in its known_hosts file. Keys get there
// by an operator approving them, and there are three ways to produce a key to
// approve: have the runner scan a scope, have it scan typed hosts, or paste
// lines the operator already has. (A fourth, copying another runner's keys, is
// the same review on a different source.) Whatever the source, the operator
// lands on ONE screen — the review table below — and nothing is trusted until
// they have seen every fingerprint on it and pressed the button that says how
// many keys, on which runner.
//
// Two rules of the server's shape this file:
//
//   - "Approved in Cronomicon" and "present in the runner's known_hosts file"
//     are different facts from different sources (the ledger; the runner's own
//     report). They are shown in separately titled sections and never merged: a
//     key seeded on the host by hand is in the second and not the first, and an
//     approval that never landed is in the first and not the second.
//   - A key that differs from one already trusted is a CHANGED key. It is the
//     one row "select all" never ticks.

type Candidate = components["schemas"]["HostKeyCandidate"];
type Candidates = components["schemas"]["HostKeyCandidates"];
type BatchResult = components["schemas"]["HostKeyBatchResult"];
type LedgerRow = components["schemas"]["HostKeyLedgerRow"];
type RunnerHostKeys = components["schemas"]["RunnerHostKeys"];
type Coverage = components["schemas"]["ScopeHostKeyCoverage"];

/** The slice of a runner these components read. */
export interface KeyRunner {
  id: string;
  name: string;
  /** The local runner: this server. Its trust store is the record itself — an
   *  approved key is in force at once, and there is no known_hosts file to send
   *  it to or to read back. */
  local?: boolean;
}

interface ScopeLite {
  id?: string;
  scope: string;
  agencies?: { id: string }[];
  boundRunners?: { runnerId: string }[];
}

interface RunnerLite {
  id?: string;
  name: string;
  status?: string | null;
  agencies?: { id: string }[];
  ownerAgency?: { id?: string; name?: string };
  /** Per-row authority from the server: false means the caller does not own this runner. */
  canManage?: boolean;
  /** "server" for the local runner. */
  kind?: string;
}

const isReachable = (status?: string | null) => status === "online" || status === "degraded";

// ── Shared bits ──────────────────────────────────────────────────────────────

const mono = (): CSSProperties => ({ fontFamily: c.mono, fontVariantNumeric: "tabular-nums" });

function Chip({ tone, children, title }: { tone: "danger" | "warning" | "success" | "info" | "muted"; children: ReactNode; title?: string }) {
  const map = {
    danger: { color: c.danger, bg: c.dangerBg },
    warning: { color: c.warning, bg: c.warningBg },
    success: { color: c.success, bg: c.successBg },
    info: { color: c.info, bg: c.infoBg },
    muted: { color: c.textSec, bg: c.panel2 },
  }[tone];
  return (
    <span
      title={title}
      style={{
        display: "inline-block",
        padding: "1px 7px",
        borderRadius: c.radiusChip,
        fontSize: c.fontXs,
        fontFamily: c.sansCond,
        fontWeight: 600,
        letterSpacing: 0.4,
        textTransform: "uppercase",
        whiteSpace: "nowrap",
        color: map.color,
        background: map.bg,
      }}
    >
      {children}
    </span>
  );
}

/** A known_hosts host as a person reads it; a hashed entry has no name to show. */
function hostLabel(host: string, hashed?: boolean): ReactNode {
  if (hashed || host.startsWith("|1|")) return <span style={{ color: c.textSec, fontStyle: "italic" }}>hashed entry</span>;
  return <span style={mono()}>{host}</span>;
}

const SOURCE_LABEL: Record<string, string> = { scan: "Scanned", pasted: "Pasted", carried: "Carried" };

// These tables are lists of fingerprints, and they sit in dialogs and panels of
// very different widths. So the fingerprint gets a cell of its own that keeps
// to one line wherever there is room for one, with the facts about it (type,
// source, who approved it) on a quieter line beneath — rather than one column
// per fact, which is how the column that matters ends up off-screen.
const dense = (extra?: CSSProperties): CSSProperties => ({ ...tdStyle(), padding: "9px 12px", verticalAlign: "top", fontSize: c.fontSm, ...extra });
const denseTh = (extra?: CSSProperties): CSSProperties => ({ ...thStyle(), padding: "9px 12px", ...extra });
const under = (): CSSProperties => ({ marginTop: 2, fontFamily: c.sans, fontSize: c.fontXs, color: c.textSec });

function KeyCell({ fingerprint, keyType, note }: { fingerprint: string; keyType: string; note?: ReactNode }) {
  return (
    <>
      <div style={{ ...mono(), color: c.text, wordBreak: "break-all" }}>{fingerprint}</div>
      <div style={under()}>
        <span style={mono()}>{keyType}</span>
        {note && <> · {note}</>}
      </div>
    </>
  );
}

function HostCell({ host, hostName, hashed, note }: { host: string; hostName?: string | null; hashed?: boolean; note?: ReactNode }) {
  return (
    <>
      {hostName ? <strong>{hostName}</strong> : hostLabel(host, hashed)}
      {hostName && <div style={under()}>{hostLabel(host, hashed)}</div>}
      {note && <div style={under()}>{note}</div>}
    </>
  );
}

// ── The review table ─────────────────────────────────────────────────────────

/** One row of the review screen, whatever produced it. */
export interface ReviewRow {
  /** Stable within the list: the pending id for a scan, host+type otherwise. */
  key: string;
  host: string;
  hostName?: string | null;
  hashed?: boolean;
  keyType: string;
  fingerprint: string;
  source: string;
  status: string;
  previousFingerprint?: string;
  previousSource?: string;
  matchedServerPin?: boolean;
  error?: string;
  /** The pasted line, shown in place of a host on a line that could not be read. */
  input?: string;
}

const STATUS_ORDER: Record<string, number> = { changed: 1, new: 2, match: 3, trusted: 4 };
const rank = (r: ReviewRow) => (r.error ? 0 : (STATUS_ORDER[r.status] ?? 5));

/** Changed and unacceptable rows first, so what needs a decision is never below the fold. */
export function sortForReview(list: ReviewRow[]): ReviewRow[] {
  return [...list].sort(
    (a, b) => rank(a) - rank(b) || (a.hostName ?? a.host).localeCompare(b.hostName ?? b.host) || a.keyType.localeCompare(b.keyType),
  );
}

/** What "select all" may tick: everything that is not a changed key and not an error. */
export const bulkSelectable = (r: ReviewRow) => !r.error && r.status !== "changed";

/** The plain-text form of the list, for pasting into a ticket or comparing by hand. */
export function reviewAsText(list: ReviewRow[]): string {
  return sortForReview(list)
    .map((r) => [r.error ? "ERROR" : r.status.toUpperCase(), r.hostName ?? "", r.host, r.keyType, r.fingerprint].join("\t"))
    .join("\n");
}

function StatusCell({ r }: { r: ReviewRow }) {
  if (r.error) {
    return (
      <div>
        <Chip tone="danger">Cannot accept</Chip>
        <div style={{ marginTop: 3, fontSize: c.fontXs, color: c.danger }}>{r.error}</div>
      </div>
    );
  }
  if (r.status === "changed") {
    const whose = r.previousSource === "server" ? "the key the server pins for this host" : "the key this runner trusts now";
    return (
      <div>
        <Chip tone="danger">Changed</Chip>
        <div style={{ marginTop: 3, fontSize: c.fontXs, color: c.textSec }}>
          Differs from {whose}:
          <div style={{ ...mono(), wordBreak: "break-all" }}>{r.previousFingerprint}</div>
          {r.previousSource !== "server" && r.matchedServerPin && <div>It matches the server's pin.</div>}
        </div>
      </div>
    );
  }
  if (r.status === "match") return <Chip tone="success" title="The same key the server itself has pinned for this host">Matches server</Chip>;
  if (r.status === "trusted") return <Chip tone="muted" title="This runner already trusts exactly this key">Already trusted</Chip>;
  return <Chip tone="info" title="Nothing is known about this host's key yet">New</Chip>;
}

export function KeyReviewTable({
  list,
  selected,
  onChange,
  late,
}: {
  list: ReviewRow[];
  selected: Set<string>;
  onChange: (next: Set<string>) => void;
  /** Rows that arrived after the list was first shown; marked, and never ticked for the operator. */
  late?: Set<string>;
}) {
  const sorted = useMemo(() => sortForReview(list), [list]);
  const bulk = sorted.filter(bulkSelectable);
  const allBulk = bulk.length > 0 && bulk.every((r) => selected.has(r.key));
  const toggle = (key: string) => {
    const next = new Set(selected);
    if (next.has(key)) next.delete(key);
    else next.add(key);
    onChange(next);
  };
  const toggleAll = () => {
    const next = new Set(selected);
    for (const r of bulk) {
      if (allBulk) next.delete(r.key);
      else next.add(r.key);
    }
    onChange(next);
  };
  return (
    <div style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, overflowX: "auto" }}>
      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr>
            <th style={denseTh({ width: 30 })}>
              <input
                type="checkbox"
                checked={allBulk}
                disabled={bulk.length === 0}
                onChange={toggleAll}
                aria-label="Select all keys except changed ones"
                title="Selects every key except changed ones — a changed key is ticked on its own"
              />
            </th>
            <th style={denseTh()}>Host</th>
            <th style={denseTh()}>Key</th>
            <th style={denseTh()}>Status</th>
          </tr>
        </thead>
        <tbody>
          {sorted.map((r) => (
            <tr key={r.key} style={{ background: r.error || r.status === "changed" ? c.dangerBg : undefined }}>
              <td style={dense()}>
                <input
                  type="checkbox"
                  checked={selected.has(r.key)}
                  disabled={!!r.error}
                  onChange={() => toggle(r.key)}
                  aria-label={`Select ${r.hostName ?? (r.host || "unreadable line")} ${r.keyType}`}
                />
              </td>
              {r.error && !r.fingerprint ? (
                // A line that could not be read has no host and no key to show:
                // show the line, so the operator can find it in what they pasted.
                <td colSpan={2} style={dense({ ...mono(), color: c.textSec, wordBreak: "break-all" })}>
                  {r.input || <EmptyCell />}
                </td>
              ) : (
                <>
                  <td style={dense({ minWidth: 130 })}>
                    <HostCell host={r.host} hostName={r.hostName} hashed={r.hashed} />
                  </td>
                  <td style={dense()}>
                    <KeyCell fingerprint={r.fingerprint} keyType={r.keyType} note={r.source} />
                  </td>
                </>
              )}
              <td style={dense({ width: 230 })}>
                <StatusCell r={r} />
                {late?.has(r.key) && <div style={under()}>Arrived after this list opened</div>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

const fromCandidate = (source: string) => (cand: Candidate): ReviewRow => ({
  key: cand.id ?? `${cand.host}\u0001${cand.keyType}\u0001${cand.input ?? ""}`,
  host: cand.host,
  hostName: cand.hostName,
  hashed: cand.hashed,
  keyType: cand.keyType,
  fingerprint: cand.fingerprint,
  source,
  status: cand.status,
  previousFingerprint: cand.previousFingerprint,
  previousSource: cand.previousSource,
  matchedServerPin: cand.matchedServerPin,
  error: cand.error,
  input: cand.input,
});

// ── The dialog ───────────────────────────────────────────────────────────────

export type KeySource = "scope" | "hosts" | "paste" | "carry";

const SOURCES: { k: KeySource; label: string }[] = [
  { k: "scope", label: "Scan a scope" },
  { k: "hosts", label: "Scan hosts" },
  { k: "paste", label: "Paste keys" },
  { k: "carry", label: "Copy from a runner" },
];

const textareaStyle = (): CSSProperties => ({
  width: "100%",
  minHeight: 110,
  padding: "8px 10px",
  background: c.panel2,
  border: `1px solid ${c.borderStrong}`,
  borderRadius: c.radiusChip,
  fontSize: c.fontSm,
  fontFamily: c.mono,
  color: c.text,
  resize: "vertical",
  boxSizing: "border-box",
});

const prose = (): CSSProperties => ({ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 });

/**
 * HostKeysDialog gets keys onto one runner. It has two steps: choose where the
 * keys come from, then review every fingerprint and accept.
 *
 * `startAt: "review"` opens straight on the keys already waiting for this
 * runner (the Runners page's "awaiting review" banner).
 */
export function HostKeysDialog({
  runner,
  initialSource = "scope",
  initialScopeId,
  initialCarryFrom,
  carryFromPrevious,
  startAt = "source",
  guest: guestProp = false,
  onClose,
  onChanged,
}: {
  runner: KeyRunner;
  /**
   * LR-63 — the caller does not own this runner; they administer an agency it
   * serves. They may scan a scope of their own agency with it and decide the
   * keys that scan finds: no typed hosts, no paste, no copy from another
   * runner, and never a key that would replace one the runner trusts.
   */
  guest?: boolean;
  initialSource?: KeySource;
  initialScopeId?: string;
  initialCarryFrom?: string;
  /** A deregistered runner whose keys may be copied: this runner's own previous registration, or the runner it replaced. */
  carryFromPrevious?: KeyRunner;
  startAt?: "source" | "review";
  onClose: () => void;
  onChanged?: () => void;
}) {
  const [pickedSource, setSource] = useState<KeySource>(initialSource);
  const [step, setStep] = useState<"source" | "review">(startAt);
  // What the review step is showing: keys the runner scanned (read live from
  // the pending list) or a fixed list the server parsed from a paste or a carry.
  const [reviewing, setReviewing] = useState<"scan" | "paste" | "carry">("scan");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [outcome, setOutcome] = useState<string | null>(null);

  // Source inputs.
  const scopesQ = useGet<unknown>(() => api.GET("/scopes"), []);
  const runnersQ = useGet<unknown>(() => api.GET("/runners"), []);
  const me = useMemo(() => rows<RunnerLite>(runnersQ.data).find((r) => r.id === runner.id), [runnersQ.data, runner.id]);
  // The local runner: this server. Known from the caller, or from the list
  // when the dialog was opened somewhere that does not know (the Scopes page).
  const local = !!runner.local || me?.kind === "server";
  // A scan needs the runner to answer. Unknown (the list has not loaded, or the
  // runner is not in it) is not treated as offline: the server has the last word.
  // The local runner scans from this server whether it is turned on or not:
  // scanning is not running a job.
  const reachable = local || !me || isReachable(me.status);
  // LR-63 — guest mode is the server's per-row answer about THIS runner, so it
  // holds wherever the dialog is opened from (the Runners page says so up
  // front; the Scopes page's coverage panel does not know, and need not).
  const guest = guestProp || me?.canManage === false;
  const source: KeySource = guest ? "scope" : pickedSource;
  const access = useMyAccess();
  const scopes = useMemo(() => {
    const mine = (s: ScopeLite) => (s.boundRunners ?? []).some((b) => b.runnerId === runner.id);
    // Only the scopes this runner serves: a scope takes a runner that serves
    // its agency (Global's takes one that serves Global). The server enforces
    // the same rule; offering the others would only offer a refusal.
    const mayServe = (s: ScopeLite) => {
      if (!me) return true;
      const mineAg = new Set((me.agencies ?? []).map((a) => a.id));
      return (s.agencies ?? []).some((a) => mineAg.has(a.id));
    };
    // A guest may scan the scopes of an agency they ADMINISTER that the runner
    // serves and does not own — the server's own list (hostKeyGuestScopes).
    // Reading a scope is not enough.
    const held = new Set(agenciesFor(access ?? null, "configureApp").map((a) => a.id));
    const guestMay = (s: ScopeLite) =>
      !guest || access == null || (s.agencies ?? []).some((a) => held.has(a.id) && a.id !== me?.ownerAgency?.id);
    const all = rows<ScopeLite>(scopesQ.data).filter((s) => s.id && guestMay(s) && (mayServe(s) || s.id === initialScopeId));
    // Scopes bound to this runner first: they are the ones it must be able to reach.
    return [...all].sort((a, b) => Number(mine(b)) - Number(mine(a)) || a.scope.localeCompare(b.scope));
  }, [scopesQ.data, runner.id, me, initialScopeId, guest, access]);
  const boundHere = (s: ScopeLite) => (s.boundRunners ?? []).some((b) => b.runnerId === runner.id);
  const others = useMemo(() => {
    const live: RunnerLite[] = rows<RunnerLite>(runnersQ.data).filter((r) => r.id && r.id !== runner.id);
    // A deregistered source is not in the fleet list; its record outlives it.
    if (carryFromPrevious && !live.some((r) => r.id === carryFromPrevious.id)) {
      return [{ id: carryFromPrevious.id, name: `${carryFromPrevious.name} (no longer registered)` }, ...live];
    }
    return live;
  }, [runnersQ.data, runner.id, carryFromPrevious]);

  const [scopeId, setScopeId] = useState(initialScopeId ?? "");
  const [hosts, setHosts] = useState("");
  const [pasted, setPasted] = useState("");
  const [carryFrom, setCarryFrom] = useState(initialCarryFrom ?? carryFromPrevious?.id ?? "");
  useEffect(() => {
    if (!scopeId && scopes.length > 0) setScopeId(String(scopes[0].id));
  }, [scopes, scopeId]);
  useEffect(() => {
    if (!carryFrom && others.length > 0) setCarryFrom(String(others[0].id));
  }, [others, carryFrom]);

  // What the last scan request queued, for the "waiting for the runner" line.
  const [queued, setQueued] = useState<{ hosts: string[]; skipped: { host: string; reason: string; pattern: string }[] } | null>(null);
  const [candidates, setCandidates] = useState<Candidates | null>(null);

  // The pending list, polled while the review step is showing scanned keys.
  const [tick, setTick] = useState(0);
  const pendingQ = useGet<Candidate[]>(
    () => api.GET("/runners/{runnerId}/host-keys/pending", { params: { path: { runnerId: runner.id } } }),
    [runner.id, tick],
    step === "review" && reviewing === "scan" ? 3000 : undefined,
  );

  const list: ReviewRow[] = useMemo(() => {
    if (reviewing === "scan") return (Array.isArray(pendingQ.data) ? pendingQ.data : []).map(fromCandidate(SOURCE_LABEL.scan));
    return (candidates?.candidates ?? []).map(fromCandidate(reviewing === "paste" ? SOURCE_LABEL.pasted : SOURCE_LABEL.carried));
  }, [reviewing, pendingQ.data, candidates]);

  // Selection. The list is ticked by rule ONCE, when the review step first has
  // a list to show — everything except changed keys and unreadable lines. A key
  // that arrives afterwards (a slow host answering, the 3s poll) is marked and
  // left UNTICKED: the button says "Trust N keys", and N must not grow between
  // the operator reading the list and pressing it.
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [late, setLate] = useState<Set<string>>(new Set());
  const primed = useRef(false);
  const known = useRef<Set<string>>(new Set());
  const resetReview = () => {
    primed.current = false;
    known.current = new Set();
    setSelected(new Set());
    setLate(new Set());
  };
  const scanLoading = reviewing === "scan" && pendingQ.loading;
  useEffect(() => {
    if (step !== "review" || scanLoading) return;
    const fresh = list.filter((r) => !known.current.has(r.key));
    fresh.forEach((r) => known.current.add(r.key));
    if (!primed.current) {
      primed.current = true;
      setSelected(new Set(list.filter(bulkSelectable).map((r) => r.key)));
    } else if (fresh.length > 0) {
      setLate((l) => new Set([...l, ...fresh.map((r) => r.key)]));
    }
  }, [list, step, scanLoading]);

  const chosen = list.filter((r) => selected.has(r.key) && !r.error);
  const chosenChanged = chosen.filter((r) => r.status === "changed").length;
  const lateUnticked = list.filter((r) => late.has(r.key) && !selected.has(r.key) && bulkSelectable(r)).length;
  const hasErrors = list.some((r) => r.error);
  const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`;

  const describe = (res: BatchResult): string => {
    const parts: string[] = [];
    if (res.approved) parts.push(`${plural(res.approved, "key")} trusted`);
    if (res.rejected) parts.push(`${plural(res.rejected, "key")} rejected`);
    if (res.unchanged) parts.push(`${res.unchanged} already trusted`);
    const tail = !res.approved
      ? ""
      : local
        ? ` ${res.approved === 1 ? "It is" : "They are"} in force now.`
        : ` ${runner.name} receives ${res.approved === 1 ? "it" : "them"} on its next poll.`;
    return (parts.join(", ") || "Nothing changed") + "." + tail;
  };

  const fail = (e: unknown) => {
    setBusy(false);
    setErr(errMsg(e));
  };

  // ── Step 1 actions ──
  const queueScan = async (body: { scopeId: string } | { hosts: string[] }) => {
    setBusy(true);
    setErr(null);
    const { data, error } = await api.POST("/runners/{runnerId}/keyscan", {
      params: { path: { runnerId: runner.id }, header: csrfHeader },
      body,
    });
    if (error) return fail(error);
    setBusy(false);
    setQueued({ hosts: data?.hosts ?? [], skipped: data?.skipped ?? [] });
    resetReview();
    setReviewing("scan");
    setStep("review");
    setTick((n) => n + 1);
  };

  const preview = async (kind: "paste" | "carry") => {
    setBusy(true);
    setErr(null);
    const { data, error } =
      kind === "paste"
        ? await api.POST("/runners/{runnerId}/host-keys/provide", {
            params: { path: { runnerId: runner.id }, header: csrfHeader },
            body: { lines: [pasted], dryRun: true },
          })
        : await api.POST("/runners/{runnerId}/host-keys/carry", {
            params: { path: { runnerId: runner.id }, query: { from: carryFrom }, header: csrfHeader },
            body: { dryRun: true },
          });
    if (error) return fail(error);
    setBusy(false);
    setCandidates(data as Candidates);
    resetReview();
    setReviewing(kind);
    setStep("review");
  };

  const next = () => {
    if (source === "scope") {
      if (!scopeId) return setErr("Choose a scope.");
      return queueScan({ scopeId });
    }
    if (source === "hosts") {
      const typed = hosts.split(/[\s,]+/).map((h) => h.trim()).filter(Boolean);
      if (typed.length === 0) return setErr("Enter at least one host.");
      return queueScan({ hosts: typed });
    }
    if (source === "paste") {
      if (!pasted.trim()) return setErr("Paste at least one known_hosts line.");
      return preview("paste");
    }
    if (!carryFrom) return setErr("Choose the runner to copy from.");
    return preview("carry");
  };

  // ── Step 2 actions ──
  const finish = (res: BatchResult) => {
    setBusy(false);
    setOutcome(describe(res));
    onChanged?.();
    if (reviewing === "scan") {
      setTick((n) => n + 1);
    } else {
      setCandidates(null);
    }
  };

  const accept = async () => {
    setBusy(true);
    setErr(null);
    if (reviewing === "scan") {
      const { data, error } = await api.POST("/runners/{runnerId}/host-keys/resolve-batch", {
        params: { path: { runnerId: runner.id }, header: csrfHeader },
        // A changed key is named twice: approved, and acknowledged as a change.
        // The server refuses one that turns out to replace a trusted key
        // without having been ticked as such.
        body: { approve: chosen.map((r) => r.key), acknowledgeChanged: chosen.filter((r) => r.status === "changed").map((r) => r.key) },
      });
      if (error) return fail(error);
      return finish(data as BatchResult);
    }
    // Each row by what was on screen: the server commits these fingerprints or nothing.
    const select = chosen.map((r) => ({ host: r.host, keyType: r.keyType, fingerprint: r.fingerprint, status: r.status }));
    const { data, error } =
      reviewing === "paste"
        ? await api.POST("/runners/{runnerId}/host-keys/provide", {
            params: { path: { runnerId: runner.id }, header: csrfHeader },
            body: { lines: [pasted], select },
          })
        : await api.POST("/runners/{runnerId}/host-keys/carry", {
            params: { path: { runnerId: runner.id }, query: { from: carryFrom }, header: csrfHeader },
            body: { select },
          });
    if (error) return fail(error);
    finish(data as BatchResult);
  };

  const reject = async () => {
    setBusy(true);
    setErr(null);
    const { data, error } = await api.POST("/runners/{runnerId}/host-keys/resolve-batch", {
      params: { path: { runnerId: runner.id }, header: csrfHeader },
      body: { reject: chosen.map((r) => r.key) },
    });
    if (error) return fail(error);
    finish(data as BatchResult);
  };

  const back = () => {
    setStep("source");
    setErr(null);
    setOutcome(null);
    setQueued(null);
    setCandidates(null);
    resetReview();
  };

  // ── Render ──
  const carryName = others.find((r) => r.id === carryFrom)?.name ?? carryFrom;
  const nothingLeft = step === "review" && list.length === 0;
  const waiting = reviewing === "scan" && !!queued && queued.hosts.length > 0 && !outcome;
  const scanning = source === "scope" || source === "hosts";
  const offlineForScan = scanning && !reachable;

  const footer =
    step === "source" ? (
      <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
        {err && <div role="alert" style={{ color: c.danger, fontSize: c.fontSm }}>{err}</div>}
        <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
          <Btn onClick={onClose} disabled={busy}>Cancel</Btn>
          <Btn
            primary
            onClick={next}
            disabled={busy || offlineForScan}
            title={
              offlineForScan
                ? guest
                  ? `${runner.name} is not online, so it cannot scan. Try again when it is back, or ask its owner.`
                  : `${runner.name} is not online, so it cannot scan. Paste keys instead: they are delivered when it returns.`
                : undefined
            }
          >
            {busy ? "Working…" : scanning ? "Queue scan" : "Review keys"}
          </Btn>
        </div>
      </div>
    ) : (
      <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
        {err && <div role="alert" style={{ color: c.danger, fontSize: c.fontSm }}>{err}</div>}
        {guest && local && (
          <div role="alert" style={{ color: c.textSec, fontSize: c.fontSm }}>
            These are waiting for a global administrator. The local runner is this server, and the keys it trusts are the same for
            every agency it serves, so its keys are approved by a global administrator and not by one agency's.
          </div>
        )}
        {guest && !local && chosenChanged > 0 && (
          <div role="alert" style={{ color: c.danger, fontSize: c.fontSm }}>
            {plural(chosenChanged, "selected key")} would replace a key {runner.name} already trusts. That is for the runner's owner
            to decide; untick {chosenChanged === 1 ? "it" : "them"} to trust the rest.
          </div>
        )}
        {!guest && chosenChanged > 0 && (
          <div style={{ color: c.danger, fontSize: c.fontSm }}>
            {plural(chosenChanged, "selected key")} {chosenChanged === 1 ? "differs" : "differ"} from a key already trusted. Accepting{" "}
            {chosenChanged === 1 ? "it" : "them"} replaces the old key on {runner.name}. Only do this if you know why the host's key changed.
          </div>
        )}
        {reviewing === "paste" && hasErrors && (
          <div style={{ color: c.danger, fontSize: c.fontSm }}>
            Some lines cannot be accepted. Go back and correct or remove them: a paste is accepted whole or not at all.
          </div>
        )}
        {lateUnticked > 0 && (
          <div style={{ color: c.textSec, fontSize: c.fontSm }}>
            {plural(lateUnticked, "key")} arrived after this list opened and {lateUnticked === 1 ? "is" : "are"} not selected: nothing is
            ticked for you once you may be reading. Read {lateUnticked === 1 ? "it" : "them"}, then tick{" "}
            {lateUnticked === 1 ? "it" : "them"} or use the box at the top of the list.
          </div>
        )}
        <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
          {list.length > 0 && <CopyButton text={reviewAsText(list)} label="Copy as text" title="Copy this list as tab-separated text" />}
          <span style={{ flex: 1 }} />
          <Btn onClick={back} disabled={busy}>{nothingLeft ? "Add more keys" : "Back"}</Btn>
          {reviewing === "scan" && list.length > 0 && (
            <Btn dangerQuiet onClick={reject} disabled={busy || chosen.length === 0} title="Reject the selected keys; they are never trusted">
              Reject {chosen.length || ""} selected
            </Btn>
          )}
          {nothingLeft ? (
            <Btn primary onClick={onClose}>Done</Btn>
          ) : (
            <Btn primary onClick={accept} disabled={busy || chosen.length === 0 || (reviewing === "paste" && hasErrors) || (guest && chosenChanged > 0) || (guest && local)}>
              {busy ? "Working…" : `Trust ${plural(chosen.length, "key")} on ${runner.name}`}
            </Btn>
          )}
        </div>
      </div>
    );

  return (
    <Modal title={`Host keys — ${runner.name}`} onClose={onClose} wide={step === "source"} table={step === "review"} footer={footer}>
      {step === "source" ? (
        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          <div style={prose()}>
            {runner.name} only connects to hosts whose SSH key it has been told to trust. Choose where the keys come from. Nothing is
            trusted until you have reviewed every fingerprint on the next screen.
          </div>
          {guest ? (
            <AlertBanner type="info">
              {runner.name} is not your agency's runner, but it serves your agency. You can scan one of your agency's scopes with it
              and trust the keys it finds for hosts it does not know yet. Scanning typed hosts, pasting or copying keys, and replacing
              a key the runner already trusts are for the runner's owner.
            </AlertBanner>
          ) : (
            <TabBar
              tabs={SOURCES.map((s) => ({ label: s.label }))}
              active={SOURCES.findIndex((s) => s.k === source)}
              onChange={(i) => {
                setSource(SOURCES[i].k);
                setErr(null);
              }}
            />
          )}
          {offlineForScan && (
            <AlertBanner type="warning">
              {runner.name} is not online, so it cannot scan.{" "}
              {guest ? "Try again when it is back, or ask its owner." : "Pasted keys do not need it: they are delivered when it returns."}
            </AlertBanner>
          )}
          {source === "scope" && (
            <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
              <div style={prose()}>
                The runner connects to every host of the scope from where it sits and reports the key each one presents. Hosts reached
                through a bastion cannot be scanned and are listed for you to provide by pasting; the bastion itself is scanned.
              </div>
              {scopesQ.loading ? (
                <InlineLoading what="scopes" />
              ) : scopes.length === 0 ? (
                <div style={prose()}>
                  There is no scope here that this runner serves. A scope takes a runner that serves its agency, and a scope that
                  is Global's takes a runner that serves Global.
                </div>
              ) : (
                <label style={{ display: "flex", flexDirection: "column", gap: 4, fontSize: c.fontSm, color: c.textSec }}>
                  Scope
                  <Select value={scopeId} onChange={(e) => setScopeId(e.target.value)} aria-label="Scope to scan">
                    {scopes.map((s) => (
                      <option key={s.id} value={s.id}>
                        {s.scope}
                        {boundHere(s) ? " (bound to this runner)" : ""}
                      </option>
                    ))}
                  </Select>
                </label>
              )}
            </div>
          )}
          {source === "hosts" && (
            <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
              <div style={prose()}>
                One per line, or separated by commas or spaces, as <code>host</code> or <code>host:port</code>. The runner connects to
                each and reports the key it presents.
              </div>
              <textarea
                style={textareaStyle()}
                value={hosts}
                onChange={(e) => setHosts(e.target.value)}
                placeholder={"web01\ndb01:2222"}
                aria-label="Hosts to scan"
              />
            </div>
          )}
          {source === "paste" && (
            <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
              <div style={prose()}>
                Paste <code>known_hosts</code> lines you already have, such as <code>ssh-keyscan</code> output you have checked or a key
                for a host behind a bastion. One line per key: <code>host key-type key</code>. The next screen shows the fingerprint of
                each before anything is trusted.
              </div>
              <textarea
                style={{ ...textareaStyle(), minHeight: 160 }}
                value={pasted}
                onChange={(e) => setPasted(e.target.value)}
                placeholder={"web01 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA…\n[db01]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA…"}
                aria-label="known_hosts lines"
                spellCheck={false}
              />
            </div>
          )}
          {source === "carry" && (
            <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
              <div style={prose()}>
                Copy the keys another runner trusts. Use this when {runner.name} replaces that runner. The two sit in different places on
                the network, so the same address is not guaranteed to be the same machine: review the list before accepting it.
              </div>
              {others.length === 0 ? (
                <div style={prose()}>There is no other runner to copy from.</div>
              ) : (
                <label style={{ display: "flex", flexDirection: "column", gap: 4, fontSize: c.fontSm, color: c.textSec }}>
                  Copy from
                  <Select value={carryFrom} onChange={(e) => setCarryFrom(e.target.value)} aria-label="Runner to copy keys from">
                    {/* A source handed in by the caller may be a runner that is no longer registered. */}
                    {carryFrom && !others.some((r) => r.id === carryFrom) && <option value={carryFrom}>{carryFrom} (no longer registered)</option>}
                    {others.map((r) => (
                      <option key={r.id} value={r.id}>
                        {r.name}
                      </option>
                    ))}
                  </Select>
                </label>
              )}
            </div>
          )}
        </div>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
          {outcome && <AlertBanner type="success">{outcome}</AlertBanner>}
          {reviewing === "scan" && queued && (
            <div style={prose()}>
              {queued.hosts.length > 0 ? (
                <>
                  Asked {runner.name} to scan {plural(queued.hosts.length, "host")}. Keys appear here as it reports them, usually within a
                  few seconds. A host it cannot reach never appears.
                </>
              ) : (
                <>None of this scope's hosts can be scanned.</>
              )}
            </div>
          )}
          {reviewing === "scan" && queued && queued.skipped.length > 0 && (
            <AlertBanner type="warning">
              <div>
                <strong>{plural(queued.skipped.length, "host")} not scanned.</strong> Provide {queued.skipped.length === 1 ? "its key" : "their keys"}{" "}
                with <em>Paste keys</em>.
                <ul style={{ margin: "6px 0 0", paddingLeft: 18 }}>
                  {queued.skipped.map((s) => (
                    <li key={s.host}>
                      <strong>{s.host}</strong>: {s.reason}. Its key line must start with <span style={mono()}>{s.pattern}</span>, which
                      is the name {runner.name} looks it up under.
                    </li>
                  ))}
                </ul>
              </div>
            </AlertBanner>
          )}
          {reviewing === "carry" && candidates && (
            <div style={prose()}>
              Keys {carryName} trusts that {runner.name} does not.
              {candidates.unverifiable > 0 && (
                <>
                  {" "}
                  {plural(candidates.unverifiable, "key")} recorded before this version cannot be verified and{" "}
                  {candidates.unverifiable === 1 ? "is" : "are"} left out; scan {candidates.unverifiable === 1 ? "that host" : "those hosts"} from{" "}
                  {runner.name} instead.
                </>
              )}
            </div>
          )}
          {list.length > 0 ? (
            <>
              <div style={prose()}>
                <strong style={{ color: c.text }}>Compare each fingerprint with the host's own</strong> before accepting. Approving a key
                you have not checked trusts whatever answered on that address.
              </div>
              <KeyReviewTable list={list} selected={selected} onChange={setSelected} late={late} />
            </>
          ) : reviewing === "scan" && pendingQ.loading ? (
            <InlineLoading what="keys" />
          ) : reviewing === "scan" && pendingQ.error ? (
            // Not "no keys are waiting": the list could not be read, which for a
            // runner outside the caller's agency is the usual reason.
            <AlertBanner type="danger">The keys waiting for {runner.name} could not be loaded: {pendingQ.error}</AlertBanner>
          ) : (
            <div style={prose()}>
              {waiting
                ? "Waiting for the runner…"
                : outcome
                  ? "No keys are waiting for review."
                  : reviewing === "carry"
                    ? `${runner.name} already trusts every key ${carryName} does.`
                    : "No keys are waiting for review."}
            </div>
          )}
          {waiting && list.length > 0 && (
            <div style={{ fontSize: c.fontXs, color: c.textSec }}>This list updates by itself while {runner.name} scans.</div>
          )}
        </div>
      )}
    </Modal>
  );
}

// ── Keys awaiting review, across runners ─────────────────────────────────────

interface PendingRow {
  id: string;
  runnerId: string;
  runnerName: string;
}

/**
 * PendingKeysBanner says which runners have scanned keys waiting and opens the
 * review for one. It renders nothing when nothing is waiting.
 */
export function PendingKeysBanner({ refreshKey, onChanged }: { refreshKey: number; onChanged?: () => void }) {
  const q = useGet<PendingRow[]>(() => api.GET("/runners/host-keys/pending"), [refreshKey]);
  const [reviewing, setReviewing] = useState<KeyRunner | null>(null);
  const dirty = useRef(false);
  const groups = useMemo(() => {
    const by = new Map<string, { runner: KeyRunner; count: number }>();
    for (const p of Array.isArray(q.data) ? q.data : []) {
      const g = by.get(p.runnerId) ?? { runner: { id: p.runnerId, name: p.runnerName }, count: 0 };
      g.count++;
      by.set(p.runnerId, g);
    }
    return [...by.values()].sort((a, b) => a.runner.name.localeCompare(b.runner.name));
  }, [q.data]);
  const total = groups.reduce((n, g) => n + g.count, 0);
  // The dialog keeps ONE place in the tree whether or not the banner is showing:
  // approving the last waiting key empties the banner, and a dialog rendered
  // inside it would be unmounted in the middle of saying what it just did.
  return (
    <>
      {groups.length > 0 && (
        <div style={{ background: c.panel, border: `1px solid ${c.warning}`, borderRadius: c.radiusSurface, marginBottom: 16, overflow: "hidden" }}>
          <div style={{ padding: "10px 14px", display: "flex", alignItems: "center", gap: 8, borderBottom: `1px solid ${c.border}` }}>
            <span style={{ fontSize: c.fontBody, fontWeight: 600, color: c.text }}>Host keys awaiting review</span>
            <span style={{ fontSize: c.fontXs, color: c.warning }}>
              {total} {total === 1 ? "key" : "keys"}
            </span>
          </div>
          <div style={{ padding: "6px 14px 10px" }}>
            <div style={{ ...prose(), margin: "6px 0" }}>
              A runner scanned these hosts and is waiting for you to approve what it saw. Until you do, its runs on those hosts fail at
              the first connection.
            </div>
            {groups.map((g, i) => (
              <Fragment key={g.runner.id}>
                {i > 0 && <Rule />}
                <div style={{ display: "flex", alignItems: "center", gap: 12, padding: "8px 0" }}>
                  <div style={{ flex: 1, fontSize: c.fontSm, color: c.text }}>
                    <strong>{g.runner.name}</strong>{" "}
                    <span style={{ color: c.textSec }}>
                      {g.count} {g.count === 1 ? "key" : "keys"}
                    </span>
                  </div>
                  <Btn small primary onClick={() => setReviewing(g.runner)} ariaLabel={`Review host keys for ${g.runner.name}`}>
                    Review
                  </Btn>
                </div>
              </Fragment>
            ))}
          </div>
        </div>
      )}
      {reviewing && (
        <HostKeysDialog
          runner={reviewing}
          startAt="review"
          // The page is told when the dialog CLOSES: its answer is to reload
          // the runner list, which is not something to do under an open dialog.
          onClose={() => {
            setReviewing(null);
            q.refetch();
            if (dirty.current) onChanged?.();
            dirty.current = false;
          }}
          onChanged={() => {
            dirty.current = true;
            q.refetch();
          }}
        />
      )}
    </>
  );
}

// ── What one runner trusts ───────────────────────────────────────────────────

const when = (iso?: string | null) => (iso ? fmtInAppZone(iso) : "");

function FileState({ k, local }: { k: LedgerRow; local?: boolean }) {
  if (local) return <Chip tone="success" title="The local runner verifies against this record directly: an approved key is in force at once">In force</Chip>;
  if (!k.deliveredAt) return <Chip tone="info" title="The runner receives it on its next poll">Not yet sent</Chip>;
  if (k.presentInFile === true) return <Chip tone="success">In the file</Chip>;
  if (k.presentInFile === false) {
    return (
      <Chip tone="warning" title="The runner's last report of its known_hosts file does not include this key">
        Not in the file
      </Chip>
    );
  }
  return <Chip tone="muted" title="The runner has not reported its known_hosts file since this key was sent">Sent</Chip>;
}

/**
 * TrustedHostKeys is the runner's host-key panel: what was approved here, what
 * the runner's own file holds, and the way to add more.
 */
export function TrustedHostKeys({
  runner,
  canScan,
  previous,
  onChanged,
}: {
  runner: KeyRunner;
  canScan: boolean;
  /** This runner's previous registration, when it re-enrolled under a new id: its approvals can be copied back. */
  previous?: KeyRunner;
  onChanged?: () => void;
}) {
  const q = useGet<RunnerHostKeys>(
    () => api.GET("/runners/{runnerId}/host-keys", { params: { path: { runnerId: runner.id } } }),
    [runner.id],
  );
  const [dialog, setDialog] = useState<{ source: KeySource; review?: boolean } | null>(null);
  const [removing, setRemoving] = useState<LedgerRow | null>(null);
  const [showHistory, setShowHistory] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  // refetch(), not a dependency bump: a bump is a fresh load, which blanks the
  // panel to its loader — and would unmount the dialog below while it is still
  // telling the operator what it just did. For the same reason the PARENT is
  // told only when the dialog closes: the Runners page answers onChanged by
  // reloading its list, which rebuilds this whole row.
  const reload = () => q.refetch();
  const dirty = useRef(false);

  const act = async (run: () => Promise<{ error?: unknown }>, done: string) => {
    setBusy(true);
    setErr(null);
    setNote(null);
    const { error } = await run();
    setBusy(false);
    if (error) return setErr(errMsg(error));
    setNote(done);
    reload();
  };
  const path = (ledgerId: number) => ({ params: { path: { runnerId: runner.id, ledgerId: String(ledgerId) }, header: csrfHeader } });

  // Anything that is not the three lists is treated as no record, not rendered
  // as an empty one: an empty "approved" table would be a claim.
  if (!q.data || !Array.isArray(q.data.inForce) || !q.data.knownHosts) {
    if (q.loading) return <InlineLoading what="host keys" />;
    return <div style={{ fontSize: c.fontSm, color: c.textSec }}>{q.error ? `Host keys could not be loaded: ${q.error}` : "No host-key record."}</div>;
  }
  const { inForce, history, knownHosts, pending } = q.data;
  const missing = runner.local ? [] : inForce.filter((k) => k.deliveredAt && k.presentInFile === false);
  const subtle = (): CSSProperties => ({ fontSize: c.fontXs, color: c.textSec });

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 18 }}>
      <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}>
        <Btn
          small
          primary
          onClick={() => setDialog({ source: "scope" })}
          disabled={!canScan}
          title={canScan ? "Scan a scope or hosts, or paste keys" : "The runner must be online to scan. Pasting keys still works: choose Paste keys."}
        >
          Scan keys…
        </Btn>
        <Btn small onClick={() => setDialog({ source: "paste" })}>Paste keys…</Btn>
        <Btn small onClick={() => setDialog({ source: "carry" })} title="Copy the keys another runner trusts, after review">
          Copy from a runner…
        </Btn>
        {pending > 0 && (
          <Btn small onClick={() => setDialog({ source: "scope", review: true })}>
            Review {pending} waiting
          </Btn>
        )}
      </div>
      {err && <AlertBanner type="danger" onDismiss={() => setErr(null)}>{err}</AlertBanner>}
      {note && <AlertBanner type="success" onDismiss={() => setNote(null)}>{note}</AlertBanner>}
      {missing.length > 0 && (
        <AlertBanner type="warning">
          {missing.length} approved {missing.length === 1 ? "key is" : "keys are"} not in {runner.name}'s known_hosts file. Runs on{" "}
          {missing.length === 1 ? "that host" : "those hosts"} will fail. Use <em>Send again</em> on each.
        </AlertBanner>
      )}

      {/* ── What was approved here ── */}
      <div>
        <SectionLabel>Approved in Cronomicon ({inForce.length})</SectionLabel>
        <div style={{ ...subtle(), marginBottom: 8 }}>
          {runner.local ? (
            <>
              Keys approved for the local runner — this server. It connects only to a host that has one here, verifies against
              this record directly, and captures nothing on first connect. Keys the server had already captured before 2.3.0 were
              carried over by the upgrade and are listed as <em>carried</em>.
            </>
          ) : (
            <>
              Keys an operator approved for this runner. This is the record of who trusted what, and it is kept for as long as a key
              is trusted.
            </>
          )}
        </div>
        {inForce.length === 0 ? (
          <div style={{ fontSize: c.fontSm, color: c.textMuted }}>No keys have been approved for this runner.</div>
        ) : (
          <div style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, overflowX: "auto" }}>
            <table style={{ width: "100%", borderCollapse: "collapse" }}>
              <thead>
                <tr>
                  <th style={denseTh()}>Host</th>
                  <th style={denseTh()}>Key</th>
                  <th style={denseTh()}>Approved</th>
                  <th style={denseTh()}>On the runner</th>
                </tr>
              </thead>
              <tbody>
                {inForce.map((k) => (
                  <tr key={k.id}>
                    <td style={dense({ minWidth: 130 })}>
                      <HostCell host={k.host} hostName={k.hostName} note={k.scopeName ? `scope ${k.scopeName}` : undefined} />
                    </td>
                    <td style={dense()}>
                      <KeyCell fingerprint={k.fingerprint} keyType={k.keyType} note={SOURCE_LABEL[k.source] ?? k.source} />
                    </td>
                    <td style={dense()}>
                      {k.actor || "unknown"}
                      <div style={under()}>{when(k.decidedAt)}</div>
                    </td>
                    <td style={dense()}>
                      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                        <FileState k={k} local={runner.local} />
                        <span style={{ flex: 1 }} />
                        {!runner.local && k.deliveredAt && k.presentInFile === false && (
                          <Btn
                            small
                            disabled={busy}
                            onClick={() =>
                              act(
                                () => api.POST("/runners/{runnerId}/host-keys/{ledgerId}/resend", path(k.id)),
                                `Queued again. ${runner.name} receives it on its next poll.`,
                              )
                            }
                            ariaLabel={`Send the key for ${k.hostName ?? k.host} again`}
                          >
                            Send again
                          </Btn>
                        )}
                        <Btn small dangerQuiet disabled={busy} onClick={() => setRemoving(k)} ariaLabel={`Remove the key for ${k.hostName ?? k.host}`}>
                          Remove
                        </Btn>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {history.length > 0 && (
          <div style={{ marginTop: 10 }}>
            <button
              type="button"
              onClick={() => setShowHistory((v) => !v)}
              aria-expanded={showHistory}
              style={{ background: "none", border: "none", padding: 0, cursor: "pointer", color: c.textSec, fontSize: c.fontSm, display: "inline-flex", alignItems: "center", gap: 6 }}
            >
              <ExpandChevron open={showHistory} /> History ({history.length})
            </button>
            {showHistory && (
              <div style={{ marginTop: 8, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, overflowX: "auto" }}>
                <table style={{ width: "100%", borderCollapse: "collapse" }}>
                  <thead>
                    <tr>
                      <th style={denseTh()}>Decision</th>
                      <th style={denseTh()}>Host</th>
                      <th style={denseTh()}>Key</th>
                      <th style={denseTh()}>By</th>
                    </tr>
                  </thead>
                  <tbody>
                    {history.map((k) => (
                      <tr key={k.id}>
                        <td style={dense({ whiteSpace: "nowrap" })}>
                          {k.decision === "approved" ? (
                            <Chip tone="muted" title={`No longer in force since ${when(k.supersededAt)}`}>Replaced</Chip>
                          ) : k.decision === "rejected" ? (
                            <Chip tone="danger">Rejected</Chip>
                          ) : (
                            <Chip tone="warning">Removed</Chip>
                          )}
                        </td>
                        <td style={dense({ minWidth: 130 })}>
                          <HostCell host={k.host} hostName={k.hostName} />
                        </td>
                        <td style={dense()}>
                          <KeyCell fingerprint={k.fingerprint} keyType={k.keyType} note={SOURCE_LABEL[k.source] ?? k.source} />
                        </td>
                        <td style={dense()}>
                          {k.actor || "unknown"}
                          <div style={under()}>{when(k.decidedAt)}</div>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        )}
      </div>

      {/* ── What the runner's own file holds ── (an agent's; the local runner has none) */}
      {!runner.local && (
      <div>
        <SectionLabel
          action={
            <Btn
              small
              disabled={busy || !canScan}
              title={canScan ? "Ask the runner to report its file again" : "The runner must be online to report its file"}
              onClick={() =>
                act(
                  () =>
                    api.POST("/runners/{runnerId}/known-hosts/refresh", { params: { path: { runnerId: runner.id }, header: csrfHeader } }),
                  `Asked ${runner.name} to report its file. Refresh this panel after its next poll.`,
                )
              }
            >
              Ask for a fresh report
            </Btn>
          }
        >
          Present in the runner's known_hosts file ({knownHosts.entries.length})
        </SectionLabel>
        <div style={{ ...subtle(), marginBottom: 8 }}>
          {knownHosts.reportedAt ? (
            <>
              What {runner.name} reported from its own file on {when(knownHosts.reportedAt)}. A line marked <em>not approved here</em> was
              put there some other way, such as by hand on the runner's host. It is trusted by the runner all the same.
            </>
          ) : (
            <>{runner.name} has not reported its file yet. It does so when it starts and after every change made from here.</>
          )}
        </div>
        {knownHosts.truncated && (
          <AlertBanner type="warning">The file is longer than one report carries. Only its first 5,000 entries are shown.</AlertBanner>
        )}
        {knownHosts.entries.length > 0 && (
          <div style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, overflowX: "auto", background: c.panel2 }}>
            <table style={{ width: "100%", borderCollapse: "collapse" }}>
              <thead>
                <tr>
                  <th style={denseTh({ width: 44 })}>Line</th>
                  <th style={denseTh()}>Host</th>
                  <th style={denseTh()}>Key</th>
                  <th style={denseTh()}>Origin</th>
                </tr>
              </thead>
              <tbody>
                {knownHosts.entries.map((e) => (
                  <tr key={e.line}>
                    <td style={dense({ ...mono(), color: c.textSec })}>{e.line}</td>
                    <td style={dense({ wordBreak: "break-all", minWidth: 130 })}>{hostLabel(e.hosts, e.hashed)}</td>
                    <td style={dense()}>
                      <KeyCell fingerprint={e.fingerprint} keyType={e.keyType} />
                    </td>
                    <td style={dense({ whiteSpace: "nowrap" })}>
                      {e.marker === "revoked" ? (
                        <Chip tone="danger" title="A @revoked line: the runner refuses this key">Revoked</Chip>
                      ) : e.marker === "cert-authority" ? (
                        <Chip tone="warning" title="A @cert-authority line: the runner trusts any host certificate this authority signed">
                          Certificate authority
                        </Chip>
                      ) : e.approvedHere ? (
                        <Chip tone="success">Approved here</Chip>
                      ) : (
                        <Chip tone="warning" title="Not approved in Cronomicon: it reached the file some other way">
                          Not approved here
                        </Chip>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
      )}

      {removing && (
        <ConfirmDialog
          title="Remove this host key?"
          confirmLabel="Remove key"
          busy={busy}
          message={
            <>
              {runner.name} stops trusting <strong>{removing.hostName ?? removing.host}</strong> ({removing.keyType}){" "}
              {runner.local ? "at once" : "on its next poll"}, and its runs on that host fail until a key is approved again. The removal
              is recorded.
            </>
          }
          onCancel={() => setRemoving(null)}
          onConfirm={async () => {
            const k = removing;
            await act(
              () => api.POST("/runners/{runnerId}/host-keys/{ledgerId}/remove", path(k.id)),
              runner.local ? `Removed. ${runner.name} no longer trusts the key.` : `Removed. ${runner.name} drops the key on its next poll.`,
            );
            setRemoving(null);
          }}
        />
      )}
      {dialog && (
        <HostKeysDialog
          runner={runner}
          initialSource={dialog.source}
          carryFromPrevious={previous}
          startAt={dialog.review ? "review" : "source"}
          onClose={() => {
            setDialog(null);
            reload();
            if (dirty.current) onChanged?.();
            dirty.current = false;
          }}
          onChanged={() => {
            dirty.current = true;
            reload();
          }}
        />
      )}
    </div>
  );
}

// ── Which of a scope's hosts its bound runners trust ─────────────────────────

/**
 * ScopeKeyCoverage shows, for a scope with bound runners, which of its hosts
 * each runner trusts — and offers the scan that fills the gaps. It renders
 * nothing for a scope with no bound runner: any runner of its agency may take
 * its runs, each with its own trusted keys, and there is no fixed set to show
 * (the inbox reports hosts the local runner has no key for).
 */
export function ScopeKeyCoverage({ scopeId, bound, canConfig }: { scopeId: string; bound: number; canConfig: boolean }) {
  const q = useGet<Coverage>(
    () => api.GET("/scopes/{scopeId}/host-key-coverage", { params: { path: { scopeId } } }),
    [scopeId, bound],
  );
  const [scanFrom, setScanFrom] = useState<KeyRunner | null>(null);
  const [open, setOpen] = useState(false);
  if (bound === 0) return null;
  if (!q.data || !Array.isArray(q.data.hosts) || !Array.isArray(q.data.runners)) {
    if (q.loading) return <InlineLoading what="host-key coverage" />;
    return q.error ? <div style={{ fontSize: c.fontSm, color: c.textSec }}>Host-key coverage could not be loaded: {q.error}</div> : null;
  }
  const { hosts, runners } = q.data;
  if (hosts.length === 0) return <div style={{ fontSize: c.fontSm, color: c.textMuted }}>This scope has no hosts.</div>;
  const th = (extra?: CSSProperties): CSSProperties => ({ ...thStyle(), ...extra });
  const td = (extra?: CSSProperties): CSSProperties => ({ ...tdStyle(), fontSize: c.fontSm, ...extra });
  const gaps = runners.filter((r) => r.missing > 0);
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
      {gaps.length === 0 ? (
        <div style={{ fontSize: c.fontSm, color: c.textSec }}>
          Every bound runner trusts all {hosts.length} {hosts.length === 1 ? "host" : "hosts"} of this scope.
        </div>
      ) : (
        gaps.map((r) => (
          <AlertBanner key={r.runnerId} type="warning">
            <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
              <span>
                <strong>
                  {r.missing} of {hosts.length} {hosts.length === 1 ? "host" : "hosts"} not yet trusted by {r.name}.
                </strong>{" "}
                Its runs on {r.missing === 1 ? "that host" : "those hosts"} fail at the first connection.
              </span>
              {canConfig && r.registered && (
                <Btn small onClick={() => setScanFrom({ id: r.runnerId, name: r.name })} ariaLabel={`Scan this scope from ${r.name}`}>
                  Scan this scope…
                </Btn>
              )}
            </div>
          </AlertBanner>
        ))
      )}
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        style={{ alignSelf: "flex-start", background: "none", border: "none", padding: 0, cursor: "pointer", color: c.textSec, fontSize: c.fontSm, display: "inline-flex", alignItems: "center", gap: 6 }}
      >
        <ExpandChevron open={open} /> Host by host
      </button>
      {open && (
        <div style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, overflowX: "auto" }}>
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr>
                <th style={th()}>Host</th>
                <th style={th()}>Address</th>
                {runners.map((r) => (
                  <th key={r.runnerId} style={th()}>
                    {r.name}
                    {!r.registered && <span style={{ textTransform: "none", color: c.textMuted }}> (deregistered)</span>}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {hosts.map((h, i) => (
                <tr key={h.host}>
                  <td style={td()}>
                    <strong>{h.host}</strong>
                    {h.notScannable && <div style={{ fontSize: c.fontXs, color: c.textSec }}>{h.notScannable}</div>}
                  </td>
                  <td style={td({ ...mono() })}>
                    {h.pattern}
                    {/* Two hops, two keys: say so where the address is read. */}
                    {h.viaPattern && (
                      <div style={{ fontFamily: c.sans, fontSize: c.fontXs, color: c.textSec }}>
                        and the bastion <span style={mono()}>{h.viaPattern}</span>
                      </div>
                    )}
                  </td>
                  {runners.map((r) => (
                    <td key={r.runnerId} style={td({ whiteSpace: "nowrap" })}>
                      {r.states[i] === "approved" ? (
                        <Chip tone="success">Trusted</Chip>
                      ) : r.states[i] === "queued" ? (
                        <Chip tone="info" title="Approved here; the runner receives the key on its next poll">
                          Approved, not yet sent
                        </Chip>
                      ) : r.states[i] === "in-file" ? (
                        <Chip tone="info" title="In the runner's own known_hosts file; not approved in Cronomicon">
                          In its file
                        </Chip>
                      ) : (
                        <Chip tone="warning">Not trusted</Chip>
                      )}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {scanFrom && (
        <HostKeysDialog
          runner={scanFrom}
          initialSource="scope"
          initialScopeId={scopeId}
          onClose={() => {
            setScanFrom(null);
            q.refetch();
          }}
          onChanged={() => q.refetch()}
        />
      )}
    </div>
  );
}

// ── A change-log batch, expanded ─────────────────────────────────────────────

const BATCH_RE = /\bbatch ([0-9A-Za-z-]+)\s*$/;

/** The batch id a "Host Keys" change-log row names, or null. */
export const hostKeyBatchId = (details?: string | null): string | null => details?.match(BATCH_RE)?.[1] ?? null;

/**
 * HostKeyBatchLink turns a change-log row's "batch <id>" into the keys that
 * batch decided. The change log carries one row per operator action; this is
 * where a 47-key approval is read key by key.
 */
export function HostKeyBatchLink({ details }: { details: string }) {
  const id = hostKeyBatchId(details);
  const [open, setOpen] = useState(false);
  if (!id) return <>{details}</>;
  return (
    <>
      {details.replace(BATCH_RE, "").replace(/[;\s]+$/, "")}{" "}
      <button
        type="button"
        onClick={(e) => {
          e.stopPropagation();
          setOpen(true);
        }}
        style={{ background: "none", border: "none", padding: 0, cursor: "pointer", color: c.primary, fontSize: "inherit", textDecoration: "underline" }}
      >
        Show keys
      </button>
      {open && <HostKeyBatchModal batchId={id} onClose={() => setOpen(false)} />}
    </>
  );
}

function HostKeyBatchModal({ batchId, onClose }: { batchId: string; onClose: () => void }) {
  const q = useGet<LedgerRow[]>(() => api.GET("/host-key-batches/{batchId}", { params: { path: { batchId } } }), [batchId]);
  const list = Array.isArray(q.data) ? q.data : [];
  const first = list[0];
  return (
    <Modal
      title="Host keys in this change"
      onClose={onClose}
      table
      footer={
        <div style={{ display: "flex", justifyContent: "flex-end" }}>
          <Btn primary onClick={onClose}>Close</Btn>
        </div>
      }
    >
      {q.loading ? (
        <InlineLoading what="keys" />
      ) : list.length === 0 ? (
        <div style={prose()}>
          {q.error ? `These keys could not be loaded: ${q.error}` : "The keys of this change are past the host-key history window and are no longer on record."}
        </div>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
          <div style={prose()}>
            {first?.actor || "unknown"} on {when(first?.decidedAt)}, runner <strong style={{ color: c.text }}>{first?.runnerName}</strong>.
          </div>
          <div style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, overflowX: "auto" }}>
            <table style={{ width: "100%", borderCollapse: "collapse" }}>
              <thead>
                <tr>
                  <th style={denseTh()}>Decision</th>
                  <th style={denseTh()}>Host</th>
                  <th style={denseTh()}>Key</th>
                </tr>
              </thead>
              <tbody>
                {list.map((k) => (
                  <tr key={k.id}>
                    <td style={dense({ whiteSpace: "nowrap" })}>
                      <Chip tone={k.decision === "approved" ? "success" : k.decision === "rejected" ? "danger" : "warning"}>{k.decision}</Chip>
                      {k.decision === "approved" && k.supersededAt && <div style={under()}>since replaced</div>}
                    </td>
                    <td style={dense({ minWidth: 130 })}>
                      <HostCell host={k.host} hostName={k.hostName} note={k.scopeName ? `scope ${k.scopeName}` : undefined} />
                    </td>
                    <td style={dense()}>
                      <KeyCell fingerprint={k.fingerprint} keyType={k.keyType} note={SOURCE_LABEL[k.source] ?? k.source} />
                      {k.previousFingerprint && (
                        <div style={under()}>
                          replaced <span style={mono()}>{k.previousFingerprint}</span>
                        </div>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </Modal>
  );
}

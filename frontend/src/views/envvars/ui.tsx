import { useMemo, type ReactNode } from "react";
import { c } from "../../theme";
import { CopyButton } from "../../components/ui";
import { fmtInAppZone } from "../../utils/datetime";
import { api, csrfHeader, errMsg } from "../../api/client";
import { useGet, rows } from "../../hooks";

// csrfHeader/errMsg live in api/client.ts (CC.21); re-exported for the Env Vars
// tab views that import them from this kit.
export { csrfHeader, errMsg };

// useInlineTags moved to the shared hooks module (CC.18) — the Env Vars tabs now
// import it from ../../hooks alongside the catalog views.

// Routes through the app-zone formatter (timezone-update §5.2) so envvar
// timestamps render in the application timezone, not the browser's.
export function fmtDate(s?: string | null): string {
  return fmtInAppZone(s);
}

// inputStyle/thStyle/tdStyle are the canonical shared field + table-cell styles
// (CC.16), re-exported here (they were exact duplicates). labelStyle stays local
// (no canonical equivalent yet).
export { inputStyle, thStyle, tdStyle } from "../../components/ui";

export const labelStyle = (): React.CSSProperties => ({
  fontSize: c.fontXs,
  fontFamily: c.sansCond,
  fontWeight: 600,
  color: c.textMuted,
  textTransform: "uppercase",
  letterSpacing: 0.7,
  display: "block",
  marginBottom: 4,
});

// ── Expand affordance + detail panel (ported from the prototype's row expander) ──
// Collapsed shows a down chevron; expanded rotates it to point up.
export function Chevron({ open, size = 14 }: { open: boolean; size?: number }) {
  return (
    <span
      style={{
        display: "inline-flex",
        color: c.textMuted,
        transform: open ? "rotate(180deg)" : "none",
        transition: "transform 0.15s",
      }}
    >
      <svg width={size} height={size} viewBox="0 0 18 18" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
        <path d="M4.5 6.5l4.5 4.5 4.5-4.5" />
      </svg>
    </span>
  );
}

export const detailBoxStyle = (): React.CSSProperties => ({
  padding: "10px 14px",
  background: c.bg,
  borderRadius: c.radiusSurface,
  border: `1px solid ${c.border}`,
  fontSize: c.fontSm,
});

// valueBoxStyle is the left-column value/secret/key display in an expanded row.
// It gives the box a comfortable height (roughly the size of the Details box on
// the right) instead of a single cramped line, and scrolls when the content
// runs past the frame — long/multi-line values (PEM keys, big blobs) stay inside
// a fixed box instead of ballooning the row. overflowWrap breaks long unbroken
// mono tokens; pre-wrap preserves newlines in multi-line values.
export const valueBoxStyle = (): React.CSSProperties => ({
  ...detailBoxStyle(),
  minHeight: 140,
  maxHeight: 220,
  overflow: "auto",
  whiteSpace: "pre-wrap",
  overflowWrap: "anywhere",
});

// Two-column layout used inside an expanded row: value/secret on the left,
// metadata on the right. Collapses to one column on narrow viewports.
export function DetailGrid({ children }: { children: ReactNode }) {
  return <div style={{ display: "grid", gridTemplateColumns: "minmax(0, 1fr) minmax(0, 1fr)", gap: 16, padding: "4px 0 6px" }}>{children}</div>;
}

// DetailLabel is the env-var tabs' name for the shared SectionLabel (EV-1). It
// was the closest existing relative of the shared component — same uppercase
// caption, same right-aligned `action` slot for the Reveal/Hide and Copy
// controls that must stay put while the value box below scrolls — so folding it
// in was a rename, not a rewrite. Re-exported under the old name so the three
// tabs' imports keep working.
import { SectionLabel } from "../../components/ui";
export { SectionLabel as DetailLabel };

export function DetailRow({ label, value, last }: { label: string; value: ReactNode; last?: boolean }) {
  return (
    <div
      style={{
        display: "flex",
        justifyContent: "space-between",
        alignItems: "center",
        gap: 12,
        padding: "5px 0",
        borderBottom: last ? "none" : `1px solid ${c.borderLight}`,
      }}
    >
      <span style={{ color: c.textMuted, whiteSpace: "nowrap" }}>{label}</span>
      <span style={{ color: c.textSec, textAlign: "right", minWidth: 0, overflow: "hidden", textOverflow: "ellipsis" }}>{value}</span>
    </div>
  );
}

// NamespaceHelp is the per-tab explainer (namespace contract, W5): how to
// reference a row's value in a run — the reserved CRONOMICON_ prefix plus the
// value-vs-path semantics of the section. Rendered as a subtle info banner above
// the table; nothing here renames a row.
export function NamespaceHelp({ children }: { children: ReactNode }) {
  return (
    <div
      style={{
        fontSize: c.fontSm,
        color: c.textSec,
        background: c.bg,
        border: `1px solid ${c.borderLight}`,
        borderRadius: c.radiusSurface,
        padding: "8px 12px",
        marginBottom: 12,
        lineHeight: 1.5,
      }}
    >
      {children}
    </div>
  );
}

// ReferenceField shows a row's DERIVED reference (namespace contract, W5) with a
// copy-to-clipboard control and a one-line value-vs-path hint. The reference is
// read-only — the row keeps its bare name; this is only the injectable form an
// operator copies into a script or inventory. Renders nothing when the API did
// not supply a reference (older server).
export function ReferenceField({ reference, semantics }: { reference?: string | null; semantics: string }) {
  if (!reference) return null;
  return (
    <div style={{ marginTop: 12 }}>
      <SectionLabel>Reference</SectionLabel>
      <div style={{ ...detailBoxStyle(), display: "flex", alignItems: "center", gap: 10 }}>
        <code style={{ flex: 1, fontFamily: c.mono, fontSize: c.fontSm, color: c.text, wordBreak: "break-all" }}>{reference}</code>
        <CopyButton
          text={reference}
          variant="text"
          ariaLabel="Copy reference to clipboard"
          style={{ fontSize: c.fontSm, flexShrink: 0 }}
        />
      </div>
      <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 4 }}>{semantics}</div>
    </div>
  );
}

// Btn, TabBar, and SearchBar now come from the canonical kit (CC.16); re-exported
// so the Env Vars / Scopes / Agencies views importing them from this kit converge
// onto the canonical styling.
export { Btn, TabBar, SearchBar } from "../../components/ui";

// Notice, Modal, and ConfirmDialog now live in the canonical kit (CC.16/CC.17);
// re-exported here for the Env Vars / Scopes / Agencies views that import them
// from this kit.
export { Notice, Modal, ConfirmDialog } from "../../components/ui";

// ── Scope legibility + binding usage (agencies plan T1.8/T1.9) ──────────────

// GLOBAL_SCOPE_LABEL: the "" / NULL scope convention rendered as a word. A blank
// Scope cell was indistinguishable from "not loaded yet", and the distinction is
// load-bearing — a global row injects into EVERY scope, which is exactly the fact
// an operator needs before editing one. Shared with the reference chips so both
// surfaces say the same word.
export { GLOBAL_SCOPE_LABEL } from "../../components/ReferenceBindings";

// scopeCell renders a row's scope for a table cell: the global word, or the name.
export function scopeCell(scope?: string | null): string {
  return scope ? scope : "global";
}

// ALL_SCOPE_OPTION is the Variables/Secrets form's first scope choice, meaning
// "every scope". It is a UI SENTINEL, not a scope name — the backend has no
// notion of a scope called "All"; global is the empty/NULL scope.
export const ALL_SCOPE_OPTION = "All";

// scopeForWrite maps the form's scope selection onto the wire value.
//
// This exists because the sentinel used to be submitted VERBATIM: picking "All"
// — the form's DEFAULT — wrote a row whose scope was the literal string "All",
// which the resolver treats as a named scope like any other. Such a row resolves
// for NO run (no scope is actually named "All"), so the reference silently failed
// to inject while the Env Vars table cheerfully displayed "All". Nothing in the
// backend has ever recognized the sentinel; the reference validator (T1.1) is
// what finally made the mismatch visible, reporting `only "All"`.
export function scopeForWrite(scope: string): string {
  return scope === ALL_SCOPE_OPTION ? "" : scope;
}

// ScopeFilterSelect narrows a list to one scope, with an explicit "Global only"
// option — the pair of controls an operator needs when a key exists in several
// scopes and only one of them is the one they meant to edit. `value` is the empty
// string for "all scopes"; GLOBAL_ONLY selects the unscoped rows.
export const GLOBAL_ONLY = " global";

export function ScopeFilterSelect({
  scopes,
  value,
  onChange,
}: {
  scopes: string[];
  value: string;
  onChange: (v: string) => void;
}) {
  return (
    <select
      value={value}
      onChange={(e) => onChange(e.target.value)}
      title="Filter by scope"
      aria-label="Filter by scope"
      style={{
        padding: "6px 10px",
        borderRadius: c.radiusChip,
        border: `1px solid ${c.borderStrong}`,
        background: c.panel2,
        color: value ? c.text : c.textSec,
        fontSize: c.fontSm,
        cursor: "pointer",
      }}
    >
      <option value="">All scopes</option>
      <option value={GLOBAL_ONLY}>Global only</option>
      {scopes.map((s) => (
        <option key={s} value={s}>
          {s}
        </option>
      ))}
    </select>
  );
}

// matchesScope is ScopeFilterSelect's predicate.
export function matchesScope(rowScope: string | null | undefined, filter: string): boolean {
  if (!filter) return true;
  if (filter === GLOBAL_ONLY) return !rowScope;
  return rowScope === filter;
}

// useReferenceUsage loads the "used by N jobs" counts (T1.9) keyed by
// `<kind> <name>`, so an admin can see what a row is actually wired into BEFORE
// editing its scope — the edit that silently strands every binding. Counts only;
// job counts are scope-filtered server-side to the caller's grants.
export function useReferenceUsage(dep: unknown) {
  const q = useGet<{ usage?: { kind: string; name: string; jobs: number; scripts: number }[] }>(
    () => api.GET("/reference-usage"),
    [dep],
  );
  const byKey = useMemo(() => {
    const m: Record<string, { jobs: number; scripts: number }> = {};
    for (const u of q.data?.usage ?? []) m[`${u.kind} ${u.name}`] = { jobs: u.jobs, scripts: u.scripts };
    return m;
  }, [q.data]);
  return {
    // usageFor returns the counts for one row, defaulting to zero — a reference
    // nothing binds is absent from the response rather than reported as zero.
    usageFor: (kind: string, name: string) => byKey[`${kind} ${name}`] ?? { jobs: 0, scripts: 0 },
    loading: q.loading,
  };
}

// UsageCell renders one row's binding count. Zero is stated rather than blanked:
// "nothing binds this" is a real, actionable answer (it means editing the scope is
// free), and a blank cell reads as missing data.
export function UsageCell({ jobs, scripts }: { jobs: number; scripts: number }) {
  const total = jobs + scripts;
  const parts = [
    jobs > 0 ? `${jobs} job${jobs === 1 ? "" : "s"}` : "",
    scripts > 0 ? `${scripts} script${scripts === 1 ? "" : "s"}` : "",
  ].filter(Boolean);
  return (
    <span
      title={total === 0 ? "No job or script declares a binding to this reference." : `Bound by ${parts.join(" and ")}.`}
      style={{ color: total === 0 ? c.textMuted : c.textSec, fontSize: c.fontSm }}
    >
      {total === 0 ? "—" : parts.join(" · ")}
    </span>
  );
}

// useShadowWarnings loads the RA-10 shadow findings keyed by `<kind> <name> <scope>`
// so a row can be decorated in place.
//
// THE THING IT MAKES VISIBLE. Reference resolution prefers a scope-exact row over
// a global one of the same key, and an EMPTY agency membership means "no
// restriction". So a scoped row created without a department wins for every
// department's runs in that scope — silently, with nothing in this list or in the
// run log saying which of the two rows was injected. Unmembered is the state
// migration 670 leaves every row in, so this is the default outcome of adding a
// scoped row and not thinking about departments.
//
// Findings are scope-filtered server-side to the caller's grants, so this shows a
// restricted operator only the shadows they can act on.
export function useShadowWarnings(dep: unknown) {
  const q = useGet<{ shadows?: { kind: string; name: string; scope: string; unrestricted: boolean; reason: string }[] }>(
    () => api.GET("/env-var-shadows"),
    [dep],
  );
  const byKey = useMemo(() => {
    const m: Record<string, { unrestricted: boolean; reason: string }> = {};
    for (const s of q.data?.shadows ?? []) {
      m[`${s.kind} ${s.name} ${s.scope}`] = { unrestricted: s.unrestricted, reason: s.reason };
    }
    return m;
  }, [q.data]);
  return {
    // shadowFor returns the finding for one row, or null. A row that shadows
    // nothing is absent from the response rather than reported as clean.
    shadowFor: (kind: string, name: string, scope?: string | null) =>
      scope ? (byKey[`${kind} ${name} ${scope}`] ?? null) : null,
  };
}

// ShadowBadge marks a row that shadows a global row of the same key.
//
// Two weights on purpose. An UNRESTRICTED shadow is the silent hole — it belongs to
// no department, so it overrides the global row for everyone in that scope — and is
// drawn as a warning. A membered shadow is usually the override feature working as
// intended (it wins only for its own department's runs), so it is drawn quietly:
// worth seeing, not worth alarming about. Flattening the two would make the badge
// noise in exactly the catalogue where it matters most.
export function ShadowBadge({ shadow }: { shadow: { unrestricted: boolean; reason: string } | null }) {
  if (!shadow) return null;
  const warn = shadow.unrestricted;
  return (
    <span
      title={shadow.reason}
      aria-label={warn ? "Shadows the global row for every department" : "Shadows the global row"}
      style={{
        padding: "1px 6px",
        borderRadius: c.radiusChip,
        border: `1px solid ${warn ? c.warning : c.borderStrong}`,
        color: warn ? c.warning : c.textSec,
        background: "transparent",
        fontFamily: c.sans,
        fontWeight: 500,
        fontSize: c.fontXs,
        whiteSpace: "nowrap",
        cursor: "help",
      }}
    >
      {warn ? "⚠ shadows global" : "shadows global"}
    </span>
  );
}

// OwnerChip names the department that OWNS a row (RA-15/RA-19, Phase E).
//
// It exists because ownership makes the list AMBIGUOUS ON PURPOSE: two departments
// may now hold the same key in the same scope, so `BECOME_PASSWORD · prod` can
// appear twice and the rows are otherwise identical on screen. Without the chip an
// operator cannot tell which one they are about to edit — and for the by-ID pickers
// (SSH Targets, Connect-as) cannot tell which key they are about to select.
//
// A shared row renders NOTHING rather than a "shared" chip: unowned is the default
// and the overwhelmingly common state, and labelling every row in an untouched
// catalogue would be noise that makes the real signal harder to see.
export function OwnerChip({ owner }: { owner?: string | null }) {
  if (!owner) return null;
  return (
    <span
      title={`Owned by ${owner}. This department's runs resolve this row before any shared row of the same name.`}
      style={{
        padding: "1px 6px",
        borderRadius: c.radiusChip,
        border: `1px solid ${c.borderStrong}`,
        color: c.textSec,
        background: c.panel2,
        fontFamily: c.sans,
        fontWeight: 500,
        fontSize: c.fontXs,
        whiteSpace: "nowrap",
        cursor: "help",
      }}
    >
      {owner}
    </span>
  );
}

// useAmbiguityWarnings loads the RA-18 findings keyed by `<kind> <name> <scope>`:
// names held by MORE THAN ONE department, where a run spanning several owners
// resolves to nothing at all.
//
// Rides the same response as the shadow warnings, so the Env Vars view pays for one
// request rather than two.
export function useAmbiguityWarnings(dep: unknown) {
  const q = useGet<{ ambiguities?: { kind: string; name: string; scope: string; owners: string[]; reason: string }[] }>(
    () => api.GET("/env-var-shadows"),
    [dep],
  );
  const byKey = useMemo(() => {
    const m: Record<string, { owners: string[]; reason: string }> = {};
    for (const a of q.data?.ambiguities ?? []) {
      m[`${a.kind} ${a.name} ${a.scope}`] = { owners: a.owners, reason: a.reason };
    }
    return m;
  }, [q.data]);
  return {
    ambiguityFor: (kind: string, name: string, scope?: string | null) =>
      byKey[`${kind} ${name} ${scope ?? ""}`] ?? null,
  };
}

// AmbiguityBadge marks a name several departments own.
//
// Deliberately NOT styled as an error: nothing is misconfigured, and each
// department's own runs work. It warns because a run whose departments span more
// than one owner fails closed — a refusal that reads as breakage unless the
// operator has seen this first.
export function AmbiguityBadge({ ambiguity }: { ambiguity: { owners: string[]; reason: string } | null }) {
  if (!ambiguity) return null;
  return (
    <span
      title={ambiguity.reason}
      aria-label={`Owned by ${ambiguity.owners.length} departments; a run spanning them resolves to nothing`}
      style={{
        padding: "1px 6px",
        borderRadius: c.radiusChip,
        border: `1px solid ${c.warning}`,
        color: c.warning,
        background: "transparent",
        fontFamily: c.sans,
        fontWeight: 500,
        fontSize: c.fontXs,
        whiteSpace: "nowrap",
        cursor: "help",
      }}
    >
      ⚠ {ambiguity.owners.length} owners
    </span>
  );
}

// ── Per-entity agency membership (RB-22) ─────────────────────────────────────
//
// The Membership matrix answered "which agencies hold this secret?" as one cell
// in an everything-times-everything grid; with the matrix gone (RB-22) that
// question moves to the entity's own catalog row, which is where an operator
// actually is when they ask it. The reads used here are the session-gated
// membership matrices that exist precisely so views can label rows.
//
// EMPTY MEMBERSHIP IS NOT BLANK. Under AG-Q1(b) an entity in no agency is
// unrestricted — reachable from everywhere — which reads as MORE access, not
// less. AgencyCell says "unrestricted" in words for exactly the reason the
// Honest View says "No access": the dangerous state must never look like
// missing data.

/** Map of entity id → agency display names, from the kind's membership matrix. */
export function useEntityAgencies(kind: "secret" | "env-var" | "ssh-credential", bump?: number) {
  const { data: agencyData } = useGet<unknown>(() => api.GET("/agencies"), []);
  const path = `/${kind}-agencies` as "/secret-agencies";
  const { data: memberData } = useGet<unknown>(() => api.GET(path), [bump]);
  return useMemo(() => {
    const nameOf: Record<string, string> = {};
    for (const a of rows<{ id?: string; name?: string }>(agencyData)) {
      if (a.id) nameOf[a.id] = a.name ?? a.id;
    }
    const out: Record<string, string[]> = {};
    for (const m of rows<{ id?: string; agencyIds?: string[] }>(memberData)) {
      if (!m.id) continue;
      // A deleted agency cannot leave a dangling row (ON DELETE CASCADE), so the
      // id fallback here is defence, not behavior.
      out[m.id] = (m.agencyIds ?? []).map((aid) => nameOf[aid] ?? aid);
    }
    return out;
  }, [agencyData, memberData]);
}

/** The Agencies cell: membership chips, or the word the empty set actually means. */
export function AgencyCell({ names }: { names: string[] | undefined }) {
  if (!names || names.length === 0) {
    return (
      <span
        title="No agency restriction — reachable from every department."
        style={{ color: c.textSec, fontSize: c.fontXs, fontStyle: "italic", cursor: "help" }}
      >
        unrestricted
      </span>
    );
  }
  return (
    <div style={{ display: "flex", flexWrap: "wrap", gap: 4 }}>
      {names.map((n) => (
        <span
          key={n}
          style={{
            padding: "1px 7px",
            borderRadius: c.radiusChip,
            fontSize: c.fontXs,
            fontFamily: c.mono,
            background: c.panel2,
            border: `1px solid ${c.border}`,
            color: c.text,
            whiteSpace: "nowrap",
          }}
        >
          {n}
        </span>
      ))}
    </div>
  );
}

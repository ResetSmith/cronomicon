// Canonical UI kit — ported from the prototype's shared components
// (amadeus-core.jsx). Views should prefer these over rolling their own so the
// app keeps one visual language. Everything reads the mutable `c` tokens during
// render (theme.ts), so components repaint on theme toggle via the Outlet
// remount in Shell.
import { useEffect, useRef, useState, type CSSProperties, type InputHTMLAttributes, type KeyboardEvent, type ReactNode, type SelectHTMLAttributes } from "react";
import { c } from "../theme";
import { useTheme } from "../theme-context";
import { useRefreshAction } from "./RefreshScope";
import { pagerLabel } from "../utils/pager";
import ansibleIcon from "../assets/ansible.webp";
import powershellIcon from "../assets/powershell.webp";
import terraformIcon from "../assets/hashicorp-terraform.webp";
import pythonIcon from "../assets/python.webp";
import shellIconLight from "../assets/shell-tips-light.webp";
import shellIconDark from "../assets/shell-tips-dark.webp";

// ── Status → color/tint ──────────────────────────────────────────────────────
// Maps semantic run/job statuses onto the palette. `bg` uses the prototype's
// tinted background tokens so badges/banners match exactly.
export type Status = "success" | "warning" | "danger" | "running" | "info" | "paused" | "idle";

export function statusTone(status?: string | null): { color: string; bg: string } {
  switch (status) {
    case "success":
    case "ok":
      return { color: c.success, bg: c.successBg };
    case "warning":
    case "warn":
      return { color: c.warning, bg: c.warningBg };
    case "danger":
    case "failed":
    case "failure":
    case "killed":
      return { color: c.danger, bg: c.dangerBg };
    case "running":
    case "queued":
    case "info":
      return { color: c.info, bg: c.infoBg };
    case "paused":
    case "skipped":
    case "cancelled":
    case "idle":
      return { color: c.textMuted, bg: "transparent" };
    default:
      return { color: c.textMuted, bg: "transparent" };
  }
}

// statusLabel is the canonical display vocabulary (V1.1-4). One labelled set
// across the app — Success · Failed · Warn · Skipped · Running · Paused — with
// the raw wire/DB aliases folded in (killed→Failed, queued→Running, ok→Success).
// `idle` is a job state (never a run result). Unknown values are title-cased.
export function statusLabel(status?: string | null): string {
  switch (status) {
    case "success":
    case "ok":
      return "Success";
    case "danger":
    case "fail":
    case "failed":
    case "failure":
    case "killed":
      return "Failed";
    case "warning":
    case "warn":
      return "Warn";
    case "skipped":
      return "Skipped";
    case "cancelled":
      return "Cancelled";
    case "running":
    case "queued":
      return "Running";
    case "paused":
      return "Paused";
    case "idle":
      return "Idle";
    default:
      return status ? status.charAt(0).toUpperCase() + status.slice(1) : "—";
  }
}

// RX-4 — the kill-disposition vocabulary. Stopping a run and saying what the
// stop MEANT are two different acts: the chosen value becomes the run's status,
// while killedBy independently records that a human ended it. `killed` is the
// default and the unclassified case — it means exactly what a kill has always
// meant, which is why it stays folded into Failed by statusLabel above.
//
// Deliberately a single exported table rather than a bare union: the dialog, the
// toast wording and any future surface all read their labels from here, so the
// operator-facing words for a disposition cannot drift between them.
export type KillOutcome = "killed" | "failure" | "success" | "warning";

export const KILL_OUTCOMES: { value: KillOutcome; label: string; hint: string }[] = [
  { value: "killed", label: "Stopped", hint: "Unclassified — a human ended it, nothing asserted about the work. Counts as a failure in health and History filters, exactly as today." },
  { value: "failure", label: "Failed", hint: "The work was going wrong. Same status a genuine failure would record." },
  { value: "success", label: "Success", hint: "The work was effectively done. In a workflow this lets the remaining steps proceed." },
  { value: "warning", label: "Warning", hint: "Completed, but not cleanly." },
];

// jobStatusLabel is statusLabel's sibling for *job* states specifically. It
// differs in one place: it does NOT fold queued→Running. A job whose next run
// is merely queued should read "Queued" — the Dashboard can simultaneously show
// "Running Now: 0", so collapsing the two would be wrong. Idle · Queued ·
// Paused · Running for job states; anything else (a job surfacing a terminal
// last-result) delegates to statusLabel.
export function jobStatusLabel(status?: string | null): string {
  switch (status) {
    case "idle":
      return "Idle";
    case "queued":
      return "Queued";
    case "paused":
      return "Paused";
    case "running":
      return "Running";
    default:
      return statusLabel(status);
  }
}

// matchesStatus is the canonical *filter* predicate, and the reason there is no
// second alias table anywhere (VF-9/F-3). A status filter shows the operator a
// canonical label; a row shows the same label via statusLabel. Deriving the
// predicate from statusLabel is what guarantees the two can never disagree —
// pick "Failed" and every row you get back says Failed, killed and failure
// folded in. "All" (the every-filter's first option) matches everything.
//
// Before this, three hand-rolled copies of the fold existed — ExecutionsTab's
// matchesResult, its RESULT_PARAM deep-link map, and Workflows' tab predicates —
// and they had already drifted (the tabs matched bare `danger`, so a killed
// workflow run was labelled Failed but excluded from the Failed tab).
export function matchesStatus(label: string, status?: string | null): boolean {
  return label === "All" || statusLabel(status) === label;
}

// ── Card ─────────────────────────────────────────────────────────────────────
export function Card({
  title,
  action,
  noPad,
  style,
  children,
}: {
  title?: ReactNode;
  action?: ReactNode;
  noPad?: boolean;
  style?: CSSProperties;
  children: ReactNode;
}) {
  return (
    <div
      style={{
        background: c.panel,
        // A surface gets a border OR a shadow, never both (VU-5). The console
        // was framing everything three ways at once — border AND shadow AND a
        // fill that differs from the page — which is what made it read as a
        // stack of boxes rather than a document. The border stays because it is
        // the one that survives both themes; the shadow is gone.
        //
        // The exception is OVERLAYS (Modal, the TagFilterSelect popover): their
        // shadow is doing separation-from-arbitrary-content work, not
        // decoration, so they keep both. Nothing in normal page flow may.
        border: `1px solid ${c.border}`,
        borderRadius: c.radiusSurface,
        padding: noPad ? 0 : 16,
        // Explicit properties, never `all`: `all` animates layout-affecting
        // properties too, so a re-render that changes padding/width pays for a
        // paint it never asked for (VU-15).
        transition: "background 0.25s, border-color 0.25s",
        ...style,
      }}
    >
      {(title || action) && (
        <div
          style={{
            display: "flex",
            justifyContent: "space-between",
            alignItems: "center",
            marginBottom: noPad ? 0 : 12,
            padding: noPad ? "14px 16px" : 0,
            borderBottom: noPad ? `1px solid ${c.border}` : undefined,
          }}
        >
          {typeof title === "string" ? (
            <div style={{ fontSize: c.fontHead, fontWeight: 600, color: c.text }}>{title}</div>
          ) : (
            title
          )}
          {action}
        </div>
      )}
      {children}
    </div>
  );
}

// ── Rule (the hairline) ──────────────────────────────────────────────────────
// The single grouping primitive (B-4). Where the app previously reached for a
// nested Card to say "these things belong together", it now reaches for a rule
// plus leading — one line and some space group content just as well as a box
// does, without adding a third frame inside two existing ones (VU-5).
//
// `strong` uses the full border tone for a section division; the default
// hairline matches the table-row separator, for grouping *within* a section.
// `inset` pulls the rule out to a padded surface's edges, so it spans the full
// width of the card rather than floating inside its padding.
export function Rule({ strong, inset, style }: { strong?: boolean; inset?: number; style?: CSSProperties }) {
  return (
    <div
      role="separator"
      style={{
        height: 1,
        background: strong ? c.border : c.borderLight,
        margin: inset ? `0 -${inset}px` : undefined,
        ...style,
      }}
    />
  );
}

// ── TableSurface ─────────────────────────────────────────────────────────────
// A table's frame, for tables that were previously wrapped in a Card (B-1). The
// enclosing box is gone: the table sits on the page between a top and a bottom
// rule, which is enough to bound it and removes one whole level of nesting from
// every catalog view. Keeps the panel fill so rows still read as a surface, and
// keeps horizontal overflow scrolling, which is what the Card was doing for the
// wide catalogs.
export function TableSurface({ children, style }: { children: ReactNode; style?: CSSProperties }) {
  return (
    <div
      style={{
        background: c.panel,
        borderTop: `1px solid ${c.border}`,
        borderBottom: `1px solid ${c.border}`,
        overflowX: "auto",
        ...style,
      }}
    >
      {children}
    </div>
  );
}

// ── StatTile ─────────────────────────────────────────────────────────────────
// `tint` optionally paints a status-tinted background (Runners uses it so the
// online/degraded/offline counts scan at a glance); Dashboard omits it and gets
// the plain panel. One implementation, one geometry — no per-view forks (VC.5).
export function StatTile({ label, value, color, tint }: { label: string; value: ReactNode; color?: string; tint?: string }) {
  return (
    <div
      style={{
        flex: 1,
        background: tint ?? c.panel,
        border: `1px solid ${c.border}`,
        borderRadius: c.radiusSurface,
        padding: "14px 18px",
        textAlign: "center",
      }}
    >
      {/* The tile's number sits on the title step rather than its own 28px: it is
          the same tier of hierarchy as a page title, and weight (700) is what
          separates them. D-7 rebuilds this row (VU-12) — until then it is at
          least on the scale. */}
      <div style={{ fontSize: c.fontTitle, fontWeight: 700, color: color || c.primary, lineHeight: 1.1 }}>{value}</div>
      <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, textTransform: "uppercase", letterSpacing: 0.6, color: c.textMuted, marginTop: 5 }}>{label}</div>
    </div>
  );
}

// ── Badge (pill + status dot; pulses while running) ─────────────────────────
export function Badge({ status, label, children }: { status?: string | null; label?: ReactNode; children?: ReactNode }) {
  const { color, bg } = statusTone(status);
  const isIdle = bg === "transparent";
  const running = status === "running";
  return (
    <span
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 5,
        padding: "3px 10px",
        borderRadius: c.radiusPill,
        fontSize: c.fontXs,
        fontWeight: 600,
        color,
        background: bg,
        border: isIdle ? `1px solid ${c.border}` : "transparent",
        whiteSpace: "nowrap",
      }}
    >
      <span
        style={{
          width: 7,
          height: 7,
          borderRadius: "50%",
          background: color,
          animation: running ? "pulse 2s infinite" : undefined,
          boxShadow: running ? `0 0 6px ${color}` : undefined,
        }}
      />
      {label ?? children}
    </span>
  );
}

// ── TagChip ──────────────────────────────────────────────────────────────────
export function TagChip({ label }: { label: string }) {
  return (
    <span
      style={{
        display: "inline-flex",
        padding: "2px 8px",
        borderRadius: c.radiusChip,
        fontSize: c.fontXs,
        fontWeight: 600,
        background: c.panelHover,
        color: c.textMuted,
        border: `1px solid ${c.borderLight}`,
        letterSpacing: 0.3,
        whiteSpace: "nowrap",
      }}
    >
      {label}
    </span>
  );
}

// A "+N" companion to TagChip for tables that cap how many tags show inline so
// every row stays a uniform height (TagChip wrapping is what makes rows grow).
// The hidden tags live in the row's expanded view; `title` lists them so they
// stay discoverable on hover without expanding.
function TagOverflowChip({ count, title }: { count: number; title?: string }) {
  return (
    <span
      title={title}
      style={{
        display: "inline-flex",
        padding: "2px 8px",
        borderRadius: c.radiusChip,
        fontSize: c.fontXs,
        fontWeight: 600,
        background: c.panelHover,
        color: c.textSec,
        border: `1px solid ${c.borderLight}`,
        letterSpacing: 0.3,
        whiteSpace: "nowrap",
        cursor: "default",
      }}
    >
      +{count}
    </span>
  );
}

// ── DerivedAgencies (catalog Agency cell) ────────────────────────────────────
// AF-1. The Agency column on Jobs and Workflows, in one place so the two cannot
// drift (they were hand-copied, and the workflows one was missed entirely when
// the jobs column shipped).
//
// The change AF-1 makes is that this column no longer says "—" for a global
// definition. An em dash reads as MISSING DATA, so the one state that used to be
// reachable by simply not filling a field looked identical to a bug — and that is
// precisely the state the band set out to make deliberate and visible. A global
// definition now says "All", which is an answer.
//
// Three distinct states, never collapsed:
//   · no scope        → "All" (deliberate global; every agency sees it)
//   · scope, agencies → the agency chips
//   · scope, none     → "—", titled: the scope maps to no agency, so only
//                        unrestricted admins see it. Fail-closed, and almost
//                        always an unfinished scope→agency mapping (AF-Q3).
export function DerivedAgencies({
  agencies,
  scope,
  derivedFrom,
}: {
  agencies: string[];
  /** The definition's scope; null/"" means global. Workflows have no scope of
   *  their own — they pass undefined and never render the All chip, because a
   *  workflow's agencies come from its jobs' scopes. */
  scope?: string | null;
  /** Tooltip explaining where these came from (they are never stored). */
  derivedFrom: string;
}) {
  if (agencies.length > 0) {
    return (
      <span style={{ display: "inline-flex", gap: 4, flexWrap: "wrap" }}>
        {agencies.map((a) => (
          <span
            key={a}
            title={derivedFrom}
            style={{ fontSize: c.fontXs, padding: "1px 6px", borderRadius: c.radiusChip, border: `1px solid ${c.border}`, color: c.textSec, whiteSpace: "nowrap" }}
          >
            {a}
          </span>
        ))}
      </span>
    );
  }
  if (scope != null && scope === "") {
    return (
      <span
        title="Global — no scope, so every agency can see this."
        style={{ fontSize: c.fontXs, padding: "1px 6px", borderRadius: c.radiusChip, border: `1px dashed ${c.borderStrong}`, color: c.textSec, whiteSpace: "nowrap" }}
      >
        All
      </span>
    );
  }
  return (
    <span
      style={{ color: c.textSec }}
      title={
        scope
          ? `Scope "${scope}" is not mapped to any agency, so only unrestricted admins see this. Map it under Settings → Scopes.`
          : derivedFrom
      }
    >
      —
    </span>
  );
}

// AGENCY_SORT_KEY — the sort value behind the Agency column (AF-1). Agencies are
// multi-value, which the table-sorting band excluded as a rule (TS-Q5); this is a
// documented exception, because in practice a definition carries zero or one
// agency and "group my department's jobs together" is the first thing anyone asks
// of the column. Joined so a rare two-agency row sorts stably rather than
// arbitrarily, and global sorts under "All" — its own label, grouped, not mixed
// into the unmapped rows.
export function agencySortKey(agencies: string[] | undefined, scope?: string | null): string {
  const list = agencies ?? [];
  if (list.length > 0) return list.join(", ");
  return scope != null && scope === "" ? "All" : "";
}

// ── InlineTags (capped table cell) ───────────────────────────────────────────
// The single-line, capped tag set shown in a catalog table cell: the first `max`
// tags as TagChips, the rest collapsed into a "+N" TagOverflowChip whose title
// lists them. Renders an em dash when empty. This is the INNER content only — the
// enclosing <td> stays per-view (cell padding/border differ across catalogs) — so
// every catalog's inline tags cap and read identically (tags-support.md §6.1).
export function InlineTags({ tags, max = 2 }: { tags: string[]; max?: number }) {
  if (!tags.length) return <>—</>;
  return (
    <span style={{ display: "inline-flex", gap: 4, flexWrap: "nowrap", alignItems: "center", verticalAlign: "middle" }}>
      {tags.slice(0, max).map((t) => (
        <TagChip key={t} label={t} />
      ))}
      {tags.length > max && (
        <TagOverflowChip count={tags.length - max} title={tags.slice(max).join(", ")} />
      )}
    </span>
  );
}

// ── TagFilterSelect (catalog "Tag" multi-select) ─────────────────────────────
// The "Tag" filter beside a catalog's Type filter. Multi-select (FU-2): the
// operator picks any number of tags and chooses whether a row must carry ANY of
// them (OR, default) or ALL of them (AND). The option list is the sorted union of
// every loaded row's tags. Callers apply the shared `matchesTags` predicate (and,
// for server-paginated catalogs above their load cap, additionally pass the same
// selection to the server as ?tag=&tagMatch=). Empty selection ⇒ no filter.
// Reads `c` at render so it repaints on a Light/Dark toggle (tags-support.md §6.1).

// matchesTags is the client-side filter predicate shared by every tagged catalog
// so single/multi + AND/OR semantics can't drift between views. Mirrors the
// backend tagutil.Filter.Match (exact element, empty ⇒ all).
export function matchesTags(rowTags: string[] | undefined, selected: string[], mode: "any" | "all"): boolean {
  if (selected.length === 0) return true;
  const set = new Set(rowTags ?? []);
  return mode === "all" ? selected.every((t) => set.has(t)) : selected.some((t) => set.has(t));
}

export function TagFilterSelect<T>({
  items,
  selected,
  onChange,
  getTags,
  matchMode = "any",
  onMatchModeChange,
}: {
  items: T[];
  selected: string[];
  onChange: (v: string[]) => void;
  getTags: (item: T) => string[] | undefined;
  matchMode?: "any" | "all";
  onMatchModeChange?: (m: "any" | "all") => void;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open]);

  const options = Array.from(new Set(items.flatMap((it) => getTags(it) ?? []))).sort();
  const toggle = (t: string) => onChange(selected.includes(t) ? selected.filter((x) => x !== t) : [...selected, t]);
  const label = selected.length === 0 ? "All" : selected.length === 1 ? selected[0] : `${selected[0]} +${selected.length - 1}`;

  return (
    <div ref={ref} style={{ position: "relative", display: "inline-flex", alignItems: "center", gap: 6, fontSize: c.fontXs, color: c.textMuted }}>
      Tag
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        style={{
          padding: "8px 10px", borderRadius: c.radiusChip, border: `1px solid ${selected.length ? c.primary : c.border}`,
          background: c.panelInput, color: selected.length ? c.text : c.textMuted, fontSize: c.fontSm, cursor: "pointer",
          display: "inline-flex", alignItems: "center", gap: 6, minWidth: 70,
        }}
        aria-haspopup="listbox"
        aria-expanded={open}
      >
        {label} <span aria-hidden style={{ opacity: 0.6 }}>▾</span>
      </button>
      {open && (
        <div
          role="listbox"
          style={{
            position: "absolute", top: "100%", left: 28, zIndex: 30, marginTop: 4, minWidth: 190, maxHeight: 300,
            overflowY: "auto", background: c.panel, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface,
            boxShadow: c.shadow, padding: 8,
          }}
        >
          {onMatchModeChange && (
            <div style={{ display: "flex", gap: 4, marginBottom: 6, paddingBottom: 6, borderBottom: `1px solid ${c.border}` }}>
              {(["any", "all"] as const).map((m) => (
                <button
                  key={m}
                  type="button"
                  onClick={() => onMatchModeChange(m)}
                  title={m === "any" ? "Match rows with ANY selected tag (OR)" : "Match rows with ALL selected tags (AND)"}
                  style={{
                    flex: 1, padding: "3px 6px", borderRadius: c.radiusChip, fontSize: c.fontXs, cursor: "pointer",
                    border: `1px solid ${matchMode === m ? c.primary : c.border}`,
                    background: matchMode === m ? `${c.primary}22` : "transparent",
                    color: matchMode === m ? c.primary : c.textMuted, fontWeight: matchMode === m ? 600 : 400,
                  }}
                >
                  {m === "any" ? "Any" : "All"}
                </button>
              ))}
            </div>
          )}
          {options.length === 0 ? (
            <div style={{ fontSize: c.fontXs, color: c.textMuted, padding: "4px 2px" }}>No tags</div>
          ) : (
            options.map((t) => (
              <label key={t} style={{ display: "flex", alignItems: "center", gap: 6, padding: "3px 2px", fontSize: c.fontSm, color: c.text, cursor: "pointer" }}>
                <input type="checkbox" checked={selected.includes(t)} onChange={() => toggle(t)} />
                {t}
              </label>
            ))
          )}
          {selected.length > 0 && (
            <button
              type="button"
              onClick={() => onChange([])}
              style={{ marginTop: 6, width: "100%", padding: "4px 6px", borderRadius: c.radiusChip, border: `1px solid ${c.border}`, background: "transparent", color: c.textMuted, fontSize: c.fontXs, cursor: "pointer" }}
            >
              Clear ({selected.length})
            </button>
          )}
        </div>
      )}
    </div>
  );
}

// ── WarningChip / WarningSummaryChip (script body-lint findings) ─────────────
// A script's sync-time body-lint finding. Distinct vocabulary from TagChip (a
// neutral user tag): severity-colored, glyph-led, with the remediation message
// exposed via title + aria-label so it is reachable by keyboard/screen reader,
// not tooltip-only. severity ∈ info|warning|danger maps to statusTone.
function warnGlyph(severity?: string | null): string {
  return severity === "info" ? "ⓘ" : "▲";
}

export function WarningChip({ severity, rule, message }: { severity?: string | null; rule: string; message?: string }) {
  const { color, bg } = statusTone(severity ?? "warning");
  return (
    <span
      role="status"
      aria-label={message ?? rule}
      title={message ?? rule}
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 5,
        padding: "2px 8px",
        borderRadius: c.radiusChip,
        fontSize: c.fontXs,
        fontWeight: 600,
        background: bg,
        color,
        border: `1px solid ${color}50`,
        letterSpacing: 0.3,
        whiteSpace: "nowrap",
      }}
    >
      {warnGlyph(severity)} {rule}
    </span>
  );
}

// WarningSummaryChip collapses a script's findings into one highest-severity chip
// with a count, for the catalog row (scannable without expanding).
export function WarningSummaryChip({ warnings }: { warnings: { severity?: string | null; message?: string }[] }) {
  if (!warnings.length) return null;
  const rank = (s?: string | null) => (s === "danger" ? 3 : s === "warning" ? 2 : 1);
  const top = warnings.reduce((a, b) => (rank(b.severity) > rank(a.severity) ? b : a));
  const { color, bg } = statusTone(top.severity ?? "warning");
  const label = warnings.length === 1 ? (top.message ?? "1 issue") : `${warnings.length} issues`;
  return (
    <span
      role="status"
      aria-label={label}
      title={label}
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 5,
        padding: "2px 8px",
        borderRadius: c.radiusChip,
        fontSize: c.fontXs,
        fontWeight: 700,
        background: bg,
        color,
        border: `1px solid ${color}50`,
        whiteSpace: "nowrap",
      }}
    >
      {warnGlyph(top.severity)} {warnings.length}
    </span>
  );
}

// ── EmptyCell (VU2-4) ────────────────────────────────────────────────────────
// The em-dash a table cell shows when it has no value. One vocabulary, one
// tone, so absence stops competing with presence: at the seeded defaults the
// most common glyph on a catalog page was "—", and the eye had to scan every
// one of them to learn nothing.
//
// FAINTER than c.textMuted, by derived opacity rather than a new palette token
// — the tone is deliberately below the AA floor and is a recorded
// non-load-bearing exemption (the same standing the decorative `c.border` has;
// see the §C-4 matrix notes). It is not text a reader must be able to read: it
// is the absence of text, and it stays a dash rather than an empty cell
// because a blank cell in a sortable column reads as a rendering failure.
//
// Scope: TABLE-CELL absence only. The "—" a formatter returns inside a value
// (fmtWhen, fmtDuration, the expanded panel's Fields) is prose and keeps full
// textMuted weight — a cell decides its own emptiness, formatters do not.
// ── DocLink — the one vocabulary for an in-app "read the docs" link ──────────
// Quiet, inline, opens in a new tab. Every href must come from the DOC_LINKS
// registry (components/docLinks.ts) so the drift-guard test can vouch for it —
// a hand-typed href here is exactly the stranded link that test exists to stop.
export function DocLink({ href, children }: { href: string; children: ReactNode }) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      style={{ fontSize: "inherit", color: c.primary, textDecoration: "none", whiteSpace: "nowrap" }}
    >
      {children} &rarr;
    </a>
  );
}

export function EmptyCell() {
  return (
    <span aria-label="no value" style={{ color: c.textMuted, opacity: 0.55 }}>
      —
    </span>
  );
}

// ── SourceBadge (definition origin: amadeus vs git) ──────────────────────────
// Small uppercase marker of a definition's origin on list rows. Shared across
// the Jobs/Schedules/Scripts lists (D3).
//
// VU2-4 — the two sources are deliberately ASYMMETRIC. Git is the majority in
// every catalog that shows this (twelve identical chips per screen on Jobs,
// every row on Scripts), so it renders as quiet text with no chip chrome;
// amadeus keeps the tinted chip. The interesting value is the minority one,
// and the ink is now spent on it. Where source is a constant (Scripts is
// all-git) the column correctly degrades to plain muted text.
export function SourceBadge({ source }: { source?: string }) {
  const isGit = source !== "amadeus";
  return (
    <span
      style={{
        fontSize: c.fontXs,
        fontWeight: 600,
        padding: isGit ? "2px 0" : "2px 7px",
        borderRadius: isGit ? undefined : c.radiusChip,
        textTransform: "uppercase",
        letterSpacing: 0.4,
        background: isGit ? "transparent" : c.panel2,
        border: isGit ? undefined : `1px solid ${c.border}`,
        color: isGit ? c.textMuted : c.primary,
      }}
    >
      {source ?? "git"}
    </span>
  );
}

// ── Btn ──────────────────────────────────────────────────────────────────────
export function Btn({
  children,
  onClick,
  primary,
  danger,
  dangerQuiet,
  small,
  disabled,
  title,
  style,
  ariaExpanded,
  ariaLabel,
}: {
  children: ReactNode;
  onClick?: () => void;
  primary?: boolean;
  danger?: boolean;
  // Row-level destructive actions (Delete/Deregister) use this instead of solid
  // `danger`: transparent, danger text + border, dangerBg tint on hover. Reserves
  // the loud solid fill for the confirm button inside ConfirmDialog (VC.6).
  dangerQuiet?: boolean;
  small?: boolean;
  disabled?: boolean;
  title?: string;
  style?: CSSProperties;
  /** For a button that opens something in place (FX-1's Score expander). Named
   *  like HoverTr's `ariaExpanded` rather than spread as arbitrary props, so the
   *  component keeps one explicit surface. */
  ariaExpanded?: boolean;
  /** Disambiguates a short visible label that repeats elsewhere on the page. Must
   *  CONTAIN the visible text (WCAG 2.5.3) — "Collapse the full score" for a
   *  button reading "Collapse", never a different phrase. */
  ariaLabel?: string;
}) {
  const [hover, setHover] = useState(false);
  const solid = primary || danger;
  // Every variant answers the cursor (VU-3). `primaryHover` was defined in both
  // palettes and referenced nowhere; secondary (the bordered default) had no
  // hover at all, so most buttons in the app read as inert.
  const lit = hover && !disabled;
  const background = primary
    ? lit
      ? c.primaryHover
      : c.primary
    : danger
      ? c.danger
      : dangerQuiet && lit
        ? c.dangerBg
        : !solid && !dangerQuiet && lit
          ? c.panelHover
          : "transparent";
  return (
    <button
      onClick={onClick}
      disabled={disabled}
      title={title}
      aria-expanded={ariaExpanded}
      aria-label={ariaLabel}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={{
        padding: small ? "5px 12px" : "8px 18px",
        fontSize: small ? c.fontXs : c.fontSm,
        fontWeight: solid || dangerQuiet ? 600 : 400,
        border: dangerQuiet ? `1px solid ${c.danger}` : solid ? "none" : `1px solid ${c.border}`,
        borderRadius: c.radiusChip,
        background,
        color: solid ? c.onSolid : dangerQuiet ? c.danger : c.text,
        cursor: disabled ? "not-allowed" : "pointer",
        opacity: disabled ? 0.5 : 1,
        whiteSpace: "nowrap",
        transition: "background 0.15s, border-color 0.15s, color 0.15s, opacity 0.15s",
        ...style,
      }}
    >
      {children}
    </button>
  );
}

// ── TabBar ───────────────────────────────────────────────────────────────────
// A tab may be passed as a plain string (every caller but Jobs) or as
// { label, muted } — VU2-4. `muted` drops an EMPTY tab a step below the
// inactive tone so the strip is scannable: the eye should land on the tabs
// that have something behind them. It never applies to the ACTIVE tab, muted
// or not — you clicked it, so it has to read as selected — and a muted tab
// stays fully clickable: "Failed (0)" is a real answer worth navigating to.
type TabSpec = string | { label: string; muted?: boolean };

export function TabBar({ tabs, active, onChange }: { tabs: TabSpec[]; active: number; onChange: (i: number) => void }) {
  return (
    <div style={{ display: "flex", borderBottom: `2px solid ${c.border}`, marginBottom: 16 }}>
      {tabs.map((t, i) => {
        const label = typeof t === "string" ? t : t.label;
        const isActive = i === active;
        const dimmed = typeof t !== "string" && !!t.muted && !isActive;
        return (
          <button
            key={label}
            onClick={() => onChange(i)}
            style={{
              padding: "10px 18px",
              background: "transparent",
              border: "none",
              borderBottom: `2px solid ${isActive ? c.primary : "transparent"}`,
              marginBottom: -2,
              color: isActive ? c.primary : c.textMuted,
              opacity: dimmed ? 0.55 : 1,
              fontSize: c.fontSm,
              fontWeight: isActive ? 600 : 400,
              cursor: "pointer",
              userSelect: "none",
              transition: "border-color 0.2s, color 0.2s, opacity 0.2s",
            }}
          >
            {label}
          </button>
        );
      })}
    </div>
  );
}

// ── AlertBanner ──────────────────────────────────────────────────────────────
export function AlertBanner({
  type = "info",
  onDismiss,
  children,
}: {
  type?: "danger" | "warning" | "info" | "success";
  onDismiss?: () => void;
  children: ReactNode;
}) {
  const map = {
    danger: { color: c.danger, bg: c.dangerBg },
    warning: { color: c.warning, bg: c.warningBg },
    info: { color: c.info, bg: c.infoBg },
    success: { color: c.success, bg: c.successBg },
  }[type];
  return (
    <div
      style={{
        display: "flex",
        alignItems: "center",
        gap: 10,
        padding: "12px 16px",
        borderRadius: c.radiusSurface,
        marginBottom: 16,
        fontSize: c.fontSm,
        color: map.color,
        background: map.bg,
        border: `1px solid ${map.color}30`,
        transition: "background 0.25s, border-color 0.25s, color 0.25s",
      }}
    >
      <span style={{ flex: 1 }}>{children}</span>
      {onDismiss && (
        <span onClick={onDismiss} style={{ cursor: "pointer", opacity: 0.7, fontSize: c.fontBody, lineHeight: 1 }}>
          ✕
        </span>
      )}
    </div>
  );
}

// ── Notice (inline dismissible info/error bar) ───────────────────────────────
// A lighter sibling of AlertBanner: a left-accent bar for transient inline
// messages. Promoted to the canonical kit (CC.16/CC.17) from envvars/ui.
export function Notice({ kind, onDismiss, children }: { kind: "info" | "error"; onDismiss: () => void; children: ReactNode }) {
  const color = kind === "error" ? c.danger : c.info;
  return (
    <div
      style={{
        display: "flex",
        alignItems: "center",
        gap: 10,
        padding: "8px 12px",
        marginBottom: 12,
        border: `1px solid ${color}40`,
        borderLeft: `3px solid ${color}`,
        borderRadius: c.radiusSurface,
        background: c.panel,
        fontSize: c.fontSm,
        color: kind === "error" ? c.danger : c.text,
      }}
    >
      <span style={{ flex: 1 }}>{children}</span>
      <span onClick={onDismiss} style={{ cursor: "pointer", color: c.textSec, fontSize: c.fontBody, lineHeight: 1 }}>
        ✕
      </span>
    </div>
  );
}

// ── Modal (backdrop + centered panel) ────────────────────────────────────────
// The one modal overlay for the app (CC.16/CC.17): a blurred 0.5 backdrop at
// zIndex 100 with a click-outside close and a titled panel. Replaces the
// hand-rolled overlays in Jobs/Runners/PublishBuilder.
//
// FX-10 — `footer` is the pinned action bar, and it exists because EVERY long
// modal in the app had the same defect: the panel caps at 84vh and scrolls, so
// Save/Cancel — and the validation error explaining why Save is disabled — slid
// below the fold on a short viewport. The Jobs run dialog grew a hand-rolled
// sticky bar for exactly this; FX-6 then found the trap in that approach (a
// sticky element pinned by its bottom edge grows UPWARD over the content when it
// outgrows the scrollport). So this does not copy the sticky version eight times:
// with a footer, the panel becomes a flex column and the footer sits OUTSIDE the
// scrollport entirely, where it cannot overlap anything however tall either part
// gets. Put the buttons and their validation error in `footer`; put everything
// that can grow in `children`.
//
// Without a footer the markup is exactly what it was, so no existing modal
// changes behaviour by being recompiled.
// RU-13 — the live viewport width, for layout decisions that inline-per-render
// styles cannot express as CSS media queries (the theme-staleness rule keeps all
// styling inline, so a breakpoint has to be a render-time value). SSR/jsdom-safe:
// defaults to 0 when `window` is absent, and jsdom's 1024 default lands every
// existing test in the narrow branch untouched.
function useViewportWidth(): number {
  const [width, setWidth] = useState(typeof window !== "undefined" ? window.innerWidth : 0);
  useEffect(() => {
    const onResize = () => setWidth(window.innerWidth);
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, []);
  return width;
}

// RU-13 — the breakpoint at which a rail-capable Modal goes two-pane. One
// exported constant so tests and call sites cannot drift.
const RAIL_BREAKPOINT = 1200;

export function Modal({
  title,
  onClose,
  wide,
  footer,
  rail,
  children,
}: {
  title: string;
  onClose: () => void;
  wide?: boolean;
  /** Pinned below the scrolling body: the action row, plus any error that explains a disabled action. */
  footer?: ReactNode;
  /** RU-13 — the wide two-pane layout. At ≥RAIL_BREAKPOINT the panel widens to
      ~960px and this node renders as a fixed right-hand column beside the
      scrolling body, REPLACING `footer` (the caller passes both; the viewport
      picks). Below the breakpoint the rail is ignored and `footer` renders
      exactly as before — the stacked layout is the narrow fallback, not a
      degraded mode. Resizing across the breakpoint mid-dialog just re-parents
      the nodes; all state lives in the caller, so nothing is lost. */
  rail?: ReactNode;
  children: ReactNode;
}) {
  const viewport = useViewportWidth();
  const railActive = !!rail && viewport >= RAIL_BREAKPOINT;
  // A hairline under the title so scrolled content visibly passes UNDER the
  // header instead of looking sliced off at the top of the scrollport.
  const header = (
    <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", paddingBottom: 12, marginBottom: 12, borderBottom: `1px solid ${c.borderLight}` }}>
      <div style={{ fontSize: c.fontHead, fontWeight: 600 }}>{title}</div>
      <button
        type="button"
        onClick={onClose}
        aria-label="Close"
        style={{ background: "none", border: "none", cursor: "pointer", color: c.textSec, fontSize: c.fontHead, lineHeight: 1, padding: 4, margin: -4, borderRadius: c.radiusChip }}
      >
        ✕
      </button>
    </div>
  );
  // The scrolling body keeps a gutter between content and its scrollbar —
  // without it the bar sits flush against right-aligned section summaries and
  // the panel reads as cramped. Padding lives INSIDE the scroll box so the bar
  // stays at the panel edge; `stable` stops content shifting when it appears.
  // No fade at the bottom edge: a fade clips the last row of a body that does
  // NOT scroll (the confirm window's deviation list lost its final entry to
  // it), and a hairline above the footer marks the boundary just as well.
  const scrollBody: CSSProperties = { overflowY: "auto", minHeight: 0, paddingRight: 14, scrollbarGutter: "stable" };
  // ~88% of the panel colour — a hint of the page behind, not a window onto it.
  const modalSurface = `${c.panel}e0`;
  // Read during render, never hoisted to a module const — the theme toggle has to
  // reach these (theme.ts).
  const backdrop: CSSProperties = {
    position: "fixed",
    inset: 0,
    background: "rgba(0,0,0,0.5)",
    backdropFilter: "blur(4px)",
    display: "flex",
    alignItems: "center",
    justifyContent: "center",
    zIndex: 100,
  };
  const panel: CSSProperties = {
    background: modalSurface,
    // Frosted: the backdrop already dims and softly blurs the page; a stronger
    // blur under the surface itself is what keeps text legible over whatever
    // scrolled content lies beneath. Contrast stays within the gate because the
    // page under the dialog is already half-black (the backdrop wash).
    backdropFilter: "blur(14px)",
    WebkitBackdropFilter: "blur(14px)",
    border: `1px solid ${c.border}`,
    borderRadius: c.radiusSurface,
    padding: 20,
    width: railActive ? 960 : wide ? 640 : 480,
    maxWidth: "92vw",
    maxHeight: "84vh",
    boxShadow: c.shadow,
  };
  if (railActive) {
    return (
      <div onClick={onClose} style={backdrop}>
        <div onClick={(e) => e.stopPropagation()} style={{ ...panel, display: "flex", flexDirection: "column", overflow: "hidden" }}>
          {header}
          {/* minmax(0,1fr) so the sections column can actually shrink below its
              content width — the same reason the footer layout needs minHeight 0.
              Only the LEFT column scrolls; the rail is the fixed "this run" pane,
              though it gets its own overflow as a relief valve for short
              viewports (a 300px column of deviations can outgrow 84vh). */}
          <div style={{ display: "grid", gridTemplateColumns: "minmax(0,1fr) 300px", gap: 20, flex: "1 1 auto", minHeight: 0 }}>
            <div style={{ display: "flex", flexDirection: "column", minHeight: 0 }}>
              <div style={{ ...scrollBody, flex: "1 1 auto" }}>{children}</div>
            </div>
            <div style={{ overflowY: "auto", minHeight: 0, borderLeft: `1px solid ${c.borderLight}`, paddingLeft: 20 }}>
              {rail}
            </div>
          </div>
        </div>
      </div>
    );
  }
  if (footer) {
    return (
      <div onClick={onClose} style={backdrop}>
        <div onClick={(e) => e.stopPropagation()} style={{ ...panel, display: "flex", flexDirection: "column", overflow: "hidden" }}>
          {header}
          {/* minHeight 0 is what actually lets this shrink: a flex item's default
              min-height is its content, so without it the body refuses to scroll
              and pushes the footer off the panel instead. */}
          <div style={{ ...scrollBody, flex: "1 1 auto" }}>{children}</div>
          <div style={{ flex: "0 0 auto", paddingTop: 12, marginTop: 12, borderTop: `1px solid ${c.borderLight}` }}>{footer}</div>
        </div>
      </div>
    );
  }
  return (
    <div onClick={onClose} style={backdrop}>
      <div onClick={(e) => e.stopPropagation()} style={{ ...panel, overflowY: "auto" }}>
        {header}
        {children}
      </div>
    </div>
  );
}

// ── ConfirmDialog (Modal + cancel/confirm) ───────────────────────────────────
export function ConfirmDialog({
  title,
  message,
  confirmLabel,
  danger = true,
  busy,
  onConfirm,
  onCancel,
}: {
  title: string;
  message: ReactNode;
  confirmLabel: string;
  danger?: boolean;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  // FX-10 — the actions ride Modal's footer, so a long `message` (the AD-mapping
  // removal names three consequences) scrolls under a bar that stays reachable,
  // instead of pushing Cancel/Confirm below the fold. One line here covers every
  // ConfirmDialog call site in the app.
  return (
    <Modal
      title={title}
      onClose={onCancel}
      footer={
        <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
          <Btn onClick={onCancel} disabled={busy}>
            Cancel
          </Btn>
          <Btn primary={!danger} danger={danger} onClick={onConfirm} disabled={busy}>
            {busy ? "Working…" : confirmLabel}
          </Btn>
        </div>
      }
    >
      <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>{message}</div>
    </Modal>
  );
}

// ── FilterSelect (labeled dropdown filter) ───────────────────────────────────
// The generic "Label: [select]" filter control used across the list/history
// views. Promoted to the canonical kit (CC.16) from history/shared.
export function FilterSelect({ label, value, options, onChange }: { label: string; value: string; options: string[]; onChange: (v: string) => void }) {
  return (
    <label style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: c.fontXs, color: c.textSec, whiteSpace: "nowrap" }}>
      {label}
      <select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        style={{
          padding: "7px 10px",
          borderRadius: c.radiusChip,
          border: `1px solid ${c.border}`,
          background: c.panelInput,
          color: c.text,
          fontSize: c.fontXs,
          fontFamily: c.sans,
        }}
      >
        {options.map((o) => (
          <option key={o} value={o}>
            {o}
          </option>
        ))}
      </select>
    </label>
  );
}

// ── Skeleton (shimmer placeholder) ───────────────────────────────────────────
function Skeleton({ width, height = 14, radius, style }: { width?: number | string; height?: number; radius?: number; style?: CSSProperties }) {
  return (
    <div
      style={{
        width: width ?? "100%",
        height,
        // Default read at render, not in the parameter list: a default there
        // would be evaluated per call anyway, but keeping it here makes the
        // token read obvious next to the other c.* reads (theme.ts).
        borderRadius: radius ?? c.radiusChip,
        background: `linear-gradient(90deg, ${c.panel} 25%, ${c.panelHover} 37%, ${c.panel} 63%)`,
        backgroundSize: "400% 100%",
        animation: "shimmer 1.4s ease infinite",
        ...style,
      }}
    />
  );
}

// ── Toast (FX-16) ────────────────────────────────────────────────────────────
// The one success channel. Success feedback used to land in three different
// places — a fixed toast (hand-inlined identically in Jobs and Workflows, and
// the two copies had already drifted: one set `color: c.text`, the other
// inherited whatever it landed on), a top-of-page banner, or an inline notice —
// so "it worked" looked different depending on which view you were in.
//
// Pairs with `useToast` (hooks.ts), which owns the timer. Renders nothing when
// there is no message, so a caller can mount it unconditionally.
export function Toast({ message }: { message: string | null }) {
  if (!message) return null;
  return (
    <div
      role="status"
      aria-live="polite"
      style={{
        position: "fixed",
        bottom: 24,
        right: 24,
        background: c.panel2,
        border: `1px solid ${c.success}60`,
        borderLeft: `3px solid ${c.success}`,
        borderRadius: c.radiusSurface,
        padding: "10px 16px",
        color: c.text,
        fontSize: c.fontSm,
        // Keeps its shadow under VU-5's overlay exception: the toast floats over
        // arbitrary page content, so the shadow is separation, not decoration.
        boxShadow: c.shadow,
        zIndex: 1000,
      }}
    >
      {message}
    </div>
  );
}

// ── CopyButton (FX-13) ───────────────────────────────────────────────────────
// One copy affordance, where the app had nine in five styles. The user-visible
// defect it closes is the runner-ID copy: a bare `<span onClick>` with no role,
// no tab stop, no keyboard path, and no "Copied" feedback — click it and nothing
// whatsoever tells you it worked. Three more sites were the same span pattern.
//
// This is always a real <button> with an accessible name, always says whether it
// worked, and always awaits the write before claiming success (two sites flashed
// "Copied" on a promise they never checked). It hides itself when the browser
// withholds the clipboard API — in a non-secure context the affordance can only
// fail, and a button that cannot work is worse than none.
//
// Variants exist because the app genuinely needs three shapes, not because three
// grew by accident: `glyph` for a copy sitting inside a dense row, `text` for a
// labelled inline action, `btn` for a normal button in a form or toolbar.
export function CopyButton({
  text,
  label,
  variant = "btn",
  overlay,
  title,
  ariaLabel,
  disabled,
  onCopied,
  stopPropagation,
  style,
}: {
  /** What lands on the clipboard. Empty ⇒ nothing to copy, so nothing renders. */
  text: string;
  /** Visible label. Omitted for `glyph`, defaults to "Copy" elsewhere. */
  label?: string;
  variant?: "glyph" | "text" | "btn";
  /** Pin to the top-right of a `position: relative` parent (a code block or log pane). */
  overlay?: boolean;
  title?: string;
  ariaLabel?: string;
  disabled?: boolean;
  /** For a caller that reports the copy its own way (e.g. a page-level notice). */
  onCopied?: () => void;
  /** The row behind this button is itself clickable — do not toggle it too. */
  stopPropagation?: boolean;
  style?: CSSProperties;
}) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => () => { if (timer.current) clearTimeout(timer.current); }, []);
  if (!text || typeof navigator === "undefined" || !navigator.clipboard) return null;

  const shown = label ?? (variant === "glyph" ? undefined : "Copy");
  const name = ariaLabel ?? (shown ? `Copy ${shown === "Copy" ? "to clipboard" : shown}` : "Copy to clipboard");
  const copy = async (e: React.MouseEvent) => {
    if (stopPropagation) e.stopPropagation();
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      // Permission denied mid-flight: stay in the resting state rather than
      // claiming a copy that did not happen.
      return;
    }
    setCopied(true);
    onCopied?.();
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setCopied(false), COPY_FLASH_MS);
  };

  const base: CSSProperties = {
    display: "inline-flex",
    alignItems: "center",
    gap: 5,
    cursor: disabled ? "not-allowed" : "pointer",
    opacity: disabled ? 0.5 : 1,
    font: "inherit",
    whiteSpace: "nowrap",
    ...(overlay ? { position: "absolute", top: 6, right: 6 } : null),
  };
  const skin: CSSProperties =
    variant === "btn"
      ? { padding: "5px 12px", fontSize: c.fontXs, border: `1px solid ${c.border}`, borderRadius: c.radiusChip, background: c.panel, color: copied ? c.success : c.text }
      : variant === "text"
        ? { padding: 0, border: "none", background: "none", fontSize: c.fontXs, color: copied ? c.success : c.primary }
        : { padding: 0, border: "none", background: "none", color: copied ? c.success : c.textSec };

  return (
    <button type="button" onClick={copy} disabled={disabled} aria-label={name} title={title ?? (copied ? "Copied!" : name)} style={{ ...base, ...skin, ...style }}>
      <CopyGlyph copied={copied} />
      {shown && (
        // aria-live so the confirmation is announced, not only seen; the button's
        // own name stays constant so its identity does not shift underfoot.
        <span aria-live="polite">{copied ? "Copied" : shown}</span>
      )}
    </button>
  );
}

/** The one clipboard glyph in the app, flipping to a check on success. */
function CopyGlyph({ copied }: { copied: boolean }) {
  return copied ? (
    <svg width={11} height={11} viewBox="0 0 24 24" fill="none" stroke={c.success} strokeWidth={3} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <path d="M20 6L9 17l-5-5" />
    </svg>
  ) : (
    <svg width={11} height={11} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" aria-hidden style={{ opacity: 0.6 }}>
      <rect x="9" y="9" width="11" height="11" rx="2" />
      <path d="M5 15V5a2 2 0 0 1 2-2h10" />
    </svg>
  );
}

/** How long "Copied" stays up. Was 1200/1500/2000ms across the old copies — the
 *  differences were incidental, so they are gone. */
const COPY_FLASH_MS = 1500;

// CopyText is CopyButton's sibling for the case where the VALUE is the
// affordance — a runner id, a fingerprint — and turning it into a button would
// lose the monospace value it is showing. The value renders inside the button, so
// it is still one tab stop with one accessible name.
export function CopyText({
  text,
  display,
  title,
  stopPropagation,
  style,
}: {
  text: string;
  /** What is shown, when it differs from what is copied (a shortened trace id). */
  display?: ReactNode;
  title?: string;
  stopPropagation?: boolean;
  style?: CSSProperties;
}) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => () => { if (timer.current) clearTimeout(timer.current); }, []);
  if (!text) return null;
  const copyable = typeof navigator !== "undefined" && !!navigator.clipboard;
  const body = (
    <>
      {display ?? text}
      {copyable && <CopyGlyph copied={copied} />}
    </>
  );
  const shell: CSSProperties = { display: "inline-flex", alignItems: "center", gap: 5, font: "inherit", color: "inherit", padding: 0, border: "none", background: "none", ...style };
  // Without a clipboard there is nothing to press, so it renders as plain text
  // rather than a button that does nothing.
  if (!copyable) return <span style={shell}>{body}</span>;
  return (
    <button
      type="button"
      aria-label={`Copy ${text} to clipboard`}
      title={title ?? (copied ? "Copied!" : `${text} — click to copy`)}
      onClick={async (e) => {
        if (stopPropagation) e.stopPropagation();
        try {
          await navigator.clipboard.writeText(text);
        } catch {
          return;
        }
        setCopied(true);
        if (timer.current) clearTimeout(timer.current);
        timer.current = setTimeout(() => setCopied(false), COPY_FLASH_MS);
      }}
      style={{ ...shell, cursor: "pointer" }}
    >
      {body}
      <span aria-live="polite" style={{ position: "absolute", width: 1, height: 1, overflow: "hidden", clip: "rect(0 0 0 0)", whiteSpace: "nowrap" }}>
        {copied ? "Copied" : ""}
      </span>
    </button>
  );
}

// ── InlineLoading (FX-15) ────────────────────────────────────────────────────
// The one wording and the one style for "this is still arriving", for the places
// a skeleton makes no sense — a form, a detail pane, an inline value. There were
// eight wordings ("Loading…", "Loading runs…", "Loading tokens…", "Loading
// steps…", "Loading references…", "Loading job detail…", "Loading inventory…",
// "Loading editor…") in four paddings and two colours.
//
// `what` keeps the specific ones specific where that genuinely helps ("Loading
// references…" appears beside other content that is already loaded) without
// letting each site invent its own casing and spacing.
export function InlineLoading({ what, style }: { what?: string; style?: CSSProperties }) {
  return (
    <div role="status" aria-live="polite" style={{ fontSize: c.fontSm, color: c.textMuted, padding: "8px 0", ...style }}>
      {what ? `Loading ${what}…` : "Loading…"}
    </div>
  );
}

// ── IcRefresh + RefreshButton (EP-3) ─────────────────────────────────────────
// The glyph was module-private in Shell.tsx, where the top-bar control lives.
// Hoisted here so the app's two refresh affordances — the top bar's "Reload
// page" and the expanded panels' "Refresh" — are drawn with ONE icon rather
// than a copy that drifts. Shell imports it from here now.
export function IcRefresh({ size = 16 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 18 18" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <path d="M14.5 4.5v3.5h-3.5M3.5 13.5v-3.5h3.5M4 8a5 5 0 018.5-2.5L14.5 8M14 10a5 5 0 01-8.5 2.5L3.5 10" />
    </svg>
  );
}

// RefreshButton — the ONE per-panel refresh control (EP-3). One component per
// vocabulary: a second hand-rolled copy is the regression this file exists to
// prevent.
//
// It carries no props for what to refresh. Inside a <RefreshScope> it bumps the
// ambient nonce, and every useGet in that subtree re-fetches in EP-2's refresh
// mode — including fetches in nested components the button has no reference to,
// and including fetches added to the panel years from now.
//
// `also` is the Shape-B half (EP-4): five panels render list-row data and fetch
// nothing of their own, so their button additionally drives the view's existing
// list counter. The button looks and reads identically either way — which kind
// of panel this is stays an implementation detail the operator never sees.
//
// Disabled while work is genuinely in flight, from the scope's real in-flight
// count — never a fixed timeout, which would claim to be working on a fast
// request and claim to be finished on a slow one. No capability gate: a refresh
// has no precondition and authorizes nothing (the FX-7 rule cuts the other way
// here — there is nothing to disable-with-explanation).
export function RefreshButton({ also, title }: { also?: () => void; title?: string }) {
  const { refresh, pending } = useRefreshAction(also);
  const busy = pending > 0;
  return (
    <button
      type="button"
      onClick={refresh}
      disabled={busy}
      aria-busy={busy}
      aria-label="Refresh this panel"
      title={title ?? "Re-fetch this panel's data"}
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 6,
        padding: "3px 9px",
        borderRadius: c.radiusChip,
        border: `1px solid ${c.border}`,
        background: "transparent",
        color: busy ? c.textMuted : c.textSec,
        fontFamily: c.sans,
        fontSize: c.fontXs,
        cursor: busy ? "default" : "pointer",
      }}
    >
      <IcRefresh size={12} />
      {busy ? "Refreshing…" : "Refresh"}
    </button>
  );
}

// DetailPanel (EP-4) — the wrapper every expanded row's content sits in.
//
// Two jobs, both of which must be uniform across thirteen panels or the control
// stops meaning one thing: it opens a RefreshScope (so one button re-fetches
// everything inside, including nested components' requests), and it renders the
// panel's action strip top-right — Refresh FIRST, then whatever else the panel
// offers. Refresh leads because it is the non-destructive one and must not sit
// beside Delete.
//
// `also` is for the five panels that render LIST-ROW data and fetch nothing of
// their own (Scopes, the three Env Vars tabs, Git Sync): there the button
// additionally drives the view's own list reload. EP-Q1 — they get the button
// like everyone else; which shape a panel is stays an implementation detail the
// operator never has to model. Pass a reload that does NOT collapse the
// expansion, or the button closes the panel it refreshed.
//
// THIS COMPONENT DOES NOT PROVIDE THE SCOPE — <RefreshScope> goes at the CALL
// SITE, around the detail component. Found the hard way: React context reaches
// DESCENDANTS, so a provider rendered inside a detail's own `return` leaves that
// component's own useGet calls OUTSIDE it — they run in the component function,
// which sits above the provider in the tree. Only the nested children's fetches
// were covered, and a Jobs panel refresh re-fired 2 of its 5 requests. Nothing
// caught it: tsc passed and 928 unit tests passed; a browser network trace
// found it. Keeping the provider out of here is what makes the mistake hard to
// repeat — placement is now a visible decision at each call site.
export function DetailPanel({
  also,
  actions,
  children,
  style,
}: {
  also?: () => void;
  actions?: ReactNode;
  children: ReactNode;
  style?: CSSProperties;
}) {
  return (
    <div style={style}>
      <div style={{ display: "flex", justifyContent: "flex-end", alignItems: "center", gap: 6, flexWrap: "wrap", marginBottom: 10 }}>
        <RefreshButton also={also} />
        {actions}
      </div>
      {children}
    </div>
  );
}

// ── ExpandChevron (FX-17) ────────────────────────────────────────────────────
// One glyph pair for "this row/section opens", where the app had five: ▲/▼ on
// Jobs, Workflows, Schedules, Scripts and the workflow-runs table; ▾/▸ on Scopes;
// ▼/▶ on the Disclosure fold and the Score; ▾/⋯ on the canvas editor's advanced
// fields. One meaning, five spellings, and no way to learn the vocabulary.
//
// ▼/▶ wins for a reason rather than by vote: it is the disclosure triangle every
// OS and `<details>` element uses, it is what the app's own `Disclosure`
// component already drew, and it frees ▲/▼ — which the registration-token table
// uses for SORT DIRECTION (Runners.tsx). Two different meanings were sharing one
// pair of arrows; now they do not.
//
// Rendered aria-hidden: the row that owns it carries the real name and
// aria-expanded, and a screen reader announcing "black right-pointing triangle"
// adds nothing.
export function ExpandChevron({ open, style }: { open: boolean; style?: CSSProperties }) {
  return (
    <span aria-hidden style={{ color: c.textSec, fontSize: c.fontXs, ...style }}>
      {open ? "▼" : "▶"}
    </span>
  );
}

// ── SortableLabel (TS-3, the sorting-update plan) ───────────────────────────
// The one header-label affordance for column sorting: label text plus the ▲/▼
// caret (the pair ExpandChevron's note reserves for SORT DIRECTION) when the
// column is the active sort. Works inside both <ResizableTh> and a plain <th> —
// the OWNING <th> carries onClick={() => toggle(key)}, aria-sort={ariaSort(key)},
// cursor:pointer and a `title="Sort by …"`; this renders only the visible label.
// Caret is aria-hidden: aria-sort on the <th> is the announced truth.
export function SortableLabel({ label, active, dir }: { label: ReactNode; active: boolean; dir: "asc" | "desc" }) {
  return (
    <>
      {label}
      {active && (
        <span aria-hidden style={{ marginLeft: 4 }}>
          {dir === "asc" ? "▲" : "▼"}
        </span>
      )}
    </>
  );
}

export function SkeletonRows({ rows = 4, cols = 1 }: { rows?: number; cols?: number }) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
      {Array.from({ length: rows }).map((_, r) => (
        <div key={r} style={{ display: "flex", gap: 10 }}>
          {Array.from({ length: cols }).map((_, ci) => (
            <Skeleton key={ci} height={16} />
          ))}
        </div>
      ))}
    </div>
  );
}

// ── Shared style atoms ───────────────────────────────────────────────────────
// These are FUNCTIONS, not constants: the theme tokens (`c`) are mutated in
// place on theme toggle (theme.ts), so a style object must be rebuilt on every
// render to pick up the active palette. Call them at the call site —
// `style={thStyle()}` / `style={{ ...inputStyle(), width: 200 }}`.
export const thStyle = (): CSSProperties => ({
  textAlign: "left",
  padding: "12px 16px",
  // Condensed uppercase for column heads (VU-7 / VU-Q1(b)). This is structural,
  // not decorative: History's table is nine columns wide, and the condensed face
  // buys real characters-per-pixel there — a head that used to truncate now
  // fits. It also gives the eye a second axis of hierarchy (width) in an app
  // whose type otherwise lived inside a 4px range.
  fontFamily: c.sansCond,
  fontWeight: 600,
  fontSize: c.fontXs,
  textTransform: "uppercase",
  letterSpacing: 0.7,
  color: c.textMuted,
  borderBottom: `1px solid ${c.border}`,
  whiteSpace: "nowrap",
  fontVariantNumeric: "tabular-nums",
});

export const tdStyle = (): CSSProperties => ({
  padding: "10px 16px",
  fontSize: c.fontSm,
  verticalAlign: "middle",
  borderBottom: `1px solid ${c.borderLight}`,
  // Proportional digits make counts, durations and timestamps ripple as the data
  // refreshes — a 1 is narrower than a 0, so a live column jitters. Tabular
  // figures also let numeric columns actually line up once right-aligned (VU-4).
  // Applied to every cell, not just numeric ones: it is a no-op on prose.
  fontVariantNumeric: "tabular-nums",
});

// ── ResizableTh (V1.1-7) ─────────────────────────────────────────────────────
// Drop-in <th> with a drag handle on its right border. Pair with the
// `useColumnWidths(tableId)` hook (hooks.ts) and set `tableLayout: "fixed"` on
// the <table> so explicit widths take effect. The handle stops click/​mousedown
// propagation so resizing never triggers a column-sort or row-expand.
export function ResizableTh({
  children,
  width,
  minWidth = 60,
  maxWidth = 640,
  onResize,
  onClick,
  title,
  style,
  ariaSort,
}: {
  children?: ReactNode;
  width?: number;
  minWidth?: number;
  maxWidth?: number;
  onResize: (w: number) => void;
  onClick?: () => void;
  title?: string;
  style?: CSSProperties;
  // Set on the active sort column when the header doubles as a sort toggle
  // (useTableSort's ariaSort(key) returns exactly this value).
  ariaSort?: "ascending" | "descending";
}) {
  const ref = useRef<HTMLTableCellElement>(null);
  const [dragging, setDragging] = useState(false);

  const startResize = (e: React.MouseEvent) => {
    e.preventDefault();
    e.stopPropagation();
    const startX = e.clientX;
    const startW = ref.current?.offsetWidth ?? width ?? 120;
    setDragging(true);
    const move = (ev: MouseEvent) => {
      onResize(Math.min(maxWidth, Math.max(minWidth, startW + (ev.clientX - startX))));
    };
    const up = () => {
      setDragging(false);
      document.removeEventListener("mousemove", move);
      document.removeEventListener("mouseup", up);
    };
    document.addEventListener("mousemove", move);
    document.addEventListener("mouseup", up);
  };

  return (
    <th
      ref={ref}
      title={title}
      onClick={onClick}
      aria-sort={ariaSort}
      style={{
        ...thStyle(),
        ...style,
        width: width ? width : undefined,
        position: "relative",
        overflow: "hidden",
        textOverflow: "ellipsis",
        userSelect: dragging ? "none" : undefined,
      }}
    >
      {children}
      <span
        onMouseDown={startResize}
        onClick={(e) => e.stopPropagation()}
        aria-hidden
        style={{
          position: "absolute",
          top: 0,
          right: 0,
          height: "100%",
          width: 6,
          cursor: "col-resize",
          userSelect: "none",
          background: dragging ? c.primary : "transparent",
        }}
      />
    </th>
  );
}

export const inputStyle = (): CSSProperties => ({
  // borderStrong, not border: a control boundary needs 3:1 (C-4 / WCAG 1.4.11).
  border: `1px solid ${c.borderStrong}`,
  borderRadius: c.radiusChip,
  padding: "8px 12px",
  fontSize: c.fontSm,
  background: c.panelInput,
  color: c.text,
  fontFamily: "inherit",
  outline: "none",
  width: "100%",
  boxSizing: "border-box",
});

// The one focus treatment for every interactive field (Input/Select below, and
// any caller that wants the same ring on a bespoke control). Inline styles can't
// express :focus and we suppress the browser's default outline on fields, so the
// ring is applied on focus via React state. A themed primary border + soft glow,
// repainting on theme toggle because c.* is read at render.
export const focusRing = (): CSSProperties => ({
  outline: "none",
  borderColor: c.primary,
  boxShadow: `0 0 0 3px ${c.primary}33`,
});

// Input / Select — focus-aware field primitives. They layer the shared focusRing()
// on top of whatever base style the caller passes (e.g. inputStyle() or a view's
// input()), so every field across the app highlights identically on focus instead
// of relying on the inconsistent browser default. Drop-in for <input>/<select>.
export function Input({ style, onFocus, onBlur, ...rest }: InputHTMLAttributes<HTMLInputElement>) {
  const [focused, setFocused] = useState(false);
  return (
    <input
      {...rest}
      onFocus={(e) => {
        setFocused(true);
        onFocus?.(e);
      }}
      onBlur={(e) => {
        setFocused(false);
        onBlur?.(e);
      }}
      style={{ ...inputStyle(), ...style, ...(focused ? focusRing() : null) }}
    />
  );
}

export function Select({ style, onFocus, onBlur, children, ...rest }: SelectHTMLAttributes<HTMLSelectElement>) {
  const [focused, setFocused] = useState(false);
  return (
    <select
      {...rest}
      onFocus={(e) => {
        setFocused(true);
        onFocus?.(e);
      }}
      onBlur={(e) => {
        setFocused(false);
        onBlur?.(e);
      }}
      style={{ ...inputStyle(), cursor: "pointer", ...style, ...(focused ? focusRing() : null) }}
    >
      {children}
    </select>
  );
}

// ── HoverTr (table body row with cursor + hover wash) ────────────────────────
// Inline styles can't express :hover and the mutable-`c` theming rules out a
// static CSS class, so hover state lives in the component. Pass `tint` for a
// persistent background (e.g. c.primaryBg when expanded, or a running-row
// success tint); hover always wins while the cursor is over the row.
export function HoverTr({
  onClick,
  tint,
  hoverTint,
  children,
  style,
  onKeyDown,
  tabIndex,
  role,
  ariaLabel,
  ariaExpanded,
}: {
  onClick?: () => void;
  tint?: string;
  hoverTint?: string;
  children: ReactNode;
  style?: CSSProperties;
  // Optional a11y/keyboard passthrough so a clickable row (e.g. a folder row in
  // FolderBrowser, or an expandable runner row) can be focused and activated by
  // keyboard. ariaExpanded reflects the open/closed state of a row that toggles a
  // detail sub-row.
  onKeyDown?: (e: KeyboardEvent<HTMLTableRowElement>) => void;
  tabIndex?: number;
  role?: string;
  ariaLabel?: string;
  ariaExpanded?: boolean;
}) {
  const [h, setH] = useState(false);
  // A clickable row is reachable from the keyboard by default (VU-2) — the
  // global :focus-visible ring (global-css.ts) is only worth anything on an
  // element that can take focus, and before this a mouse was the only way to
  // expand a table row. Callers that already manage their own tab order
  // (FolderBrowser, Runners) pass tabIndex/onKeyDown explicitly and keep it.
  const click = onClick; // const so the narrowing survives into the handler
  const effTabIndex = tabIndex ?? (click ? 0 : undefined);
  const handleKeyDown =
    onKeyDown ??
    (click
      ? (e: KeyboardEvent<HTMLTableRowElement>) => {
          // Space would otherwise scroll the page; Enter/Space both activate,
          // matching what the row does under a click.
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            click();
          }
        }
      : undefined);
  return (
    <tr
      onClick={onClick}
      onKeyDown={handleKeyDown}
      tabIndex={effTabIndex}
      role={role}
      aria-label={ariaLabel}
      aria-expanded={ariaExpanded}
      onMouseEnter={() => setH(true)}
      onMouseLeave={() => setH(false)}
      style={{
        cursor: onClick ? "pointer" : "default",
        background: h ? hoverTint ?? c.panelHover : tint ?? "transparent",
        transition: "background 0.15s",
        ...style,
      }}
    >
      {children}
    </tr>
  );
}

// ── TypeBadge (runtime/language icon badge) ──────────────────────────────────
// Renders the runtime's icon (the prototype's webp set). The shell/perl icon is
// theme-aware — a light glyph in dark mode, a dark glyph in light mode — so it
// stays legible against the badge tint. Unknown types fall back to a short
// uppercase label.
function typeIcon(t: string, isDark: boolean): string | null {
  switch (t) {
    case "bash":
    case "sh":
    case "shell":
    case "perl":
      return isDark ? shellIconLight : shellIconDark;
    case "ansible":
      return ansibleIcon;
    case "powershell":
    case "ps":
    case "pwsh":
      return powershellIcon;
    case "terraform":
    case "tf":
      return terraformIcon;
    case "python":
    case "py":
      return pythonIcon;
    default:
      return null;
  }
}

// Theme-aware tints (the prototype hardcoded pastels that wash out in dark).
function typeTone(t: string): { bg: string; color: string } {
  switch (t) {
    case "bash":
    case "sh":
    case "perl":
      return { bg: c.infoBg, color: c.info };
    case "ansible":
      return { bg: c.primaryBg, color: c.primary };
    case "powershell":
    case "ps":
      return { bg: c.accentBg, color: c.accent };
    case "terraform":
    case "tf":
      return { bg: `${c.success}1f`, color: c.success };
    case "python":
    case "py":
      // Python's brand blue, hardcoded (like terraform's purple in Runners) so it
      // reads distinctly on both themes rather than colliding with bash/perl's info tint.
      return { bg: "#3776ab1f", color: "#3776ab" };
    default:
      return { bg: c.panelHover, color: c.textSec };
  }
}
// TypeBadge renders a run-type as a colored chip. Two shapes from one component
// (VC.7): icon-only (Jobs/History dense columns — the default) and `withLabel`
// (Scopes supported-types, Runners capabilities — icon + lowercase mono name).
// `dashed` draws a dashed accent border for Scopes' inferred-vs-declared origin
// cue; `title` overrides the hover tooltip (e.g. to carry that origin).
export function TypeBadge({
  type,
  size = 16,
  withLabel = false,
  dashed = false,
  title,
}: {
  type?: string | null;
  size?: number;
  withLabel?: boolean;
  dashed?: boolean;
  title?: string;
}) {
  const { mode } = useTheme();
  const t = (type ?? "").toLowerCase();
  const { bg, color } = typeTone(t);
  const src = typeIcon(t, mode === "dark");
  const base: CSSProperties = {
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    gap: withLabel ? 5 : 0,
    minWidth: withLabel ? undefined : 28,
    height: 22,
    padding: withLabel ? "3px 8px" : "3px 7px",
    borderRadius: c.radiusChip,
    background: bg,
    color,
    border: dashed ? `1px dashed ${color}` : undefined,
    boxSizing: "border-box",
  };
  const textStyle: CSSProperties = {
    fontSize: c.fontXs,
    fontWeight: 700,
    textTransform: "uppercase",
    letterSpacing: 0.3,
    whiteSpace: "nowrap",
  };
  const labelStyle: CSSProperties = {
    fontFamily: c.mono,
    fontSize: c.fontXs,
    fontWeight: 600,
    whiteSpace: "nowrap",
  };
  const icon = src ? (
    <img src={src} alt={withLabel ? "" : t} style={{ width: size, height: size, objectFit: "contain", display: "block" }} />
  ) : null;
  if (withLabel) {
    return (
      <span title={title ?? t ?? undefined} style={base}>
        {icon}
        <span style={labelStyle}>{t || "—"}</span>
      </span>
    );
  }
  return (
    <span title={title ?? t ?? undefined} style={src ? base : { ...base, ...textStyle }}>
      {icon ?? (t || "—")}
    </span>
  );
}

// ── SearchBar (magnifier icon + borderless input in a bordered container) ─────
function IcSearch({ size = 14 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <circle cx="11" cy="11" r="7" />
      <line x1="21" y1="21" x2="16.65" y2="16.65" />
    </svg>
  );
}
export function SearchBar({
  value,
  onChange,
  placeholder,
  style,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  style?: CSSProperties;
}) {
  return (
    <div
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 8,
        ...inputStyle(),
        padding: "7px 12px",
        ...style,
      }}
    >
      <span style={{ color: c.textMuted, display: "inline-flex", flexShrink: 0 }}>
        <IcSearch />
      </span>
      <input
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        style={{
          flex: 1,
          minWidth: 0,
          border: "none",
          outline: "none",
          background: "transparent",
          color: c.text,
          fontSize: c.fontSm,
          fontFamily: "inherit",
        }}
      />
    </div>
  );
}

// ── EnvRowsEditor (shared plaintext key/value env editor) ────────────────────
// One implementation behind every env surface (per-run overrides, per-schedule
// env, schedule-builder env) so they look and behave identically (JC6). Holds an
// ORDERED {key,value}[] — order-preserving and duplicate-tolerant during typing —
// and reports edits via onChange; the CALLER trims empty keys when it serializes
// to a map (so a half-typed row never collapses mid-edit). Mono inputs because
// env keys/values read better fixed-width.
export interface EnvKV {
  key: string;
  value: string;
}

export function EnvRowsEditor({
  rows,
  onChange,
  addLabel = "+ Add variable",
  keyPlaceholder = "KEY",
  valuePlaceholder = "value",
  style,
}: {
  rows: EnvKV[];
  onChange: (rows: EnvKV[]) => void;
  addLabel?: string;
  keyPlaceholder?: string;
  valuePlaceholder?: string;
  style?: CSSProperties;
}) {
  const patch = (i: number, p: Partial<EnvKV>) => onChange(rows.map((r, j) => (j === i ? { ...r, ...p } : r)));
  return (
    <div>
      {rows.map((row, i) => (
        <div key={i} style={{ display: "flex", gap: 6, marginBottom: 6 }}>
          <Input
            placeholder={keyPlaceholder}
            value={row.key}
            onChange={(e) => patch(i, { key: e.target.value })}
            style={{ flex: 1, fontFamily: c.mono, ...style }}
          />
          <Input
            placeholder={valuePlaceholder}
            value={row.value}
            onChange={(e) => patch(i, { value: e.target.value })}
            style={{ flex: 2, fontFamily: c.mono, ...style }}
          />
          <Btn small onClick={() => onChange(rows.filter((_, j) => j !== i))} title="Remove">
            ✕
          </Btn>
        </div>
      ))}
      <Btn small onClick={() => onChange([...rows, { key: "", value: "" }])}>
        {addLabel}
      </Btn>
    </div>
  );
}

// TagEditor — an inline, controlled tag list: chips with a ✕ to remove plus an
// input that commits the typed tag on Enter or comma (and Backspace-on-empty
// removes the last). Blanks are ignored and a case-insensitive duplicate is a
// no-op (first casing wins). It is fully controlled — `onChange` fires the new
// list and the caller persists it; `disabled` renders the chips read-only and
// hides the input. Used by the Scripts detail to edit SQLite-only script tags.
export function TagEditor({
  tags,
  onChange,
  disabled,
  placeholder = "Add a tag…",
}: {
  tags: string[];
  onChange: (next: string[]) => void;
  disabled?: boolean;
  placeholder?: string;
}) {
  const [draft, setDraft] = useState("");
  const add = (raw: string) => {
    const t = raw.trim();
    setDraft("");
    if (!t) return;
    if (tags.some((x) => x.toLowerCase() === t.toLowerCase())) return;
    onChange([...tags, t]);
  };
  const remove = (t: string) => onChange(tags.filter((x) => x !== t));
  return (
    <div style={{ display: "flex", flexWrap: "wrap", gap: 6, alignItems: "center" }}>
      {tags.map((t) => (
        <span
          key={t}
          style={{
            display: "inline-flex",
            alignItems: "center",
            gap: 4,
            padding: disabled ? "2px 8px" : "2px 4px 2px 8px",
            borderRadius: c.radiusChip,
            fontSize: c.fontXs,
            fontWeight: 600,
            background: c.panelHover,
            color: c.textMuted,
            border: `1px solid ${c.borderLight}`,
            letterSpacing: 0.3,
            whiteSpace: "nowrap",
          }}
        >
          {t}
          {!disabled && (
            <button
              // preventDefault on mousedown so clicking ✕ never blurs the input —
              // otherwise the input's onBlur would commit any typed draft first and
              // race this remove (stale-closure double-update).
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => remove(t)}
              title={`Remove ${t}`}
              aria-label={`Remove tag ${t}`}
              style={{ border: "none", background: "transparent", color: c.textMuted, cursor: "pointer", fontSize: c.fontSm, lineHeight: 1, padding: "0 2px" }}
            >
              ×
            </button>
          )}
        </span>
      ))}
      {!disabled && (
        <input
          value={draft}
          placeholder={placeholder}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e: KeyboardEvent<HTMLInputElement>) => {
            if (e.key === "Enter" || e.key === ",") {
              e.preventDefault();
              add(draft);
            } else if (e.key === "Backspace" && draft === "" && tags.length > 0) {
              remove(tags[tags.length - 1]);
            }
          }}
          onBlur={() => add(draft)}
          style={{ ...inputStyle(), width: "auto", flex: "0 1 160px", padding: "4px 8px", fontSize: c.fontXs }}
        />
      )}
    </div>
  );
}

// ── Pagination ───────────────────────────────────────────────────────────────
// Relocated here from views/history/shared.tsx (PP) so catalog pages no longer
// reach into the history/ namespace for a pager; shared.tsx re-exports these as a
// back-compat shim. usePager holds per-table state (default pageSize 25, options
// 25/50/100). The <Pager> control is the canonical "Show 25 / 50 / 100 entries"
// button group + ← Prev / Next → + an "x–y of N" label. The UI is 0-based and
// callers add the +1 for a 1-based API. Pure math (clampPage/sliceForPage) lives
// in utils/pager so it stays unit-testable; import it directly from there.
export interface PagerState {
  page: number;
  setPage: (p: number) => void;
  pageSize: number;
  setPageSize: (n: number) => void;
}

/** Each table calls this once, so page/pageSize never leak across sibling tables. */
export function usePager(): PagerState {
  const [page, setPage] = useState(0);
  const [pageSize, setPageSizeRaw] = useState(25);
  return {
    page,
    setPage,
    pageSize,
    setPageSize: (n: number) => {
      setPageSizeRaw(n);
      setPage(0);
    },
  };
}

const navBtn = (disabled: boolean): CSSProperties => ({
  padding: "4px 10px",
  borderRadius: c.radiusChip,
  border: `1px solid ${c.border}`,
  background: "transparent",
  color: c.textSec,
  fontSize: c.fontXs,
  fontFamily: c.sans,
  cursor: disabled ? "default" : "pointer",
  opacity: disabled ? 0.4 : 1,
});

export function Pager({
  pager,
  page,
  total,
  noun,
  shown,
}: {
  pager: PagerState;
  page: number;
  total: number;
  noun: string;
  /** Rows actually rendered after any client-side (within-page) filtering. When
   * fewer than the server returned for this page, the label says so honestly
   * rather than implying the page-position count reflects the filter (PP-H7). */
  shown?: number;
}) {
  const { pageSize, setPage, setPageSize } = pager;
  const pageCount = Math.max(1, Math.ceil(total / pageSize));
  return (
    <div
      style={{
        display: "flex",
        justifyContent: "space-between",
        alignItems: "center",
        padding: "10px 14px",
        borderTop: `1px solid ${c.border}`,
        fontSize: c.fontXs,
        color: c.textSec,
        // The page-position label counts up as you page; without tabular figures
        // the whole string shifts under the cursor (VU-4).
        fontVariantNumeric: "tabular-nums",
      }}
    >
      <div role="group" aria-label="Rows per page" style={{ display: "flex", alignItems: "center", gap: 6 }}>
        <span>Show</span>
        {[25, 50, 100].map((n) => (
          <button
            key={n}
            aria-pressed={pageSize === n}
            onClick={() => setPageSize(n)}
            style={{
              padding: "3px 9px",
              borderRadius: c.radiusChip,
              cursor: "pointer",
              fontFamily: c.sans,
              fontSize: c.fontSm,
              fontWeight: pageSize === n ? 600 : 400,
              background: pageSize === n ? `${c.primary}1a` : "transparent",
              color: pageSize === n ? c.primary : c.textSec,
              border: `1px solid ${pageSize === n ? `${c.primary}40` : c.border}`,
            }}
          >
            {n}
          </button>
        ))}
        <span>entries</span>
      </div>
      <span>{pagerLabel(total, page, pageSize, noun, shown)}</span>
      <div style={{ display: "flex", gap: 6 }}>
        <button disabled={page === 0} onClick={() => setPage(Math.max(0, page - 1))} style={navBtn(page === 0)}>
          ← Prev
        </button>
        <button
          disabled={page >= pageCount - 1}
          onClick={() => setPage(Math.min(pageCount - 1, page + 1))}
          style={navBtn(page >= pageCount - 1)}
        >
          Next →
        </button>
      </div>
    </div>
  );
}

// ── Expanded-detail primitives (EV-1) ────────────────────────────────────────
// One definition each for the two ideas that had SEVEN implementations across
// the tree before this: a section caption and a label/value cell. Four of the
// five `SectionLabel` copies were byte-identical and the fifth had silently
// drifted (a different colour token and margin) — nobody decided that, it was a
// copy that aged, which is the whole argument for these living here.

// SectionLabel is the flat uppercase caption: one hierarchy step above body
// text, used where a detail is a simple stack of captioned blocks (Scripts,
// Schedules, workflow run detail, the env-var tabs). `action` hosts a
// right-aligned control on the caption row itself — it is where the env-var
// tabs' Reveal/Copy buttons live, so they stay put while the value box below
// scrolls. Prefer `Section` when the block needs a chapter break.
export function SectionLabel({ children, action }: { children: ReactNode; action?: ReactNode }) {
  return (
    <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 10, minHeight: 18, marginBottom: 6 }}>
      <span style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textMuted, textTransform: "uppercase", letterSpacing: 0.7 }}>
        {children}
      </span>
      {action}
    </div>
  );
}

// Section is the expanded-detail chapter break: a sentence-case header one
// visible step ABOVE the XS-uppercase field labels, closed by a hairline, so
// sections read as blocks instead of a flat label stack. Before EV-0, headers
// and field labels shared one type style and ~20 identical rows ran down a
// detail with no way to see where a section began.
//
// `info` is static education text — it costs space on every expansion of every
// row but only teaches once, so it lives behind the ⓘ rather than inline. The
// rule the details follow: education goes behind the toggle, a CONSEQUENCE
// (e.g. "this runner receives secret values") stays inline and conditional.
//
// `actions` is the section-scoped action slot, right-aligned on the header row.
//
// The glyph is DRAWN rather than typed: as the character "i" at fontXs/700 the
// tittle antialiased into the stem at 1x and the badge read as a capital "I".
// A path keeps the gap at whole pixels, so it stays a lowercase i everywhere.
function IcInfo({ size = 14 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" fill="currentColor" aria-hidden>
      <circle cx={8} cy={4.6} r={1.15} />
      <rect x={7.25} y={7} width={1.5} height={5} rx={0.75} />
    </svg>
  );
}

// InfoToggle (EP-7) — the ⓘ affordance itself, extracted from Section so the
// Job Composer's form sections can carry the same one rather than hand-rolling
// a second. One component per vocabulary; `Section` below renders exactly what
// it always did.
//
// Controlled: the caller owns `open`, because the body it reveals lives outside
// this button in both hosts (under a section header here, under a form-field
// label in the Composer) and only the caller knows where to put it.
export function InfoToggle({ open, onToggle }: { open: boolean; onToggle: () => void }) {
  return (
    <button
      type="button"
      onClick={onToggle}
      aria-label="About this section"
      aria-expanded={open}
      title={open ? "Hide explanation" : "What is this?"}
      style={{
        width: 16,
        height: 16,
        borderRadius: "50%",
        border: `1px solid ${open ? c.primary : c.textMuted}`,
        background: "none",
        color: open ? c.primary : c.textMuted,
        lineHeight: 1,
        cursor: "pointer",
        padding: 0,
        display: "inline-flex",
        alignItems: "center",
        justifyContent: "center",
        flexShrink: 0,
      }}
    >
      <IcInfo />
    </button>
  );
}

// InfoBody (EP-7) — the revealed explanation. Shared so the two hosts cannot
// drift on measure, tone or spacing; 70ch is the readable-line cap.
export function InfoBody({ children, margin = "0 0 10px" }: { children: ReactNode; margin?: string }) {
  return (
    <div style={{ fontSize: c.fontXs, color: c.textMuted, margin, maxWidth: "70ch", lineHeight: 1.5 }}>{children}</div>
  );
}

export function Section({
  title,
  info,
  actions,
  children,
}: {
  title: ReactNode;
  info?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
}) {
  const [showInfo, setShowInfo] = useState(false);
  return (
    <div>
      <div style={{ display: "flex", alignItems: "center", gap: 8, paddingBottom: 6, borderBottom: `1px solid ${c.border}`, marginBottom: 10 }}>
        <span style={{ fontSize: c.fontSm, fontWeight: 600, color: c.text }}>{title}</span>
        {info != null && <InfoToggle open={showInfo} onToggle={() => setShowInfo((v) => !v)} />}
        {actions && <span style={{ marginLeft: "auto", display: "inline-flex", gap: 6 }}>{actions}</span>}
      </div>
      {info != null && showInfo && <InfoBody>{info}</InfoBody>}
      {children}
    </div>
  );
}

// One executor option as a selectable card. Born in the Jobs Run dialog's picker
// (R5.2); shared so the Job Composer offers the same surface. Disabled + tooltip
// when the run-type's capability matrix forbids it.
export function ExecutorChoice({
  label,
  sub,
  selected,
  disabled,
  title,
  onClick,
}: {
  label: string;
  sub: string;
  selected: boolean;
  disabled?: boolean;
  title?: string;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      onClick={disabled ? undefined : onClick}
      disabled={disabled}
      title={title}
      style={{
        flex: 1,
        textAlign: "left",
        padding: "9px 12px",
        borderRadius: c.radiusChip,
        border: `1px solid ${selected ? c.primary : c.border}`,
        background: selected ? c.primaryBg : disabled ? c.panel2 : "transparent",
        color: disabled ? c.textMuted : c.text,
        cursor: disabled ? "not-allowed" : "pointer",
        opacity: disabled ? 0.6 : 1,
        fontFamily: "inherit",
      }}
    >
      <div style={{ fontSize: c.fontSm, fontWeight: 600 }}>{label}</div>
      <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 1 }}>{disabled ? "Unavailable" : sub}</div>
    </button>
  );
}

// RP-4 — the ONE form-field label idiom, shared by the Jobs Run dialog and the
// Job Composer so the two authoring surfaces read as the same form. A function,
// not a frozen const: the theme tokens (`c`) mutate in place on theme toggle,
// so a captured literal would keep stale colors. `margin` exists because the
// two surfaces space fields differently (the dialog stacks flat with a 12px
// lead-in; the Composer's Field blocks own their outer spacing).
export const fieldLabelStyle = (margin: string = "12px 0 6px"): CSSProperties => ({
  display: "block",
  fontSize: c.fontXs,
  fontFamily: c.sansCond,
  fontWeight: 600,
  color: c.textMuted,
  textTransform: "uppercase",
  letterSpacing: 0.7,
  margin,
});

// RP-4 — label + control + helper line, the repeating unit of the Run dialog's
// "Where it runs" fold and the Composer's spec form. `helper` renders in the
// 11px secondary tone every field-explanation in both surfaces already uses;
// `helperTone: "danger"` is for a helper that doubles as the field's inline
// validation verdict (e.g. an empty host subset).
// RU-10 — `helperMode` decides WHEN the helper shows. Default "always" is the
// pre-Phase-C behaviour, so every existing call site is untouched until it opts in.
//
// "engaged" exists because the helpers are individually good and collectively a
// wall: one to three lines of prose under nearly every control, permanently, so
// the defaults state of an open section reads as dense regardless of whether the
// operator is changing anything. Engaged shows the helper when the operator is
// actually engaging with the field — focus is inside it, or the caller says the
// control is non-default (`active`) — and hides it otherwise.
//
// Two things deliberately override the mode:
//   · `helperTone === "danger"` always renders. A helper that doubles as the
//     field's validation verdict is not description, it is the error.
//   · A caller that wants a warning or a leak caveat permanent simply leaves the
//     mode at "always". Rule of thumb: what the control DOES → engaged; what
//     could go wrong or leak → always.
//
// Focus is tracked with onFocus/onBlur on the wrapper rather than CSS
// :focus-within, because the visibility is an OR across three arms and only one
// of them is expressible in CSS. React's focus events bubble, so the wrapper sees
// focus landing on any control inside it.
export function FormField({
  label,
  helper,
  helperTone,
  helperMode = "always",
  active,
  labelMargin,
  children,
}: {
  label: string;
  helper?: ReactNode;
  helperTone?: "danger";
  /** "always" (default) renders the helper permanently; "engaged" renders it on
      focus-within, when `active`, or when the tone is danger. */
  helperMode?: "always" | "engaged";
  /** For helperMode "engaged": the control is checked / non-empty / non-default,
      i.e. the operator has done something worth explaining. */
  active?: boolean;
  labelMargin?: string;
  children: ReactNode;
}) {
  const [focused, setFocused] = useState(false);
  const showHelper =
    !!helper && (helperMode === "always" || helperTone === "danger" || active === true || focused);
  return (
    <div
      onFocus={helperMode === "engaged" ? () => setFocused(true) : undefined}
      onBlur={helperMode === "engaged" ? () => setFocused(false) : undefined}
    >
      <label style={fieldLabelStyle(labelMargin)}>{label}</label>
      {children}
      {showHelper && (
        <div style={{ fontSize: c.fontXs, color: helperTone === "danger" ? c.danger : c.textSec, marginTop: 6 }}>
          {helper}
        </div>
      )}
    </div>
  );
}

// RP-4 — the collapsible options fold, promoted from views/jobs/RunInputs.tsx
// (born as the Run dialog's "Where it runs"). `summary` states the collapsed
// content's one un-hideable fact (e.g. the run's targeting) so an operator can
// confirm it without opening anything.
// RU-12 — `tone` lets the bar itself say how routine its contents are, where
// before the four Run-dialog sections carried identical visual authority and
// hierarchy was title-only:
//   · "primary"  — the section that genuinely needs the operator (Run inputs):
//     stronger border, stronger title.
//   · "default"  — the everyday sections, exactly the pre-RU-12 styling.
//   · "quiet"    — the exceptional path (Advanced) and nested subgroups: smaller
//     muted title, fainter border, transparent ground.
// The SUMMARY is deliberately identical across tones: the collapsed Advanced line
// is where CHECK MODE and an active raw --limit live, and the one fact that
// reinterprets the whole run must not shrink with the bar that carries it.
// All tokens are read inside render (theme-staleness rule).
export function Disclosure({
  title,
  summary,
  open,
  onToggle,
  tone = "default",
  children,
}: {
  title: string;
  summary: string;
  open: boolean;
  onToggle: () => void;
  tone?: "primary" | "default" | "quiet";
  children: ReactNode;
}) {
  const headStyle: CSSProperties = {
    display: "flex",
    alignItems: "center",
    gap: 10,
    width: "100%",
    padding: tone === "quiet" ? "8px 12px" : "10px 12px",
    borderRadius: c.radiusSurface,
    border: `1px solid ${tone === "primary" ? c.borderStrong : tone === "quiet" ? `${c.border}80` : c.border}`,
    background: tone === "quiet" ? "transparent" : c.panel2,
    color: c.text,
    font: "inherit",
    textAlign: "left",
    cursor: "pointer",
  };
  const titleStyle: CSSProperties =
    tone === "primary"
      ? { fontSize: c.fontSm, fontWeight: 700 }
      : tone === "quiet"
        ? { fontSize: c.fontXs, fontWeight: 600, color: c.textSec }
        : { fontSize: c.fontSm, fontWeight: 600 };
  return (
    <div>
      <button type="button" onClick={onToggle} aria-expanded={open} style={headStyle}>
        <ExpandChevron open={open} style={{ color: c.textMuted, width: 10 }} />
        <span style={titleStyle}>{title}</span>
        <span style={{ flex: 1 }} />
        <span style={{ fontSize: c.fontXs, color: c.textSec, whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>
          {summary}
        </span>
      </button>
      {open && <div style={{ padding: "4px 2px 0" }}>{children}</div>}
    </div>
  );
}

// Field is one label/value cell in a detail's field grid. The signature is the
// reconciliation of three prior variants (`mono`/`isMono`, `value`/`children`):
// `value` is the one input, and it takes a node, so a caller that used to pass
// children passes them as `value`. `breakAll` is for opaque identifiers (trace
// ids, hashes) that should wrap anywhere rather than at word boundaries.
export function Field({
  label,
  value,
  mono,
  breakAll,
  title,
}: {
  label: string;
  value: ReactNode;
  mono?: boolean;
  breakAll?: boolean;
  /** Native tooltip on the whole cell — for a value that is truncated or needs a gloss. */
  title?: string;
}) {
  return (
    <div title={title}>
      <div style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textMuted, textTransform: "uppercase", letterSpacing: 0.7, marginBottom: 3 }}>{label}</div>
      <div style={{ fontSize: c.fontSm, color: c.text, fontFamily: mono ? c.mono : c.sans, wordBreak: breakAll ? "break-all" : "break-word" }}>{value}</div>
    </div>
  );
}

// ── The column model (CO-2) ──────────────────────────────────────────────────
// Re-exported so an adopting view keeps ONE import for its table vocabulary,
// the same way it already gets ResizableTh / SortableLabel / thStyle from here.
// The implementation lives in ./table to keep this file from growing a second
// subject; importing either path is equivalent.
export {
  TableHead,
  renderCells,
  minWidthOf,
  useTableColumns,
  mergeOrder,
  type TableColumn,
  type TableColumnsApi,
  type TableSortApi,
  type ColumnWidthsApi,
} from "./table";

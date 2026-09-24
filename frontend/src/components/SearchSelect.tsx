import { useEffect, useId, useRef, useState, type CSSProperties, type ReactNode } from "react";
import { c } from "../theme";
import { InlineLoading } from "./ui";
import { useTheme } from "../theme-context";
import { focusRing } from "./ui";

// SearchSelect — a generic, controlled, searchable combobox (JC24). It is the
// shared base for the Composer's Script picker (folder-grouped, select-only,
// server-search past a cap) and Scope picker (flat, creatable), and is built to
// the JC25 quality bar: full keyboard a11y (↑/↓/Home/End/Enter/Esc, type-to-
// filter), role=combobox/listbox/option + aria-activedescendant, click-outside to
// close, theme-safe styling (every colour read from the mutable `c` palette at
// render — never a module-level const, which would freeze the load-time theme and
// break the Light-Mode toggle), and ≥40px touch rows.
//
// Data flow: the consumer owns `items`. The component owns the query and reports it
// via onSearch so a consumer can drive a server query; set disableClientFilter when
// the passed items are already server-filtered. The current selection
// (`selectedItem`) is always rendered even when filtered out, so an edit prefill or
// off-page value never silently drops.
export type SearchSelectProps<T> = {
  value: string;
  onChange: (next: string, item: T | null) => void;
  items: T[];
  getKey: (t: T) => string;
  getSearchText: (t: T) => string;
  renderOption: (t: T) => ReactNode;
  /** Plain-text label for the closed control (defaults to getKey). */
  getLabel?: (t: T) => string;
  /** Resolved current selection — may be off-list (pinned) so it always renders. */
  selectedItem?: T | null;
  /** Left adornment for the closed control (e.g. a run-type chip). */
  renderAdornment?: (t: T) => ReactNode;
  /** Optional folder grouping → non-selectable headers, options sorted within. */
  groupBy?: (t: T) => string;
  groupLabel?: (key: string) => ReactNode;
  /** Accept a typed value not in the list (Scope picker); shows a "Use …" row. */
  creatable?: boolean;
  onCreateLabel?: (typed: string) => ReactNode;
  onCreate?: (typed: string) => void;
  /** Notified of every query change (drives a server ?q= search). */
  onSearch?: (q: string) => void;
  /** Skip client-side filtering — items are already filtered for the query. */
  disableClientFilter?: boolean;
  /** Current value resolved to nothing (e.g. a deleted script) → "missing" marker. */
  missing?: boolean;
  loading?: boolean;
  disabled?: boolean;
  emptyNote?: ReactNode;
  placeholder?: string;
  /** Accessible name for the combobox input (the visual <Field> label is not a <label>). */
  ariaLabel?: string;
  style?: CSSProperties;
};

type NavEntry<T> = { kind: "option"; item: T } | { kind: "create"; value: string };
type Row<T> = { kind: "header"; key: string } | { kind: "option"; item: T; nav: number } | { kind: "create"; value: string; nav: number };

function Chevron({ open }: { open: boolean }) {
  return (
    <svg
      width={14}
      height={14}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
      style={{ flex: "none", transform: open ? "rotate(180deg)" : "none", transition: "transform 0.12s", color: c.textMuted }}
    >
      <polyline points="6 9 12 15 18 9" />
    </svg>
  );
}

export function SearchSelect<T>({
  value,
  onChange,
  items,
  getKey,
  getSearchText,
  renderOption,
  getLabel,
  selectedItem,
  renderAdornment,
  groupBy,
  groupLabel,
  creatable,
  onCreateLabel,
  onCreate,
  onSearch,
  disableClientFilter,
  missing,
  loading,
  disabled,
  emptyNote,
  placeholder,
  ariaLabel,
  style,
}: SearchSelectProps<T>) {
  useTheme(); // re-render on theme toggle so `c.*` reads stay live while the panel is open
  const baseId = useId();
  const optId = (nav: number) => `${baseId}-opt-${nav}`;
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [focused, setFocused] = useState(false);
  const [active, setActive] = useState(0);

  const labelOf = getLabel ?? getKey;
  const closedText = missing ? value : selectedItem ? labelOf(selectedItem) : "";

  // ── Filter + (optional) group → flat render rows + a parallel navigable list ──
  // Built only while open: the panel renders and the keyboard handlers act only
  // when open, so skipping this when closed avoids re-filtering/sorting the whole
  // catalog on every unrelated parent re-render (e.g. a keystroke in a sibling field).
  const rows: Row<T>[] = [];
  const nav: NavEntry<T>[] = [];
  if (open) {
    const trimmed = query.trim().toLowerCase();
    let filtered = !disableClientFilter && trimmed ? items.filter((it) => getSearchText(it).toLowerCase().includes(trimmed)) : items.slice();
    // Always render the current selection, even when filtered out or off-page (§2.5).
    if (value && selectedItem && !filtered.some((it) => getKey(it) === value)) {
      filtered = [selectedItem, ...filtered];
    }
    const pushOption = (item: T) => {
      rows.push({ kind: "option", item, nav: nav.length });
      nav.push({ kind: "option", item });
    };
    if (groupBy) {
      const groups = new Map<string, T[]>();
      for (const it of filtered) {
        const g = groupBy(it);
        if (!groups.has(g)) groups.set(g, []);
        groups.get(g)!.push(it);
      }
      for (const key of [...groups.keys()].sort((a, b) => a.localeCompare(b))) {
        rows.push({ kind: "header", key });
        for (const it of groups.get(key)!) pushOption(it);
      }
    } else {
      for (const it of filtered) pushOption(it);
    }
    const typed = query.trim();
    const showCreate = !!creatable && !!typed && !filtered.some((it) => getKey(it).toLowerCase() === typed.toLowerCase());
    if (showCreate) {
      rows.push({ kind: "create", value: typed, nav: nav.length });
      nav.push({ kind: "create", value: typed });
    }
  }
  // Clamp into range; an empty list (or End/ArrowUp/ArrowDown on it) resolves to 0, not -1.
  const activeClamped = nav.length ? Math.min(Math.max(active, 0), nav.length - 1) : 0;

  // ── Commit / open / close ────────────────────────────────────────────────────
  const close = () => {
    setOpen(false);
    setQuery("");
    setActive(0);
    // Keep the consumer's server-search term (if any) in lockstep with the cleared
    // query, so reopening a server-backed picker doesn't show the prior query's results.
    onSearch?.("");
  };
  const selectOption = (item: T) => {
    onChange(getKey(item), item);
    close();
  };
  const commitCreate = (v: string) => {
    if (onCreate) onCreate(v);
    else onChange(v, null);
    close();
  };
  const commitNav = (entry: NavEntry<T> | undefined) => {
    if (!entry) return;
    if (entry.kind === "option") selectOption(entry.item);
    else commitCreate(entry.value);
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (disabled) return;
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        if (!open) setOpen(true);
        else setActive((a) => Math.min(nav.length - 1, a + 1));
        break;
      case "ArrowUp":
        if (open) {
          e.preventDefault();
          setActive((a) => Math.max(0, a - 1));
        }
        break;
      case "Home":
        if (open) {
          e.preventDefault();
          setActive(0);
        }
        break;
      case "End":
        if (open) {
          e.preventDefault();
          setActive(nav.length - 1);
        }
        break;
      case "Enter":
        if (open) {
          // Swallow Enter while open even with no active row, so it can't trigger an
          // implicit submit if SearchSelect is ever mounted inside a <form>.
          e.preventDefault();
          if (nav[activeClamped]) commitNav(nav[activeClamped]);
        }
        break;
      case "Escape":
        if (open) {
          e.preventDefault();
          e.stopPropagation();
          close();
        }
        break;
    }
  };

  // On open, highlight the current selection (when present) rather than the first
  // row, so an immediate Enter confirms it and the highlight tracks aria-selected.
  useEffect(() => {
    if (!open) return;
    const i = nav.findIndex((n) => n.kind === "option" && getKey(n.item) === value);
    setActive(i >= 0 ? i : 0);
    // nav is rebuilt each render; intentionally seed only on the open transition.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  // Keep the active option scrolled into view as the user arrows through.
  useEffect(() => {
    if (!open) return;
    // optional-chain the method: not all environments implement it (jsdom/SSR).
    listRef.current?.querySelector<HTMLElement>(`[data-nav="${activeClamped}"]`)?.scrollIntoView?.({ block: "nearest" });
  }, [activeClamped, open]);

  // ── Styles (read `c.*` at render — theme-safe per JC25) ───────────────────────
  const controlStyle: CSSProperties = {
    display: "flex",
    alignItems: "center",
    gap: 6,
    cursor: disabled ? "not-allowed" : "text",
    opacity: disabled ? 0.6 : 1,
    ...style,
    ...(focused ? focusRing() : null),
  };
  const panelStyle: CSSProperties = {
    position: "absolute",
    top: "calc(100% + 4px)",
    left: 0,
    right: 0,
    zIndex: 50,
    background: c.panel,
    border: `1px solid ${c.border}`,
    borderRadius: c.radiusSurface,
    // An overlay keeps its shadow alongside the border (the VU-5 exception): it
    // floats over arbitrary content, so the shadow is separation work, not decor.
    boxShadow: c.shadow,
    maxHeight: 320,
    overflowY: "auto",
    padding: 4,
  };
  const headerStyle: CSSProperties = {
    padding: "8px 10px 4px",
    fontSize: c.fontXs,
    fontFamily: c.sansCond,
    fontWeight: 700,
    textTransform: "uppercase",
    letterSpacing: 0.7,
    color: c.textMuted,
  };
  const optionStyle = (isActive: boolean, isSelected: boolean): CSSProperties => ({
    display: "flex",
    alignItems: "center",
    gap: 8,
    minHeight: 40,
    padding: "8px 10px",
    borderRadius: c.radiusChip,
    cursor: "pointer",
    fontSize: c.fontSm,
    color: c.text,
    background: isActive ? c.primaryBg : isSelected ? c.panelHover : "transparent",
  });
  const missingChip: CSSProperties = {
    flex: "none",
    fontSize: c.fontXs,
    fontWeight: 700,
    textTransform: "uppercase",
    letterSpacing: 0.3,
    color: c.danger,
    background: c.dangerBg,
    borderRadius: c.radiusChip,
    padding: "2px 6px",
  };

  return (
    <div style={{ position: "relative", width: "100%" }}>
      <div
        style={controlStyle}
        onClick={() => {
          if (disabled) return;
          setOpen(true);
          inputRef.current?.focus();
        }}
      >
        {!open && missing && <span style={missingChip}>missing</span>}
        {!open && !missing && selectedItem && renderAdornment && <span style={{ flex: "none", display: "inline-flex" }}>{renderAdornment(selectedItem)}</span>}
        <input
          ref={inputRef}
          role="combobox"
          aria-label={ariaLabel}
          aria-expanded={open}
          aria-controls={`${baseId}-listbox`}
          aria-activedescendant={open && nav[activeClamped] ? optId(activeClamped) : undefined}
          aria-autocomplete="list"
          autoComplete="off"
          spellCheck={false}
          disabled={disabled}
          value={open ? query : closedText}
          placeholder={open ? closedText || placeholder || "" : placeholder || ""}
          onChange={(e) => {
            setQuery(e.target.value);
            setOpen(true);
            setActive(0);
            onSearch?.(e.target.value);
          }}
          onFocus={() => {
            setFocused(true);
            setOpen(true);
            setQuery("");
            onSearch?.(""); // open shows the default (unfiltered) list, not the last server query
          }}
          onBlur={() => {
            // Option/panel clicks preventDefault their mousedown (below), so they
            // never blur the input — a blur here is a genuine focus-out → close.
            setFocused(false);
            close();
          }}
          onKeyDown={onKeyDown}
          style={{
            flex: 1,
            minWidth: 0,
            border: "none",
            outline: "none",
            background: "transparent",
            color: missing && !open ? c.danger : c.text,
            fontSize: c.fontSm,
            fontFamily: c.sans,
            padding: 0,
          }}
        />
        {/* preventDefault keeps focus on the input so a chevron click while open
            doesn't blur→close→reopen (a one-frame flicker). */}
        <span style={{ display: "inline-flex", flex: "none" }} onMouseDown={(e) => e.preventDefault()}>
          <Chevron open={open} />
        </span>
      </div>

      {open && (
        <div
          ref={listRef}
          id={`${baseId}-listbox`}
          role="listbox"
          style={panelStyle}
          // Keep focus on the input during clicks inside the panel so onBlur (which
          // closes) doesn't fire before an option's onClick runs.
          onMouseDown={(e) => e.preventDefault()}
        >
          {loading && nav.length === 0 ? (
            <InlineLoading style={{ padding: 10 }} />
          ) : nav.length === 0 ? (
            <div style={{ padding: "10px", fontSize: c.fontSm, color: c.textMuted }}>{emptyNote ?? "No matches."}</div>
          ) : (
            rows.map((r, i) => {
              if (r.kind === "header") {
                return (
                  <div key={`h-${i}`} role="presentation" style={headerStyle}>
                    {groupLabel ? groupLabel(r.key) : r.key || "/"}
                  </div>
                );
              }
              if (r.kind === "create") {
                const isActive = r.nav === activeClamped;
                return (
                  <div
                    key={`create-${r.nav}`}
                    id={optId(r.nav)}
                    data-nav={r.nav}
                    role="option"
                    aria-selected={false}
                    onMouseEnter={() => setActive(r.nav)}
                    onClick={() => commitCreate(r.value)}
                    style={optionStyle(isActive, false)}
                  >
                    {onCreateLabel ? onCreateLabel(r.value) : `Use "${r.value}"`}
                  </div>
                );
              }
              const key = getKey(r.item);
              const isSelected = key === value;
              const isActive = r.nav === activeClamped;
              return (
                <div
                  key={key || `opt-${r.nav}`}
                  id={optId(r.nav)}
                  data-nav={r.nav}
                  role="option"
                  aria-selected={isSelected}
                  onMouseEnter={() => setActive(r.nav)}
                  onClick={() => selectOption(r.item)}
                  style={optionStyle(isActive, isSelected)}
                >
                  {renderOption(r.item)}
                </div>
              );
            })
          )}
        </div>
      )}
    </div>
  );
}

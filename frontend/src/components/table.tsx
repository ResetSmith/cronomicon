import { useEffect, useRef, useState, type CSSProperties, type ReactNode } from "react";
import { Btn, ResizableTh, SortableLabel, tdStyle as defaultTdStyle, thStyle as defaultThStyle } from "./ui";
import { c } from "../theme";

// ── The column model (CO-2, the columns plan) ──────────────────────
//
// Before this, a table's columns did not exist as a THING. Headers were a
// hand-listed sequence of `headCell(...)` calls and body cells were a separate
// hand-written sequence of <td>s, matched to each other only by position, with
// the column count typed out a third time in every colSpan. Nothing could be
// permuted, hidden or counted, because nothing was addressable — and the three
// hand-maintained copies of "how many columns are there" had already drifted
// apart into the bugs CO-2.0 catalogued.
//
// A TableColumn ties a stable key to the cell that renders it. Once a table's
// columns are a keyed array, reordering is a permutation, hiding is a filter,
// and every count is `visible.length`.

export type TableColumn<T> = {
  /** Stable identity — the localStorage key AND the React key. Never a label. */
  key: string;
  /** Header text. "" for an unlabeled actions/expand column. */
  label: string;
  /**
   * What the Columns menu calls this column. Required in practice for an
   * unlabeled column: a menu row reading "" names nothing, and "Always last"
   * beside a blank is not an explanation.
   */
  menuLabel?: string;
  /** Present ⇒ the header is a sort toggle, wired to this useTableSort key. */
  sortKey?: string;
  /** Default width in px; a stored `colw:` override outranks it. */
  width?: number;
  /**
   * No resize handle — a plain <th>. The V1.1-7 Q5/Q6 hybrid policy: content
   * columns resize, narrow/control columns do not.
   */
  fixed?: boolean;
  /**
   * CO-Q4 — neither movable nor hideable. The column declared FIRST in a table's
   * default order carries "first" (it is not always the name: six adopters open
   * with a 44px expand/toggle control), and a trailing unlabeled actions cell
   * carries "last". Actions in column 3 is not a preference, it is a mistake the
   * UI should not offer.
   */
  pin?: "first" | "last";
  /** The <td> body. */
  cell: (row: T) => ReactNode;
  /** Per-column cell style — mono, nowrap, right-align. */
  tdStyle?: CSSProperties;
};

/**
 * BUILD THE SPEC ARRAY DURING RENDER, NEVER AT MODULE SCOPE.
 *
 * A spec carries `c.*` theme tokens in `tdStyle`, and `c` is a MUTABLE object
 * reassigned by `applyTheme`. A module-level const captures whichever palette
 * was loaded first and then silently stops responding to the Light/Dark toggle —
 * it will pass every test and break the toggle. This is the exact trap recorded
 * in [[theme-token-module-const-staleness]], and it is why the existing
 * module-level COL_W / SORT_COLS consts are safe: they hold no colours.
 *
 * Build specs inside the component, or in a factory taking whatever the cells
 * close over (inlineTags / inlineAnnotation resolvers, canRun, expanded, …).
 */

// ── The stored preference ────────────────────────────────────────────────────
// `cols:<tableId>` → {order, hidden}. A SECOND key beside useColumnWidths'
// `colw:<tableId>`, deliberately: order/visibility and width are independent
// preferences, and keeping them apart means this band needs no migration and
// nobody loses a width they have dragged.
type StoredCols = { order?: string[]; hidden?: string[] };

const storageKey = (tableId: string) => `cols:${tableId}`;

const readStored = (tableId: string): StoredCols => {
  try {
    const raw = localStorage.getItem(storageKey(tableId));
    if (!raw) return {};
    const parsed = JSON.parse(raw) as StoredCols;
    // A hand-edited or half-written value must not take the table down.
    return {
      order: Array.isArray(parsed?.order) ? parsed.order.filter((k) => typeof k === "string") : undefined,
      hidden: Array.isArray(parsed?.hidden) ? parsed.hidden.filter((k) => typeof k === "string") : undefined,
    };
  } catch {
    return {};
  }
};

const writeStored = (tableId: string, v: StoredCols) => {
  try {
    localStorage.setItem(storageKey(tableId), JSON.stringify(v));
  } catch {
    /* private browsing — keep in-memory only, exactly as useColumnWidths does */
  }
};

/**
 * CO-Q10 — the forward-compatibility merge. THE load-bearing rule of this band.
 *
 * A stored order is a PARTIAL order over a column set that will change in later
 * releases. The rule: stored keys first (dropping any that no longer name a
 * column), then every default-order key not present in the stored list,
 * re-inserted AT ITS DEFAULT INDEX.
 *
 * Without the second half, the next column anyone adds is invisible to every
 * existing user — and because it only affects people who happen to have a stored
 * order, it fails silently and arrives months later as "the new column never
 * shipped". Everything else in this band fails loudly in review; this one does
 * not, which is why it has its own test (CO-2.4).
 */
export function mergeOrder(defaultKeys: string[], storedOrder: string[] | undefined): string[] {
  if (!storedOrder?.length) return [...defaultKeys];
  const known = new Set(defaultKeys);
  // Stored keys that still name a column, de-duplicated (a corrupt value could
  // repeat one, which would render the same column twice).
  const seen = new Set<string>();
  const merged = storedOrder.filter((k) => known.has(k) && !seen.has(k) && seen.add(k));
  // Then re-insert anything the stored order never knew about, at its default
  // index — so a column added in a later release lands where its author put it
  // rather than being appended to the end or dropped.
  defaultKeys.forEach((k, defaultIndex) => {
    if (!seen.has(k)) merged.splice(Math.min(defaultIndex, merged.length), 0, k);
  });
  return merged;
}

/**
 * Pins are positional law, not preference (CO-Q4). They are re-applied AFTER the
 * merge rather than trusted from storage: a stored order written before a column
 * was pinned would otherwise keep it loose forever.
 */
function applyPins<T>(order: string[], byKey: Map<string, TableColumn<T>>): string[] {
  const first = order.filter((k) => byKey.get(k)?.pin === "first");
  const last = order.filter((k) => byKey.get(k)?.pin === "last");
  const middle = order.filter((k) => !byKey.get(k)?.pin);
  return [...first, ...middle, ...last];
}

export type TableColumnsApi<T> = {
  /** Ordered, unhidden — the single answer to "how many columns are there". */
  visible: TableColumn<T>[];
  /** Ordered, INCLUDING hidden — what the Columns menu lists (CO-3). */
  all: TableColumn<T>[];
  hidden: Set<string>;
  isHidden: (key: string) => boolean;
  /** One step, for the menu's ▲/▼ buttons. */
  move: (key: string, dir: -1 | 1) => void;
  /**
   * Whether that move would actually do anything. The menu's ▲/▼ ask THIS
   * rather than re-deriving it from list position: a move is refused at the
   * ends of the list *and* into a pin, and a menu that only knew the first rule
   * rendered a live arrow on the topmost loose column of every table with a
   * pinned opener — which is most of them — that silently did nothing when
   * clicked. The house rule (FX-7) is that a precondition disables the control,
   * so the precondition and the affordance have to come from one place.
   */
  canMove: (key: string, dir: -1 | 1) => boolean;
  /**
   * To an absolute index in `all`, for a drag. Not expressible as repeated
   * move() calls: those each read `order`, which is derived from state that has
   * not committed yet within a single tick, so the second call would compute
   * from a stale order.
   */
  moveTo: (key: string, index: number) => void;
  setHidden: (key: string, hidden: boolean) => void;
  /** Clears `cols:` — the caller clears `colw:` alongside it (CO-3). */
  reset: () => void;
  /** Sum of the VISIBLE widths. Recomputed per render — see the note below. */
  minWidth: number;
};

/**
 * useTableColumns — the order/visibility half of a table's preferences, beside
 * useColumnWidths' widths and useTableSort's sort. Same three properties as both:
 * localStorage-backed, private-browsing tolerant (a failed write just means the
 * choice is in-memory), and a stored key that no longer names a column is
 * ignored rather than rendered as a ghost.
 *
 * `columns` is the DEFAULT spec, rebuilt every render (see the theme note
 * above) — so this hook must never memoize on its identity.
 */
export function useTableColumns<T>(
  tableId: string,
  columns: TableColumn<T>[],
  // Test seam: the initial stored value. Production always reads localStorage.
  initial?: StoredCols,
): TableColumnsApi<T> {
  const [stored, setStored] = useState<StoredCols>(() => initial ?? readStored(tableId));

  const byKey = new Map(columns.map((col) => [col.key, col]));
  const defaultKeys = columns.map((col) => col.key);
  const order = applyPins(mergeOrder(defaultKeys, stored.order), byKey);

  // A pinned column can never be hidden, whatever storage says (CO-Q4) — so a
  // pin added after someone hid that column un-hides it rather than leaving an
  // unreachable preference no menu row can clear.
  const hidden = new Set((stored.hidden ?? []).filter((k) => byKey.has(k) && !byKey.get(k)!.pin));

  const all = order.map((k) => byKey.get(k)!).filter(Boolean);
  const visible = all.filter((col) => !hidden.has(col.key));

  const commit = (next: StoredCols) => {
    setStored(next);
    writeStored(tableId, next);
  };

  const canMove = (key: string, dir: -1 | 1): boolean => {
    const col = byKey.get(key);
    if (!col || col.pin) return false; // CO-Q4 — a pin refuses to move
    const from = order.indexOf(key);
    const to = from + dir;
    if (from < 0 || to < 0 || to >= order.length) return false;
    // Refuse to move PAST a pin as well as to move a pin: swapping with a
    // pinned neighbour would push it out of first/last position.
    return !byKey.get(order[to])?.pin;
  };

  // Delegates rather than restating the guards, so the arrows the menu enables
  // and the moves this accepts cannot drift apart.
  const move = (key: string, dir: -1 | 1) => {
    if (!canMove(key, dir)) return;
    const from = order.indexOf(key);
    const to = from + dir;
    const next = [...order];
    [next[from], next[to]] = [next[to], next[from]];
    commit({ ...stored, order: next });
  };

  // The half-open range a loose column may land in: after the leading pins,
  // before the trailing ones. A drag that overshoots clamps into this band
  // rather than being refused — a drop that silently does nothing reads as a
  // broken drag, where a clamped one reads as a boundary.
  const looseBounds = () => {
    let lo = 0;
    while (lo < order.length && byKey.get(order[lo])?.pin === "first") lo++;
    let hi = order.length - 1;
    while (hi >= 0 && byKey.get(order[hi])?.pin === "last") hi--;
    return { lo, hi };
  };

  const moveTo = (key: string, index: number) => {
    const col = byKey.get(key);
    if (!col || col.pin) return;
    const from = order.indexOf(key);
    if (from < 0) return;
    const { lo, hi } = looseBounds();
    const to = Math.max(lo, Math.min(hi, index));
    if (to === from) return;
    const next = [...order];
    next.splice(from, 1);
    next.splice(to, 0, key);
    commit({ ...stored, order: next });
  };

  const setHidden = (key: string, hide: boolean) => {
    const col = byKey.get(key);
    if (!col || col.pin) return; // CO-Q4 — a pin refuses to hide
    const next = new Set(hidden);
    if (hide) next.add(key);
    else next.delete(key);
    commit({ ...stored, hidden: [...next] });
  };

  const reset = () => {
    setStored({});
    try {
      localStorage.removeItem(storageKey(tableId));
    } catch {
      /* ignore */
    }
  };

  return {
    visible,
    all,
    hidden,
    isHidden: (key: string) => hidden.has(key),
    move,
    canMove,
    moveTo,
    setHidden,
    reset,
    minWidth: minWidthOf(visible),
  };
}

/**
 * minWidthOf — the sum the table refuses to compress below (LB14), replacing the
 * hand-maintained literal that CO-2.0 #2 found already stale by a column.
 *
 * Pass the VISIBLE columns and recompute per render: summing `all` would leave a
 * hidden column's width reserved, and the table would refuse to compress into
 * the space hiding it was supposed to free.
 */
export function minWidthOf<T>(columns: TableColumn<T>[]): number {
  return columns.reduce((sum, col) => sum + (col.width ?? 0), 0);
}

// ── Renderers ────────────────────────────────────────────────────────────────

/** The sort surface TableHead needs — structurally what useTableSort returns. */
export type TableSortApi = {
  sortDir: "asc" | "desc";
  toggle: (key: string) => void;
  isActive: (key: string) => boolean;
  ariaSort: (key: string) => "ascending" | "descending" | undefined;
};

/** The width surface TableHead needs — structurally what useColumnWidths returns. */
export type ColumnWidthsApi = {
  widths: Record<string, number>;
  setWidth: (col: string, w: number) => void;
  /** Present on the real hook; ColumnsMenu's Reset clears widths alongside order. */
  reset?: () => void;
};

/**
 * TableHead — the ONE table-header renderer. This replaces the headCell /
 * fixedCell / tableHeader trio that every adopting view had grown its own drifted
 * copy of; the "one component per vocabulary" rule in components/ui.tsx is the
 * whole point of the exercise.
 *
 * `sortEnabled` gates the sort affordance rather than the columns' sortKeys —
 * a caller that renders this header over rows it does not control can pass
 * false for the same header with no carets and no click handlers. The four
 * folder-tree catalogs USED to do that in browse mode; since TS-24 they sort
 * there too (they feed FolderBrowser their already-sorted rows and set
 * preserveLeafOrder), so nothing in the app passes false today.
 */
export function TableHead<T>({
  columns,
  sort,
  cw,
  sortEnabled = true,
  thStyle,
  trStyle,
}: {
  columns: TableColumn<T>[];
  sort: TableSortApi;
  cw: ColumnWidthsApi;
  sortEnabled?: boolean;
  /** Per-table header style base. Defaults to the shared ui.tsx thStyle(). */
  thStyle?: () => CSSProperties;
  trStyle?: CSSProperties;
}) {
  const base = thStyle ?? defaultThStyle;
  return (
    <tr style={trStyle}>
      {columns.map((col) => {
        const sortKey = sortEnabled ? col.sortKey : undefined;
        const active = sortKey !== undefined && sort.isActive(sortKey);
        const shared: CSSProperties = {
          ...base(),
          cursor: sortKey ? "pointer" : undefined,
          // The active sort column reads at full text colour; the other sortable
          // heads stay secondary, so the sorted column is findable without
          // having to read carets. Read from `c` HERE, during render — see the
          // theme note at the top of this file.
          color: sortKey ? (active ? c.text : c.textSec) : undefined,
        };
        const label = <SortableLabel label={col.label} active={active} dir={sort.sortDir} />;

        // A fixed column is a plain <th>: no drag handle, explicit width (V1.1-7
        // Q5/Q6). A stored width still wins if one was somehow recorded.
        if (col.fixed) {
          return (
            <th
              key={col.key}
              onClick={sortKey ? () => sort.toggle(sortKey) : undefined}
              title={sortKey ? `Sort by ${col.label}` : undefined}
              aria-sort={sortKey ? sort.ariaSort(sortKey) : undefined}
              style={{ ...shared, width: cw.widths[col.key] ?? col.width, overflow: "hidden", textOverflow: "ellipsis" }}
            >
              {label}
            </th>
          );
        }
        return (
          <ResizableTh
            key={col.key}
            width={cw.widths[col.key] ?? col.width}
            onResize={(w) => cw.setWidth(col.key, w)}
            onClick={sortKey ? () => sort.toggle(sortKey) : undefined}
            title={sortKey ? `Sort by ${col.label}` : undefined}
            ariaSort={sortKey ? sort.ariaSort(sortKey) : undefined}
            style={shared}
          >
            {label}
          </ResizableTh>
        );
      })}
    </tr>
  );
}

/**
 * renderCells — the row body, mapping the VISIBLE columns to <td>s. A hidden
 * column leaves NO <td> in the DOM at all (CO-Q9): `display: none` would keep
 * three different answers to "how many columns are there" — the rendered count,
 * the colSpan arithmetic, and what a screen reader announces — and disagreement
 * between exactly those three is how CO-2.0's bugs got in.
 *
 * `rowStyle` exists for one shipped pattern: Jobs and Workflows suppress every
 * cell's borderBottom while a row is expanded, so the detail panel reads as part
 * of the row rather than a separate band. Without it that override would have to
 * be smeared across ten column specs.
 */
export function renderCells<T>(
  columns: TableColumn<T>[],
  row: T,
  opts?: { rowStyle?: CSSProperties; base?: CSSProperties },
): ReactNode[] {
  const base = opts?.base ?? defaultTdStyle();
  return columns.map((col) => (
    <td key={col.key} style={{ ...base, ...col.tdStyle, ...opts?.rowStyle }}>
      {col.cell(row)}
    </td>
  ));
}

// ── ColumnsMenu (CO-3) ───────────────────────────────────────────────────────
//
// WHY A MENU AND NOT A HEADER DRAG (CO-Q5). A <th> in this app already answers
// TWO gestures: click-to-sort, and a right-edge resize handle that must
// stopPropagation on both mousedown and click precisely so that dragging never
// sorts. Adding drag-to-reorder would make one 30px-tall target answer three,
// one of which — horizontal drag — is already claimed by resize. Telling them
// apart needs a movement threshold and a hit-test, and the failure mode is a
// sort firing when somebody meant to drag. The menu also has to exist either
// way, because show/hide has nowhere else to live: a hidden column has no
// header left to drag back.
//
// So reordering happens by dragging WITHIN this vertical list — an unambiguous
// surface with no competing gesture — with ▲/▼ on every row as the keyboard and
// assistive-technology path.

/** Fixed row height. The drag maths is `round(dy / ROW_H)`, so this is load-bearing. */
const MENU_ROW_H = 34;

export function ColumnsMenu<T>({ cols, cw, label = "Columns" }: { cols: TableColumnsApi<T>; cw?: ColumnWidthsApi; label?: string }) {
  const [open, setOpen] = useState(false);
  const [dragKey, setDragKey] = useState<string | null>(null);
  const [dragDy, setDragDy] = useState(0);
  // Announced to screen readers on every order/visibility change: a reorder is
  // otherwise a purely visual event, and the ▲/▼ button that caused it has
  // already moved out from under the user's focus.
  const [announce, setAnnounce] = useState("");
  const ref = useRef<HTMLDivElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLDivElement>(null);

  // Click-outside + Esc, the shipped TagFilterSelect dismissal pattern — no new
  // overlay vocabulary. Esc additionally returns focus to the trigger, or the
  // keyboard user is stranded at the top of the document.
  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        setOpen(false);
        triggerRef.current?.querySelector("button")?.focus();
        return;
      }
      if (e.key !== "Tab" || !panelRef.current) return;
      // Focus trap: the list is long and its controls repeat, so tabbing out of
      // it mid-reorder loses the user's place entirely.
      const focusables = panelRef.current.querySelectorAll<HTMLElement>(
        'button:not([disabled]), input:not([disabled]), [tabindex]:not([tabindex="-1"])',
      );
      if (!focusables.length) return;
      const first = focusables[0];
      const last = focusables[focusables.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey, true);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey, true);
    };
  }, [open]);

  const startDrag = (key: string, e: React.PointerEvent) => {
    e.preventDefault();
    e.stopPropagation();
    const startY = e.clientY;
    const from = cols.all.findIndex((col) => col.key === key);
    setDragKey(key);
    setDragDy(0);
    const move = (ev: PointerEvent) => setDragDy(ev.clientY - startY);
    const up = (ev: PointerEvent) => {
      document.removeEventListener("pointermove", move);
      document.removeEventListener("pointerup", up);
      setDragKey(null);
      setDragDy(0);
      const steps = Math.round((ev.clientY - startY) / MENU_ROW_H);
      if (steps !== 0) {
        cols.moveTo(key, from + steps);
        setAnnounce(`${menuName(cols.all[from])} moved`);
      }
    };
    document.addEventListener("pointermove", move);
    document.addEventListener("pointerup", up);
  };

  const hiddenCount = cols.all.length - cols.visible.length;

  return (
    <div ref={ref} style={{ position: "relative", display: "inline-flex" }}>
      <div ref={triggerRef}>
        <Btn
          small
          onClick={() => setOpen((o) => !o)}
          ariaExpanded={open}
          // A preference silently in force on a shared workstation is
          // indistinguishable from missing data, so the count is on the trigger
          // rather than inside the popover nobody has opened.
          title={hiddenCount ? `${hiddenCount} column${hiddenCount === 1 ? "" : "s"} hidden` : "Choose and arrange columns"}
        >
          {hiddenCount ? `${label} · ${hiddenCount} hidden` : label}
        </Btn>
      </div>
      {open && (
        <div
          ref={panelRef}
          role="dialog"
          aria-label="Choose and arrange columns"
          style={{
            position: "absolute",
            top: "100%",
            right: 0,
            zIndex: 30,
            marginTop: 4,
            minWidth: 260,
            maxHeight: 420,
            // The LIST scrolls; the Reset row below it does not. Same lesson as
            // Modal's `footer` (FX-6): an action inside the scrollport is an
            // action people cannot find, and here it renders clipped by the
            // panel edge at the exact list length a real catalog has.
            display: "flex",
            flexDirection: "column",
            background: c.panel,
            // A floating overlay is the documented exception to "a surface gets
            // a border OR a shadow, never both" — the shadow separates it from
            // whatever arbitrary content sits beneath.
            border: `1px solid ${c.border}`,
            borderRadius: c.radiusSurface,
            boxShadow: c.shadow,
            padding: 8,
          }}
        >
          <div aria-live="polite" style={{ position: "absolute", width: 1, height: 1, overflow: "hidden", clip: "rect(0 0 0 0)" }}>
            {announce}
          </div>
          <div style={{ overflowY: "auto", minHeight: 0 }}>
          {cols.all.map((col) => {
            const pinned = !!col.pin;
            const isHidden = cols.isHidden(col.key);
            const dragging = dragKey === col.key;
            return (
              <div
                key={col.key}
                style={{
                  display: "flex",
                  alignItems: "center",
                  gap: 8,
                  height: MENU_ROW_H,
                  padding: "0 4px",
                  borderRadius: c.radiusChip,
                  opacity: pinned ? 0.6 : 1,
                  background: dragging ? c.panelHover : undefined,
                  transform: dragging ? `translateY(${dragDy}px)` : undefined,
                  position: dragging ? "relative" : undefined,
                  zIndex: dragging ? 1 : undefined,
                }}
              >
                <span
                  aria-hidden
                  onPointerDown={pinned ? undefined : (e) => startDrag(col.key, e)}
                  title={pinned ? undefined : "Drag to reorder"}
                  style={{ cursor: pinned ? "default" : "grab", color: c.textMuted, width: 12, userSelect: "none" }}
                >
                  {pinned ? "" : "⣿"}
                </span>
                <input
                  type="checkbox"
                  checked={!isHidden}
                  disabled={pinned}
                  aria-label={`Show ${menuName(col)}`}
                  onChange={() => {
                    cols.setHidden(col.key, !isHidden);
                    setAnnounce(`${menuName(col)} ${isHidden ? "shown" : "hidden"}`);
                  }}
                />
                <span style={{ flex: 1, fontSize: c.fontSm, color: pinned ? c.textMuted : c.text, whiteSpace: "nowrap" }}>
                  {menuName(col)}
                </span>
                {pinned ? (
                  // Shown greyed with the reason rather than omitted, so the pin
                  // is visible rather than mysterious (CO-Q4).
                  <span style={{ fontSize: c.fontXs, color: c.textMuted, fontFamily: c.sansCond, textTransform: "uppercase", letterSpacing: 0.7 }}>
                    {col.pin === "first" ? "Always first" : "Always last"}
                  </span>
                ) : (
                  <>
                    <MenuArrow label={`Move ${menuName(col)} up`} disabled={!cols.canMove(col.key, -1)} onClick={() => { cols.move(col.key, -1); setAnnounce(`${menuName(col)} moved up`); }}>
                      ▲
                    </MenuArrow>
                    <MenuArrow label={`Move ${menuName(col)} down`} disabled={!cols.canMove(col.key, 1)} onClick={() => { cols.move(col.key, 1); setAnnounce(`${menuName(col)} moved down`); }}>
                      ▼
                    </MenuArrow>
                  </>
                )}
              </div>
            );
          })}
          </div>
          <div style={{ borderTop: `1px solid ${c.border}`, marginTop: 6, paddingTop: 6, flexShrink: 0 }}>
            <Btn
              small
              onClick={() => {
                // Order AND widths together: "put the table back" is one mental
                // model, and two buttons for it is a worse answer than one.
                cols.reset();
                cw?.reset?.();
                setAnnounce("Columns reset to defaults");
              }}
            >
              Reset to defaults
            </Btn>
          </div>
        </div>
      )}
    </div>
  );
}

/** The name a column answers to in the menu — never the empty header string. */
function menuName<T>(col: TableColumn<T>): string {
  return col.menuLabel || col.label || col.key;
}

function MenuArrow({ label, disabled, onClick, children }: { label: string; disabled?: boolean; onClick: () => void; children: ReactNode }) {
  return (
    <button
      type="button"
      aria-label={label}
      title={label}
      disabled={disabled}
      onClick={onClick}
      style={{
        border: `1px solid ${c.border}`,
        background: "transparent",
        color: disabled ? c.textMuted : c.textSec,
        borderRadius: c.radiusChip,
        width: 22,
        height: 22,
        lineHeight: 1,
        fontSize: c.fontXs,
        cursor: disabled ? "default" : "pointer",
        opacity: disabled ? 0.4 : 1,
      }}
    >
      {children}
    </button>
  );
}

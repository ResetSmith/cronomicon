// Pure table-sort comparators (TS-1, the sorting-update plan). No React / DOM /
// theme imports so the module is unit-testable in node — pair with the
// `useTableSort` hook (hooks.ts) for state, and `SortableLabel` (components/ui)
// for the header affordance.

export type SortDir = "asc" | "desc";
export type SortType = "text" | "number" | "date" | "rank";

// One sortable column: `get` extracts the raw value from a row; `type` picks the
// comparator ("text" default). "rank" orders by the supplied map (TS-Q5: rank
// maps list worst first, so ascending floats problems to the top); values absent
// from the map sort after every ranked value. `defaultDir` is the direction the
// FIRST click applies — unset, it falls back by type: date/number → "desc"
// (newest/largest first), text/rank → "asc".
export interface SortColumn<T> {
  key: string;
  get: (row: T) => unknown;
  type?: SortType;
  rank?: Record<string, number>;
  defaultDir?: SortDir;
}

export const defaultDirFor = <T>(col: SortColumn<T>): SortDir =>
  col.defaultDir ?? (col.type === "date" || col.type === "number" ? "desc" : "asc");

// Empty cells (null/undefined/"") sort LAST regardless of direction — a job
// that never ran must not float to the top of "Last Run ▲".
const isEmpty = (v: unknown): boolean => v === null || v === undefined || v === "";

// Ascending compare of two non-empty raw values by type. Text is locale- and
// numeric-aware ("host2" < "host10", case/diacritic-insensitive). Dates accept
// ISO strings or epoch numbers; an unparseable pair falls back to text so the
// order is still deterministic.
function compareRaw(a: unknown, b: unknown, type: SortType, rank?: Record<string, number>): number {
  switch (type) {
    case "number": {
      const na = Number(a);
      const nb = Number(b);
      if (Number.isNaN(na) || Number.isNaN(nb)) return Number.isNaN(na) ? (Number.isNaN(nb) ? 0 : 1) : -1;
      return na - nb;
    }
    case "date": {
      const ta = Date.parse(String(a));
      const tb = Date.parse(String(b));
      if (Number.isNaN(ta) || Number.isNaN(tb)) return compareRaw(a, b, "text");
      return ta - tb;
    }
    case "rank": {
      const ra = rank?.[String(a)] ?? Number.POSITIVE_INFINITY;
      const rb = rank?.[String(b)] ?? Number.POSITIVE_INFINITY;
      if (ra !== rb) return ra < rb ? -1 : 1;
      return compareRaw(a, b, "text"); // equal/unranked values: deterministic text order
    }
    default:
      return String(a).localeCompare(String(b), undefined, { sensitivity: "base", numeric: true });
  }
}

// Row comparator for one column + direction. Empties compare last in BOTH
// directions (checked before the direction flip).
export function makeComparator<T>(col: SortColumn<T>, dir: SortDir): (a: T, b: T) => number {
  return (ra, rb) => {
    const a = col.get(ra);
    const b = col.get(rb);
    const ae = isEmpty(a);
    const be = isEmpty(b);
    if (ae || be) return ae && be ? 0 : ae ? 1 : -1;
    const r = compareRaw(a, b, col.type ?? "text", col.rank);
    return dir === "desc" ? -r : r;
  };
}

// First-non-zero chain, for tiebreaks.
export function chainComparators<T>(...cmps: Array<(a: T, b: T) => number>): (a: T, b: T) => number {
  return (a, b) => {
    for (const cmp of cmps) {
      const r = cmp(a, b);
      if (r !== 0) return r;
    }
    return 0;
  };
}

// Sorted copy of `rows` by the active column + direction, then the tiebreak
// chain `{ key, dir? }[]` (dir defaults to each tiebreak column's defaultDir).
// The active column is skipped inside the chain so it never double-applies.
// Array.prototype.sort is stable, so equal-through-the-chain rows keep their
// incoming (server) order.
export function sortRows<T>(
  rows: T[],
  columns: SortColumn<T>[],
  active: { key: string; dir: SortDir } | null,
  tiebreak: Array<{ key: string; dir?: SortDir }> = [],
): T[] {
  if (!active) return rows;
  const byKey = new Map(columns.map((col) => [col.key, col]));
  const primaryCol = byKey.get(active.key);
  if (!primaryCol) return rows;
  const cmps = [makeComparator(primaryCol, active.dir)];
  for (const tb of tiebreak) {
    if (tb.key === active.key) continue;
    const col = byKey.get(tb.key);
    if (col) cmps.push(makeComparator(col, tb.dir ?? defaultDirFor(col)));
  }
  return [...rows].sort(chainComparators(...cmps));
}

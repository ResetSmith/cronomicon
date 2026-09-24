import { describe, it, expect } from "vitest";
import { defaultDirFor, makeComparator, chainComparators, sortRows, type SortColumn } from "./sort";

// Pure comparator layer behind useTableSort (TS-1/TS-7, the sorting-update plan).
// The hook's toggle/persistence needs a DOM and is covered in hooks.sort.test.tsx;
// this covers the ordering rules every table delegates here.

interface Row {
  name?: string | null;
  count?: number | null;
  at?: string | null;
  status?: string | null;
}

const byName: SortColumn<Row> = { key: "name", get: (r) => r.name, type: "text" };
const byCount: SortColumn<Row> = { key: "count", get: (r) => r.count, type: "number" };
const byAt: SortColumn<Row> = { key: "at", get: (r) => r.at, type: "date" };
const RANK: Record<string, number> = { failure: 0, warning: 1, success: 2 };
const byStatus: SortColumn<Row> = { key: "status", get: (r) => r.status, type: "rank", rank: RANK };

const names = (rows: Row[]) => rows.map((r) => r.name);

describe("defaultDirFor", () => {
  it("opens text and rank columns ascending, date and number descending", () => {
    expect(defaultDirFor(byName)).toBe("asc");
    expect(defaultDirFor(byStatus)).toBe("asc");
    expect(defaultDirFor(byAt)).toBe("desc");
    expect(defaultDirFor(byCount)).toBe("desc");
  });

  it("lets an explicit defaultDir win", () => {
    expect(defaultDirFor({ ...byAt, defaultDir: "asc" })).toBe("asc");
  });
});

describe("makeComparator — text", () => {
  it("is numeric-aware, so host2 sorts before host10", () => {
    const rows: Row[] = [{ name: "host10" }, { name: "host2" }, { name: "host1" }];
    expect(names(rows.sort(makeComparator(byName, "asc")))).toEqual(["host1", "host2", "host10"]);
  });

  it("ignores case", () => {
    const rows: Row[] = [{ name: "bravo" }, { name: "Alpha" }];
    expect(names(rows.sort(makeComparator(byName, "asc")))).toEqual(["Alpha", "bravo"]);
  });

  it("keeps empties last in BOTH directions", () => {
    const rows = (): Row[] => [{ name: null }, { name: "b" }, { name: undefined }, { name: "a" }, { name: "" }];
    expect(names(rows().sort(makeComparator(byName, "asc")))).toEqual(["a", "b", null, undefined, ""]);
    expect(names(rows().sort(makeComparator(byName, "desc")))).toEqual(["b", "a", null, undefined, ""]);
  });
});

describe("makeComparator — number", () => {
  it("sorts numerically with empties last", () => {
    const rows: Row[] = [{ name: "n", count: null }, { name: "b", count: 200 }, { name: "a", count: 30 }];
    expect(names(rows.sort(makeComparator(byCount, "asc")))).toEqual(["a", "b", "n"]);
  });

  it("desc still keeps the empty row last, not first", () => {
    const rows: Row[] = [{ name: "n", count: undefined }, { name: "a", count: 30 }, { name: "b", count: 200 }];
    expect(names(rows.sort(makeComparator(byCount, "desc")))).toEqual(["b", "a", "n"]);
  });
});

describe("makeComparator — date", () => {
  it("orders ISO timestamps chronologically", () => {
    const rows: Row[] = [
      { name: "mid", at: "2026-07-31T12:00:00Z" },
      { name: "new", at: "2026-08-01T09:00:00Z" },
      { name: "old", at: "2026-01-05T00:00:00Z" },
    ];
    expect(names(rows.sort(makeComparator(byAt, "desc")))).toEqual(["new", "mid", "old"]);
  });

  it("never-ran rows (empty date) trail even in desc", () => {
    const rows: Row[] = [{ name: "never", at: null }, { name: "ran", at: "2026-08-01T09:00:00Z" }];
    expect(names(rows.sort(makeComparator(byAt, "desc")))).toEqual(["ran", "never"]);
  });
});

describe("makeComparator — rank", () => {
  it("orders by the rank map, worst first ascending (TS-Q5)", () => {
    const rows: Row[] = [{ name: "s", status: "success" }, { name: "f", status: "failure" }, { name: "w", status: "warning" }];
    expect(names(rows.sort(makeComparator(byStatus, "asc")))).toEqual(["f", "w", "s"]);
  });

  it("values missing from the map sort after every ranked value", () => {
    const rows: Row[] = [{ name: "x", status: "mystery" }, { name: "s", status: "success" }];
    expect(names(rows.sort(makeComparator(byStatus, "asc")))).toEqual(["s", "x"]);
  });

  it("equal-rank values fall back to deterministic text order", () => {
    const rows: Row[] = [{ name: "b", status: "verified" }, { name: "a", status: "reachable" }];
    // Neither is in RANK: both unranked ⇒ text fallback on the status string.
    expect(names(rows.sort(makeComparator(byStatus, "asc")))).toEqual(["a", "b"]);
  });
});

describe("chainComparators / sortRows", () => {
  const cols = [byName, byCount, byAt, byStatus];

  it("applies the tiebreak chain after the active column", () => {
    const rows: Row[] = [
      { name: "b", status: "failure", at: "2026-08-01T00:00:00Z" },
      { name: "a", status: "failure", at: "2026-08-02T00:00:00Z" },
      { name: "c", status: "success", at: "2026-08-03T00:00:00Z" },
    ];
    // Active: status asc (failures first); tiebreak: at desc (newer failure first).
    const out = sortRows(rows, cols, { key: "status", dir: "asc" }, [{ key: "at", dir: "desc" }]);
    expect(names(out)).toEqual(["a", "b", "c"]);
  });

  it("skips the active column inside the chain (no double-apply flip)", () => {
    const rows: Row[] = [{ name: "a", count: 1 }, { name: "b", count: 2 }];
    const out = sortRows(rows, cols, { key: "count", dir: "desc" }, [{ key: "count", dir: "asc" }, { key: "name" }]);
    expect(names(out)).toEqual(["b", "a"]);
  });

  it("returns the input order untouched for a null or unknown active key", () => {
    const rows: Row[] = [{ name: "b" }, { name: "a" }];
    expect(names(sortRows(rows, cols, null))).toEqual(["b", "a"]);
    expect(names(sortRows(rows, cols, { key: "ghost", dir: "asc" }))).toEqual(["b", "a"]);
  });

  it("does not mutate the input array", () => {
    const rows: Row[] = [{ name: "b" }, { name: "a" }];
    sortRows(rows, cols, { key: "name", dir: "asc" });
    expect(names(rows)).toEqual(["b", "a"]);
  });

  it("is stable: rows equal through the whole chain keep server order", () => {
    const rows: Row[] = [
      { name: "same", count: 1 },
      { name: "same", count: 2 },
      { name: "same", count: 3 },
    ];
    const out = sortRows(rows, cols, { key: "name", dir: "asc" });
    expect(out.map((r) => r.count)).toEqual([1, 2, 3]);
  });

  it("chainComparators returns the first non-zero result", () => {
    const always0 = () => 0;
    const minus = () => -1;
    expect(chainComparators<Row>(always0, minus)( {}, {} )).toBe(-1);
    expect(chainComparators<Row>(always0, always0)({}, {})).toBe(0);
  });
});

// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { useTableSort } from "./hooks";
import { type SortColumn } from "./utils/sort";

// useTableSort state semantics (TS-2/TS-4/TS-7, the sorting-update plan):
// toggle direction defaults, per-table localStorage persistence, aria wiring.
// The ordering rules themselves are covered by utils/sort.test.ts.

interface Row {
  name: string;
  at?: string | null;
}

const COLS: SortColumn<Row>[] = [
  { key: "name", get: (r) => r.name, type: "text" },
  { key: "at", get: (r) => r.at, type: "date" },
];

const ROWS: Row[] = [
  { name: "bravo", at: "2026-08-01T00:00:00Z" },
  { name: "alpha", at: "2026-08-02T00:00:00Z" },
];

const names = (rows: Row[]) => rows.map((r) => r.name);

beforeEach(() => localStorage.clear());

describe("useTableSort", () => {
  it("applies the default sort on mount", () => {
    const { result } = renderHook(() => useTableSort(ROWS, COLS, { key: "name", dir: "asc" }));
    expect(names(result.current.sorted)).toEqual(["alpha", "bravo"]);
    expect(result.current.sortKey).toBe("name");
    expect(result.current.ariaSort("name")).toBe("ascending");
    expect(result.current.ariaSort("at")).toBeUndefined();
  });

  it("first click on a date column opens desc; on a text column asc; second click flips", () => {
    const { result } = renderHook(() => useTableSort(ROWS, COLS, { key: "name", dir: "asc" }));
    act(() => result.current.toggle("at"));
    expect(result.current.sortDir).toBe("desc"); // newest first
    expect(names(result.current.sorted)).toEqual(["alpha", "bravo"]);
    act(() => result.current.toggle("at"));
    expect(result.current.sortDir).toBe("asc");
    expect(names(result.current.sorted)).toEqual(["bravo", "alpha"]);
    // Switching back to a text column resets to ITS default (asc), not the
    // direction the date column happened to be in.
    act(() => result.current.toggle("name"));
    expect(result.current.sortDir).toBe("asc");
  });

  it("ignores a toggle on an unknown key", () => {
    const { result } = renderHook(() => useTableSort(ROWS, COLS, { key: "name", dir: "asc" }));
    act(() => result.current.toggle("ghost"));
    expect(result.current.sortKey).toBe("name");
  });

  it("fires onChange on every toggle (server-paged callers reset their page)", () => {
    const onChange = vi.fn();
    const { result } = renderHook(() => useTableSort(ROWS, COLS, null, { onChange }));
    act(() => result.current.toggle("name"));
    act(() => result.current.toggle("name"));
    expect(onChange).toHaveBeenCalledTimes(2);
  });

  it("persists the choice under sort:<tableId> and restores it on remount", () => {
    const first = renderHook(() => useTableSort(ROWS, COLS, { key: "name", dir: "asc" }, { tableId: "t1" }));
    act(() => first.result.current.toggle("at"));
    expect(JSON.parse(localStorage.getItem("sort:t1")!)).toEqual({ key: "at", dir: "desc" });
    first.unmount();
    const second = renderHook(() => useTableSort(ROWS, COLS, { key: "name", dir: "asc" }, { tableId: "t1" }));
    expect(second.result.current.sortKey).toBe("at");
    expect(second.result.current.sortDir).toBe("desc");
  });

  it("falls back to the default when the stored key no longer names a column", () => {
    localStorage.setItem("sort:t1", JSON.stringify({ key: "removed-column", dir: "desc" }));
    const { result } = renderHook(() => useTableSort(ROWS, COLS, { key: "name", dir: "asc" }, { tableId: "t1" }));
    expect(result.current.sortKey).toBe("name");
    expect(result.current.sortDir).toBe("asc");
  });

  it("falls back to the default on an unparseable stored value", () => {
    localStorage.setItem("sort:t1", "{not json");
    const { result } = renderHook(() => useTableSort(ROWS, COLS, { key: "at" }, { tableId: "t1" }));
    expect(result.current.sortKey).toBe("at");
    expect(result.current.sortDir).toBe("desc"); // date column's defaultDir
  });

  it("null default leaves rows in server order until the first click", () => {
    const { result } = renderHook(() => useTableSort(ROWS, COLS, null));
    expect(names(result.current.sorted)).toEqual(["bravo", "alpha"]);
    expect(result.current.sortKey).toBeNull();
  });

  it("serverSide passes rows through untouched but still owns header state", () => {
    // TS-23: the endpoint ordered the page; a client re-sort could disagree
    // with the SQL collation, so `sorted` must be the input array as-is.
    const { result } = renderHook(() => useTableSort(ROWS, COLS, { key: "name", dir: "asc" }, { serverSide: true }));
    expect(result.current.sorted).toBe(ROWS); // identity, not a re-sorted copy
    expect(result.current.sortKey).toBe("name");
    expect(result.current.ariaSort("name")).toBe("ascending");
    act(() => result.current.toggle("at"));
    expect(result.current.sortKey).toBe("at");
    expect(result.current.sortDir).toBe("desc");
    expect(result.current.sorted).toBe(ROWS); // still untouched after toggling
  });
});

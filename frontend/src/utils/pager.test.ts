import { describe, it, expect } from "vitest";
import { clampPage, sliceForPage, pagerLabel } from "./pager";

// Pure pagination math behind the <Pager> control (components/ui) and the
// client-side useClientPager hook (hooks.ts). The hook/component wiring needs a
// DOM to test; this covers the logic those layers delegate here.

describe("clampPage", () => {
  it("leaves an in-range page untouched", () => {
    // 120 items / 25 per page = 5 pages (0..4); page 2 is valid.
    expect(clampPage(2, 120, 25)).toBe(2);
  });

  it("pulls the page back into range when the set shrinks", () => {
    // Was on page 4, but a tighter filter leaves only 30 items (2 pages: 0..1).
    expect(clampPage(4, 30, 25)).toBe(1);
  });

  it("clamps to 0 when there are no items", () => {
    expect(clampPage(3, 0, 25)).toBe(0);
  });

  it("never returns a negative index", () => {
    expect(clampPage(0, 0, 25)).toBe(0);
  });

  it("keeps a single full page on page 0", () => {
    expect(clampPage(0, 25, 25)).toBe(0);
    expect(clampPage(5, 25, 25)).toBe(0);
  });
});

describe("sliceForPage", () => {
  const items = Array.from({ length: 57 }, (_, i) => i); // 0..56

  it("slices items[page*pageSize, +pageSize] for the first page", () => {
    expect(sliceForPage(items, 0, 25)).toEqual(items.slice(0, 25));
  });

  it("slices a middle page", () => {
    expect(sliceForPage(items, 1, 25)).toEqual(items.slice(25, 50));
  });

  it("returns the short final page", () => {
    const last = sliceForPage(items, 2, 25);
    expect(last).toEqual(items.slice(50, 57));
    expect(last).toHaveLength(7);
  });

  it("returns empty for a page past the end", () => {
    expect(sliceForPage(items, 9, 25)).toEqual([]);
  });

  it("composes with clampPage to never strand the user", () => {
    // Simulate useClientPager: a stored page beyond the (shrunken) set is clamped
    // first, then sliced — yielding the real last page rather than an empty one.
    const page = clampPage(9, items.length, 25);
    expect(page).toBe(2);
    expect(sliceForPage(items, page, 25)).toHaveLength(7);
  });
});

describe("pagerLabel", () => {
  it("renders the plain x–y of N form", () => {
    expect(pagerLabel(120, 0, 25, "jobs")).toBe("1–25 of 120 jobs");
    expect(pagerLabel(120, 4, 25, "jobs")).toBe("101–120 of 120 jobs");
  });

  it("renders the empty form", () => {
    expect(pagerLabel(0, 0, 25, "jobs")).toBe("0 jobs");
  });

  it("renders the plain form when shown equals the page range", () => {
    expect(pagerLabel(120, 0, 25, "jobs", 25)).toBe("1–25 of 120 jobs");
  });

  it("renders the narrowed form when within-page filtering shows fewer rows", () => {
    // A within-page client filter (e.g. Jobs tagFilter) leaves 7 of the 25 rows.
    expect(pagerLabel(120, 0, 25, "jobs", 7)).toBe("7 of 25 on this page · 1–25 of 120 jobs");
  });

  it("accounts for a short final page in the narrowed range", () => {
    // Last page holds 20 rows (101–120); a filter narrows them to 3.
    expect(pagerLabel(120, 4, 25, "jobs", 3)).toBe("3 of 20 on this page · 101–120 of 120 jobs");
  });
});

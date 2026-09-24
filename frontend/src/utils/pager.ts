// Pure pagination math shared by the server-side <Pager> control (components/ui)
// and the client-side useClientPager hook (hooks.ts). Deliberately free of React,
// theme, and asset imports so it stays unit-testable in a plain (non-DOM) vitest
// environment — mirroring the utils/tree.ts house style. The UI is 0-based;
// callers that drive a 1-based API add the +1 themselves.

/** Clamp a stored page index against the current (possibly filtered-down) total,
 * so a shrinking result set never strands the user on an empty trailing page. */
export function clampPage(page: number, total: number, pageSize: number): number {
  return Math.min(page, Math.max(0, Math.ceil(total / pageSize) - 1));
}

/** The slice of `items` for a 0-based page: items[page*pageSize, +pageSize]. */
export function sliceForPage<T>(items: T[], page: number, pageSize: number): T[] {
  return items.slice(page * pageSize, page * pageSize + pageSize);
}

/** The "x–y of N noun" position label. When within-page client filtering renders
 * fewer rows than the page range, a narrowed variant says so honestly rather than
 * implying the page-position count reflects the filter (PP-H7). */
export function pagerLabel(total: number, page: number, pageSize: number, noun: string, shown?: number): string {
  if (total === 0) return `0 ${noun}`;
  const start = page * pageSize + 1;
  const end = Math.min((page + 1) * pageSize, total);
  const range = end - start + 1;
  if (shown !== undefined && shown < range) {
    return `${shown} of ${range} on this page · ${start}–${end} of ${total} ${noun}`;
  }
  return `${start}–${end} of ${total} ${noun}`;
}

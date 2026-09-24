// Pure response-shape helpers, kept free of any React / DOM / theme imports so
// they (and modules that build on them, e.g. utils/scripts) can be unit-tested in
// a plain node environment without pulling in the UI graph. Re-exported from
// ../hooks so existing `import { rows, paged } from "../hooks"` call sites are
// unaffected.

// list-or-paged: many endpoints return { items: [...] }, some a bare array.
export function rows<T = Record<string, unknown>>(data: unknown): T[] {
  if (Array.isArray(data)) return data as T[];
  if (data && typeof data === "object" && Array.isArray((data as { items?: unknown }).items)) {
    return (data as { items: T[] }).items;
  }
  return [];
}

// paged unwraps the server Page envelope ({ items, totalItems, totalPages,
// page, pageSize }) for true server-side pagination (PP-H7). totalItems falls
// back to items.length when the envelope omits counts (bare-array endpoints), so
// callers can rely on it for the "x–y of N" label without a separate count.
export interface Paged<T> {
  items: T[];
  totalItems: number;
  totalPages: number;
  page: number;
  pageSize: number;
}
export function paged<T = Record<string, unknown>>(data: unknown): Paged<T> {
  const items = rows<T>(data);
  const env = (data && typeof data === "object" ? (data as Record<string, unknown>) : {}) as {
    totalItems?: number;
    totalPages?: number;
    page?: number;
    pageSize?: number;
  };
  const totalItems = typeof env.totalItems === "number" ? env.totalItems : items.length;
  return {
    items,
    totalItems,
    totalPages: typeof env.totalPages === "number" ? env.totalPages : 1,
    page: typeof env.page === "number" ? env.page : 1,
    pageSize: typeof env.pageSize === "number" ? env.pageSize : items.length,
  };
}

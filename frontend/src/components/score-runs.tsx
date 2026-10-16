import { allReadable, api, getRequestAgency } from "../api/client";
import { paged } from "../utils/paging";
import { HOURS_BACK } from "./score-model";
import { c } from "../theme";

// The Score's past half, read whole (2.4.4). The server answers at most 200
// runs a page, newest first; a busy day holds more, and one page silently
// dropped its morning off the strip (a side agent's finding on the overview,
// then the Dashboard's on the owner's word). The Score reads its window page
// after page, up to SCORE_MAX_PAGES, and ScoreCutNote says when that was not
// all of it.

const SCORE_PAGE = 200;
export const SCORE_MAX_PAGES = 5;

// The cost (a side agent's finding): the Dashboard polls every 4 seconds, and
// every open tab of a busy agency read five pages each time. Page 1, the
// newest runs, is read on every poll as it always was; the older pages, hours
// old, at most once a minute, kept in between per agency (and per "every
// agency", the overview's), so a switch of agency never mixes two agencies' runs.
export const OLDER_PAGES_MS = 60_000;
const olderPages = new Map<string, { at: number; items: unknown[] }>();

/** Forget the kept older pages (tests). */
export function resetScoreRunsCache(): void {
  olderPages.clear();
}

/**
 * fetchScoreRuns — the runs of the Score's window, page after page. With
 * allAgencies the requests name no agency (the Operations overview, whose
 * audience reads every agency); otherwise they are the agency in use's, like
 * every list.
 */
export async function fetchScoreRuns(opts: { allAgencies?: boolean } = {}): Promise<{ data?: unknown; error?: unknown }> {
  const from = new Date(Date.now() - (HOURS_BACK + 1) * 3600 * 1000).toISOString();
  const read = (page: number) =>
    api.GET("/runs", {
      params: { query: { kind: "job", from, pageSize: SCORE_PAGE, page } },
      ...(opts.allAgencies ? { headers: allReadable } : {}),
    });
  const first = await read(1);
  if (first.error) return { error: first.error };
  const p1 = paged(first.data);
  const items: unknown[] = [...p1.items];
  const totalItems = p1.totalItems;
  const pages = Math.min(p1.totalPages, SCORE_MAX_PAGES);
  if (pages > 1 && p1.items.length > 0) {
    const key = opts.allAgencies ? "*" : getRequestAgency();
    const now = Date.now();
    let kept = olderPages.get(key);
    if (!kept || now - kept.at >= OLDER_PAGES_MS) {
      const older: unknown[] = [];
      for (let page = 2; page <= pages; page++) {
        const res = await read(page);
        if (res.error) return { error: res.error };
        const p = paged(res.data);
        older.push(...p.items);
        if (p.items.length === 0) break;
      }
      kept = { at: now, items: older };
      olderPages.set(key, kept);
    }
    // Page 1 has moved on since the older pages were read: a run can be in
    // both. Each is drawn once.
    const seen = new Set(p1.items.map((r) => (r as { traceId?: string }).traceId));
    for (const r of kept.items) {
      const id = (r as { traceId?: string }).traceId;
      if (!id || !seen.has(id)) items.push(r);
    }
  }
  return { data: { items, totalItems } };
}

/** ScoreCutNote — said above the Score when its day held more runs than it drew. */
export function ScoreCutNote({ shown, total, where = "" }: { shown: number; total: number; where?: string }) {
  if (total <= shown) return null;
  return (
    <div data-testid="score-cut" role="note" style={{ fontSize: c.fontSm, color: c.warning, marginBottom: 6 }}>
      Showing the newest {shown.toLocaleString()} of {total.toLocaleString()} runs from the last day; the older ones are not
      drawn. History has them all{where}.
    </div>
  );
}

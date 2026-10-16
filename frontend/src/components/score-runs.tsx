import { allReadable, api } from "../api/client";
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

/**
 * fetchScoreRuns — the runs of the Score's window, page after page. With
 * allAgencies the requests name no agency (the Operations overview, whose
 * audience reads every agency); otherwise they are the agency in use's, like
 * every list.
 */
export async function fetchScoreRuns(opts: { allAgencies?: boolean } = {}): Promise<{ data?: unknown; error?: unknown }> {
  const from = new Date(Date.now() - (HOURS_BACK + 1) * 3600 * 1000).toISOString();
  const items: unknown[] = [];
  let totalItems = 0;
  for (let page = 1; page <= SCORE_MAX_PAGES; page++) {
    const res = await api.GET("/runs", {
      params: { query: { kind: "job", from, pageSize: SCORE_PAGE, page } },
      ...(opts.allAgencies ? { headers: allReadable } : {}),
    });
    if (res.error) return { error: res.error };
    const p = paged(res.data);
    items.push(...p.items);
    totalItems = p.totalItems;
    if (page >= p.totalPages || p.items.length === 0) break;
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

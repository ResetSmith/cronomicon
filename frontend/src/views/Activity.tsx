import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { api } from "../api/client";
import { useGet, paged, useDebounced } from "../hooks";
import { useNameDisambiguator } from "../utils/disambiguate";
import { c, outcomeColor } from "../theme";
import { Badge, Btn, Pager, Select, SkeletonRows, TagChip, inputStyle, statusLabel, usePager } from "../components/ui";
import { fmtInAppZone } from "../utils/datetime";

export interface Entry {
  id?: number;
  kind: string;
  outcome?: string | null;
  actor?: string;
  jobName?: string;
  // R2-1 identities + the scope-derived agency set (R2F-3). NULL uid ⇒ a
  // pre-backfill row that cannot be attributed to either twin; it never badges.
  jobUid?: string | null;
  workflowUid?: string | null;
  agencies?: string[];
  // AA-1 — the runner's DISPLAY name, snapshotted when the row was written.
  // `actor` carries `runner:<id>` for agent-written rows and that id stops
  // resolving once the runner is deregistered, which is why this is its own
  // field rather than something the client parses.
  runnerName?: string | null;
  workflowName?: string;
  summary?: string;
  at?: string;
}

interface Me {
  email?: string;
}

// ── Repeat collapsing (VU-13 / E-1) ──────────────────────────────────────────
// A dev install's default view is nine consecutive `CRONOMICON_GITLAB_BASE_URL not
// configured` rows. The signal is "this happened nine times"; nine identical
// lines bury it and push everything else off the screen.

/** Consecutive entries that render identically apart from when they happened. */
export interface ActivityGroup {
  /** Stable across refetches — keyed by content + first instant, never by array
   *  index, which slides under the row when a new event arrives at the front. */
  id: string;
  key: string;
  /** The member whose fields the row renders. By construction any member would do. */
  head: Entry;
  /** Every member, in the order received (newest first, as the API returns them). */
  members: Entry[];
  earliestAt?: string;
  latestAt?: string;
}

// An activity row's identity is WHAT IT SAYS, not when it said it. These six
// fields are exactly the ones the row renders — title (job/workflow/kind), the
// kind chip, the outcome badge, the summary and the actor. Two entries matching
// on all six are indistinguishable to a reader, so the only information a second
// copy carries is its instant, which the collapsed row reports as a range.
// Deliberately excludes `id` and `at`, and excludes wire fields the row does not
// display (traceId, target, details): folding on them would split visually
// identical rows apart for reasons the operator cannot see.
export function activityKey(e: Entry): string {
  return JSON.stringify([
    e.kind ?? "",
    e.outcome ?? "",
    e.actor ?? "",
    e.jobName ?? "",
    e.workflowName ?? "",
    e.summary ?? "",
  ]);
}

// CONSECUTIVE only. A A B A A must render three rows (A×2, B, A×2), not two:
// this is a chronological feed, and merging across an intervening event would
// assert an ordering that never happened — the one thing a feed must not do.
export function groupConsecutive(entries: Entry[]): ActivityGroup[] {
  const out: ActivityGroup[] = [];
  for (const e of entries) {
    const key = activityKey(e);
    const last = out[out.length - 1];
    if (last && last.key === key) last.members.push(e);
    else out.push({ id: "", key, head: e, members: [e] });
  }
  for (const g of out) {
    const span = instantSpan(g.members);
    g.earliestAt = span.earliest;
    g.latestAt = span.latest;
    g.id = `${g.key}@${g.earliestAt ?? g.head.id ?? ""}`;
  }
  return out;
}

// Min/max by parsed instant rather than by array position: the API orders by
// `at DESC` today, but a range that silently inverts if that ever changes is a
// worse bug than the one this function exists to fix. Unparseable/absent
// timestamps are skipped, not treated as epoch 0.
function instantSpan(members: Entry[]): { earliest?: string; latest?: string } {
  let lo: { t: number; at: string } | undefined;
  let hi: { t: number; at: string } | undefined;
  for (const m of members) {
    if (!m.at) continue;
    const t = Date.parse(m.at);
    if (Number.isNaN(t)) continue;
    if (!lo || t < lo.t) lo = { t, at: m.at };
    if (!hi || t > hi.t) hi = { t, at: m.at };
  }
  return { earliest: lo?.at, latest: hi?.at };
}

// Every absolute time in this view goes through the app-zone helpers, so a
// collapsed range means the same wall clock as the single rows around it.
const AT_FMT: Intl.DateTimeFormatOptions = {
  month: "short",
  day: "numeric",
  year: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
};
const DAY_FMT: Intl.DateTimeFormatOptions = { month: "short", day: "numeric", year: "numeric" };
const TIME_FMT: Intl.DateTimeFormatOptions = { hour: "2-digit", minute: "2-digit", hour12: false };

/** "Jul 27, 2026, 14:02 – 14:37" within one app-zone day, both dates across a boundary. */
export function fmtSpan(earliest?: string, latest?: string): string {
  if (!earliest || !latest) return fmtInAppZone(latest ?? earliest, AT_FMT);
  if (earliest === latest) return fmtInAppZone(latest, AT_FMT);
  const sameDay = fmtInAppZone(earliest, DAY_FMT) === fmtInAppZone(latest, DAY_FMT);
  return `${fmtInAppZone(earliest, AT_FMT)} – ${fmtInAppZone(latest, sameDay ? TIME_FMT : AT_FMT)}`;
}

// ── Retention windows (AS-1) ─────────────────────────────────────────────────
// The feed opens on the last 72 hours — the "what is happening now" view — and
// widens one step at a time toward the 90-day retention (A4). Each step is a
// server `?from=`, never a client slice: the API had always served all 90 days
// and the page had always shown page 1 of it while CLAIMING 72h. The window is
// a fact about the query now, not about the empty-state copy.
export const WINDOWS = [
  { key: "72h", hours: 72, label: "last 72 hours", option: "Last 72 hours" },
  { key: "7d", hours: 7 * 24, label: "last 7 days", option: "Last 7 days" },
  { key: "30d", hours: 30 * 24, label: "last 30 days", option: "Last 30 days" },
  { key: "90d", hours: 90 * 24, label: "last 90 days", option: "Last 90 days (everything retained)" },
] as const;
type WindowKey = (typeof WINDOWS)[number]["key"];
type RangeKey = WindowKey | "custom";

// datetime-local speaks the BROWSER's wall clock while the API speaks absolute
// instants — the same seam the AR run-scheduler picker documents, and these
// convert the same way rather than inventing a second convention. Note the app
// can be configured to DISPLAY a different zone; where it is, the picker's
// clock and the feed's timestamps differ, so the control names the zone it is
// taking input in instead of leaving the operator to discover the offset.
export function localInputToISO(v: string): string | null {
  if (!v.trim()) return null;
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? null : d.toISOString();
}
function browserZoneLabel(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "local time";
  } catch {
    return "local time";
  }
}

// The seven wire kinds (ActivityKind). Start/end pairs stay separate here on
// purpose: "which runs FAILED" is a run-end question and the outcome badge only
// exists on the end row, so folding the pair would hide the one that matters.
const KINDS: { value: string; label: string }[] = [
  { value: "run-start", label: "Run started" },
  { value: "run-end", label: "Run finished" },
  { value: "workflow-start", label: "Workflow started" },
  { value: "workflow-end", label: "Workflow finished" },
  { value: "config", label: "Config change" },
  { value: "gitsync", label: "Git sync" },
  { value: "push", label: "Push" },
];

// The actor filter is ONE choice across three groups, so it is one piece of
// state rather than a user-picker plus a runner-picker (AA-Q3). Which group the
// choice came from decides which query param carries it — `?actor=` matches the
// actor column, `?runner=` matches runner_name — so the group travels with the
// value instead of being re-derived from its shape.
export type ActorSel = { group: "user" | "runner" | "system"; value: string };

// Option values are group-prefixed so the change handler knows which param to
// send without guessing from the string. A runner and a user could in principle
// share a name; the prefix means that never becomes a wrong query.
const actorOptionValue = (group: ActorSel["group"], value: string) => `${group[0]}:${value}`;
function parseActorOption(v: string): ActorSel | null {
  const i = v.indexOf(":");
  if (i < 0) return null;
  const g = { u: "user", r: "runner", s: "system" }[v.slice(0, i)] as ActorSel["group"] | undefined;
  return g ? { group: g, value: v.slice(i + 1) } : null;
}

interface ActorFacets {
  users?: string[];
  runners?: string[];
  system?: string[];
}

export function Activity() {
  const navigate = useNavigate();
  const meQ = useGet<Me>(() => api.GET("/me"));
  const myEmail = meQ.data?.email;

  const [searchRaw, setSearchRaw] = useState("");
  const [kind, setKindRaw] = useState("All");
  const [mineOnly, setMineOnlyRaw] = useState(false);
  const [actorSel, setActorSelRaw] = useState<ActorSel | null>(null);
  const [windowKey, setWindowKeyRaw] = useState<RangeKey>("72h");
  const [customFrom, setCustomFromRaw] = useState("");
  const [customTo, setCustomToRaw] = useState("");
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const pager = usePager();
  // A burst of keystrokes is one query, not one per character.
  const search = useDebounced(searchRaw, 300);

  // Every filter change restarts at page 1: page 3 of "all kinds" is not a
  // meaningful position inside "config changes only".
  const setSearch = (v: string) => { setSearchRaw(v); pager.setPage(0); };
  const setKind = (v: string) => { setKindRaw(v); pager.setPage(0); };
  // "My triggers" and the dropdown are two affordances for ONE filter, so each
  // clears the other. Leaving both set would send actor=<me> AND runner=<x>,
  // which the API honours as an AND — a query no click asked for.
  const setMineOnly = (v: boolean) => { setMineOnlyRaw(v); if (v) setActorSelRaw(null); pager.setPage(0); };
  const setActorSel = (sel: ActorSel | null) => { setActorSelRaw(sel); if (sel) setMineOnlyRaw(false); pager.setPage(0); };
  const setWindowKey = (k: RangeKey) => { setWindowKeyRaw(k); pager.setPage(0); };
  const setCustomFrom = (v: string) => { setCustomFromRaw(v); pager.setPage(0); };
  const setCustomTo = (v: string) => { setCustomToRaw(v); pager.setPage(0); };

  const isCustom = windowKey === "custom";
  const window_ = WINDOWS.find((w) => w.key === windowKey);

  // A relative window's `from` is anchored when the RANGE IS CHOSEN, not on
  // every render — a re-render must not move it and re-issue the query for a
  // feed that has not changed. A custom range moves only when its inputs do.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const from = useMemo(
    () => (isCustom ? localInputToISO(customFrom) : new Date(Date.now() - (window_?.hours ?? 72) * 3600 * 1000).toISOString()),
    [windowKey, customFrom],
  );
  // `to` is only ever sent by a custom range. The relative windows are all
  // "…until now", and sending an explicit now() would freeze the feed's upper
  // edge at page-load time — events arriving after it would silently never show.
  const to = isCustom ? localInputToISO(customTo) : null;

  // What the empty-state copy calls the range it searched. A half-open custom
  // range is described honestly rather than pretending it has both ends.
  const rangeLabel = !isCustom
    ? window_?.label ?? "selected range"
    : from && to
      ? `range ${fmtInAppZone(from, AT_FMT)} – ${fmtInAppZone(to, AT_FMT)}`
      : from
        ? `range since ${fmtInAppZone(from, AT_FMT)}`
        : to
          ? `range before ${fmtInAppZone(to, AT_FMT)}`
          : "full retained history";

  // AS-1 — every filter is a server param. The endpoint has carried q/kind/
  // actor/from/page since the API's first cut; the page fetched bare
  // `/activity` (page 1, 50 rows) and filtered THAT in the browser, so a search
  // silently covered the newest 50 events and nothing older. Now the total
  // reflects the filters and a search spans the whole window.
  // Only the actor the query will actually SEND is a dependency: /me resolving
  // must not re-issue the feed query while "My triggers" is off (the browser
  // trace showed every load firing twice before this).
  const actor = mineOnly && myEmail ? myEmail : actorSel && actorSel.group !== "runner" ? actorSel.value : null;
  const runner = actorSel?.group === "runner" ? actorSel.value : null;

  // The picker's options, scoped to the SAME range as the feed (AA-Q5) — widen
  // the range and the list widens with it. Keyed on the range bounds only: the
  // options describe the range, not the current filters, so narrowing by kind
  // must not shrink the menu the user is choosing from. An absent bound is
  // omitted rather than sent empty, which is what makes an open-ended custom
  // range mean "no limit on that side" to the server.
  const facetsQ = useGet<ActorFacets>(
    () => api.GET("/activity/actors", { params: { query: { ...(from ? { from } : {}), ...(to ? { to } : {}) } } }),
    [from, to],
  );
  const facets = facetsQ.data;
  const { data, error, loading } = useGet<unknown>(
    () =>
      api.GET("/activity", {
        params: {
          query: {
            page: pager.page + 1,
            pageSize: pager.pageSize,
            ...(from ? { from } : {}),
            ...(to ? { to } : {}),
            ...(search ? { q: search } : {}),
            ...(kind !== "All" ? { kind: kind as "run-start" } : {}),
            // "My triggers" is `?actor=` now, not a client-side compare — the
            // chip narrows the dataset, not the page. It waits for /me so the
            // first query is never sent with an unknown actor.
            ...(actor ? { actor } : {}),
            ...(runner ? { runner } : {}),
          },
        },
      }),
    [pager.page, pager.pageSize, from, to, search, kind, actor, runner],
  );
  const pg = paged<Entry>(data);
  const items = pg.items;
  const anyFilter = !!search || kind !== "All" || mineOnly || !!actorSel;
  // What the empty state should call the thing it found nothing for.
  const actorLabel = mineOnly ? "you" : actorSel?.value;

  // Grouped per PAGE: filtering and paging both change which events are
  // adjacent, and the collapsed row must describe what the operator is
  // actually looking at. A run of identical rows that straddles a page
  // boundary renders as two collapsed rows, one per page — honest, if plain.
  const groups = useMemo(() => groupConsecutive(items), [items]);

  // R2F-3 — a feed row names a job OR a workflow, so it is disambiguated against
  // whichever it carries. Keyed on the PAGE: the badge answers "is this
  // ambiguous in what I am looking at".
  const entryLabel = useNameDisambiguator(items, (e) =>
    e.jobName
      ? { uid: e.jobUid, name: e.jobName, agencies: e.agencies, group: "job" }
      : { uid: e.workflowUid, name: e.workflowName, agencies: e.agencies, group: "workflow" },
  );

  const clearFilters = () => {
    setSearchRaw("");
    setKindRaw("All");
    setMineOnlyRaw(false);
    setActorSelRaw(null);
    pager.setPage(0);
  };

  // The Range select is the control; this is only the empty state's escape
  // hatch, offered when there IS something wider to look at. It jumps straight
  // to the full retention rather than stepping — the one-step "+ Show older"
  // ratchet it replaces could widen but never narrow, so the only way back to
  // 72 hours was reloading the page.
  const alreadyWidest = windowKey === "90d" || isCustom;
  const searchEverything = alreadyWidest ? null : (
    <Btn small onClick={() => setWindowKey("90d")}>Search all 90 days</Btn>
  );

  return (
    <div>
      <div style={{ display: "flex", gap: 8, marginBottom: 16, alignItems: "center", flexWrap: "wrap" }}>
        <input
          value={searchRaw}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Search activity…"
          aria-label="Search activity"
          style={{ ...inputStyle(), flex: 1, minWidth: 200, maxWidth: 340 }}
        />
        <label style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: c.fontXs, color: c.textSec }}>
          Kind
          <Select value={kind} onChange={(e) => setKind(e.target.value)} aria-label="Filter by kind">
            <option value="All">All</option>
            {KINDS.map((k) => (
              <option key={k.value} value={k.value}>{k.label}</option>
            ))}
          </Select>
        </label>
        <label style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: c.fontXs, color: c.textSec }}>
          Actor
          <Select
            value={actorSel ? actorOptionValue(actorSel.group, actorSel.value) : ""}
            onChange={(e) => setActorSel(parseActorOption(e.target.value))}
            aria-label="Filter by actor"
          >
            <option value="">Anyone</option>
            {/* Empty groups are omitted rather than rendered blank — a heading
                over nothing reads as a bug. */}
            {!!facets?.users?.length && (
              <optgroup label="Users">
                {facets.users.map((u) => (
                  <option key={u} value={actorOptionValue("user", u)}>{u}</option>
                ))}
              </optgroup>
            )}
            {!!facets?.runners?.length && (
              <optgroup label="Runners">
                {facets.runners.map((rn) => (
                  <option key={rn} value={actorOptionValue("runner", rn)}>{rn}</option>
                ))}
              </optgroup>
            )}
            {!!facets?.system?.length && (
              <optgroup label="System">
                {facets.system.map((sa) => (
                  <option key={sa} value={actorOptionValue("system", sa)}>{sa}</option>
                ))}
              </optgroup>
            )}
          </Select>
        </label>
        <Chip label="All" on={!mineOnly && !actorSel} onClick={() => { setMineOnlyRaw(false); setActorSel(null); }} />
        <Chip label="My triggers" on={mineOnly} onClick={() => setMineOnly(true)} />
        <label style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: c.fontXs, color: c.textSec }}>
          Range
          <Select value={windowKey} onChange={(e) => setWindowKey(e.target.value as RangeKey)} aria-label="Time range">
            {WINDOWS.map((w) => (
              <option key={w.key} value={w.key}>{w.option}</option>
            ))}
            <option value="custom">Custom range…</option>
          </Select>
        </label>
      </div>
      {/* Revealed by the Range select rather than living permanently in the
          toolbar: an absolute range answers "what happened on the 14th", which
          is a real question but a rare one, and two date inputs beside three
          dropdowns would crowd the common case. Either end may be left blank —
          the API treats from/to independently, so "everything since the 14th"
          and "everything before the 14th" are both valid. */}
      {isCustom && (
        <div style={{ display: "flex", gap: 10, alignItems: "center", flexWrap: "wrap", marginTop: -6, marginBottom: 16 }}>
          <label style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: c.fontXs, color: c.textSec }}>
            From
            <input type="datetime-local" value={customFrom} onChange={(e) => setCustomFrom(e.target.value)}
              aria-label="Range start" style={{ ...inputStyle(), width: 210 }} />
          </label>
          <label style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: c.fontXs, color: c.textSec }}>
            To
            <input type="datetime-local" value={customTo} onChange={(e) => setCustomTo(e.target.value)}
              aria-label="Range end" style={{ ...inputStyle(), width: 210 }} />
          </label>
          <span style={{ color: c.textMuted, fontSize: c.fontXs }}>
            Times are {browserZoneLabel()}. Leave either end blank for an open range.
          </span>
        </div>
      )}
      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={5} /></div>}
      {error && <div style={{ color: c.danger }}>Error: {error}</div>}
      {/* E-2/VU-14: a read-only feed cannot offer a create button, so the offer is
          the honest one — clear the filters that are hiding everything, widen the
          window, or go to the surface that holds the record this one is too
          short to keep. */}
      {!loading && !error && items.length === 0 && (
        <div style={{ display: "flex", flexDirection: "column", alignItems: "flex-start", gap: 10, padding: "16px 0" }}>
          {!anyFilter ? (
            <>
              <div style={{ color: c.textSec }}>No activity in the {rangeLabel}.</div>
              <div style={{ color: c.textMuted, fontSize: c.fontSm, maxWidth: 460 }}>
                Runs, syncs and configuration changes appear here as they happen. Run history keeps the full record of every execution.
              </div>
              <div style={{ display: "flex", gap: 8 }}>
                {searchEverything}
                <Btn onClick={() => navigate("/runs")}>Open History</Btn>
              </div>
            </>
          ) : (
            <>
              <div style={{ color: c.textSec }}>
                {actorLabel
                  ? `No activity by ${actorLabel} in the ${rangeLabel}.`
                  : `No activity matches your filters in the ${rangeLabel}.`}
              </div>
              <div style={{ display: "flex", gap: 8 }}>
                <Btn onClick={clearFilters}>Clear filters</Btn>
                {searchEverything}
              </div>
            </>
          )}
        </div>
      )}
      <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
        {groups.map((g) => {
          const e = g.head;
          const count = g.members.length;
          const open = expanded[g.id] ?? false;
          return (
            <div
              key={g.id}
              style={{
                background: c.panel,
                border: `1px solid ${c.border}`,
                borderLeft: `3px solid ${outcomeColor(e.outcome)}`,
                borderRadius: c.radiusSurface,
                padding: "10px 14px",
              }}
            >
              <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 10 }}>
                <span style={{ display: "inline-flex", alignItems: "center", gap: 8, minWidth: 0 }}>
                  <span style={{ fontWeight: 600 }}>{entryLabel(e) || e.kind}</span>
                  <TagChip label={e.kind} />
                </span>
                <span style={{ display: "inline-flex", alignItems: "center", gap: 10, flexShrink: 0 }}>
                  {e.outcome && <Badge status={e.outcome === "failure" ? "danger" : e.outcome} label={statusLabel(e.outcome)} />}
                  {count > 1 && (
                    <button
                      onClick={() => setExpanded((m) => ({ ...m, [g.id]: !open }))}
                      aria-expanded={open}
                      title={open ? "Hide each occurrence" : "Show each occurrence"}
                      style={{
                        padding: "2px 8px",
                        borderRadius: c.radiusChip,
                        border: `1px solid ${c.border}`,
                        background: open ? c.panelHover : c.panel2,
                        color: c.textSec,
                        fontSize: c.fontXs,
                        fontFamily: c.mono,
                        fontVariantNumeric: "tabular-nums",
                        fontWeight: 600,
                        cursor: "pointer",
                      }}
                    >
                      ×{count}
                    </button>
                  )}
                  <span style={{ color: c.textMuted, fontSize: c.fontXs, fontFamily: c.mono, fontVariantNumeric: "tabular-nums" }}>
                    {count > 1 ? fmtSpan(g.earliestAt, g.latestAt) : fmtInAppZone(e.at, AT_FMT)}
                  </span>
                </span>
              </div>
              {e.summary && <div style={{ color: c.textSec, fontSize: c.fontSm, marginTop: 2 }}>{e.summary}</div>}
              {/* AA-3 — the readable half of what a runner sort would have
                  bought: the eye can scan for "which runner" without sorting.
                  A `runner:<id>` actor is redundant beside the name, so it
                  demotes to the chip's title rather than printing a UUID. */}
              {(e.actor || e.runnerName) && (
                <div style={{ display: "flex", alignItems: "center", gap: 6, marginTop: 3, flexWrap: "wrap" }}>
                  {e.runnerName && (
                    <span title={e.actor?.startsWith("runner:") ? e.actor : undefined}>
                      <TagChip label={`⚙ ${e.runnerName}`} />
                    </span>
                  )}
                  {e.actor && !(e.runnerName && e.actor.startsWith("runner:")) && (
                    <span style={{ color: c.textSec, fontSize: c.fontXs }}>{e.actor}</span>
                  )}
                </div>
              )}
              {/* Nothing is lost to the collapse: the members differ from the row
                  above ONLY in their instant (that is what activityKey asserts),
                  so listing the instants restores every event individually. */}
              {count > 1 && open && (
                <div style={{ marginTop: 8, paddingTop: 6, borderTop: `1px solid ${c.borderLight}`, display: "flex", flexDirection: "column", gap: 2 }}>
                  {g.members.map((m, j) => (
                    <span key={m.id ?? j} style={{ color: c.textMuted, fontSize: c.fontXs, fontFamily: c.mono, fontVariantNumeric: "tabular-nums" }}>
                      {fmtInAppZone(m.at, AT_FMT)}
                    </span>
                  ))}
                </div>
              )}
            </div>
          );
        })}
      </div>
      {/* Server-side pages: the count is the FILTERED total across the window,
          so "1–50 of 312 events" is a true statement about the search. */}
      {!loading && !error && pg.totalItems > 0 && (
        <div style={{ marginTop: 12 }}>
          <Pager pager={pager} page={pager.page} total={pg.totalItems} noun="events" />
        </div>
      )}
    </div>
  );
}

function Chip({ label, on, onClick }: { label: string; on: boolean; onClick: () => void }) {
  return (
    <span
      onClick={onClick}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onClick();
        }
      }}
      // A toggle the keyboard can reach and a screen reader can name — this
      // was a bare clickable span.
      role="button"
      tabIndex={0}
      aria-pressed={on}
      style={{
        padding: "7px 14px",
        // A filter toggle, not a status — chip shape (B-3/VU-17).
        borderRadius: c.radiusChip,
        cursor: "pointer",
        userSelect: "none",
        fontSize: c.fontSm,
        fontWeight: 600,
        border: `1px solid ${on ? c.primary : c.border}`,
        background: on ? c.primaryBg : "transparent",
        color: on ? c.primary : c.textSec,
      }}
    >
      {label}
    </span>
  );
}

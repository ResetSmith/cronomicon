// RX-14 — the Reactions tab under Schedules.
//
// A reaction is a fourth trigger kind: "when <definition> finishes with
// <outcome>, run me". It is the only trigger whose event is another definition
// finishing rather than an instant arriving, and that is exactly what makes this
// tab load-bearing rather than a convenience.
//
// §2.10 — reactions make the run graph IMPLICIT. A workflow's coupling is
// visible in its step graph; a reaction's is not visible anywhere unless
// somewhere shows the edges. Upcoming cannot help: it projects instants, and a
// reaction has none. So this tab is the one canonical picture of what triggers
// what, and the direct answer to the objection that reactions are spookier than
// a workflow's steps.
//
// Following CAL-18's precedent it is a TAB, not a sidebar item: the sidebar
// count is a standing constraint and a reaction is scheduling data rather than
// its own domain.
import { useMemo, useState } from "react";
import { api } from "../../api/client";
import { useGet } from "../../hooks";
import { c } from "../../theme";
import { FilterSelect, SearchBar, SkeletonRows, TableSurface } from "../../components/ui";
import { OwnerBadge, banner, td, th } from "./ui";

export type Reaction = {
  ownerKind: string;
  ownerName: string;
  ownerSource: string;
  name: string;
  onKind: string;
  onName: string;
  onSource: string;
  onOutcome: string;
  delaySeconds: number;
  minIntervalSeconds: number;
  includeWorkflowChildren: boolean;
  enabled: boolean;
  position: number;
  missing: boolean;
};

// OutcomeBadge — which terminal outcome the reaction waits for.
//
// The tones carry meaning an operator already knows from run statuses, with one
// deliberate exception: `stopped` is deliberately NEUTRAL rather than red.
// It means a human ended the upstream and did not say what it meant, and
// colouring it as a failure would re-assert exactly the conflation the outcome
// exists to avoid.
function OutcomeBadge({ outcome }: { outcome: string }) {
  const tone =
    outcome === "success" ? c.success
      : outcome === "failure" ? c.danger
        : outcome === "stopped" ? c.textSec
          : c.info; // any
  const title =
    outcome === "success" ? "Fires when the upstream succeeds. A run that exited with warnings counts as a success — it ran and it finished."
      : outcome === "failure" ? "Fires when the upstream fails, including system failures like a lost executor."
        : outcome === "stopped" ? "Fires when a human ended the upstream WITHOUT saying what it meant — an unclassified stop, or a cancelled workflow. A stop that was classified follows its classification instead."
          : "Fires whenever the upstream finishes, any of the three outcomes. Never fires for a run that did not happen, such as one suppressed by a calendar.";
  return (
    <span
      title={title}
      style={{
        fontSize: c.fontXs,
        fontWeight: 600,
        padding: "2px 8px",
        borderRadius: c.radiusPill,
        background: `${tone}18`,
        color: tone,
        border: `1px solid ${tone}40`,
        whiteSpace: "nowrap",
      }}
    >
      {outcome}
    </span>
  );
}

// MissingBadge — the watched definition is gone, so this reaction can never
// fire. Deleting a watched definition never cascades its reactions away (that
// would be a silent loss of something you built), and the Git prune can create
// this state with nobody to ask — so dangling is a supported state that has to
// be VISIBLE rather than quietly inert.
const MissingBadge = () => (
  <span
    title="The watched definition no longer exists, so this reaction can never fire. It was kept rather than deleted, because removing it silently would lose work you authored — repoint it or delete it."
    style={{
      fontSize: c.fontXs,
      fontWeight: 600,
      padding: "2px 8px",
      borderRadius: c.radiusChip,
      background: `${c.danger}18`,
      color: c.danger,
      border: `1px solid ${c.danger}40`,
      whiteSpace: "nowrap",
    }}
  >
    missing
  </span>
);

const OFF_BADGE_TITLE =
  "Switched off. It stays authored and keeps its place, but observes nothing until re-enabled — which is the point: pausing the whole definition would also take its schedules down.";

function secs(n: number): string {
  if (n <= 0) return "—";
  if (n % 3600 === 0) return `${n / 3600}h`;
  if (n % 60 === 0) return `${n / 60}m`;
  return `${n}s`;
}

const KINDS = ["All", "Jobs", "Workflows"];

export function ReactionsTab() {
  const { data, error, loading } = useGet<Reaction[]>(() => api.GET("/reactions", {}), []);
  const [kind, setKind] = useState("All");
  const [search, setSearch] = useState("");
  const [grouped, setGrouped] = useState(false);

  const all = useMemo<Reaction[]>(() => (Array.isArray(data) ? data : []), [data]);

  const rows = useMemo(() => {
    const q = search.trim().toLowerCase();
    return all.filter((r) => {
      if (kind === "Jobs" && r.ownerKind !== "job") return false;
      if (kind === "Workflows" && r.ownerKind !== "workflow") return false;
      if (!q) return true;
      // One box over both ends of the edge plus the entry name: an operator
      // asking "what touches nightly-extract" does not know or care which side
      // of the arrow it sits on.
      return (
        r.ownerName.toLowerCase().includes(q) ||
        r.onName.toLowerCase().includes(q) ||
        r.name.toLowerCase().includes(q)
      );
    });
  }, [all, kind, search]);

  // The grouped view is this tab's "picture": the edge list re-read as
  // "finishing X starts these". Fan-out is the shape that actually surprises
  // people, and a flat list sorted by owner hides it.
  const byUpstream = useMemo(() => {
    const m = new Map<string, Reaction[]>();
    for (const r of rows) {
      const key = `${r.onKind}:${r.onSource}/${r.onName}`;
      const list = m.get(key);
      if (list) list.push(r);
      else m.set(key, [r]);
    }
    return [...m.entries()].sort((a, b) => a[0].localeCompare(b[0]));
  }, [rows]);

  const danglingCount = all.filter((r) => r.missing).length;

  return (
    <div>
      <div style={{ display: "flex", alignItems: "center", gap: 12, marginBottom: 12, flexWrap: "wrap" }}>
        <div style={{ fontSize: c.fontSm, color: c.textSec, flex: 1, minWidth: 300 }}>
          A reaction runs one definition when another one finishes. Authored on the definition that runs —
          in its composer, or in <code>spec.reactions</code> for a Git-defined one.
        </div>
      </div>

      {/* §2.10 — the cost this feature makes the product pay, stated rather than
          discovered. Upcoming is no longer the complete answer to "what will
          run", and the only honest fix is to say so where the edges live. */}
      <div style={{ ...banner(c.info), marginBottom: 16 }}>
        <strong>Reactions have no clock.</strong> They fire when something else finishes, so they cannot be
        projected onto <em>Upcoming</em> — the only place they are visible ahead of time is this list. A new
        reaction also starts from now: it fires on the <strong>next</strong> matching completion and never
        retroactively on one that already happened.
      </div>

      {danglingCount > 0 && (
        <div style={{ ...banner(c.danger), marginBottom: 16 }}>
          <strong>
            {danglingCount === 1
              ? "1 reaction watches a definition that no longer exists"
              : `${danglingCount} reactions watch definitions that no longer exist`}
          </strong>{" "}
          and can never fire. {danglingCount === 1 ? "It was" : "They were"} kept rather than deleted — repoint or
          remove {danglingCount === 1 ? "it" : "them"}.
        </div>
      )}

      <div style={{ display: "flex", alignItems: "center", gap: 10, marginBottom: 14, flexWrap: "wrap" }}>
        <SearchBar value={search} onChange={setSearch} placeholder="Search by job, workflow or reaction name…" />
        <FilterSelect label="Owner" value={kind} options={KINDS} onChange={setKind} />
        <label style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: c.fontXs, color: c.textSec, whiteSpace: "nowrap" }}>
          <input type="checkbox" checked={grouped} onChange={(e) => setGrouped(e.target.checked)} />
          Group by what they watch
        </label>
      </div>

      {loading && <div style={{ padding: 16 }}><SkeletonRows rows={4} /></div>}
      {error && <div style={{ color: c.danger }}>Error: {error}</div>}

      {!loading && !error && all.length === 0 && (
        // The teaching empty state: this is where a first-time operator learns
        // what a reaction is, and — just as important — what it is not.
        <div style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, background: c.panel, padding: 20, maxWidth: 720 }}>
          <div style={{ fontSize: c.fontSm, color: c.text, fontWeight: 600, marginBottom: 8 }}>No reactions yet.</div>
          <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
            <p style={{ marginTop: 0 }}>
              Jobs and workflows can only depend on each other <em>inside</em> a workflow today. A reaction lifts that
              out: <strong>when this finishes, run that</strong> — in any combination of jobs and workflows, whichever
              way the first one was triggered.
            </p>
            <p>
              You choose which outcome to wait for. <strong>success</strong> (a run that exited with warnings counts —
              it ran and it finished), <strong>failure</strong>, <strong>stopped</strong> for when a human ended it
              without saying what it meant, or <strong>any</strong>. Stopped is its own outcome on purpose: treating it
              as a failure would fire your rollback while you are already hands-on fixing the thing.
            </p>
            <p style={{ marginBottom: 0 }}>
              A reaction is <strong>edge-triggered</strong> — it never waits, accumulates or holds state. It sees a
              completion and fires, or there is nothing. That is why a new one starts from now rather than catching up
              on history, and why this list is the only place they can be seen ahead of time.
            </p>
          </div>
        </div>
      )}

      {!loading && !error && all.length > 0 && rows.length === 0 && (
        <div style={{ padding: 20, fontSize: c.fontSm, color: c.textMuted }}>
          No reactions match these filters.
        </div>
      )}

      {!loading && !error && rows.length > 0 && !grouped && (
        <TableSurface>
          <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm }}>
            <thead>
              <tr style={{ color: c.textSec, borderBottom: `1px solid ${c.border}` }}>
                <th style={th()}>Runs</th>
                <th style={th()}>When this finishes</th>
                <th style={th()}>Outcome</th>
                <th style={th()}>Delay</th>
                <th style={th()}>Min interval</th>
                <th style={th()}>Reaction</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={`${r.ownerKind}:${r.ownerSource}/${r.ownerName}/${r.name}`} style={{ borderBottom: `1px solid ${c.border}`, opacity: r.enabled ? 1 : 0.55 }}>
                  <td style={td}>
                    <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                      <OwnerBadge kind={r.ownerKind} />
                      <span style={{ fontFamily: c.mono, fontWeight: 600, color: c.text }}>{r.ownerName}</span>
                      {!r.enabled && (
                        <span title={OFF_BADGE_TITLE} style={{ fontSize: c.fontXs, color: c.textMuted, fontWeight: 600 }}>off</span>
                      )}
                    </div>
                  </td>
                  <td style={td}>
                    <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                      <OwnerBadge kind={r.onKind} />
                      <span style={{ fontFamily: c.mono, color: r.missing ? c.textMuted : c.text }}>{r.onName}</span>
                      {r.missing && <MissingBadge />}
                      {r.includeWorkflowChildren && r.onKind === "job" && (
                        <span
                          title="Also fires when this job runs as a step INSIDE a workflow. Off by default, because it gives the parent workflow fan-out that appears nowhere in its step graph."
                          style={{ fontSize: c.fontXs, color: c.textMuted }}
                        >
                          incl. workflow steps
                        </span>
                      )}
                    </div>
                  </td>
                  <td style={td}><OutcomeBadge outcome={r.onOutcome} /></td>
                  <td style={{ ...td, color: c.textSec }}>{secs(r.delaySeconds)}</td>
                  <td style={{ ...td, color: c.textSec }}>{secs(r.minIntervalSeconds)}</td>
                  <td style={{ ...td, fontFamily: c.mono, color: c.textMuted, fontSize: c.fontXs }}>{r.name}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableSurface>
      )}

      {!loading && !error && rows.length > 0 && grouped && (
        <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
          {byUpstream.map(([key, list]) => {
            const first = list[0];
            return (
              <div key={key} style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, background: c.panel, padding: 14 }}>
                <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 10, flexWrap: "wrap" }}>
                  <span style={{ fontSize: c.fontXs, color: c.textMuted, textTransform: "uppercase", letterSpacing: 0.7 }}>
                    When
                  </span>
                  <OwnerBadge kind={first.onKind} />
                  <span style={{ fontFamily: c.mono, fontWeight: 600, color: first.missing ? c.textMuted : c.text }}>{first.onName}</span>
                  {first.missing && <MissingBadge />}
                  <span style={{ fontSize: c.fontXs, color: c.textMuted }}>
                    finishes → {list.length} reaction{list.length === 1 ? "" : "s"}
                  </span>
                </div>
                <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
                  {list.map((r) => (
                    <div
                      key={`${r.ownerKind}/${r.ownerName}/${r.name}`}
                      style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap", opacity: r.enabled ? 1 : 0.55 }}
                    >
                      <OutcomeBadge outcome={r.onOutcome} />
                      <span style={{ color: c.textMuted }}>→</span>
                      <OwnerBadge kind={r.ownerKind} />
                      <span style={{ fontFamily: c.mono, color: c.text }}>{r.ownerName}</span>
                      {r.delaySeconds > 0 && (
                        <span style={{ fontSize: c.fontXs, color: c.textMuted }}>after {secs(r.delaySeconds)}</span>
                      )}
                      {!r.enabled && (
                        <span title={OFF_BADGE_TITLE} style={{ fontSize: c.fontXs, color: c.textMuted, fontWeight: 600 }}>off</span>
                      )}
                    </div>
                  ))}
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

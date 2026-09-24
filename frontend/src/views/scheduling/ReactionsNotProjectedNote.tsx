// RX-18 (§2.10) — Upcoming cannot project a reaction, and says so.
//
// This is the visibility cost the reactions feature makes the product pay, and
// the plan is explicit that it must be STATED rather than discovered. Upcoming
// answers "what will run" by projecting instants forward from cron expressions
// and windows. A reaction has no instant — it fires when something else
// finishes — so the moment reactions shipped, this table silently stopped being
// the complete answer to the question it exists to answer.
//
// CAL 1C set the precedent for the same class of failure: a projection that
// quietly omits something is worse than one that admits its own boundary,
// because the omission looks exactly like "nothing is scheduled".
//
// It renders unconditionally rather than only when reactions exist. A note that
// appears once somebody authors a reaction teaches nobody the boundary before
// they hit it, and the count would cost a fetch on a table that does not
// otherwise need one.
import { c } from "../../theme";

// The Reactions tab's index in Schedules' TABS array. A constant here rather
// than a magic number at the call site, next to the reason it is an index at all.
const REACTIONS_TAB_INDEX = 4;

// onGoToTab is supplied by the Schedules host, NOT a URL link. The host derives
// the active tab from ?tab= exactly once, at mount, so navigating to
// /schedules?tab=reactions from inside Schedules changes the URL and nothing
// else — the view stays put and the URL then disagrees with what is on screen.
// The repo documents this trap in Schedules.tsx and UpcomingTab.tsx; the first
// version of this note walked straight into it.
export function ReactionsNotProjectedNote({ onGoToTab }: { onGoToTab?: (i: number) => void }) {
  return (
    <div
      style={{
        display: "flex",
        alignItems: "baseline",
        gap: 6,
        flexWrap: "wrap",
        padding: "8px 0 14px",
        fontSize: c.fontXs,
        color: c.textMuted,
        lineHeight: 1.6,
      }}
    >
      <span>
        This projects <strong>scheduled</strong> fires. Reactions are not shown — a reaction fires when another
        definition finishes, so it has no instant to project.
      </span>
      {onGoToTab && (
        <button
          onClick={() => onGoToTab(REACTIONS_TAB_INDEX)}
          style={{
            background: "none",
            border: "none",
            padding: 0,
            color: c.primary,
            cursor: "pointer",
            fontSize: "inherit",
            fontFamily: "inherit",
            whiteSpace: "nowrap",
          }}
        >
          See the Reactions tab →
        </button>
      )}
    </div>
  );
}

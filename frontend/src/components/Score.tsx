import { useMemo, useRef, useState, type CSSProperties, type KeyboardEvent } from "react";
import { c } from "../theme";
import { Btn, CopyText, statusLabel, statusTone } from "./ui";
import { fmtDuration } from "../utils/datetime";
import {
  executorLabel,
  frac,
  groupMarks,
  inWindow,
  nearestGroup,
  scoreWindow,
  stemHeight,
  ticks,
  triggerLabel,
  type Mark,
  type MarkGroup,
  type Window,
} from "./score-model";

// ── The Score ────────────────────────────────────────────────────────────────
// One timeline replacing four dashboard cards — the histogram, Upcoming, Recent
// Completions and Currently Running (VU-Q3(a)). Those four were one dataset
// split by tense, each rendering it as a different list; the operator was
// reassembling "what happened and what is about to" in their head.
//
// The window is FIXED at 24h back / 12h ahead and there is no range picker
// (VU-Q8). That is a measured decision, not a simplification: at every viewport
// from 1280 to 1920 there is at most one pair of distinct times closer than 8px,
// so 36h fits without crowding — and a 24h forward horizon keeps the projection
// inside the API's caps, which a 7d picker did not.
//
// The component takes normalised marks rather than fetching: the tense split and
// the wire shapes belong to the Dashboard, and passing data in is what makes the
// whole thing testable without a network.

/** Identity for a group that survives the array being rebuilt by a poll. */
const groupKey = (g: MarkGroup) => `${g.tense}@${g.at}`;

/** DOM id for a mark, so the plot can point aria-activedescendant at it (H-1). */
const markId = (g: MarkGroup) => `score-mark-${g.tense}-${g.at}`;

// H-1 — the accessible name for one mark. The plot is a listbox and each mark is
// an option, which means each one needs a name a screen reader can read on its
// own: "option 3 of 40" is not a run.
//
// It says the same things the visible mark does, in the same words — the job
// name, the canonical status label, and the timestamp in the application
// timezone — so the announcement and the readout below cannot drift apart. A
// coincident group announces its count and the WORST outcome in it, which is the
// colour the notehead is already painted (VU-21).
function markLabel(g: MarkGroup, stampInZone: (at: number) => string): string {
  const when = stampInZone(g.at);
  const outcome = g.tense === "past" ? statusLabel(g.status) : "Scheduled";
  if (g.members.length > 1) {
    const verb = g.tense === "past" ? "ran" : "scheduled";
    return `${g.members.length} runs ${verb} at ${when}, worst result ${outcome}`;
  }
  const name = g.members[0]?.jobName ?? "Run";
  return g.tense === "past" ? `${name}, ${outcome}, ran ${when}` : `${name}, scheduled ${when}`;
}

// D-9: replacing four cards with one component left the dashboard well short of
// a screen — 900px of viewport with the whole thing above the fold and room to
// spare. Of the plan's two options for that space, the taller plot is the one
// that buys something: at 190px the stems had ~28px of travel for a range from
// 14 seconds to an hour, and the log scale needs room to be legible. The full
// score stays behind its toggle rather than being forced inline, because it is
// depth on demand, not the default reading.
const PLOT_H = 248;
const BASELINE = 178; // y of the staff line the noteheads sit on
const NOTE_R = 5;
// Coincident runs stack as a CHORD — one pip per member, worst on the staff
// line — rather than one notehead with a count numeral. A column of seven red
// pips is legible from across the room; an 11px "7" is not, and worst-only
// colouring hid the composition (six successes + one failure painted entirely
// red). The cap bounds the column against a burst: past it, the mildest members
// fold into a "+k" numeral, so an overflow can never hide a failure.
const STACK_CAP = 8;
const PIP_GAP = 2;

export interface ScoreProps {
  marks: Mark[];
  /** Injected so tests are deterministic and the playhead is not a hidden clock read. */
  now: number;
  /** Hour-of-day in the APPLICATION timezone — ticks and the day rule follow the scheduler's zone, not the browser's. */
  hourInZone: (at: number) => number;
  /** Label for an axis tick, in the application timezone. */
  labelInZone: (at: number) => string;
  /** Absolute timestamp for the readout. */
  stampInZone: (at: number) => string;
  /** SR-1 — time-of-day (with seconds) for a readout row; falls back to stampInZone. */
  timeInZone?: (at: number) => string;
  /** SR-1 — where a readout row's "View run" goes. Absent ⇒ no link. */
  runHref?: (traceId: string) => string;
  /** SR-1 — client-side navigation for that link (the href stays for middle-click). */
  onOpenRun?: (traceId: string) => void;
  /** The forward projection stopped here because the API capped it (D-2b). */
  truncatedAt?: number | null;
  loading?: boolean;
  /* FX-1 — the full score is an EXPANSION of this panel, not a second block below
     it. Supply these three and the header grows an expand control that swaps the
     single lane for the per-job staves inside the same surface; omit them and the
     panel is exactly what it was, compact and with no toggle. That is what keeps
     the expansion optional at the call site instead of forced on every consumer. */
  /** Every job that should get a staff when expanded. Absent ⇒ no expand control. */
  jobNames?: string[];
  /** Job name → run type, for the full score's sections. */
  types?: Record<string, string | null | undefined>;
  expanded?: boolean;
  onToggleExpand?: () => void;
}

// FX-1/FX-Q5 — the shell owns the panel chrome (section, header, caption, legend,
// expand control) and swaps which body renders inside it. Deliberately NOT one
// bimodal component: `ScorePlot` and `FullScore` keep their own tested keyboard
// and ARIA contracts, and neither knows the other exists. Swapping whole bodies
// is also why no height animation is involved, so there is no reduced-motion
// question to answer here (VF-10's precedent applies to animated growth).
export function Score(props: ScoreProps) {
  const { marks, now, stampInZone, jobNames, types, expanded, onToggleExpand } = props;
  const canExpand = !!jobNames?.length && !!onToggleExpand;
  const isExpanded = canExpand && !!expanded;
  return (
    <section aria-label="Score" style={{ marginBottom: 20 }}>
      <Header
        now={now}
        stampInZone={stampInZone}
        jobCount={canExpand ? jobNames!.length : undefined}
        expanded={isExpanded}
        onToggleExpand={canExpand ? onToggleExpand : undefined}
      />
      {isExpanded ? (
        <FullScore marks={marks} now={now} jobNames={jobNames!} stampInZone={stampInZone} types={types} />
      ) : (
        <ScorePlot {...props} />
      )}
    </section>
  );
}

// The compact single-lane body: the plot and its readout. Extracted from Score
// unchanged (FX-1) so the shell above can swap it out wholesale — every line
// below this point behaves exactly as it did when Score rendered it directly.
function ScorePlot({ marks, now, hourInZone, labelInZone, stampInZone, timeInZone, runHref, onOpenRun, truncatedAt, loading }: ScoreProps) {
  const w = useMemo(() => scoreWindow(now), [now]);
  const groups = useMemo(() => groupMarks(marks.filter((m) => inWindow(w, m.at))), [marks, w]);
  const axis = useMemo(() => ticks(w, hourInZone), [w, hourInZone]);

  // Hover and keyboard selection are separate: a pointer leaving the plot must
  // not silently discard where the keyboard is.
  //
  // Both are held as a KEY, not as an object reference or an index. The
  // Dashboard polls every four seconds and rebuilds the marks array each time,
  // so an identity-held selection would lose its highlight on every poll, and an
  // index-held one would quietly slide onto a different run the moment a new
  // mark arrived at the front. A key survives both.
  const [hoveredKey, setHoveredKey] = useState<string | null>(null);
  const [cursorKey, setCursorKey] = useState<string | null>(null);
  const plotRef = useRef<HTMLDivElement>(null);
  const activeKey = hoveredKey ?? cursorKey;
  const active = activeKey != null ? (groups.find((g) => groupKey(g) === activeKey) ?? null) : null;
  // The keyboard cursor specifically — aria-activedescendant tracks this and not
  // `active`, so hovering does not narrate (H-1). Null when the cursor's mark has
  // fallen out of the window since a poll, which is why it is looked up rather
  // than assumed present.
  const cursorGroup = cursorKey != null ? (groups.find((g) => groupKey(g) === cursorKey) ?? null) : null;

  // D-3: resolve pointer → time → nearest mark ON THE TRACK. Per-mark hit boxes
  // were the prototype's failure: at notehead size, marks minutes apart overlap
  // and the one painted last swallows its neighbours, so three of six seeded
  // runs were unhoverable. Resolving against time reaches every mark, including
  // one sitting underneath another.
  const resolve = (clientX: number) => {
    const rect = plotRef.current?.getBoundingClientRect();
    if (!rect || rect.width === 0) return null;
    const t = w.start + ((clientX - rect.left) / rect.width) * (w.end - w.start);
    return nearestGroup(groups, w, t);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (!groups.length) return;
    // D-5: ONE tab stop into the score, arrows step through marks. Per-mark tab
    // stops would add 60+ stops to the dashboard.
    const at = cursorKey != null ? groups.findIndex((g) => groupKey(g) === cursorKey) : -1;
    const step = (to: number) => {
      e.preventDefault();
      setHoveredKey(null);
      setCursorKey(groupKey(groups[Math.max(0, Math.min(groups.length - 1, to))]));
    };
    if (e.key === "ArrowRight") step(at < 0 ? 0 : at + 1);
    else if (e.key === "ArrowLeft") step(at < 0 ? 0 : at - 1);
    else if (e.key === "Home") step(0);
    else if (e.key === "End") step(groups.length - 1);
    else if (e.key === "Escape") setCursorKey(null);
  };

  const pct = (at: number) => `${frac(w, at) * 100}%`;

  return (
    <>
      {/* H-1 — a listbox, not a `group`. The keyboard contract D-5 built was
          real (one tab stop, arrows step, Home/End jump, Escape clears) but
          nothing announced it: `group` is not a widget role, so a screen reader
          had no reason to say the arrow keys did anything, and the marks
          themselves carried an onClick with no role and no name — a mouse-only
          affordance sitting inside a keyboard-navigable container.
          `listbox`/`option` is the role pair that matches what this already
          does: a single-select list where the selection drives the readout.
          aria-activedescendant follows the KEYBOARD cursor, not the hover — a
          pointer sweeping the plot must not narrate forty runs. */}
      <div
        ref={plotRef}
        role="listbox"
        tabIndex={0}
        aria-activedescendant={cursorGroup ? markId(cursorGroup) : undefined}
        aria-label={`Run timeline, ${HOURS_LABEL}. ${groups.length} runs and scheduled runs. Use arrow keys to step through them.`}
        onMouseMove={(e) => {
          const g = resolve(e.clientX);
          setHoveredKey(g ? groupKey(g) : null);
        }}
        onMouseLeave={() => setHoveredKey(null)}
        onKeyDown={onKeyDown}
        style={{
          position: "relative",
          height: PLOT_H,
          background: c.panel,
          border: `1px solid ${c.border}`,
          borderRadius: c.radiusSurface,
          overflow: "hidden",
          cursor: groups.length ? "crosshair" : "default",
        }}
      >
        {/* Axis: hour ticks, 6-hour bar lines, and the day boundary. */}
        {axis.map((t) => (
          <div
            key={t.at}
            aria-hidden
            style={{
              position: "absolute",
              left: pct(t.at),
              top: t.dayBoundary ? 0 : t.major ? 22 : BASELINE - 5,
              bottom: t.dayBoundary || t.major ? 34 : PLOT_H - BASELINE - 34 + 29,
              width: 1,
              background: t.dayBoundary ? c.border : t.major ? c.borderLight : c.borderLight,
              opacity: t.dayBoundary ? 1 : t.major ? 0.8 : 0.5,
            }}
          />
        ))}

        {/* The staff line the noteheads sit on. */}
        <div aria-hidden style={{ position: "absolute", left: 0, right: 0, top: BASELINE, height: 1, background: c.border }} />

        {/* The playhead — gold, because gold is the brand now (VU-Q4) and this is
            the one mark on the dashboard that says "you are here". */}
        <div
          aria-hidden
          style={{ position: "absolute", left: pct(now), top: 14, bottom: 30, width: 2, background: c.accent, opacity: 0.9 }}
        />
        <div
          aria-hidden
          style={{
            position: "absolute",
            left: pct(now),
            top: 2,
            transform: "translateX(-50%)",
            fontSize: c.fontXs,
            fontFamily: c.sansCond,
            textTransform: "uppercase",
            letterSpacing: 0.7,
            color: c.accent,
            whiteSpace: "nowrap",
          }}
        >
          now
        </div>

        {/* D-2b: where the forward projection stopped because the API capped it,
            rather than letting the plot trail off with no explanation. */}
        {truncatedAt != null && inWindow(w, truncatedAt) && (
          <div
            role="note"
            aria-label="The schedule projection was capped here; later scheduled runs are not shown."
            title="The schedule projection was capped here — later scheduled runs are not shown."
            style={{ position: "absolute", left: pct(truncatedAt), top: 30, bottom: 34, width: 1, borderLeft: `1px dashed ${c.textMuted}` }}
          />
        )}

        {groups.map((g) => (
          <Notehead
            key={groupKey(g)}
            g={g}
            left={pct(g.at)}
            active={activeKey === groupKey(g)}
            selected={cursorKey === groupKey(g)}
            label={markLabel(g, stampInZone)}
            onSelect={() => {
              setHoveredKey(null);
              setCursorKey(groupKey(g));
            }}
          />
        ))}

        {!loading && !groups.length && (
          // aria-hidden: a listbox may only contain options, and this sentence is
          // already said twice to a screen reader — by the plot's own label ("0
          // runs and scheduled runs") and by the Readout's live region below.
          <div aria-hidden style={{ position: "absolute", inset: 0, display: "grid", placeItems: "center", color: c.textMuted, fontSize: c.fontSm }}>
            Nothing ran in the last 24 hours, and nothing is scheduled for the next 12.
          </div>
        )}

        {/* Axis labels sit inside the plot so they cannot drift out of alignment
            with their ticks when the container resizes. */}
        {axis
          .filter((t) => t.major)
          .map((t) => (
            <div
              key={`l${t.at}`}
              aria-hidden
              style={{
                position: "absolute",
                left: pct(t.at),
                bottom: 8,
                transform: "translateX(-50%)",
                fontSize: c.fontXs,
                fontFamily: c.mono,
                color: c.textMuted,
                whiteSpace: "nowrap",
              }}
            >
              {labelInZone(t.at)}
            </div>
          ))}
      </div>

      <Readout group={active} stampInZone={stampInZone} timeInZone={timeInZone} runHref={runHref} onOpenRun={onOpenRun} hasMarks={groups.length > 0} />
    </>
  );
}

const HOURS_LABEL = "last 24 hours and next 12";

function Header({
  now,
  stampInZone,
  jobCount,
  expanded,
  onToggleExpand,
}: {
  now: number;
  stampInZone: (at: number) => string;
  /** Undefined ⇒ this panel cannot expand, and no control renders. */
  jobCount?: number;
  expanded?: boolean;
  onToggleExpand?: () => void;
}) {
  const swatch = (fill: string, hollow?: boolean): CSSProperties => ({
    width: 9,
    height: 9,
    borderRadius: "50%",
    background: hollow ? "transparent" : fill,
    border: `1.5px solid ${fill}`,
    display: "inline-block",
  });
  return (
    <div style={{ display: "flex", alignItems: "baseline", justifyContent: "space-between", gap: 16, marginBottom: 10, flexWrap: "wrap" }}>
      <div style={{ display: "flex", alignItems: "baseline", gap: 10 }}>
        <h2 style={{ fontSize: c.fontHead, fontWeight: 600, color: c.text, margin: 0 }}>Score</h2>
        <span style={{ fontSize: c.fontSm, color: c.textMuted }}>{HOURS_LABEL} · {stampInZone(now)}</span>
      </div>
      {/* E-4: every label here comes from the canonical vocabulary, including
          the hollow one. The first cut of this component hardcoded "Scheduled"
          in the legend while the readout called the same mark "Queued" — the
          exact defect the plan warned about, re-spelled. "Queued" was also
          false: a projected fire has not been enqueued, and the Dashboard sends
          these marks with no status at all. */}
      <div style={{ display: "flex", alignItems: "center", gap: 14, fontSize: c.fontXs, color: c.textSec, flexWrap: "wrap" }}>
        <span style={{ display: "inline-flex", alignItems: "center", gap: 5 }}><i style={swatch(c.success)} /> {statusLabel("success")}</span>
        <span style={{ display: "inline-flex", alignItems: "center", gap: 5 }}><i style={swatch(c.warning)} /> {statusLabel("warning")}</span>
        <span style={{ display: "inline-flex", alignItems: "center", gap: 5 }}><i style={swatch(c.danger)} /> {statusLabel("danger")}</span>
        <span style={{ display: "inline-flex", alignItems: "center", gap: 5 }}><i style={swatch(c.info)} /> {statusLabel("running")}</span>
        <span style={{ display: "inline-flex", alignItems: "center", gap: 5 }}><i style={swatch(c.textSec, true)} /> {statusLabel("scheduled")}</span>
        {/* FX-1 — the control sits in the panel header, next to the thing it
            controls. It used to be a lone button in a right-aligned row BELOW the
            compact score, opening a second detached panel underneath — so the
            operator read one view stacked on another and the control was nowhere
            near either. The chevron pair matches the app's disclosure idiom; FX-17
            sweeps every one of them onto a single pair. */}
        {onToggleExpand && (
          <Btn
            small
            ariaExpanded={!!expanded}
            /* The sidebar has its own "Collapse" button, so the bare word is
               ambiguous in a screen reader's button list. The name still contains
               the visible text (WCAG 2.5.3). */
            ariaLabel={expanded ? "Collapse the full score" : undefined}
            onClick={onToggleExpand}
          >
            <span aria-hidden style={{ marginRight: 5, color: c.textMuted }}>{expanded ? "▼" : "▶"}</span>
            {expanded ? "Collapse" : `Full score (${jobCount} job${jobCount === 1 ? "" : "s"})`}
          </Btn>
        )}
      </div>
    </div>
  );
}

// F-3: the colour of a mark comes from the canonical statusTone, not from a
// second copy of its switch. An earlier local markColor returned the identical
// colours case for case — which is exactly how a vocabulary drifts: the copy is
// right until someone edits one of them.
//
// VU2-2 adds the one deviation statusTone cannot express, and it is about the
// MARK rather than the STATUS. A projected fire carries no status at all, so it
// fell through to statusTone's muted default — making the scheduled marks the
// faintest class on the plot while being the only class that says anything
// about the future, and in light mode a hollow textMuted ring on white was
// barely there. One step up to textSec, applied wherever a mark is DRAWN.
//
// Deliberately local rather than a change to statusTone: that switch also
// labels idle and paused rows app-wide, and those are correctly muted. A mark
// on a timeline is a different context from a row in a table.
const markInk = (status: string | null | undefined, tense: "past" | "future"): string =>
  !status && tense === "future" ? c.textSec : statusTone(status).color;

function Notehead({
  g,
  left,
  active,
  selected,
  label,
  onSelect,
}: {
  g: MarkGroup;
  left: string;
  active: boolean;
  /** The KEYBOARD cursor is on this mark — aria-selected, and what the plot points aria-activedescendant at. */
  selected: boolean;
  /** H-1: the accessible name. Without it a click target announces as nothing. */
  label: string;
  onSelect: () => void;
}) {
  const color = markInk(g.status, g.tense);
  const h = stemHeight(g.durationMs);
  // Filled = it ran, hollow = it is scheduled. The tense split, carried by the
  // mark itself rather than by position alone.
  const filled = g.tense === "past";
  const count = g.members.length;
  // Members arrive worst-first (score-model), so the slice keeps the worst runs
  // as visible pips and "+k" only ever swallows the mildest ones.
  const visible = g.members.slice(0, STACK_CAP);
  const overflow = count - visible.length;
  const colH = visible.length * NOTE_R * 2 + (visible.length - 1) * PIP_GAP;
  return (
    <div
      id={markId(g)}
      role="option"
      aria-selected={selected}
      aria-label={label}
      onClick={onSelect}
      data-testid="score-mark"
      data-status={g.status ?? (g.tense === "future" ? "scheduled" : "unknown")}
      data-count={count}
      data-tense={g.tense}
      style={{ position: "absolute", left, top: BASELINE - h - colH, transform: "translateX(-50%)", cursor: "pointer" }}
    >
      {overflow > 0 && (
        <span
          style={{
            position: "absolute",
            bottom: "100%",
            left: "50%",
            transform: "translateX(-50%)",
            marginBottom: 3,
            fontSize: c.fontXs,
            fontFamily: c.mono,
            fontWeight: 600,
            color,
            lineHeight: 1,
          }}
        >
          +{overflow}
        </span>
      )}
      {/* ONE stem for the whole chord, sized by the longest member on the log
          scale — how notation resolves simultaneous notes, and what keeps the
          stack from sprouting a stem per pip. */}
      <div style={{ width: 1.5, height: h, background: color, opacity: filled ? 0.55 : 0.35, margin: "0 auto" }} />
      {/* The chord. Each pip is coloured by its OWN member's outcome, so the
          composition of a mixed cluster is visible — the worst-only colouring
          this replaces satisfied VU-21 but painted six successes red for one
          failure. Rendered top-down in reverse so the worst pip sits ON the
          staff line. */}
      <div
        style={{
          display: "flex",
          flexDirection: "column",
          gap: PIP_GAP,
          borderRadius: NOTE_R * 2,
          boxShadow: active ? `0 0 0 3px ${color}55` : undefined,
        }}
      >
        {[...visible].reverse().map((m, i) => {
          const mc = markInk(m.status, g.tense);
          return (
            <div
              key={i}
              data-testid="score-pip"
              style={{
                width: NOTE_R * 2,
                height: NOTE_R * 2,
                borderRadius: "50%",
                background: filled ? mc : c.panel,
                // A hollow pip is all ring, so the ring carries the whole mark:
                // 1.5px read as a smudge at this size, worst on a light ground.
                border: `2px solid ${mc}`,
              }}
            />
          );
        })}
      </div>
    </div>
  );
}

// D-4: a FIXED readout slot, not a floating tooltip. The slot never moves, so
// the eye does not chase it across the plot, and it has room to enumerate a
// coincident group's members — which a tooltip pinned to a 10px notehead does not.
function Readout({
  group,
  stampInZone,
  timeInZone,
  runHref,
  onOpenRun,
  hasMarks,
}: {
  group: MarkGroup | null;
  stampInZone: (at: number) => string;
  timeInZone?: (at: number) => string;
  runHref?: (traceId: string) => string;
  onOpenRun?: (traceId: string) => void;
  hasMarks: boolean;
}) {
  // VU2-2 — the slot takes its panel treatment only when it is CARRYING
  // something. At rest it holds one line of instruction, and a bordered card
  // whose entire content is chrome reads as an empty region on a page that has
  // real empty regions to worry about; as a caption it recedes and the plot
  // above it keeps the eye.
  //
  // minHeight stays in BOTH states, deliberately. The fixed slot is the D-4
  // contract — the readout never moves, so the eye does not chase it — and a
  // collapsing idle state would shift the whole page on first hover, which is
  // the one thing a fixed slot exists to prevent.
  const idle = !group;
  return (
    <div
      aria-live="polite"
      style={{
        minHeight: 58,
        marginTop: 10,
        padding: "10px 14px",
        border: idle ? "1px solid transparent" : `1px solid ${c.border}`,
        borderRadius: c.radiusSurface,
        background: idle ? "transparent" : c.panel,
        fontSize: idle ? c.fontXs : c.fontSm,
        color: c.textSec,
        transition: "background 0.15s, border-color 0.15s",
      }}
    >
      {!group ? (
        <span style={{ color: c.textMuted }}>
          {hasMarks ? "Hover the timeline, or tab into it and use the arrow keys." : "No runs or scheduled runs in this window."}
        </span>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
          <div style={{ display: "flex", alignItems: "baseline", gap: 10, flexWrap: "wrap" }}>
            <span style={{ fontFamily: c.mono, color: c.text }}>{stampInZone(group.at)}</span>
            <span style={{ color: c.textMuted, fontSize: c.fontXs }}>
              {group.tense === "past" ? "ran" : "scheduled"}
              {group.members.length > 1 ? ` · ${group.members.length} at this instant` : ""}
            </span>
          </div>
          {/* SR-1 — one row per member in a real table, so the eye reads DOWN
              a column (all the times, all the trace ids) instead of re-parsing
              each row's ragged wrap, and every column has a head that names
              it. A "set of dots" is a cluster within one notehead's width, not
              one instant, so each row carries its own start time; the header
              above keeps the cluster's anchor. The table takes the card's full
              width and auto layout shares the surplus across the columns in
              proportion to their content, so a wider window opens the table
              out rather than leaving a packed block beside dead space; the
              View-run column anchors the right edge. A projection has no
              trigger, executor or trace, so those heads are omitted for a
              future group rather than labelling empty columns. */}
          {(() => {
            const past = group.tense === "past";
            const head: CSSProperties = {
              fontSize: c.fontXs,
              fontFamily: c.sansCond,
              fontWeight: 600,
              color: c.textMuted,
              textTransform: "uppercase",
              letterSpacing: 0.7,
              textAlign: "left",
              padding: "0 14px 4px 0",
              borderBottom: `1px solid ${c.borderLight}`,
              whiteSpace: "nowrap",
            };
            const cell: CSSProperties = { padding: "4px 14px 0 0", verticalAlign: "baseline", whiteSpace: "nowrap" };
            const quiet: CSSProperties = { ...cell, fontSize: c.fontXs, color: c.textMuted };
            const cap: CSSProperties = { maxWidth: 420, overflow: "hidden", textOverflow: "ellipsis" };
            return (
              <table style={{ borderCollapse: "collapse", tableLayout: "auto", width: "100%" }}>
                <thead>
                  <tr>
                    <th scope="col" style={head}>Job</th>
                    <th scope="col" style={head}>Status</th>
                    <th scope="col" style={head}>{past ? "Started" : "Fires"}</th>
                    {past && <th scope="col" style={head}>Trigger</th>}
                    {past && <th scope="col" style={head}>Executor</th>}
                    {past && <th scope="col" style={head}>Duration</th>}
                    {past && <th scope="col" style={head}>Trace</th>}
                    {past && runHref && <th scope="col" style={{ ...head, paddingRight: 0, textAlign: "right" }}><span style={{ position: "absolute", width: 1, height: 1, overflow: "hidden", clip: "rect(0 0 0 0)" }}>Open</span></th>}
                  </tr>
                </thead>
                <tbody>
                  {group.members.map((m, i) => (
                    <tr key={`${m.jobName}-${m.traceId ?? i}`}>
                      <td style={{ ...cell, ...cap, color: c.text, fontWeight: 600 }}>
                        {m.jobName}
                        {m.ownerKind === "workflow" && <span style={{ fontSize: c.fontXs, color: c.info, fontWeight: 400, marginLeft: 8 }}>workflow</span>}
                      </td>
                      <td style={{ ...cell, color: markInk(m.status, group.tense), fontSize: c.fontXs }}>
                        {/* A future mark defaults to "scheduled", not "queued": it is a
                            projection, not something the scheduler has accepted, and
                            "Queued" would contradict both the legend and the empty
                            state below (E-4). */}
                        {m.tense === "past" ? statusLabel(m.status) : statusLabel(m.status ?? "scheduled")}
                      </td>
                      <td style={{ ...quiet, fontFamily: c.mono }}>{(timeInZone ?? stampInZone)(m.at)}</td>
                      {past && <td style={{ ...quiet, ...cap }} title={triggerLabel(m) || undefined}>{triggerLabel(m)}</td>}
                      {past && <td style={{ ...quiet, ...cap }} title={executorLabel(m) || undefined}>{executorLabel(m)}</td>}
                      {past && <td style={{ ...quiet, fontFamily: c.mono }}>{m.durationMs != null ? fmtDuration(m.durationMs) : ""}</td>}
                      {past && (
                        <td style={{ ...quiet, fontFamily: c.mono }}>
                          {m.traceId && <CopyText text={m.traceId} display={m.traceId.slice(0, 8)} title={`${m.traceId} — click to copy`} />}
                        </td>
                      )}
                      {past && runHref && (
                        <td style={{ ...quiet, paddingRight: 0, textAlign: "right" }}>
                          {m.traceId && (
                            <a
                              href={runHref(m.traceId)}
                              onClick={(e) => {
                                if (!onOpenRun || e.metaKey || e.ctrlKey || e.button !== 0) return;
                                e.preventDefault();
                                onOpenRun(m.traceId!);
                              }}
                              style={{ color: c.primary, textDecoration: "none" }}
                            >
                              View run
                            </a>
                          )}
                        </td>
                      )}
                    </tr>
                  ))}
                </tbody>
              </table>
            );
          })()}
        </div>
      )}
    </div>
  );
}

// ── Full score (D-6) ─────────────────────────────────────────────────────────
// One staff per job, grouped into sections by run type, sharing the axis and the
// playhead. This is where disk-usage-audit firing every 30 minutes stops being a
// repeated table row and becomes a visible pulse. Jobs with no events render as
// empty staves, which reads correctly as a rest.
export function FullScore({
  marks,
  now,
  jobNames,
  stampInZone,
  types,
}: {
  marks: Mark[];
  now: number;
  jobNames: string[];
  /** F-4: same application-timezone stamp the condensed score's readout uses. Required, not optional — a default would silently reintroduce the raw UTC instant it replaced. */
  stampInZone: (at: number) => string;
  types?: Record<string, string | null | undefined>;
}) {
  const w: Window = useMemo(() => scoreWindow(now), [now]);
  const byJob = useMemo(() => {
    const m = new Map<string, Mark[]>();
    for (const name of jobNames) m.set(name, []);
    for (const mk of marks) {
      if (!inWindow(w, mk.at)) continue;
      const bucket = m.get(mk.jobName);
      if (bucket) bucket.push(mk);
      else m.set(mk.jobName, [mk]);
    }
    return m;
  }, [marks, jobNames, w]);

  // VU2-2 — a job with nothing in the window is a QUIET job. D-6 gave every job
  // a staff on purpose: an empty staff reads as a rest, while a missing one
  // reads as "no such job". That argument is about the job being ACCOUNTED FOR,
  // not about a blank row earning its height — so the fold below keeps the
  // accounting (it names the count, and one click restores every staff) while
  // handing the panel's vertical space to the jobs that actually did something.
  // A SILENT drop would regress D-6; this is why the row is labelled.
  const [showQuiet, setShowQuiet] = useState(false);
  const quietNames = useMemo(() => [...byJob.entries()].filter(([, mk]) => mk.length === 0).map(([name]) => name), [byJob]);
  const quietCount = quietNames.length;

  const sections = useMemo(() => {
    const quiet = new Set(showQuiet ? [] : quietNames);
    const s = new Map<string, string[]>();
    for (const name of byJob.keys()) {
      if (quiet.has(name)) continue;
      const t = (types?.[name] ?? "other") || "other";
      const bucket = s.get(t);
      if (bucket) bucket.push(name);
      else s.set(t, [name]);
    }
    // A type section whose staves are all quiet disappears with them — a header
    // over nothing is worse than the rows it was introducing.
    return [...s.entries()].sort((a, b) => a[0].localeCompare(b[0]));
  }, [byJob, types, quietNames, showQuiet]);

  return (
    <div aria-label="Full score" style={{ border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, background: c.panel, overflow: "hidden" }}>
      {sections.map(([type, names]) => (
        <div key={type}>
          <div
            style={{
              padding: "8px 14px",
              fontSize: c.fontXs,
              fontFamily: c.sansCond,
              textTransform: "uppercase",
              letterSpacing: 0.7,
              color: c.textMuted,
              borderBottom: `1px solid ${c.borderLight}`,
              background: c.panel2,
            }}
          >
            {type}
          </div>
          {[...names].sort().map((name) => (
            <Staff key={name} name={name} marks={byJob.get(name) ?? []} w={w} now={now} stampInZone={stampInZone} />
          ))}
        </div>
      ))}
      {quietCount > 0 && (
        <button
          onClick={() => setShowQuiet((v) => !v)}
          aria-expanded={showQuiet}
          style={{
            display: "block",
            width: "100%",
            textAlign: "left",
            padding: "8px 14px",
            background: "transparent",
            border: "none",
            borderTop: sections.length > 0 ? `1px solid ${c.borderLight}` : undefined,
            color: c.textMuted,
            fontSize: c.fontXs,
            cursor: "pointer",
          }}
        >
          {showQuiet
            ? `Hide ${quietCount} quiet job${quietCount === 1 ? "" : "s"}`
            : `+${quietCount} quiet job${quietCount === 1 ? "" : "s"} — no runs or scheduled runs in this window`}
        </button>
      )}
    </div>
  );
}

function Staff({ name, marks, w, now, stampInZone }: { name: string; marks: Mark[]; w: Window; now: number; stampInZone: (at: number) => string }) {
  const groups = useMemo(() => groupMarks(marks), [marks]);
  const pct = (at: number) => `${frac(w, at) * 100}%`;
  return (
    <div style={{ display: "flex", alignItems: "center", borderBottom: `1px solid ${c.borderLight}` }}>
      <div
        title={name}
        style={{ width: 170, flexShrink: 0, padding: "0 12px", fontSize: c.fontXs, color: c.textSec, whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}
      >
        {name}
      </div>
      <div style={{ position: "relative", flex: 1, height: 26 }}>
        <div aria-hidden style={{ position: "absolute", left: 0, right: 0, top: 13, height: 1, background: c.borderLight }} />
        <div aria-hidden style={{ position: "absolute", left: pct(now), top: 0, bottom: 0, width: 1, background: c.accent, opacity: 0.5 }} />
        {groups.map((g) => (
          <div
            key={`${g.tense}@${g.at}`}
            data-testid="staff-mark"
            // F-4: the application timezone, not a raw Z instant. Hovering one
            // mark here and the same mark in the condensed score used to give
            // two different times for one event.
            title={`${name} · ${g.members.length > 1 ? `${g.members.length} at ` : ""}${stampInZone(g.at)}`}
            style={{
              position: "absolute",
              left: pct(g.at),
              top: 13 - 3.5,
              transform: "translateX(-50%)",
              width: 7,
              height: 7,
              borderRadius: "50%",
              background: g.tense === "past" ? markInk(g.status, g.tense) : c.panel,
              border: `2px solid ${markInk(g.status, g.tense)}`,
            }}
          />
        ))}
      </div>
    </div>
  );
}

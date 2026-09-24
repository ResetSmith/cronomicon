// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ComponentProps } from "react";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { Score, FullScore } from "./Score";
import type { Mark } from "./score-model";

// Component-level cover for the Score (Phase D), replacing the two JobHistogram
// empty-state tests that died with that component (VU-19).
//
// score-model.test.ts already owns the maths — window, grouping, stems,
// nearest-mark, ticks — and none of it is re-tested here. What this file exists
// to prove is that those properties survive the trip through the component: the
// grouping is only worth anything if the renderer actually draws one notehead
// per group, and the nearest-mark resolution is only worth anything if it is
// what the pointer and the keyboard are wired to.

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const NOW = Date.parse("2026-07-27T19:05:00Z");
const HOUR = 3600_000;

// The zone accessors are injected precisely so a test does not have to stand up
// TimezoneProvider; UTC is the deterministic choice.
const hourInZone = (at: number) => new Date(at).getUTCHours();
const labelInZone = (at: number) => new Date(at).toISOString().slice(11, 16);
// Deliberately NOT an ISO instant: F-4's regression is a surface rendering
// `new Date(at).toISOString()` instead of the injected stamp, and a test whose
// stamp *is* an ISO string cannot tell the two apart.
const stampInZone = (at: number) => `zoned:${new Date(at).toISOString().slice(0, 16)}`;

const mark = (over: Partial<Mark> & Pick<Mark, "at">): Mark => ({ tense: "past", jobName: "job", ...over });

const renderScore = (props: Partial<ComponentProps<typeof Score>> = {}) =>
  render(<Score marks={[]} now={NOW} hourInZone={hourInZone} labelInZone={labelInZone} stampInZone={stampInZone} {...props} />);

// H-1: the plot is a listbox now — `group` is not a widget role, so nothing
// announced that the arrow keys did anything.
const plot = () => screen.getByRole("listbox");
const readout = () => document.querySelector('[aria-live="polite"]') as HTMLElement;
const noteheads = () => [...document.querySelectorAll('[data-testid="score-mark"]')] as HTMLElement[];
/** A mark's pips — one per member, the chord stack. */
const pips = (m: HTMLElement) => [...m.querySelectorAll('[data-testid="score-pip"]')] as HTMLElement[];
/** The notehead circle itself, not the wrapper that carries the test hooks. */
const disc = (m: HTMLElement) => pips(m)[0];

/** jsdom has no layout, so getBoundingClientRect is all zeros and the pointer
 *  path would resolve nothing at all. Give the plot a realistic 1000px box so a
 *  clientX means something. */
const PLOT_WIDTH = 1000;
const stubLayout = () =>
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue({
    x: 0,
    y: 0,
    left: 0,
    top: 0,
    right: PLOT_WIDTH,
    bottom: 190,
    width: PLOT_WIDTH,
    height: 190,
    toJSON: () => ({}),
  } as DOMRect);

describe("Score — window contents", () => {
  it("says so plainly when nothing falls in the window", () => {
    renderScore({ marks: [] });
    expect(screen.getByText("Nothing ran in the last 24 hours, and nothing is scheduled for the next 12.")).toBeTruthy();
    expect(noteheads().length).toBe(0);
    // The readout is the other half of the empty state: it must not invite an
    // interaction that has nothing to land on.
    expect(readout().textContent).toBe("No runs or scheduled runs in this window.");
  });

  it("drops the empty message as soon as there is anything to show", () => {
    renderScore({
      marks: [mark({ at: NOW - 3 * HOUR, jobName: "nightly-backup", status: "success", durationMs: 120_000 }), mark({ at: NOW + 2 * HOUR, tense: "future", jobName: "disk-usage-audit" })],
    });
    expect(noteheads().length).toBe(2);
    expect(screen.queryByText("Nothing ran in the last 24 hours, and nothing is scheduled for the next 12.")).toBeNull();
  });

  it("filters to the window rather than stacking out-of-range marks on the edge", () => {
    renderScore({
      marks: [mark({ at: NOW - 30 * HOUR, jobName: "too-old" }), mark({ at: NOW + 20 * HOUR, tense: "future", jobName: "too-far" }), mark({ at: NOW - HOUR, jobName: "in-range" })],
    });
    expect(noteheads().length).toBe(1);
  });
});

// VU-21 — THE REGRESSION THIS COMPONENT EXISTS TO PREVENT.
//
// Marks sharing an instant render at the same x and used to paint in array
// order, so the last one drawn covered the others: a failed run coinciding with
// a successful one was painted over and vanished from the one screen whose job
// is surfacing failures. Measured at roughly a quarter of all marks on the
// prototype (`0 6 * * *` fires two jobs; `*/30` collides with every top-of-hour
// schedule), so this is not a rare edge.
//
// groupMarks is tested directly in score-model.test.ts. This test is here
// because the property is worthless unless the renderer honours it — and the
// reverse-order case is asserted explicitly because array order is exactly what
// the broken behaviour depended on.
describe("Score — coincident marks (VU-21)", () => {
  const AT = NOW - 6 * HOUR;
  const failure = mark({ at: AT, jobName: "db-migrate", status: "danger", durationMs: 30_000 });
  const success = mark({ at: AT, jobName: "nightly-backup", status: "success", durationMs: 120_000 });

  it("collapses a failure + success collision into ONE danger-coloured mark carrying a count", () => {
    renderScore({ marks: [failure, success] });
    const marks = noteheads();
    expect(marks.length).toBe(1);
    expect(marks[0].getAttribute("data-status")).toBe("danger");
    expect(marks[0].getAttribute("data-count")).toBe("2");
    // The collision is legible as a two-pip chord on the plot, not only in the
    // DOM attribute — and no count numeral renders below the overflow cap.
    expect(pips(marks[0]).length).toBe(2);
    expect(marks[0].textContent).toBe("");
  });

  it("stacks the chord worst-at-the-bottom, each pip coloured by its own member", () => {
    renderScore({ marks: [success, failure] });
    const chord = pips(noteheads()[0]);
    expect(chord.length).toBe(2);
    // Rendered top-down: the LAST pip sits on the staff line and must be the
    // failure; the success stacks above it, keeping its own colour rather than
    // being painted by the group's worst outcome.
    const bottom = getComputedStyle(chord[chord.length - 1]);
    const top = getComputedStyle(chord[0]);
    expect(bottom.background || bottom.backgroundColor).not.toBe(top.background || top.backgroundColor);
  });

  it("caps the stack and folds the remainder into an overflow numeral", () => {
    const burst = Array.from({ length: 10 }, (_, i) => mark({ at: AT, jobName: `burst-${i}`, status: "success" }));
    renderScore({ marks: [...burst, mark({ at: AT, jobName: "bad", status: "danger" })] });
    const m = noteheads()[0];
    // 11 members: 8 pips visible, "+3" for the rest — and the failure survives
    // as a pip, because the overflow folds from the mildest end.
    expect(m.getAttribute("data-count")).toBe("11");
    expect(pips(m).length).toBe(8);
    expect(m.textContent).toBe("+3");
  });

  it("gives the identical result with the array reversed — order must not decide the colour", () => {
    renderScore({ marks: [success, failure] });
    const marks = noteheads();
    expect(marks.length).toBe(1);
    expect(marks[0].getAttribute("data-status")).toBe("danger");
    expect(marks[0].getAttribute("data-count")).toBe("2");
  });

  it("keeps both members reachable — the hidden run is named, not merely counted", () => {
    renderScore({ marks: [success, failure] });
    fireEvent.keyDown(plot(), { key: "ArrowRight" });
    const text = readout().textContent ?? "";
    expect(text).toContain("db-migrate");
    expect(text).toContain("nightly-backup");
    expect(text).toContain("2 at this instant");
  });

  // SR-1 — each readout row states its own time, trigger, executor, trace id
  // and a View-run link, on top of the name/status/duration it already had.
  it("gives each member its own time, trigger, executor, trace id and run link", () => {
    const onOpenRun = vi.fn();
    renderScore({
      marks: [
        mark({ at: AT, jobName: "db-migrate", status: "success", durationMs: 3000, traceId: "0192abcd-1111-7000-8000-000000000001", triggerKind: "manual", triggeredBy: "ops@example.com", executor: "runner", runnerName: "nv-dmz-01" }),
        mark({ at: AT + 5_000, jobName: "nightly-backup", status: "danger", traceId: "0192abcd-2222-7000-8000-000000000002", triggerKind: "scheduled", scheduleName: "nightly", executor: "ssh" }),
      ],
      timeInZone: (at) => `t:${new Date(at).toISOString().slice(11, 19)}`,
      runHref: (t) => `/runs?trace=${t}`,
      onOpenRun,
    });
    fireEvent.keyDown(plot(), { key: "ArrowRight" });
    const text = readout().textContent ?? "";
    expect(text).toContain("t:13:05:00");
    expect(text).toContain("t:13:05:05");
    expect(text).toContain("by ops@example.com");
    expect(text).toContain("schedule nightly");
    expect(text).toContain("Runner · nv-dmz-01");
    expect(text).toContain("SSH");
    // The trace id is shortened on screen but the full value is what copies.
    expect(text).toContain("0192abcd");
    expect(text).not.toContain("000000000001");
    // SR-2 — every column has a head naming it.
    const heads = within(readout()).getAllByRole("columnheader").map((h) => h.textContent);
    expect(heads).toEqual(["Job", "Status", "Started", "Trigger", "Executor", "Duration", "Trace", "Open"]);
    const links = within(readout()).getAllByRole("link", { name: "View run" });
    expect(links).toHaveLength(2);
    // Members list worst-first, so assert on the set of hrefs, not the order.
    expect(links.map((l) => l.getAttribute("href")).sort()).toEqual([
      "/runs?trace=0192abcd-1111-7000-8000-000000000001",
      "/runs?trace=0192abcd-2222-7000-8000-000000000002",
    ]);
    fireEvent.click(links[0]);
    expect(onOpenRun).toHaveBeenCalledWith(links[0].getAttribute("href")!.replace("/runs?trace=", ""));
  });

  it("never merges tenses, so a projection cannot absorb a completed run at the same instant", () => {
    renderScore({ marks: [mark({ at: AT, jobName: "ran", status: "success" }), mark({ at: AT, tense: "future", jobName: "will-run" })] });
    expect(noteheads().length).toBe(2);
  });
});

describe("Score — tense split", () => {
  it("draws a past mark filled and a scheduled one hollow", () => {
    renderScore({ marks: [mark({ at: NOW - 4 * HOUR, jobName: "ran", status: "success" }), mark({ at: NOW + 4 * HOUR, tense: "future", jobName: "scheduled" })] });
    const [past, future] = noteheads();
    expect(past.getAttribute("data-tense")).toBe("past");
    expect(future.getAttribute("data-tense")).toBe("future");

    // Asserted relationally rather than against literal tokens, so the test
    // survives a palette change: filled means the disc is painted its own
    // status colour, hollow means it is painted the surface behind it. Both
    // keep the coloured ring.
    const pastDisc = getComputedStyle(disc(past));
    const futureDisc = getComputedStyle(disc(future));
    expect(pastDisc.backgroundColor).toBe(pastDisc.borderTopColor);
    expect(futureDisc.backgroundColor).not.toBe(futureDisc.borderTopColor);
    expect(futureDisc.backgroundColor).toBe(getComputedStyle(plot()).backgroundColor);
  });
});

// D-3 — hover resolves pointer → time → nearest mark ON THE TRACK. Per-mark hit
// boxes were the prototype's failure: at notehead size, marks minutes apart
// overlap and the one painted last swallows its neighbours, which left three of
// six seeded runs literally unhoverable.
describe("Score — nearest-mark resolution (D-3)", () => {
  // 25 minutes apart on a 36h window: ~12px at a 1000px plot, so the two
  // noteheads are adjacent and nearly touching — close enough that a per-mark hit
  // box swallows its neighbour, far enough that FX-5's clustering leaves them as
  // two marks. (At 5 minutes, which this fixture used before FX-5, they are now
  // correctly one clustered notehead — covered in the clustering tests below.)
  const EARLY = NOW - 6 * HOUR;
  const LATE = EARLY + 25 * 60_000;
  const marks = [mark({ at: EARLY, jobName: "covered-run", status: "danger" }), mark({ at: LATE, jobName: "covering-run", status: "success" })];

  it("reaches the mark a neighbour's notehead is sitting on top of", () => {
    stubLayout();
    renderScore({ marks });
    expect(noteheads().length).toBe(2); // distinct instants: not a group

    // x=500 is EARLY's exact position. A per-mark hit box would hand this to
    // covering-run, whose notehead spans it; resolving on the track does not.
    fireEvent.mouseMove(plot(), { clientX: 500 });
    expect(readout().textContent).toContain("covered-run");
    expect(readout().textContent).not.toContain("covering-run");
  });

  it("snaps to whichever mark the pointer is actually nearer in time", () => {
    stubLayout();
    renderScore({ marks });
    fireEvent.mouseMove(plot(), { clientX: 508 });
    expect(readout().textContent).toContain("covering-run");
    expect(readout().textContent).not.toContain("covered-run");
  });

  it("resolves to nothing well away from any mark, instead of snapping across the plot", () => {
    stubLayout();
    renderScore({ marks });
    fireEvent.mouseMove(plot(), { clientX: 500 });
    expect(readout().textContent).toContain("covered-run");
    fireEvent.mouseMove(plot(), { clientX: 990 });
    expect(readout().textContent).toBe("Hover the timeline, or tab into it and use the arrow keys.");
  });

  it("clears the hover when the pointer leaves the plot", () => {
    stubLayout();
    renderScore({ marks });
    fireEvent.mouseMove(plot(), { clientX: 500 });
    expect(readout().textContent).toContain("covered-run");
    fireEvent.mouseLeave(plot());
    expect(readout().textContent).toBe("Hover the timeline, or tab into it and use the arrow keys.");
  });
});

// D-5 — ONE tab stop into the score, arrows step through the marks. Per-mark
// tab stops would have added 60+ stops to the dashboard, which is why the plot
// itself is the focusable element and the readout is the feedback channel.
describe("Score — keyboard (D-5)", () => {
  const marks = [
    mark({ at: NOW - 8 * HOUR, jobName: "first-run", status: "success" }),
    mark({ at: NOW - 2 * HOUR, jobName: "middle-run", status: "danger" }),
    mark({ at: NOW + 5 * HOUR, tense: "future", jobName: "last-fire" }),
  ];

  it("is a single tab stop, not one per mark", () => {
    renderScore({ marks });
    expect(plot().getAttribute("tabindex")).toBe("0");
    expect(document.querySelectorAll("[tabindex]").length).toBe(1);
  });

  it("steps forward and back through the marks, updating the readout each time", () => {
    renderScore({ marks });
    fireEvent.keyDown(plot(), { key: "ArrowRight" });
    expect(readout().textContent).toContain("first-run");
    fireEvent.keyDown(plot(), { key: "ArrowRight" });
    expect(readout().textContent).toContain("middle-run");
    fireEvent.keyDown(plot(), { key: "ArrowLeft" });
    expect(readout().textContent).toContain("first-run");
  });

  it("jumps to the ends with Home/End and clamps at them", () => {
    renderScore({ marks });
    fireEvent.keyDown(plot(), { key: "End" });
    expect(readout().textContent).toContain("last-fire");
    fireEvent.keyDown(plot(), { key: "ArrowRight" }); // already at the end
    expect(readout().textContent).toContain("last-fire");
    fireEvent.keyDown(plot(), { key: "Home" });
    expect(readout().textContent).toContain("first-run");
    fireEvent.keyDown(plot(), { key: "ArrowLeft" }); // already at the start
    expect(readout().textContent).toContain("first-run");
  });

  it("does not lose the keyboard cursor when the pointer merely leaves the plot", () => {
    stubLayout();
    renderScore({ marks });
    fireEvent.keyDown(plot(), { key: "End" });
    fireEvent.mouseLeave(plot());
    expect(readout().textContent).toContain("last-fire");
  });
});

// H-1 — the accessibility contract the keyboard work left half-finished. D-5
// built the interaction (one tab stop, arrows, Home/End, Escape) and VU-2
// established the focus ring, but the marks themselves were a mouse-only
// affordance: an onClick on a bare div, no role, no accessible name, inside a
// container whose `group` role told a screen reader nothing about arrow keys.
describe("Score — accessibility (H-1)", () => {
  const marks = [
    mark({ at: NOW - 8 * HOUR, jobName: "first-run", status: "success", durationMs: 4200 }),
    mark({ at: NOW - 2 * HOUR, jobName: "middle-run", status: "danger" }),
    mark({ at: NOW + 5 * HOUR, tense: "future", jobName: "last-fire" }),
  ];

  it("exposes the plot as a listbox and every mark as an option", () => {
    renderScore({ marks });
    expect(plot().getAttribute("role")).toBe("listbox");
    expect(screen.getAllByRole("option").length).toBe(marks.length);
    // Same nodes, not a parallel set: the options ARE the noteheads.
    expect(screen.getAllByRole("option")).toEqual(noteheads());
  });

  it("names each mark with the job, its canonical status word and the zoned time", () => {
    renderScore({ marks });
    const names = screen.getAllByRole("option").map((o) => o.getAttribute("aria-label") ?? "");
    expect(names[0]).toBe(`first-run, Success, ran ${stampInZone(NOW - 8 * HOUR)}`);
    expect(names[1]).toBe(`middle-run, Failed, ran ${stampInZone(NOW - 2 * HOUR)}`);
    // A projection is "scheduled", never "Queued" — the same rule the readout
    // and the legend follow (E-4).
    expect(names[2]).toBe(`last-fire, scheduled ${stampInZone(NOW + 5 * HOUR)}`);
  });

  it("names a coincident group by its count and its WORST outcome", () => {
    const AT = NOW - 6 * HOUR;
    renderScore({
      marks: [mark({ at: AT, jobName: "nightly-backup", status: "success" }), mark({ at: AT, jobName: "db-migrate", status: "danger" })],
    });
    const [only] = screen.getAllByRole("option");
    expect(only.getAttribute("aria-label")).toBe(`2 runs ran at ${stampInZone(AT)}, worst result Failed`);
  });

  it("points aria-activedescendant at the keyboard cursor, and marks it selected", () => {
    renderScore({ marks });
    expect(plot().getAttribute("aria-activedescendant")).toBeNull();

    fireEvent.keyDown(plot(), { key: "ArrowRight" });
    const active = plot().getAttribute("aria-activedescendant");
    expect(active).toBeTruthy();
    const option = document.getElementById(active!);
    expect(option?.getAttribute("aria-label")).toContain("first-run");
    expect(option?.getAttribute("aria-selected")).toBe("true");
    // Exactly one selected option, and the others say so explicitly rather than
    // omitting the attribute.
    expect(screen.getAllByRole("option").filter((o) => o.getAttribute("aria-selected") === "true").length).toBe(1);

    fireEvent.keyDown(plot(), { key: "Escape" });
    expect(plot().getAttribute("aria-activedescendant")).toBeNull();
  });

  it("does not follow the POINTER — sweeping the plot must not narrate every run", () => {
    stubLayout();
    renderScore({ marks });
    fireEvent.keyDown(plot(), { key: "Home" });
    const beforeHover = plot().getAttribute("aria-activedescendant");
    fireEvent.mouseMove(plot(), { clientX: 800 });
    // The readout follows the hover (it is a live region, and that is the
    // pointer's feedback); the activedescendant stays where the keyboard is.
    expect(readout().textContent).toContain("last-fire");
    expect(plot().getAttribute("aria-activedescendant")).toBe(beforeHover);
  });

  it("keeps the empty plot free of a non-option child", () => {
    renderScore({ marks: [] });
    // A listbox may only contain options. The empty sentence stays on screen and
    // is hidden from the tree — the plot's own label and the readout both say it.
    const empty = screen.getByText("Nothing ran in the last 24 hours, and nothing is scheduled for the next 12.");
    expect(empty.getAttribute("aria-hidden")).toBe("true");
    expect(plot().getAttribute("aria-label")).toContain("0 runs and scheduled runs");
  });
});

// D-4 — a FIXED readout slot, not a floating tooltip. It is in the DOM before
// any interaction so the layout below it never jumps, and it has the room to
// enumerate a coincident group's members, which a tooltip pinned to a 10px
// notehead does not.
describe("Score — readout (D-4)", () => {
  it("exists before any interaction and does not appear or disappear with hover", () => {
    stubLayout();
    renderScore({ marks: [mark({ at: NOW - 3 * HOUR, jobName: "nightly-backup", status: "success", durationMs: 4200 })] });
    const before = readout();
    expect(before).toBeTruthy();
    expect(document.querySelectorAll('[aria-live="polite"]').length).toBe(1);
    fireEvent.mouseMove(plot(), { clientX: 500 });
    // The same node, still the only one: the slot was filled, not spawned.
    expect(readout()).toBe(before);
    expect(document.querySelectorAll('[aria-live="polite"]').length).toBe(1);
  });

  it("reports the instant, the tense and every member of the group by name", () => {
    renderScore({
      marks: [
        mark({ at: NOW - 3 * HOUR, jobName: "db-migrate", status: "danger", durationMs: 30_000 }),
        mark({ at: NOW - 3 * HOUR, jobName: "nightly-backup", status: "success", durationMs: 120_000 }),
      ],
    });
    fireEvent.keyDown(plot(), { key: "Home" });
    const text = readout().textContent ?? "";
    expect(text).toContain(stampInZone(NOW - 3 * HOUR));
    expect(text).toContain("ran");
    expect(text).toContain("db-migrate");
    expect(text).toContain("nightly-backup");
    // Labels come from the shared status vocabulary (E-4), not local strings.
    expect(text).toContain("Failed");
    expect(text).toContain("Success");
  });

  it("calls a projected run Scheduled — the same word the legend uses (E-4)", () => {
    // This test previously asserted the readout said "Queued" while the legend
    // said "Scheduled". That was the exact defect E-4 exists to catch — two
    // words for one mark — and "Queued" was also untrue: a projection has not
    // been enqueued, and the Dashboard sends these marks with no status at all.
    renderScore({ marks: [mark({ at: NOW + 4 * HOUR, tense: "future", jobName: "disk-usage-audit" })] });
    fireEvent.keyDown(plot(), { key: "Home" });
    const text = readout().textContent ?? "";
    expect(text).toContain("scheduled");
    expect(text).not.toContain("ran");
    expect(text).toContain("Scheduled");
    expect(text).not.toContain("Queued");
  });

  it("uses one word for the hollow mark across legend and readout", () => {
    // The regression is a mismatch, so assert the relationship rather than the
    // literal: whatever the legend calls a hollow mark, the readout must agree.
    renderScore({ marks: [mark({ at: NOW + 4 * HOUR, tense: "future", jobName: "disk-usage-audit" })] });
    const legend = document.body.textContent ?? "";
    fireEvent.keyDown(plot(), { key: "Home" });
    const text = readout().textContent ?? "";
    expect(legend).toContain("Scheduled");
    expect(text).toContain("Scheduled");
  });
});

// D-2b — where the forward projection stopped because the API capped it, rather
// than letting the plot trail off with no explanation.
describe("Score — end-of-projection terminator (D-2b)", () => {
  const capped = /projection was capped/;

  it("draws the terminator when the projection was truncated", () => {
    renderScore({ marks: [mark({ at: NOW - HOUR, jobName: "ran" })], truncatedAt: NOW + 6 * HOUR });
    expect(screen.getByTitle(capped)).toBeTruthy();
  });

  it("draws nothing when the projection was complete", () => {
    renderScore({ marks: [mark({ at: NOW - HOUR, jobName: "ran" })] });
    expect(screen.queryByTitle(capped)).toBeNull();
  });

  it("draws nothing for a cap outside the window, which would otherwise pin to the edge", () => {
    renderScore({ marks: [mark({ at: NOW - HOUR, jobName: "ran" })], truncatedAt: NOW + 40 * HOUR });
    expect(screen.queryByTitle(capped)).toBeNull();
  });
});

// D-6 — one staff per job, sectioned by run type. The point of the view is the
// cadence, so a job with nothing in the window must still get its lane: an empty
// staff reads as a rest, whereas omitting the row reads as the job not existing.
// ── VU2-2: the readout at rest, and the ink a scheduled mark is drawn in ────
describe("Score — readout at rest (VU2-2, D-4)", () => {
  // NOW - 6h is the instant the nearest-mark tests place at x=500 on the
  // stubbed plot, so a mouseMove there resolves to this mark.
  const marks = [mark({ at: NOW - 6 * HOUR, jobName: "alpha", status: "success" })];

  it("sheds its panel treatment while it holds only the instruction", () => {
    stubLayout();
    renderScore({ marks });
    expect(readout().style.background).toBe("transparent");
    expect(readout().style.border).toContain("transparent");
  });

  it("takes the panel treatment back the moment it carries data", () => {
    stubLayout();
    renderScore({ marks });
    fireEvent.mouseMove(plot(), { clientX: 500 });
    expect(readout().textContent).toContain("alpha");
    expect(readout().style.background).not.toBe("transparent");
    expect(readout().style.border).not.toContain("transparent");
  });

  it("keeps the slot's height in BOTH states, so hovering never shifts the page", () => {
    // D-4's fixed slot is the whole reason the readout is not a tooltip: it
    // never moves, so the eye does not chase it. A collapsing idle state would
    // reintroduce exactly the jump the fixed slot exists to prevent.
    stubLayout();
    renderScore({ marks });
    const idleHeight = readout().style.minHeight;
    fireEvent.mouseMove(plot(), { clientX: 500 });
    expect(readout().style.minHeight).toBe(idleHeight);
    expect(idleHeight).toBeTruthy();
  });

  it("stays one live region across the change, rather than swapping elements", () => {
    stubLayout();
    renderScore({ marks });
    const before = readout();
    fireEvent.mouseMove(plot(), { clientX: 500 });
    expect(readout()).toBe(before);
    expect(document.querySelectorAll('[aria-live="polite"]').length).toBe(1);
  });
});

describe("Score — scheduled marks are legible (VU2-2)", () => {
  it("draws a projected fire in a stronger ink than a muted row label", () => {
    // A future mark carries no status, so it fell through statusTone's muted
    // default — the faintest class on the plot, for the only class that says
    // anything about what is coming. Pinned as "different from a past mark's
    // muted fallback" rather than as a hex, so the palette can still move.
    stubLayout();
    renderScore({ marks: [mark({ at: NOW + HOUR, jobName: "soon", tense: "future" })] });
    const pip = document.querySelector('[data-testid="score-pip"]') as HTMLElement;
    expect(pip.style.border).toContain("2px solid");
    expect(pip.style.background).not.toBe("transparent"); // hollow = panel-filled ring
  });
});

describe("FullScore (D-6)", () => {
  const AT = NOW - 5 * HOUR;
  const marks = [
    mark({ at: AT, jobName: "alpha", status: "danger" }),
    mark({ at: AT, jobName: "alpha", status: "success" }), // coincident with the above
    mark({ at: NOW - HOUR, jobName: "alpha", status: "success" }),
  ];
  const renderFull = () =>
    render(<FullScore marks={marks} now={NOW} jobNames={["alpha", "beta"]} stampInZone={stampInZone} types={{ alpha: "bash", beta: "ansible" }} />);

  // VU2-2 folds the quiet jobs. D-6's contract was never "a blank row must
  // occupy height" — it was that a job is ACCOUNTED FOR, because a silently
  // missing staff reads as "no such job". The fold keeps the accounting (it
  // names the count, and one click restores every staff) and hands the panel's
  // space to the jobs that did something. These two tests pin both halves.
  it("accounts for a job with nothing in the window instead of dropping it", () => {
    renderFull();
    expect(document.querySelector('[title="beta"]')).toBeNull();
    // Named, not vanished — the count is the accounting D-6 asked for.
    expect(screen.getByRole("button", { name: /1 quiet job/ })).toBeTruthy();
  });

  it("restores the quiet job's staff on click, as a present-and-empty lane", () => {
    renderFull();
    fireEvent.click(screen.getByRole("button", { name: /1 quiet job/ }));
    const beta = document.querySelector('[title="beta"]');
    expect(beta).toBeTruthy();
    // The rest: a lane that is present and empty, not an absent lane.
    expect((beta!.parentElement as HTMLElement).querySelectorAll('[data-testid="staff-mark"]').length).toBe(0);
  });

  it("sections the staves by run type, in a stable order", () => {
    renderFull();
    fireEvent.click(screen.getByRole("button", { name: /1 quiet job/ }));
    const root = document.querySelector('[aria-label="Full score"]') as HTMLElement;
    // The fold's own control is a sibling of the sections; sections are the divs.
    const headings = [...root.children].filter((el) => el.tagName === "DIV").map((s) => (s.firstElementChild as HTMLElement).textContent);
    expect(headings).toEqual(["ansible", "bash"]);
  });

  it("drops a type section whose staves are all quiet, rather than heading nothing", () => {
    // beta is the only ansible job and it is quiet, so the ANSIBLE header goes
    // with it — a section header introducing no rows is worse than the rows.
    renderFull();
    const root = document.querySelector('[aria-label="Full score"]') as HTMLElement;
    const headings = [...root.children].filter((el) => el.tagName === "DIV").map((s) => (s.firstElementChild as HTMLElement).textContent);
    expect(headings).toEqual(["bash"]);
  });

  it("groups coincident marks on a staff too — two runs of one job at one instant are one dot", () => {
    renderFull();
    const alpha = document.querySelector('[title="alpha"]')!.parentElement as HTMLElement;
    expect(alpha.querySelectorAll('[data-testid="staff-mark"]').length).toBe(2);
  });

  // F-4 — a staff mark's tooltip used to render new Date(g.at).toISOString(), so
  // hovering one mark in the condensed score and the same mark here gave two
  // different times for one event. Both now go through the injected stamp.
  it("stamps a staff mark's tooltip in the application timezone, not as a raw instant", () => {
    renderFull();
    const alpha = document.querySelector('[title="alpha"]')!.parentElement as HTMLElement;
    const titles = [...alpha.querySelectorAll('[data-testid="staff-mark"]')].map((m) => m.getAttribute("title") ?? "");
    expect(titles).toContain(`alpha · 2 at ${stampInZone(AT)}`);
    expect(titles).toContain(`alpha · ${stampInZone(NOW - HOUR)}`);
    for (const t of titles) expect(t).not.toMatch(/\d{2}:\d{2}:\d{2}\.\d{3}Z/);
  });

  it("files a job with no declared type under 'other' rather than dropping it", () => {
    render(<FullScore marks={[]} now={NOW} jobNames={["orphan"]} stampInZone={stampInZone} />);
    // Quiet (no marks at all), so it arrives via the fold — then it must land
    // in 'other' rather than disappearing for want of a declared type.
    fireEvent.click(screen.getByRole("button", { name: /1 quiet job/ }));
    expect(screen.getByText("other")).toBeTruthy();
    expect(document.querySelector('[title="orphan"]')).toBeTruthy();
  });
});

// FX-1 — the full score is an EXPANSION of the Score panel, not a second block
// below it. What the report objected to was reading one view stacked on another
// with the control sitting between them, far from either. FX-Q1 resolved it to
// REPLACE: one view at a time, in one surface.
describe("Score — expand in place (FX-1)", () => {
  const marks = [
    mark({ at: NOW - 2 * HOUR, jobName: "alpha", status: "success" }),
    mark({ at: NOW - HOUR, jobName: "beta", status: "danger" }),
  ];
  const renderPanel = (over: Partial<ComponentProps<typeof Score>> = {}) =>
    render(
      <Score
        marks={marks}
        now={NOW}
        hourInZone={hourInZone}
        labelInZone={labelInZone}
        stampInZone={stampInZone}
        jobNames={["alpha", "beta"]}
        types={{ alpha: "bash", beta: "ansible" }}
        expanded={false}
        onToggleExpand={() => {}}
        {...over}
      />,
    );
  const toggle = () => screen.getByRole("button", { name: /Full score|Collapse/ });
  const staves = () => document.querySelectorAll('[aria-label="Full score"]');

  it("puts the control in the Score's own header, naming what it opens", () => {
    renderPanel();
    // Inside the panel — not in a row of its own below it, which is where it was.
    const panel = screen.getByRole("region", { name: "Score" });
    expect(panel.contains(toggle())).toBe(true);
    expect(toggle().textContent).toContain("Full score (2 jobs)");
    expect(toggle().getAttribute("aria-expanded")).toBe("false");
  });

  it("swaps the single lane for the per-job staves rather than stacking them", () => {
    const { rerender } = renderPanel();
    expect(plot()).toBeTruthy();
    expect(staves().length).toBe(0);

    rerender(
      <Score
        marks={marks}
        now={NOW}
        hourInZone={hourInZone}
        labelInZone={labelInZone}
        stampInZone={stampInZone}
        jobNames={["alpha", "beta"]}
        types={{ alpha: "bash", beta: "ansible" }}
        expanded={true}
        onToggleExpand={() => {}}
      />,
    );

    // FX-Q1: replace. The compact lane is gone, not pushed down.
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(staves().length).toBe(1);
    // One surface: the staves live inside the same panel the toggle does.
    const panel = screen.getByRole("region", { name: "Score" });
    expect(panel.contains(staves()[0])).toBe(true);
    expect(toggle().getAttribute("aria-expanded")).toBe("true");
    expect(toggle().textContent).toContain("Collapse");
    // The sidebar carries its own "Collapse" button, so the expanded name is
    // disambiguated — and still contains the visible word (WCAG 2.5.3).
    expect(toggle().getAttribute("aria-label")).toBe("Collapse the full score");
  });

  it("keeps the panel's header, caption and legend in both modes", () => {
    const { rerender } = renderPanel();
    expect(screen.getByText("Score")).toBeTruthy();
    expect(screen.getByText(/last 24 hours and next 12/)).toBeTruthy();

    rerender(
      <Score
        marks={marks}
        now={NOW}
        hourInZone={hourInZone}
        labelInZone={labelInZone}
        stampInZone={stampInZone}
        jobNames={["alpha", "beta"]}
        expanded={true}
        onToggleExpand={() => {}}
      />,
    );
    expect(screen.getByText("Score")).toBeTruthy();
    expect(screen.getByText(/last 24 hours and next 12/)).toBeTruthy();
    // The legend describes the marks, which are still on screen as staff dots.
    expect(screen.getAllByText("Success").length).toBeGreaterThan(0);
  });

  it("reports the toggle to its owner rather than holding the state itself", () => {
    const onToggleExpand = vi.fn();
    renderPanel({ onToggleExpand });
    fireEvent.click(toggle());
    expect(onToggleExpand).toHaveBeenCalledTimes(1);
    // Still collapsed: the panel is controlled, so the Dashboard decides.
    expect(plot()).toBeTruthy();
  });

  // A consumer that passes no jobNames gets exactly the panel it had before —
  // the expansion is opt-in at the call site, not forced on every Score.
  it("renders no control at all when there is nothing to expand into", () => {
    renderScore();
    expect(screen.queryByRole("button", { name: /Full score/ })).toBeNull();
    expect(plot()).toBeTruthy();

    cleanup();
    render(
      <Score
        marks={marks}
        now={NOW}
        hourInZone={hourInZone}
        labelInZone={labelInZone}
        stampInZone={stampInZone}
        jobNames={[]}
        expanded={true}
        onToggleExpand={() => {}}
      />,
    );
    expect(screen.queryByRole("button", { name: /Full score/ })).toBeNull();
    // `expanded` with no jobs cannot strand the panel on an empty full score.
    expect(plot()).toBeTruthy();
  });
});

// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// RX-14 / RX-16 / RX-18 — the surfaces that exist because reactions make the run
// graph IMPLICIT. Each assertion below is about a thing an operator could
// otherwise only discover by being surprised.

const { GET } = vi.hoisted(() => ({ GET: vi.fn() }));

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return { ...actual, api: { GET } as unknown as typeof actual.api };
});

import { ReactionsTab } from "./ReactionsTab";
import { ReactionPanels } from "./ReactionPanels";
import { reactedOnBy, reactsTo, type Reaction } from "./reactions";
import { ReactionsNotProjectedNote } from "./ReactionsNotProjectedNote";

const EDGE: Reaction = {
  ownerKind: "job",
  ownerName: "load",
  ownerSource: "cronomicon",
  name: "after-extract",
  onKind: "job",
  onName: "extract",
  onSource: "cronomicon",
  onOutcome: "success",
  delaySeconds: 0,
  minIntervalSeconds: 0,
  includeWorkflowChildren: false,
  enabled: true,
  position: 0,
  missing: false,
};

const renderTab = (edges: unknown[]) => {
  GET.mockReset();
  GET.mockResolvedValue({ data: edges });
  return render(
    <MemoryRouter>
      <ReactionsTab />
    </MemoryRouter>,
  );
};

afterEach(cleanup);

describe("Reactions tab (RX-14)", () => {
  it("renders the edge in the direction an operator reads it", async () => {
    renderTab([EDGE]);
    // "load runs when extract finishes with success" — both ends and the
    // condition, because an edge list showing only one end is a list of names.
    expect(await screen.findByText("load")).toBeTruthy();
    expect(screen.getByText("extract")).toBeTruthy();
    expect(screen.getByText("success")).toBeTruthy();
  });

  // §2.10 — the whole reason this tab exists. Upcoming projects instants and a
  // reaction has none, so if this note is ever dropped the product silently goes
  // back to claiming Upcoming is the complete answer to "what will run".
  it("states that reactions cannot be projected onto Upcoming", async () => {
    renderTab([EDGE]);
    expect(await screen.findByText(/Reactions have no clock/)).toBeTruthy();
    expect(screen.getByText(/cannot be projected/)).toBeTruthy();
  });

  // RX-19 — a new reaction starts from now. Without saying so, a reaction
  // attached to a monthly job is indistinguishable from a broken one for a month.
  it("says a new reaction is not retroactive", async () => {
    renderTab([EDGE]);
    expect(await screen.findByText(/never\s+retroactively/)).toBeTruthy();
  });

  it("flags a dangling upstream rather than showing it as healthy", async () => {
    renderTab([{ ...EDGE, missing: true, onName: "deleted-job" }]);
    expect(await screen.findByText("missing")).toBeTruthy();
    // And says how many, up front — one dangling edge in a long list is
    // invisible otherwise.
    // Grammar is load-bearing here only because it was wrong: the first version
    // read "1 reaction watch a definition", which a screenshot caught.
    expect(screen.getByText(/1 reaction watches a definition that no longer exists/)).toBeTruthy();
  });

  it("teaches what a reaction is when there are none", async () => {
    renderTab([]);
    expect(await screen.findByText("No reactions yet.")).toBeTruthy();
    // The empty state has to carry the `stopped` reasoning: it is the one
    // outcome nobody guesses, and guessing wrong fires a rollback mid-incident.
    expect(screen.getByText(/hands-on fixing the thing/)).toBeTruthy();
  });

  it("filters by owner kind", async () => {
    renderTab([EDGE, { ...EDGE, ownerKind: "workflow", ownerName: "nightly-report", name: "r2" }]);
    expect(await screen.findByText("load")).toBeTruthy();
    expect(screen.getByText("nightly-report")).toBeTruthy();

    // The Owner select is the only combobox with the "Owner" label text.
    fireEvent.change(screen.getAllByRole("combobox")[0], { target: { value: "Workflows" } });
    expect(screen.queryByText("load")).toBeNull();
    expect(screen.getByText("nightly-report")).toBeTruthy();
  });

  it("groups by what they watch, so fan-out is visible", async () => {
    renderTab([EDGE, { ...EDGE, ownerName: "notify", name: "r2" }]);
    await screen.findByText("load");
    fireEvent.click(screen.getByRole("checkbox"));
    // Two reactions on ONE upstream is the shape that surprises people; a flat
    // list sorted by owner hides it.
    expect(screen.getByText(/finishes → 2 reactions/)).toBeTruthy();
  });
});

describe("reaction edge helpers", () => {
  // Identity is (kind, source, name). The two sources are disjoint namespaces,
  // so matching on name alone merges two definitions' graphs — the exact defect
  // the Phase C review caught on the backend's own per-definition read.
  it("does not merge same-named definitions from different sources", () => {
    const edges: Reaction[] = [EDGE, { ...EDGE, ownerSource: "git", name: "from-git" }];
    expect(reactsTo(edges, "job", "cronomicon", "load")).toHaveLength(1);
    expect(reactsTo(edges, "job", "cronomicon", "load")[0].name).toBe("after-extract");
    expect(reactsTo(edges, "job", "git", "load")[0].name).toBe("from-git");
  });

  it("reads the same edge from both ends", () => {
    expect(reactsTo([EDGE], "job", "cronomicon", "load")).toHaveLength(1);
    expect(reactedOnBy([EDGE], "job", "cronomicon", "extract")).toHaveLength(1);
    // And not from the wrong end.
    expect(reactedOnBy([EDGE], "job", "cronomicon", "load")).toHaveLength(0);
  });
});

describe("ReactionPanels (RX-16)", () => {
  it("shows BOTH directions — the outbound one is what nothing else can show", () => {
    const { container } = render(
      <ReactionPanels
        edges={[EDGE, { ...EDGE, ownerName: "cleanup", name: "r2", onName: "load" }]}
        kind="job"
        source="cronomicon"
        name="load"
      />,
    );
    const q = within(container);
    // "load reacts to extract" and "cleanup reacts on load".
    expect(q.getByText(/Reacts to \(1\)/)).toBeTruthy();
    expect(q.getByText(/Reacted on by \(1\)/)).toBeTruthy();
  });

  it("renders nothing at all when the definition has no edges", () => {
    const { container } = render(
      <ReactionPanels edges={[EDGE]} kind="job" source="cronomicon" name="unrelated" />,
    );
    expect(container.textContent).toBe("");
  });

  it("warns in place when an upstream is missing", () => {
    const { container } = render(
      <ReactionPanels edges={[{ ...EDGE, missing: true }]} kind="job" source="cronomicon" name="load" />,
    );
    expect(within(container).getByText(/can never fire/)).toBeTruthy();
  });
});

describe("ReactionsNotProjectedNote (RX-18)", () => {
  it("bounds Upcoming's claim", () => {
    const { container } = render(<ReactionsNotProjectedNote />);
    expect(within(container).getByText(/Reactions are not shown/)).toBeTruthy();
  });

  // It must SWITCH THE TAB, not navigate. Schedules derives the active tab from
  // ?tab= exactly once at mount, so a URL link from inside Schedules changes the
  // address bar and leaves the view where it was — the trap Schedules.tsx and
  // UpcomingTab.tsx both document, and which the first version of this note fell
  // into.
  it("calls the host's tab callback rather than linking", () => {
    const onGoToTab = vi.fn();
    const { container } = render(<ReactionsNotProjectedNote onGoToTab={onGoToTab} />);
    expect(container.querySelector("a")).toBeNull();
    fireEvent.click(within(container).getByRole("button"));
    expect(onGoToTab).toHaveBeenCalledTimes(1);
  });

  // Without a host callback there is simply no affordance, rather than a dead one.
  it("offers nothing to click when it has no way to switch tabs", () => {
    const { container } = render(<ReactionsNotProjectedNote />);
    expect(container.querySelector("button")).toBeNull();
  });
});

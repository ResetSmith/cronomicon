// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, within } from "@testing-library/react";

import { RunnerPinValue, pinCountLabel, type RunnerTagInfo } from "./RunnerPin";

afterEach(cleanup);

const tags: RunnerTagInfo[] = [
  { tag: "vlan-dmz", online: 2, total: 3 },
  { tag: "vlan-core", online: 0, total: 1 },
];

// RT-3 — the pin as a VALUE. What an operator needs from it is the tag and
// whether anything is actually carrying it; a pin naming a tag with no online
// runner is legal (RT-Q4) and queues, so the capacity has to be visible next to
// the answer rather than discovered when nothing runs.
//
// This block used to assert the attribution clause too — the pin resolved
// through an operator override that could mask the declared value, and showing
// the winner without its reason made the UI disagree with the YAML for no
// visible cause. That layer was retired in v1.3.5, and with one source there is
// nothing left to attribute.
describe("RunnerPinValue", () => {
  it("shows the declared pin", () => {
    const { container } = render(<RunnerPinValue declared="vlan-dmz" tags={tags} />);
    const q = within(container);
    expect(q.getByText("vlan-dmz")).toBeTruthy();
  });

  it("says a job with no pin runs anywhere eligible", () => {
    const { container } = render(<RunnerPinValue declared={null} tags={tags} />);
    expect(container.textContent).toContain("Any eligible runner");
  });

  it("reports capacity beside the pin", () => {
    const { container } = render(<RunnerPinValue declared="vlan-dmz" tags={tags} />);
    expect(container.textContent).toContain("2 online of 3");
  });
});

describe("pinCountLabel", () => {
  it("is loud about a tag with runners but none online", () => {
    // Legal (RT-Q4) but the run will queue, so the wording must not read as
    // healthy capacity.
    expect(pinCountLabel("vlan-core", tags)).toBe("0 online of 1");
  });

  it("says so when nothing in the fleet carries the tag", () => {
    expect(pinCountLabel("vlan-lab", tags)).toBe("no runner carries this tag yet");
  });

  it("matches case-insensitively, as the claim query does", () => {
    // runner_tags.tag is COLLATE NOCASE since migration 1080 (RT-G9). A
    // case-sensitive lookup here would tell an operator their pin matches
    // nothing while dispatch happily claims it.
    expect(pinCountLabel("VLAN-DMZ", tags)).toBe("2 online of 3");
    expect(pinCountLabel("  vlan-DMZ  ", tags)).toBe("2 online of 3");
  });
});

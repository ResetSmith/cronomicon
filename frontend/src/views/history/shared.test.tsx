// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { RunnerCell } from "./shared";

afterEach(cleanup);

// LR-50 — History's Runner cell stands where the Executor badge stood. Every
// run is the runner executor's since 2.3.0, so the cell names WHICH runner
// took the run; what it must never do is name one that did not.
describe("RunnerCell", () => {
  it("names the runner that took the run", () => {
    const { container } = render(<RunnerCell executor="runner" runnerName="dmz-agent-01" />);
    expect(container.textContent).toBe("dmz-agent-01");
    expect(container.querySelector("span")?.title).toBe("Taken by runner dmz-agent-01");
  });

  it("says the server ran a run from before 2.3.0, which had no runner", () => {
    const { container } = render(<RunnerCell executor="ssh" runnerName={null} />);
    expect(container.textContent).toBe("Server (SSH)");
    expect(container.querySelector("span")?.title).toMatch(/in-app SSH executor \(before v2\.3\.0\)/);
  });

  it("shows the empty cell, not a runner, for a run nobody has claimed", () => {
    const { container } = render(<RunnerCell executor="runner" runnerName={null} />);
    expect(container.textContent).not.toMatch(/Server|Runner/);
    expect(container.textContent?.trim()).toBe("—");
  });

  // The name is the run's own copy, so it wins even on a row that still says
  // ssh (it cannot happen today; the label must not depend on that).
  it("prefers a recorded name over the executor", () => {
    const { container } = render(<RunnerCell executor="ssh" runnerName="local" />);
    expect(container.textContent).toBe("local");
  });
});

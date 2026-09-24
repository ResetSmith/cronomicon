// @vitest-environment jsdom
import { describe, expect, it, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { StepChain } from "./StepChain";

afterEach(cleanup);

// PS-1b — the chain renderer must draw a sequence.
//
// graphView routes a sequence nested inside a parallel arm to the canvas (it
// sets `nested`), which is why this gap stayed invisible. But a TOP-LEVEL
// sequence sets nothing, so a workflow under the 10-node canvas threshold still
// reaches StepChain — where an unhandled type fell through to the job default
// and rendered the whole chain as one chip reading "step", with every job in it
// gone from the view.

describe("StepChain (PS-1b)", () => {
  it("draws a top-level sequence's jobs instead of collapsing it to one chip", () => {
    render(
      <StepChain
        steps={[
          { type: "sequence", label: "nightly chain", steps: [{ type: "job", name: "quiesce" }, { type: "job", name: "snapshot" }] },
          { type: "job", name: "notify" },
        ]}
      />,
    );

    expect(screen.getByText("quiesce")).toBeTruthy();
    expect(screen.getByText("snapshot")).toBeTruthy();
    expect(screen.getByText("notify")).toBeTruthy();
    // The give-away of the old fall-through: a bare chip labelled "step".
    expect(screen.queryByText("step")).toBeNull();
  });

  it("labels the sequence container, falling back when unlabelled", () => {
    const { unmount } = render(
      <StepChain steps={[{ type: "sequence", label: "nightly chain", steps: [{ type: "job", name: "a" }] }]} />,
    );
    expect(screen.getByText(/nightly chain/)).toBeTruthy();
    unmount();

    render(<StepChain steps={[{ type: "sequence", steps: [{ type: "job", name: "a" }] }]} />);
    expect(screen.getByText(/sequence/)).toBeTruthy();
  });

  it("says 'empty' for a sequence with no steps rather than rendering nothing", () => {
    render(<StepChain steps={[{ type: "sequence", label: "hollow", steps: [] }]} />);
    expect(screen.getByText("empty")).toBeTruthy();
  });

  it("still renders parallel, branch and plain job steps", () => {
    render(
      <StepChain
        steps={[
          { type: "job", name: "first" },
          { type: "parallel", label: "fan", jobs: [{ type: "job", name: "left" }, { type: "job", name: "right" }] },
          {
            type: "branch",
            condition: { label: "first succeeded" },
            pass: [{ type: "job", name: "on-pass" }],
            fail: [{ type: "job", name: "on-fail" }],
          },
        ]}
      />,
    );
    for (const name of ["first", "left", "right", "on-pass", "on-fail"]) {
      expect(screen.getByText(name)).toBeTruthy();
    }
  });
});

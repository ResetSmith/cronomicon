// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import { EmptyCell, SourceBadge, TabBar } from "./ui";

afterEach(cleanup);

// VU2-4 (the visual-updates2 plan) — the quieting pass. Each of these
// three atoms exists to spend LESS ink on the value a catalog repeats in every
// row, so the assertions are about relative weight, not about exact colours: a
// hard-coded hex here would fail the moment the palette moves, and would not
// catch the regression that actually matters (the quiet variant loudening back
// up to match the loud one).

describe("EmptyCell", () => {
  it("renders a dash, not a blank cell", () => {
    // A blank cell in a sortable column reads as a rendering failure — the
    // dash is a one-character answer to "is there no value, or did this break?"
    const { container } = render(<EmptyCell />);
    expect(container.textContent).toBe("—");
  });

  it("is fainter than muted text", () => {
    const { container } = render(<EmptyCell />);
    const span = container.querySelector("span")!;
    expect(span.style.color).toBeTruthy();
    expect(Number(span.style.opacity)).toBeLessThan(1);
  });

  it("names itself for assistive tech", () => {
    // The dash is decorative; the meaning is "no value". A screen reader
    // announcing "em dash" would be worse than useless in a nine-column table.
    render(<EmptyCell />);
    expect(screen.getByLabelText("no value")).toBeTruthy();
  });
});

describe("SourceBadge", () => {
  // The asymmetry is the whole point: git is the majority in every catalog that
  // shows this column, so it renders as quiet text while cronomicon keeps the chip.
  it("gives git no chip chrome", () => {
    const { container } = render(<SourceBadge source="git" />);
    const span = container.querySelector("span")!;
    expect(span.style.background).toBe("transparent");
    expect(span.style.border).toBe("");
  });

  it("keeps the chip for cronomicon", () => {
    const { container } = render(<SourceBadge source="cronomicon" />);
    const span = container.querySelector("span")!;
    expect(span.style.background).not.toBe("transparent");
    expect(span.style.border).toContain("1px solid");
    expect(span.style.color).toBeTruthy();
  });

  it("treats an unknown source as git", () => {
    const { container } = render(<SourceBadge />);
    expect(container.textContent).toBe("git");
    expect(container.querySelector("span")!.style.background).toBe("transparent");
  });
});

describe("TabBar", () => {
  it("accepts plain strings, as every non-Jobs caller passes", () => {
    const onChange = vi.fn();
    render(<TabBar tabs={["One", "Two"]} active={0} onChange={onChange} />);
    fireEvent.click(screen.getByText("Two"));
    expect(onChange).toHaveBeenCalledWith(1);
  });

  it("dims a muted tab below its inactive neighbour", () => {
    render(
      <TabBar
        tabs={[{ label: "All (14)" }, { label: "Failed (0)", muted: true }]}
        active={0}
        onChange={() => {}}
      />,
    );
    const muted = screen.getByText("Failed (0)") as HTMLElement;
    expect(Number(muted.style.opacity)).toBeLessThan(1);
  });

  it("never dims the active tab, muted or not", () => {
    // You clicked it, so it has to read as selected — dimming the tab you are
    // standing on would say "this view is disabled", which it is not. Compared
    // against a plain-string active tab rather than a colour literal: jsdom
    // normalises hex to rgb(), and the invariant is "same as any active tab".
    render(<TabBar tabs={[{ label: "Failed (0)", muted: true }]} active={0} onChange={() => {}} />);
    render(<TabBar tabs={["Plain"]} active={0} onChange={() => {}} />);
    const mutedActive = screen.getByText("Failed (0)") as HTMLElement;
    const plainActive = screen.getByText("Plain") as HTMLElement;
    expect(Number(mutedActive.style.opacity || 1)).toBe(1);
    expect(mutedActive.style.color).toBe(plainActive.style.color);
    expect(mutedActive.style.fontWeight).toBe(plainActive.style.fontWeight);
  });

  it("keeps a muted tab clickable", () => {
    // "Failed (0)" is a real answer worth navigating to; quiet is not disabled.
    const onChange = vi.fn();
    render(
      <TabBar tabs={[{ label: "All (14)" }, { label: "Failed (0)", muted: true }]} active={0} onChange={onChange} />,
    );
    fireEvent.click(screen.getByText("Failed (0)"));
    expect(onChange).toHaveBeenCalledWith(1);
  });
});

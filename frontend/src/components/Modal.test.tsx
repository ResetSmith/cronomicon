// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { Modal } from "./ui";

// FX-10 — the Modal caps its panel at 84vh and scrolls. Every long modal in the
// app therefore had the same defect: the action row, and the validation error
// explaining why the action was disabled, scrolled below the fold on a short
// viewport. `footer` is the fix, made a property of the component rather than
// copied into each call site.
//
// The property that matters is structural, and jsdom can check it exactly: the
// footer must not live inside the element that scrolls. FX-6's finding was that
// a pinned bar INSIDE the scrollport (sticky, bottom-anchored) grows upward over
// the content when it outgrows it — so "pinned" has to mean "outside", not
// "sticky".

afterEach(cleanup);

const body = () => screen.getByTestId("modal-body");
const footer = () => screen.getByTestId("modal-footer");
/** The element that actually scrolls: the nearest ancestor with overflowY auto. */
const scrollport = (el: Element): HTMLElement | null => {
  for (let n = el.parentElement; n; n = n.parentElement) {
    if (n.style.overflowY === "auto") return n;
  }
  return null;
};

describe("Modal — pinned footer (FX-10)", () => {
  it("keeps the footer outside the scrolling body", () => {
    render(
      <Modal title="Edit" onClose={vi.fn()} footer={<div data-testid="modal-footer">Save</div>}>
        <div data-testid="modal-body">a very long form</div>
      </Modal>,
    );

    // The body scrolls...
    expect(scrollport(body())).not.toBeNull();
    // ...and the footer is not inside anything that does, so it cannot be
    // scrolled away from and cannot paint over the body however tall it gets.
    expect(scrollport(footer())).toBeNull();
    expect(scrollport(body())!.contains(footer())).toBe(false);
  });

  it("caps the panel and lets the body shrink rather than pushing the footer out", () => {
    render(
      <Modal title="Edit" onClose={vi.fn()} footer={<div data-testid="modal-footer">Save</div>}>
        <div data-testid="modal-body">form</div>
      </Modal>,
    );

    // The footer sits in its own flex-none wrapper; the panel is one above.
    const panel = footer().parentElement!.parentElement!;
    expect(panel.style.maxHeight).toBe("84vh");
    expect(panel.style.flexDirection).toBe("column");
    // Without minHeight 0 a flex item refuses to shrink below its content, and
    // the body would push the footer off the panel instead of scrolling.
    expect(scrollport(body())!.style.minHeight).toBe("0px");
  });

  // A modal that passes no footer must render exactly as it always did — this
  // capability is opt-in, and eight other modals were not migrated blind.
  it("leaves a footerless modal's structure alone", () => {
    render(
      <Modal title="Plain" onClose={vi.fn()}>
        <div data-testid="modal-body">content</div>
      </Modal>,
    );

    // The panel itself is the scrollport, as before — no inner scrolling body.
    const port = scrollport(body())!;
    expect(port.style.maxHeight).toBe("84vh");
    expect(port.style.flexDirection).toBe("");
  });

  it("still closes on the backdrop but not on a click inside, with a footer", () => {
    const onClose = vi.fn();
    const { container } = render(
      <Modal title="Edit" onClose={onClose} footer={<div data-testid="modal-footer">Save</div>}>
        <div data-testid="modal-body">form</div>
      </Modal>,
    );

    fireEvent.click(footer());
    expect(onClose).not.toHaveBeenCalled();
    fireEvent.click(container.firstElementChild!);
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});

// RU-13 — the rail layout. The testids are the caller's nodes, as above; the
// structural property is that the rail replaces the footer at ≥1200px and is
// ignored below it, with the caller's state untouched either way (both nodes are
// always passed; the viewport picks).
describe("Modal — rail layout (RU-13)", () => {
  const setViewport = (width: number) => {
    Object.defineProperty(window, "innerWidth", { value: width, configurable: true, writable: true });
    // act(): the resize handler sets React state; dispatching outside act would
    // leave the re-render unflushed and the assertion reading the old layout.
    act(() => {
      window.dispatchEvent(new Event("resize"));
    });
  };
  afterEach(() => setViewport(1024));

  const renderRail = () =>
    render(
      <Modal
        title="Run"
        onClose={vi.fn()}
        footer={<div data-testid="modal-footer">stacked commit</div>}
        rail={<div data-testid="modal-rail">this run</div>}
      >
        <div data-testid="modal-body">sections</div>
      </Modal>,
    );

  it("renders the rail instead of the footer at ≥1200px", () => {
    setViewport(1600);
    renderRail();

    expect(screen.getByTestId("modal-rail")).toBeTruthy();
    expect(screen.queryByTestId("modal-footer")).toBeNull();
    // Only the sections column scrolls; the rail is outside it.
    expect(scrollport(body())).not.toBeNull();
    expect(scrollport(body())!.contains(screen.getByTestId("modal-rail"))).toBe(false);
  });

  it("ignores the rail below the breakpoint — the stacked layout is not a degraded mode", () => {
    setViewport(1100);
    renderRail();

    expect(screen.queryByTestId("modal-rail")).toBeNull();
    expect(screen.getByTestId("modal-footer")).toBeTruthy();
  });

  it("re-parents live on resize across the breakpoint", () => {
    setViewport(1600);
    renderRail();
    expect(screen.getByTestId("modal-rail")).toBeTruthy();

    setViewport(1100);
    expect(screen.queryByTestId("modal-rail")).toBeNull();
    expect(screen.getByTestId("modal-footer")).toBeTruthy();

    setViewport(1600);
    expect(screen.getByTestId("modal-rail")).toBeTruthy();
  });

  it("widens the panel only while the rail is active", () => {
    setViewport(1600);
    const { container } = renderRail();
    const panel = (container.querySelector('[data-testid="modal-rail"]') as HTMLElement).closest(
      "div[style*='max-height']",
    ) as HTMLElement;
    expect(panel.style.width).toBe("960px");
  });
});

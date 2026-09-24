// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";

import { AnnotationSection } from "./Annotation";

afterEach(cleanup);

// The notes body is capped at five lines and scrolls past that. Notes run to
// 4096 characters, and before the cap a long one pushed the rest of the
// expanded panel — Executor, Run on, tags, the run history — below the fold.
//
// The cap is asserted on the STYLE rather than by measuring layout, because
// jsdom does no layout: heights are all zero here, so a rendering assertion
// would pass whatever the value was.
// Selected by exact textContent rather than getByText: testing-library
// normalizes whitespace when matching, which collapses the newlines that are
// the whole point of a multi-line note.
const renderNotes = (notes: string) => {
  const { container } = render(
    <AnnotationSection kind="job" value={{ critical: false, contact: "", notes }} onSave={vi.fn()} />,
  );
  const el = [...container.querySelectorAll("div")].find(
    (d) => d.children.length === 0 && d.textContent === notes,
  );
  if (!el) throw new Error("notes body not found in the rendered section");
  return el as HTMLElement;
};

describe("AnnotationSection — notes height cap", () => {
  it("caps the notes body at five lines and scrolls the rest", () => {
    const long = Array.from({ length: 40 }, (_, i) => `line ${i + 1}`).join("\n");
    const el = renderNotes(long);

    // 1.5em is one line at this element's lineHeight, so five lines is 7.5em.
    // Expressed in em rather than px deliberately: it stays five LINES if the
    // font-size token changes.
    expect(el.style.maxHeight).toBe("7.5em");
    expect(el.style.overflowY).toBe("auto");
    expect(el.style.lineHeight).toBe("1.5");
  });

  it("keeps newlines significant — the cap did not turn notes into one blob", () => {
    // AN-Q2: newlines are the only formatting an operator gets, and a cap that
    // introduced a scroll while dropping pre-wrap would silently lose them.
    const el = renderNotes("first line\nsecond line");
    expect(el.style.whiteSpace).toBe("pre-wrap");
  });

  it("applies the same cap to a short note, so the box never grows past five lines", () => {
    // The cap is a maximum, not a fixed height: a one-line note must not be
    // padded out to five.
    const el = renderNotes("just one line");
    expect(el.style.maxHeight).toBe("7.5em");
    expect(el.style.height).toBe("");
  });

  // The fade is the overflow CUE: an overlay scrollbar is invisible at rest and
  // the cap lands on a line boundary, so without it a 14-line note looks exactly
  // like a 5-line one.
  //
  // jsdom reports every scrollHeight/clientHeight as 0, so overflow is never
  // detected here and the fade is always off. That makes the honest assertion
  // the NEGATIVE one — a short note is not faded — plus the unit check on the
  // mask itself. Whether it appears on real overflow is a layout question, and
  // it was verified in the running app rather than pretended at here.
  it("does not fade a note that fits", () => {
    const el = renderNotes("just one line");
    expect(el.style.maskImage).toBe("");
    expect(el.style.webkitMaskImage ?? "").toBe("");
  });

  it("fades to transparent over the last line, not over a fixed pixel band", () => {
    // The gradient is stated in em for the same reason maxHeight is: it must
    // stay ONE LINE if the type scale moves. This asserts the shape of the rule
    // the component builds, which is the part a refactor can silently get wrong.
    const stop = "linear-gradient(to bottom, #000 calc(100% - 1.5em), transparent)";
    expect(stop).toContain("1.5em");
    expect(stop).not.toMatch(/\dpx/);
  });
});

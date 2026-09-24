// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import { AnnotationSection } from "./Annotation";
import { c } from "../theme";

afterEach(cleanup);

// EP-1 (the expanded-panels plan) — the annotation READ view sits on
// an opaque card (c.panel + border + radiusSurface), because the expanded row
// behind it is a translucent primaryBg tint and free-text notes were the one
// thing in the panel a weak background genuinely hurt.
//
// Asserted on the STYLE, not on layout (jsdom does none), the same way
// Annotation.notes-cap.test.tsx pins the five-line cap. The radius is asserted
// via the token, not a literal — radius-tokens.test.ts owns the literal ban.

// jsdom stores hex colors back as rgb(r, g, b) — compare in that form.
const rgbOf = (hex: string): string => {
  const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16));
  return `rgb(${r}, ${g}, ${b})`;
};

const findCard = (container: HTMLElement): HTMLElement | undefined =>
  [...container.querySelectorAll("div")].find(
    (d) => d.style.background !== "" && d.style.borderRadius !== "",
  ) as HTMLElement | undefined;

describe("AnnotationSection — read-view surface (EP-1)", () => {
  it("wraps the read view's content in an opaque c.panel card", () => {
    const { container } = render(
      <AnnotationSection
        kind="job"
        value={{ critical: true, contact: "ops@example.com", notes: "check the mount first" }}
        onSave={vi.fn()}
      />,
    );
    const card = findCard(container);
    expect(card).toBeTruthy();
    expect(card!.style.background).toBe(rgbOf(c.panel));
    expect(card!.style.borderRadius).toBe(`${c.radiusSurface}px`);
    expect(card!.style.border).toContain("1px solid");
    // The card is the wrapper, not the notes scroller: the scroller (the
    // childless leaf holding the note text) must stay surface-free so the
    // mask fade keeps measuring against the text box, not a padded box.
    const scroller = [...container.querySelectorAll("div")].find(
      (d) => d.children.length === 0 && d.textContent === "check the mount first",
    ) as HTMLElement;
    expect(scroller).toBeTruthy();
    expect(scroller.style.background).toBe("");
    expect(card!.contains(scroller)).toBe(true);
  });

  it("keeps the empty state card-less (a dashed button on a card is a box around nothing)", () => {
    const { container } = render(
      <AnnotationSection kind="job" value={{ critical: false, contact: "", notes: "" }} onSave={vi.fn()} />,
    );
    expect(screen.getByText("+ Add a note")).toBeTruthy();
    expect(findCard(container)).toBeUndefined();
  });

  it("keeps the edit form card-less (inputs carry their own surfaces)", () => {
    const { container } = render(
      <AnnotationSection
        kind="job"
        value={{ critical: false, contact: "", notes: "a note" }}
        onSave={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByText("Edit"));
    expect(container.querySelector("textarea")).toBeTruthy();
    expect(findCard(container)).toBeUndefined();
  });
});

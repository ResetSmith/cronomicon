// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { Wordmark, WORDMARK_TAGLINE } from "./Wordmark";

// Guards the LG series (the ux-fixes plan Part C). The brand lockup was
// two bitmaps with the wordmark baked in, picked per theme. They drifted — different
// canvas padding and aspect, so identical CSS drew the light mark at under half the
// dark one's size, and they disagreed on the tagline. The properties below are the
// ones that made that class of bug possible, so they are what this pins.
afterEach(cleanup);

describe("Wordmark", () => {
  it("renders ONE emblem asset — never a per-theme wordmark bitmap (LG-Q1)", () => {
    render(<Wordmark />);
    const imgs = screen.getAllByAltText("Cronomicon");
    expect(imgs).toHaveLength(1);
    // The white-lettered asset on the true light rail (#ffffff) would be
    // white-on-white: the exact regression VU-Q2 introduced and LG-Q1 forbids.
    // Retired outright — assert no descendant of the lockup can reach for it.
    expect(imgs[0].getAttribute("src") ?? "").not.toMatch(/logo-white-text/);
    expect(imgs[0].getAttribute("src") ?? "").not.toMatch(/logo\.png/);
  });

  it("renders the wordmark as real, selectable DOM text, with the tagline opt-in only", () => {
    render(<Wordmark />);
    expect(screen.getByText("CRONOMICON")).toBeTruthy();
    // The sidebar lockup is emblem + name. The tagline is a separate line that
    // a call site must ask for; the default rail never shows it.
    expect(screen.queryByText(WORDMARK_TAGLINE)).toBeNull();
    // LG-3: the two retired assets said "CONDUCTING AUTOMATION" and "CONDUCTING
    // YOUR AUTOMATION" respectively. One string now, so they cannot disagree.
    expect(WORDMARK_TAGLINE).toBe("ANCIENT RITES OF SCHEDULING, MADE EASY");
  });

  it("renders the canonical tagline when a call site opts in (Login)", () => {
    render(<Wordmark tone="page" tagline />);
    expect(screen.getByText("CRONOMICON")).toBeTruthy();
    expect(screen.getByText(WORDMARK_TAGLINE)).toBeTruthy();
  });

  it("collapses by HIDING the text, not by cropping the bitmap (LG-2)", () => {
    render(<Wordmark collapsed tagline />);
    // The emblem survives collapse — the rail still shows the brand.
    expect(screen.getAllByAltText("Cronomicon")).toHaveLength(1);
    // ...and the lettering is simply gone, so there is no hand-tuned crop window
    // to go stale against a differently-sized asset (the LG-2 defect).
    expect(screen.queryByText("CRONOMICON")).toBeNull();
    expect(screen.queryByText(WORDMARK_TAGLINE)).toBeNull();
  });

  it("renders the same lockup on a page surface, only re-toning the text", () => {
    render(<Wordmark tone="page" />);
    expect(screen.getAllByAltText("Cronomicon")).toHaveLength(1);
    expect(screen.getByText("CRONOMICON")).toBeTruthy();
  });
});

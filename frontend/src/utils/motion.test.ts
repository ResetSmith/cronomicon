// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { prefersReducedMotion, scrollIntoViewRespectingMotion } from "./motion";

// H-2 — the half of reduced-motion that CSS cannot do.
//
// global-css.ts kills every transition and animation under `reduce`, inline
// styles included, via !important. It cannot touch a scroll requested with an
// explicit behaviour: `scrollIntoView({ behavior: "smooth" })` passes the
// behaviour as an ARGUMENT, and the argument wins over the `scroll-behavior`
// property. So the sheet says "no motion" and the element glides anyway.

const stubMatchMedia = (matches: boolean) => {
  const mm = vi.fn().mockReturnValue({ matches, media: "(prefers-reduced-motion: reduce)" });
  vi.stubGlobal("matchMedia", mm);
  return mm;
};

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("prefersReducedMotion (H-2)", () => {
  it("reads the reduce query", () => {
    const mm = stubMatchMedia(true);
    expect(prefersReducedMotion()).toBe(true);
    expect(mm).toHaveBeenCalledWith("(prefers-reduced-motion: reduce)");
  });

  it("is false when the viewer expressed no preference", () => {
    stubMatchMedia(false);
    expect(prefersReducedMotion()).toBe(false);
  });

  it("defaults to animating when matchMedia is unavailable", () => {
    // An absent API is not a request to reduce; treating it as one would invent
    // an intent nobody expressed — and would silently change behaviour in every
    // environment whose matchMedia is not stubbed.
    vi.stubGlobal("matchMedia", undefined);
    expect(prefersReducedMotion()).toBe(false);
  });
});

describe("scrollIntoViewRespectingMotion (H-2)", () => {
  const el = () => {
    const node = document.createElement("div");
    node.scrollIntoView = vi.fn();
    return node;
  };

  it("glides normally", () => {
    stubMatchMedia(false);
    const node = el();
    scrollIntoViewRespectingMotion(node, { block: "center" });
    expect(node.scrollIntoView).toHaveBeenCalledWith({ block: "center", behavior: "smooth" });
  });

  it("jumps under reduce — and still scrolls", () => {
    stubMatchMedia(true);
    const node = el();
    scrollIntoViewRespectingMotion(node, { block: "center" });
    // "auto", not a skipped call: reduced motion removes the animation, never
    // the navigation. A row the user was sent to must still be on screen.
    expect(node.scrollIntoView).toHaveBeenCalledWith({ block: "center", behavior: "auto" });
  });

  it("never lets a caller's options override the decision", () => {
    stubMatchMedia(true);
    const node = el();
    // `behavior` is excluded from the parameter type; this is the runtime half
    // of that contract, for a JS caller or a cast that gets past the compiler.
    scrollIntoViewRespectingMotion(node, { behavior: "smooth" } as Parameters<typeof scrollIntoViewRespectingMotion>[1]);
    expect(node.scrollIntoView).toHaveBeenCalledWith({ behavior: "auto" });
  });
});

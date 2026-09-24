// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import { globalCss } from "./global-css";
import { applyTheme, c } from "./theme";

// Two a11y guarantees live in the one global stylesheet, and neither is visible
// to tsc or to a render test — jsdom does not evaluate :focus-visible or
// prefers-reduced-motion. So assert on the sheet itself: these tests exist to
// catch the rule being dropped or the keyframes being hoisted back out of the
// media query, which is exactly how this kind of fix silently rots.

describe("global stylesheet — focus ring (VU-2)", () => {
  it("gives every interactive element a :focus-visible outline", () => {
    expect(globalCss).toMatch(/:focus-visible\s*\{[^}]*outline:\s*2px solid/);
  });

  it("colours the ring from the theme variable, with a dark-mode literal fallback", () => {
    expect(globalCss).toContain("var(--am-focus, #5298c2)");
  });

  it("insets the ring on table rows so an overflow container cannot crop it", () => {
    expect(globalCss).toMatch(/tr:focus-visible\s*\{[^}]*outline-offset:\s*-2px/);
  });
});

describe("global stylesheet — reduced motion (VU-15)", () => {
  it("defines pulse and shimmer ONLY inside a no-preference media query", () => {
    const guard = globalCss.match(/@media \(prefers-reduced-motion: no-preference\) \{([\s\S]*?)\n  \}/);
    expect(guard).not.toBeNull();
    const guarded = guard![1];
    expect(guarded).toContain("@keyframes pulse");
    expect(guarded).toContain("@keyframes shimmer");
    // Nothing outside the guard may define them, or the guard is decorative.
    expect(globalCss.match(/@keyframes pulse/g)).toHaveLength(1);
    expect(globalCss.match(/@keyframes shimmer/g)).toHaveLength(1);
  });
});

// H-2 — the keyframes guard above only ever covered the two ANIMATIONS. Every
// transition in this app is an inline style on a component (a chevron rotating
// 180deg, a toggle knob sliding, a tile lifting), and inline styles beat a
// stylesheet — so under `reduce` they all kept moving. This is the rule that
// stops them, and it needs !important to be able to.
describe("global stylesheet — reduced motion, transitions (H-2)", () => {
  const reduceBlock = () => globalCss.match(/@media \(prefers-reduced-motion: reduce\) \{([\s\S]*?)\n  \}/)?.[1] ?? "";

  it("neutralises transitions and animations under reduce", () => {
    const block = reduceBlock();
    expect(block).not.toBe("");
    expect(block).toMatch(/\*,\s*\*::before,\s*\*::after/);
    expect(block).toMatch(/transition-duration:\s*0\.01ms\s*!important/);
    expect(block).toMatch(/animation-duration:\s*0\.01ms\s*!important/);
    expect(block).toMatch(/animation-iteration-count:\s*1\s*!important/);
  });

  it("uses !important, without which an inline transition wins", () => {
    // The whole mechanism depends on this: drop the flags and the rule is
    // decorative, because every transition it targets is set inline.
    const decls = reduceBlock().split(";").filter((d) => d.includes(":"));
    expect(decls.length).toBeGreaterThan(0);
    for (const d of decls) expect(d).toContain("!important");
  });

  it("keeps near-zero rather than none, so transitionend still fires", () => {
    expect(reduceBlock()).not.toMatch(/transition-duration:\s*0s/);
    expect(reduceBlock()).not.toMatch(/transition:\s*none/);
  });
});

describe("applyTheme publishes the focus colour", () => {
  it("writes --am-focus and repaints it on toggle", () => {
    applyTheme("dark");
    const dark = document.documentElement.style.getPropertyValue("--am-focus");
    expect(dark).toBe(c.primary);

    applyTheme("light");
    const light = document.documentElement.style.getPropertyValue("--am-focus");
    expect(light).toBe(c.primary);
    expect(light).not.toBe(dark);
  });
});

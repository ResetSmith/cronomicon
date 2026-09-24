// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { c } from "./theme";

// Drift guard for the type scale (VU-7, Phase C) — the sibling of
// radius-tokens.test.ts, and it exists for the same reason.
//
// The app's type lived entirely in a 10–14px band in ONE family. With four
// sizes covering everything from a page title to a chip, hierarchy had nowhere
// to go and fell to colour instead. The scale is six steps now, but a scale is
// only a scale while nobody types a literal: one `fontSize: 15` and the band
// starts reassembling, and nothing about it looks wrong in review.
//
// Six tokens: fontDisplay / fontTitle / fontHead / fontBody / fontSm / fontXs.
// Need a seventh? Add it to the Palette and update this test — deliberately.

const SRC = dirname(fileURLToPath(import.meta.url));

function sourceFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...sourceFiles(p));
    else if ((entry.name.endsWith(".tsx") || entry.name.endsWith(".ts")) && !entry.name.includes(".test.")) out.push(p);
  }
  return out;
}

describe("type scale (VU-7)", () => {
  it("exposes exactly six steps, strictly descending", () => {
    const steps = [c.fontDisplay, c.fontTitle, c.fontHead, c.fontBody, c.fontSm, c.fontXs];
    expect(steps).toEqual([36, 24, 16, 14, 13, 11]);
    for (let i = 1; i < steps.length; i++) expect(steps[i]).toBeLessThan(steps[i - 1]);
  });

  it("has no step below 11px", () => {
    // 10px (and a stray 9px) was where uppercase Plex stopped being legible on a
    // dense row, and it was doing the same job as 11.
    expect(Math.min(c.fontXs, c.fontSm)).toBeGreaterThanOrEqual(11);
  });

  it("no source file hardcodes a numeric fontSize", () => {
    const offenders: string[] = [];
    for (const file of sourceFiles(SRC)) {
      // latin1, not utf8: Jobs.tsx and ReferenceBindings.tsx contain a NUL byte
      // (it is why grep calls them binary and silently returns nothing).
      readFileSync(file, "latin1")
        .split("\n")
        .forEach((line, i) => {
          // Scan the whole VALUE, not just its first character. `fontSize: 12`
          // is the obvious case, but `fontSize: small ? 12 : 13` is the one that
          // actually got through the first version of this guard and shipped
          // four off-scale buttons — a ternary starts with an identifier, so a
          // leading-digit test never sees it.
          const m = line.match(/fontSize:\s*([^,}\n]+)/);
          if (m && /(?<![\w.])\d/.test(m[1])) {
            offenders.push(`${file.slice(SRC.length)}:${i + 1} — ${line.trim().slice(0, 110)}`);
          }
        });
    }
    expect(offenders, `use the c.font* scale tokens instead:\n${offenders.join("\n")}`).toEqual([]);
  });

  it("declares no display face — the app has three faces, not four", () => {
    // F2-2 / VF-Q1. This test used to allow c.display on Login and the sidebar
    // wordmark, the two surfaces VU-Q1 reserved the serif for. Neither ever
    // consumed it: both render the wordmark as a raster asset, so there was no
    // text for a display face to set, and the token sat there as an attractive
    // nuisance for the next person wanting a serif page title.
    //
    // Now the token is gone and this guards the absence, in both directions —
    // no consumer anywhere, and no re-declaration in theme.ts. Fraunces itself
    // is unaffected: it is a docs-only face now (documentation-palette.test.ts),
    // and the manuals carry their own @font-face blocks.
    const consumers = sourceFiles(SRC)
      .filter((f) => /\bc\.display\b/.test(readFileSync(f, "latin1")))
      .map((f) => f.slice(SRC.length));
    expect(consumers, "c.display was removed in F2-2 — do not reintroduce a reserved face").toEqual([]);
    expect(/\bdisplay:\s*string/.test(readFileSync(`${SRC}/theme.ts`, "latin1"))).toBe(false);
  });
});

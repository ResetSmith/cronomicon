// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { c } from "./theme";

// Drift guard for the three-radius rule (VU-6, Phase B).
//
// The app had grown NINE radius values across 243 uses — 6, 4, 8, 3, 10, 20, 5,
// 2, 7, 14 — with no system behind which was which. A single Jobs row carried
// four shapes at four radii. That re-fragments the moment someone types a
// literal, and nothing about a literal looks wrong in review, so the rule is
// enforced here rather than in a style guide nobody re-reads.
//
// Three tokens, and only three: c.radiusChip / c.radiusSurface / c.radiusPill
// (theme.ts). If you need a fourth, change the rule deliberately — add the token
// to the Palette and update this test — rather than by adding one literal.

const SRC = dirname(fileURLToPath(import.meta.url));
const SELF = "radius-tokens.test.ts";

function tsxFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...tsxFiles(p));
    else if ((entry.name.endsWith(".tsx") || entry.name.endsWith(".ts")) && entry.name !== SELF) out.push(p);
  }
  return out;
}

describe("radius tokens (VU-6)", () => {
  it("exposes exactly three radii, and they are distinct", () => {
    expect(c.radiusChip).toBe(4);
    expect(c.radiusSurface).toBe(8);
    expect(c.radiusPill).toBe(999);
    expect(new Set([c.radiusChip, c.radiusSurface, c.radiusPill]).size).toBe(3);
  });

  it("no source file hardcodes a numeric borderRadius", () => {
    const offenders: string[] = [];
    for (const file of tsxFiles(SRC)) {
      // Read as latin1, not utf8: Jobs.tsx contains a NUL byte (it is why grep
      // treats that file as binary and silently returns nothing). utf8 decoding
      // would still work here, but latin1 guarantees byte-for-byte scanning of
      // every file regardless of what else lands in one later.
      const text = readFileSync(file, "latin1");
      text.split("\n").forEach((line, i) => {
        // Matches `borderRadius: 6` / `borderRadius:6,` but NOT `borderRadius: "50%"`
        // (a circle is a shape, not a scale step) and not a token or expression.
        if (/borderRadius:\s*\d/.test(line)) offenders.push(`${file.slice(SRC.length)}:${i + 1} — ${line.trim()}`);
      });
    }
    expect(offenders, `use c.radiusChip / c.radiusSurface / c.radiusPill instead:\n${offenders.join("\n")}`).toEqual([]);
  });

  it("reserves the fully-round pill for status, so a row's other chips cannot mimic it", () => {
    // The pill is the one shape that pops in a table row carrying type, tag and
    // source chips (VU-17). Badge is the only kit component allowed to use it.
    const kit = readFileSync(join(SRC, "components/ui.tsx"), "utf8");
    const pillUses = kit.split("\n").filter((l) => l.includes("c.radiusPill"));
    expect(pillUses).toHaveLength(1);
  });
});

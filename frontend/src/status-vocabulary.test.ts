// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { statusLabel, jobStatusLabel } from "./components/ui";

// G-4 — drift guard for the STATUS VOCABULARY, in the spirit of
// radius-tokens.test.ts and type-scale.test.ts, and it exists because the thing
// it guards has already gone wrong four separate times.
//
// One concept, one word. `statusLabel` (and its job-state sibling
// `jobStatusLabel`) is the only thing allowed to turn a wire status into the
// word an operator reads. Every drift the E-4 audit found had the same shape: a
// second copy of one of those words, typed by hand somewhere else.
//
//   `wfStatusLabel`      said "Passing" where the canonical said "Success"
//   `markColor`          re-implemented statusTone's switch, case for case
//   Jobs' status tabs    said "Healthy" where Workflows' said "Success"
//   Jobs' Schedule cell  said "⏸ paused" beside a Status cell reading "Paused"
//
// None of those looked wrong in review. Each was one screen agreeing with
// itself and disagreeing with the next screen over, which is invisible until
// you put the two side by side — or until a test does.
//
// WHAT THIS CHECKS, and why it is narrow: a bare string literal whose ENTIRE
// value is one of the canonical words. That is deliberately not "any file
// mentioning Failed": prose legitimately contains these words ("No runs
// recorded yet"), and a guard that drowns in false positives gets an exemption
// list until it means nothing. A standalone "Failed" in a .tsx file is almost
// always a label that should have come from the function.
//
// If you are adding a genuinely new use, the fix is nearly always to call
// statusLabel/jobStatusLabel rather than to add an exemption below.

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

// Every word the two canonical functions can emit, derived from the functions
// themselves rather than retyped — a hand-written list here would be the exact
// defect this file guards against, one level up.
const CANONICAL_WORDS = [
  ...new Set(
    ["success", "danger", "failure", "failed", "killed", "warning", "skipped", "cancelled", "running", "queued", "paused", "idle"].flatMap((s) => [
      statusLabel(s),
      jobStatusLabel(s),
    ]),
  ),
];

// The canonical functions live here; ui.tsx is where the words are allowed to be
// literals, because it is where they are defined.
const CANONICAL_MODULE = join(SRC, "components", "ui.tsx");

// Comments are stripped first. The files that were FIXED carry comments quoting
// the old wrong words ("said \"Passing\" where the canonical said \"Success\""),
// and a guard that flagged its own explanation would be self-defeating — the
// note recording why a mistake was made is not the mistake.
const stripComments = (src: string): string =>
  src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/(^|[^:])\/\/[^\n]*/g, "$1");

// A string literal — '…', "…" or a template with no interpolation — whose whole
// value is one canonical word. CASE is what separates a label from a wire value,
// and the distinction is the reason this guard is usable at all:
//
//   "Failed"    a display word, and the thing being guarded
//   "failed"    a WIRE value — `status === "failed"`, a query param, a test
//               fixture. Legitimate everywhere, and by far the more common.
//
// So an exact literal is matched case-SENSITIVELY. The one exception is a
// literal behind a leading glyph — "⏸ paused" — which cannot be a wire value
// whatever its case, and is precisely the drift A-4 found; those are matched
// case-insensitively.
//
// The gap this leaves, stated so nobody assumes otherwise: a lowercase display
// literal with no glyph (`label="failed"`) reads as a wire value and is not
// caught. Nothing in the app does that, and closing it would mean flagging every
// legitimate status comparison in the codebase.
function offendersIn(src: string): string[] {
  const body = stripComments(src);
  const found: string[] = [];
  for (const m of body.matchAll(/(['"`])([^'"`\n]*)\1/g)) {
    const raw = m[2].trim();
    const stripped = raw.replace(/^[^\p{L}]+/u, "").trim(); // drop a leading glyph
    if (!stripped) continue;
    const glyphed = stripped !== raw;
    const hit = CANONICAL_WORDS.some((w) => (glyphed ? w.toLowerCase() === stripped.toLowerCase() : w === stripped));
    if (hit) found.push(raw);
  }
  return found;
}

describe("status vocabulary (G-4)", () => {
  it("derives its word list from the canonical functions, not a copy", () => {
    // Strength check: if statusLabel is ever refactored into something this can
    // no longer read, the list empties and every assertion below passes forever.
    expect(CANONICAL_WORDS).toContain("Success");
    expect(CANONICAL_WORDS).toContain("Failed");
    expect(CANONICAL_WORDS).toContain("Warn");
    expect(CANONICAL_WORDS).toContain("Cancelled");
    expect(CANONICAL_WORDS).toContain("Queued"); // jobStatusLabel only — the queued fold
    expect(CANONICAL_WORDS.length).toBeGreaterThan(6);
  });

  it("bites on a hand-typed status word (self-check)", () => {
    // The exact shapes that shipped, in memory only.
    expect(offendersIn('const TABS = [{ key: "danger", label: "Failed" }]')).toEqual(["Failed"]);
    expect(offendersIn('{j.status === "paused" ? "⏸ paused" : j.schedule}')).toEqual(["⏸ paused"]);
    expect(offendersIn('<span>{w.disabled ? "Paused" : x}</span>')).toEqual(["Paused"]);
    // ...and does not bite on prose, on a wire value, or on an unrelated word.
    expect(offendersIn('hint="No runs recorded yet — every job run lands here."')).toEqual([]);
    expect(offendersIn('const STATUS_WIRE = ["running", "success", "danger"]')).toEqual([]);
    expect(offendersIn('<Btn>Cancel run</Btn>; label="Active"')).toEqual([]);
  });

  it("no view produces a status word from a string literal", () => {
    const offenders: string[] = [];
    for (const file of sourceFiles(SRC)) {
      if (file === CANONICAL_MODULE) continue;
      for (const word of offendersIn(readFileSync(file, "latin1"))) {
        offenders.push(`${file.slice(SRC.length)}: ${JSON.stringify(word)}`);
      }
    }
    expect(
      offenders,
      `a user-visible status word is being typed rather than produced by the canonical function:\n  ${offenders.join("\n  ")}\n` +
        `Call statusLabel(wireValue) — or jobStatusLabel(wireValue) for a JOB state, which does not fold queued into Running. ` +
        `That is what keeps one screen from disagreeing with the next; see the header of this file for the four times it already has.`,
    ).toEqual([]);
  });
});

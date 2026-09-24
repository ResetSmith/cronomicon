import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

// Drift guard for the TRAINING WIDGET HARNESS (TR-6).
//
// The two courses are hand-authored standalone fragments that each carry their
// own copy of the widget CSS and the deck/progress scripts. That duplication is
// deliberate (TR-D3) and it is NOT going away: extracting the harness to a
// shared .css/.js would break the property the delivery plan depends on —
// each page being a single self-contained file Moodle can serve as a File
// resource, and a reader can save and open offline.
//
// TR-D3 originally said "revisit past ~300 lines". TR-6 took the harness well
// past that, so the threshold is superseded rather than met: the reason to
// duplicate is self-containment, which does not weaken as the file grows. What
// changes at this size is the RISK — a fix applied to one course and not the
// other is now easy to miss in review and invisible at runtime.
//
// So the duplication becomes an enforced invariant instead of a convention.
// The two harnesses must be byte-identical apart from one documented
// difference: the localStorage key that separates the two courses' progress.
//
// If this fails: you changed a widget or the deck in one course only. Copy it
// across. Do not "fix" the test by loosening the comparison.

const SRC = dirname(fileURLToPath(import.meta.url));
const DOCS = join(SRC, "..", "..", "documentation");

const read = (f: string) => readFileSync(join(DOCS, f), "utf8");

/** Every <style> block in the fragment, concatenated. */
function styles(html: string): string {
  return [...html.matchAll(/<style>([\s\S]*?)<\/style>/g)].map((m) => m[1]).join("\n");
}

/** Every <script> block, with the per-course storage key normalised away. */
function scripts(html: string): string {
  return [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)]
    .map((m) => m[1])
    .join("\n")
    .replace(/am_training_(operator|admin)/g, "am_training_COURSE");
}

describe("training harness parity", () => {
  const op = read("training-operator.html");
  const ad = read("training-admin.html");

  it("both courses carry a harness at all", () => {
    expect(styles(op).length).toBeGreaterThan(1000);
    expect(scripts(op).length).toBeGreaterThan(1000);
  });

  it("the widget CSS is identical across both courses", () => {
    expect(styles(ad)).toBe(styles(op));
  });

  it("the scripts are identical apart from the progress storage key", () => {
    expect(scripts(ad)).toBe(scripts(op));
  });

  it("each course keeps its OWN progress key, so they do not share state", () => {
    expect(op).toContain('"am_training_operator"');
    expect(ad).toContain('"am_training_admin"');
    expect(op).not.toContain('"am_training_admin"');
    expect(ad).not.toContain('"am_training_operator"');
  });

  it("deck mode is an enhancement: the fragments author no slide markup", () => {
    // The slides are built at runtime from the existing document. If a
    // <section class="tr-slide"> ever appears in the SOURCE, someone has begun
    // hand-authoring slides — which forks the no-JS page view away from the
    // deck view and quietly breaks print and the offline download.
    for (const html of [op, ad]) {
      expect(html).not.toMatch(/<section[^>]*class="[^"]*tr-slide/);
    }
  });
});

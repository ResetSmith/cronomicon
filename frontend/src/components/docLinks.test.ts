// Drift guard for the doc-link registry: every DOC_LINKS target must resolve
// against the BUILT docs in backend/web/dist (the wrapped guides' h2 ids are
// generated at build time, so the dist copy — git-tracked — is the truth a
// link actually lands on). A red here means either a doc file was renamed, an
// anchor id changed, or `npm run build` wasn't run after a docs edit.
import { describe, expect, it } from "vitest";
import { readFileSync, existsSync } from "node:fs";
import { join } from "node:path";
import { DOC_LINKS } from "./docLinks";

const DIST = join(__dirname, "..", "..", "..", "backend", "web", "dist");

describe("DOC_LINKS drift guard", () => {
  for (const [key, href] of Object.entries(DOC_LINKS)) {
    it(`${key} → ${href} resolves in the built dist`, () => {
      const [path, anchor] = href.split("#");
      const file = join(DIST, path.replace(/^\//, ""));
      expect(existsSync(file), `${path} missing from dist`).toBe(true);
      if (anchor) {
        const html = readFileSync(file, "utf8");
        expect(html.includes(`id="${anchor}"`), `#${anchor} not found in ${path}`).toBe(true);
      }
    });
  }
});

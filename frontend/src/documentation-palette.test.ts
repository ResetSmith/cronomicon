// Runs in the default NODE environment, unlike radius-tokens.test.ts, which
// needs jsdom because it imports `c` from theme.ts (savedThemeMode touches
// window at module load). This guard reads theme.ts as TEXT instead — on
// purpose, see themeColors() below — so it never touches window, and jsdom
// would actively break it: vite-manuals-plugin.js resolves its own path from
// import.meta.url at module scope, which is not a file: URL under jsdom.
import { describe, expect, it } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { wrapGuide } from "../vite-manuals-plugin.js";

// Drift guard for the hand-authored HTML documentation's palette (VU-18).
//
// documentation/*.html are standalone documents — they are not part of the React
// bundle and cannot import theme.ts, so they carry their own copy of the product
// palette as CSS custom properties with hex literals. That copy was made by hand
// and there was nothing keeping it honest: Phase C replaced every colour in
// theme.ts and all five documents would have kept rendering the retired one,
// with no test, no type error and no build warning to say so.
//
// So: every hex literal in those documents must be a value that actually exists
// in theme.ts. This does not check that a colour was mapped to the RIGHT role —
// nothing can — but it does make a palette change fail loudly here, which is the
// point at which someone re-reads the mapping.
//
// Re-map by ROLE, never by picking the nearest-looking new hex:
//   page background → bg          card / callout background → panel
//   body copy       → text        muted copy / timestamps   → textMuted
//   links, accents  → primary     brand rules, active nav   → accent
//   error, danger   → danger      warning callouts          → warning
//   text on a solid fill          → onSolid (NOT #fff — see theme.ts)
//
// Two Phase-C role moves are easy to get backwards: `warning` is no longer the
// same gold as `accent` (it is a distinct orange), and `accent` gold is now the
// BRAND. An old #c49a2d used for a warn callout becomes orange; the same hex
// used for the hero spine or the h2 rule stays gold at the new value.
//
// Both theme blocks in each document must be updated — the documents ship their
// own light/dark toggle (`data-theme` / `toggleTheme()`), so updating only the
// dark block leaves half the document on the retired palette.

const SRC = dirname(fileURLToPath(import.meta.url));
const DOCS = join(SRC, "..", "..", "documentation");

// The two hand-authored manuals plus the three runner-guide fragments. The
// fragments are body-only and are wrapped at build time by
// vite-manuals-plugin.js, which injects user-manual.html's <style> verbatim —
// so they inherit the palette and should contain no colours of their own. They
// are scanned anyway: that is exactly how a stray inline style would get in.
const DOC_FILES = [
  "administrator-manual.html",
  "user-manual.html",
  "runner-install.html",
  "runner-manage.html",
  "runner-security.html",
  // The training courses (TR) are the first fragments to author colour of their
  // own — the runner guides carry none and inherit everything. Their widget CSS
  // uses var(--*) exclusively for exactly this reason, so this entry should stay
  // trivially green; it is here so that stops being true LOUDLY if anyone types
  // a hex into one.
  "training-operator.html",
  "training-admin.html",
  // The three usage guides were never added when they were introduced, so their
  // CSS went unguarded. They inherit the manuals' stylesheet like the runner
  // guides do and should carry no colour of their own — which is exactly the
  // claim this list exists to keep true (TR-5, pulled forward into TR-4 while
  // the file was open).
  "ansible-guide.html",
  "bash-guide.html",
  "powershell-guide.html",
  "python-guide.html",
];

// Colours the documents legitimately need that are NOT palette values.
//
// Deliberately empty. Every colour the manuals use now resolves to a theme
// token — including the former `#fff`-on-a-solid-fill cases, which became
// var(--onSolid) because white on the brightened dark-mode primary is 2.78:1
// (theme.ts documents that failure). If you add an entry, say WHY in a comment
// on the entry itself; this list is for a genuine non-palette need (a syntax
// highlight colour, a print-only ink), never for dodging a re-map you did not
// want to do.
const ALLOWED: Record<string, string> = {};

// HTML numeric character references are stripped before scanning: the manuals'
// TOC and callouts are full of them (`&#9656;`, `&#128213;`) and a six-digit one
// is indistinguishable from a hex colour once the `&` is gone.
const stripEntities = (html: string): string => html.replace(/&#\d+;?/g, "");

// #rrggbb or #rgb, word-bounded so `#4-1-add-a-job` fragment links and id
// selectors like `#totop` cannot match.
const HEX = /#[0-9a-fA-F]{6}\b|#[0-9a-fA-F]{3}\b/g;

function hexesIn(html: string): string[] {
  return (stripEntities(html).match(HEX) ?? []).map((h) => h.toLowerCase());
}

// The theme's colours, taken from QUOTED VALUES only. theme.ts's comments cite
// several RETIRED hexes by name ("on the old #0c1620…", "The old #2b5ea0 was a
// navy") to explain why they moved; a naive scan of the whole file would accept
// exactly the stale colours this guard exists to catch.
function themeColors(): Set<string> {
  const ts = readFileSync(join(SRC, "theme.ts"), "utf8");
  return new Set((ts.match(/"#[0-9a-fA-F]{3,8}"/g) ?? []).map((q) => q.slice(1, -1).toLowerCase()));
}

// The shared assertion, so the self-check below exercises the same code path the
// real files do rather than a re-implementation of it.
function offenders(file: string, html: string, theme: Set<string>): string[] {
  return [...new Set(hexesIn(html))].filter((h) => !theme.has(h) && !(h in ALLOWED)).map((h) => `${file}: ${h}`);
}

describe("documentation palette (VU-18)", () => {
  const theme = themeColors();

  it("reads a non-trivial palette out of theme.ts", () => {
    // Cheap sanity check on the extractor: if the quoted-value regex ever stops
    // matching, `theme` goes empty and every assertion below inverts into a
    // guard that always fails — or, worse, one that always passes.
    expect(theme.size).toBeGreaterThan(20);
    expect(theme.has("#c9a227")).toBe(true); // brand gold
    expect(theme.has("#d98a2b")).toBe(true); // warning orange — no longer the same value
  });

  it("uses no colour that theme.ts does not define", () => {
    const found: string[] = [];
    for (const file of DOC_FILES) {
      found.push(...offenders(file, readFileSync(join(DOCS, file), "utf8"), theme));
    }
    expect(
      found,
      `documentation/*.html hardcodes the product palette and has drifted from frontend/src/theme.ts.\n` +
        `Offending hexes:\n  ${found.join("\n  ")}\n` +
        `Pick the replacement by ROLE (bg / panel / text / textMuted / primary / accent / ` +
        `warning / danger / onSolid), not by which new hex looks closest — and update BOTH ` +
        `the html[data-theme="dark"] and html[data-theme="light"] blocks.`,
    ).toEqual([]);
  });

  // These two matched the theme blocks by exact literal — `html[data-theme="dark"]{`
  // with no space before the brace. The manual has since been reformatted to
  // `html[data-theme="dark"] {`, so BOTH the scan check and the self-check that
  // injects through the same literal broke, while the real colour-drift guard
  // above kept passing. A meta-test that asserts on the formatting of the file
  // it is checking fails for a reason that has nothing to do with what it
  // guards, so the anchor is a whitespace-tolerant pattern now.
  const themeBlock = (mode: "dark" | "light") => new RegExp(`html\\[data-theme="${mode}"\\]\\s*\\{`);

  it("scans the manuals rather than silently finding nothing", () => {
    // The guard would pass trivially if DOCS pointed at the wrong directory or
    // the manuals stopped carrying a palette. They do carry one, in both blocks.
    const manual = readFileSync(join(DOCS, "user-manual.html"), "utf8");
    expect(hexesIn(manual).length).toBeGreaterThan(20);
    expect(manual).toMatch(themeBlock("dark"));
    expect(manual).toMatch(themeBlock("light"));
  });

  it("bites when a document carries a colour the theme does not (self-check)", () => {
    // In memory only — never written to a real file. #c49a2d is the palette's
    // OWN retired gold, so this is the exact shape of the drift being guarded:
    // a hex that reads as plausible and is simply from the previous release.
    const manual = readFileSync(join(DOCS, "user-manual.html"), "utf8");
    const mutilated = manual.replace(themeBlock("dark"), (m) => `${m} --stale:#c49a2d;`);
    // The injection must actually have landed — otherwise this test passes by
    // checking that an unmodified file has no offender, which is the failure
    // mode the self-check exists to rule out.
    expect(mutilated).not.toBe(manual);
    expect(mutilated).toContain("--stale:#c49a2d");
    expect(offenders("user-manual.html", mutilated, theme)).toEqual(["user-manual.html: #c49a2d"]);
  });

  it("does not mistake an HTML numeric entity for a colour", () => {
    // &#128213; (📕, the Manuals link in the wrapped guides' TOC) is six digits
    // and every one of them is a valid hex digit.
    expect(hexesIn("<a>&#128213; User Manual</a>")).toEqual([]);
    expect(hexesIn('<a href="#4-1-add-a-job">4.1</a><div id="totop"></div>')).toEqual([]);
    expect(hexesIn("color:#c9a227")).toEqual(["#c9a227"]);
  });
});

// ── Fonts (VU-1, same defect one layer out) ─────────────────────────────────
//
// The manuals used to pull Outfit / JetBrains Mono / Fraunces from
// fonts.googleapis.com. That is the identical failure frontend/index.html was
// fixed for in Phase A: the Cronomicon binary serves these documents to operators
// on networks that may have no egress to Google, and a blocked font stylesheet
// does not error — it silently drops the whole manual to system-ui. The faces
// are now self-hosted from /fonts/*.woff2, the same absolute paths index.html
// uses, declared inside the manuals' <style> because vite-manuals-plugin.js
// injects that block into the wrapped runner guides and their generated <head>
// has nowhere to put a <link>.

const FONT_CDNS = ["fonts.googleapis.com", "fonts.gstatic.com"];

// Comments are stripped before the CDN check, and ONLY for that check. The
// manuals carry a comment explaining why they do not use Google Fonts — which
// names the host — and a commented-out @import cannot fetch anything. The
// palette guard above deliberately does not strip comments, so a retired hex
// left behind in a comment still gets flagged.
const stripComments = (html: string): string =>
  html.replace(/\/\*[\s\S]*?\*\//g, "").replace(/<!--[\s\S]*?-->/g, "");

// Every family the documents NAME: the `font-family:` declarations plus the
// custom properties that feed them. `var(--sans)` references are skipped — the
// stack they resolve to is checked at its definition.
function familiesNamed(html: string): string[] {
  const body = stripComments(html);
  const decls = [...body.matchAll(/(?:font-family|--sans|--sansCond|--mono|--display)\s*:\s*([^;}]+)/g)];
  return [
    ...new Set(
      decls
        .flatMap((m) => m[1].split(","))
        .map((f) => f.trim().replace(/^['"]|['"]$/g, "").toLowerCase())
        .filter((f) => f.length > 0 && !f.startsWith("var(")),
    ),
  ];
}

// The families theme.ts declares, from its three face stacks (mono / sans /
// sansCond) — including the generic fallbacks, so a doc falling back to `serif`
// where the app falls back to `system-ui, sans-serif` is caught too.
function themeFamilies(): Set<string> {
  const ts = readFileSync(join(SRC, "theme.ts"), "utf8");
  const stacks = [...ts.matchAll(/const\s+(?:mono|sans|sansCond)\s*=\s*"([^"]+)"/g)];
  return new Set(
    stacks.flatMap((m) => m[1].split(",").map((f) => f.trim().replace(/^['"]|['"]$/g, "").toLowerCase())),
  );
}

// DOCS-ONLY FACES (F2-2). The manuals' hero is set in Fraunces via their own
// `--display:'Fraunces',Georgia,serif`, and the app no longer declares a display
// face at all: VU-Q1 put the serif on the login page and sidebar wordmark, both
// of which render as raster assets, so the reserved `c.display` token styled
// nothing and was removed rather than left as an invitation.
//
// This is a deliberate, enumerated exemption and not a weakening of the check
// below. The rule the check enforces is "the manuals may not name a family the
// product does not have" — and the product does still ship these files
// (public/fonts/fraunces-*.woff2, asserted self-hosted further down). Anything
// NOT on this list still has to come from theme.ts. Add to it only for a face
// the documents legitimately own; a face the app uses belongs in theme.ts.
const DOCS_ONLY_FAMILIES = new Set(["fraunces", "georgia", "serif"]);

describe("documentation fonts (VU-1)", () => {
  const families = themeFamilies();

  it("reads the face stacks out of theme.ts", () => {
    expect(families.has("ibm plex sans")).toBe(true);
    expect(families.has("ibm plex sans condensed")).toBe(true);
    expect(families.has("ibm plex mono")).toBe(true);
    expect(families.has("outfit")).toBe(false); // retired in Phase C
    // Three faces, not four: Fraunces is docs-only since F2-2. Asserting its
    // ABSENCE here is what makes DOCS_ONLY_FAMILIES honest — if someone
    // reintroduces a display token, this fails and the exemption gets revisited
    // rather than silently covering a face the app has again.
    expect(families.has("fraunces")).toBe(false);
  });

  it("fetches no font from a third-party CDN", () => {
    const offenders: string[] = [];
    for (const file of DOC_FILES) {
      const body = stripComments(readFileSync(join(DOCS, file), "utf8"));
      for (const host of FONT_CDNS) if (body.includes(host)) offenders.push(`${file}: ${host}`);
    }
    expect(
      offenders,
      `documentation/*.html must self-host its fonts (VU-1).\n  ${offenders.join("\n  ")}\n` +
        `A deployment with no egress to Google gets system-ui and no error. Add @font-face ` +
        `blocks pointing at /fonts/*.woff2 (copy them from frontend/index.html) inside the ` +
        `manual's <style> — the wrapped runner guides have no <head> of their own.`,
    ).toEqual([]);
  });

  // I-3 (VF-14) — the guard above covers documentation/*.html. The APP SHELL,
  // which is where this defect originally was (VU-1: DM Sans, Outfit, Source
  // Sans 3 and JetBrains Mono pulled from Google), had nothing enforcing it —
  // only a sentence in AGENTS.md.
  //
  // That matters more than a docs regression would, because the failure mode is
  // SILENT: a blocked font stylesheet does not error, it just drops the whole
  // type system to system-ui, and the console looks subtly wrong to whoever is
  // running it and completely fine to whoever ships it. Whether production has
  // egress to fonts.googleapis.com has never actually been confirmed (VF-14) —
  // this test is what makes the answer stop mattering.
  it("keeps the APP SHELL off a font CDN too, not just the manuals", () => {
    const index = readFileSync(join(SRC, "..", "index.html"), "utf8");
    const body = stripComments(index);
    for (const host of FONT_CDNS) {
      expect(body, `frontend/index.html must not fetch fonts from ${host} — self-host under public/fonts/`).not.toContain(host);
    }
    // Strength check: the file must actually be the shell, and it must still be
    // declaring the self-hosted faces rather than having lost its fonts entirely.
    expect(index).toContain("@font-face");
    expect(index).toContain("/fonts/plex-sans-400-700-latin.woff2");
    // ...and the mention that IS in there is prose in a comment explaining why.
    expect(index).toContain("fonts.googleapis.com");
  });

  it("names no font family that theme.ts does not declare", () => {
    // Strength check first: an extractor that silently parses nothing would make
    // every assertion below pass forever.
    expect(familiesNamed(readFileSync(join(DOCS, "user-manual.html"), "utf8")).length).toBeGreaterThan(5);
    const offenders: string[] = [];
    for (const file of DOC_FILES) {
      const html = readFileSync(join(DOCS, file), "utf8");
      offenders.push(
        ...familiesNamed(html)
          .filter((f) => !families.has(f) && !DOCS_ONLY_FAMILIES.has(f))
          .map((f) => `${file}: ${f}`),
      );
    }
    expect(
      offenders,
      `documentation/*.html names a font the product does not use, so the manuals and the ` +
        `app would read as two different things:\n  ${offenders.join("\n  ")}\n` +
        `Use theme.ts's stacks — sans / sansCond / mono — verbatim, or add a documented ` +
        `entry to DOCS_ONLY_FAMILIES if the face genuinely belongs to the documents.`,
    ).toEqual([]);
  });

  it("actually self-hosts the faces it needs, from the paths index.html uses", () => {
    // Guards the other direction: retiring the @import without adding @font-face
    // would pass both checks above and still ship a document with no webfont.
    const manual = readFileSync(join(DOCS, "user-manual.html"), "utf8");
    const urls = [...manual.matchAll(/url\(['"]?(\/fonts\/[^'")]+)['"]?\)/g)].map((m) => m[1]);
    expect(urls.length).toBeGreaterThan(0);
    // Every path the manual asks for must be a file the product actually ships.
    // This checks public/fonts rather than index.html's declarations (F2-2):
    // Fraunces is now docs-only, so the app declares no @font-face for it, but
    // the .woff2 files are still shipped — Vite copies public/ verbatim into
    // backend/web/dist and //go:embed is recursive, so the manual's absolute
    // /fonts/* URL resolves from the same binary.
    for (const u of urls) {
      expect(existsSync(join(SRC, "..", "public", u)), `${u} is not a font the product ships`).toBe(true);
    }
    // The Plex faces the app itself renders must still be declared by index.html
    // — that is the half of the invariant a docs-only face does not relax.
    const index = readFileSync(join(SRC, "..", "index.html"), "utf8");
    for (const u of urls.filter((u) => u.includes("plex"))) {
      expect(index, `${u} is not a font frontend/index.html serves`).toContain(u);
    }
    // Whitespace-tolerant for the same reason as the theme-block anchors above:
    // the manual declares `font-display: swap`, and pinning the un-spaced form
    // made this fail on a reformat rather than on a missing declaration.
    expect(manual).toMatch(/font-display:\s*swap/);
  });

  it("carries the self-hosted faces into a wrapped runner guide", () => {
    // The guides are the reason the faces live in the <style> instead of a
    // <link>: wrapGuide builds their <head> and offers no hook for one. Both a
    // manual (/user-manual.html) and a guide (/runner-install.html) are served
    // from the root of backend/web/dist, so the same absolute /fonts/* URL
    // resolves for both.
    const style = readFileSync(join(DOCS, "user-manual.html"), "utf8").match(/<style>([\s\S]*?)<\/style>/)?.[1] ?? "";
    const guide = wrapGuide(
      readFileSync(join(DOCS, "runner-install.html"), "utf8"),
      { file: "runner-install.html", label: "Install Guide" },
      style,
    );
    expect(guide).toContain("/fonts/plex-sans-400-700-latin.woff2");
    expect(guide).toContain("/fonts/plex-mono-400-latin.woff2");
    expect(FONT_CDNS.some((h) => stripComments(guide).includes(h))).toBe(false);
  });

  it("bites on a CDN reference and on an unknown family (self-check)", () => {
    // In memory only — never written to a real file.
    const manual = readFileSync(join(DOCS, "user-manual.html"), "utf8");

    const cdn = manual.replace("<style>", "<style>\n@import url('https://fonts.googleapis.com/css2?family=Outfit');");
    expect(FONT_CDNS.some((h) => stripComments(cdn).includes(h))).toBe(true);
    // ...and the real file, whose only mention of the host is inside a comment,
    // does not trip it.
    expect(FONT_CDNS.some((h) => stripComments(manual).includes(h))).toBe(false);
    expect(manual).toContain("fonts.googleapis.com"); // it IS in there, as prose

    // The same predicate the real check uses, exemptions included — otherwise
    // this self-check would drift from the guard it is meant to exercise.
    const unknown = (html: string) => familiesNamed(html).filter((f) => !families.has(f) && !DOCS_ONLY_FAMILIES.has(f));
    const rogue = manual.replace(/--sans:\s*'IBM Plex Sans'/, "--sans:'Comic Sans MS','IBM Plex Sans'");
    // Same guard as the palette self-check: prove the injection landed, or this
    // asserts nothing more than that a clean file is clean.
    expect(rogue).not.toBe(manual);
    expect(unknown(rogue)).toEqual(["comic sans ms"]);
    expect(unknown(manual)).toEqual([]);
    // And the exemption is doing real work: the manual DOES name Fraunces, which
    // theme.ts no longer declares (F2-2).
    expect(familiesNamed(manual)).toContain("fraunces");
    expect(families.has("fraunces")).toBe(false);
  });
});

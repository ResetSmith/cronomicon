import { describe, expect, it } from "vitest";
import { GUIDES, wrapGuide } from "../../vite-manuals-plugin.js";

// wrapGuide (in the publishManuals build plugin) turns a body-only runner-guide
// fragment into a full standalone document matching the manuals. These guard the
// non-trivial string transforms — the equivalent of the old in-app RunnerGuides
// render tests, adapted to the standalone-file model the Runners buttons now open.

const FRAGMENT = `<h1>Runner install &amp; configure guide (R7.2)</h1>
<p>Intro paragraph mentioning <code>runner-security.html</code> as plain text.</p>
<h2>0 · Before you begin</h2>
<p>Have these ready.</p>
<blockquote><p>See <a href="./runner-manage.html"><code>runner-manage.html</code></a> for day-2 ops.</p></blockquote>
<h2>1 · Install</h2>
<pre><code>curl -fsSL https://example/install.sh | sh</code></pre>
<table><thead><tr><th>Var</th></tr></thead><tbody><tr><td>X</td></tr></tbody></table>`;

// Stand-ins; the real build extracts the manuals' <style> and sidebar logo
// from user-manual.html so the guides share the manuals' look.
const STYLE = ".hero{}";
const BRAND = { logo: "data:image/png;base64,AAAA" };
const wrapped = wrapGuide(FRAGMENT, { file: "runner-install.html", label: "Runner Install Guide" }, STYLE, BRAND);

describe("wrapGuide — standalone runner guide generation", () => {
  it("emits a self-contained document with the manuals' style and theme toggle", () => {
    expect(wrapped.startsWith("<!doctype html>")).toBe(true);
    expect(wrapped).toContain("<style>.hero{}</style>");
    expect(wrapped).toContain("function toggleTheme(");
    expect(wrapped).toContain('data-theme="dark"');
  });

  it("renders the manuals' masthead: logo, wordmark and the guide's own title", () => {
    // Same logo as the manuals, single-sourced from user-manual.html; the hero
    // is the manuals' too — the Cronomicon wordmark over the document's title.
    expect(wrapped).toContain('<div class="logo"><img src="data:image/png;base64,AAAA" alt="Cronomicon logo"></div>');
    expect(wrapped).toMatch(
      /<div class="hero">\s*<div class="eyebrow">Cronomicon<\/div>\s*<h1>Runner install &amp; configure guide<\/h1>/,
    );
  });

  it("lifts the leading <h1> into the hero and splits off the version tag", () => {
    // The "(R7.2)" tag becomes a version chip beside the document-kind chip.
    expect(wrapped).toContain('<span class="chip">Runner Guide</span>');
    expect(wrapped).toContain("Version <b>R7.2</b>");
    // ...and it is not left duplicated in the body as a bare <h1>.
    expect(wrapped.match(/<h1>/g)?.length).toBe(1);
  });

  it("names a registered guide's kind in its chip", () => {
    const bash = GUIDES.find((g) => g.file === "bash-guide.html")!;
    expect(wrapGuide(FRAGMENT, bash, STYLE, BRAND)).toContain('<span class="chip">Usage Guide</span>');
  });

  it("auto-builds a TOC from the <h2>s and gives each an id", () => {
    expect(wrapped).toContain('<a href="#0-before-you-begin"><span class="ic">&#9656;</span> 0 · Before you begin</a>');
    expect(wrapped).toContain('<a href="#1-install">');
    // The id survives the chapter-number rewrite below.
    expect(wrapped).toContain('<h2 id="0-before-you-begin"><span class="secnum">0</span> Before you begin</h2>');
    expect(wrapped).toContain('<h2 id="1-install"><span class="secnum">1</span> Install</h2>');
  });

  it("sets each <h2> as an alternating chapter band, leaving the intro above them", () => {
    const chaps = [...wrapped.matchAll(/<section class="(chap(?: alt)?)">\n<h2/g)].map((m) => m[1]);
    expect(chaps).toEqual(["chap", "chap alt"]);
    expect(wrapped).toMatch(/<p>Intro paragraph[\s\S]*?<\/p>\n<section class="chap">/);
    // Every section opened is closed, so the band wraps the whole chapter.
    expect(wrapped.match(/<section\b/g)?.length).toBe(wrapped.match(/<\/section>/g)?.length);
  });

  it("leaves a course's modules unwrapped for its deck script", () => {
    // The training deck slices main's direct children at every h2/h3; a
    // section around a module would hide its headings from that walk.
    const course = GUIDES.find((g) => g.course)!;
    const out = wrapGuide(FRAGMENT, course, STYLE, BRAND);
    expect(out).not.toContain('class="chap');
    expect(out).toContain('<h2 id="0-before-you-begin">0 · Before you begin</h2>');
  });

  it("boxes bare <pre> in .codeblock so it gets code styling + copy button", () => {
    expect(wrapped).toContain('<div class="codeblock"><pre><code>curl');
    expect(wrapped).toContain("</code></pre></div>");
  });

  it("relabels cross-guide anchors to the friendly name, keeping the sibling .html href", () => {
    // The href stays the sibling file (resolves natively as a standalone doc);
    // only the visible label is rewritten from the bare filename.
    expect(wrapped).toContain('href="./runner-manage.html">Runner Config Guide</a>');
    expect(wrapped).not.toContain(">runner-manage.html</code></a>");
  });

  it("links the sibling guides and both manuals, marking the current guide active", () => {
    expect(wrapped).toContain('<a class="active"><span class="ic">&#9656;</span> Runner Install Guide</a>');
    expect(wrapped).toContain('href="runner-manage.html"');
    expect(wrapped).toContain('href="runner-security.html"');
    expect(wrapped).toContain('href="user-manual.html"');
    expect(wrapped).toContain('href="administrator-manual.html"');
  });

  it("carries no dead deploy-doc anchors (every href resolves)", () => {
    // The allowlist is DERIVED from the real GUIDES list, not restated here: the
    // original hardcoded copy of the basenames failed the moment a new guide was
    // registered, which is a guard failing on a correct change rather than a
    // wrong one. Anything wrapGuide links must be a registered guide or a manual.
    const guideNames = GUIDES.map((g) => g.file.replace(/\.html$/, "")).join("|");
    const guideHref = new RegExp(`^\\.?/?(${guideNames})\\.html$`);
    const hrefs = Array.from(wrapped.matchAll(/href="([^"]+)"/g)).map((m) => m[1]);
    for (const h of hrefs) {
      const ok =
        h.startsWith("#") ||
        h.startsWith("http") ||
        guideHref.test(h) ||
        /^(user|administrator)-manual\.html$/.test(h);
      expect(ok, `unexpected href: ${h}`).toBe(true);
    }
  });
});

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

// Stand-ins; the real build extracts the manuals' <style>, sidebar logo and
// hero lead from user-manual.html so the guides share the manuals' masthead.
const STYLE = ".hero{}";
const BRAND = {
  logo: "data:image/png;base64,AAAA",
  lead: "The self-hosted <strong>Script Orchestrator</strong> — stand-in subtitle.",
};
const wrapped = wrapGuide(FRAGMENT, { file: "runner-install.html", label: "Runner Install Guide" }, STYLE, BRAND);

describe("wrapGuide — standalone runner guide generation", () => {
  it("emits a self-contained document with the manuals' style and theme toggle", () => {
    expect(wrapped.startsWith("<!doctype html>")).toBe(true);
    expect(wrapped).toContain("<style>.hero{}</style>");
    expect(wrapped).toContain("function toggleTheme(");
    expect(wrapped).toContain('data-theme="dark"');
  });

  it("renders the manuals' masthead: logo, Cronomicon title and lead subtitle", () => {
    // Same logo/title/subtitle as the manuals, single-sourced from user-manual.html.
    expect(wrapped).toContain('<div class="logo"><img src="data:image/png;base64,AAAA" alt="Cronomicon logo"></div>');
    expect(wrapped).toMatch(/<div class="hero">[\s\S]*<h1>Cronomicon<\/h1>/);
    expect(wrapped).toContain(`<p class="lead">${BRAND.lead}</p>`);
  });

  it("lifts the leading <h1> into a Guide chip and splits off the version tag", () => {
    // The doc title becomes a hero chip; the "(R7.2)" tag becomes a version chip.
    expect(wrapped).toContain("Guide <b>Runner install &amp; configure guide</b>");
    expect(wrapped).toContain("Version <b>R7.2</b>");
    // ...and it is not left duplicated in the body as a bare <h1>.
    expect(wrapped.match(/<h1>/g)?.length).toBe(1);
  });

  it("auto-builds a TOC from the <h2>s and gives each an id", () => {
    expect(wrapped).toContain('<h2 id="0-before-you-begin">0 · Before you begin</h2>');
    expect(wrapped).toContain('<h2 id="1-install">1 · Install</h2>');
    expect(wrapped).toContain('<a href="#0-before-you-begin">');
    expect(wrapped).toContain('<a href="#1-install">');
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

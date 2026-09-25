// Publishes the HTML documentation to the app.
//
// The docs live in documentation/ (their single source) and open in a new tab,
// not inside the React app. This Vite plugin makes them reachable at /<name>.html
// with no backend change:
//   • on build, it writes each doc into the output dir (backend/web/dist), so the
//     Go binary's `go:embed all:dist` picks it up and spa.go serves it as a real
//     file at /<name>.html;
//   • in `vite dev`, a middleware serves the same paths on the fly.
//
// Three published classes:
//   • MANUALS — full self-contained documents (user/administrator manual); copied
//     verbatim.
//   • GUIDES — the runner Install/Config/Security guides, authored as body-only
//     HTML fragments (documentation/runner-*.html, also the operator-repo docs).
//     They are WRAPPED at build time in the manuals' shell (see wrapGuide) so they
//     publish as standalone styled docs that match the manuals — reusing the
//     manuals' own <style> so the look never drifts. The Runners view links
//     straight to these files; there is no in-app guide renderer.
//   • FILES — non-HTML repo files copied verbatim, e.g. the runner install script
//     (backend/deploy/runner-install.sh → /runner-install.sh), so the Runners
//     panel's Download button and install one-liner work from the app itself —
//     a runner host reaches the Cronomicon server by definition, GitLab maybe not.
//
// Kept as plain JS (no TS) so the node:fs usage never enters the type-checked src
// graph — see the note in vite.config.ts. Add a MANUALS entry to publish another
// standalone doc, a GUIDES entry to publish another wrapped fragment, or a FILES
// entry to publish another verbatim file.
import { copyFileSync, existsSync, readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { join } from "node:path";

const DOCS_DIR = fileURLToPath(new URL("../documentation", import.meta.url));
const REPO_DIR = fileURLToPath(new URL("..", import.meta.url));

const MANUALS = ["user-manual.html", "administrator-manual.html"];
const FILES = [
  {
    src: "backend/deploy/runner-install.sh", // repo-relative single source
    publish: "runner-install.sh",
    type: "text/x-shellscript; charset=utf-8",
  },
  {
    // The annotated runner.env skeleton — fetched by the "Provision a Runner"
    // helper (runner-provision.ts), which patches values into it verbatim so
    // the emitted env can't drift from this single source (Phase 6).
    src: "backend/deploy/cronomicon-runner.env.example",
    publish: "cronomicon-runner.env.example",
    type: "text/plain; charset=utf-8",
  },
];
// Exported (like wrapGuide) so runner-guide-wrap.test.ts can derive its
// "every href resolves" allowlist from the real list rather than restating the
// basenames — a hardcoded copy there failed on the first new entry added after
// it was written (the Operator Course, TR-1).
export const GUIDES = [
  { file: "runner-install.html", label: "Runner Install Guide" },
  { file: "runner-manage.html", label: "Runner Config Guide" }, // the app calls runner-manage the "Config Guide"
  { file: "runner-security.html", label: "Runner Security Guide" },
  // Usage guides ride the same wrap pipeline; `eyebrow` overrides the hero
  // eyebrow (default "🔧 Runner Guide") so a non-runner guide isn't mislabeled.
  { file: "ansible-guide.html", label: "Ansible Guide", eyebrow: "&#128215; Usage Guide" },
  { file: "bash-guide.html", label: "Bash Guide", eyebrow: "&#128215; Usage Guide" },
  { file: "powershell-guide.html", label: "PowerShell Guide", eyebrow: "&#128215; Usage Guide" },
  { file: "python-guide.html", label: "Python Guide", eyebrow: "&#128215; Usage Guide" },
  // Training courses (TR, the training-course plan) ride the same wrap
  // pipeline as the guides: body-only fragments, the manuals' <style>, a TOC
  // built from their <h2>s. They carry their own inline <style>/<script> for the
  // interactive widgets, which pass through wrapGuide untouched.
  // `course: true` moves an entry from the sidebar's Guides section to its
  // Courses section; everything else about the wrap is identical.
  { file: "training-operator.html", label: "Operator Course", eyebrow: "&#127891; Training", course: true },
  { file: "training-admin.html", label: "Administrator Course", eyebrow: "&#127891; Training", course: true },
];

// The manuals are the single source of visual identity; the wrapped guides
// reuse their <style>, sidebar logo and hero subtitle verbatim so the look
// (logo/title/subtitle) can never drift from the manuals'. Read lazily.
function manualAssets() {
  const html = readFileSync(join(DOCS_DIR, "user-manual.html"), "utf8");
  return {
    style: html.match(/<style>([\s\S]*?)<\/style>/)?.[1] ?? "",
    logo: html.match(/<div class="logo"><img src="(data:image\/[^"]+)"/)?.[1] ?? "",
    lead: html.match(/<p class="lead">([\s\S]*?)<\/p>/)?.[1]?.trim() ?? "",
  };
}

const stripTags = (s) => s.replace(/<[^>]+>/g, "").replace(/&amp;/g, "&").trim();
const esc = (s) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
const slugify = (s) =>
  stripTags(s).toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 60);

// wrapGuide turns a body-only fragment into a full standalone document that
// matches the manuals: reuses their <style>, sidebar logo and hero masthead
// (eyebrow / "Cronomicon" h1 / lead subtitle — the guide's own title moves to a
// "Guide" chip), adds a sidebar TOC auto-built from the fragment's <h2>s,
// sibling-guide + manual cross-links, and a topbar with the same light/dark
// toggle (sharing the manuals' `am_manual_theme` preference).
// Exported for unit testing (runner-guide-wrap.test.ts).
export function wrapGuide(fragment, current, style, brand = {}) {
  let body = fragment.replace(/\r\n/g, "\n");

  // Lift the leading <h1> (doc title, e.g. "… guide (R7.2)") into the hero and
  // drop it from the body so the title isn't rendered twice. A trailing
  // "(…)" is split off as a version chip.
  let title = current.label;
  let version = "";
  const h1 = body.match(/^\s*<h1\b[^>]*>([\s\S]*?)<\/h1>\s*/);
  if (h1) {
    const raw = stripTags(h1[1]);
    const vm = raw.match(/\(([^)]+)\)\s*$/);
    if (vm) {
      title = raw.slice(0, vm.index).trim();
      version = vm[1].trim();
    } else {
      title = raw;
    }
    body = body.slice(h1[0].length);
  }

  // Rewrite cross-guide anchors' visible label to the friendly guide name; the
  // href stays the sibling .html, which resolves natively as a standalone file.
  for (const g of GUIDES) {
    const base = g.file.replace(/\.html$/, "");
    const re = new RegExp(`(<a\\b[^>]*href="[^"]*${base}\\.html[^"]*"[^>]*>)[\\s\\S]*?(</a>)`, "g");
    body = body.replace(re, `$1${g.label}$2`);
  }

  // Box bare <pre> in .codeblock so it picks up the manuals' code-block styling
  // (background/border/radius) and copy button — bare <pre> only gets the mono font.
  body = body.replace(/<pre>/g, '<div class="codeblock"><pre>').replace(/<\/pre>/g, "</pre></div>");

  // Give each <h2> an id and collect the on-this-page TOC.
  const toc = [];
  body = body.replace(/<h2\b([^>]*)>([\s\S]*?)<\/h2>/g, (m, attrs, inner) => {
    const existing = attrs.match(/\bid="([^"]+)"/);
    const id = existing ? existing[1] : slugify(inner) || `sec-${toc.length + 1}`;
    toc.push({ id, text: stripTags(inner) });
    return existing ? m : `<h2${attrs} id="${id}">${inner}</h2>`;
  });

  const tocLinks = toc
    .map((t) => `<a href="#${t.id}"><span class="ic">&#9656;</span> ${esc(t.text)}</a>`)
    .join("\n      ");
  const siblingLink = (g) =>
    g.file === current.file
      ? `<a class="active"><span class="ic">&#9656;</span> ${g.label}</a>`
      : `<a href="${g.file}"><span class="ic">&#9656;</span> ${g.label}</a>`;
  const guideLinks = GUIDES.filter((g) => !g.course).map(siblingLink).join("\n      ");
  const courseLinks = GUIDES.filter((g) => g.course).map(siblingLink).join("\n      ");

  return `<!doctype html>
<html lang="en" data-theme="dark">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Cronomicon — ${esc(title)}</title>
<style>${style}</style>
</head>
<body>
<div id="scrim" onclick="document.body.classList.remove('nav-open')"></div>
<aside id="sidebar">
  <div class="brand">
    ${brand.logo ? `<div class="logo"><img src="${brand.logo}" alt="Cronomicon logo"></div>` : ""}
    <div class="bt"><b>Cronomicon</b><span>${current.label}${version ? " · " + esc(version) : ""}</span></div>
  </div>
  <nav class="toc" id="toc">
    <div class="grp">On this page</div>
      ${tocLinks}
    <details class="grp-sec"${current.course ? "" : " open"}><summary class="grp">Guides</summary>
      ${guideLinks}
    </details>
    <details class="grp-sec"><summary class="grp">Manuals</summary>
      <a href="user-manual.html">&#128213; User Manual &rarr;</a>
      <a href="administrator-manual.html">&#9881;&#65039; Administrator Manual &rarr;</a>
    </details>
    <details class="grp-sec"${current.course ? " open" : ""}><summary class="grp">Courses</summary>
      ${courseLinks}
    </details>
  </nav>
  <div class="sb-foot">Script Orchestrator · single Go binary + SQLite · self-hosted</div>
</aside>
<div id="wrap">
<header id="topbar">
  <button class="tbtn" id="menuToggle" aria-label="Toggle navigation" onclick="document.body.classList.toggle('nav-open')">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 6h18M3 12h18M3 18h18"/></svg>
  </button>
  <div class="crumbs" id="crumbs"><b>Cronomicon</b> · ${esc(current.label)}</div>
  <button class="tbtn" onclick="window.print()" title="Print or save as PDF">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M6 9V2h12v7M6 18H4a2 2 0 0 1-2-2v-5a2 2 0 0 1 2-2h16a2 2 0 0 1 2 2v5a2 2 0 0 1-2 2h-2M6 14h12v8H6z"/></svg>
    Print
  </button>
  <button class="tbtn" id="themeBtn" onclick="toggleTheme()" title="Toggle light / dark">
    <svg id="themeIcon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>
    <span id="themeLabel">Light</span>
  </button>
</header>
<main>
  <div class="hero">
    <div class="eyebrow">${current.eyebrow || "&#128295; Runner Guide"}</div>
    <h1>Cronomicon</h1>
    ${brand.lead ? `<p class="lead">${brand.lead}</p>` : ""}
    <div class="meta">
      <span class="chip">Guide <b>${esc(title)}</b></span>
      ${version ? `<span class="chip">Version <b>${esc(version)}</b></span>` : ""}
    </div>
  </div>
  ${body}
</main>
</div>
<button id="totop" onclick="window.scrollTo({top:0,behavior:'smooth'})" aria-label="Back to top">
  <svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2.4"><path d="M12 19V5M5 12l7-7 7 7"/></svg>
</button>
<script>
function applyTheme(t){
  document.documentElement.setAttribute('data-theme',t);
  document.getElementById('themeLabel').textContent=t==='dark'?'Light':'Dark';
  var ic=document.getElementById('themeIcon');
  ic.innerHTML=t==='dark'
    ? '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>'
    : '<path d="M21 12.8A9 9 0 1 1 11.2 3 7 7 0 0 0 21 12.8z"/>';
  try{localStorage.setItem('am_manual_theme',t)}catch(e){}
}
function toggleTheme(){applyTheme(document.documentElement.getAttribute('data-theme')==='dark'?'light':'dark')}
(function(){var s;try{s=localStorage.getItem('am_manual_theme')}catch(e){}applyTheme(s==='light'?'light':'dark')})();
document.querySelectorAll('.codeblock').forEach(function(cb){
  var pre=cb.querySelector('pre'); if(!pre||cb.querySelector('.copybtn'))return;
  var b=document.createElement('button'); b.className='copybtn'; b.textContent='Copy';
  b.onclick=function(){navigator.clipboard.writeText(pre.innerText).then(function(){b.textContent='Copied';b.classList.add('ok');setTimeout(function(){b.textContent='Copy';b.classList.remove('ok')},1400)})};
  cb.appendChild(b);
});
(function(){
  var links=[].slice.call(document.querySelectorAll('nav.toc a[href^="#"]'));
  var map={}; links.forEach(function(a){map[a.getAttribute('href').slice(1)]=a});
  if(!links.length)return;
  var obs=new IntersectionObserver(function(es){es.forEach(function(e){if(e.isIntersecting){var a=map[e.target.id];if(!a)return;links.forEach(function(l){l.classList.remove('active')});a.classList.add('active')}})},{rootMargin:'-64px 0px -72% 0px',threshold:0});
  document.querySelectorAll('main h2[id]').forEach(function(h){obs.observe(h)});
})();
window.addEventListener('scroll',function(){document.getElementById('totop').classList.toggle('show',window.scrollY>500)});
document.querySelectorAll('nav.toc a').forEach(function(a){a.addEventListener('click',function(){document.body.classList.remove('nav-open')})});
</script>
</body>
</html>`;
}

// buildGuide reads a fragment and returns the wrapped standalone document.
function buildGuide(guide, assets) {
  const src = join(DOCS_DIR, guide.file);
  if (!existsSync(src)) return null;
  return wrapGuide(readFileSync(src, "utf8"), guide, assets.style, assets);
}

export function publishManuals() {
  return {
    name: "cronomicon-publish-manuals",
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const path = (req.url || "").split("?")[0];
        const manual = MANUALS.find((m) => path === "/" + m);
        if (manual && existsSync(join(DOCS_DIR, manual))) {
          res.setHeader("Content-Type", "text/html; charset=utf-8");
          res.end(readFileSync(join(DOCS_DIR, manual)));
          return;
        }
        const guide = GUIDES.find((g) => path === "/" + g.file);
        if (guide) {
          const html = buildGuide(guide, manualAssets());
          if (html) {
            res.setHeader("Content-Type", "text/html; charset=utf-8");
            res.end(html);
            return;
          }
        }
        const file = FILES.find((f) => path === "/" + f.publish);
        if (file && existsSync(join(REPO_DIR, file.src))) {
          res.setHeader("Content-Type", file.type);
          res.end(readFileSync(join(REPO_DIR, file.src)));
          return;
        }
        next();
      });
    },
    writeBundle(options) {
      const outDir = options.dir ?? fileURLToPath(new URL("../backend/web/dist", import.meta.url));
      for (const name of MANUALS) {
        const src = join(DOCS_DIR, name);
        if (existsSync(src)) copyFileSync(src, join(outDir, name));
      }
      const assets = manualAssets();
      for (const guide of GUIDES) {
        const html = buildGuide(guide, assets);
        if (html) writeFileSync(join(outDir, guide.file), html);
      }
      for (const file of FILES) {
        const src = join(REPO_DIR, file.src);
        // Unlike the manuals (best-effort), a missing FILES source fails the
        // build: the Runners view's Download button and install one-liner
        // depend on /runner-install.sh existing, and a silent skip would ship
        // an app whose install command curl-pipes the SPA fallback into bash.
        if (!existsSync(src)) throw new Error(`vite-manuals-plugin: FILES source missing: ${src}`);
        copyFileSync(src, join(outDir, file.publish));
      }
    },
  };
}

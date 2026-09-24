// Design tokens. The palette, the type scale, the radii — everything visual the
// app reads at render. Phase C (the visual-updates plan) replaced the
// prototype's colours and faces; the token architecture is unchanged.
//
// Theming: `c` is a MUTABLE token object. ThemeProvider (theme-context.tsx)
// calls applyTheme(mode) and re-renders the tree, so every existing `c.xxx`
// read picks up the active palette without threading a context through all
// 30 view files. Components must read c.* during render (they all do — inline
// styles), never capture token values in module-level constants.

export type ThemeMode = "dark" | "light";

interface Palette {
  bg: string;
  panel: string; // card / surface background (prototype bgCard)
  panel2: string; // secondary surface: chip buttons, expanded rows (prototype bgCardHover)
  panelHover: string; // row / card hover (prototype bgCardHover)
  panelInput: string; // input background (prototype bgInput)
  border: string;
  borderLight: string; // lighter separators between table rows
  // The boundary of a FORM CONTROL, and the one border that carries an
  // accessibility requirement (C-4). WCAG 1.4.11 wants 3:1 for anything needed
  // to identify a control; `border` above is a decorative separator between
  // surfaces and is exempt, but an input's edge is not — and at the hairline
  // tone a field was genuinely hard to locate, since panelInput and panel sit
  // within ~1.1:1 of each other in dark mode. Used by inputStyle().
  borderStrong: string;
  // Ink for a SOLID fill (primary / danger buttons, filled badges). Not always
  // white: dark mode's fills were brightened for legibility on the deeper ground
  // (VU-8), and white on #4da3d9 is 2.78:1 — a fail. Near-black on that same
  // fill is 6.4:1. Light mode's fills are dark, so there it stays white. The
  // point of the token is that "solid button" no longer means "white text".
  onSolid: string;
  text: string;
  textSec: string;
  textMuted: string; // table headers, timestamps, footers
  primary: string;
  primaryHover: string;
  primaryBg: string; // tinted background for primary badges/alerts
  accent: string;
  accentBg: string;
  success: string;
  successBg: string;
  danger: string;
  dangerBg: string;
  warning: string;
  warningBg: string;
  info: string;
  infoBg: string;
  // Sidebar tokens. The rail is no longer dark-in-both-modes (VU-Q2): light mode
  // gets a true light rail, so these track the theme like everything else.
  sidebar: string;
  sidebarHover: string;
  sidebarText: string;
  sidebarTextActive: string;
  sidebarActive: string;
  shadow: string;
  shadowSm: string;
  mono: string;
  sans: string;
  // ── Faces (VU-7 / VU-Q1) ───────────────────────────────────────────────────
  // `sansCond` is IBM Plex Sans Condensed, for the uppercase structural type —
  // column heads, eyebrows, field labels. Condensed there is not a style choice:
  // it buys real characters-per-pixel in History's nine-column table.
  //
  // There is NO display face here, and that is deliberate (F2-2 / VF-Q1). The
  // app carried a reserved `display` token for Fraunces that nothing set:
  // VU-Q1's hybrid put the serif on the login page and the sidebar wordmark
  // only, and both render the wordmark as a raster asset, so there was no text
  // for it to style. A reserved-but-unused token is an attractive nuisance —
  // the next person wanting a serif page title finds the token and not the
  // reasoning — so it is gone rather than sitting here inviting use.
  //
  // Fraunces itself is NOT gone: the two hand-authored manuals in
  // documentation/ legitimately set their hero in it, with their own inlined
  // @font-face blocks pointing at the self-hosted /fonts/fraunces-*.woff2.
  // Those files stay. It is now a docs-only face, which is exactly what
  // documentation-palette.test.ts records.
  sansCond: string;
  // ── Type scale (VU-7) ──────────────────────────────────────────────────────
  // The app's type lived entirely in a 10-14px band in one family, so hierarchy
  // had nowhere to go and fell to colour. Six steps, as tokens rather than the
  // inline literals that produced the band in the first place.
  //
  //   fontDisplay 36  the login wordmark's scale; brand surfaces only
  //   fontTitle   24  page titles — the step that was missing entirely
  //   fontHead    16  card and section headings
  //   fontBody    14  body copy, form fields, buttons
  //   fontSm      13  table cells, secondary copy
  //   fontXs      11  eyebrows, column heads, chips, timestamps
  //
  // 10px is gone: it was below the point where Plex's uppercase stays legible
  // on a dense row, and it was doing the same job as 11.
  fontDisplay: number;
  fontTitle: number;
  fontHead: number;
  fontBody: number;
  fontSm: number;
  fontXs: number;
  // ── Radii (VU-6) ───────────────────────────────────────────────────────────
  // THREE values, and only three. The app had grown nine (6×92, 4×50, 8×29,
  // 3×25, 10×12, 20×9, 5×7, 2×3, 7×2, 14×1) with no system behind which was
  // which, which is a large part of why the console read as templated. They live
  // on the palette so they are reached the same way as every other token —
  // `c.radiusChip` — rather than as literals a future edit can quietly re-fragment.
  //
  //   radiusChip    chips, tags, buttons, inputs, and every small control
  //   radiusSurface cards, panels, modals, banners, tiles — anything that frames
  //   radiusPill    STATUS pills only; the one shape allowed to be fully round,
  //                 which is what makes a status readable at a glance in a row
  //                 that also carries type, tag and source chips
  radiusChip: number;
  radiusSurface: number;
  radiusPill: number;
}

// Shared by both palettes: geometry and type do not change with the colour scheme.
const radii = { radiusChip: 4, radiusSurface: 8, radiusPill: 999 };
const scale = { fontDisplay: 36, fontTitle: 24, fontHead: 16, fontBody: 14, fontSm: 13, fontXs: 11 };

const mono = "'IBM Plex Mono', ui-monospace, monospace";
const sans = "'IBM Plex Sans', system-ui, sans-serif";
const sansCond = "'IBM Plex Sans Condensed', 'IBM Plex Sans', system-ui, sans-serif";
const faces = { mono, sans, sansCond };

// The palettes. Still deliberately muted and desaturated — that is the right
// call for a screen an operator stares at all day, and Phase C sharpened it
// rather than brightening it. Do not substitute Tailwind defaults.
//
// Every text/background pair here is checked to WCAG AA by the matrix recorded
// in the visual-updates plan §C-4 (4.5:1 body, 3:1 for control
// boundaries). If you change a value, re-run that audit — several of these are
// within 0.2 of the threshold and were tuned to clear it.
const palettes: Record<ThemeMode, Palette> = {
  dark: {
    // The ground is deeper than the prototype's (VU-8). A near-black blue gives
    // the tinted status fills somewhere to sit — on the old #0c1620 a success
    // tint and a panel were within a few points of each other.
    bg: "#0a1119",
    panel: "#101a26",
    panel2: "#182534",
    panelHover: "#182534",
    panelInput: "#0d1621",
    border: "#22344a",
    borderLight: "#1a2838",
    borderStrong: "#566c86",
    onSolid: "#0a1119",
    // Two greys plus one emphasis near-white (VU-7). `text` is no longer a third
    // desaturated blue-grey: it is the emphasis tone, and the hierarchy between
    // the three is now visible rather than nominal.
    text: "#e8eff7",
    textSec: "#9db2c8",
    textMuted: "#78909f",
    primary: "#4da3d9",
    primaryHover: "#6fb8e5",
    primaryBg: "rgba(77,163,217,0.15)",
    // Gold is THE BRAND now, not a warning colour (VU-Q4): active nav, brand
    // rules, and Phase D's playhead. That is only possible because warning moved.
    accent: "#c9a227",
    accentBg: "rgba(201,162,39,0.14)",
    success: "#45b26b",
    successBg: "rgba(69,178,107,0.14)",
    danger: "#e05a5a",
    dangerBg: "rgba(224,90,90,0.14)",
    // Warning is orange, no longer the same hex as accent. With both on gold a
    // "warn" run and an "in flight" run were indistinguishable in light mode.
    warning: "#d98a2b",
    warningBg: "rgba(217,138,43,0.14)",
    info: "#4da3d9",
    infoBg: "rgba(77,163,217,0.14)",
    sidebar: "#0c1420",
    sidebarHover: "#16222f",
    sidebarText: "#93a9bf",
    sidebarTextActive: "#f0e2b8",
    // The active nav item is where the brand gold first shows up in the product.
    sidebarActive: "rgba(201,162,39,0.16)",
    shadow: "0 2px 12px rgba(0,0,0,0.45)",
    shadowSm: "0 1px 4px rgba(0,0,0,0.3)",
    ...faces,
    ...scale,
    ...radii,
  },
  light: {
    bg: "#f4f6fa",
    panel: "#ffffff",
    panel2: "#eef2f7",
    panelHover: "#eef2f7",
    panelInput: "#ffffff",
    border: "#d5dde8",
    borderLight: "#e6ebf2",
    borderStrong: "#828fa0",
    onSolid: "#ffffff",
    text: "#131e2b",
    textSec: "#46596e",
    textMuted: "#596c80",
    // A luminance sibling of the dark mode's #4da3d9, not a different
    // personality (VU-9). The old #2b5ea0 was a navy — light mode read as a
    // different product rather than the same one with the lights on.
    primary: "#15709f",
    primaryHover: "#0f5c85",
    primaryBg: "rgba(21,112,159,0.10)",
    accent: "#7c6311",
    accentBg: "rgba(124,99,17,0.12)",
    success: "#1e7f45",
    successBg: "rgba(30,127,69,0.10)",
    danger: "#c0342f",
    dangerBg: "rgba(192,52,47,0.10)",
    warning: "#9c5f11",
    warningBg: "rgba(156,95,17,0.12)",
    info: "#15709f",
    infoBg: "rgba(21,112,159,0.10)",
    // A TRUE light rail (VU-Q2). The rail stayed dark in light mode, which is
    // the classic bootstrap-admin split and the most dated thing about the
    // theme. White rail, hairline border, gold-tinted active item — so the
    // brand accent reads identically in both modes.
    sidebar: "#ffffff",
    sidebarHover: "#eef2f7",
    sidebarText: "#46596e",
    sidebarTextActive: "#131e2b",
    sidebarActive: "rgba(124,99,17,0.13)",
    shadow: "0 2px 12px rgba(15,30,50,0.08)",
    shadowSm: "0 1px 4px rgba(15,30,50,0.05)",
    ...faces,
    ...scale,
    ...radii,
  },
};

const STORAGE_KEY = "am_theme";

export function savedThemeMode(): ThemeMode {
  // URL override (?theme=light|dark) — handy for testing/deep links; the choice
  // then persists via applyTheme like any toggle.
  const q = new URLSearchParams(window.location.search).get("theme");
  if (q === "light" || q === "dark") return q;
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    if (v === "light" || v === "dark") return v;
  } catch {
    /* private browsing */
  }
  return "dark";
}

// The live token object. Mutated in place by applyTheme so existing imports
// (`import { c } from "../theme"`) stay valid across the whole app.
export const c: Palette = { ...palettes[savedThemeMode()] };

export function applyTheme(mode: ThemeMode): void {
  Object.assign(c, palettes[mode]);
  try {
    localStorage.setItem(STORAGE_KEY, mode);
  } catch {
    /* private browsing */
  }
  document.body.style.background = c.bg;
  // The bridge from the mutable token object to the one global stylesheet
  // (global-css.ts): :focus-visible is a pseudo-class, so it cannot be an inline
  // style and cannot read `c` directly. Re-published on every toggle so the
  // focus ring changes colour with the theme like everything else.
  document.documentElement.style.setProperty("--am-focus", c.primary);
  document.documentElement.style.setProperty("--am-sans", c.sans);
}

export const outcomeColor = (outcome?: string | null): string => {
  switch (outcome) {
    case "success":
      return c.success;
    case "failure":
      return c.danger;
    case "warning":
      return c.warning;
    default:
      return c.info;
  }
};

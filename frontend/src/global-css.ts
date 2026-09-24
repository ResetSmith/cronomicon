// The one global stylesheet. Lives here rather than inline in main.tsx so it can
// be asserted on in tests (global-css.test.ts) — main.tsx mounts the app as a
// side effect of import, which makes it untestable.
//
// Everything else in this app is inline styles, because the theme tokens (`c`,
// theme.ts) are mutated in place and a static stylesheet cannot read them. The
// two things that MUST live here are the two things inline styles cannot
// express: pseudo-classes (:focus-visible) and @keyframes. Theme-dependent
// colours reach this sheet through the `--am-*` custom properties that
// applyTheme() writes onto the root element.

export const globalCss = `
  * { box-sizing: border-box; margin: 0; padding: 0; }
  /* The body itself was never given a family: the Shell sets one on its root
     div, so everything inside inherited correctly and nobody noticed that
     document.body still computed to the browser default — Times New Roman.
     Anything rendered outside that root (a portal, an error boundary's
     fallback) landed in serif. --am-sans is published by applyTheme, same
     mechanism as --am-focus. */
  body { font-family: var(--am-sans, 'IBM Plex Sans', system-ui, sans-serif); }
  button { cursor: pointer; font-family: inherit; }
  input, select, textarea { font-family: inherit; }
  /* WebKit draws the number-input spin buttons flush against the value, which
     collides with the right-aligned values on Settings. The buttons are a
     pseudo-element, so inline styles cannot reach them — the gap has to come
     from here. Firefox already draws its spinner clear of the text. */
  input[type="number"]::-webkit-inner-spin-button { margin-left: 6px; }
  ::-webkit-scrollbar { width: 6px; height: 6px; }
  ::-webkit-scrollbar-track { background: transparent; }
  ::-webkit-scrollbar-thumb { background: rgba(128,128,128,0.25); border-radius: 3px; }
  ::-webkit-scrollbar-thumb:hover { background: rgba(128,128,128,0.4); }

  /* Keyboard focus (VU-2). Every interactive element in the app — Btn, TabBar
     tabs, Pager buttons, sidebar NavItems, clickable HoverTr rows, links —
     picks this up from one rule, so it cannot drift the way a per-component
     React state would. :focus-visible (not :focus) means a mouse click does not
     leave a ring behind.

     Input/Select are the deliberate exception: they set outline:none inline
     (inputStyle()) and paint focusRing() from React state, and an inline style
     outranks this sheet — so fields keep their existing border+glow treatment
     and never show two rings at once.

     --am-focus is written by applyTheme(); the literal is the dark-mode
     primary, used only if a component renders outside ThemeProvider. */
  :focus-visible {
    outline: 2px solid var(--am-focus, #5298c2);
    outline-offset: 2px;
  }
  /* Table rows sit inside an overflow-clipped container, so an outset ring is
     cropped on the leading/trailing edge. Inset it. */
  tr:focus-visible {
    outline-offset: -2px;
  }

  /* Motion is opt-in (VU-15). Leaving the keyframes undefined under
     reduce is what actually stops the animation: the animation shorthand
     stays on the element, finds no matching name, and renders the static
     state — the running-Badge dot at full opacity (it keeps its colour and
     glow, so "running" is still distinguishable) and the Skeleton at its
     gradient's rest position. */
  @media (prefers-reduced-motion: no-preference) {
    @keyframes pulse { 0%,100% { opacity: 1; } 50% { opacity: 0.5; } }
    @keyframes shimmer { 0% { background-position: 200% 0; } 100% { background-position: -200% 0; } }
  }

  /* H-2 — the OTHER half of "motion is opt-in", and the reason it has to live
     here. The keyframes rule above covers the two animations; it does nothing
     for the app's transitions, which are inline styles on components — a
     chevron rotating 180deg, a settings toggle's knob sliding, a Dashboard tile
     lifting on hover. Those are transform transitions, the kind vestibular
     disorders react to, and until now they ran under reduce regardless.

     A stylesheet cannot normally beat an inline style; !important can, and
     that is the only mechanism available short of threading a matchMedia read
     through every component that transitions anything. One rule that no future
     component can forget to opt into beats thirty that each have to remember.

     Near-zero rather than none: a 0.01ms transition still FIRES its
     transitionend event, so anything sequencing on one keeps working — it just
     arrives at the end state immediately.

     scroll-behavior is here for CSS-driven scrolling only. It does NOT reach
     a scrollIntoView called with an explicit smooth behaviour, where the
     argument wins over the property — those call sites check
     prefersReducedMotion() themselves (utils/motion.ts). */
  @media (prefers-reduced-motion: reduce) {
    *, *::before, *::after {
      animation-duration: 0.01ms !important;
      animation-iteration-count: 1 !important;
      transition-duration: 0.01ms !important;
      scroll-behavior: auto !important;
    }
  }
`;

// Reduced-motion, for the cases CSS cannot reach (H-2).
//
// The blanket rule in global-css.ts handles every CSS transition and animation
// in the app, inline styles included. It cannot handle a scroll requested from
// JavaScript: `el.scrollIntoView({ behavior: "smooth" })` passes the behaviour
// as an ARGUMENT, and an explicit argument beats the `scroll-behavior` property
// the stylesheet sets — so a smooth-scroll call keeps animating under `reduce`
// no matter what the sheet says. The only fix is to ask before calling.
//
// Defaults to "no preference" when matchMedia is unavailable (jsdom without a
// stub, an old browser): the app has always animated there, and treating an
// absent API as a request to reduce would be inventing an intent nobody
// expressed.
export function prefersReducedMotion(): boolean {
  return typeof window !== "undefined" && typeof window.matchMedia === "function"
    ? window.matchMedia("(prefers-reduced-motion: reduce)").matches
    : false;
}

/**
 * scrollIntoView with the behaviour the viewer asked for. Same signature as the
 * DOM method minus `behavior`, which this decides: smooth normally, instant
 * under `reduce`. The element still ends up in view either way — reduced motion
 * removes the animation, never the navigation.
 */
export function scrollIntoViewRespectingMotion(el: Element, options: Omit<ScrollIntoViewOptions, "behavior"> = {}): void {
  el.scrollIntoView({ ...options, behavior: prefersReducedMotion() ? "auto" : "smooth" });
}

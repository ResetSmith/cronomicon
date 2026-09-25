import { c } from "../theme";
import logoEmblem from "../assets/logo-emblem.png";

// The Cronomicon brand lockup: emblem bitmap + wordmark as real DOM text (LG-Q2).
//
// The wordmark used to be baked into the logo bitmap, which forced one asset per
// theme (white lettering for the dark rail, navy for the light one) and made the
// lettering unthemeable, unselectable and invisible to a screen reader. The two
// assets drifted — different canvas padding and aspect, so identical CSS drew the
// light mark at under half the dark one's size, and they disagreed on the tagline.
//
// Rendering the text here fixes all of that by construction: one asset, one size
// in both themes, one tagline string, and a collapsed rail that simply hides the
// text instead of cropping the bitmap through a hand-tuned window.
//
// The emblem itself is theme-independent on purpose — a colour badge with dark
// outlines inside a gold ring, verified legible on both the #0c1420 dark rail and
// the #ffffff light one — so there is deliberately no per-theme `src` here.
export const WORDMARK_TAGLINE = "ANCIENT RITES OF SCHEDULING, MADE EASY";

// `tone` picks the text tokens for the surface the lockup sits on: the sidebar
// rail (its own token pair — in dark mode `sidebarTextActive` is the brand gold,
// which is exactly right for the wordmark) or an ordinary page/card background
// (Login). Getting this wrong is how you end up with rail-tinted text on a card.
export function Wordmark({
  collapsed,
  emblemWidth = 84,
  tone = "sidebar",
}: {
  collapsed?: boolean;
  emblemWidth?: number;
  tone?: "sidebar" | "page";
}) {
  const nameColor = tone === "sidebar" ? c.sidebarTextActive : c.text;
  const taglineColor = tone === "sidebar" ? c.sidebarText : c.textSec;
  return (
    <div style={{ display: "flex", flexDirection: "column", alignItems: "center", gap: collapsed ? 0 : 7 }}>
      <img
        src={logoEmblem}
        alt="Cronomicon"
        style={{ display: "block", width: collapsed ? 38 : emblemWidth, height: "auto", margin: "0 auto" }}
      />
      {!collapsed && (
        // aria-hidden: the emblem's alt text already names the product, so
        // announcing "Cronomicon" twice would be noise.
        <div style={{ textAlign: "center", lineHeight: 1.15 }} aria-hidden>
          <div
            style={{
              fontSize: c.fontHead,
              fontFamily: c.sansCond,
              fontWeight: 700,
              letterSpacing: 2.4,
              color: nameColor,
            }}
          >
            CRONOMICON
          </div>
          <div
            style={{
              fontSize: c.fontXs,
              fontFamily: c.sansCond,
              fontWeight: 600,
              letterSpacing: 0.9,
              color: taglineColor,
              marginTop: 2,
            }}
          >
            {WORDMARK_TAGLINE}
          </div>
        </div>
      )}
    </div>
  );
}

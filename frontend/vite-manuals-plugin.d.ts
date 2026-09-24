import type { Plugin } from "vite";

// Types for the plain-JS publishManuals plugin (see vite-manuals-plugin.js).
export function publishManuals(): Plugin;

// Exported for unit testing: wraps a body-only runner-guide fragment in the
// manuals' shell, returning a full standalone HTML document.
export function wrapGuide(
  fragment: string,
  current: { file: string; label: string; eyebrow?: string },
  style: string,
  brand?: { logo?: string; lead?: string },
): string;

// The registered guide list. Exported so tests can derive expectations from the
// real registry instead of restating basenames — see runner-guide-wrap.test.ts.
export const GUIDES: ReadonlyArray<{ file: string; label: string; eyebrow?: string }>;

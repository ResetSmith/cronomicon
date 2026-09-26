import type { Plugin } from "vite";

// Types for the plain-JS publishManuals plugin (see vite-manuals-plugin.js).
export function publishManuals(): Plugin;

// Exported for unit testing: wraps a body-only runner-guide fragment in the
// manuals' shell, returning a full standalone HTML document.
export function wrapGuide(fragment: string, current: Guide, style: string, brand?: { logo?: string }): string;

// The registered guide list. Exported so tests can derive expectations from the
// real registry instead of restating basenames — see runner-guide-wrap.test.ts.
export const GUIDES: ReadonlyArray<Guide>;

// One registry entry. `kind` names the document in the hero chip (default
// "Runner Guide"); `course` files it under Courses and leaves its <h2>s
// unwrapped for the training deck.
export interface Guide {
  file: string;
  label: string;
  kind?: string;
  course?: boolean;
}

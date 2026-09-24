// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";

import { ShadowBadge } from "./ui";

// RA-10 (the runas-update plan Phase D) — the Env Vars shadow warning.
//
// The badge exists because the list cannot tell two rows apart: a global row and a
// scoped row of the same key both render as ordinary rows, and only the scoped one
// is ever injected for runs in its scope. The distinction the badge must PRESERVE
// is between a shadow that belongs to a department (the override feature working)
// and one that belongs to none (the silent hole — it overrides for everyone in
// that scope). Flattening those two makes the badge noise in exactly the catalogue
// where it matters most, so it is asserted rather than left to a glance.

afterEach(cleanup);

describe("ShadowBadge", () => {
  it("renders nothing for a row that shadows nothing", () => {
    const { container } = render(<ShadowBadge shadow={null} />);
    expect(container.textContent).toBe("");
  });

  it("warns loudly when the shadowing row belongs to no department", () => {
    const reason = "the secret in scope \"prod\" belongs to no department, so it shadows the global secret";
    const { getByTitle } = render(<ShadowBadge shadow={{ unrestricted: true, reason }} />);
    const el = getByTitle(reason);
    // The warning glyph is what distinguishes this from the benign case at a glance.
    expect(el.textContent).toContain("⚠");
    expect(el.textContent).toContain("shadows global");
    // The reason carries the whole explanation, so it must reach the operator.
    expect(el.getAttribute("title")).toBe(reason);
    expect(el.getAttribute("aria-label")).toMatch(/every department/i);
  });

  it("states the benign shadow quietly, without the warning glyph", () => {
    const reason = 'the secret in scope "prod" ([DSS]) shadows the global secret for runs in that scope';
    const { getByTitle } = render(<ShadowBadge shadow={{ unrestricted: false, reason }} />);
    const el = getByTitle(reason);
    expect(el.textContent).toBe("shadows global");
    expect(el.textContent).not.toContain("⚠");
  });
});

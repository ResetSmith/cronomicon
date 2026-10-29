// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";

import { setGlobalAgencyName } from "../../globalName";
import { ShadowBadge } from "./ui";

// RA-10 (the runas-update plan Phase D) — the Env Vars shadow warning.
//
// The badge exists because the list cannot tell two rows apart: an agency-wide row
// and a scoped row of the same key both render as ordinary rows, and only the
// scoped one is injected for runs in its scope. The distinction the badge must
// PRESERVE is between a shadow that belongs to a department (the override feature
// working) and a row of the built-in agency, which belongs to none and, since
// 2.5.3, no run uses (2.5.4: it says "unused", not that it shadows anything).
// Flattening those two makes the badge noise in exactly the catalogue where it
// matters most, so it is asserted rather than left to a glance.

afterEach(() => {
  cleanup();
  setGlobalAgencyName(null);
});

describe("ShadowBadge", () => {
  it("renders nothing for a row that shadows nothing", () => {
    const { container } = render(<ShadowBadge shadow={null} />);
    expect(container.textContent).toBe("");
  });

  it("warns that a row of the built-in agency is unused, naming the agency as it is called now", () => {
    setGlobalAgencyName("Central");
    const reason =
      "the secret in scope \"prod\" is the built-in administration agency's (it belongs to no department), which runs nothing since 2.5.3, so no run uses it in place of the agency-wide secret";
    const { getByTitle } = render(<ShadowBadge shadow={{ unrestricted: true, reason }} />);
    const el = getByTitle(reason);
    // The warning glyph is what distinguishes this from the benign case at a glance.
    expect(el.textContent).toBe("⚠ unused: Central's");
    expect(el.textContent).not.toMatch(/shadows/);
    // The reason carries the whole explanation, so it must reach the operator.
    expect(el.getAttribute("title")).toBe(reason);
    expect(el.getAttribute("aria-label")).toBe("Central · administration holds this row, and no run uses it");
    expect(el.getAttribute("aria-label")).not.toMatch(/every department/i);
  });

  it("states the benign shadow quietly, without the warning glyph", () => {
    const reason = 'the secret in scope "prod" ([DSS]) shadows the agency-wide secret for runs in that scope';
    const { getByTitle } = render(<ShadowBadge shadow={{ unrestricted: false, reason }} />);
    const el = getByTitle(reason);
    expect(el.textContent).toBe("shadows agency-wide");
    expect(el.textContent).not.toContain("⚠");
  });
});

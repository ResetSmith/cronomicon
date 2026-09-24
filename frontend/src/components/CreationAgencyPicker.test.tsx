// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";

// RF-14 — the client half of the RF-Q2(a) creation rule
// (the RBAC-fixes plan).
//
// The server 422s a restricted creator who names no agency, because an unmembered
// entity is unrestricted-only for writes (RB-Q14) and would lock its own author
// out. This control is the compliance path — the thing that keeps that rule from
// being discovered as a rejection, which is the shape RB-29 exists to prevent.
//
// The two properties worth fencing are both about WHO SEES IT: an unrestricted
// operator must not (omitting membership is how they deliberately mint shared
// infrastructure), and a capabilities FAILURE must show it rather than hide it.

let CAPS: Record<string, boolean> = {};
let capsThrows = false;

// The fail-closed default the real client returns on ANY error (its CAPS_OFF is
// module-private). Everything false — which is what makes the failure case below
// SHOW the picker rather than hide it.
const CAPS_OFF = {
  vault: false, apprise: false, compose: false, manageRoles: false,
  configureApp: false, manageEnvVars: false, publishSchedule: false,
  triggerJobs: false, killJobs: false, unrestricted: false,
};

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/agencies") return { data: [{ id: "ag-tax", name: "Tax" }, { id: "ag-fin", name: "Finance" }] };
        return { data: [] };
      }),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => {
      // The real fetchCapabilities catches its own errors and returns CAPS_OFF
      // (all false, fail-closed). Modelling that explicitly is the point of the
      // failure case below.
      if (capsThrows) return { ...CAPS_OFF };
      return { ...CAPS_OFF, ...CAPS };
    }),
  };
});

import { CreationAgencyPicker, useCreationAgencies } from "./CreationAgencyPicker";

// A minimal host that exercises the hook exactly as the three dialogs do.
function Host({ isEdit }: { isEdit: boolean }) {
  const pick = useCreationAgencies(isEdit);
  return (
    <div>
      <button disabled={!!pick.blockedReason}>{pick.blockedReason || "Save"}</button>
      <CreationAgencyPicker
        label="secret"
        required={pick.required}
        agencies={pick.agencies}
        selected={pick.selected}
        setSelected={pick.setSelected}
      />
    </div>
  );
}

beforeEach(() => {
  CAPS = {};
  capsThrows = false;
});
afterEach(cleanup);

describe("RF-Q2(a) creation agency picker", () => {
  it("asks a restricted creator to choose, and blocks Save until they do", async () => {
    CAPS = { unrestricted: false };
    render(<Host isEdit={false} />);
    await waitFor(() => expect(screen.getByText(/Agencies/)).toBeTruthy());
    // The reason is ON the button, not hidden in a tooltip: a disabled Save with
    // no explanation is the worst shape for a rule the operator has never met.
    const btn = screen.getByRole("button") as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(btn.textContent).toMatch(/Choose at least one agency/i);
    expect(screen.getByText(/department-scoped/i)).toBeTruthy();
  });

  it("stays out of an unrestricted creator's way", async () => {
    CAPS = { unrestricted: true };
    render(<Host isEdit={false} />);
    // Give the effects a chance to run before asserting an ABSENCE.
    await waitFor(() => expect((screen.getByRole("button") as HTMLButtonElement).disabled).toBe(false));
    // Creating something with no department is how shared infrastructure is
    // deliberately made, and only they may do it.
    expect(screen.queryByText(/Agencies/)).toBeNull();
  });

  it("never appears on an edit", async () => {
    CAPS = { unrestricted: false };
    render(<Host isEdit={true} />);
    await waitFor(() => expect((screen.getByRole("button") as HTMLButtonElement).disabled).toBe(false));
    // Membership is edited on the Agencies tab (RB-22); re-asking here would imply
    // this form could change it, which it cannot.
    expect(screen.queryByText(/Agencies/)).toBeNull();
  });

  it("fails CLOSED when capabilities cannot be read", async () => {
    capsThrows = true;
    render(<Host isEdit={false} />);
    // CAPS_OFF reports unrestricted=false, so the picker APPEARS and the operator
    // can comply. Hiding it on error would send them into a 422 with no control
    // to fix it — failing open on the UI is failing closed on the workflow.
    await waitFor(() => expect(screen.getByText(/Agencies/)).toBeTruthy());
    expect((screen.getByRole("button") as HTMLButtonElement).disabled).toBe(true);
  });

  it("offers the agencies it fetched", async () => {
    CAPS = { unrestricted: false };
    render(<Host isEdit={false} />);
    await waitFor(() => expect(screen.getByRole("listbox")).toBeTruthy());
    const opts = screen.getAllByRole("option").map((o) => o.textContent);
    expect(opts).toContain("Tax");
    expect(opts).toContain("Finance");
  });
});

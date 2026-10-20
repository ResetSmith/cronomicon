// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// 2.5.0 C11, defect 84 (question 105): the installation freeze has a switch in
// Settings → General and a banner above every page. Until C11 only the API set
// it, and nothing in the app said why every agency's schedules were quiet.

let FREEZE: Record<string, unknown> = { on: false, reason: "" };
const puts: { path: string; body: unknown }[] = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => (path === "/settings/freeze" ? { data: FREEZE } : { data: undefined })),
      PUT: vi.fn(async (path: string, opts: { body: Record<string, unknown> }) => {
        puts.push({ path, body: opts.body });
        FREEZE = { ...opts.body, setBy: "root@example.com", setAt: "2026-10-19T12:00:00Z" };
        return { data: FREEZE };
      }),
    } as unknown as typeof actual.api,
  };
});

import { InstallationFreezeBanner } from "../../components/InstallationFreezeBanner";
import { InstallationFreezeCard } from "./InstallationFreeze";

beforeEach(() => {
  FREEZE = { on: false, reason: "" };
  puts.length = 0;
});
afterEach(cleanup);

describe("the installation freeze (C11)", () => {
  it("shows nothing while the installation is not frozen", async () => {
    render(<InstallationFreezeBanner />);
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByTestId("installation-freeze-banner")).toBeNull();
  });

  it("names the freeze, its reason and who set it, above every page", async () => {
    FREEZE = { on: true, reason: "Year-end change freeze", setBy: "root@example.com", setAt: "2026-10-19T12:00:00Z" };
    render(<InstallationFreezeBanner />);
    const banner = await screen.findByTestId("installation-freeze-banner");
    expect(banner.textContent).toContain("no run starts in any agency");
    expect(banner.textContent).toContain("Year-end change freeze");
    expect(banner.textContent).toContain("root@example.com");
  });

  // 2.5.0 C12, defect 93: the freeze binds a manual run and a token's too (the
  // gate matrix's globalFreeze, FX-Q2); the copy said they went on.
  it("does not say a manual run or a token's run goes on", async () => {
    FREEZE = { on: true, reason: "Year-end", setBy: "root@example.com", setAt: "2026-10-19T12:00:00Z" };
    render(<InstallationFreezeBanner />);
    const banner = await screen.findByTestId("installation-freeze-banner");
    expect(banner.textContent).not.toMatch(/unaffected|go on/i);
    expect(banner.textContent).toContain("Run, tokens");
  });

  it("freezes with a reason, and the banner follows at once", async () => {
    render(
      <>
        <InstallationFreezeBanner />
        <InstallationFreezeCard canWrite />
      </>,
    );
    const freeze = await screen.findByRole("button", { name: "Freeze every agency's schedules" });
    expect((freeze as HTMLButtonElement).disabled).toBe(true); // no reason yet
    fireEvent.change(screen.getByLabelText("Reason for the freeze"), { target: { value: "Data-centre move" } });
    fireEvent.click(freeze);
    await waitFor(() => expect(puts).toEqual([{ path: "/settings/freeze", body: { on: true, reason: "Data-centre move" } }]));
    await waitFor(() => expect(screen.getByTestId("installation-freeze-banner").textContent).toContain("Data-centre move"));
    fireEvent.click(await screen.findByRole("button", { name: "Lift the freeze" }));
    await waitFor(() => expect(puts[1]).toEqual({ path: "/settings/freeze", body: { on: false, reason: "" } }));
    await waitFor(() => expect(screen.queryByTestId("installation-freeze-banner")).toBeNull());
  });

  it("is read, and not changed, by anyone but a global administrator", async () => {
    FREEZE = { on: true, reason: "Audit week" };
    render(<InstallationFreezeCard canWrite={false} />);
    const lift = (await screen.findByRole("button", { name: "Lift the freeze" })) as HTMLButtonElement;
    expect(lift.disabled).toBe(true);
    expect(lift.title).toMatch(/global administrator/);
  });
});

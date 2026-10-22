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
import { setAppZone } from "../../utils/datetime";

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
    // 2.5.1: a loud confirmation, with an acknowledgement, before anything is sent.
    expect(await screen.findByText(/No run will start in any agency/)).toBeTruthy();
    const confirm = screen.getByRole("button", { name: "Freeze the installation" }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    expect(puts).toEqual([]);
    fireEvent.click(screen.getByRole("checkbox"));
    fireEvent.click(confirm);
    await waitFor(() => expect(puts).toEqual([{ path: "/settings/freeze", body: { on: true, reason: "Data-centre move" } }]));
    await waitFor(() => expect(screen.getByTestId("installation-freeze-banner").textContent).toContain("Data-centre move"));
    fireEvent.click(await screen.findByRole("button", { name: "Lift the freeze" }));
    await waitFor(() => expect(puts[1]).toEqual({ path: "/settings/freeze", body: { on: false, reason: "" } }));
    await waitFor(() => expect(screen.queryByTestId("installation-freeze-banner")).toBeNull());
  });

  it("cancelling the confirmation sends nothing", async () => {
    render(<InstallationFreezeCard canWrite />);
    fireEvent.change(await screen.findByLabelText("Reason for the freeze"), { target: { value: "Audit week" } });
    fireEvent.click(screen.getByRole("button", { name: "Freeze every agency's schedules" }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));
    await new Promise((r) => setTimeout(r, 20));
    expect(puts).toEqual([]);
    expect(screen.queryByText(/No run will start in any agency/)).toBeNull();
  });

  // 2.5.1 (the owner): the card is a global administrator's alone; anyone else
  // sees only the banner.
  it("is not shown to anyone but a global administrator", async () => {
    FREEZE = { on: true, reason: "Audit week" };
    render(<InstallationFreezeCard canWrite={false} />);
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByText("Installation freeze")).toBeNull();
    expect(screen.queryByRole("button", { name: "Lift the freeze" })).toBeNull();
  });
});

// 2.5.2 FZ-1 to FZ-6 — a freeze with a window.
describe("a scheduled installation freeze (2.5.2 FZ)", () => {
  beforeEach(() => setAppZone("America/Los_Angeles"));
  afterEach(() => setAppZone(undefined));

  it("the banner announces a scheduled freeze, is red while it is in effect, and goes when it ends (FZ-4)", async () => {
    FREEZE = { on: true, reason: "Year-end", from: "2026-12-25T02:00:00Z", until: "2026-12-27T14:00:00Z", state: "scheduled" };
    const { unmount } = render(<InstallationFreezeBanner />);
    let banner = await screen.findByTestId("installation-freeze-banner");
    expect(banner.getAttribute("data-state")).toBe("scheduled");
    expect(banner.textContent).toContain("Installation freeze from");
    expect(banner.textContent).toContain("America/Los_Angeles");
    expect(banner.textContent).toContain("Year-end");
    unmount();
    FREEZE = { ...FREEZE, state: "in_effect" };
    const second = render(<InstallationFreezeBanner />);
    banner = await screen.findByTestId("installation-freeze-banner");
    expect(banner.getAttribute("data-state")).toBe("in_effect");
    expect(banner.textContent).toContain("no run starts in any agency until");
    second.unmount();
    FREEZE = { ...FREEZE, state: "ended" };
    render(<InstallationFreezeBanner />);
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByTestId("installation-freeze-banner")).toBeNull();
  });

  it("schedules a window typed in the app's timezone, naming it in the confirmation (FZ-1, FZ-5)", async () => {
    render(<InstallationFreezeCard canWrite />);
    fireEvent.change(await screen.findByLabelText("Reason for the freeze"), { target: { value: "Year-end" } });
    fireEvent.click(screen.getByRole("radio", { name: "Schedule…" }));
    const button = screen.getByRole("button", { name: "Schedule the freeze" }) as HTMLButtonElement;
    expect(button.disabled).toBe(true); // no start yet
    fireEvent.change(screen.getByLabelText(/Begins \(America\/Los_Angeles\)/), { target: { value: "2026-12-24T18:00" } });
    fireEvent.change(screen.getByLabelText(/Ends \(optional: until lifted\) \(America\/Los_Angeles\)/), { target: { value: "2026-12-27T06:00" } });
    fireEvent.click(button);
    const words = await screen.findByTestId("freeze-window-words");
    expect(words.textContent).toMatch(/^from .*24.* until .*27.*, America\/Los_Angeles$/);
    fireEvent.click(screen.getByRole("checkbox"));
    fireEvent.click(screen.getAllByRole("button", { name: "Schedule the freeze" }).at(-1) as HTMLElement);
    await waitFor(() =>
      expect(puts).toEqual([
        { path: "/settings/freeze", body: { on: true, reason: "Year-end", from: "2026-12-25T02:00:00Z", until: "2026-12-27T14:00:00Z" } },
      ]),
    );
  });

  it("cancels a scheduled freeze with one button (FZ-5)", async () => {
    FREEZE = { on: true, reason: "Year-end", from: "2026-12-25T02:00:00Z", state: "scheduled", setBy: "root@example.com" };
    render(<InstallationFreezeCard canWrite />);
    fireEvent.click(await screen.findByRole("button", { name: "Cancel the scheduled freeze" }));
    await waitFor(() => expect(puts).toEqual([{ path: "/settings/freeze", body: { on: false, reason: "" } }]));
  });
});

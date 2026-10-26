// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// The Local runner card (LR-1, LR-17, LR-43, LR-44; MA-14).
//
// Turning it on makes this server hold SSH keys and run jobs, so both
// directions are confirmed, each saying what changes; the host can forbid it;
// and since 2.5.3 (CA-8 to CA-10) it belongs to one agency, chosen here, or to
// none and is off.

let LR: Record<string, unknown> = {};
const puts: { path: string; body: unknown }[] = [];
let putError: unknown = null;

const GLOBAL = { id: "global", name: "Global" };
const FIN = { id: "ag-fin", name: "Finance" };
const TAX = { id: "ag-tax", name: "Tax" };

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/local-runner") return { data: LR };
        if (path === "/agencies") return { data: [GLOBAL, FIN, TAX] };
        return { data: [] };
      }),
      PUT: vi.fn(async (path: string, init?: { body?: Record<string, unknown> }) => {
        puts.push({ path, body: init?.body });
        if (putError) return { error: putError };
        const body = init?.body ?? {};
        if ("ownerAgency" in body) {
          // The server: a change of agency turns it off (CA-10).
          const owner = [FIN, TAX].find((a) => a.id === body.ownerAgency) ?? GLOBAL;
          LR = { ...LR, owner, serves: [owner], enabled: false };
        }
        const { ownerAgency: _ignored, ...rest } = body;
        LR = { ...LR, ...rest };
        return { data: LR };
      }),
    } as unknown as typeof actual.api,
  };
});

import { LocalRunnerSection } from "./LocalRunner";

beforeEach(() => {
  LR = {
    runnerId: "r-local",
    enabled: false,
    forbidden: false,
    status: "offline",
    maxConcurrent: 4,
    capabilities: ["bash", "perl", "powershell", "python"],
    serves: [FIN],
    owner: FIN,
  };
  puts.length = 0;
  putError = null;
});
afterEach(cleanup);

const show = (canWrite = true) =>
  render(
    <MemoryRouter>
      <LocalRunnerSection canWrite={canWrite} />
    </MemoryRouter>,
  );
const button = (name: string | RegExp) => screen.getByRole("button", { name }) as HTMLButtonElement;
const agencySelect = () => screen.findByRole("combobox", { name: "Agency" }) as Promise<HTMLSelectElement>;

describe("Local runner card", () => {
  it("says what turning it on means, and turns it on only after a confirmation", async () => {
    show();
    expect(await screen.findByText("Off")).toBeTruthy();
    expect(screen.getByText(/this server holds SSH private keys and opens\s+connections to job targets/)).toBeTruthy();

    fireEvent.click(button("Turn on…"));
    expect(puts).toHaveLength(0);
    expect(await screen.findByText(/This server will hold SSH private keys in memory/)).toBeTruthy();
    expect(screen.getByText(/queued shell runs of Finance that no other runner has taken/)).toBeTruthy();
    fireEvent.click(button("Turn on"));
    await waitFor(() => expect(puts).toEqual([{ path: "/local-runner", body: { enabled: true } }]));
    expect(await screen.findByText("On")).toBeTruthy();
  });

  it("confirms turning it off, and says that nothing is failed", async () => {
    LR = { ...LR, enabled: true, status: "online" };
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Turn off…" }));
    expect(await screen.findByText(/Runs it is running now will finish\./)).toBeTruthy();
    expect(screen.getByText(/a scope bound only to it stays closed\. Nothing is failed\./)).toBeTruthy();
    expect(puts).toHaveLength(0);
    fireEvent.click(button("Turn off"));
    await waitFor(() => expect(puts).toEqual([{ path: "/local-runner", body: { enabled: false } }]));
  });

  it("shows a host that forbids it as forbidden, with no switch", async () => {
    LR = { ...LR, forbidden: true };
    show();
    expect(await screen.findByText("Forbidden on this host")).toBeTruthy();
    expect(screen.getByText(/CRONOMICON_LOCAL_RUNNER=forbid is set on the host/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Turn o/ })).toBeNull();
  });

  it("saves how many runs it takes at once, within bounds", async () => {
    show();
    const input = (await screen.findByLabelText("Runs at once")) as HTMLInputElement;
    expect(input.value).toBe("4");
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull(); // nothing changed yet
    fireEvent.change(input, { target: { value: "0" } });
    expect(button("Save").disabled).toBe(true);
    expect(button("Save").title).toBe("A whole number from 1 to 64");
    fireEvent.change(input, { target: { value: "8" } });
    fireEvent.click(button("Save"));
    await waitFor(() => expect(puts).toEqual([{ path: "/local-runner", body: { maxConcurrent: 8 } }]));
  });

  // 2.5.3 CA-8: its agency is chosen here, among the agencies; never Central.
  it("offers the agencies and Unassigned, never Global", async () => {
    show();
    const sel = await agencySelect();
    expect(sel.value).toBe("ag-fin");
    expect(Array.from(sel.options).map((o) => o.textContent)).toEqual(["Unassigned (off)", "Finance", "Tax"]);
  });

  // CA-9: with no agency it is off and cannot be turned on.
  it("is off with no agency, and says why it cannot be turned on", async () => {
    LR = { ...LR, owner: GLOBAL, serves: [GLOBAL] };
    show();
    expect(await screen.findByText("Off: it belongs to no agency")).toBeTruthy();
    expect((await agencySelect()).value).toBe("");
    expect(button("Turn on…").disabled).toBe(true);
    expect(button("Turn on…").title).toBe("Choose its agency first: with no agency it is off.");
  });

  // CA-10: changing its agency clears its host-key approvals and turns it off.
  it("gives it to another agency only after a confirmation that says what changes", async () => {
    LR = { ...LR, enabled: true, status: "online" };
    show();
    fireEvent.change(await agencySelect(), { target: { value: "ag-tax" } });
    expect(puts).toHaveLength(0);
    expect(await screen.findByText(/It will claim Tax's shell runs instead of Finance's\./)).toBeTruthy();
    expect(screen.getByText(/Its host-key approvals are\s+cleared/)).toBeTruthy();
    expect(screen.getByText(/it is turned off/)).toBeTruthy();
    fireEvent.click(button("Give it to Tax"));
    await waitFor(() => expect(puts).toEqual([{ path: "/local-runner", body: { ownerAgency: "ag-tax" } }]));
    expect(await screen.findByText("Off")).toBeTruthy();
    expect((await agencySelect()).value).toBe("ag-tax");
  });

  it("unassigns it, which turns it off", async () => {
    show();
    fireEvent.change(await agencySelect(), { target: { value: "" } });
    expect(await screen.findByText(/It will belong to no agency, and is turned off/)).toBeTruthy();
    fireEvent.click(button("Unassign it"));
    await waitFor(() => expect(puts).toEqual([{ path: "/local-runner", body: { ownerAgency: "" } }]));
    expect(await screen.findByText("Off: it belongs to no agency")).toBeTruthy();
  });

  it("shows why a change of agency was refused", async () => {
    putError = { code: "local_runner_busy", message: "the local runner is running a run: let it finish (or stop it) before it changes agency" };
    show();
    fireEvent.change(await agencySelect(), { target: { value: "ag-tax" } });
    fireEvent.click(await screen.findByRole("button", { name: "Give it to Tax" }));
    expect(await screen.findByText(/let it finish \(or stop it\) before it changes agency/)).toBeTruthy();
  });

  it("disables every control, with the reason, for someone who is not a global administrator", async () => {
    show(false);
    const why = "Only a global administrator (a role on every agency) can change this.";
    expect((await screen.findByRole("button", { name: "Turn on…" }) as HTMLButtonElement).title).toBe(why);
    expect(button("Turn on…").disabled).toBe(true);
    expect((screen.getByLabelText("Runs at once") as HTMLInputElement).disabled).toBe(true);
    expect((await agencySelect()).disabled).toBe(true);
  });

  // 500 apply_failed means the setting WAS saved; the card must show that.
  it("reads the state back after a refusal, because a refusal can still have saved", async () => {
    putError = { code: "apply_failed", message: "the setting was saved, but the running server could not apply it" };
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Turn on…" }));
    LR = { ...LR, enabled: true };
    fireEvent.click(await screen.findByRole("button", { name: "Turn on" }));
    expect(await screen.findByText(/the setting was saved, but the running server could not apply it/)).toBeTruthy();
    expect(await screen.findByText("On")).toBeTruthy();
  });

  it("shows the server's refusal", async () => {
    putError = { code: "local_runner_forbidden", message: "the local runner is forbidden on this host" };
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Turn on…" }));
    fireEvent.click(await screen.findByRole("button", { name: "Turn on" }));
    expect(await screen.findByText(/the local runner is forbidden on this host/)).toBeTruthy();
  });
});

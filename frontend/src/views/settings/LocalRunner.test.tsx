// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// The Local runner card (LR-1, LR-17, LR-43, LR-44; MA-14).
//
// Turning it on makes this server hold SSH keys and run jobs, so both
// directions are confirmed, each saying what changes; the host can forbid it;
// and it is the one runner whose serve list is edited — any non-empty set.

let LR: Record<string, unknown> = {};
const puts: { path: string; body: unknown }[] = [];
let putError: unknown = null;
let getDelay = 0;
let serveOnSave = false;

const GLOBAL = { id: "global", name: "Global" };
const FIN = { id: "ag-fin", name: "Finance" };
const TAX = { id: "ag-tax", name: "Tax" };

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/local-runner") {
          const snapshot = LR;
          if (getDelay) await new Promise((r) => setTimeout(r, getDelay));
          return { data: snapshot };
        }
        if (path === "/agencies") return { data: [GLOBAL, FIN, TAX] };
        return { data: [] };
      }),
      PUT: vi.fn(async (path: string, init?: { body?: unknown }) => {
        puts.push({ path, body: init?.body });
        if (putError) return { error: putError };
        if (path === "/local-runner") LR = { ...LR, ...(init?.body as object) };
        if (path === "/runner-agencies" && serveOnSave) {
          const ids = (init?.body as { agencyIds: string[] }[])[0].agencyIds;
          LR = { ...LR, serves: [GLOBAL, FIN, TAX].filter((a) => ids.includes(a.id)) };
        }
        return { data: LR };
      }),
    } as unknown as typeof actual.api,
  };
});

import { LocalRunnerSection } from "./LocalRunner";

beforeEach(() => {
  LR = { runnerId: "r-local", enabled: false, forbidden: false, status: "offline", maxConcurrent: 4, capabilities: ["bash", "perl", "powershell", "python"], serves: [GLOBAL] };
  puts.length = 0;
  putError = null;
  getDelay = 0;
  serveOnSave = false;
});
afterEach(cleanup);

const show = (canWrite = true) =>
  render(
    <MemoryRouter>
      <LocalRunnerSection canWrite={canWrite} />
    </MemoryRouter>,
  );
const button = (name: string | RegExp) => screen.getByRole("button", { name }) as HTMLButtonElement;

describe("Local runner card", () => {
  it("says what turning it on means, and turns it on only after a confirmation", async () => {
    show();
    expect(await screen.findByText("Off")).toBeTruthy();
    expect(screen.getByText(/this server holds SSH private keys and opens\s+connections to job targets/)).toBeTruthy();

    fireEvent.click(button("Turn on…"));
    expect(puts).toHaveLength(0);
    expect(await screen.findByText(/This server will hold SSH private keys in memory/)).toBeTruthy();
    expect(screen.getByText(/queued shell runs of Global that no other runner has taken/)).toBeTruthy();
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

  it("edits its serve list: any agency can be added, Global beside others, and the last cannot be removed", async () => {
    show();
    const add = (await screen.findByRole("combobox", { name: "Serve another agency" })) as HTMLSelectElement;
    await waitFor(() => expect(Array.from(add.options).map((o) => o.textContent)).toEqual(["+ Serve an agency…", "Finance", "Tax"]));
    // The only agency it serves cannot be taken away: a runner always serves one.
    expect(button("Stop serving Global").disabled).toBe(true);
    expect(button("Stop serving Global").title).toMatch(/always serves at least one agency/);

    fireEvent.change(add, { target: { value: "ag-fin" } });
    await waitFor(() => expect(puts).toHaveLength(1));
    // The whole list, Global kept beside the agency.
    expect(puts[0]).toEqual({ path: "/runner-agencies", body: [{ runnerId: "r-local", agencyIds: ["global", "ag-fin"] }] });
  });

  it("confirms before it stops serving an agency, and says what happens to that agency's bound scopes", async () => {
    LR = { ...LR, serves: [GLOBAL, FIN] };
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Stop serving Finance" }));
    expect(puts).toHaveLength(0);
    expect(await screen.findByText(/A scope of Finance\s+that is bound to it stays bound, and waits/)).toBeTruthy();
    const confirm = screen.getAllByRole("button", { name: "Stop serving Finance" });
    fireEvent.click(confirm[confirm.length - 1]);
    await waitFor(() => expect(puts).toEqual([{ path: "/runner-agencies", body: [{ runnerId: "r-local", agencyIds: ["global"] }] }]));
  });

  it("disables every control, with the reason, for someone who is not a global administrator", async () => {
    LR = { ...LR, serves: [GLOBAL, FIN] };
    show(false);
    const why = "Only a global administrator (a role on every agency) can change this.";
    expect((await screen.findByRole("button", { name: "Turn on…" }) as HTMLButtonElement).title).toBe(why);
    expect(button("Turn on…").disabled).toBe(true);
    expect((screen.getByLabelText("Runs at once") as HTMLInputElement).disabled).toBe(true);
    expect(button("Stop serving Finance").disabled).toBe(true);
    expect((screen.getByRole("combobox", { name: "Serve another agency" }) as HTMLSelectElement).disabled).toBe(true);
  });

  // The list is saved whole, from what is on screen. Until it has been read
  // back, a second change would be built on the list as it stood before the
  // first and would drop it.
  it("takes no second change to the serve list until the first has been read back", async () => {
    getDelay = 40;
    show();
    const add = (await screen.findByRole("combobox", { name: "Serve another agency" })) as HTMLSelectElement;
    await waitFor(() => expect(add.options.length).toBe(3));
    serveOnSave = true;
    fireEvent.change(add, { target: { value: "ag-fin" } });
    await waitFor(() => expect(puts).toHaveLength(1));
    // Written, not yet re-read: the control waits.
    await waitFor(() => expect((screen.getByRole("combobox", { name: "Serve another agency" }) as HTMLSelectElement).disabled).toBe(true));
    await screen.findByRole("button", { name: "Stop serving Finance" });
    const again = screen.getByRole("combobox", { name: "Serve another agency" }) as HTMLSelectElement;
    await waitFor(() => expect(again.disabled).toBe(false));
    fireEvent.change(again, { target: { value: "ag-tax" } });
    await waitFor(() => expect(puts).toHaveLength(2));
    expect(puts[1].body).toEqual([{ runnerId: "r-local", agencyIds: ["global", "ag-fin", "ag-tax"] }]);
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

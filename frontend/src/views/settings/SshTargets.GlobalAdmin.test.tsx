// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

// GC (v2.2.2, gate closing) — SSH Targets as an administrator of one agency.
//
// Two rules, and they differ, which is why this is worth a test of its own:
//   · a BASTION is shared by every agency's hosts, and a host registered by hand
//     belongs to no scope — so "+ Add Host" and every bastion write need a
//     global administrator. Disabled, with the reason (FX-7).
//   · an existing HOST row is writable by whoever administers the scope it was
//     imported for. Nothing on the row says which, and the view does not invent
//     an answer: its actions stay live, and a refusal shows the server's sentence.

const HOSTS = [
  { id: "h1", hostname: "web-01.internal", address: "10.0.0.1", port: 22, user: "deploy", source: "cronomicon", status: "verified" },
];
const BASTIONS = [
  { id: "b1", name: "jump-dmz", address: "10.9.0.1", port: 22, user: "jump", status: "verified", hostKeyPinned: true },
];

const writes: { method: string; path: string }[] = [];
let putHostError: unknown = null;

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  const record = (method: string) =>
    vi.fn(async (path: string) => {
      writes.push({ method, path });
      if (method === "PUT" && path === "/ssh/hosts/{hostId}" && putHostError) return { error: putHostError };
      return { data: {} };
    });
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/ssh/hosts") return { data: HOSTS };
        if (path === "/ssh/bastions") return { data: BASTIONS };
        return { data: [] };
      }),
      POST: record("POST"),
      PUT: record("PUT"),
      DELETE: record("DELETE"),
    } as unknown as typeof actual.api,
  };
});

import { SshTargetsSection } from "./SshTargets";

const WHY = "Only a global administrator (a role on every agency) can change this.";

beforeEach(() => {
  writes.length = 0;
  putHostError = null;
  localStorage.clear();
});
afterEach(cleanup);

const open = async (canWrite: boolean) => {
  render(<SshTargetsSection canWrite={canWrite} />);
  await waitFor(() => expect(screen.getByText("web-01.internal")).toBeTruthy());
  await waitFor(() => expect(screen.getByText("jump-dmz")).toBeTruthy());
};
const rowOf = (text: string) => within(screen.getByText(text).closest("tr") as HTMLElement);
const button = (q: { getByRole: typeof screen.getByRole }, name: string) => q.getByRole("button", { name }) as HTMLButtonElement;

describe("SSH Targets — an administrator of one agency (GC)", () => {
  it("disables + Add Host and + Add Bastion, with the reason", async () => {
    await open(false);
    for (const name of ["+ Add Host", "+ Add Bastion"]) {
      const b = button(screen, name);
      expect(b.disabled, name).toBe(true);
      expect(b.title, name).toBe(WHY);
    }
  });

  it("disables every bastion row action, with the reason", async () => {
    await open(false);
    const row = rowOf("jump-dmz");
    for (const name of ["Test", "Clear pin", "Edit", "Remove"]) {
      const b = button(row, name);
      expect(b.disabled, name).toBe(true);
      expect(b.title, name).toBe(WHY);
    }
    fireEvent.click(button(row, "Test"));
    expect(writes.length).toBe(0);
  });

  it("leaves an existing host's actions live, and shows the server's refusal when one is refused", async () => {
    putHostError = { code: "forbidden", message: "this host was registered by hand, so only an administrator of every agency may change it" };
    await open(false);
    const row = rowOf("web-01.internal");
    for (const name of ["Test", "Edit", "Remove"]) expect(button(row, name).disabled, name).toBe(false);

    fireEvent.click(button(row, "Edit"));
    fireEvent.click(await waitFor(() => button(rowOf("web-01.internal"), "Save")));
    await waitFor(() =>
      expect(screen.getByText(/this host was registered by hand, so only an administrator of every agency may change it/)).toBeTruthy(),
    );
  });
});

describe("SSH Targets — a global administrator (GC)", () => {
  it("has every create and bastion control enabled", async () => {
    await open(true);
    for (const name of ["+ Add Host", "+ Add Bastion"]) {
      const b = button(screen, name);
      expect(b.disabled, name).toBe(false);
      expect(b.title, name).toBe("");
    }
    const row = rowOf("jump-dmz");
    for (const name of ["Test", "Clear pin", "Edit", "Remove"]) expect(button(row, name).disabled, name).toBe(false);
  });
});

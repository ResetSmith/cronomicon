// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// Unrevealable secrets (2.5.1 SG-19): a locked secret shows "Not revealable"
// where Reveal was; an unlocked one offers "Make unrevealable…" behind a
// confirmation that says it cannot be undone.

const SECRETS = [
  { id: "s-open", key: "DB_PASS", scope: "Prod", source: "stored", tags: [], ownerAgency: "Global" },
  { id: "s-locked", key: "SEALED_PASS", scope: "Prod", source: "stored", tags: [], ownerAgency: "Global", revealLocked: true, revealLockedBy: "ana@corp" },
];
const posts: string[] = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    fetchCapabilities: vi.fn(async () => ({ vault: false, manageEnvVars: true, unrestricted: true })),
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/env-secrets") return { data: SECRETS };
        return { data: [] };
      }),
      POST: vi.fn(async (path: string) => {
        posts.push(path);
        return { data: {} };
      }),
      PUT: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
  };
});

import { SecretsTab } from "./SecretsTab";

beforeEach(() => {
  posts.length = 0;
  localStorage.clear();
});
afterEach(cleanup);

const open = async () => {
  render(
    <MemoryRouter>
      <SecretsTab scopeNames={["All", "Prod"]} canEdit />
    </MemoryRouter>,
  );
  await waitFor(() => expect(screen.getByText("DB_PASS")).toBeTruthy());
};

describe("Secrets — unrevealable", () => {
  it("shows a locked secret as not revealable, with no Reveal and no lock button", async () => {
    await open();
    fireEvent.click(screen.getByText("SEALED_PASS"));
    expect(await screen.findByText("Not revealable")).toBeTruthy();
    expect(screen.queryByText("Reveal")).toBeNull();
    expect(screen.queryByRole("button", { name: "Make unrevealable…" })).toBeNull();
  });

  it("makes a secret unrevealable only after a confirmation that says it cannot be undone", async () => {
    await open();
    fireEvent.click(screen.getByText("DB_PASS"));
    fireEvent.click(await screen.findByRole("button", { name: "Make unrevealable…" }));
    expect(screen.getByText(/cannot be undone/)).toBeTruthy();
    expect(posts).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Make unrevealable" }));
    await waitFor(() => expect(posts).toContain("/env-secrets/{secretId}/lock-reveal"));
  });
});

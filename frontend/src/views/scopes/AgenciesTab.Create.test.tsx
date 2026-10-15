// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// An agency is created with its repository (2.4.0 I3, IN-2; R8): the form
// asks for the repository's URL, a token and its branch, and sends them as
// `repository`; without them it says so and sends nothing.

const posts: Array<{ path: string; body: unknown }> = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/agencies") return { data: [{ id: "global", name: "Global", builtin: true }] };
        return { data: [] };
      }),
      POST: vi.fn(async (path: string, opts?: { body?: unknown }) => {
        posts.push({ path, body: opts?.body });
        return { data: { id: "ag-ops", name: "Ops" } };
      }),
      PUT: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
  };
});

import { AgenciesTab } from "./AgenciesTab";

beforeEach(() => {
  posts.length = 0;
});
afterEach(cleanup);

describe("Agencies → Add Agency", () => {
  it("creates an agency with its repository, and not without one", async () => {
    render(<AgenciesTab canEdit globalAdmin />);
    await waitFor(() => expect(screen.getAllByText(/Add Agency/).length).toBeGreaterThan(0));
    fireEvent.click(screen.getAllByText(/Add Agency/)[0]);
    fireEvent.change(screen.getByPlaceholderText("e.g. agency-alpha"), { target: { value: "Ops" } });

    fireEvent.click(screen.getByText("Save Agency"));
    await waitFor(() => expect(screen.getByText(/created with its repository/)).toBeTruthy());
    expect(posts).toHaveLength(0);

    fireEvent.change(screen.getByLabelText("Repository URL"), { target: { value: "https://gitlab.example.org/ops/defs.git" } });
    fireEvent.change(screen.getByLabelText("Token"), { target: { value: "glpat-ops" } });
    fireEvent.click(screen.getByText("Save Agency"));
    await waitFor(() => expect(posts).toHaveLength(1));
    const body = posts[0].body as { name: string; repository: { repoUrl: string; pat: string; writeBranch: string; webhookEnabled: boolean } };
    expect(posts[0].path).toBe("/agencies");
    expect(body.name).toBe("Ops");
    expect(body.repository).toMatchObject({ repoUrl: "https://gitlab.example.org/ops/defs.git", pat: "glpat-ops", writeBranch: "main", webhookEnabled: true });
  });
});

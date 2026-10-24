// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

// An agency connects its repository at creation or later (2.5.1, SG-40;
// before, 2.4.0 I3, IN-2, required it here): the form's repository section is
// closed by default and sends no `repository`; opened, it asks for the URL, a
// token and the branch, and sends them, and without them says so and sends
// nothing.

const posts: Array<{ path: string; body: unknown }> = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/agencies")
          return {
            data: [
              { id: "global", name: "Global", builtin: true, canEdit: true, canDelete: false, canRepair: true },
              { id: "ag-fin", name: "Finance", canEdit: true, canDelete: true, canRepair: true },
            ],
          };
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

describe("Agencies — the row's own flags (R8)", () => {
  it("offers a global administrator Edit and Delete on an agency, and only Edit on the built-in one (2.5.2 GR)", async () => {
    render(<AgenciesTab canEdit globalAdmin />);
    await waitFor(() => expect(screen.getByText("Finance")).toBeTruthy());
    const rowOf = (name: string) =>
      within(screen.getAllByText(name).map((el) => el.closest("tr")).find((tr) => tr && within(tr).queryByRole("button", { name: "Edit" })) as HTMLElement);
    for (const label of ["Edit", "Delete"]) {
      expect((rowOf("Finance").getByRole("button", { name: label }) as HTMLButtonElement).disabled, label).toBe(false);
    }
    // 2.5.3 CA-16: the built-in agency is shown apart as the administration, not
    // as an agency row; renamed there since 2.5.2 (GR), never deleted (no Delete).
    expect(screen.getAllByRole("row").some((tr) => within(tr).queryByText("Global"))).toBe(false);
    const apart = within(screen.getByTestId("central-apart"));
    expect(apart.getByText("Global · administration")).toBeTruthy();
    expect((apart.getByRole("button", { name: "Rename" }) as HTMLButtonElement).disabled).toBe(false);
    expect(apart.queryByRole("button", { name: "Delete" })).toBeNull();
  });
});

describe("Agencies → Add Agency", () => {
  it("creates an agency without a repository by default", async () => {
    render(<AgenciesTab canEdit globalAdmin />);
    await waitFor(() => expect(screen.getAllByText(/Add Agency/).length).toBeGreaterThan(0));
    fireEvent.click(screen.getAllByText(/Add Agency/)[0]);
    fireEvent.change(screen.getByPlaceholderText("e.g. agency-alpha"), { target: { value: "Ops" } });
    expect(screen.queryByLabelText("Repository URL")).toBeNull();
    fireEvent.click(screen.getByText("Save Agency"));
    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0].body).toEqual({ name: "Ops", description: null });
  });

  it("creates an agency with its repository when the section is opened, and not without its URL and token", async () => {
    render(<AgenciesTab canEdit globalAdmin />);
    await waitFor(() => expect(screen.getAllByText(/Add Agency/).length).toBeGreaterThan(0));
    fireEvent.click(screen.getAllByText(/Add Agency/)[0]);
    fireEvent.change(screen.getByPlaceholderText("e.g. agency-alpha"), { target: { value: "Ops" } });
    fireEvent.click(screen.getByText("Connect its repository now (optional)"));

    fireEvent.click(screen.getByText("Save Agency"));
    await waitFor(() => expect(screen.getByText(/or close the repository section/)).toBeTruthy());
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

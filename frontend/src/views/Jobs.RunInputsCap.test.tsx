// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// JP-2 — the Run inputs list caps at ~5 rows and scrolls the rest. Worth pinning
// in both directions: a long declaration list must get the scrollport (the
// section stops growing unboundedly), and a short one must NOT (no scrollbar
// sliver on small lists). The cap is a conditional inline maxHeight, so the
// assertion reads the style attribute rather than layout (jsdom has no layout).

const prompt = (i: number) => ({ name: `INPUT_${i}`, label: `Input ${i}` });

// Two jobs: one with 7 declared inputs, one with 3.
const JOBS = {
  items: [
    { id: 1, name: "many-inputs", type: "bash", scope: "Prod", source: "cronomicon", status: "success" },
    { id: 2, name: "few-inputs", type: "bash", scope: "Prod", source: "cronomicon", status: "success" },
  ],
  totalItems: 2,
  totalPages: 1,
  page: 1,
  pageSize: 50,
};

const DETAILS: Record<number, object> = {
  1: { ...JOBS.items[0], prompts: Array.from({ length: 7 }, (_, i) => prompt(i)) },
  2: { ...JOBS.items[1], prompts: Array.from({ length: 3 }, (_, i) => prompt(i)) },
};

const laterTick = () => new Promise((resolve) => setTimeout(resolve, 0));

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string, opts?: { params?: { path?: { jobId?: number } } }) => {
        if (path === "/jobs") {
          await laterTick();
          return { data: JOBS };
        }
        if (path === "/jobs/{jobId}") return { data: DETAILS[opts?.params?.path?.jobId ?? 0] };
        if (path === "/runs") return { data: { items: [] } };
        if (path === "/job-reference-bindings/{jobId}") return { data: { bindings: [] } };
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      PUT: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      vault: false,
      apprise: false,
      compose: false,
      // AF-2 — an unrestricted composer, so the All scope option is offered.
      composeUnbound: true,
      manageRoles: false,
      configureApp: false,
      manageEnvVars: false,
      publishSchedule: false,
    })),
  };
});

import { Jobs } from "./Jobs";

afterEach(cleanup);

const renderJobs = async () => {
  const { container } = render(
    <MemoryRouter>
      <Jobs />
    </MemoryRouter>,
  );
  const q = within(container);
  fireEvent.change(q.getByPlaceholderText("Search jobs…"), { target: { value: "inputs" } });
  await waitFor(() => expect(q.getByText("many-inputs")).toBeTruthy());
  return { container, q };
};

const expandRow = (q: ReturnType<typeof within>, name: string) => {
  fireEvent.click(q.getByText(name).closest("tr")!);
};

// The scrollport is the only element in the detail that carries the JP-2 cap.
const cappedDiv = (container: HTMLElement) =>
  Array.from(container.querySelectorAll("div")).find((d) => d.style.maxHeight === "165px");

describe("Jobs — Run inputs scroll cap (JP-2)", () => {
  it("caps the list and scrolls when more than 5 inputs are declared", async () => {
    const { container, q } = await renderJobs();
    expandRow(q, "many-inputs");
    await waitFor(() => expect(q.getByText(/Run inputs \(7/)).toBeTruthy());
    const port = cappedDiv(container);
    expect(port).toBeTruthy();
    expect(port!.style.overflowY).toBe("auto");
    // All 7 rows render — the cap scrolls, it never truncates.
    expect(q.getByText("Input 6")).toBeTruthy();
  });

  it("applies no cap when 5 or fewer inputs are declared", async () => {
    const { container, q } = await renderJobs();
    expandRow(q, "few-inputs");
    await waitFor(() => expect(q.getByText(/Run inputs \(3/)).toBeTruthy());
    expect(cappedDiv(container)).toBeUndefined();
  });
});

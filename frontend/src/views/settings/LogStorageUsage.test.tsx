// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, waitFor, within } from "@testing-library/react";

// The Log Storage usage line (LU-11).
//
// This line answers one operator question — "what is filling my disk?" — and it
// used to answer it wrongly: a single "N log files · X" that counted run output,
// the process log, its rotated generations and the per-folder sidecars as one
// undifferentiated number. A deployment with 5 GB of run history and one runaway
// process log looked identical to the reverse. So the behaviours pinned here are
// that run logs are reported on their own, that the other classes appear only
// when they exist (a permanent "Audit log 0 B" reads as a fault, not as
// information), and that the total shown is the whole tree rather than a sum the
// reader has to do themselves.

const BASE = {
  backend: "local",
  local: { path: "/var/lib/amadeus/logs" },
  stats: {
    totalSizeBytes: 0,
    fileCount: 0,
    oldestLogAt: null as string | null,
    classes: {
      runLogs: { fileCount: 0, totalSizeBytes: 0 },
      processLog: { fileCount: 0, totalSizeBytes: 0 },
      auditLog: { fileCount: 0, totalSizeBytes: 0 },
      other: { fileCount: 0, totalSizeBytes: 0 },
    },
  },
};

let getData: unknown = BASE;

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async () => ({ data: getData })),
      PUT: vi.fn(async () => ({ data: getData })),
    } as unknown as typeof actual.api,
  };
});

import { LogStorageSection } from "./Integrations";

beforeEach(() => {
  getData = BASE;
});
afterEach(cleanup);

describe("Log Storage usage line", () => {
  it("reports run logs on their own, not the whole tree", async () => {
    // 2 MiB of run output next to a 10 MiB process log. Counting them together
    // is what made the old number useless. The three sizes are kept distinct so
    // an assertion cannot pass by matching the wrong one.
    getData = {
      ...BASE,
      stats: {
        totalSizeBytes: 12 * 1024 * 1024,
        fileCount: 2,
        oldestLogAt: "2026-01-15T00:00:00Z",
        classes: {
          runLogs: { fileCount: 2, totalSizeBytes: 2 * 1024 * 1024 },
          processLog: { fileCount: 1, totalSizeBytes: 10 * 1024 * 1024 },
          auditLog: { fileCount: 0, totalSizeBytes: 0 },
          other: { fileCount: 0, totalSizeBytes: 0 },
        },
      },
    };
    const { container } = render(<LogStorageSection />);
    const q = within(container);
    await waitFor(() => expect(q.getByText(/2 run logs/)).toBeTruthy());
    // The run-log size, not the total — the whole point of the split.
    expect(q.getByText(/2 run logs · 2.0 MB/)).toBeTruthy();
    expect(q.getByText(/Process log 10.0 MB/)).toBeTruthy();
    // …and the whole tree is still stated, so nobody has to add it up.
    expect(q.getByText(/12.0 MB/)).toBeTruthy();
  });

  it("hides classes that have no files rather than showing a permanent zero", async () => {
    // Nothing writes an audit log yet. A standing "Audit log 0 B" would read as a
    // broken subsystem instead of an absent one.
    getData = {
      ...BASE,
      stats: {
        ...BASE.stats,
        totalSizeBytes: 512,
        fileCount: 1,
        classes: { ...BASE.stats.classes, runLogs: { fileCount: 1, totalSizeBytes: 512 } },
      },
    };
    const { container } = render(<LogStorageSection />);
    const q = within(container);
    await waitFor(() => expect(q.getByText(/1 run logs/)).toBeTruthy());
    expect(q.queryByText(/Audit log/)).toBeNull();
    expect(q.queryByText(/Process log/)).toBeNull();
    expect(q.queryByText(/Other/)).toBeNull();
  });

  it("counts the folder sidecars as Other, never as run logs", async () => {
    // _meta.json files sit inside the run-log tree and are not logs. Folding them
    // into the run count would inflate "roughly how many runs are on disk" by one
    // per job forever.
    getData = {
      ...BASE,
      stats: {
        totalSizeBytes: 900,
        fileCount: 1,
        oldestLogAt: null,
        classes: {
          runLogs: { fileCount: 1, totalSizeBytes: 800 },
          processLog: { fileCount: 0, totalSizeBytes: 0 },
          auditLog: { fileCount: 0, totalSizeBytes: 0 },
          other: { fileCount: 3, totalSizeBytes: 100 },
        },
      },
    };
    const { container } = render(<LogStorageSection />);
    const q = within(container);
    await waitFor(() => expect(q.getByText(/1 run logs/)).toBeTruthy());
    expect(q.getByText(/Other 100 B/)).toBeTruthy();
  });

  it("omits the oldest-log date when there are no run logs to date", async () => {
    // oldestLogAt is null when the tree holds only a process log. Rendering a
    // stray "oldest —" there would imply a run history that does not exist.
    getData = {
      ...BASE,
      stats: {
        totalSizeBytes: 4096,
        fileCount: 0,
        oldestLogAt: null,
        classes: { ...BASE.stats.classes, processLog: { fileCount: 1, totalSizeBytes: 4096 } },
      },
    };
    const { container } = render(<LogStorageSection />);
    const q = within(container);
    await waitFor(() => expect(q.getByText(/0 run logs/)).toBeTruthy());
    expect(q.queryByText(/oldest/)).toBeNull();
  });
});

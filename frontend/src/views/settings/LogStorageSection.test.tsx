// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";

// The Log Storage card's archive-tier controls (SL-5, the s3-logging plan).
//
// Pinned: the daily time input appears only in Daily mode and is labelled UTC;
// Sync now is disabled WITH a reason on a local backend and while a sync runs
// (FX-7: a precondition disables with an explanation, only irrelevance hides);
// a save with the S3 backend sends the timetable and useSsl; the archived tier
// is listed apart from the disk classes and only when it holds objects.

const STATS = {
  totalSizeBytes: 0,
  fileCount: 0,
  oldestLogAt: null as string | null,
  classes: {
    runLogs: { fileCount: 0, totalSizeBytes: 0 },
    processLog: { fileCount: 0, totalSizeBytes: 0 },
    auditLog: { fileCount: 0, totalSizeBytes: 0 },
    other: { fileCount: 0, totalSizeBytes: 0 },
  },
};

const LOCAL = {
  backend: "local",
  local: { path: "/var/lib/cronomicon/logs" },
  sync: { mode: "interval", intervalSeconds: 900 },
  stats: STATS,
  lastModifiedAt: "2026-09-09T12:00:00Z",
};

const S3 = {
  backend: "s3",
  local: { path: "/var/lib/cronomicon/logs" },
  s3: { endpoint: "minio:9000", bucket: "logs", region: "us-east-1", accessKey: "AK", prefix: "cronomicon/", useSsl: false },
  sync: { mode: "interval", intervalSeconds: 300 },
  archive: {
    count: 12,
    bytes: 3 * 1024 * 1024,
    pending: 2,
    inProgress: false,
    lastSyncStartedAt: "2026-09-09T11:59:00Z",
    lastSyncFinishedAt: "2026-09-09T12:00:00Z",
    lastSyncError: null as string | null,
  },
  stats: STATS,
  lastModifiedAt: "2026-09-09T12:00:00Z",
};

let getData: unknown = LOCAL;
const puts: unknown[] = [];
const posts: string[] = [];
let postResult: { error?: unknown; response?: { status: number } } = { response: { status: 202 } };

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async () => ({ data: getData })),
      PUT: vi.fn(async (_p: string, opts: { body: unknown }) => {
        puts.push(opts.body);
        return { data: getData };
      }),
      POST: vi.fn(async (p: string) => {
        posts.push(p);
        return postResult;
      }),
    } as unknown as typeof actual.api,
  };
});

import { LogStorageSection } from "./Integrations";

beforeEach(() => {
  getData = LOCAL;
  puts.length = 0;
  posts.length = 0;
  postResult = { response: { status: 202 } };
});
afterEach(cleanup);

describe("Log Storage archive controls", () => {
  it("names the S3 backend as local + archive and hides its fields on local", async () => {
    const { container } = render(<LogStorageSection canWrite />);
    const q = within(container);
    await waitFor(() => expect(q.getByText("Local + S3 archive")).toBeTruthy());
    expect(q.queryByLabelText("Sync schedule")).toBeNull();
    expect(q.queryByText("Bucket")).toBeNull();
  });

  it("disables Sync now with the reason on a local backend", async () => {
    const { container } = render(<LogStorageSection canWrite />);
    const q = within(container);
    await waitFor(() => expect(q.getByText("Local volume")).toBeTruthy());
    // The status line (and its button) is irrelevant on local with nothing
    // archived — FX-7's one case for hiding.
    expect(q.queryByText("Sync now")).toBeNull();

    // Switch the dropdown without saving: the button appears, disabled, and
    // says the server has no store yet.
    fireEvent.change(q.getByDisplayValue("Local volume"), { target: { value: "s3" } });
    const btn = (await q.findByText("Sync now")).closest("button") as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(btn.title).toMatch(/save/i);
  });

  it("shows the daily time input, labelled UTC, only in Daily mode", async () => {
    getData = S3;
    const { container } = render(<LogStorageSection canWrite />);
    const q = within(container);
    const sel = (await q.findByLabelText("Sync schedule")) as HTMLSelectElement;
    expect(sel.value).toBe("300");
    expect(q.queryByLabelText("Daily sync time (UTC)")).toBeNull();

    fireEvent.change(sel, { target: { value: "daily" } });
    const at = q.getByLabelText("Daily sync time (UTC)") as HTMLInputElement;
    expect(at.value).toBe("02:00");
    expect(q.getByText("UTC")).toBeTruthy();

    fireEvent.change(sel, { target: { value: "3600" } });
    expect(q.queryByLabelText("Daily sync time (UTC)")).toBeNull();
  });

  it("saves the timetable and useSsl with the S3 backend", async () => {
    getData = S3;
    const { container } = render(<LogStorageSection canWrite />);
    const q = within(container);
    const sel = (await q.findByLabelText("Sync schedule")) as HTMLSelectElement;
    fireEvent.change(sel, { target: { value: "daily" } });
    fireEvent.change(q.getByLabelText("Daily sync time (UTC)"), { target: { value: "03:30" } });
    fireEvent.click(q.getByText("Save Changes"));
    await waitFor(() => expect(puts).toHaveLength(1));
    const body = puts[0] as { backend: string; sync: { mode: string; at: string }; s3: { useSsl: boolean } };
    expect(body.backend).toBe("s3");
    expect(body.sync.mode).toBe("daily");
    expect(body.sync.at).toBe("03:30");
    expect(body.s3.useSsl).toBe(false);
  });

  it("offers the CA bundle only with SSL on and saves it", async () => {
    getData = { ...S3, s3: { ...S3.s3, useSsl: true } };
    const { container } = render(<LogStorageSection canWrite />);
    const q = within(container);
    const ta = (await q.findByLabelText("CA bundle (PEM)")) as HTMLTextAreaElement;
    fireEvent.change(ta, { target: { value: "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----" } });
    fireEvent.click(q.getByText("Save Changes"));
    await waitFor(() => expect(puts).toHaveLength(1));
    expect((puts[0] as { s3: { caPem: string } }).s3.caPem).toMatch(/BEGIN CERTIFICATE/);
  });

  it("hides the CA bundle with SSL off — it would be ignored", async () => {
    getData = S3; // useSsl: false
    const { container } = render(<LogStorageSection canWrite />);
    const q = within(container);
    await q.findByLabelText("Sync schedule");
    expect(q.queryByLabelText("CA bundle (PEM)")).toBeNull();
  });

  it("reports the archive status and runs Sync now against the endpoint", async () => {
    getData = S3;
    const { container } = render(<LogStorageSection canWrite />);
    const q = within(container);
    await waitFor(() => expect(q.getByText(/2 pending/)).toBeTruthy());
    expect(q.getByText(/Last sync/)).toBeTruthy();
    // Archived tier, apart from the disk classes.
    expect(q.getByText(/Archived \(S3\) 12 run logs · 3.0 MB/)).toBeTruthy();

    const btn = q.getByText("Sync now").closest("button") as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
    fireEvent.click(btn);
    await waitFor(() => expect(posts).toEqual(["/settings/log-storage/sync"]));
    await waitFor(() => expect(q.getByText(/Sync started/)).toBeTruthy());
  });

  it("disables Sync now while a sync runs and explains a 409", async () => {
    getData = { ...S3, archive: { ...S3.archive, inProgress: true } };
    const { container } = render(<LogStorageSection canWrite />);
    const q = within(container);
    const btn = (await q.findByText("Sync now")).closest("button") as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(btn.title).toBe("A sync is running");
    expect(q.getByText(/Sync running/)).toBeTruthy();
  });

  it("surfaces the last sync error", async () => {
    getData = { ...S3, archive: { ...S3.archive, lastSyncError: "probe s3://logs: Access Denied." } };
    const { container } = render(<LogStorageSection canWrite />);
    const q = within(container);
    await waitFor(() => expect(q.getByText(/Last error: probe s3:\/\/logs: Access Denied\./)).toBeTruthy());
  });

  it("omits the archived tier line when nothing is archived", async () => {
    getData = { ...S3, archive: { ...S3.archive, count: 0, bytes: 0 } };
    const { container } = render(<LogStorageSection canWrite />);
    const q = within(container);
    await waitFor(() => expect(q.getByText("Sync now")).toBeTruthy());
    expect(q.queryByText(/Archived \(S3\)/)).toBeNull();
  });
});

// GC (v2.2.2, gate closing) — PUT /settings/log-storage and POST
// /settings/log-storage/sync need a global administrator. The read is open to
// every session, so an administrator of one agency sees the backend, the archive
// status and the usage; Save and Sync now are disabled with the reason. The
// permission outranks the other Sync-now reasons because it is the one the
// caller cannot clear by saving or waiting.
describe("Log Storage — global-administrator gate (GC)", () => {
  const WHY = "Only a global administrator (a role on every agency) can change this.";

  it("disables Save and Sync now with the reason for a non-global administrator, and still shows the status", async () => {
    getData = S3;
    const { container } = render(<LogStorageSection canWrite={false} />);
    const q = within(container);
    await waitFor(() => expect(q.getByText(/2 pending/)).toBeTruthy());

    const save = q.getByRole("button", { name: "Save Changes" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    expect(save.title).toBe(WHY);
    const sync = q.getByRole("button", { name: "Sync now" }) as HTMLButtonElement;
    expect(sync.disabled).toBe(true);
    expect(sync.title).toBe(WHY);
    // The form is inert, not absent.
    expect((q.getByLabelText("Sync schedule") as HTMLSelectElement).closest("fieldset")!.disabled).toBe(true);

    fireEvent.click(save);
    fireEvent.click(sync);
    expect(puts.length).toBe(0);
    expect(posts.length).toBe(0);
  });

  it("leaves both enabled for a global administrator", async () => {
    getData = S3;
    const { container } = render(<LogStorageSection canWrite />);
    const q = within(container);
    await waitFor(() => expect(q.getByText(/2 pending/)).toBeTruthy());
    const save = q.getByRole("button", { name: "Save Changes" }) as HTMLButtonElement;
    expect(save.disabled).toBe(false);
    expect(save.title).toBe("");
    const sync = q.getByRole("button", { name: "Sync now" }) as HTMLButtonElement;
    expect(sync.disabled).toBe(false);
    expect(sync.title).toBe("Run one archive sync now");
    expect((q.getByLabelText("Sync schedule") as HTMLSelectElement).closest("fieldset")!.disabled).toBe(false);
  });
});

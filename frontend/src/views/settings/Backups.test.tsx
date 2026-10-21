// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// Settings → Backups (2.5.1 SG1.4). Encryption is optional and says nothing
// when off (SG-4); turning it on is the loud step (SG-8): a danger banner and
// three statements to tick before the button works; one key keeps a warning on
// the card; removing a key says which local backups only it opens.

const KEY1 = "age1zvkyg2lqzraa2lnjvqej32nkuu0ues2s82hzrye869xeexvn73equnujwj";
const KEY2 = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p";

let S: Record<string, unknown> = {};
const puts: { path: string; body: Record<string, unknown> }[] = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async () => ({ data: S })),
      PUT: vi.fn(async (path: string, init?: { body?: Record<string, unknown> }) => {
        const body = init?.body ?? {};
        puts.push({ path, body });
        const recips = (body.recipients as string[]).map((k, i) => ({ key: k, fingerprint: `fp${i + 1}` }));
        S = { ...S, recipients: recips, encrypted: recips.length > 0 };
        return { data: S };
      }),
    } as unknown as typeof actual.api,
  };
});

import { BackupsSection } from "./Backups";

const base = {
  recipients: [],
  encrypted: false,
  lastBackup: null,
  localBackups: [],
  localDirectory: "/var/lib/cronomicon/backups",
  localKeepDays: 7,
  bucket: "",
  schedule: "02:00",
};

beforeEach(() => {
  S = { ...base };
  puts.length = 0;
});
afterEach(cleanup);

const button = (name: string | RegExp) => screen.getByRole("button", { name }) as HTMLButtonElement;

describe("Backups card", () => {
  it("says backups are not encrypted, with no warning, when no key is set", async () => {
    render(<BackupsSection canWrite />);
    await screen.findByText(/not encrypted/);
    expect(screen.getByText(/Encryption is optional/)).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText(/If its private key is lost/)).toBeNull();
  });

  it("turns encryption on only after all three statements are ticked, and sends the acknowledgement", async () => {
    render(<BackupsSection canWrite />);
    fireEvent.click(await screen.findByRole("button", { name: "Encrypt backups" }));
    fireEvent.change(screen.getByLabelText("Public key to add"), { target: { value: KEY1 } });
    fireEvent.click(button("Add key"));

    expect(await screen.findByText(/every encrypted backup is unreadable/)).toBeTruthy();
    const encrypts = screen.getAllByRole("button", { name: "Encrypt backups" });
    const confirm = encrypts[encrypts.length - 1] as HTMLButtonElement;
    const boxes = screen.getAllByRole("checkbox") as HTMLInputElement[];
    expect(boxes).toHaveLength(3);
    expect(confirm.disabled).toBe(true);
    fireEvent.click(boxes[0]);
    fireEvent.click(boxes[1]);
    expect(confirm.disabled).toBe(true);
    fireEvent.click(boxes[2]);
    expect(confirm.disabled).toBe(false);
    fireEvent.click(confirm);

    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0].body).toEqual({ recipients: [KEY1], acknowledgedKeyCustody: true });
  });

  it("keeps a warning on the card while only one key is set, and not with two", async () => {
    S = { ...base, encrypted: true, recipients: [{ key: KEY1, fingerprint: "fp1" }] };
    const { unmount } = render(<BackupsSection canWrite />);
    expect(await screen.findByText(/One key is set/)).toBeTruthy();
    unmount();
    S = { ...base, encrypted: true, recipients: [{ key: KEY1, fingerprint: "fp1" }, { key: KEY2, fingerprint: "fp2" }] };
    render(<BackupsSection canWrite />);
    await screen.findByText("fp2");
    expect(screen.queryByText(/One key is set/)).toBeNull();
  });

  it("adds a second key with a plain confirmation showing the whole key", async () => {
    S = { ...base, encrypted: true, recipients: [{ key: KEY1, fingerprint: "fp1" }] };
    render(<BackupsSection canWrite />);
    fireEvent.change(await screen.findByLabelText("Public key to add"), { target: { value: KEY2 } });
    fireEvent.click(button("Add key"));
    expect(screen.getByText("Add a backup key")).toBeTruthy();
    expect(screen.getAllByText(KEY2).length).toBeGreaterThan(0);
    expect(screen.queryAllByRole("checkbox")).toHaveLength(0);
    const adds = screen.getAllByRole("button", { name: "Add key" });
    fireEvent.click(adds[adds.length - 1]);
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0].body).toEqual({ recipients: [KEY1, KEY2] });
  });

  it("removing the last key confirms that later backups are unencrypted and names the copies only it opens", async () => {
    S = {
      ...base,
      encrypted: true,
      recipients: [{ key: KEY1, fingerprint: "fp1" }],
      localBackups: [
        { file: "cronomicon-20261021.db.age", recipients: ["fp1"] },
        { file: "cronomicon-20261020.db", recipients: [] },
      ],
    };
    render(<BackupsSection canWrite />);
    fireEvent.click(await screen.findByRole("button", { name: "Remove" }));
    expect(screen.getByText(/backups are written unencrypted/)).toBeTruthy();
    expect(screen.getByText(/cronomicon-20261021\.db\.age/)).toBeTruthy();
    fireEvent.click(button("Remove the last key"));
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0].body).toEqual({ recipients: [], confirmUnencrypted: true });
  });

  it("is read only for anyone who is not a global administrator", async () => {
    render(<BackupsSection canWrite={false} />);
    expect(((await screen.findByRole("button", { name: "Encrypt backups" })) as HTMLButtonElement).disabled).toBe(true);
  });
});

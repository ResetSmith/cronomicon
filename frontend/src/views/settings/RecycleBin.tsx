import { useCallback, useEffect, useState } from "react";
import { api, csrfHeader } from "../../api/client";
import { c } from "../../theme";
import { Btn, Card, errMsg, fmtDateTime, tdStyle, thStyle } from "./ui";

// Recycle Bin (RH-F, the prod-features plan §4).
//
// Lives in Settings rather than on each catalog because it is cross-kind and
// because the knob that governs it — the purge window — lives next door in
// Audit & Compliance. The catalogs show what exists; this shows what used to.

type RecycleBinEntry = {
  kind: "job" | "workflow" | "schedule";
  name: string;
  deletedAt: string;
  deletedBy: string;
  purgeAt?: string | null;
};

const KIND_LABEL: Record<string, string> = { job: "Job", workflow: "Workflow", schedule: "Schedule" };

// daysUntil renders the purge countdown. The number is the point: "in the bin"
// and "gone on Thursday" are different states for an operator deciding whether
// to act now.
function daysUntil(iso?: string | null): string {
  if (!iso) return "never";
  const ms = new Date(iso).getTime() - Date.now();
  if (Number.isNaN(ms)) return "—";
  if (ms <= 0) return "any moment";
  const days = Math.ceil(ms / 86_400_000);
  return days === 1 ? "tomorrow" : `in ${days} days`;
}

export function RecycleBinSection() {
  const [items, setItems] = useState<RecycleBinEntry[] | null>(null);
  const [retentionDays, setRetentionDays] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const load = useCallback(async () => {
    const { data, error: e } = await api.GET("/recycle-bin");
    if (e) {
      setError(errMsg(e));
      setItems([]);
      return;
    }
    const d = data as { items?: RecycleBinEntry[]; retentionDays?: number } | undefined;
    setItems((d?.items ?? []) as RecycleBinEntry[]);
    setRetentionDays(d?.retentionDays ?? null);
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  async function restore(it: RecycleBinEntry) {
    setBusy(it.kind + it.name);
    setError(null);
    const { error: e } = await api.POST("/recycle-bin/{kind}/{name}/restore", {
      params: { path: { kind: it.kind, name: it.name }, header: csrfHeader },
    });
    setBusy(null);
    if (e) {
      setError(errMsg(e));
      return;
    }
    await load();
  }

  async function purge(it: RecycleBinEntry) {
    if (
      !confirm(
        `Permanently delete the ${KIND_LABEL[it.kind].toLowerCase()} "${it.name}"?\n\n` +
          `This cannot be undone. Its schedules and pauses go with it, and the name becomes available again.`,
      )
    ) {
      return;
    }
    setBusy(it.kind + it.name);
    setError(null);
    const { error: e } = await api.DELETE("/recycle-bin/{kind}/{name}", {
      params: { path: { kind: it.kind, name: it.name }, header: csrfHeader },
    });
    setBusy(null);
    if (e) {
      setError(errMsg(e));
      return;
    }
    await load();
  }

  return (
    <Card title="Recycle Bin">
      <div style={{ padding: "4px 0 12px", fontSize: c.fontSm, color: c.textSec, lineHeight: 1.5 }}>
        Deleting an in-app Job, Workflow or Schedule moves it here instead of destroying it. A binned
        definition leaves its catalog, stops firing and refuses runs and edits — but keeps its schedules,
        pauses, tags and history, so restoring it is a single step that loses nothing.
        <br />
        It also keeps its <strong>name</strong>, so a replacement cannot be created until it is restored or
        purged.
        {retentionDays != null && retentionDays > 0 && (
          <>
            {" "}
            Items are purged automatically after <strong>{retentionDays} days</strong> (Audit &amp; Compliance
            → Recycle Bin).
          </>
        )}
        {retentionDays === 0 && <> Automatic purging is off, so items stay until removed by hand.</>}
      </div>

      {error && <div style={{ color: c.danger, fontSize: c.fontSm, paddingBottom: 10 }}>{error}</div>}

      {items == null ? (
        <div style={{ color: c.textSec, fontSize: c.fontSm, padding: "8px 0" }}>Loading…</div>
      ) : items.length === 0 ? (
        <div style={{ color: c.textSec, fontSize: c.fontSm, padding: "8px 0" }}>
          Nothing has been deleted. Git-source definitions never appear here — their history is Git's.
        </div>
      ) : (
        <div style={{ overflowX: "auto" }}>
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr>
                {["Type", "Name", "Deleted", "By", "Purges", ""].map((h) => (
                  <th key={h} style={thStyle()}>
                    {h}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {items.map((it) => {
                const working = busy === it.kind + it.name;
                return (
                  <tr key={it.kind + "/" + it.name}>
                    <td style={tdStyle()}>{KIND_LABEL[it.kind] ?? it.kind}</td>
                    <td style={{ ...tdStyle(), fontWeight: 600 }}>{it.name}</td>
                    <td style={tdStyle()}>{fmtDateTime(it.deletedAt)}</td>
                    <td style={tdStyle()}>{it.deletedBy || "—"}</td>
                    <td style={tdStyle()}>
                      <span title={it.purgeAt ?? undefined}>{daysUntil(it.purgeAt)}</span>
                    </td>
                    <td style={{ ...tdStyle(), textAlign: "right", whiteSpace: "nowrap" }}>
                      <Btn onClick={() => restore(it)} disabled={working}>
                        Restore
                      </Btn>{" "}
                      <Btn dangerQuiet onClick={() => purge(it)} disabled={working}>
                        Delete forever
                      </Btn>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}

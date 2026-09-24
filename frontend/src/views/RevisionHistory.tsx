import { useEffect, useState } from "react";
import { api, csrfHeader } from "../api/client";
import { c } from "../theme";
import { Btn, Modal } from "../components/ui";
import { fmtInAppZone } from "../utils/datetime";

// Revision history for one amadeus-source definition (RH-F).
//
// The diff is line-oriented over pretty-printed JSON rather than a structural
// object diff. That is a deliberate v1 choice: a JSON diff renderer is a project
// in itself, and the question an operator actually asks here — "what did this
// look like before, and what changed?" — is answered by two adjacent snapshots
// and a +/- gutter. The renderer is the one PublishBuilder already uses for
// GitLab publish conflicts, lifted so both read the same way.

type Revision = {
  revisionNo: number;
  action: "created" | "updated" | "deleted" | "restored";
  actor: string;
  createdAt: string;
  digest: string;
  snapshot: unknown;
};

const ACTION_TONE: Record<string, string> = {
  created: c.success,
  updated: c.info,
  deleted: c.danger,
  restored: c.warning,
};

function pretty(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}

// lineDiff is the smallest thing that answers the question: mark lines present
// in one side and not the other. It does not attempt to align moved blocks —
// with definitions this small, a false "changed" reads as noise rather than as
// a wrong answer.
function lineDiff(before: string, after: string): { text: string; kind: "add" | "del" | "same" }[] {
  const a = before.split("\n");
  const b = after.split("\n");
  const inB = new Set(b);
  const inA = new Set(a);
  const out: { text: string; kind: "add" | "del" | "same" }[] = [];
  for (const line of a) {
    if (!inB.has(line)) out.push({ text: line, kind: "del" });
  }
  for (const line of b) {
    out.push({ text: line, kind: inA.has(line) ? "same" : "add" });
  }
  return out;
}

export function RevisionHistory({
  kind,
  name,
  onClose,
  onRestored,
}: {
  kind: "job" | "workflow" | "schedule";
  name: string;
  onClose: () => void;
  onRestored?: () => void;
}) {
  const [items, setItems] = useState<Revision[] | null>(null);
  const [selected, setSelected] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function load() {
    const { data, error: e } = await api.GET("/definitions/{kind}/{name}/revisions", {
      params: { path: { kind, name } },
    });
    if (e) {
      setError(typeof e === "string" ? e : "could not load history");
      setItems([]);
      return;
    }
    const list = ((data as { items?: Revision[] } | undefined)?.items ?? []) as Revision[];
    setItems(list);
    if (list.length > 0 && selected == null) setSelected(list[0].revisionNo);
  }

  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [kind, name]);

  async function restore(no: number) {
    if (!confirm(`Roll ${name} back to revision ${no}?\n\nThis is itself recorded as a new revision.`)) return;
    setBusy(true);
    setError(null);
    const { error: e } = await api.POST("/definitions/{kind}/{name}/revisions/{no}/restore", {
      params: { path: { kind, name, no }, header: csrfHeader },
    });
    setBusy(false);
    if (e) {
      setError(typeof e === "string" ? e : "restore failed");
      return;
    }
    await load();
    onRestored?.();
  }

  const current = items?.find((r) => r.revisionNo === selected) ?? null;
  const previous = current ? (items ?? []).find((r) => r.revisionNo === current.revisionNo - 1) ?? null : null;
  const diff = current ? lineDiff(previous ? pretty(previous.snapshot) : "", pretty(current.snapshot)) : [];

  return (
    <Modal title={`History — ${name}`} onClose={onClose} wide>
      {error && <div style={{ color: c.danger, fontSize: c.fontSm, paddingBottom: 8 }}>{error}</div>}
      {items == null ? (
        <div style={{ color: c.textSec, fontSize: c.fontSm }}>Loading…</div>
      ) : items.length === 0 ? (
        <div style={{ color: c.textSec, fontSize: c.fontSm }}>
          No history yet. Revisions are recorded from the next save onward.
        </div>
      ) : (
        <div style={{ display: "grid", gridTemplateColumns: "230px 1fr", gap: 14, alignItems: "start" }}>
          <div style={{ maxHeight: 420, overflowY: "auto" }}>
            {items.map((r) => {
              const active = r.revisionNo === selected;
              return (
                <div
                  key={r.revisionNo}
                  onClick={() => setSelected(r.revisionNo)}
                  style={{
                    padding: "8px 10px",
                    borderRadius: c.radiusChip,
                    cursor: "pointer",
                    marginBottom: 2,
                    background: active ? `${c.primary}1a` : "transparent",
                    border: `1px solid ${active ? `${c.primary}55` : "transparent"}`,
                  }}
                >
                  <div style={{ display: "flex", gap: 6, alignItems: "baseline" }}>
                    <span style={{ fontSize: c.fontSm, fontWeight: 700 }}>#{r.revisionNo}</span>
                    <span
                      style={{
                        fontSize: c.fontXs,
                        fontWeight: 700,
                        textTransform: "uppercase",
                        letterSpacing: 0.6,
                        color: ACTION_TONE[r.action] ?? c.textSec,
                      }}
                    >
                      {r.action}
                    </span>
                  </div>
                  <div style={{ fontSize: c.fontXs, color: c.textSec }}>{r.actor}</div>
                  <div style={{ fontSize: c.fontXs, color: c.textMuted }}>
                    {fmtInAppZone(r.createdAt)}
                  </div>
                </div>
              );
            })}
          </div>

          <div>
            <div
              style={{
                display: "flex",
                justifyContent: "space-between",
                alignItems: "center",
                paddingBottom: 8,
                gap: 10,
              }}
            >
              <span style={{ fontSize: c.fontXs, color: c.textSec }}>
                {previous
                  ? `Changes from #${previous.revisionNo} to #${current?.revisionNo}`
                  : "The first recorded revision"}
              </span>
              {current && current.action !== "deleted" && current.revisionNo !== items[0].revisionNo && (
                <Btn onClick={() => restore(current.revisionNo)} disabled={busy}>
                  Restore this revision
                </Btn>
              )}
            </div>
            <pre
              style={{
                margin: 0,
                maxHeight: 380,
                overflow: "auto",
                fontSize: c.fontXs,
                lineHeight: 1.5,
                background: c.panel2,
                border: `1px solid ${c.border}`,
                borderRadius: c.radiusSurface,
                padding: "8px 10px",
              }}
            >
              {diff.map((l, i) => (
                <div
                  key={i}
                  style={{
                    color: l.kind === "add" ? c.success : l.kind === "del" ? c.danger : c.textSec,
                    background: l.kind === "same" ? "transparent" : `${l.kind === "add" ? c.success : c.danger}12`,
                    whiteSpace: "pre-wrap",
                  }}
                >
                  {l.kind === "add" ? "+ " : l.kind === "del" ? "- " : "  "}
                  {l.text}
                </div>
              ))}
            </pre>
          </div>
        </div>
      )}
    </Modal>
  );
}

import { useState } from "react";
import { api, csrfHeader, errMsg } from "../../api/client";
import type { components } from "../../api/schema";
import { useAgencyOptional } from "../../agency";
import { useGet } from "../../hooks";
import { c } from "../../theme";
import { Btn, SectionLabel } from "../../components/ui";
import { fmtTime } from "./shared";

// The run's acknowledgement and notes (2.5.2 OP4; OP-8 to OP-19). What a shift
// knew about a run, for the next one: an append-only thread shared by the
// agency's people and the central team, and whether someone has the run in
// hand. It sits between the run's fields and its log, on a job run's detail and
// a workflow run's. The server keeps the record (OP3); this shows it, lets
// anyone who can read the run add to it, and polls so a note another person
// adds appears while the run is open. A draft survives the poll.

type RunNotes = components["schemas"]["RunNotes"];

/** How long between polls of the thread while it is open. */
const POLL_MS = 15_000;

/**
 * The author's label (OP-14), from how they stood to the run's agency when
 * they wrote: one of its people (the agency's name), a global administrator,
 * or the central team (someone whose access reaches every agency; on a run
 * that is the only other reader there is), shown as the central agency.
 */
export function relationLabel(relation: string, agencyName: string, centralName: string): string {
  switch (relation) {
    case "member":
      return agencyName;
    case "global_admin":
      return "Global administrator";
    case "outside":
      return centralName;
  }
  return "";
}

/** A failed, stopped or lost run: the runs a "Not acknowledged" marker is for. */
export const needsAck = (status?: string | null) => status === "danger" || status === "failure" || status === "killed";

/**
 * RunNotesCell — History's Notes column (OP-18, mockup board 3): a tick and the
 * note count on an acknowledged run, the count on one with notes only, "Not
 * acknowledged" on a failed, stopped or lost run nobody has taken up, and
 * nothing on the rest.
 */
export function RunNotesCell({ notes, status }: { notes?: { acknowledged: boolean; count: number } | null; status?: string | null }) {
  const count = notes?.count ?? 0;
  const noun = `${count} note${count === 1 ? "" : "s"}`;
  if (notes?.acknowledged) {
    return (
      <span data-testid="run-notes-cell" title={count ? `Acknowledged · ${noun}` : "Acknowledged"} style={{ display: "inline-flex", alignItems: "center", gap: 6, color: c.success }}>
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true">
          <path d="M3 8.5l3 3 7-7" />
        </svg>
        <span style={{ color: c.text }}>{count || ""}</span>
        <span style={{ position: "absolute", width: 1, height: 1, overflow: "hidden", clip: "rect(0 0 0 0)" }}>Acknowledged</span>
      </span>
    );
  }
  if (count) {
    return (
      <span data-testid="run-notes-cell" title={`${noun}, not acknowledged`} style={{ color: c.textSec }}>
        {noun}
      </span>
    );
  }
  if (needsAck(status)) {
    return (
      <span data-testid="run-notes-cell" style={{ display: "inline-flex", alignItems: "center", gap: 6, color: c.warning, whiteSpace: "nowrap" }}>
        <svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true">
          <circle cx="8" cy="8" r="5.5" />
        </svg>
        Not acknowledged
      </span>
    );
  }
  return null;
}

export function RunNotesPanel({
  kind,
  traceId,
  agencyId,
  offerAck,
}: {
  kind: "run" | "workflow";
  traceId: string;
  /** Whose run it is, for the authors' labels. */
  agencyId?: string | null;
  /** The run can be acknowledged here: it failed, was stopped, was lost, or is still running (it may be overdue). */
  offerAck: boolean;
}) {
  const ag = useAgencyOptional();
  const agencyName = agencyId ? (ag?.nameOf(agencyId) ?? agencyId) : "this agency";
  const central = ag?.nameOf("global") ?? "Global";
  const q = useGet<RunNotes>(
    () =>
      kind === "workflow"
        ? api.GET("/workflow-runs/{traceId}/notes", { params: { path: { traceId } } })
        : api.GET("/runs/{traceId}/notes", { params: { path: { traceId } } }),
    [kind, traceId],
    POLL_MS,
  );
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const acked = q.data?.acknowledged ?? null;
  const notes = q.data?.notes ?? [];

  async function post(entry: "note" | "acknowledged" | "unacknowledged", body: string) {
    setBusy(true);
    setErr(null);
    const req = { body: { kind: entry, body } };
    const { error } =
      kind === "workflow"
        ? await api.POST("/workflow-runs/{traceId}/notes", { params: { path: { traceId }, header: csrfHeader }, ...req })
        : await api.POST("/runs/{traceId}/notes", { params: { path: { traceId }, header: csrfHeader }, ...req });
    setBusy(false);
    if (error) {
      setErr(errMsg(error));
      return;
    }
    if (body) setDraft("");
    q.refetch();
  }

  const label = (relation: string) => relationLabel(relation, agencyName, central);
  const state = acked ? (
    <span data-testid="run-ack-state" style={{ display: "inline-flex", alignItems: "center", gap: 6, color: c.success, fontSize: c.fontSm, fontWeight: 600 }}>
      <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true">
        <path d="M3 8.5l3 3 7-7" />
      </svg>
      Acknowledged by {acked.by}
      {label(acked.relation) && <span style={{ color: c.textMuted, fontWeight: 400 }}> · {label(acked.relation)}</span>}
      <span style={{ color: c.textMuted, fontWeight: 400 }}>, {fmtTime(acked.at)}</span>
    </span>
  ) : null;

  return (
    <div data-testid="run-notes" style={{ marginBottom: 16, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, padding: "10px 12px" }}>
      <SectionLabel
        action={
          <span style={{ display: "inline-flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
            {state}
            {acked ? (
              <Btn small onClick={() => post("unacknowledged", "")} disabled={busy}>
                Remove acknowledgement
              </Btn>
            ) : (
              offerAck && (
                <Btn
                  small
                  primary
                  onClick={() => post("acknowledged", draft.trim())}
                  disabled={busy || draft.trim().length > 1000}
                  title={draft.trim() ? "Acknowledge, with the note below as its first" : "Say that you have this run in hand"}
                >
                  Acknowledge
                </Btn>
              )
            )}
          </span>
        }
      >
        Acknowledgement and notes
      </SectionLabel>

      {q.loading && !q.data ? null : notes.length === 0 ? (
        <div style={{ fontSize: c.fontSm, color: c.textMuted, padding: "4px 0 8px" }}>No notes on this run yet.</div>
      ) : (
        <ol style={{ listStyle: "none", margin: "0 0 10px", padding: 0 }}>
          {notes.map((n) => (
            <li key={n.id} data-testid="run-note" style={{ display: "flex", gap: 14, padding: "8px 0", borderBottom: `1px solid ${c.borderLight}` }}>
              <div style={{ width: 140, flexShrink: 0, display: "flex", flexDirection: "column", gap: 2 }}>
                <span style={{ fontSize: c.fontSm, fontFamily: c.mono, color: c.textSec, fontVariantNumeric: "tabular-nums" }}>{fmtTime(n.at)}</span>
                {n.kind !== "note" && (
                  <span style={{ fontSize: c.fontXs, color: n.kind === "acknowledged" ? c.success : c.textMuted }}>
                    {n.kind === "acknowledged" ? "acknowledged" : "acknowledgement removed"}
                  </span>
                )}
              </div>
              <div style={{ display: "flex", flexDirection: "column", gap: 2, minWidth: 0 }}>
                <span style={{ fontSize: c.fontSm, fontWeight: 600, color: c.text }}>
                  {n.author}
                  {label(n.relation) && <span style={{ fontWeight: 400, color: c.textMuted }}> · {label(n.relation)}</span>}
                </span>
                {n.body && <span style={{ fontSize: c.fontSm, color: c.text, whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>{n.body}</span>}
              </div>
            </li>
          ))}
        </ol>
      )}

      <label htmlFor={`run-note-${traceId}`} style={{ display: "block", fontSize: c.fontXs, color: c.textSec, marginBottom: 4 }}>
        Add a note
      </label>
      <textarea
        id={`run-note-${traceId}`}
        rows={2}
        value={draft}
        maxLength={1000}
        placeholder="What changed, who you spoke to, what the next shift should do"
        onChange={(e) => setDraft(e.target.value)}
        style={{
          width: "100%",
          boxSizing: "border-box",
          resize: "vertical",
          background: c.panelInput,
          color: c.text,
          border: `1px solid ${c.borderStrong}`,
          borderRadius: c.radiusChip,
          padding: "6px 8px",
          fontFamily: c.sans,
          fontSize: c.fontSm,
        }}
      />
      <div style={{ display: "flex", alignItems: "center", gap: 10, marginTop: 6, flexWrap: "wrap" }}>
        <span style={{ flex: 1, fontSize: c.fontXs, color: c.textMuted }}>
          Do not paste credentials. Notes cannot be edited once added, and each is recorded in {agencyName}'s change log.
        </span>
        <Btn small onClick={() => post("note", draft.trim())} disabled={busy || !draft.trim()}>
          Add note
        </Btn>
      </div>
      {(err || q.error) && <div style={{ marginTop: 6, fontSize: c.fontSm, color: c.danger }}>{err ?? q.error}</div>}
    </div>
  );
}

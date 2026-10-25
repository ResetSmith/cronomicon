// The Central calendar catalogue and the agencies' copies (2.5.3 CA-25 to CA-29).
//
// Global administrators keep a catalogue of calendars in Central; no run reads
// them (CA-25). An agency takes one as a COPY, its own row linked to the
// catalogue calendar: by "Add from Central" (its calendar editors), or by a
// global administrator's "Add to agencies" (CA-26). A copy is read-only in the
// agency; "Make it our own" detaches it into an ordinary calendar (CA-27). A
// change in the catalogue puts each copy behind: one that asks shows "Update
// available" until the agency updates it (or a global administrator pushes the
// update); one set to update automatically changes with the catalogue (CA-28).
import { useState } from "react";
import { api, csrfHeader, errMsg } from "../../api/client";
import { GLOBAL_AGENCY } from "../../api/access";
import { useGet, rows } from "../../hooks";
import { Btn, ConfirmDialog, Modal } from "../../components/ui";
import { c } from "../../theme";
import type { components } from "../../api/schema";
import type { Calendar } from "./calendars";
import { globalAgencyName } from "../../globalName";

type CatalogueCalendar = components["schemas"]["CatalogueCalendar"];
type Origin = NonNullable<Calendar["fromCentral"]>;
type UpdateMode = "ask" | "auto";

const pill = (tone: string): React.CSSProperties => ({
  fontSize: c.fontXs,
  fontWeight: 600,
  padding: "2px 8px",
  borderRadius: c.radiusPill,
  background: `${tone}18`,
  color: tone,
  border: `1px solid ${tone}40`,
  whiteSpace: "nowrap",
});

/** What a pending update changes, in a few words (CA-28). */
export function pendingSummary(o: Origin): string {
  const p = o.pending;
  if (!p) return "";
  const parts: string[] = [];
  if (p.daysAdded?.length) parts.push(`${p.daysAdded.length} day${p.daysAdded.length === 1 ? "" : "s"} added`);
  if (p.daysRemoved?.length) parts.push(`${p.daysRemoved.length} removed`);
  if (p.daysRelabelled?.length) parts.push(`${p.daysRelabelled.length} relabelled`);
  if (p.descriptionChanged) parts.push("description changed");
  if (p.global != null) parts.push(p.global ? "becomes a freeze calendar" : "no longer a freeze calendar");
  if (p.recordSuppressed != null) parts.push(p.recordSuppressed ? "records Allow misses" : "stops recording Allow misses");
  return parts.join(", ");
}

/** A copy's state (CA-29): up to date, update available, updates automatically, no longer offered. */
export function CopyState({ origin }: { origin: Origin }) {
  if (origin.state === "no_longer_offered") {
    return (
      <span style={pill(c.danger)} title={`Its calendar was removed from ${globalAgencyName()}'s catalogue. Make it your own to edit it, or delete it.`}>
        no longer offered
      </span>
    );
  }
  if (origin.state === "update_available") {
    return (
      <span style={pill(c.warning)} title={origin.blocked ? `The update cannot be applied: ${origin.blocked}` : pendingSummary(origin)}>
        {origin.blocked ? "update blocked" : "update available"}
      </span>
    );
  }
  return <span style={pill(origin.updateMode === "auto" ? c.info : c.success)}>{origin.updateMode === "auto" ? "updates automatically" : "up to date"}</span>;
}

/** A copy's own actions, for its agency's calendar editors: Update, its mode, Make it our own. */
export function CopyActions({ cal, onChanged }: { cal: Calendar; onChanged: () => void }) {
  const origin = cal.fromCentral!;
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [detaching, setDetaching] = useState(false);
  const name = cal.name ?? "";
  const query = cal.ownerAgency ? { agencyId: cal.ownerAgency } : {};
  const gone = origin.state === "no_longer_offered";

  const run = async (call: () => Promise<{ error?: unknown }>) => {
    setBusy(true);
    setErr(null);
    const { error } = await call();
    setBusy(false);
    if (error) setErr(errMsg(error));
    else onChanged();
  };
  const update = () =>
    run(() => api.POST("/calendars/{name}/update", { params: { path: { name }, query, header: csrfHeader } } as never));
  const setMode = (updateMode: UpdateMode) =>
    run(() => api.PUT("/calendars/{name}/update-mode", { params: { path: { name }, query, header: csrfHeader }, body: { updateMode } } as never));
  const detach = () =>
    run(() => api.POST("/calendars/{name}/detach", { params: { path: { name }, query, header: csrfHeader } } as never)).then(() =>
      setDetaching(false),
    );

  return (
    <div style={{ display: "inline-flex", gap: 6, alignItems: "center", flexWrap: "wrap", justifyContent: "flex-end" }}>
      {origin.state === "update_available" && !origin.blocked && (
        <Btn small primary onClick={update} disabled={busy} title={pendingSummary(origin) || undefined}>
          Update
        </Btn>
      )}
      {!gone && (
        <select
          aria-label={`How ${name} takes updates from ${globalAgencyName()}`}
          value={origin.updateMode}
          disabled={busy}
          onChange={(e) => setMode(e.target.value as UpdateMode)}
          style={{ padding: "4px 6px", fontSize: c.fontXs, background: c.panelInput, color: c.text, border: `1px solid ${c.borderStrong}`, borderRadius: c.radiusChip }}
        >
          <option value="ask">Ask before updating</option>
          <option value="auto">Update automatically</option>
        </select>
      )}
      <Btn small onClick={() => setDetaching(true)} disabled={busy}>
        Make it our own
      </Btn>
      {err && (
        <span role="alert" style={{ color: c.danger, fontSize: c.fontXs, flexBasis: "100%", textAlign: "right" }}>
          {err}
        </span>
      )}
      {detaching && (
        <ConfirmDialog
          title={`Make ${name} your own?`}
          danger={false}
          busy={busy}
          message={
            <>
              It becomes an ordinary calendar of this agency, which its calendar editors change. It is no longer linked to
              {origin.catalogueName ? ` ${origin.catalogueName} in` : ""} {globalAgencyName()}'s catalogue, and takes no more updates from
              it. Its days and bindings stay as they are.
            </>
          }
          confirmLabel="Make it our own"
          onCancel={() => setDetaching(false)}
          onConfirm={detach}
        />
      )}
    </div>
  );
}

/** Add from Central (CA-26): an agency's calendar editor takes a copy of a catalogue calendar. */
export function AddFromCentralDialog({ onClose, onAdded }: { onClose: () => void; onAdded: () => void }) {
  const q = useGet<{ items?: CatalogueCalendar[] }>(() => api.GET("/calendars/catalogue"), []);
  const items = q.data?.items ?? [];
  const [picked, setPicked] = useState<CatalogueCalendar | null>(null);
  const [name, setName] = useState("");
  const [mode, setMode] = useState<UpdateMode>("ask");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const add = async () => {
    if (!picked) return;
    setBusy(true);
    setErr(null);
    const { error } = await api.POST("/calendars/from-central", {
      params: { header: csrfHeader },
      body: { catalogueUid: picked.uid, ...(name.trim() && name.trim() !== picked.name ? { name: name.trim() } : {}), updateMode: mode },
    } as never);
    setBusy(false);
    if (error) return setErr(errMsg(error));
    onAdded();
  };

  return (
    <Modal
      title={`Add a calendar from ${globalAgencyName()}`}
      onClose={onClose}
      footer={
        <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
          {err && <div role="alert" style={{ color: c.danger, fontSize: c.fontSm }}>{err}</div>}
          <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
            <Btn onClick={onClose} disabled={busy}>Cancel</Btn>
            <Btn primary onClick={add} disabled={busy || !picked}>{busy ? "Adding…" : "Add"}</Btn>
          </div>
        </div>
      }
    >
      <div style={{ display: "flex", flexDirection: "column", gap: 12, fontSize: c.fontSm }}>
        <div style={{ color: c.textSec }}>
          A copy is this agency's own calendar, linked to {globalAgencyName()}'s: it changes only when {globalAgencyName()}'s does, by an update
          you take or that comes by itself. Make it your own later to edit it.
        </div>
        {q.loading && <div style={{ color: c.textSec }}>Loading the catalogue…</div>}
        {q.error && <div role="alert" style={{ color: c.danger }}>{q.error}</div>}
        {!q.loading && !q.error && items.length === 0 && (
          <div style={{ color: c.textMuted }}>
            {globalAgencyName()}'s catalogue has no calendars yet. A global administrator adds them, working in {globalAgencyName()}.
          </div>
        )}
        {items.map((it) => {
          const on = picked?.uid === it.uid;
          return (
            <label key={it.uid} style={{ display: "flex", gap: 8, alignItems: "flex-start", padding: 8, border: `1px solid ${on ? c.primary : c.border}`, borderRadius: c.radiusChip, cursor: "pointer" }}>
              <input
                type="radio"
                name="catalogue"
                checked={on}
                onChange={() => {
                  setPicked(it);
                  setName(it.name ?? "");
                  setErr(null);
                }}
                style={{ marginTop: 3 }}
              />
              <span style={{ flex: 1 }}>
                <span style={{ fontFamily: c.mono, fontWeight: 600, color: c.text }}>{it.name}</span>
                {it.global && <span style={{ ...pill(c.info), marginLeft: 8 }}>freeze</span>}
                <span style={{ color: c.textMuted, marginLeft: 8, fontSize: c.fontXs }}>
                  {it.dayCount} days{it.lastDay ? `, through ${it.lastDay}` : ""}
                </span>
                {it.description && <div style={{ color: c.textSec, fontSize: c.fontXs }}>{it.description}</div>}
                {(it.copiedAs ?? []).length > 0 && (
                  <div style={{ color: c.textMuted, fontSize: c.fontXs }}>Already here as {(it.copiedAs ?? []).join(", ")}.</div>
                )}
              </span>
            </label>
          );
        })}
        {picked && (
          <div style={{ display: "flex", gap: 16, flexWrap: "wrap", alignItems: "flex-end" }}>
            <label style={{ color: c.textMuted }}>
              Name here
              <input
                aria-label="Name here"
                value={name}
                onChange={(e) => setName(e.target.value)}
                style={{ display: "block", marginTop: 4, padding: "6px 8px", background: c.panelInput, color: c.text, border: `1px solid ${c.borderStrong}`, borderRadius: c.radiusChip, fontFamily: c.mono }}
              />
            </label>
            <UpdateModeChoice value={mode} onChange={setMode} />
          </div>
        )}
      </div>
    </Modal>
  );
}

function UpdateModeChoice({ value, onChange }: { value: UpdateMode; onChange: (m: UpdateMode) => void }) {
  return (
    <fieldset style={{ border: "none", padding: 0, margin: 0, color: c.text }}>
      <legend style={{ color: c.textMuted, marginBottom: 4 }}>When {globalAgencyName()} changes it</legend>
      <label style={{ display: "flex", gap: 6, alignItems: "center" }}>
        <input type="radio" checked={value === "ask"} onChange={() => onChange("ask")} /> Ask before updating
      </label>
      <label style={{ display: "flex", gap: 6, alignItems: "center" }}>
        <input type="radio" checked={value === "auto"} onChange={() => onChange("auto")} /> Update automatically
      </label>
    </fieldset>
  );
}

/** A catalogue calendar's copies, in a few words, for Central's list. */
export function CopiesSummary({ cal }: { cal: Calendar }) {
  const copies = cal.copies ?? [];
  if (copies.length === 0) return <span style={{ color: c.textMuted }}>no agency has it</span>;
  const behind = copies.filter((k) => k.state === "update_available").length;
  return (
    <span title={copies.map((k) => `${k.agencyName}: ${k.name} (${(k.state ?? "").replace(/_/g, " ")}${k.updateMode === "auto" ? ", automatic" : ""})`).join("\n")}>
      {copies.length} {copies.length === 1 ? "agency" : "agencies"}
      {behind > 0 && <span style={{ color: c.warning }}> · {behind} behind</span>}
    </span>
  );
}

type Agency = { id: string; name: string };

/**
 * Add to agencies, or Push the update (CA-26, CA-28), a global administrator's,
 * working in Central: chosen agencies get a copy, or their copies the latest.
 */
export function CentralCopyDialog({ cal, kind, onClose, onDone }: { cal: Calendar; kind: "add" | "push"; onClose: () => void; onDone: (said: string) => void }) {
  const agenciesQ = useGet<unknown>(() => api.GET("/agencies"), []);
  const agencies = rows<Agency>(agenciesQ.data).filter((a) => a.id !== GLOBAL_AGENCY);
  const copies = cal.copies ?? [];
  const has = new Set(copies.map((k) => k.agencyId));
  const offered = kind === "add" ? agencies.filter((a) => !has.has(a.id)) : agencies.filter((a) => copies.some((k) => k.agencyId === a.id && k.state === "update_available"));
  const [chosen, setChosen] = useState<Set<string>>(() => new Set(kind === "push" ? copies.filter((k) => k.state === "update_available").map((k) => k.agencyId ?? "") : []));
  const [mode, setMode] = useState<UpdateMode>("ask");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const toggle = (id: string) =>
    setChosen((cur) => {
      const next = new Set(cur);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  const nameOf = (id: string) => agencies.find((a) => a.id === id)?.name ?? id;

  const go = async () => {
    setBusy(true);
    setErr(null);
    const agencyIds = [...chosen];
    if (kind === "add") {
      const { error } = await api.POST("/calendars/from-central", {
        params: { header: csrfHeader },
        body: { catalogueUid: cal.uid, agencyIds, updateMode: mode },
      } as never);
      setBusy(false);
      if (error) return setErr(errMsg(error));
      onDone(`${cal.name} added to ${agencyIds.map(nameOf).join(", ")}.`);
      return;
    }
    const { data, error } = await api.POST("/calendars/catalogue/{uid}/push", {
      params: { path: { uid: cal.uid ?? "" }, header: csrfHeader },
      body: { agencyIds },
    } as never);
    setBusy(false);
    if (error) return setErr(errMsg(error));
    const r = data as { updated?: { agencyId: string }[]; upToDate?: { agencyId: string }[]; blocked?: { agencyId: string; reason: string }[]; noCopy?: string[] };
    const said = [
      r.updated?.length ? `Updated in ${r.updated.map((u) => nameOf(u.agencyId)).join(", ")}.` : "",
      r.upToDate?.length ? `Already up to date in ${r.upToDate.map((u) => nameOf(u.agencyId)).join(", ")}.` : "",
      r.blocked?.length ? `Not applied in ${r.blocked.map((b) => `${nameOf(b.agencyId)} (${b.reason})`).join(", ")}.` : "",
      r.noCopy?.length ? `No copy in ${r.noCopy.map(nameOf).join(", ")}.` : "",
    ]
      .filter(Boolean)
      .join(" ");
    onDone(said || "Nothing to update.");
  };

  return (
    <Modal
      title={kind === "add" ? `Add ${cal.name} to agencies` : `Push the update of ${cal.name}`}
      onClose={onClose}
      footer={
        <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
          {err && <div role="alert" style={{ color: c.danger, fontSize: c.fontSm }}>{err}</div>}
          <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
            <Btn onClick={onClose} disabled={busy}>Cancel</Btn>
            <Btn primary onClick={go} disabled={busy || chosen.size === 0}>
              {busy ? "Working…" : kind === "add" ? "Add" : "Push the update"}
            </Btn>
          </div>
        </div>
      }
    >
      <div style={{ display: "flex", flexDirection: "column", gap: 12, fontSize: c.fontSm }}>
        <div style={{ color: c.textSec }}>
          {kind === "add"
            ? "Each agency chosen gets its own copy, read-only there and linked to this calendar. It is recorded as done in each agency."
            : "Each copy chosen takes this calendar's days and settings now, as if the agency had updated it. An update an agency's bindings forbid is not applied, and is reported."}
        </div>
        {offered.length === 0 && (
          <div style={{ color: c.textMuted }}>{kind === "add" ? "Every agency has a copy already." : "No copy is behind."}</div>
        )}
        {offered.map((a) => (
          <label key={a.id} style={{ display: "flex", gap: 8, alignItems: "center", color: c.text }}>
            <input type="checkbox" checked={chosen.has(a.id)} onChange={() => toggle(a.id)} aria-label={a.name} /> {a.name}
          </label>
        ))}
        {kind === "add" && offered.length > 0 && <UpdateModeChoice value={mode} onChange={setMode} />}
      </div>
    </Modal>
  );
}

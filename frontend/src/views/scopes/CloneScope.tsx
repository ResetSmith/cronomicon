import { useState } from "react";
import { api, csrfHeader, errMsg } from "../../api/client";
import { GLOBAL_AGENCY, agenciesFor, useMyAccess, type AgencyRef } from "../../api/access";
import { useGet, rows } from "../../hooks";
import { Btn, Modal } from "../../components/ui";
import { c } from "../../theme";
import { inputStyle, labelStyle } from "../envvars/ui";
import { globalAgencyName } from "../../globalName";

/**
 * CloneScopeDialog — Scopes → a scope → "Clone to an agency" (2.4.0; R6 has
 * the rule, the owner's decision of 2026-10-13).
 *
 * A scope does not change agency (that is a global administrator's repair);
 * another agency gets one by a clone: a NEW scope, built in the app, the
 * target agency's, with a name of its own. It copies the hosts, groups,
 * variables, inventory and tags, and a record per host with no key and no
 * bastion. It does not copy the runner bindings, the approved host keys or the
 * jobs. Afterwards the dialog says what was copied and what was not.
 *
 * Who: configureApp on the scope's agency AND on the target (a global
 * administrator for Global, on either side). The targets offered are the
 * agencies the caller administers (every agency and Global for a global
 * administrator), without the scope's own.
 */
export function CloneScopeDialog({
  scopeId,
  scopeName,
  currentAgency,
  globalAdmin,
  onClose,
  onCloned,
}: {
  scopeId: string;
  scopeName: string;
  /** The agency the scope is in, which is not a target. */
  currentAgency?: string;
  globalAdmin: boolean;
  onClose: () => void;
  onCloned: (message: string) => void;
}) {
  const access = useMyAccess();
  const catalogQ = useGet<unknown>(() => api.GET("/agencies"), []);
  const catalog = rows<{ id: string; name: string }>(catalogQ.data);
  const targets: AgencyRef[] = (
    globalAdmin
      ? [{ id: GLOBAL_AGENCY, name: globalAgencyName() }, ...catalog.filter((a) => a.id !== GLOBAL_AGENCY)]
      : agenciesFor(access ?? null, "configureApp")
  ).filter((a) => a.id !== (currentAgency || GLOBAL_AGENCY));
  const [target, setTarget] = useState("");
  const [name, setName] = useState(scopeName);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [done, setDone] = useState<{ copied: Record<string, number>; note: string; to: string; name: string } | null>(null);
  const chosen = targets.find((a) => a.id === target) ?? (targets.length === 1 ? targets[0] : undefined);

  const clone = async () => {
    if (!chosen) return setErr("Choose the agency the clone is for.");
    if (!name.trim()) return setErr("The clone needs a name.");
    setBusy(true);
    setErr("");
    const { data, error } = await api.POST("/scopes/{scopeId}/clone", {
      params: { path: { scopeId }, header: csrfHeader },
      body: { agencyId: chosen.id, name: name.trim() },
    });
    setBusy(false);
    if (error) return setErr(errMsg(error));
    const d = (data ?? {}) as { copied?: Record<string, number>; note?: string };
    setDone({ copied: d.copied ?? {}, note: d.note ?? "", to: chosen.name, name: name.trim() });
  };

  if (done) {
    const n = (k: string) => done.copied[k] ?? 0;
    return (
      <Modal
        title={`${done.name} is ${done.to}'s`}
        onClose={() => onCloned(`Cloned ${scopeName} to ${done.to} as ${done.name}`)}
        footer={
          <div style={{ display: "flex", justifyContent: "flex-end" }}>
            <Btn primary onClick={() => onCloned(`Cloned ${scopeName} to ${done.to} as ${done.name}`)}>
              Done
            </Btn>
          </div>
        }
      >
        <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
          <p style={{ marginTop: 0 }}>
            Copied: {n("hosts")} hosts, {n("groups")} groups, {n("hostVars")} host variables, {n("groupVars")} group variables and{" "}
            {n("hostRecords")} host records, with the inventory as written and the tags.
          </p>
          <p>
            Not copied: the runner bindings, the approved host keys, the jobs, and each host record's SSH key and bastion.{" "}
            {done.to} sets its own before the clone connects to anything.
          </p>
          {done.note && <p style={{ marginBottom: 0 }}>{done.note}</p>}
        </div>
      </Modal>
    );
  }

  return (
    <Modal
      title={`Clone ${scopeName} to an agency`}
      onClose={onClose}
      footer={
        <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
          <Btn onClick={onClose} disabled={busy}>
            Cancel
          </Btn>
          <Btn primary onClick={clone} disabled={busy || !chosen}>
            {busy ? "Cloning…" : "Clone"}
          </Btn>
        </div>
      }
    >
      <div style={{ display: "flex", flexDirection: "column", gap: 12, fontSize: c.fontSm, color: c.textSec }}>
        <div style={{ lineHeight: 1.6 }}>
          A new scope, built in the app, the agency's own. It copies the hosts, groups, variables, inventory and tags, and a
          record per host without its SSH key or bastion. The runner bindings, approved host keys and jobs stay with this
          scope, which stays where it is.
        </div>
        {targets.length === 0 ? (
          <div role="note">You administer no other agency to clone this scope to.</div>
        ) : (
          <label style={{ display: "block" }}>
            <span style={labelStyle()}>Agency</span>
            <select aria-label="Clone to agency" value={chosen?.id ?? ""} onChange={(e) => setTarget(e.target.value)} style={inputStyle()}>
              {targets.length > 1 && <option value="">Choose an agency…</option>}
              {targets.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.name}
                </option>
              ))}
            </select>
          </label>
        )}
        <label style={{ display: "block" }}>
          <span style={labelStyle()}>Name</span>
          <input aria-label="Clone name" value={name} onChange={(e) => setName(e.target.value)} style={inputStyle()} />
        </label>
        {err && (
          <div role="alert" style={{ color: c.danger, fontSize: c.fontXs }}>
            {err}
          </div>
        )}
      </div>
    </Modal>
  );
}

import { useState } from "react";
import { api, csrfHeader, errMsg } from "../../api/client";
import { GLOBAL_AGENCY, type AgencyRef } from "../../api/access";
import { REPAIR_HINT, RepairDialog, useRepairAllowed, type RepairBody } from "../../components/RepairDialog";
import { useGet, rows } from "../../hooks";
import { c } from "../../theme";
import { inputStyle } from "./ui";

type Kind = "secret" | "env-var" | "ssh-credential";

const NOUN: Record<Kind, string> = { secret: "secret", "env-var": "variable", "ssh-credential": "SSH key" };
const ROUTE = {
  secret: "/secret-agencies",
  "env-var": "/env-var-agencies",
  "ssh-credential": "/ssh-credential-agencies",
} as const;

/**
 * MoveAgency — repair the agency of a secret, variable or SSH key (v2.3.0;
 * a repair since 2.4.0 I4, IN-7; the owner's answer to question 48).
 *
 * Each belongs to exactly one agency, which owns it: its administrators change
 * it and only its runs may use it (Global's too, since 2.4.0, §0a). The
 * owner is chosen at creation. Since 2.4.0 it does not change by an agency
 * administrator's move: another agency gets one by making it again. What is
 * left is a global administrator's repair of a placement that was a mistake,
 * with a reason written to the audit log (`PUT /{kind}-agencies` with
 * `{repair, reason, assignments}`).
 *
 * A global administrator gets the control, offering every agency but the one
 * it is in. Anyone else reads that the agency does not change (REPAIR_HINT).
 */
export function MoveAgency({
  kind,
  id,
  name,
  ownerName,
  canRepair,
  onMoved,
  onError,
}: {
  kind: Kind;
  id: string;
  /** The row's own name (a key, a label), for the confirmation. */
  name: string;
  /** The owning agency's NAME, as the row reports it; "Global" for Global's. */
  ownerName?: string | null;
  /** The row's own canRepair (R8); without it, whether the caller is a global administrator. */
  canRepair?: boolean;
  /** Unused since the move became a repair; kept so callers need not change. */
  permission?: "manageEnvVars" | "configureApp";
  onMoved: (message: string) => void;
  onError: (message: string) => void;
}) {
  const globalAdmin = useRepairAllowed();
  const allowed = canRepair ?? globalAdmin;
  const catalogQ = useGet<unknown>(() => api.GET("/agencies"), []);
  const catalog = rows<{ id: string; name: string }>(catalogQ.data);
  const [target, setTarget] = useState<AgencyRef | null>(null);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState("");

  const owner = ownerName || "Global";
  const ownerId = owner === "Global" ? GLOBAL_AGENCY : catalog.find((a) => a.name === owner)?.id;
  const candidates: AgencyRef[] = [{ id: GLOBAL_AGENCY, name: "Global" }, ...catalog.filter((a) => a.id !== GLOBAL_AGENCY)].filter(
    (a) => a.id !== ownerId,
  );

  if (!allowed) {
    return <div style={{ marginTop: 10, paddingTop: 10, borderTop: `1px solid ${c.borderLight}`, fontSize: c.fontXs, color: c.textMuted }}>Owned by {owner}. {REPAIR_HINT}</div>;
  }
  if (catalog.length === 0 || candidates.length === 0) return null;

  const move = async (rep: RepairBody) => {
    if (!target) return;
    setBusy(true);
    setRefusal("");
    const { error } = await api.PUT(ROUTE[kind], {
      params: { header: csrfHeader },
      body: { ...rep, assignments: [{ id, agencyIds: [target.id] }] } as never,
    });
    setBusy(false);
    if (error) {
      setRefusal(errMsg(error));
      onError(`Repair failed: ${errMsg(error)}`);
      return;
    }
    const to = target.name;
    setTarget(null);
    onMoved(`${name} now belongs to ${to}`);
  };

  return (
    <div style={{ marginTop: 10, paddingTop: 10, borderTop: `1px solid ${c.borderLight}`, display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
      <select
        aria-label={`Repair: move ${name} to another agency`}
        value=""
        disabled={busy}
        onChange={(e) => {
          const a = candidates.find((x) => x.id === e.target.value);
          if (a) {
            setRefusal("");
            setTarget(a);
          }
        }}
        style={{ ...inputStyle(), width: "auto", maxWidth: 260, fontSize: c.fontSm, cursor: "pointer" }}
      >
        <option value="">Repair: move to another agency…</option>
        {candidates.map((a) => (
          <option key={a.id} value={a.id}>
            {a.name}
          </option>
        ))}
      </select>
      <span style={{ fontSize: c.fontXs, color: c.textMuted }}>Owned by {owner}. Moving it is a repair of a placement that was a mistake.</span>
      {target && (
        <RepairDialog
          title={`Move ${name} to ${target.name}?`}
          message={
            <>
              {target.id === GLOBAL_AGENCY ? (
                <>It becomes Global's: only Global's runs may use it, and only global administrators change it.</>
              ) : (
                <>
                  {target.name}'s administrators will manage it, and only {target.name}'s runs will resolve it.
                </>
              )}{" "}
              {owner === "Global" ? "Runs of every other agency" : `${owner}'s runs`} that bind this {NOUN[kind]} will stop resolving it
              the next time they start. Check where it is used before moving it.
            </>
          }
          confirmLabel={`Move to ${target.name}`}
          busy={busy}
          error={refusal}
          onCancel={() => setTarget(null)}
          onConfirm={move}
        />
      )}
    </div>
  );
}

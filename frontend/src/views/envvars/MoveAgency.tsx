import { useEffect, useState } from "react";
import { api, csrfHeader, errMsg, fetchCapabilities } from "../../api/client";
import { GLOBAL_AGENCY, agenciesFor, useMyAccess, type AgencyRef } from "../../api/access";
import { ConfirmDialog } from "../../components/ui";
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
 * MoveAgency — give a secret, variable or SSH key to another agency (v2.3.0).
 *
 * Each belongs to exactly one agency, which owns it: its administrators change
 * it and only its runs may use it (Global's is usable by every agency). The
 * owner is chosen at creation; this is the one way it changes afterwards. The
 * server asks for the kind's permission on the agency that has the row AND on
 * the agency it goes to, and, for a Vault-backed row, a path inside the
 * target's Vault paths (`PUT /{kind}-agencies`).
 *
 * What is offered follows that rule: the agencies the caller administers (and
 * Global, for a global administrator), without the one it is in. A caller who
 * does not administer the row's present owner sees the control disabled with
 * whose it is; one with nowhere to move it to sees nothing, since there is
 * nothing for the control to do.
 */
export function MoveAgency({
  kind,
  id,
  name,
  ownerName,
  permission,
  onMoved,
  onError,
}: {
  kind: Kind;
  id: string;
  /** The row's own name (a key, a label), for the confirmation. */
  name: string;
  /** The owning agency's NAME, as the row reports it; "Global" for Global's. */
  ownerName?: string | null;
  permission: "manageEnvVars" | "configureApp";
  onMoved: (message: string) => void;
  onError: (message: string) => void;
}) {
  const access = useMyAccess();
  const [global, setGlobal] = useState(false);
  useEffect(() => {
    let cancelled = false;
    void fetchCapabilities().then((caps) => {
      if (!cancelled) setGlobal(!!(permission === "manageEnvVars" ? caps.manageEnvVarsGlobal : caps.configureAppGlobal));
    });
    return () => {
      cancelled = true;
    };
  }, [permission]);
  const catalogQ = useGet<unknown>(() => api.GET("/agencies"), []);
  const catalog = rows<{ id: string; name: string }>(catalogQ.data);
  const [target, setTarget] = useState<AgencyRef | null>(null);
  const [busy, setBusy] = useState(false);

  const owner = ownerName || "Global";
  const ownerId = owner === "Global" ? GLOBAL_AGENCY : catalog.find((a) => a.name === owner)?.id;
  const mine = agenciesFor(access ?? null, permission);
  const candidates: AgencyRef[] = (global ? [{ id: GLOBAL_AGENCY, name: "Global" }, ...catalog.filter((a) => a.id !== GLOBAL_AGENCY)] : mine).filter(
    (a) => a.id !== ownerId,
  );
  // Unknown access is not second-guessed: the server decides.
  const ownsIt = global || access == null || (!!ownerId && mine.some((a) => a.id === ownerId));
  const why = ownsIt
    ? ""
    : owner === "Global"
      ? `This ${NOUN[kind]} is Global's — only a global administrator (a role on every agency) can move it.`
      : `This ${NOUN[kind]} belongs to ${owner} — moving it takes authority over that agency too.`;
  if (candidates.length === 0) return null;

  const move = async () => {
    if (!target) return;
    setBusy(true);
    const { error } = await api.PUT(ROUTE[kind], {
      params: { header: csrfHeader },
      body: [{ id, agencyIds: [target.id] }],
    });
    setBusy(false);
    const to = target.name;
    setTarget(null);
    if (error) onError(`Move failed: ${errMsg(error)}`);
    else onMoved(`${name} now belongs to ${to}`);
  };

  return (
    <div style={{ marginTop: 10, paddingTop: 10, borderTop: `1px solid ${c.borderLight}`, display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
      <select
        aria-label={`Move ${name} to another agency`}
        value=""
        disabled={!!why || busy}
        title={why || undefined}
        onChange={(e) => {
          const a = candidates.find((x) => x.id === e.target.value);
          if (a) setTarget(a);
        }}
        style={{ ...inputStyle(), width: "auto", maxWidth: 260, fontSize: c.fontSm, cursor: why ? "not-allowed" : "pointer" }}
      >
        <option value="">Move to another agency…</option>
        {candidates.map((a) => (
          <option key={a.id} value={a.id}>
            {a.name}
          </option>
        ))}
      </select>
      <span style={{ fontSize: c.fontXs, color: c.textMuted }}>{why || `Owned by ${owner}. Moving it changes who manages it and whose runs may use it.`}</span>
      {target && (
        <ConfirmDialog
          title={`Move ${name} to ${target.name}?`}
          message={
            <>
              {target.id === GLOBAL_AGENCY ? (
                <>It becomes Global's: every agency's runs may use it, and only global administrators change it.</>
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
          onCancel={() => setTarget(null)}
          onConfirm={move}
        />
      )}
    </div>
  );
}

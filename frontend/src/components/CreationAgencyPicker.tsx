import { useEffect, useState } from "react";
import { api, fetchCapabilities } from "../api/client";
import { GLOBAL_AGENCY, agenciesFor, fetchMyAccess, type AgencyRef } from "../api/access";
import { c } from "../theme";

type Agency = { id?: string; name?: string };

/**
 * useCreationAgencies — "whose is this?" on a create form.
 *
 * A scope, secret, variable and SSH key belongs to exactly ONE agency (v2.3.0,
 * LR-7, LR-54), and that agency is its owner: its administrators change it, and
 * only its runs may use it. The form therefore asks for one agency, once, at
 * creation.
 *
 *   - An administrator of ONE agency is not asked: the row is theirs, and the
 *     server would place it there anyway (RA-9). The picker shows which.
 *   - An administrator of SEVERAL must say which (the server answers 422
 *     `agency_required` otherwise). Their own agencies are offered and no others.
 *   - A GLOBAL administrator may name any agency, or leave it Global's, which is
 *     the default: usable by every agency, changed only by global administrators.
 *
 * Until 2.3.0 this was a multi-select shown only to restricted callers, because
 * a row could be shared by several agencies and "no agency" meant shared
 * infrastructure. Global is a real agency now, and it is offered to global
 * administrators only: for anyone else it is a refusal waiting to happen.
 *
 * `permission` names the verb the create route checks — "configureApp" for
 * scopes and SSH keys, "manageEnvVars" for secrets and variables. With none it
 * falls back to the permission-blind `unrestricted`, as before.
 */
export function useCreationAgencies(isEdit: boolean, permission?: "configureApp" | "manageEnvVars") {
  const [global, setGlobal] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [agencies, setAgencies] = useState<AgencyRef[]>([]);
  const [selected, setSelected] = useState("");

  useEffect(() => {
    if (isEdit) return;
    let cancelled = false;
    (async () => {
      // Fail CLOSED on a capabilities error: CAPS_OFF reports every flag false,
      // so the caller is treated as restricted and must choose. The reverse
      // would offer Global to someone the server will refuse.
      const caps = await fetchCapabilities();
      const isGlobal =
        permission === "configureApp"
          ? !!caps.configureAppGlobal
          : permission === "manageEnvVars"
            ? !!caps.manageEnvVarsGlobal
            : !!caps.unrestricted;
      const catalog = async (): Promise<AgencyRef[]> => {
        const res = await api.GET("/agencies");
        return ((res.data as Agency[] | undefined) ?? [])
          .filter((a) => a.id && a.id !== GLOBAL_AGENCY)
          .map((a) => ({ id: a.id as string, name: a.name || (a.id as string) }));
      };
      let options: AgencyRef[] = [];
      try {
        if (isGlobal) {
          options = [{ id: GLOBAL_AGENCY, name: "Global" }, ...(await catalog())];
        } else {
          const mine = permission ? agenciesFor(await fetchMyAccess(), permission) : [];
          // Their own agencies when the server could say; otherwise the catalog,
          // so the operator can still comply (the server refuses a wrong pick).
          options = mine.length > 0 ? mine : await catalog();
        }
      } catch {
        // A failed read leaves whatever was gathered (Global, for a global
        // administrator). The form must still settle: with nothing to choose
        // from, the server names the owner or says what is missing.
        if (isGlobal && options.length === 0) options = [{ id: GLOBAL_AGENCY, name: "Global" }];
      }
      if (cancelled) return;
      setGlobal(isGlobal);
      setAgencies(options);
      // Global for a global administrator; the one agency for an administrator
      // of one; nothing for an administrator of several, who must choose.
      setSelected(isGlobal ? GLOBAL_AGENCY : options.length === 1 ? options[0].id : "");
      setLoaded(true);
    })();
    return () => {
      cancelled = true;
    };
  }, [isEdit, permission]);

  const visible = !isEdit;
  return {
    /** Mount the picker (every create form; never an edit). */
    visible,
    /** A choice must be made before submitting: everyone but a global administrator. */
    required: visible && !global,
    global,
    agencies,
    selected,
    setSelected,
    /** Spread into the create body. The API takes a list and accepts exactly one. */
    body: visible && selected ? { agencyIds: [selected] } : {},
    /** Non-empty when the form must not submit yet. */
    // Nothing to choose from (the lists could not be read) does not block: the
    // server places the row in the caller's one agency, or says which to name.
    blockedReason: visible && loaded && !global && !selected && agencies.length > 0 ? "Choose an agency" : "",
  };
}

export type CreationAgencies = ReturnType<typeof useCreationAgencies>;

/**
 * CreationAgencyPicker renders the select for the hook above. It renders
 * nothing on an edit, so every call site can mount it unconditionally.
 */
export function CreationAgencyPicker({
  label,
  pick,
}: {
  /** The entity noun, for the helper text: "secret", "variable", "SSH key", "scope". */
  label: string;
  pick: CreationAgencies;
}) {
  if (!pick.visible) return null;
  const only = !pick.global && pick.agencies.length === 1;
  return (
    <div style={{ marginTop: 12 }}>
      <label style={{ display: "block", fontSize: c.fontSm, color: c.textMuted, marginBottom: 4 }}>
        Agency {pick.required && <span style={{ color: c.danger }}>*</span>}
        <select
          aria-label="Agency"
          value={pick.selected}
          disabled={only}
          onChange={(e) => pick.setSelected(e.target.value)}
          style={{
            display: "block",
            width: "100%",
            marginTop: 4,
            background: c.panelInput,
            color: c.text,
            border: `1px solid ${c.borderStrong}`,
            borderRadius: c.radiusChip,
            padding: 6,
            fontSize: c.fontBody,
          }}
        >
          {!pick.global && !only && <option value="">Choose an agency…</option>}
          {pick.agencies.map((a) => (
            <option key={a.id} value={a.id}>
              {a.name}
            </option>
          ))}
        </select>
      </label>
      <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 4 }}>
        {pick.global ? (
          <>
            A {label} belongs to one agency. Global's is usable by every agency and changed only by global
            administrators; an agency's is that agency's own.
          </>
        ) : only ? (
          <>This {label} will belong to your agency, whose administrators manage it.</>
        ) : (
          <>
            A {label} belongs to one agency, whose administrators manage it. You administer several, so choose the one
            that owns this.
          </>
        )}
      </div>
    </div>
  );
}

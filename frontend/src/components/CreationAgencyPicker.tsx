import { useEffect, useState } from "react";
import { api, fetchCapabilities } from "../api/client";
import { c } from "../theme";

type Agency = { id?: string; name?: string };

/**
 * useCreationAgencies — the client half of the RF-Q2(a) creation rule
 * (the RBAC-fixes plan).
 *
 * A secret, variable or SSH key created with NO agency membership is shared
 * infrastructure, and RB-Q14 makes shared infrastructure unrestricted-only for
 * writes and reveal. So a department-scoped operator who creates one without
 * naming an agency is locked out of the row the moment it exists — the server
 * therefore 422s them with `agency_required`.
 *
 * That is the right server rule and the wrong user experience on its own: a rule
 * whose only compliance path is a rejected request is the exact shape RB-29 was
 * written to avoid. This hook plus CreationAgencyPicker are the compliance path —
 * the control appears precisely when the caller is restricted and the form is a
 * CREATE, and it is required there and absent everywhere else.
 *
 * Unrestricted callers see nothing: omitting membership is how they deliberately
 * mint shared infrastructure, which is a choice only they can make.
 */
export function useCreationAgencies(isEdit: boolean) {
  const [restricted, setRestricted] = useState(false);
  const [agencies, setAgencies] = useState<Agency[]>([]);
  const [selected, setSelected] = useState<string[]>([]);

  useEffect(() => {
    if (isEdit) return;
    let cancelled = false;
    // Fail CLOSED on a capabilities error: CAPS_OFF reports unrestricted=false, so
    // the picker appears and the operator can comply. The reverse (hiding it) would
    // send them into a 422 with no control to fix it.
    fetchCapabilities().then((caps) => {
      if (!cancelled) setRestricted(!caps.unrestricted);
    });
    api.GET("/agencies").then((res) => {
      if (!cancelled && res.data) setAgencies(res.data as Agency[]);
    });
    return () => {
      cancelled = true;
    };
  }, [isEdit]);

  const required = !isEdit && restricted;
  return {
    /** Show the picker, and block submission until something is chosen. */
    required,
    agencies,
    selected,
    setSelected,
    /** Spread into the create body; empty for edits and unrestricted callers. */
    body: !isEdit && selected.length > 0 ? { agencyIds: selected } : {},
    /** Non-empty when the form must not submit yet. */
    blockedReason: required && selected.length === 0 ? "Choose at least one agency" : "",
  };
}

/**
 * CreationAgencyPicker renders the multi-select for the hook above. It renders
 * nothing unless the rule applies, so every call site can mount it
 * unconditionally.
 */
export function CreationAgencyPicker({
  label,
  required,
  agencies,
  selected,
  setSelected,
}: {
  /** The entity noun, for the helper text: "secret", "variable", "SSH key". */
  label: string;
  required: boolean;
  agencies: Agency[];
  selected: string[];
  setSelected: (next: string[]) => void;
}) {
  if (!required) return null;
  return (
    <div style={{ marginTop: 12 }}>
      <label style={{ display: "block", fontSize: c.fontSm, color: c.textMuted, marginBottom: 4 }}>
        Agencies <span style={{ color: c.danger }}>*</span>
      </label>
      <select
        multiple
        size={Math.min(Math.max(agencies.length, 3), 6)}
        value={selected}
        onChange={(e) => setSelected(Array.from(e.target.selectedOptions, (o) => o.value))}
        style={{
          width: "100%",
          background: c.panelInput,
          color: c.text,
          border: `1px solid ${c.border}`,
          borderRadius: c.radiusSurface,
          padding: 6,
        }}
      >
        {agencies.map((a) => (
          <option key={a.id} value={a.id}>
            {a.name}
          </option>
        ))}
      </select>
      <div style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 4 }}>
        Your access is department-scoped, so this {label} must belong to one of your
        agencies — otherwise it would count as shared infrastructure and only an
        unrestricted operator could change it.
      </div>
    </div>
  );
}

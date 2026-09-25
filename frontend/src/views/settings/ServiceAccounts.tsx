import { useCallback, useEffect, useState } from "react";
import { api, csrfHeader } from "../../api/client";
import { c } from "../../theme";
import { Btn, Card, SettingRow, StatusBadge, errMsg, fmtDateTime, inputStyle, lblStyle, tdStyle, thStyle } from "./ui";

// Service Accounts (ET-A/ET-C, the prod-features plan §1).
//
// The shown-once idiom is copied deliberately from Runners.tsx's registration
// tokens (LB8): the plaintext exists only in the mint response, so every
// copyable affordance is gated on holding that response in state, and the copy
// says plainly that it cannot be recovered. Reload the page and it is gone —
// which is the point, not a rough edge.

type ServiceAccount = {
  id: string;
  name: string;
  description?: string;
  role: string;
  agencyId?: string;
  agencyName?: string;
  allScopes: boolean;
  createdBy: string;
  createdAt: string;
  expiresAt?: string;
  revokedAt?: string;
  lastUsedAt?: string;
  status: string;
};

type Minted = ServiceAccount & { token: string };
type Agency = { id: string; name: string };
type Role = { name: string; label?: string };

export function ServiceAccountsSection() {
  const [items, setItems] = useState<ServiceAccount[] | null>(null);
  const [agencies, setAgencies] = useState<Agency[]>([]);
  const [roles, setRoles] = useState<Role[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // The plaintext token, held ONLY until the operator dismisses it.
  const [minted, setMinted] = useState<Minted | null>(null);
  const [copied, setCopied] = useState(false);

  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [role, setRole] = useState("operator");
  const [where, setWhere] = useState<string>(""); // "" = all scopes, else agency id
  const [expiresAt, setExpiresAt] = useState("");

  const load = useCallback(async () => {
    const { data, error: e } = await api.GET("/service-accounts");
    if (e) {
      setError(errMsg(e));
      setItems([]);
      return;
    }
    setItems(((data as { items?: ServiceAccount[] } | undefined)?.items ?? []) as ServiceAccount[]);
  }, []);

  useEffect(() => {
    load();
    api.GET("/agencies").then(({ data }) => setAgencies((data as Agency[]) ?? []));
    api.GET("/roles").then(({ data }) => setRoles((data as Role[]) ?? []));
  }, [load]);

  async function create() {
    setBusy(true);
    setError(null);
    try {
      const body: Record<string, unknown> = { name: name.trim(), description: description.trim(), role };
      if (where === "") body.allScopes = true;
      else body.agencyId = where;
      if (expiresAt) body.expiresAt = new Date(expiresAt).toISOString();
      const { data, error: e } = await api.POST("/service-accounts", {
        params: { header: csrfHeader },
        body: body as never,
      });
      if (e) {
        setError(errMsg(e));
        return;
      }
      setMinted(data as Minted);
      setCopied(false);
      setName("");
      setDescription("");
      setExpiresAt("");
      await load();
    } finally {
      setBusy(false);
    }
  }

  async function revoke(a: ServiceAccount) {
    if (!confirm(`Revoke "${a.name}"? Anything using this token stops working immediately.`)) return;
    setError(null);
    const { error: e } = await api.DELETE("/service-accounts/{id}", {
      params: { path: { id: a.id }, header: csrfHeader },
    });
    if (e) {
      setError(errMsg(e));
      return;
    }
    if (minted?.id === a.id) setMinted(null);
    await load();
  }

  // LB8: the token is revealable only while the mint response is in hand.
  const revealable = minted != null && minted.token !== "";

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      <Card title="Service Accounts">
        <div style={{ padding: "4px 0 12px", fontSize: c.fontSm, color: c.textSec, lineHeight: 1.5 }}>
          A service account lets a monitoring system, ticketing tool or CI job start a run over the API. The
          token is a <strong>principal, not a bypass</strong>: it carries a role and a place, and every scope,
          concurrency and run-input rule that applies to a person clicking Run applies to it.
          <br />
          A job must also be marked <strong>requestable</strong> before any token can trigger it.
        </div>

        {revealable && (
          <div
            style={{
              border: `1px solid ${c.success}55`,
              background: `${c.success}12`,
              borderRadius: c.radiusSurface,
              padding: "12px 14px",
              marginBottom: 14,
            }}
          >
            <div style={{ fontSize: c.fontSm, fontWeight: 600, color: c.success, marginBottom: 6 }}>
              Token for “{minted!.name}” — shown once
            </div>
            <div style={{ fontSize: c.fontXs, color: c.textSec, marginBottom: 8 }}>
              Only a hash is stored. Copy it now; it cannot be revealed again, and a lost token must be
              replaced rather than recovered.
            </div>
            <div style={{ display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap" }}>
              <code
                data-testid="minted-token"
                style={{
                  flex: 1,
                  minWidth: 260,
                  fontSize: c.fontXs,
                  background: c.panel2,
                  border: `1px solid ${c.border}`,
                  borderRadius: c.radiusChip,
                  padding: "7px 10px",
                  wordBreak: "break-all",
                }}
              >
                {minted!.token}
              </code>
              <Btn
                onClick={() => {
                  navigator.clipboard?.writeText(minted!.token);
                  setCopied(true);
                }}
              >
                {copied ? "Copied" : "Copy"}
              </Btn>
              <Btn onClick={() => setMinted(null)}>Done</Btn>
            </div>
          </div>
        )}

        <SettingRow label="Name" hint="Becomes the audit actor on every run it triggers, as svc:<name>. No spaces or colons.">
          <input style={inputStyle()} value={name} onChange={(e) => setName(e.target.value)} placeholder="nagios" />
        </SettingRow>
        <SettingRow label="Description" hint="What uses this token. Optional, but the thing you will want in a year.">
          <input
            style={inputStyle()}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder="Nagios check_cronomicon wrapper"
          />
        </SettingRow>
        <SettingRow label="Role" hint="The permissions the token acts with — the same roles people hold.">
          <select style={inputStyle()} value={role} onChange={(e) => setRole(e.target.value)}>
            {(roles.length ? roles.map((r) => r.name) : ["admin", "operator", "viewer"]).map((rn) => (
              <option key={rn} value={rn}>
                {rn}
              </option>
            ))}
          </select>
        </SettingRow>
        <SettingRow label="Where" hint="Bind the token to one agency's scopes, or grant every scope. Prefer an agency.">
          <select style={inputStyle()} value={where} onChange={(e) => setWhere(e.target.value)}>
            <option value="">All scopes (*)</option>
            {agencies.map((a) => (
              <option key={a.id} value={a.id}>
                {a.name}
              </option>
            ))}
          </select>
        </SettingRow>
        <SettingRow
          label="Expires"
          hint="Optional. Leaving it empty means the token never expires — convenient, and the reason dead credentials survive for years."
          last
        >
          <input
            type="datetime-local"
            style={inputStyle()}
            value={expiresAt}
            onChange={(e) => setExpiresAt(e.target.value)}
          />
        </SettingRow>

        <div style={{ display: "flex", gap: 10, alignItems: "center", paddingTop: 12 }}>
          <Btn primary onClick={create} disabled={busy || name.trim() === ""}>
            {busy ? "Creating…" : "Create service account"}
          </Btn>
          {error && <span style={{ color: c.danger, fontSize: c.fontSm }}>{error}</span>}
        </div>
      </Card>

      <Card title="Existing accounts">
        {items == null ? (
          <div style={{ color: c.textSec, fontSize: c.fontSm, padding: "8px 0" }}>Loading…</div>
        ) : items.length === 0 ? (
          <div style={{ color: c.textSec, fontSize: c.fontSm, padding: "8px 0" }}>
            No service accounts yet.
          </div>
        ) : (
          <div style={{ overflowX: "auto" }}>
            <table style={{ width: "100%", borderCollapse: "collapse" }}>
              <thead>
                <tr>
                  {["Name", "Role", "Where", "Status", "Last used", "Expires", ""].map((h) => (
                    <th key={h} style={thStyle()}>
                      {h}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {items.map((a) => (
                  <tr key={a.id}>
                    <td style={tdStyle()}>
                      <div style={{ fontWeight: 600 }}>{a.name}</div>
                      {a.description && (
                        <div style={{ fontSize: c.fontXs, color: c.textSec }}>{a.description}</div>
                      )}
                    </td>
                    <td style={tdStyle()}>{a.role}</td>
                    <td style={tdStyle()}>{a.allScopes ? "All scopes (*)" : a.agencyName || a.agencyId || "—"}</td>
                    <td style={tdStyle()}>
                      <StatusBadge status={a.status} />
                    </td>
                    <td style={tdStyle()}>
                      {a.lastUsedAt ? (
                        fmtDateTime(a.lastUsedAt)
                      ) : (
                        <span style={{ color: c.textMuted }}>never</span>
                      )}
                    </td>
                    <td style={tdStyle()}>{a.expiresAt ? fmtDateTime(a.expiresAt) : "—"}</td>
                    <td style={{ ...tdStyle(), textAlign: "right" }}>
                      {a.status !== "revoked" && <Btn onClick={() => revoke(a)}>Revoke</Btn>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <div style={{ ...lblStyle(), paddingTop: 10, fontSize: c.fontXs, color: c.textSec }}>
          Revoking keeps the row: the name is the audit actor on every run it ever triggered, and deleting it
          would make that history unreadable.
        </div>
      </Card>
    </div>
  );
}

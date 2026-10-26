import { useEffect, useState } from "react";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { c } from "../../theme";
import { GLOBAL_AGENCY, agenciesFor, useMyAccess } from "../../api/access";
import { useAgencyOptional } from "../../agency";
import { Btn, Card, ReadOnlyFields, SettingRow, csrfHeader, errMsg, inputStyle, tdStyle, thStyle } from "./ui";
import { ConfirmDialog, InlineLoading } from "../../components/ui";
import { globalAgencyName } from "../../globalName";

type AgencyVault = components["schemas"]["AgencyVault"];
type VaultRef = components["schemas"]["AgencyVaultReference"];

// Settings → Agency Vault (2.5.1 SG3). An agency may connect a
// Vault of its own; one that does not uses the installation's. A Vault-backed
// secret or SSH key resolves in its OWNER agency's Vault (SG-21), and never
// falls back to the installation's when the agency's own cannot be used.
// configureApp on the agency changes it; anyone else sees why not. In Global,
// a pointer to the installation's Vault.
export function AgencyVaultSection({ isGlobal }: { isGlobal: boolean }) {
  const ag = useAgencyOptional();
  const access = useMyAccess();
  const mine = agenciesFor(access ?? null, "configureApp");
  const agency = ag?.agency ?? (isGlobal ? GLOBAL_AGENCY : mine.length === 1 ? mine[0].id : "");
  const agencyName = ag?.agencyName ?? (agency === GLOBAL_AGENCY ? globalAgencyName() : mine.find((a) => a.id === agency)?.name ?? agency);
  const canWrite = isGlobal || access == null || mine.some((a) => a.id === agency);

  if (agency === GLOBAL_AGENCY) {
    return (
      <Card title="Vault">
        <div role="note" style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
          {agencyName}'s Vault is the installation's, used by every agency without a Vault of its own.{" "}
          {isGlobal ? (
            <a href="/settings?tab=vault" style={{ color: c.accent }}>
              Settings → Vault (Installation)
            </a>
          ) : (
            "A global administrator configures it."
          )}
          {" "}To connect an agency's own Vault, switch to that agency.
        </div>
      </Card>
    );
  }
  if (!agency) {
    return (
      <Card title="Vault">
        <div role="note" style={{ fontSize: c.fontSm, color: c.textSec }}>Switch to an agency to see its Vault.</div>
      </Card>
    );
  }
  if (!canWrite) {
    return (
      <Card title="Vault">
        <div role="note" style={{ fontSize: c.fontSm, color: c.textSec }}>
          Only {agencyName}'s administrators can see and change its Vault connection.
        </div>
      </Card>
    );
  }
  return <AgencyVaultForm key={agency} agency={agency} agencyName={agencyName} />;
}

function AgencyVaultForm({ agency, agencyName }: { agency: string; agencyName: string }) {
  const path = { params: { path: { agencyId: agency } } };
  const [loaded, setLoaded] = useState<{ vault: AgencyVault | null } | null>(null);
  const [loadErr, setLoadErr] = useState("");
  const [addr, setAddr] = useState("");
  const [namespace, setNamespace] = useState("");
  const [roleId, setRoleId] = useState("");
  const [secretId, setSecretId] = useState("");
  // undefined keeps the stored bundle; "" clears it.
  const [caPem, setCaPem] = useState<string | undefined>(undefined);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [refs, setRefs] = useState<VaultRef[] | null>(null);
  const [unresolved, setUnresolved] = useState<VaultRef[] | null>(null);
  const [disconnecting, setDisconnecting] = useState(false);
  const [bump, setBump] = useState(0);

  useEffect(() => {
    let cancelled = false;
    api.GET("/agencies/{agencyId}/vault", path).then(({ data, error }) => {
      if (cancelled) return;
      if (error || !data) {
        setLoadErr(errMsg(error));
        return;
      }
      const v = data.vault ?? null;
      setLoaded({ vault: v });
      setAddr(v?.addr ?? "");
      setNamespace(v?.namespace ?? "");
      setRoleId("");
      setSecretId("");
      setCaPem(undefined);
    });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agency, bump]);

  if (loadErr) return <Card title="Vault"><div style={{ color: c.danger }}>Error: {loadErr}</div></Card>;
  if (!loaded) return <InlineLoading style={{ padding: 16 }} />;
  const v = loaded.vault;

  const body = (confirmUnresolved = false) => ({
    addr,
    namespace,
    roleId,
    secretId,
    ...(caPem !== undefined ? { caPem } : {}),
    ...(confirmUnresolved ? { confirmUnresolved: true } : {}),
  });

  async function test() {
    setBusy(true);
    setMsg(null);
    const { data, error } = await api.POST("/agencies/{agencyId}/vault/test", { ...path, params: { ...path.params, header: csrfHeader }, body: body() });
    setBusy(false);
    if (error) setMsg({ ok: false, text: errMsg(error) });
    else setMsg(data?.ok ? { ok: true, text: "The Vault accepted the login." } : { ok: false, text: `The login failed: ${data?.problem ?? "no answer"}` });
  }

  async function preview() {
    setBusy(true);
    setMsg(null);
    const { data, error } = await api.POST("/agencies/{agencyId}/vault/preview", { ...path, params: { ...path.params, header: csrfHeader }, body: body() });
    setBusy(false);
    if (error) setMsg({ ok: false, text: errMsg(error) });
    else setRefs(data?.references ?? []);
  }

  async function save(confirm: boolean) {
    setBusy(true);
    setMsg(null);
    const { error, response } = await api.PUT("/agencies/{agencyId}/vault", { ...path, params: { ...path.params, header: csrfHeader }, body: body(confirm) });
    setBusy(false);
    if (response?.status === 409) {
      setUnresolved(((error as { references?: VaultRef[] } | undefined)?.references) ?? []);
      return;
    }
    setUnresolved(null);
    if (error) {
      setMsg({ ok: false, text: errMsg(error) });
      return;
    }
    setMsg({ ok: true, text: "Saved. The agency's Vault-backed secrets and SSH keys now resolve in this Vault." });
    setRefs(null);
    setBump((b) => b + 1);
  }

  async function disconnect() {
    setBusy(true);
    const { error } = await api.DELETE("/agencies/{agencyId}/vault", { ...path, params: { ...path.params, header: csrfHeader } });
    setBusy(false);
    setDisconnecting(false);
    if (error) setMsg({ ok: false, text: errMsg(error) });
    else {
      setMsg({ ok: true, text: "Disconnected. The agency uses the installation's Vault." });
      setBump((b) => b + 1);
    }
  }

  const wide: React.CSSProperties = { ...inputStyle(), width: 320 };
  const missing = !addr.trim() || (!v && (!roleId.trim() || !secretId.trim()));
  const missingWhy = missing ? "Give the address, and the role id and secret id for a first connection" : undefined;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
        {v ? (
          <>
            <strong style={{ color: c.text }}>{agencyName}</strong> has a Vault of its own. Its Vault-backed secrets and SSH
            keys are read there, and only there: if it cannot be reached they fail, and are never read from the
            installation's Vault instead.
          </>
        ) : (
          <>
            <strong style={{ color: c.text }}>{agencyName}</strong> uses the installation's Vault, inside the paths a global
            administrator assigned it. Connecting a Vault of its own moves every one of its Vault references there at once.
          </>
        )}
      </div>
      <Card
        title={v ? "Its own Vault" : "Connect a Vault of its own"}
        action={
          <div style={{ display: "flex", gap: 8 }}>
            <Btn small onClick={test} disabled={busy || missing} title={missingWhy}>Test</Btn>
            <Btn small onClick={preview} disabled={busy || missing} title={missingWhy}>Preview references</Btn>
            <Btn small primary onClick={() => save(false)} disabled={busy || missing} title={missingWhy}>Save</Btn>
          </div>
        }
      >
        <ReadOnlyFields readOnly={busy}>
          <div style={{ fontSize: c.fontSm }}>
            <SettingRow label="Vault address" hint="https only, on the installation's allowed Vault hosts">
              <input aria-label="Vault address" value={addr} onChange={(e) => setAddr(e.target.value)} placeholder="https://vault.agency.internal:8200" style={wide} />
            </SettingRow>
            <SettingRow label="Namespace" hint="Vault Enterprise namespace (optional)">
              <input aria-label="Namespace" value={namespace} onChange={(e) => setNamespace(e.target.value)} style={wide} />
            </SettingRow>
            <SettingRow label="AppRole role id" hint={v ? "Never shown. Leave blank to keep the stored one." : "Never shown after saving."}>
              <input aria-label="Role id" type="password" value={roleId} onChange={(e) => setRoleId(e.target.value)} placeholder={v?.roleIdSet ? "•••• (set)" : ""} style={wide} />
            </SettingRow>
            <SettingRow label="AppRole secret id" hint={v ? "Never shown. Leave blank to keep the stored one." : "Never shown after saving."}>
              <input aria-label="Secret id" type="password" value={secretId} onChange={(e) => setSecretId(e.target.value)} placeholder={v?.secretIdSet ? "•••• (set)" : ""} style={wide} />
            </SettingRow>
            <SettingRow
              label="CA bundle"
              hint={
                caPem === undefined
                  ? v?.caPresent
                    ? "A bundle is stored. Paste one to replace it."
                    : "PEM, for a Vault whose certificate the server does not already trust (optional)"
                  : caPem === ""
                    ? "The stored bundle is removed on save."
                    : "Replaces the stored bundle on save."
              }
              last
            >
              <div style={{ display: "flex", flexDirection: "column", gap: 6, alignItems: "flex-end" }}>
                <textarea aria-label="CA bundle" value={caPem ?? ""} onChange={(e) => setCaPem(e.target.value)} rows={3} style={{ ...wide, fontFamily: c.mono, fontSize: c.fontXs }} />
                {v?.caPresent && caPem === undefined && (
                  <Btn small onClick={() => setCaPem("")}>Remove the stored bundle</Btn>
                )}
              </div>
            </SettingRow>
          </div>
        </ReadOnlyFields>
        {msg && (
          <div role={msg.ok ? "status" : "alert"} style={{ marginTop: 10, fontSize: c.fontSm, color: msg.ok ? c.success : c.danger }}>
            {msg.text}
          </div>
        )}
        {v && (
          <div style={{ marginTop: 10, fontSize: c.fontXs, color: c.textMuted }}>
            Last changed by {v.lastModifiedBy} at {v.lastModifiedAt}.
          </div>
        )}
      </Card>
      {refs && <RefsCard title="Its Vault references in this Vault" refs={refs} />}
      {v && <OwnPrefixesCard agency={agency} agencyName={agencyName} />}
      {v && (
        <Card title="Disconnect">
          <div style={{ display: "flex", alignItems: "center", gap: 12, fontSize: c.fontSm, color: c.textSec }}>
            <span style={{ flex: 1 }}>
              Refused while {agencyName} owns any Vault-backed secret or SSH key: they would all move to the installation's
              Vault at once. Move them out of Vault, or delete them, first.
            </span>
            <Btn dangerQuiet onClick={() => setDisconnecting(true)} disabled={busy}>Disconnect…</Btn>
          </div>
        </Card>
      )}
      {unresolved && (
        <ConfirmDialog
          title="Some references would not resolve"
          message={
            <div>
              <p style={{ marginTop: 0 }}>
                These of {agencyName}'s Vault-backed secrets and SSH keys could not be read in this Vault. Saved, they fail
                until their paths exist there.
              </p>
              <RefsTable refs={unresolved} />
            </div>
          }
          confirmLabel="Save anyway"
          busy={busy}
          onConfirm={() => save(true)}
          onCancel={() => setUnresolved(null)}
        />
      )}
      {disconnecting && (
        <ConfirmDialog
          title={`Disconnect ${agencyName}'s Vault?`}
          message={`${agencyName} goes back to the installation's Vault, inside the paths a global administrator assigns it. The Vault path prefixes it set for its own Vault are cleared: until a global administrator assigns some, it names no Vault path.`}
          confirmLabel="Disconnect"
          busy={busy}
          onConfirm={disconnect}
          onCancel={() => setDisconnecting(false)}
        />
      )}
    </div>
  );
}

// With a Vault of its own an agency's prefixes are optional and its own
// (SG-26): none means any path in its Vault.
function OwnPrefixesCard({ agency, agencyName }: { agency: string; agencyName: string }) {
  const path = { params: { path: { agencyId: agency } } };
  const [text, setText] = useState<string | null>(null);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  useEffect(() => {
    api.GET("/agencies/{agencyId}/vault-prefixes", path).then(({ data }) => setText((data?.prefixes ?? []).join("\n")));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agency]);
  async function save() {
    const prefixes = (text ?? "").split("\n").map((p) => p.trim()).filter(Boolean);
    const { data, error } = await api.PUT("/agencies/{agencyId}/vault-prefixes", {
      ...path,
      params: { ...path.params, header: csrfHeader },
      body: { prefixes },
    });
    if (error) setMsg({ ok: false, text: errMsg(error) });
    else {
      setText((data?.prefixes ?? []).join("\n"));
      setMsg({ ok: true, text: "Saved." });
    }
  }
  return (
    <Card title="Path prefixes" action={<Btn small onClick={save} disabled={text == null}>Save prefixes</Btn>}>
      <div style={{ fontSize: c.fontSm, color: c.textSec, marginBottom: 8, lineHeight: 1.6 }}>
        Optional. With none, {agencyName}'s secrets and keys may name any path in its own Vault; with some, only paths inside
        them. One per line; a prefix holds whole path segments (<code style={{ fontFamily: c.mono }}>secret/data/tax</code> does
        not hold <code style={{ fontFamily: c.mono }}>secret/data/tax-audit</code>).
      </div>
      <textarea aria-label="Path prefixes" value={text ?? ""} onChange={(e) => setText(e.target.value)} rows={3} style={{ ...inputStyle(), width: 320, fontFamily: c.mono }} />
      {msg && (
        <div role={msg.ok ? "status" : "alert"} style={{ fontSize: c.fontSm, color: msg.ok ? c.success : c.danger }}>
          {msg.text}
        </div>
      )}
    </Card>
  );
}

function RefsCard({ title, refs }: { title: string; refs: VaultRef[] }) {
  return (
    <Card title={title}>
      {refs.length === 0 ? (
        <div style={{ fontSize: c.fontSm, color: c.textSec }}>The agency has no Vault-backed secrets or SSH keys.</div>
      ) : (
        <RefsTable refs={refs} />
      )}
    </Card>
  );
}

function RefsTable({ refs }: { refs: VaultRef[] }) {
  return (
    <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm }}>
      <thead>
        <tr>
          <th style={thStyle()}>Kind</th>
          <th style={thStyle()}>Name</th>
          <th style={thStyle()}>Path</th>
          <th style={thStyle()}>Resolves</th>
        </tr>
      </thead>
      <tbody>
        {refs.map((r) => (
          <tr key={r.kind + r.id}>
            <td style={tdStyle()}>{r.kind === "key" ? "SSH key" : "Secret"}</td>
            <td style={tdStyle()}>{r.name}</td>
            <td style={{ ...tdStyle(), fontFamily: c.mono }}>{r.path}</td>
            <td style={{ ...tdStyle(), color: r.resolves ? c.success : c.danger }}>
              {r.resolves ? "Yes" : `No: ${r.problem ?? "not readable"}`}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

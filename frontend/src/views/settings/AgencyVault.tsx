import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
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

// Settings → Secret store (2.5.1 SG3 as "Agency Vault"; any kind of store since
// 2.5.5 SM5: HashiCorp Vault / OpenBao or AWS Secrets Manager). An agency may connect a
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
      <Card title="Secret store">
        <div role="note" style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
          {agencyName}'s Vault is the installation's, used by every agency without a Vault of its own.{" "}
          {isGlobal ? (
            <a href="/settings?tab=vault" style={{ color: c.accent }}>
              Settings → Installation secret store
            </a>
          ) : (
            "A global administrator configures it."
          )}
          {" "}To connect an agency's own secret store, switch to that agency.
        </div>
      </Card>
    );
  }
  if (!agency) {
    return (
      <Card title="Secret store">
        <div role="note" style={{ fontSize: c.fontSm, color: c.textSec }}>Switch to an agency to see its secret store.</div>
      </Card>
    );
  }
  if (!canWrite) {
    return (
      <Card title="Secret store">
        <div role="note" style={{ fontSize: c.fontSm, color: c.textSec }}>
          Only {agencyName}'s administrators can see and change its secret store connection.
        </div>
      </Card>
    );
  }
  return <AgencyVaultForm key={agency} agency={agency} agencyName={agencyName} />;
}

type Provider = "hashicorp" | "aws";
const PROVIDERS: { id: Provider; label: string }[] = [
  { id: "hashicorp", label: "HashiCorp Vault / OpenBao" },
  { id: "aws", label: "AWS Secrets Manager" },
];

function AgencyVaultForm({ agency, agencyName }: { agency: string; agencyName: string }) {
  const path = { params: { path: { agencyId: agency } } };
  const [loaded, setLoaded] = useState<{ vault: AgencyVault | null; externalRows: number } | null>(null);
  const [loadErr, setLoadErr] = useState("");
  const [provider, setProvider] = useState<Provider>("hashicorp");
  // HashiCorp Vault / OpenBao.
  const [addr, setAddr] = useState("");
  const [namespace, setNamespace] = useState("");
  const [roleId, setRoleId] = useState("");
  const [secretId, setSecretId] = useState("");
  // AWS Secrets Manager (2.5.5 SM-4).
  const [region, setRegion] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const [accessKeyId, setAccessKeyId] = useState("");
  const [secretAccessKey, setSecretAccessKey] = useState("");
  const [sessionToken, setSessionToken] = useState("");
  const [exampleRef, setExampleRef] = useState("");
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
      setLoaded({ vault: v, externalRows: data.externalRows ?? 0 });
      setProvider(v?.provider === "aws" ? "aws" : "hashicorp");
      setAddr(v?.addr ?? "");
      setNamespace(v?.namespace ?? "");
      setRegion(v?.settings?.region ?? "");
      setEndpoint(v?.settings?.endpoint ?? "");
      setRoleId("");
      setSecretId("");
      setAccessKeyId("");
      setSecretAccessKey("");
      setSessionToken("");
      setCaPem(undefined);
    });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agency, bump]);

  if (loadErr) return <Card title="Secret store"><div style={{ color: c.danger }}>Error: {loadErr}</div></Card>;
  if (!loaded) return <InlineLoading style={{ padding: 16 }} />;
  const v = loaded.vault;
  const stored = v ? (v.provider === "aws" ? "aws" : "hashicorp") : null;
  // The same kind of store keeps its stored credential when a field is blank;
  // another kind starts afresh.
  const sameKind = stored === provider;
  // SM-9: the kind of store does not change while rows name it.
  const kindLockedWhy =
    v && loaded.externalRows > 0
      ? `${loaded.externalRows} of ${agencyName}'s external secrets and SSH keys use this store. Change them to stored values, or delete them, before choosing another kind.`
      : "";
  const label = PROVIDERS.find((p) => p.id === provider)!.label;

  const body = (confirmUnresolved = false) =>
    provider === "aws"
      ? {
          provider,
          settings: { region: region.trim(), endpoint: endpoint.trim(), ...(caPem !== undefined ? { caPem } : {}) },
          credential: { accessKeyId, secretAccessKey, sessionToken },
          ...(exampleRef.trim() ? { exampleRef: exampleRef.trim() } : {}),
          ...(confirmUnresolved ? { confirmUnresolved: true } : {}),
        }
      : {
          provider,
          addr,
          namespace,
          roleId,
          secretId,
          ...(caPem !== undefined ? { caPem } : {}),
          ...(confirmUnresolved ? { confirmUnresolved: true } : {}),
        };

  async function test() {
    setBusy(true);
    setMsg(null);
    const { data, error } = await api.POST("/agencies/{agencyId}/vault/test", { ...path, params: { ...path.params, header: csrfHeader }, body: body() });
    setBusy(false);
    if (error) setMsg({ ok: false, text: errMsg(error) });
    else
      setMsg(
        data?.ok
          ? { ok: true, text: provider === "aws" ? "AWS accepted the credential." : "The Vault accepted the login." }
          : { ok: false, text: `The test failed: ${data?.problem ?? "no answer"}` },
      );
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
    const e = error as { code?: string; references?: VaultRef[] } | undefined;
    if (response?.status === 409 && e?.code === "references_unresolved") {
      setUnresolved(e.references ?? []);
      return;
    }
    setUnresolved(null);
    if (error) {
      setMsg({ ok: false, text: errMsg(error) });
      return;
    }
    setMsg({ ok: true, text: `Saved. ${agencyName}'s external secrets and SSH keys now resolve in this store.` });
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
  const keepHint = (set: boolean | undefined) =>
    sameKind && set ? "Never shown. Leave blank to keep the stored one." : "Never shown after saving.";
  const setMark = (set: boolean | undefined) => (sameKind && set ? "•••• (set)" : "");
  const missingWhy =
    provider === "aws"
      ? !region.trim()
        ? "Give the region"
        : !sameKind && (!accessKeyId.trim() || !secretAccessKey.trim())
          ? "Give the access key id and secret access key"
          : undefined
      : !addr.trim() || (!sameKind && (!roleId.trim() || !secretId.trim()))
        ? "Give the address, and the role id and secret id for a first connection"
        : undefined;
  const missing = !!missingWhy;
  const caField = (
    <SettingRow
      label="CA bundle"
      hint={
        caPem === undefined
          ? sameKind && v?.caPresent
            ? "A bundle is stored. Paste one to replace it."
            : provider === "aws"
              ? "PEM, for an endpoint of your own whose certificate the server does not already trust (optional)"
              : "PEM, for a Vault whose certificate the server does not already trust (optional)"
          : caPem === ""
            ? "The stored bundle is removed on save."
            : "Replaces the stored bundle on save."
      }
      last
    >
      <div style={{ display: "flex", flexDirection: "column", gap: 6, alignItems: "flex-end" }}>
        <textarea aria-label="CA bundle" value={caPem ?? ""} onChange={(e) => setCaPem(e.target.value)} rows={3} style={{ ...wide, fontFamily: c.mono, fontSize: c.fontXs }} />
        {sameKind && v?.caPresent && caPem === undefined && <Btn small onClick={() => setCaPem("")}>Remove the stored bundle</Btn>}
      </div>
    </SettingRow>
  );

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
        {v ? (
          <>
            <strong style={{ color: c.text }}>{agencyName}</strong> has a secret store of its own ({v.providerLabel || label}).
            Its external secrets and SSH keys are read there, and only there: if it cannot be reached they fail, and are
            never read from the installation's Vault instead.
          </>
        ) : (
          <>
            <strong style={{ color: c.text }}>{agencyName}</strong> uses the installation's Vault, inside the paths a global
            administrator assigned it. Connecting a secret store of its own moves every one of its external references there
            at once.
          </>
        )}
      </div>
      <Card
        title={v ? "Its own secret store" : "Connect a secret store of its own"}
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
            <SettingRow label="Kind of store" hint={kindLockedWhy || "Where this agency's external secrets and SSH keys are read"}>
              <select
                aria-label="Kind of store"
                value={provider}
                disabled={!!kindLockedWhy}
                title={kindLockedWhy || undefined}
                onChange={(e) => {
                  setProvider(e.target.value as Provider);
                  setCaPem(undefined);
                  setMsg(null);
                  setRefs(null);
                }}
                style={{ ...wide, cursor: kindLockedWhy ? "not-allowed" : "pointer", opacity: kindLockedWhy ? 0.5 : 1 }}
              >
                {PROVIDERS.map((p) => (
                  <option key={p.id} value={p.id}>{p.label}</option>
                ))}
              </select>
            </SettingRow>
            {kindLockedWhy && (
              <div role="note" style={{ fontSize: c.fontXs, color: c.textMuted, margin: "0 0 8px" }}>
                <Link to="/env-vars?tab=secrets" style={{ color: c.accent }}>Open Env Vars → Secrets</Link>
              </div>
            )}
            {provider === "aws" ? (
              <>
                <SettingRow label="Region" hint="The region the agency's secrets are in, such as us-east-1">
                  <input aria-label="Region" value={region} onChange={(e) => setRegion(e.target.value)} placeholder="us-east-1" style={wide} />
                </SettingRow>
                <SettingRow label="Endpoint" hint="Optional: a VPC or FIPS endpoint (https, on the installation's allowed hosts). Blank: the region's own.">
                  <input aria-label="Endpoint" value={endpoint} onChange={(e) => setEndpoint(e.target.value)} placeholder="https://vpce-….secretsmanager.us-east-1.vpce.amazonaws.com" style={wide} />
                </SettingRow>
                <SettingRow label="Access key id" hint={keepHint(v?.credentialSet?.accessKeyId)}>
                  <input aria-label="Access key id" type="password" value={accessKeyId} onChange={(e) => setAccessKeyId(e.target.value)} placeholder={setMark(v?.credentialSet?.accessKeyId)} style={wide} />
                </SettingRow>
                <SettingRow label="Secret access key" hint={keepHint(v?.credentialSet?.secretAccessKey)}>
                  <input aria-label="Secret access key" type="password" value={secretAccessKey} onChange={(e) => setSecretAccessKey(e.target.value)} placeholder={setMark(v?.credentialSet?.secretAccessKey)} style={wide} />
                </SettingRow>
                <SettingRow label="Session token" hint="Optional, for temporary credentials">
                  <input aria-label="Session token" type="password" value={sessionToken} onChange={(e) => setSessionToken(e.target.value)} style={wide} />
                </SettingRow>
                <SettingRow
                  label="Example reference"
                  hint="AWS has no login to test: the test and the save read this secret (or one the agency already names) and discard it"
                >
                  <input aria-label="Example reference" value={exampleRef} onChange={(e) => setExampleRef(e.target.value)} placeholder="myapp/db#password" style={{ ...wide, fontFamily: c.mono }} />
                </SettingRow>
                {endpoint.trim() && caField}
                <div role="note" style={{ fontSize: c.fontXs, color: c.textMuted, marginTop: 8, lineHeight: 1.5 }}>
                  Cronomicon uses only the credential given here: never the server's own AWS identity, instance role or
                  metadata service. Give it read access (secretsmanager:GetSecretValue) to the agency's secrets alone.
                </div>
              </>
            ) : (
              <>
                <SettingRow label="Vault address" hint="https only, on the installation's allowed Vault hosts">
                  <input aria-label="Vault address" value={addr} onChange={(e) => setAddr(e.target.value)} placeholder="https://vault.agency.internal:8200" style={wide} />
                </SettingRow>
                <SettingRow label="Namespace" hint="Vault Enterprise namespace (optional)">
                  <input aria-label="Namespace" value={namespace} onChange={(e) => setNamespace(e.target.value)} style={wide} />
                </SettingRow>
                <SettingRow label="AppRole role id" hint={keepHint(v?.roleIdSet)}>
                  <input aria-label="Role id" type="password" value={roleId} onChange={(e) => setRoleId(e.target.value)} placeholder={setMark(v?.roleIdSet)} style={wide} />
                </SettingRow>
                <SettingRow label="AppRole secret id" hint={keepHint(v?.secretIdSet)}>
                  <input aria-label="Secret id" type="password" value={secretId} onChange={(e) => setSecretId(e.target.value)} placeholder={setMark(v?.secretIdSet)} style={wide} />
                </SettingRow>
                {caField}
              </>
            )}
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
      {refs && <RefsCard title="Its external references in this store" refs={refs} />}
      {v && <OwnPrefixesCard agency={agency} agencyName={agencyName} />}
      {v && (
        <Card title="Disconnect">
          <div style={{ display: "flex", alignItems: "center", gap: 12, fontSize: c.fontSm, color: c.textSec }}>
            <span style={{ flex: 1 }}>
              Refused while {agencyName} owns any external secret or SSH key: they would all move to the installation's
              Vault at once. Change them to stored values, or delete them, first.
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
                These of {agencyName}'s external secrets and SSH keys could not be read in this store. Saved, they fail
                until they exist there.
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
          title={`Disconnect ${agencyName}'s secret store?`}
          message={`${agencyName} goes back to the installation's Vault, inside the paths a global administrator assigns it. The prefixes it set for its own store are cleared: until a global administrator assigns some, it names no reference.`}
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
        Optional. With none, {agencyName}'s secrets and keys may name anything in its own store; with some, only
        references inside them. One per line; a prefix holds whole segments (<code style={{ fontFamily: c.mono }}>tax</code> does
        not hold <code style={{ fontFamily: c.mono }}>tax-audit</code>): a Vault path, or the start of an AWS secret name.
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
        <div style={{ fontSize: c.fontSm, color: c.textSec }}>The agency has no external secrets or SSH keys.</div>
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

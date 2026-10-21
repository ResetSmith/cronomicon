import { useState } from "react";
import { Link } from "react-router-dom";
import { api, errMsg } from "../../api/client";
import { useAgencyOptional } from "../../agency";
import type { components } from "../../api/schema";
import { useGet } from "../../hooks";
import { c } from "../../theme";
import { Btn, InlineLoading, SectionLabel } from "../../components/ui";
import { detailBoxStyle, fmtDate } from "./ui";

type RowUsage = components["schemas"]["RowUsage"];

// A stored secret's or an SSH key's Usage (2.5.1 SG2.4): what refers to it,
// which runs used it, and what was done to it and by whom. The references are
// the ones the dispatch resolver would give THIS row (a job of another agency
// binding the same name is that agency's row's, not this one's). Someone
// outside the agency is marked, global administrators included, because that
// is what an agency reviews its secrets for.

const VIA: Record<string, string> = {
  binding: "binds it",
  become: "become password",
  connectAs: "connects as it",
  host: "host record",
  bastion: "bastion",
  override: "per-run reference",
  connect_as: "connect-as",
  host_auth: "host connection",
  bastion_auth: "bastion connection",
};

const RELATION: Record<string, string | undefined> = {
  global_admin: "Global administrator",
  outside: "Outside the agency",
};

// The agency in use's secrets report (2.5.1 SG2.5, SG-15): every stored secret
// and SSH key it owns, with each Usage in one CSV row, for the agency to hand
// to its own auditors. No values. Hidden on the overview and outside the
// agency switcher (a test, the login screen): it is one agency's.
export function SecretsReportButton() {
  const ctx = useAgencyOptional();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  if (!ctx || ctx.onOverview || !ctx.agency) return null;
  const agency = ctx.agency;
  const download = async () => {
    setBusy(true);
    setErr(null);
    const { data, error } = await api.GET("/agencies/{agencyId}/secrets-report", {
      params: { path: { agencyId: agency }, query: { format: "csv" } },
      parseAs: "blob",
    });
    setBusy(false);
    if (error || !data) {
      setErr(error ? errMsg(error) : "the report could not be made");
      return;
    }
    const url = URL.createObjectURL(data as unknown as Blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `secrets-report-${agency}.csv`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  };
  return (
    <Btn onClick={() => void download()} disabled={busy} title={err ?? `Every secret and SSH key of ${ctx.agencyName}: references, uses, reveals and changes by anyone outside it. No values.`}>
      {busy ? "Working…" : "Download report"}
    </Btn>
  );
}

export function RowUsagePanel({ kind, id }: { kind: "secret" | "key"; id: string }) {
  const q = useGet<RowUsage>(
    () =>
      kind === "secret"
        ? api.GET("/env-secrets/{secretId}/usage", { params: { path: { secretId: id } } })
        : api.GET("/ssh/credentials/{credentialId}/usage", { params: { path: { credentialId: id } } }),
    [kind, id],
  );
  if (q.loading && !q.data) return <InlineLoading what="usage" />;
  if (q.error || !q.data) return <div style={{ color: c.danger, fontSize: c.fontSm }}>Could not read the usage: {q.error ?? "no data"}</div>;
  // A partial answer (an older server, a test's mock) shows as empty lists.
  const u = {
    references: q.data.references ?? [],
    recent: q.data.recent ?? [],
    changes: q.data.changes ?? [],
    uses: q.data.uses ?? { days7: 0, days30: 0, days90: 0 },
  };
  const muted = { color: c.textMuted, fontSize: c.fontSm } as const;

  return (
    <div style={{ marginTop: 12 }} aria-label="Usage">
      <SectionLabel>Usage</SectionLabel>
      <div style={{ ...detailBoxStyle(), display: "grid", gap: 14 }}>
        <div>
          <div style={{ fontWeight: 600, fontSize: c.fontSm, marginBottom: 4 }}>Referred to by</div>
          {u.references.length === 0 ? (
            <div style={muted}>Nothing refers to it.</div>
          ) : (
            <ul style={{ margin: 0, paddingLeft: 18, fontSize: c.fontSm }}>
              {u.references.map((r) => (
                <li key={`${r.via}:${r.ownerKind}:${r.ownerId}`}>
                  {r.ownerKind === "job" ? <Link to={`/runs?jobUid=${encodeURIComponent(r.ownerId)}`} title="Its runs in History">{r.name}</Link> : r.name}{" "}
                  <span style={{ color: c.textMuted }}>
                    ({r.ownerKind}
                    {r.scope ? `, scope ${r.scope}` : ""}, {VIA[r.via] ?? r.via}
                    {r.as ? `, as ${r.as}` : ""})
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>

        <div>
          <div style={{ fontWeight: 600, fontSize: c.fontSm, marginBottom: 4 }}>
            Used by runs{" "}
            <span style={{ ...muted, fontWeight: 400, fontFamily: c.mono, fontVariantNumeric: "tabular-nums" }}>
              {u.uses.days7} in 7 days · {u.uses.days30} in 30 · {u.uses.days90} in 90
            </span>
          </div>
          {u.recent.length === 0 ? (
            <div style={muted}>No run has used it in the recorded window.</div>
          ) : (
            <ul style={{ margin: 0, paddingLeft: 18, fontSize: c.fontSm }}>
              {u.recent.map((r) => (
                <li key={`${r.traceId}:${r.via}`}>
                  <span style={{ fontFamily: c.mono, fontVariantNumeric: "tabular-nums", color: c.textSec }}>{fmtDate(r.at)}</span>{" "}
                  <Link to={`/runs?trace=${encodeURIComponent(r.traceId)}`}>{r.jobName || r.traceId}</Link>
                  <span style={{ color: c.textMuted }}>
                    {" "}
                    ({VIA[r.via] ?? r.via}
                    {r.runnerName ? `, on ${r.runnerName}` : ""})
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>

        <div>
          <div style={{ fontWeight: 600, fontSize: c.fontSm, marginBottom: 4 }}>Changes</div>
          {u.changes.length === 0 ? (
            <div style={muted}>No change recorded.</div>
          ) : (
            <ul style={{ margin: 0, paddingLeft: 18, fontSize: c.fontSm }}>
              {u.changes.map((ch, i) => {
                const mark = ch.actorRelation ? RELATION[ch.actorRelation] : undefined;
                return (
                  <li key={`${ch.at}:${i}`}>
                    <span style={{ fontFamily: c.mono, fontVariantNumeric: "tabular-nums", color: c.textSec }}>{fmtDate(ch.at)}</span> {ch.action} by{" "}
                    {ch.actor}
                    {mark && (
                      <span
                        style={{
                          marginLeft: 6,
                          fontFamily: c.sansCond,
                          textTransform: "uppercase",
                          fontSize: c.fontXs,
                          color: c.warning,
                          border: `1px solid ${c.warning}`,
                          borderRadius: c.radiusChip,
                          padding: "0 4px",
                        }}
                      >
                        {mark}
                      </span>
                    )}
                    {ch.actingAgency ? <span style={{ color: c.textMuted }}> (acting in {ch.actingAgency})</span> : null}
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      </div>
    </div>
  );
}

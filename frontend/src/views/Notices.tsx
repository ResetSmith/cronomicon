import { useState } from "react";
import { Link } from "react-router-dom";
import { api, csrfHeader, errMsg } from "../api/client";
import { useGet } from "../hooks";
import { AlertBanner, Btn, InlineLoading } from "../components/ui";
import { fmtInAppZone } from "../utils/datetime";
import { c } from "../theme";
import type { components } from "../api/schema";

// Notices — one inbox for standing conditions (LR-85, v2.3.0).
//
// A notice is something the installation can be in and the application does not
// create: what an upgrade left for a person to decide, or damage. Each belongs
// to an agency and is shown to whoever administers that agency (Global's to a
// global administrator, who therefore sees them all).
//
// A notice RESOLVES ITSELF when its cause is gone; the checks run when this
// page is opened. Dismissing only hides one and records who did: it is for a
// condition that has been read and is being left as it is.

type Notice = components["schemas"]["Notice"];

/** What a kind is called, and where its remedy is made. Unknown kinds show their detail under a neutral title. */
const KINDS: Record<string, { title: string; to?: string; toLabel?: string }> = {
  agency_renamed: { title: "An agency was renamed by the upgrade", to: "/scopes?tab=agencies", toLabel: "Agencies" },
  shared_ownership: { title: "Shared by several agencies", to: "/scopes?tab=agencies", toLabel: "Agencies" },
  orphaned: { title: "In no agency", to: "/scopes?tab=agencies", toLabel: "Agencies" },
  retired_runner_pin: { title: "A runner-tag pin that could not be converted", to: "/scopes", toLabel: "Scopes" },
  scope_several_agencies: { title: "A scope in several agencies", to: "/scopes", toLabel: "Scopes" },
  target_host_outside_scope: { title: "A job's target host is not in its scope", to: "/jobs", toLabel: "Jobs" },
  record_key_outside_owner: { title: "A host record or bastion names a key its owner cannot use", to: "/settings?tab=targets", toLabel: "SSH Targets" },
  vault_path_outside_prefix: { title: "A Vault path outside its agency's prefixes", to: "/scopes?tab=agencies", toLabel: "Agencies" },
  legacy_placement: { title: "A runner that serves agencies it is not owned by", to: "/runners", toLabel: "Runners" },
  may_run_on_agent: { title: "Shell jobs that ran from the server may now run on an agent", to: "/scopes", toLabel: "Scopes" },
  mixed_scope: { title: "Shell jobs that ran in two places may now run in either", to: "/scopes", toLabel: "Scopes" },
  may_run_on_server: { title: "Shell jobs that ran on agents may now run from the server", to: "/scopes", toLabel: "Scopes" },
  agency_placed: { title: "The upgrade made the local runner serve an agency", to: "/settings?tab=localrunner", toLabel: "Local runner" },
  shell_job_requires: { title: "A shell job requires what no agent has", to: "/jobs", toLabel: "Jobs" },
  no_runner_for_shell_jobs: { title: "An agency's shell jobs have no runner", to: "/runners", toLabel: "Runners" },
};

export function Notices() {
  const [dep, setDep] = useState(0);
  const q = useGet<Notice[]>(() => api.GET("/notices"), [dep]);
  const [busy, setBusy] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const list = Array.isArray(q.data) ? q.data : [];

  const dismiss = async (n: Notice) => {
    setBusy(n.id);
    setErr(null);
    const { error } = await api.POST("/notices/dismiss", { params: { header: csrfHeader }, body: { ids: [n.id] } });
    setBusy(null);
    if (error) setErr(errMsg(error));
    else setDep((d) => d + 1);
  };

  // Grouped by agency, Global first: its notices are about the installation.
  const groups = new Map<string, { name: string; items: Notice[] }>();
  for (const n of list) {
    const g = groups.get(n.agencyId) ?? { name: n.agencyName || n.agencyId, items: [] };
    g.items.push(n);
    groups.set(n.agencyId, g);
  }
  const ordered = [...groups].sort(([a, ga], [b, gb]) => (a === "global" ? -1 : b === "global" ? 1 : ga.name.localeCompare(gb.name)));

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16, maxWidth: 980 }}>
      <div style={{ display: "flex", alignItems: "flex-start", gap: 12 }}>
        <p style={{ margin: 0, flex: 1, fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
          Conditions that need a person: what an upgrade left to decide, and anything found in a state the application does not
          create. A notice goes away by itself when its cause is put right. Dismissing hides one that has been read and is
          being left as it is, and records who dismissed it.
        </p>
        <Btn small onClick={() => setDep((d) => d + 1)} disabled={q.loading}>
          Refresh
        </Btn>
      </div>
      {err && <AlertBanner type="danger">{err}</AlertBanner>}
      {q.loading && <InlineLoading what="notices" />}
      {q.error && <AlertBanner type="danger">Could not load notices: {q.error}</AlertBanner>}
      {!q.loading && !q.error && list.length === 0 && (
        <div style={{ background: c.panel, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, padding: 20, color: c.textSec, fontSize: c.fontSm }}>
          Nothing needs attention. Notices appear here for the agencies you administer.
        </div>
      )}
      {ordered.map(([agencyId, g]) => (
        <section key={agencyId} aria-label={`Notices for ${g.name}`} style={{ background: c.panel, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, overflow: "hidden" }}>
          <div style={{ padding: "10px 16px", borderBottom: `1px solid ${c.border}`, fontFamily: c.sansCond, textTransform: "uppercase", letterSpacing: 0.6, fontSize: c.fontXs, color: c.textSec }}>
            {g.name} · {g.items.length}
          </div>
          {g.items.map((n, i) => {
            const k = KINDS[n.kind];
            return (
              <div key={n.id} style={{ padding: "12px 16px", borderTop: i === 0 ? "none" : `1px solid ${c.borderLight}`, display: "flex", gap: 14, alignItems: "flex-start" }}>
                <div style={{ flex: 1, minWidth: 0 }}>
                  <div style={{ fontSize: c.fontBody, fontWeight: 600, color: c.text }}>{k?.title ?? "Notice"}</div>
                  <div style={{ fontSize: c.fontSm, color: c.text, lineHeight: 1.6, marginTop: 4, overflowWrap: "anywhere" }}>{n.detail}</div>
                  <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 6, display: "flex", gap: 12, flexWrap: "wrap" }}>
                    <span>First seen {n.firstSeenAt ? fmtInAppZone(n.firstSeenAt) : "—"}</span>
                    {k?.to && (
                      <Link to={k.to} style={{ color: c.primary }}>
                        Open {k.toLabel}
                      </Link>
                    )}
                  </div>
                </div>
                <Btn
                  small
                  onClick={() => dismiss(n)}
                  disabled={busy != null}
                  ariaLabel={`Dismiss: ${k?.title ?? "notice"}, ${g.name}`}
                  title="Hide this notice and record that you did. It does not fix the condition, and it comes back if the condition does."
                >
                  {busy === n.id ? "Dismissing…" : "Dismiss"}
                </Btn>
              </div>
            );
          })}
        </section>
      ))}
    </div>
  );
}

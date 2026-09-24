import { useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api, fetchCapabilities } from "../api/client";
import { useGet, rows } from "../hooks";
import { TabBar } from "./envvars/ui";
import { ScopesTab, type ScopeRow } from "./scopes/ScopesTab";
import { AgenciesTab } from "./scopes/AgenciesTab";

// Scopes — the WHERE primitive (hosts / execution environments). Promoted from a
// tab on Env Vars to its own top-level page (SC1); the Agencies catalog moved
// here with it (SC2). This mirrors the parent half of the old EnvVars host: it
// owns the GET /scopes fetch + the ConfigureApp capability and renders the
// Scopes and Agencies tabs.
// Tab slugs for ?tab= deep links (mirrors the Schedules surface). VU-14 needs them:
// two empty states elsewhere point at the Agencies catalog, and a link that lands on
// the Scopes tab and leaves the operator to find the right one is not an action.
//
// "membership" is not a tab any more (RB-22) but stays mapped: the matrix's job
// moved INTO the Agencies tab (each agency expands into an editable member list),
// so an old deep link should land there rather than silently falling to Scopes.
const SLUGS = ["scopes", "agencies"];

export function Scopes() {
  const [params, setParams] = useSearchParams();
  const [tab, setTab] = useState(() => {
    const slug = params.get("tab") ?? SLUGS[0];
    if (slug === "membership") return 1; // RB-22: the matrix's successor lives on Agencies
    return Math.max(0, SLUGS.indexOf(slug));
  });
  const select = (i: number) => {
    setTab(i);
    const next = new URLSearchParams(params);
    if (i === 0) next.delete("tab");
    else next.set("tab", SLUGS[i]);
    setParams(next, { replace: true });
  };
  // Scope + agency writes require ConfigureApp (matching the server requirePerm
  // gates). Mutation controls are hidden when the caller lacks the permission.
  const [canConfig, setCanConfig] = useState(false);
  useEffect(() => {
    fetchCapabilities().then((caps) => setCanConfig(caps.configureApp));
  }, []);
  // ScopesTab is a controlled child: it renders the rows we fetch and bumps the
  // dep via refetch() after a mutation. AgenciesTab self-fetches and needs none.
  const [scopesDep, setScopesDep] = useState(0);
  const { data, error, loading } = useGet<unknown>(() => api.GET("/scopes"), [scopesDep]);
  const scopes = rows<ScopeRow>(data);

  return (
    <div>
      <TabBar tabs={["Scopes", "Agencies"]} active={tab} onChange={select} />
      {tab === 0 && <ScopesTab scopes={scopes} loading={loading} error={error} refetch={() => setScopesDep((n) => n + 1)} canEdit={canConfig} />}
      {/* RB-22 — the Membership matrix tab is gone. It was rows × 24 agency
          columns with a non-sticky name cell: scrolled right, you were ticking
          anonymous checkboxes — the same horizontal wall as the deleted Scope
          Restrictions grid, with far more rows. Its two jobs moved to where the
          question is actually asked: "what is in Tax?" is the Agencies tab (each
          row expands into an editable member list), and "which agencies hold this
          secret?" is an Agencies column on the entity's own catalog row. */}
      {tab === 1 && <AgenciesTab canEdit={canConfig} />}
    </div>
  );
}

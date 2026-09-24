import { useEffect, useState } from "react";
import { api, fetchCapabilities } from "../api/client";
import { useGet, rows } from "../hooks";
import { TabBar } from "./envvars/ui";
import { VariablesTab } from "./envvars/VariablesTab";
import { SecretsTab } from "./envvars/SecretsTab";
import { SshKeysTab } from "./envvars/SshKeysTab";

// Env Vars — variables + secrets + SSH keys. Scope management (and the Agencies
// catalog) moved to the top-level Scopes page (SC1/SC2); this page is now the
// name/value tabs plus the first-class SSH key store.
export function EnvVars() {
  const [tab, setTab] = useState(0);
  // PP-B1: variables + secrets writes require ManageEnvVars; SSH key credentials
  // are ConfigureApp-gated (SK-D6), matching the server requirePerm gates.
  // Mutation controls are hidden when the caller lacks the permission instead of
  // rendering a 403 click.
  const [canManageEnv, setCanManageEnv] = useState(false);
  const [canConfigure, setCanConfigure] = useState(false);
  useEffect(() => {
    fetchCapabilities().then((caps) => {
      setCanManageEnv(caps.manageEnvVars);
      setCanConfigure(caps.configureApp);
    });
  }, []);
  // Variables and Secrets both filter by scope, so we still fetch the scope
  // names here for their selects even though scope management itself now lives
  // on the Scopes page (SC3 regression-guard).
  const { data } = useGet<unknown>(() => api.GET("/scopes"), []);
  const scopes = rows<{ scope: string }>(data);
  const scopeNames = ["All", ...Array.from(new Set(scopes.map((s) => s.scope))).sort()];

  return (
    <div>
      <TabBar tabs={["Variables", "Secrets", "SSH Keys"]} active={tab} onChange={setTab} />
      {tab === 0 && <VariablesTab scopeNames={scopeNames} canEdit={canManageEnv} />}
      {tab === 1 && <SecretsTab scopeNames={scopeNames} canEdit={canManageEnv} />}
      {tab === 2 && <SshKeysTab canEdit={canConfigure} />}
    </div>
  );
}

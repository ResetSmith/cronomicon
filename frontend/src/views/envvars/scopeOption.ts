import { useAgencyOptional } from "../../agency";
import { ALL_SCOPE_OPTION } from "./ui";

// The Variables/Secrets form's "All" choice makes a global row: every scope of
// the row's agency, and no other agency's (2.4.0). The bare word read as "every
// agency", so the option names the agency: the row's owner when editing, the
// agency in use when creating (a create is the agency in use's, 2.4.4).
export function useScopeOptionLabel(ownerAgency: string | undefined): (option: string) => string {
  const ctx = useAgencyOptional();
  const agencyName = ownerAgency || (ctx && !ctx.onOverview ? ctx.agencyName : "");
  return (option) => {
    if (option !== ALL_SCOPE_OPTION) return option;
    return agencyName ? `All scopes in ${agencyName} (global)` : "All scopes (global)";
  };
}

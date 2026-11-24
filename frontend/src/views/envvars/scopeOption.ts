import { useAgencyOptional } from "../../agency";
import { ALL_SCOPE_OPTION } from "./ui";

// A row with no scope reaches every scope of its own agency and no other
// agency's (2.4.0). The API calls it global; the screens say "agency-wide"
// (2.5.4), because "global" read as "every agency" and, on an installation
// that kept the name, as the built-in agency Global.
export function agencyWideLabel(agencyName: string | undefined | null): string {
  return agencyName ? `Agency-wide (every scope in ${agencyName})` : "Agency-wide (every scope)";
}

// useAgencyWide is what the Variables/Secrets form says about its "All"
// choice, which makes an agency-wide row: the agency (the row's owner when
// editing, the agency in use when creating, since a create is the agency in
// use's (2.4.4), and outside the switcher the agency picked on the form), the
// option's label, and the hint shown under the picker while it is chosen.
export function useAgencyWide(ownerAgency: string | undefined, pickedAgency?: string) {
  const ctx = useAgencyOptional();
  const agencyName = ownerAgency || (ctx ? (ctx.onOverview ? "" : ctx.agencyName) : pickedAgency || "");
  return {
    agencyName,
    optionLabel: (option: string) => (option === ALL_SCOPE_OPTION ? agencyWideLabel(agencyName) : option),
    hint: (noun: string) =>
      `Every run in ${agencyName || "the agency"} that references this ${noun} receives it, whatever its scope, unless a row in the run's own scope has the same key. Pick a scope to narrow it.`,
  };
}

/** The name of the agency a create form's picker has selected, if any. */
export function pickedAgencyName(pick: { agencies: { id: string; name: string }[]; selected: string }): string {
  return pick.agencies.find((a) => a.id === pick.selected)?.name ?? "";
}

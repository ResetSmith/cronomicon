// The built-in agency's name (2.5.2 GR). Its id is fixed ("global"); its name
// is "Administration" on an installation made with 2.5.3 or later, "Central"
// on one made with 2.5.2, "Global" on an older one, until a global
// administrator renames it. Rows report owners by
// NAME, so a comparison with the literal "Global" stops matching the day it is
// renamed, and a label that says "Global" says the wrong thing.
//
// A module-level holder, set by AgencyProvider on every render (as it sets the
// request agency) from GET /me/context, so plain functions and components read
// the same value without a hook; it has no imports, so anything may import it.
// "Global" until it is set (a test, an older server).

let _name = "Global";

/** Set by AgencyProvider; not for anything else. */
export function setGlobalAgencyName(name: string | null | undefined): void {
  _name = name && name.trim() ? name : "Global";
}

/** The built-in agency's name now. */
export function globalAgencyName(): string {
  return _name;
}

/**
 * 2.5.3 CA-12: the built-in agency runs nothing, so where it is listed beside
 * the agencies it reads as the administration: "Global · administration". A
 * name that already says so (a new installation's "Administration") stands
 * alone.
 */
export function administrationLabel(name: string): string {
  return /administration/i.test(name) ? name : `${name} · administration`;
}

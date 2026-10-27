import { allReadable, api, csrfHeader, errMsg } from "./client";

/**
 * grantAgencyAdmins gives an AD group the admin role on one agency, through the
 * ordinary grant route (2.5.3 CA-16, question 210: the first agency's
 * administrators). Sent naming no agency (allReadable): the route takes a grant
 * for the agency the request works in, and a new agency is not that yet. The
 * refusal's message, or null.
 */
export async function grantAgencyAdmins(agencyId: string, adGroup: string): Promise<string | null> {
  const { error } = await api.POST("/access-grants", {
    params: { header: csrfHeader },
    headers: allReadable,
    body: { adGroup: adGroup.trim(), role: "admin", agencyId } as never,
  });
  return error ? errMsg(error) : null;
}

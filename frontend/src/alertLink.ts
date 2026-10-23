import { useEffect } from "react";
import { api, csrfHeader, fetchCapabilities } from "./api/client";

// The address alerts link to (2.5.2 OP2.1). The server cannot know the address
// people open the app at; the browser can, as the GitLab webhook URL
// (settings/Repositories) and the runner install commands already rely on.
// When a global administrator opens the app, the page's address is offered to
// the server, which keeps it only when no address is stored yet: the first
// visit decides, a later one through another name (a tunnel, an internal
// address) changes nothing, and Settings → Notifications shows it and lets a
// global administrator change it.

/** The address this page was opened at: its origin, under the app's base path. */
export function appAddress(): string {
  const base = (import.meta.env.BASE_URL ?? "/").replace(/\/+$/, "");
  return window.location.origin + base;
}

let offered = false;

/** Offer the page's address once per load, for a global administrator only. */
export function useOfferAlertLinkAddress(): void {
  useEffect(() => {
    if (offered) return;
    offered = true;
    void fetchCapabilities().then((caps) => {
      if (!caps.configureAppGlobal) return;
      // Fire and forget: the server records it or keeps what it has, and a
      // failure only means no link until the next visit.
      void api.POST("/settings/alert-link-address/detect", { params: { header: csrfHeader }, body: { address: appAddress() } });
    });
  }, []);
}

/** For tests: offer again on the next mount. */
export function resetAlertLinkOffer(): void {
  offered = false;
}

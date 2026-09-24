import { createContext, useEffect, useState, type ReactNode } from "react";
import { api } from "./api/client";
import { appZone, setAppZone } from "./utils/datetime";

// TimezoneProvider resolves the application timezone once at startup and installs
// it for the shared formatters (utils/datetime). Mounted inside the authenticated
// tree (GET /settings/general is session-gated), so the fetch runs with a session.
// The browser zone is the pre-fetch fallback, so timestamps never break while the
// request is in flight (timezone-update §5.1, §7).
interface TimezoneCtx {
  // appTimezone is the IANA name the app schedules + displays in, or undefined
  // until resolved (consumers should fall back to the browser zone).
  appTimezone: string | undefined;
}

const Ctx = createContext<TimezoneCtx>({ appTimezone: undefined });

export function TimezoneProvider({ children }: { children: ReactNode }) {
  const [tz, setTz] = useState<string | undefined>(appZone());

  useEffect(() => {
    let cancelled = false;
    api.GET("/settings/general").then(({ data }) => {
      if (cancelled || !data?.appTimezone) return;
      setAppZone(data.appTimezone);
      setTz(data.appTimezone);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  return <Ctx.Provider value={{ appTimezone: tz }}>{children}</Ctx.Provider>;
}

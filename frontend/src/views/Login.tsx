import { useEffect, useState } from "react";
import { c } from "../theme";
import { Card } from "../components/ui";
import { login, devLogin, fetchProviders, type Providers } from "../api/client";
// The shared brand lockup (LG-Q2): one emblem asset plus the wordmark as DOM
// text. This screen used to run the same per-theme `src` switch the sidebar did,
// over the same two drifting assets — so a user toggling the theme here watched
// the mark change size AND tagline. There is one of each now.
import { Wordmark } from "../components/Wordmark";

export function Login() {
  const [providers, setProviders] = useState<Providers>({ mode: "oidc", oidc: true, dev: false, logoutUrl: "" });

  useEffect(() => {
    let cancelled = false;
    fetchProviders().then((p) => {
      if (!cancelled) setProviders(p);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  // In trusted-header mode the reverse proxy (forward-auth against your identity provider) authenticates
  // before the SPA ever loads, so an unauthenticated user never reaches this
  // screen in production — the in-app OIDC button would be dead. Show an
  // informational state instead (and keep the dev bypass when enabled).
  const headerMode = providers.mode === "trusted-header";

  return (
    <div style={{ height: "100vh", display: "grid", placeItems: "center", background: c.bg, color: c.text, fontFamily: c.sans, padding: 16 }}>
      <Card style={{ width: "100%", maxWidth: 400, padding: 32 }}>
        <div style={{ textAlign: "center" }}>
          <div style={{ margin: "0 auto 22px" }}>
            <Wordmark tone="page" emblemWidth={110} />
          </div>
          {headerMode ? (
            <div style={{ color: c.textSec, marginBottom: providers.dev ? 4 : 0 }}>
              This deployment authenticates through your identity provider. If you're
              seeing this page, your session may have expired — reload to sign in again.
            </div>
          ) : (
            <>
              <div style={{ color: c.textSec, marginBottom: 24 }}>Sign in with your organization account.</div>
              <button
                onClick={login}
                disabled={!providers.oidc}
                title={providers.oidc ? "" : "Single sign-on is not configured yet"}
                style={{
                  display: "block",
                  width: "100%",
                  background: providers.oidc ? c.primary : c.panel2,
                  color: providers.oidc ? c.onSolid : c.textMuted,
                  border: 0,
                  borderRadius: c.radiusChip,
                  padding: "10px 20px",
                  fontSize: c.fontBody,
                  fontWeight: 600,
                  cursor: providers.oidc ? "pointer" : "not-allowed",
                }}
              >
                Sign in with SSO
              </button>
            </>
          )}

          {providers.dev && (
            <>
              <div style={{ display: "flex", alignItems: "center", gap: 10, margin: "18px 0", color: c.textSec, fontSize: c.fontXs, fontFamily: c.sansCond, letterSpacing: 0.7 }}>
                <div style={{ flex: 1, height: 1, background: c.border }} />
                DEV ONLY
                <div style={{ flex: 1, height: 1, background: c.border }} />
              </div>
              <button
                onClick={devLogin}
                style={{
                  display: "block",
                  width: "100%",
                  background: "transparent",
                  color: c.text,
                  border: `1px dashed ${c.warning ?? c.border}`,
                  borderRadius: c.radiusChip,
                  padding: "10px 20px",
                  fontSize: c.fontBody,
                  fontWeight: 600,
                  cursor: "pointer",
                }}
              >
                Developer login (bypass SSO)
              </button>
              <div style={{ color: c.textSec, marginTop: 10, fontSize: c.fontXs }}>
                Mints a local admin session for previewing the UI. Not available in production.
              </div>
            </>
          )}
        </div>
      </Card>
    </div>
  );
}

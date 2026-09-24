import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
// publishManuals lives in a plain-JS sibling (with a .d.ts) rather than inline
// here so its node:fs/url/path usage stays out of the type-checked graph — the
// frontend has no @types/node on purpose (a browser-only src), and adding it
// would leak Node globals into src (e.g. flip bare setTimeout to NodeJS.Timeout).
import { publishManuals } from "./vite-manuals-plugin.js";

// The build emits into the backend's embedded asset dir (backend/web/dist),
// which the Go binary serves via go:embed (T2/T3). In dev, /api and the OIDC
// auth routes are proxied to the running backend on :8080.
export default defineConfig({
  plugins: [react(), publishManuals()],
  build: {
    outDir: "../backend/web/dist",
    emptyOutDir: true,
  },
  server: {
    // Least-privilege filesystem scope for the dev server: serve only the frontend
    // root, NOT the rest of the repo (Vite's default `fs.allow` is the git-root,
    // which would expose all of backend/). `deny` then blocks any env files and
    // secrets (e.g. amadeus.env, secrets/amadeus_kek) elsewhere in the repo. The
    // documentation/ manuals + runner guides are served by the publishManuals
    // plugin (which reads them with node:fs, bypassing this allow-list), so they
    // need no entry here.
    fs: {
      allow: ["."],
      deny: ["**/*.env", "**/secrets/**", "**/*.{crt,pem,key}", "**/.git/**"],
    },
    proxy: {
      "/api": "http://localhost:8080",
      "/healthz": "http://localhost:8080",
      "/readyz": "http://localhost:8080",
    },
  },
});

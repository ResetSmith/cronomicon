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
    // Vite 5's default target, written out. Vite 7's own default is a newer
    // baseline (Chrome 107, Safari 16, Firefox 104); an operator's locked-down
    // browser that loaded the app before the toolchain moved must still load it.
    // Raise this deliberately, in a release that says so.
    target: ["es2020", "edge88", "firefox78", "chrome87", "safari14"],
  },
  server: {
    // Least-privilege filesystem scope for the dev server: serve only the frontend
    // root, NOT the rest of the repo (Vite's default `fs.allow` is the git-root,
    // which would expose all of backend/). `deny` then blocks any env files and
    // secrets (e.g. cronomicon.env, secrets/cronomicon_kek) elsewhere in the repo. The
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

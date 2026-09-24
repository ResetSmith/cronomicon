import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { applyTheme, savedThemeMode, type ThemeMode } from "./theme";

interface ThemeCtx {
  mode: ThemeMode;
  toggle: () => void;
}

const Ctx = createContext<ThemeCtx>({ mode: "dark", toggle: () => {} });

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [mode, setMode] = useState<ThemeMode>(savedThemeMode);

  // Mutate the shared token object BEFORE children render so every `c.xxx`
  // read in this pass sees the active palette (applyTheme is idempotent).
  applyTheme(mode);

  useEffect(() => {
    applyTheme(mode); // re-assert post-commit (body background, persistence)
  }, [mode]);

  const toggle = () => setMode((m) => (m === "dark" ? "light" : "dark"));

  return <Ctx.Provider value={{ mode, toggle }}>{children}</Ctx.Provider>;
}

export const useTheme = () => useContext(Ctx);

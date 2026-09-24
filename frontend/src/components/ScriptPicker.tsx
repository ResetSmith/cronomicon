import { useEffect, useState, type CSSProperties } from "react";
import { api } from "../api/client";
import { c } from "../theme";
import type { components } from "../api/schema";
import { TypeBadge } from "./ui";
import { SearchSelect } from "./SearchSelect";
import { useAllScripts, scriptFolder, scriptSearchText } from "../utils/scripts";
import { useDebounced } from "../hooks";

type Script = components["schemas"]["Script"];

// ScriptPicker — the Composer's searchable, folder-grouped, select-only Script
// field (JC17–JC19), a thin specialization of the shared SearchSelect (JC24). It
// owns the catalog load (the two-mode useAllScripts: full list under the cap,
// debounced server ?q= past it) and resolves the current selection — including one
// that isn't on the loaded page (edit prefill / narrowing search / capped catalog)
// via GET /scripts/{name}, or marks it "missing" when the script was deleted (§2.5).
//
// `onResolved` lifts the resolved Script object to the Composer so its downstream
// derivation (runType → executor lock, lint warnings, declared-variable hints)
// reads the selection without a second fetch.
export function ScriptPicker({
  value,
  onChange,
  onResolved,
  onMissing,
  disabled,
  style,
}: {
  value: string;
  onChange: (next: string) => void;
  onResolved?: (script: Script | null) => void;
  /** Fires true when the current value resolved to a deleted/unknown script (404). */
  onMissing?: (missing: boolean) => void;
  disabled?: boolean;
  style?: CSSProperties;
}) {
  const [search, setSearch] = useState("");
  const debounced = useDebounced(search, 200);
  const { items, capped, loading } = useAllScripts(debounced, 0);

  // Resolve the Script for the current value. It stays "sticky" for a stable value —
  // we never null it just because the loaded `items` swapped to a different search —
  // so the closed-state label and the downstream selectedScript don't flicker while
  // the user is typing. We (re)resolve only when value changes to one not on the
  // loaded page (edit prefill / narrowing search / capped catalog) via GET
  // /scripts/{name}; a 404 marks it "missing" rather than blank (§2.5).
  const [resolved, setResolved] = useState<Script | null>(null);
  const [missing, setMissing] = useState(false);
  useEffect(() => {
    if (!value) {
      setResolved(null);
      setMissing(false);
      return;
    }
    if (resolved?.name === value) {
      setMissing(false);
      return; // already resolved this ref — keep it sticky across item swaps
    }
    const found = items.find((s) => s.name === value);
    if (found) {
      setResolved(found);
      setMissing(false);
      return;
    }
    let cancelled = false;
    setMissing(false);
    (async () => {
      const r = await api.GET("/scripts/{name}", { params: { path: { name: value } } });
      if (cancelled) return;
      if (r.error || !r.data) {
        setResolved(null);
        setMissing(true); // deleted / unresolvable ref → "missing" marker, not blank
      } else {
        setResolved(r.data as Script);
        setMissing(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [value, items, resolved]);

  const selected = resolved?.name === value ? resolved : null;

  // Lift the resolved selection to the parent (drives runType/executor, lint, vars).
  useEffect(() => {
    onResolved?.(selected);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selected]);

  // Surface the dangling-ref state so the Composer can soft-warn at save time (A3).
  useEffect(() => {
    onMissing?.(missing);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [missing]);

  return (
    <SearchSelect<Script>
      value={value}
      onChange={(next, item) => {
        // Capture the chosen Script synchronously so selecting never flashes a null
        // resolution before the effect re-resolves.
        if (item) setResolved(item);
        onChange(next);
      }}
      items={items}
      getKey={(s) => s.name ?? ""}
      getLabel={(s) => s.name ?? ""}
      getSearchText={scriptSearchText}
      groupBy={scriptFolder}
      renderOption={(s) => <ScriptOptionRow s={s} />}
      renderAdornment={(s) => <TypeBadge type={s.runType} />}
      selectedItem={selected}
      missing={missing}
      onSearch={setSearch}
      disableClientFilter={capped}
      loading={loading}
      disabled={disabled}
      placeholder="Search scripts…"
      ariaLabel="Script"
      emptyNote="No scripts match."
      style={style}
    />
  );
}

function ScriptOptionRow({ s }: { s: Script }) {
  const folder = scriptFolder(s);
  return (
    <>
      <TypeBadge type={s.runType} />
      <span style={{ flex: 1, minWidth: 0, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
        {s.name}
        {folder && <span style={{ color: c.textMuted, marginLeft: 8, fontSize: c.fontXs }}>{folder}</span>}
      </span>
    </>
  );
}

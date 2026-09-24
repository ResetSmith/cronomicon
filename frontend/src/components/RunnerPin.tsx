import { useMemo } from "react";
import { api } from "../api/client";
import { useGet } from "../hooks";
import { c } from "../theme";
import { inputStyle } from "./ui";

// RT-3 — the runner-pin controls (the runner-targeting plan).
//
// A pin says "only a runner carrying this tag may claim this run". The operator
// is thinking "which runner", so the control presents runners; what it STORES is
// a tag, because a runner id is a registration identity that dangles the moment
// a host is re-enrolled (RT-Q1). The online count is what makes that honest —
// picking "vlan-dmz (3 online)" reads as picking capacity, not as picking a
// label.
//
// TAG SOURCE. Derived client-side from GET /runners rather than a dedicated
// endpoint. RT-Q3 asks for the list to be agency-filtered, and this is NOT
// (see the note in the RT-3 section of the plan): /runners is RequireSession
// and already returns the whole fleet — names, agencies, tags, status — to any
// signed-in user, so a filtered tag endpoint beside it would narrow a view of
// data the caller can already fetch directly. Closing that properly means
// gating /runners itself, which is a change with its own blast radius (the
// Runners page reads it) and is not RT-3's to make.

export type RunnerTagInfo = { tag: string; online: number; total: number };

type RunnerLite = { status?: string | null; tags?: string[] | null };

// useRunnerTags returns the fleet's distinct tags with their online/total counts,
// sorted by usefulness: tags with an online runner first, then alphabetically.
// A tag nobody carries is legal (the runner may be enrolled tomorrow, RT-Q4) and
// simply is not in this list — the inputs below all accept free text for exactly
// that case.
export function useRunnerTags(): { tags: RunnerTagInfo[]; loading: boolean } {
  const q = useGet<RunnerLite[]>(() => api.GET("/runners"), []);
  const tags = useMemo(() => {
    const rows = Array.isArray(q.data) ? q.data : [];
    const byTag = new Map<string, RunnerTagInfo>();
    for (const r of rows) {
      for (const raw of r.tags ?? []) {
        const tag = (raw ?? "").trim();
        if (!tag) continue;
        // Case-insensitive keying mirrors the backend: runner_tags.tag is
        // COLLATE NOCASE (mig. 1080, RT-G9), so "DMZ" and "dmz" are ONE tag at
        // claim time and must be one entry here. First casing seen wins, the
        // same rule tagutil.Normalize uses.
        const key = tag.toLowerCase();
        const cur = byTag.get(key) ?? { tag, online: 0, total: 0 };
        cur.total += 1;
        if ((r.status ?? "") === "online") cur.online += 1;
        byTag.set(key, cur);
      }
    }
    return [...byTag.values()].sort(
      (a, b) => (b.online > 0 ? 1 : 0) - (a.online > 0 ? 1 : 0) || a.tag.localeCompare(b.tag),
    );
  }, [q.data]);
  return { tags, loading: q.loading };
}

// pinCountLabel describes a tag's capacity in the words an operator needs when
// deciding whether the pin will actually run. "0 online" is deliberately loud:
// it is legal, and it means the run will queue until something shows up.
export function pinCountLabel(tag: string, tags: RunnerTagInfo[]): string {
  const t = tags.find((x) => x.tag.toLowerCase() === tag.trim().toLowerCase());
  if (!t) return "no runner carries this tag yet";
  if (t.online === 0) return `0 online of ${t.total}`;
  return `${t.online} online${t.total > t.online ? ` of ${t.total}` : ""}`;
}

// RunnerPinInput is the shared editor: a free-text field backed by a datalist of
// the fleet's tags. Free text is required by RT-Q3 — a pin naming a runner that
// will be enrolled tomorrow is a legitimate thing to write — so this is not a
// <select>.
export function RunnerPinInput({
  value,
  onChange,
  tags,
  placeholder = "Any eligible runner",
  label = "Run on",
  id,
  disabled,
}: {
  value: string;
  onChange: (v: string) => void;
  tags: RunnerTagInfo[];
  placeholder?: string;
  /** Accessible name. The surrounding FormField/Field renders a bare <label>
      with no htmlFor, so without this the control has no name for a screen
      reader — the visible text sits next to it but is not associated with it. */
  label?: string;
  id: string;
  disabled?: boolean;
}) {
  const listId = `${id}-tags`;
  return (
    <div>
      <input
        list={listId}
        id={id}
        aria-label={label}
        value={value}
        disabled={disabled}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        style={{ ...inputStyle(), cursor: disabled ? "not-allowed" : "text", opacity: disabled ? 0.6 : 1 }}
      />
      <datalist id={listId}>
        {tags.map((t) => (
          <option key={t.tag} value={t.tag}>
            {t.online > 0 ? `${t.online} online` : `${t.total} runner${t.total === 1 ? "" : "s"}, none online`}
          </option>
        ))}
      </datalist>
      {value.trim() !== "" && (
        <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 4 }}>{pinCountLabel(value, tags)}</div>
      )}
    </div>
  );
}

// RunnerPinValue renders a job's pin as a value: the tag plus how much of the
// fleet actually carries it, or "Any eligible runner" when there is none.
//
// It used to carry an attribution clause too ("— operator override; the job
// declares vlan-dmz"), because the pin resolved through an operator layer that
// could mask the declared one and a winner shown without its reason reads as a
// bug. That layer was retired in v1.3.5, so the job-level pin has one source and
// nothing to attribute. A per-run pin is still shown in the Run dialog, where it
// is the operator's own in-flight choice rather than someone else's standing
// decision.
export function RunnerPinValue({ declared, tags }: { declared?: string | null; tags?: RunnerTagInfo[] }) {
  const effective = declared ?? "";
  return (
    <span>
      {effective ? (
        <strong style={{ fontFamily: c.mono }}>{effective}</strong>
      ) : (
        <span style={{ color: c.textSec }}>Any eligible runner</span>
      )}
      {effective && tags && (
        <span style={{ color: c.textSec, fontSize: c.fontXs, marginLeft: 6 }}>({pinCountLabel(effective, tags)})</span>
      )}
    </span>
  );
}

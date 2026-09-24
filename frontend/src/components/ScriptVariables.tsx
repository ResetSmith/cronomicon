import { c } from "../theme";
import type { components } from "../api/schema";

type Variable = components["schemas"]["ScriptVariable"];

// Run types the backend variable scan doesn't analyze yet (it returns [] for
// these, so an empty array alone can't distinguish "none" from "not analyzed").
const NOT_ANALYZED = new Set(["ansible", "terraform"]);

// isRequired — a variable is required when the body provides no fallback. A
// non-null default (including "") means the body supplies one via ${VAR:-x}, so
// the reference is optional. Mirrors the backend's Default-pointer semantics.
const isRequired = (v: Variable) => v.default == null;

// ScriptVariablesPanel renders the env variables a script references, so an
// operator can see what to define. It is the single rendering used by all three
// surfaces (Scripts detail, Run dialog, Job Composer); each passes a different
// `satisfied` set (keys already provided) and an optional `onAdd` to wire a
// missing variable into that surface's env editor.
export function ScriptVariablesPanel({
  variables,
  runType,
  satisfied,
  onAdd,
  addLabel = "add",
  title = "Variables this script uses",
  emptyNote = "No environment variables referenced.",
}: {
  variables?: Variable[] | null;
  runType?: string | null;
  satisfied?: ReadonlySet<string>;
  onAdd?: (name: string, def: string) => void;
  addLabel?: string;
  title?: string;
  emptyNote?: string;
}) {
  const vars = variables ?? [];
  const notAnalyzed = !!runType && NOT_ANALYZED.has(runType);
  const isSatisfied = (name: string) => satisfied?.has(name) ?? false;
  const missing = vars.filter((v) => !isSatisfied(v.name));

  return (
    <div>
      <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 6 }}>
        <span style={label()}>{title}</span>
        {!notAnalyzed && vars.length > 0 && <span style={{ fontSize: c.fontXs, color: c.textMuted }}>({vars.length})</span>}
        {onAdd && missing.length > 1 && (
          <button
            type="button"
            onClick={() => missing.forEach((v) => onAdd(v.name, v.default ?? ""))}
            style={addAllBtn()}
            title="Add every variable that isn't already provided"
          >
            + {addLabel} all missing ({missing.length})
          </button>
        )}
      </div>

      {notAnalyzed ? (
        <div style={note()}>Variable detection isn't available for {runType} scripts yet.</div>
      ) : vars.length === 0 ? (
        <div style={note()}>{emptyNote}</div>
      ) : (
        <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
          {vars.map((v) => (
            <VariableChip
              key={v.name}
              variable={v}
              satisfied={isSatisfied(v.name)}
              onAdd={onAdd}
              addLabel={addLabel}
            />
          ))}
        </div>
      )}
    </div>
  );
}

// VariableChip — one variable's status: ✓ provided (success), required & missing
// (warning), or optional with a default (neutral, shows the fallback). When onAdd
// is given and the variable isn't yet provided, the chip carries a "+ add" action.
function VariableChip({
  variable,
  satisfied,
  onAdd,
  addLabel,
}: {
  variable: Variable;
  satisfied: boolean;
  onAdd?: (name: string, def: string) => void;
  addLabel: string;
}) {
  const required = isRequired(variable);
  const def = variable.default;
  const tone = satisfied ? toneOk() : required ? toneNeed() : toneOpt();
  const glyph = satisfied ? "✓" : required ? "●" : "○";
  const titleParts = [
    satisfied ? "Already provided" : required ? "Required — define this variable" : "Optional",
    def != null ? `default: ${def === "" ? "(empty)" : def}` : null,
    variable.line ? `first referenced on line ${variable.line}` : null,
  ].filter(Boolean);

  return (
    <span
      title={titleParts.join(" · ")}
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 6,
        padding: "3px 8px",
        borderRadius: c.radiusChip,
        fontSize: c.fontXs,
        background: tone.bg,
        border: `1px solid ${tone.color}50`,
        color: tone.color,
        whiteSpace: "nowrap",
      }}
    >
      <span aria-hidden style={{ fontSize: c.fontXs }}>{glyph}</span>
      <span style={{ fontFamily: c.mono, fontWeight: 600 }}>{variable.name}</span>
      {def != null && (
        <span style={{ color: c.textMuted, fontFamily: c.mono, fontWeight: 400 }}>
          ={def === "" ? "∅" : def}
        </span>
      )}
      {onAdd && !satisfied && (
        <button
          type="button"
          onClick={() => onAdd(variable.name, def ?? "")}
          title={`${addLabel} ${variable.name}`}
          style={addChipBtn()}
        >
          + {addLabel}
        </button>
      )}
    </span>
  );
}

// These styles are functions, not module-level constants, so a theme toggle
// re-reads the active palette on every render. Capturing c.* tokens at module
// load freezes the load-time theme (default: dark) — e.g. toneOpt's c.panel2
// would stay dark navy in light mode. See theme.ts: "never capture token
// values in module-level constants."
const label = (): React.CSSProperties => ({
  fontSize: c.fontXs,
  fontFamily: c.sansCond,
  fontWeight: 600,
  color: c.textMuted,
  textTransform: "uppercase",
  letterSpacing: 0.7,
});

const note = (): React.CSSProperties => ({ fontSize: c.fontSm, color: c.textMuted });

const toneOk = () => ({ color: c.success, bg: c.successBg });
const toneNeed = () => ({ color: c.warning, bg: c.warningBg });
const toneOpt = () => ({ color: c.textSec, bg: c.panel2 });

const addChipBtn = (): React.CSSProperties => ({
  marginLeft: 2,
  padding: "0 4px",
  fontSize: c.fontXs,
  fontWeight: 600,
  lineHeight: 1.6,
  border: `1px solid ${c.border}`,
  borderRadius: c.radiusChip,
  background: c.panel,
  color: c.primary,
  cursor: "pointer",
  fontFamily: "inherit",
});

const addAllBtn = (): React.CSSProperties => ({
  padding: "2px 8px",
  fontSize: c.fontXs,
  fontWeight: 600,
  border: `1px solid ${c.border}`,
  borderRadius: c.radiusChip,
  background: c.panel,
  color: c.primary,
  cursor: "pointer",
  fontFamily: "inherit",
});

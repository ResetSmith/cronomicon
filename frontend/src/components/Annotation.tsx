import { useEffect, useState } from "react";
import { c } from "../theme";
import { Btn, Section, inputStyle } from "./ui";
import { fmtWhen } from "../views/scheduling/ui";

// AN-3 (the annotations plan) — the operator annotation surface,
// shared by the Jobs and Workflows catalogs.
//
// One module for both because the two catalogs kept diverging on exactly this
// kind of thing (the FX-D1 comment in the backend row struct lists three
// consecutive defects that were a change applied to one twin and not the
// other). A shared component makes the twins the same by construction rather
// than by review.
//
// What this renders is deliberately plain: notes are TEXT with significant
// newlines, not markdown, and URLs are not linkified (AN-Q2). The app ships no
// markdown renderer and no sanitizer, and an operator note is not worth opening
// that surface to save someone typing a bare URL.

export type Annotation = {
  critical?: boolean;
  contact?: string;
  notes?: string;
  notesBy?: string;
  notesAt?: string;
};

// AN-Q3 — mirrors of the server-side caps. The server is the enforcement (it
// answers 400 `invalid_annotation`); these only stop the typing earlier.
const MAX_NOTES = 4096;
const MAX_CONTACT = 256;

/** annotationOf extracts the annotation fields from a job or workflow row. */
// NotesBody renders the note itself: five lines, scrolling past that.
//
// pre-wrap, not a markdown render: newlines are the only formatting an operator
// gets, and they are the one that matters (AN-Q2).
//
// The cap exists because notes hold 4096 characters, and a long one pushed the
// whole expanded panel — Executor, Run on, tags, the run history — below the
// fold. It is expressed in `em`, so it stays five LINES if the font-size token
// changes: 1.5em is one line at this lineHeight. A px value would silently
// become 4.6 lines the first time somebody nudged the type scale.
//
// THE FADE. An overlay scrollbar is invisible at rest, and the cap lands
// exactly on a line boundary, so a 14-line note looked identical to a 5-line
// one — the content was reachable and nothing said so. The bottom line is
// faded out while there is more below.
//
// Done with mask-image rather than a gradient overlay in the panel's background
// colour, deliberately: a mask fades the TEXT to transparent whatever is behind
// it, so it is correct in both themes and stays correct if the surface it sits
// on ever changes. A background-coloured gradient would have to guess that
// colour and would be wrong the moment the guess drifted.
function NotesBody({ notes }: { notes: string }) {
  const [el, setEl] = useState<HTMLDivElement | null>(null);
  // Faded only while content is BOTH overflowing and not scrolled to the end —
  // a short note must not be faded, and neither must the last line once the
  // reader has reached it.
  const [faded, setFaded] = useState(false);
  useEffect(() => {
    if (!el) return;
    const sync = () => {
      // 1px slack: fractional line heights make scrollTop + clientHeight land
      // just short of scrollHeight at the bottom, which would leave the fade on
      // forever.
      const more = el.scrollHeight - el.clientHeight - el.scrollTop > 1;
      setFaded(more);
    };
    sync();
    el.addEventListener("scroll", sync, { passive: true });
    // The note can reflow without scrolling — a panel resize, or a save that
    // replaces the text — and the fade has to follow.
    //
    // Guarded rather than assumed: this is an enhancement to a cue, and a
    // missing ResizeObserver must degrade to "recomputed on mount and on
    // scroll", never take the whole annotation panel down with it. (jsdom has
    // no ResizeObserver, so unguarded it threw and blanked the section — which
    // is exactly the failure mode the guard is for, just found early.)
    const ro = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(sync);
    ro?.observe(el);
    return () => {
      el.removeEventListener("scroll", sync);
      ro?.disconnect();
    };
  }, [el, notes]);

  const mask = faded ? "linear-gradient(to bottom, #000 calc(100% - 1.5em), transparent)" : undefined;
  return (
    <div
      ref={setEl}
      style={{
        fontSize: c.fontSm,
        color: c.text,
        whiteSpace: "pre-wrap",
        lineHeight: 1.5,
        maxHeight: "7.5em",
        overflowY: "auto",
        maskImage: mask,
        WebkitMaskImage: mask,
      }}
    >
      {notes}
    </div>
  );
}

export function annotationOf(row: Annotation | undefined | null): Annotation {
  return {
    critical: row?.critical ?? false,
    contact: row?.contact ?? "",
    notes: row?.notes ?? "",
    notesBy: row?.notesBy ?? "",
    notesAt: row?.notesAt ?? "",
  };
}

/**
 * isAnnotated — whether anything was ever written.
 *
 * Attribution alone does NOT count: the server deletes the row on an all-empty
 * write, so notesBy/notesAt without content should not exist. Testing content
 * only means a stale row from a future bug degrades to "not annotated" instead
 * of rendering an empty section with a byline.
 */
function isAnnotated(a: Annotation): boolean {
  return !!a.critical || !!a.contact || !!a.notes;
}

/**
 * CriticalChip — the danger-tone marker used on catalog rows, in the detail
 * header and in the Run dialog banner.
 *
 * Rendered only when critical: absence is the common case and the signal is the
 * exception, so a "Not critical" chip on every row would train people to stop
 * seeing the one that matters.
 *
 * Tones are computed HERE, inside render, never hoisted to a module const — a
 * module-level style object freezes whichever theme was loaded first and stops
 * responding to the Light/Dark toggle.
 */
export function CriticalChip({ title }: { title?: string }) {
  return (
    <span
      title={title ?? "Marked critical by an operator"}
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 4,
        padding: "2px 8px",
        borderRadius: c.radiusChip,
        fontSize: c.fontXs,
        fontWeight: 700,
        letterSpacing: 0.3,
        color: c.danger,
        background: c.dangerBg,
        border: `1px solid ${c.danger}40`,
        whiteSpace: "nowrap",
        fontFamily: c.sans,
      }}
    >
      Critical
    </span>
  );
}

/**
 * AnnotationSection — the display/edit block at the top of an expanded row.
 *
 * It sits ABOVE the overview grid and the step graph rather than below the
 * tags, because it answers "what am I looking at and who owns it" — the
 * question you have before any of the fields underneath mean anything.
 *
 * Editing is open to any logged-in user (AN-Q1), matching the Tags section
 * further down the same panel, which renders its editor unconditionally. There
 * is deliberately no `canEdit` prop: the one in JobDetail means
 * `canCompose && source === "amadeus"`, so borrowing it would deny annotations
 * on every git-sourced job — precisely the population that needs them, since
 * git owns their description and this is their only operator-writable surface.
 */
export function AnnotationSection({
  value,
  onSave,
  error,
  kind,
}: {
  value: Annotation;
  onSave: (next: Annotation) => void;
  error?: string;
  kind: "job" | "workflow";
}) {
  const [editing, setEditing] = useState(false);
  const [critical, setCritical] = useState(!!value.critical);
  const [contact, setContact] = useState(value.contact ?? "");
  const [notes, setNotes] = useState(value.notes ?? "");

  // Re-seed the draft whenever the saved value changes underneath (a refetch, or
  // the optimistic override landing). Skipped while editing so a background
  // refresh cannot overwrite what someone is halfway through typing.
  useEffect(() => {
    if (editing) return;
    setCritical(!!value.critical);
    setContact(value.contact ?? "");
    setNotes(value.notes ?? "");
  }, [value.critical, value.contact, value.notes, editing]);

  const annotated = isAnnotated(value);

  const start = () => {
    setCritical(!!value.critical);
    setContact(value.contact ?? "");
    setNotes(value.notes ?? "");
    setEditing(true);
  };
  const commit = () => {
    setEditing(false);
    onSave({ critical, contact, notes });
  };

  const info =
    "Notes, criticality and a contact are stored in Cronomicon only — never written back to Git, and kept across syncs. " +
    "They are separate from the description, which Git owns and overwrites on every sync.";

  if (!editing && !annotated) {
    // Empty state: one quiet affordance, not a labelled empty box. Every logged-in
    // user can write, so there is no read-only branch that renders nothing.
    return (
      <Section title="Notes" info={info}>
        <button
          onClick={start}
          style={{
            background: "none",
            border: `1px dashed ${c.border}`,
            borderRadius: c.radiusChip,
            color: c.textMuted,
            fontSize: c.fontXs,
            fontFamily: c.sans,
            padding: "4px 10px",
            cursor: "pointer",
          }}
        >
          + Add a note
        </button>
        {error && <div style={{ fontSize: c.fontXs, color: c.danger, marginTop: 6 }}>{error}</div>}
      </Section>
    );
  }

  if (!editing) {
    return (
      <Section
        title="Notes"
        info={info}
        actions={
          <Btn small onClick={start}>
            Edit
          </Btn>
        }
      >
        {/* EP-1 (20260818-expanded-panels.md) — the read view sits on an opaque
            card. The expanded row behind it is a translucent primaryBg tint over
            the table, and free-text prose was the one thing in the panel that a
            weak background genuinely hurt. c.panel deliberately: it inverts
            against the tint in both themes, and c.text on c.panel is the app's
            primary body pair — already in the WCAG matrix, so no new contrast
            pair. The surface lives on THIS wrapper, never on NotesBody's scroll
            container — padding there would pull the mask fade's
            calc(100% - 1.5em) into the padding instead of over the last line.
            Border, not shadow (house rule: a surface gets one or the other).
            The empty state and the edit form stay card-less: one dashed button
            on a card is a box around nothing, and the form's inputs already
            carry their own panelInput surfaces. */}
        <div
          style={{
            display: "flex",
            flexDirection: "column",
            gap: 8,
            background: c.panel,
            border: `1px solid ${c.border}`,
            borderRadius: c.radiusSurface,
            padding: "10px 12px",
          }}
        >
          {value.critical && (
            <div>
              <CriticalChip title={`This ${kind} is marked critical`} />
            </div>
          )}
          {value.contact && (
            <div style={{ fontSize: c.fontSm, color: c.text }}>
              <span style={{ color: c.textMuted }}>Contact: </span>
              {value.contact}
            </div>
          )}
          {value.notes && <NotesBody notes={value.notes} />}
          {(value.notesBy || value.notesAt) && (
            // Attribution makes staleness visible: a note is last-writer-wins, so
            // "who and when" is what tells you whether to trust it.
            <div style={{ fontSize: c.fontXs, color: c.textMuted }}>
              Updated{value.notesBy ? ` by ${value.notesBy}` : ""}
              {value.notesAt ? `, ${fmtWhen(value.notesAt)}` : ""}
            </div>
          )}
          {error && <div style={{ fontSize: c.fontXs, color: c.danger }}>{error}</div>}
        </div>
      </Section>
    );
  }

  return (
    <Section title="Notes" info={info}>
      <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
        <label style={{ display: "inline-flex", alignItems: "center", gap: 8, fontSize: c.fontSm, color: c.text }}>
          <input type="checkbox" checked={critical} onChange={(e) => setCritical(e.target.checked)} />
          Critical
          <span style={{ fontSize: c.fontXs, color: c.textMuted }}>
            — shown as a chip and named in failure notifications. It does not change scheduling or who is paged.
          </span>
        </label>

        <label style={{ display: "flex", flexDirection: "column", gap: 4, fontSize: c.fontXs, color: c.textMuted }}>
          Contact
          <input
            value={contact}
            maxLength={MAX_CONTACT}
            placeholder="Who to contact when this breaks — a rota or distribution list is fine"
            onChange={(e) => setContact(e.target.value)}
            style={inputStyle()}
          />
        </label>

        <label style={{ display: "flex", flexDirection: "column", gap: 4, fontSize: c.fontXs, color: c.textMuted }}>
          Notes
          <textarea
            value={notes}
            maxLength={MAX_NOTES}
            rows={5}
            placeholder="What this is for, what to check before re-running…"
            onChange={(e) => setNotes(e.target.value)}
            style={{ ...inputStyle(), resize: "vertical", fontFamily: c.sans, lineHeight: 1.5 }}
          />
        </label>

        <div style={{ display: "flex", gap: 8 }}>
          <Btn small primary onClick={commit}>
            Save
          </Btn>
          <Btn small onClick={() => setEditing(false)}>
            Cancel
          </Btn>
        </div>
        {error && <div style={{ fontSize: c.fontXs, color: c.danger }}>{error}</div>}
      </div>
    </Section>
  );
}

/**
 * AnnotationBanner — the Run dialog's context strip.
 *
 * Shown when the job is critical or carries a contact. Deliberately NOT a
 * fourth reviewedSections gate (run-dialog-sections): the review gate exists
 * for inputs the operator must SUPPLY, and turning context into a visited-gate
 * is how you train people to click through gates.
 *
 * The notes are truncated to their first line — the dialog is for running the
 * job, not reading its history; the full note is one expand away in the catalog.
 */
export function AnnotationBanner({ value }: { value: Annotation }) {
  if (!value.critical && !value.contact) return null;
  const firstLine = (value.notes ?? "").split("\n").find((l) => l.trim() !== "") ?? "";
  const shown = firstLine.length > 140 ? `${firstLine.slice(0, 140)}…` : firstLine;
  return (
    <div
      // A note landmark: this is context ABOUT the job, not part of the run form
      // the dialog otherwise consists of, and a screen reader should be able to
      // tell those apart.
      role="note"
      aria-label="Operator annotation"
      style={{
        display: "flex",
        alignItems: "baseline",
        flexWrap: "wrap",
        gap: 10,
        padding: "8px 12px",
        marginBottom: 12,
        borderRadius: c.radiusChip,
        border: `1px solid ${value.critical ? `${c.danger}40` : c.border}`,
        background: value.critical ? c.dangerBg : c.panelHover,
        fontSize: c.fontSm,
        color: c.text,
      }}
    >
      {value.critical && <CriticalChip />}
      {value.contact && (
        <span>
          <span style={{ color: c.textMuted }}>Contact: </span>
          {value.contact}
        </span>
      )}
      {shown && <span style={{ color: c.textSec }}>{shown}</span>}
    </div>
  );
}

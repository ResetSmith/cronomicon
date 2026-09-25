// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { REACTION_NAME_RE, emptyReaction, reactionFromServer, reactionToWire, reactionsError } from "./ReactionsEditor";

// RX-15 — the editor's wire discipline and its client-side mirror of the
// server's refusals.

describe("reactionToWire — the preservedInline defence", () => {
  // The PUT is a WHOLESALE REPLACE, so a field this mapper forgets to send is a
  // field the save silently DELETES. The mapped return type makes the compiler
  // refuse to build if ReactionInput gains a field and this is not updated —
  // this test pins the runtime half of that guarantee.
  it("carries every authored field through the wire mapping", () => {
    const draft = {
      name: "after-extract",
      onKind: "workflow" as const,
      onName: "nightly",
      onSource: "git" as const,
      onOutcome: "stopped" as const,
      delaySeconds: 45,
      minIntervalSeconds: 900,
      includeWorkflowChildren: true,
      enabled: false,
    };
    expect(reactionToWire(draft)).toEqual(draft);
  });

  // A value the editor does not render must still survive an edit of one that
  // it does — the round trip is where fields go missing.
  it("round-trips a server row through fromServer → toWire unchanged", () => {
    const server = {
      ownerKind: "job" as const,
      ownerName: "load",
      ownerSource: "cronomicon" as const,
      name: "r",
      onKind: "job" as const,
      onName: "extract",
      onSource: "git" as const,
      onOutcome: "any" as const,
      delaySeconds: 30,
      minIntervalSeconds: 60,
      includeWorkflowChildren: true,
      enabled: false,
      position: 3,
      missing: false,
    };
    const wire = reactionToWire(reactionFromServer(server));
    expect(wire.onSource).toBe("git");
    expect(wire.onOutcome).toBe("any");
    expect(wire.delaySeconds).toBe(30);
    expect(wire.minIntervalSeconds).toBe(60);
    expect(wire.includeWorkflowChildren).toBe(true);
    expect(wire.enabled).toBe(false);
  });
});

describe("reactionsError — the client mirror of the server's 422s", () => {
  const owner = { kind: "job" as const, name: "load", source: "cronomicon" };
  const ok = { ...emptyReaction(1), onName: "extract", onSource: "cronomicon" as const };

  it("accepts a valid list", () => {
    expect(reactionsError(owner, [ok])).toBeNull();
  });

  it("rejects a name the YAML path would reject", () => {
    // The three slug rules — this one, the server's, and the YAML path's — must
    // agree, or "move this reaction into Git" fails on a name the app accepted.
    expect(REACTION_NAME_RE.test("after_extract-2")).toBe(true);
    expect(REACTION_NAME_RE.test("My Reaction!")).toBe(false);
    expect(reactionsError(owner, [{ ...ok, name: "My Reaction!" }])).toMatch(/slug/);
  });

  it("rejects self-reference", () => {
    expect(reactionsError(owner, [{ ...ok, onName: "load", onKind: "job" }])).toMatch(/cannot react to itself/);
  });

  it("rejects duplicate names and an unchosen target", () => {
    expect(reactionsError(owner, [ok, ok])).toMatch(/duplicate/);
    expect(reactionsError(owner, [{ ...ok, onName: "" }])).toMatch(/choose what this reacts to/);
  });

  it("rejects negative delays", () => {
    expect(reactionsError(owner, [{ ...ok, delaySeconds: -1 }])).toMatch(/cannot be negative/);
  });

  // Deliberately NOT mirrored: cycles are cross-plane (they need the whole
  // stored edge set, including Git-authored edges this form cannot see) and the
  // min-interval ceiling is a server setting. A client guess that disagreed with
  // the server would be worse than deferring to its 422.
  it("does not attempt cycle or retention validation", () => {
    expect(reactionsError(owner, [{ ...ok, minIntervalSeconds: 99999999 }])).toBeNull();
  });
});

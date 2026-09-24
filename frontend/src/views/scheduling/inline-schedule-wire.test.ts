// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { inlineScheduleToWire } from "../JobComposer";
import { preservedInlineToWire } from "../WorkflowEditor";

// CAL-23 — the pinned regression, written as a REFLECTION test rather than a
// per-field assertion.
//
// The defect it exists to stop has now happened twice. The Workflow Editor's
// preservedInline replay dropped the activation window on every save until the
// AW review caught it (a deferred start silently cleared by an unrelated edit),
// and the Job Composer's inline mapping was still dropping startAt/endAt/interval
// when CAL-13 went looking. Both were object literals that listed the fields
// someone remembered.
//
// So the guarantee is two-layered, and the compile-time half is the important
// one: both mappers declare a return type of `{[K in keyof Required<Wire>]: ...}`,
// so a field ADDED to the OpenAPI schema fails `tsc` until the mapper carries it.
// This test is the runtime half — it asserts the mapper emits every key it
// claims to and preserves each value, so a mapping that type-checks by writing
// `undefined` everywhere still fails here.

// Every field of the inline-schedule wire object, both surfaces. If the spec
// gains one, `tsc` fails on the mappers first; this list is what makes the
// failure legible rather than a type error nobody reads.
const WIRE_KEYS = ["name", "cron", "env", "startAt", "endAt", "interval", "skipCalendars", "onlyCalendars"].sort();

describe("inlineScheduleToWire (Job Composer)", () => {
  const entry = {
    name: "nightly",
    cron: "0 2 * * *",
    env: [{ key: "TIER", value: "prod" }],
    startAt: "2026-08-01T00:00:00Z",
    endAt: "2026-12-31T00:00:00Z",
    interval: null,
    skipCalendars: ["holidays"],
    onlyCalendars: [] as string[],
  };

  it("emits every field of the wire type", () => {
    expect(Object.keys(inlineScheduleToWire(entry)).sort()).toEqual(WIRE_KEYS);
  });

  it("preserves each field's value — the composer used to drop the window", () => {
    const wire = inlineScheduleToWire(entry);
    expect(wire).toMatchObject({
      name: "nightly",
      cron: "0 2 * * *",
      env: { TIER: "prod" },
      startAt: "2026-08-01T00:00:00Z",
      endAt: "2026-12-31T00:00:00Z",
      interval: null,
      skipCalendars: ["holidays"],
      onlyCalendars: [],
    });
  });

  it("sends an explicit empty/null for every unset field, so a full-replace PUT clears rather than keeps", () => {
    const wire = inlineScheduleToWire({ name: "adhoc", cron: "0 7 * * *", env: [] });
    expect(wire.startAt).toBeNull();
    expect(wire.endAt).toBeNull();
    expect(wire.interval).toBeNull();
    expect(wire.skipCalendars).toEqual([]);
    expect(wire.onlyCalendars).toEqual([]);
  });
});

describe("preservedInlineToWire (Workflow Editor)", () => {
  const entry = {
    name: "weekly",
    cron: "",
    env: { REGION: "us-east" },
    startAt: "2026-08-01T00:00:00Z",
    endAt: null,
    interval: "7d",
    skipCalendars: [] as string[],
    onlyCalendars: ["business-days"],
  };

  it("emits every field of the wire type", () => {
    expect(Object.keys(preservedInlineToWire(entry)).sort()).toEqual(WIRE_KEYS);
  });

  it("replays a preserved entry unchanged — this is what a workflow save must not lose", () => {
    expect(preservedInlineToWire(entry)).toEqual(entry);
  });

  it("carries the calendar bindings, which a workflow save would otherwise clear", () => {
    const wire = preservedInlineToWire({ name: "e", cron: "0 3 * * *", env: {}, onlyCalendars: ["business-days"] });
    expect(wire.onlyCalendars).toEqual(["business-days"]);
    expect(wire.skipCalendars).toEqual([]);
  });
});

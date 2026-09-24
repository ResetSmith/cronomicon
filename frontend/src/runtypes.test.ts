// RP-5 — the run-type vocabulary is authored once in runtypes.ts and consumed by
// every picker/filter (Jobs run dialog, Scopes capability editor, History type
// filter, Publish builder, Runner provisioner). This test pins it against the
// backend's OpenAPI RunType enum so a backend addition fails HERE, loudly,
// instead of silently missing from the UI's dropdowns.
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { RUN_TYPES, RUNNER_ONLY_TYPES, isRunnerOnly } from "./runtypes";

// The canonical spec copy (its root twin is byte-identical, guarded by the
// backend's openapi_mirror_test.go — either works, this one is the canon).
const SPEC_PATH = resolve(__dirname, "../../backend/openapi.yaml");

const specRunTypes = (): string[] => {
  const spec = readFileSync(SPEC_PATH, "utf8");
  // The enum is authored flow-style on one line:  RunType:\n  type: string\n  enum: [a, b, …]
  const m = spec.match(/RunType:\s*\n\s*type: string\s*\n\s*enum: \[([^\]]+)\]/);
  if (!m) throw new Error("RunType enum not found in backend/openapi.yaml — did its shape change?");
  return m[1].split(",").map((s) => s.trim()).filter(Boolean);
};

describe("run-type vocabulary (RP-5)", () => {
  it("matches the OpenAPI RunType enum exactly (as a set)", () => {
    expect([...RUN_TYPES].sort()).toEqual(specRunTypes().sort());
  });

  it("keeps the runner-only set a subset of the vocabulary", () => {
    for (const t of RUNNER_ONLY_TYPES) expect(RUN_TYPES).toContain(t);
  });

  it("isRunnerOnly agrees with the set for every known type", () => {
    for (const t of RUN_TYPES) expect(isRunnerOnly(t)).toBe(RUNNER_ONLY_TYPES.has(t));
  });
});

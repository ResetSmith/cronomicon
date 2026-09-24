// Run-type ↔ executor rules shared across the run path (Jobs RunDialog) and the
// authoring path (JobComposer). Kept in one module so the two surfaces never
// drift on which run-types are runner-only (JC6).

export type Executor = "ssh" | "runner";

// RP-5 — the closed run-type vocabulary (openapi.yaml RunType), in the ONE
// display order every dropdown/filter uses: the ssh-family shells first, then
// the runner-only local toolchains. Pinned against the OpenAPI enum by
// runtypes.test.ts so a backend addition fails the build loudly instead of
// silently missing from a picker.
export const RUN_TYPES = ["bash", "perl", "powershell", "python", "ansible", "terraform"] as const;
export type RunType = (typeof RUN_TYPES)[number];

// Run-types that can only execute on a runner agent (they need the local
// ansible/terraform toolchain); the in-app SSH executor cannot run them (R5.2).
// The backend resolveExecutor 422s ssh×these, so the UI treats it as a hard lock.
export const RUNNER_ONLY_TYPES = new Set<string>(["ansible", "terraform"]);

export const isRunnerOnly = (type?: string | null): boolean => !!type && RUNNER_ONLY_TYPES.has(type);

// RP-6 — run-types that can honor a "connect as" identity override, mirroring
// the server's execspec.IdentityCapableRunType exactly (the 422 boundary). The
// ssh-family types apply it to the in-app SSH client's targets; ansible applies
// it as connection extra-vars, which beat inventory-authored identity. terraform
// authenticates through its providers, so the fields could only no-op there —
// the surfaces hide them rather than offer a control the server refuses.
//
// An unknown type answers false, like the backend: identity support is granted
// by something having been taught to apply it, never assumed.
export const isIdentityCapable = (type?: string | null): boolean =>
  !!type && (!RUNNER_ONLY_TYPES.has(type) || type === "ansible") && (RUN_TYPES as readonly string[]).includes(type);

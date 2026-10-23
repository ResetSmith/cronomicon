// Run-type rules shared across the run path (Jobs RunDialog) and the authoring
// path (JobComposer). Kept in one module so the two surfaces never drift on
// which run types only an agent can run (JC6).

// RP-5 — the closed run-type vocabulary (openapi.yaml RunType), in the ONE
// display order every dropdown/filter uses: the shells first, then the
// agent-only local toolchains. Pinned against the OpenAPI enum by
// runtypes.test.ts so a backend addition fails the build loudly instead of
// silently missing from a picker.
export const RUN_TYPES = ["bash", "perl", "powershell", "python", "ansible", "terraform"] as const;
export type RunType = (typeof RUN_TYPES)[number];

// Run types only an agent can run: they need the local ansible/terraform
// toolchain, which the local runner (the server, over SSH) does not have
// (R5.2). The claim's capability rule enforces it; the UI only words it.
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

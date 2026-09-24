// The doc-link registry (TR-5 follow-up: contextual discoverability). Every
// in-app "Learn more" link to the bundled documentation goes through this map,
// for one reason: docLinks.test.ts walks it and asserts each target file exists
// in the built dist and each #anchor resolves to a real id there — so a renamed
// manual section cannot silently strand a link. Add links HERE, not inline.
export const DOC_LINKS = {
  /** The operator training course (Dashboard invitation, empty states). */
  operatorCourse: "/training-operator.html",
  /** User manual's Jobs chapter — targeting, blast radius, the Run dialog. */
  runTargeting: "/user-manual.html#jobs",
  /** Administrator manual §3 — grants, the seven permissions, delegation. */
  usersAccess: "/administrator-manual.html#ui-gating",
  /** Administrator manual §6 — agency membership & per-agency ownership. */
  agencies: "/administrator-manual.html#secrets",
  /** Administrator manual §4.4 — working-calendar semantics (veto-only). */
  calendars: "/administrator-manual.html#calendars",
  /** Per-run-type usage guides, linked from the Composer's Script field. */
  ansibleGuide: "/ansible-guide.html",
  bashGuide: "/bash-guide.html",
  powershellGuide: "/powershell-guide.html",
  pythonGuide: "/python-guide.html",
} as const;

/** The usage guide for a run type, or null for types without one. */
export function guideForRunType(runType: string | undefined): { href: string; label: string } | null {
  switch (runType) {
    case "ansible": return { href: DOC_LINKS.ansibleGuide, label: "Ansible Guide" };
    case "bash": return { href: DOC_LINKS.bashGuide, label: "Bash Guide" };
    case "powershell": return { href: DOC_LINKS.powershellGuide, label: "PowerShell Guide" };
    case "python": return { href: DOC_LINKS.pythonGuide, label: "Python Guide" };
    default: return null;
  }
}

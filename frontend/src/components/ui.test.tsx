// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import { render, screen, fireEvent, within } from "@testing-library/react";
import { statusLabel, jobStatusLabel, matchesStatus, matchesTags, Section, Field, FormField, Disclosure, SectionLabel } from "./ui";

// The canonical display vocabulary (V1.1-4) and its job-state sibling. These map
// raw wire/DB tokens onto the labels the whole app shows (VC.1). The one subtle
// rule is the queued fold: statusLabel folds queued→Running (correct for a run
// result), jobStatusLabel does NOT (a job whose next run is queued reads
// "Queued", not "Running" — else it contradicts a "Running Now: 0" tile).

describe("statusLabel", () => {
  it("maps every known run/job token to its canonical label", () => {
    const cases: [string, string][] = [
      ["success", "Success"],
      ["ok", "Success"],
      ["danger", "Failed"],
      ["fail", "Failed"], // the ?result=fail deep-link token (F-3)
      ["failed", "Failed"],
      ["failure", "Failed"],
      ["killed", "Failed"],
      ["warning", "Warn"],
      ["warn", "Warn"],
      ["skipped", "Skipped"],
      ["cancelled", "Cancelled"],
      ["running", "Running"],
      ["queued", "Running"], // run-result fold
      ["paused", "Paused"],
      ["idle", "Idle"],
    ];
    for (const [wire, label] of cases) expect(statusLabel(wire)).toBe(label);
  });

  it("title-cases an unknown token and em-dashes nullish input", () => {
    expect(statusLabel("mystery")).toBe("Mystery");
    expect(statusLabel(null)).toBe("—");
    expect(statusLabel(undefined)).toBe("—");
  });
});

describe("jobStatusLabel", () => {
  it("does NOT fold queued→Running for job states", () => {
    expect(jobStatusLabel("queued")).toBe("Queued");
    expect(jobStatusLabel("idle")).toBe("Idle");
    expect(jobStatusLabel("paused")).toBe("Paused");
    expect(jobStatusLabel("running")).toBe("Running");
  });

  it("delegates terminal/unknown tokens to statusLabel", () => {
    expect(jobStatusLabel("danger")).toBe("Failed");
    expect(jobStatusLabel("success")).toBe("Success");
    expect(jobStatusLabel(null)).toBe("—");
  });

  it("diverges from statusLabel exactly on queued", () => {
    expect(statusLabel("queued")).toBe("Running");
    expect(jobStatusLabel("queued")).toBe("Queued");
  });
});

// F-3 — the one filter predicate. Its whole contract is that it agrees with
// statusLabel by construction, so a status filter can never exclude a row that
// carries its own label. The three hand-rolled copies it replaced (ExecutionsTab's
// matchesResult, its RESULT_PARAM map, Workflows' tab predicates) had each drifted.
describe("matchesStatus (canonical filter predicate)", () => {
  it("matches every wire alias that renders under the chosen label", () => {
    for (const wire of ["danger", "failed", "failure", "killed"]) expect(matchesStatus("Failed", wire)).toBe(true);
    for (const wire of ["running", "queued"]) expect(matchesStatus("Running", wire)).toBe(true);
    for (const wire of ["success", "ok"]) expect(matchesStatus("Success", wire)).toBe(true);
    expect(matchesStatus("Cancelled", "cancelled")).toBe(true);
  });

  it("excludes a row whose label differs", () => {
    expect(matchesStatus("Failed", "success")).toBe(false);
    expect(matchesStatus("Running", "skipped")).toBe(false);
    expect(matchesStatus("Success", "warning")).toBe(false);
    expect(matchesStatus("Failed", undefined)).toBe(false);
  });

  it("agrees with statusLabel for every token, which is the point", () => {
    for (const wire of ["success", "ok", "danger", "killed", "warning", "skipped", "cancelled", "running", "queued", "paused", "mystery"]) {
      expect(matchesStatus(statusLabel(wire), wire)).toBe(true);
    }
  });

  it("lets 'All' through, including rows with no status at all", () => {
    expect(matchesStatus("All", "danger")).toBe(true);
    expect(matchesStatus("All", undefined)).toBe(true);
    expect(matchesStatus("All", null)).toBe(true);
  });
});

describe("matchesTags (shared catalog filter predicate)", () => {
  const row = ["prod", "web"];
  it("empty selection matches everything", () => {
    expect(matchesTags(row, [], "any")).toBe(true);
    expect(matchesTags(undefined, [], "all")).toBe(true);
  });
  it("matches per whole tag, never substring", () => {
    expect(matchesTags(row, ["prod"], "any")).toBe(true);
    expect(matchesTags(row, ["production"], "any")).toBe(false);
  });
  it("any = OR, all = AND", () => {
    expect(matchesTags(row, ["prod", "db"], "any")).toBe(true);
    expect(matchesTags(row, ["prod", "db"], "all")).toBe(false);
    expect(matchesTags(row, ["prod", "web"], "all")).toBe(true);
  });
  it("handles a row with no tags", () => {
    expect(matchesTags(undefined, ["prod"], "any")).toBe(false);
  });
});

// ── EV-1 shared expanded-detail primitives ───────────────────────────────────
// These replaced SEVEN local implementations across the tree (four byte-identical
// SectionLabel copies, one that had silently drifted, envvars' DetailLabel, and
// two Field variants). The behaviour worth pinning is the ⓘ disclosure: it is
// what lets static education text live outside the always-rendered body.

describe("Section", () => {
  it("renders its title and children, with no ⓘ when there is no info text", () => {
    render(
      <Section title="Registration">
        <div>body content</div>
      </Section>,
    );
    expect(screen.getByText("Registration")).toBeTruthy();
    expect(screen.getByText("body content")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "About this section" })).toBeNull();
  });

  it("keeps info text hidden until the ⓘ is pressed, and tracks aria-expanded", () => {
    render(
      <Section title="Groups" info="Groups are network-isolation agencies.">
        <div>editor</div>
      </Section>,
    );
    // The education text is NOT in the DOM until asked for — the whole point of
    // moving it behind the toggle is that it costs nothing on every expansion.
    expect(screen.queryByText("Groups are network-isolation agencies.")).toBeNull();
    const btn = screen.getByRole("button", { name: "About this section" });
    expect(btn.getAttribute("aria-expanded")).toBe("false");

    fireEvent.click(btn);
    expect(screen.getByText("Groups are network-isolation agencies.")).toBeTruthy();
    expect(btn.getAttribute("aria-expanded")).toBe("true");

    fireEvent.click(btn);
    expect(screen.queryByText("Groups are network-isolation agencies.")).toBeNull();
    expect(btn.getAttribute("aria-expanded")).toBe("false");
  });

  it("renders the actions slot", () => {
    render(
      <Section title="Managed settings" actions={<button>⚙ Edit</button>}>
        <div>summary</div>
      </Section>,
    );
    expect(screen.getByRole("button", { name: "⚙ Edit" })).toBeTruthy();
  });
});

describe("SectionLabel", () => {
  it("renders its caption and an optional right-aligned action", () => {
    render(<SectionLabel action={<button>Reveal</button>}>Value</SectionLabel>);
    expect(screen.getByText("Value")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reveal" })).toBeTruthy();
  });
});

describe("Field", () => {
  it("renders a label over a value node", () => {
    render(<Field label="Protocol version" value="v2" />);
    expect(screen.getByText("Protocol version")).toBeTruthy();
    expect(screen.getByText("v2")).toBeTruthy();
  });
});

// RU-10 — helperMode. The default must stay "always" so every pre-Phase-C call
// site is untouched; "engaged" is the opt-in that stops the helper prose from
// being a permanent wall under controls nobody is touching.
//
// Queries are scoped to each render's own container: this file has no global
// cleanup, so `screen` (which reads all of document.body) would find the
// previous test's helper node and report a passing suite for a broken component.
describe("FormField helperMode", () => {
  const HELP = "Ansible's -v levels.";

  it("renders the helper permanently by default", () => {
    const { container } = render(
      <FormField label="Verbosity" helper={HELP}>
        <input />
      </FormField>,
    );
    expect(within(container).getByText(HELP)).toBeTruthy();
  });

  it("hides an engaged helper until the field is focused, and restores it on blur", () => {
    const { container } = render(
      <FormField label="Verbosity" helperMode="engaged" helper={HELP}>
        <input />
      </FormField>,
    );
    const q = within(container);
    expect(q.queryByText(HELP)).toBeNull();

    fireEvent.focus(q.getByRole("textbox"));
    expect(q.getByText(HELP)).toBeTruthy();

    fireEvent.blur(q.getByRole("textbox"));
    expect(q.queryByText(HELP)).toBeNull();
  });

  it("shows an engaged helper whenever the control is active, unfocused", () => {
    const { container } = render(
      <FormField label="Verbosity" helperMode="engaged" active helper={HELP}>
        <input />
      </FormField>,
    );
    expect(within(container).getByText(HELP)).toBeTruthy();
  });

  // The one arm that must never be suppressible: a helper carrying the field's
  // validation verdict is the error, not a description of the control.
  it("always shows a danger helper, engaged mode or not", () => {
    const { container } = render(
      <FormField label="Hosts" helperMode="engaged" helperTone="danger" helper="Select at least one host.">
        <input />
      </FormField>,
    );
    expect(within(container).getByText("Select at least one host.")).toBeTruthy();
  });
});

// RU-12 — Disclosure tones. Render smokes only: what the tones LOOK like is the
// screenshot sweep's job; these pin that every tone still renders a working bar
// (title, summary, toggle, children) and that the summary — the one un-hideable
// fact — is present at the same node in all three. Queries scoped per-container
// (no global cleanup in this file).
describe("Disclosure tone", () => {
  const renderTone = (tone?: "primary" | "default" | "quiet") => {
    const onToggle = () => {};
    const { container } = render(
      <Disclosure title="Advanced options" summary="CHECK MODE" open={false} onToggle={onToggle} tone={tone}>
        <div>body</div>
      </Disclosure>,
    );
    return within(container);
  };

  it("renders title, summary and a collapsed body for every tone", () => {
    for (const tone of [undefined, "primary", "default", "quiet"] as const) {
      const q = renderTone(tone);
      expect(q.getByText("Advanced options")).toBeTruthy();
      expect(q.getByText("CHECK MODE")).toBeTruthy();
      expect(q.queryByText("body")).toBeNull();
      expect(q.getByRole("button").getAttribute("aria-expanded")).toBe("false");
    }
  });

  it("keeps the summary styling identical across tones", () => {
    // The collapsed summary carries CHECK MODE / an active --limit; a quiet bar
    // must not shrink it. Same font size in every tone.
    const sizes = ([undefined, "primary", "quiet"] as const).map((tone) => {
      const q = renderTone(tone);
      return (q.getByText("CHECK MODE") as HTMLElement).style.fontSize;
    });
    expect(new Set(sizes).size).toBe(1);
  });
});

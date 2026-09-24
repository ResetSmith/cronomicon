package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notify"
	"gopkg.in/yaml.v3"
)

// TestAlertEnumsAreHonoured is the guard VF-15 was missing (J-6).
//
// Settings → Notifications offered five controls the dispatcher had no branch
// for — `tag` targeting, the `n-failures-window` trigger, and the slack,
// webhook and in-app channels — and the API accepted all of them. Each one
// persisted, displayed, and produced nothing. Nobody noticed for months,
// because a notification rule that never fires is indistinguishable from a
// system that never failed.
//
// So this test asserts the property that was silently false: every value
// openapi.yaml's AlertRuleInput will accept has a live branch in the dispatcher.
// It checks BOTH directions — a spec value with no branch is the original
// defect; a branch the spec forbids means the UI cannot reach working
// behaviour, which is how Apprise stayed unselectable while working.
//
// Follows TestSpecRouteConformance's pattern: read the frozen spec, unmarshal,
// assert against the real implementation rather than a restated copy of it.
func TestAlertEnumsAreHonoured(t *testing.T) {
	enums := loadAlertRuleInputEnums(t)

	// ── spec → implementation: nothing accepted may be inert ──────────────────
	for _, mode := range enums["targetMode"] {
		if !notify.TargetModeHonoured(mode) {
			t.Errorf("openapi.yaml accepts targetMode %q but notify.targetMatches has no branch for it — "+
				"a rule created with it can never match a run (VF-15)", mode)
		}
	}
	for _, trigger := range enums["trigger"] {
		if !notify.TriggerHonoured(trigger) {
			t.Errorf("openapi.yaml accepts trigger %q but notify.triggerMatches has no branch for it — "+
				"a rule created with it can never fire (VF-15)", trigger)
		}
	}
	for _, ch := range enums["channels"] {
		if !notify.ChannelHonoured(ch) {
			t.Errorf("openapi.yaml accepts channel %q but the dispatcher has no transport for it — "+
				"a rule created with it sends nothing, silently (VF-15)", ch)
		}
	}

	// ── implementation → spec: working behaviour must be reachable ────────────
	//
	// The alias halves are intentionally NOT in the spec: `smtp`/`push` are the
	// config layer's older names for the same two transports, `always`/
	// `completion`/`completed` are synonyms of `any`, and an empty targetMode
	// means `all`. Listing them would offer an operator two words for one thing,
	// which is what Phase G spent a day removing. Everything else must be
	// creatable, or the UI cannot reach it.
	aliases := map[string]bool{
		"smtp": true, "push": true, // channel aliases of email / apprise
		"always": true, "completion": true, "completed": true, // aliases of `any`
		"": true, // empty targetMode is treated as `all`
	}
	honoured := map[string][]string{
		"targetMode": {"job", "all", ""},
		// SL added two triggers that are not run outcomes. They are listed here
		// (not as aliases) precisely because they must be creatable in the UI —
		// an alert the dispatcher honours but the spec forbids is the second
		// half of the VF-15 defect.
		"trigger": {"success", "failure", "warning", "killed", "skipped", "any", "always", "completion", "completed",
			"sla-breach", "missed-run"},
		"channels": {"email", "smtp", "apprise", "push"},
	}
	for field, values := range honoured {
		spec := map[string]bool{}
		for _, v := range enums[field] {
			spec[v] = true
		}
		for _, v := range values {
			if aliases[v] || spec[v] {
				continue
			}
			t.Errorf("the dispatcher honours %s %q but openapi.yaml's AlertRuleInput does not accept it — "+
				"working behaviour no client can reach (this is how Apprise stayed unselectable)", field, v)
		}
	}

	// ── the guard's own self-check ────────────────────────────────────────────
	// If the spec stops declaring these enums the loops above pass vacuously and
	// the guard silently stops guarding — the exact failure mode it exists to
	// prevent. Removing an enum is legal, but it must be a deliberate edit here.
	for _, field := range []string{"targetMode", "trigger", "channels"} {
		if len(enums[field]) == 0 {
			t.Errorf("AlertRuleInput.%s declares no enum — this guard is now vacuous. "+
				"If the field became free-form on purpose, drop it from this test and say why", field)
		}
	}
	if notify.TargetModeHonoured("tag") || notify.TriggerHonoured("n-failures-window") || notify.ChannelHonoured("slack") {
		t.Error("a value VF-15 removed is honoured again — if that is intentional, it needs the spec enum, " +
			"the UI control and this assertion updated together")
	}
}

// loadAlertRuleInputEnums pulls the enum lists off AlertRuleInput's targetMode,
// trigger and channels. `channels` is an array, so its enum sits on `items`.
func loadAlertRuleInputEnums(t *testing.T) map[string][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Enum  []string `yaml:"enum"`
					Items struct {
						Enum []string `yaml:"enum"`
					} `yaml:"items"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	schema, ok := spec.Components.Schemas["AlertRuleInput"]
	if !ok {
		t.Fatal("openapi.yaml has no AlertRuleInput schema — wrong file, or the schema was renamed")
	}
	out := map[string][]string{}
	for _, field := range []string{"targetMode", "trigger", "channels"} {
		prop, ok := schema.Properties[field]
		if !ok {
			t.Fatalf("AlertRuleInput has no %q property — this guard needs updating alongside that change", field)
		}
		if len(prop.Enum) > 0 {
			out[field] = prop.Enum
		} else {
			out[field] = prop.Items.Enum
		}
	}
	return out
}

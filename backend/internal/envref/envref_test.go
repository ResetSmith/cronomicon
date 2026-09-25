package envref

import "testing"

func TestSplit(t *testing.T) {
	cases := []struct {
		in      string
		section Section
		bare    string
		ok      bool
	}{
		{"CRONOMICON_VAR_FOO", SectionVariable, "FOO", true},
		{"CRONOMICON_SECRET_NWD_BECOME_PASS", SectionSecret, "NWD_BECOME_PASS", true},
		{"CRONOMICON_KEY_ansible_rh8_key", SectionKey, "ansible_rh8_key", true},
		{"CRONOMICON_RUN_TRACE_ID", SectionRun, "TRACE_ID", true},
		// Exact-prefix: SECRETS_ (plural) is NOT a reference.
		{"CRONOMICON_SECRETS_INJECTION_ENABLED", SectionNone, "", false},
		// Config names are not references.
		{"CRONOMICON_KEK_FILE", SectionNone, "", false},
		{"CRONOMICON_VAULT_ADDR", SectionNone, "", false},
		{"PLAIN", SectionNone, "", false},
	}
	for _, c := range cases {
		sec, bare, ok := Split(c.in)
		if sec != c.section || bare != c.bare || ok != c.ok {
			t.Errorf("Split(%q) = (%v,%q,%v), want (%v,%q,%v)", c.in, sec, bare, ok, c.section, c.bare, c.ok)
		}
	}
}

func TestHasCronomiconPrefix(t *testing.T) {
	if !HasCronomiconPrefix("CRONOMICON_ANYTHING") {
		t.Error("CRONOMICON_ANYTHING should be in the namespace")
	}
	if HasCronomiconPrefix("PATH") {
		t.Error("PATH is not in the namespace")
	}
}

func TestStripKeyAndValue(t *testing.T) {
	if b, ok := StripKey("CRONOMICON_KEY_x"); !ok || b != "x" {
		t.Errorf("StripKey = (%q,%v)", b, ok)
	}
	if b, ok := StripKey("x"); ok || b != "x" {
		t.Errorf("StripKey passthrough = (%q,%v)", b, ok)
	}
	if b, ok := StripValueReference("CRONOMICON_SECRET_x"); !ok || b != "x" {
		t.Errorf("StripValueReference secret = (%q,%v)", b, ok)
	}
	if b, ok := StripValueReference("CRONOMICON_VAR_x"); !ok || b != "x" {
		t.Errorf("StripValueReference var = (%q,%v)", b, ok)
	}
	if _, ok := StripValueReference("CRONOMICON_KEY_x"); ok {
		t.Error("StripValueReference must not strip a KEY reference (it is a path, not a value)")
	}
}

func TestValidateRowName(t *testing.T) {
	good := []string{"FOO", "ansible_rh8_key", "_x", "A1_B2", "NWD_BECOME_PASS"}
	for _, n := range good {
		if err := ValidateRowName(n); err != nil {
			t.Errorf("ValidateRowName(%q) unexpected error: %v", n, err)
		}
	}
	bad := []string{"", "1abc", "has-dash", "has space", "CRONOMICON_X", "CRONOMICON_SECRET_X"}
	for _, n := range bad {
		if err := ValidateRowName(n); err == nil {
			t.Errorf("ValidateRowName(%q) expected error", n)
		}
	}
}

func TestValidateSecretRowName(t *testing.T) {
	for _, n := range []string{"KEK", "KEK_FILE", "KEK_VERSION", "KEK_1", "KEK_2_FILE"} {
		if err := ValidateSecretRowName(n); err == nil {
			t.Errorf("ValidateSecretRowName(%q) must be rejected (collides with KEK config)", n)
		}
	}
	// A non-KEK name that merely contains KEK is fine.
	for _, n := range []string{"KEKW", "MY_KEK", "KEKS"} {
		if err := ValidateSecretRowName(n); err != nil {
			t.Errorf("ValidateSecretRowName(%q) unexpected error: %v", n, err)
		}
	}
}

func TestValidateOperatorEnv(t *testing.T) {
	if err := ValidateOperatorEnv(map[string]string{"FOO": "1", "BAR": "2"}); err != nil {
		t.Errorf("clean env should pass: %v", err)
	}
	if err := ValidateOperatorEnv(nil); err != nil {
		t.Errorf("nil env should pass: %v", err)
	}
	for _, k := range []string{"CRONOMICON_SECRET_X", "CRONOMICON_VAR_X", "CRONOMICON_KEY_X", "CRONOMICON_RUN_X", "CRONOMICON_KEK", "CRONOMICON_ANYTHING"} {
		if err := ValidateOperatorEnv(map[string]string{k: "v"}); err == nil {
			t.Errorf("ValidateOperatorEnv must reject %q", k)
		}
	}
}

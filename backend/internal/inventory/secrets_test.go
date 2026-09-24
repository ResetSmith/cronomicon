package inventory

import (
	"strings"
	"testing"
)

// TestValidateSecretsFailsClosedOnOversizedLine: a line exceeding the 1 MiB
// scanner buffer must be REJECTED (fail-closed), never silently treated as clean
// — otherwise a padded secret-bearing line would bypass the only guard.
func TestValidateSecretsFailsClosedOnOversizedLine(t *testing.T) {
	huge := strings.Repeat("x", (1<<20)+16)
	content := "[web]\nweb1\n" + huge + " ansible_ssh_pass=hunter2\n"
	errs := ValidateSecrets(content, "inventory/big.ini")
	if len(errs) == 0 {
		t.Fatal("oversized line must be rejected fail-closed, got no errors")
	}
}

func TestValidateSecrets(t *testing.T) {
	cases := []struct {
		name         string
		content      string
		wantRejected bool
		wantLine     int    // first offending line (when rejected)
		wantVar      string // first offending var (when rejected)
	}{
		// ── allowed ──────────────────────────────────────────────────────────
		{name: "plain hosts", content: "[web]\nweb1\nweb2 ansible_host=10.0.0.2\n"},
		{name: "private key file path is allowed",
			content: "db1 ansible_user=deploy ansible_ssh_private_key_file=/keys/db1\n"},
		{name: "anchored env lookup double quotes allowed",
			content: "db1 ansible_become_pass=\"{{ lookup('env','DB1_PW') }}\"\n"},
		{name: "anchored builtin env lookup allowed",
			content: "db1 ansible_ssh_pass=\"{{ lookup('ansible.builtin.env','PW') }}\"\n"},
		{name: "comment line with pass is ignored",
			content: "# ansible_become_pass=whatever is just a comment\nweb1\n"},
		{name: "non-secret var with =pass substring not matched",
			content: "web1 ansible_ssh_private_key_file=/k some_password_note=ok\n"},

		// ── rejected ─────────────────────────────────────────────────────────
		{name: "inline ssh pass literal", content: "db1 ansible_ssh_pass=hunter2\n",
			wantRejected: true, wantLine: 1, wantVar: "ansible_ssh_pass"},
		{name: "become pass literal in group vars",
			content: "[db:vars]\nansible_become_pass=s3cr3t\n",
			wantRejected: true, wantLine: 2, wantVar: "ansible_become_pass"},
		{name: "sudo pass quoted literal",
			content: "db1 ansible_sudo_pass='letmein'\n",
			wantRejected: true, wantLine: 1, wantVar: "ansible_sudo_pass"},
		{name: "mixed literal + lookup is rejected (anchoring regression)",
			content: "db1 ansible_become_pass=\"hunter2 {{ lookup('env','X') }}\"\n",
			wantRejected: true, wantLine: 1, wantVar: "ansible_become_pass"},
		{name: "inline vault marker",
			content: "db1 some_var=!vault |\n",
			wantRejected: true, wantLine: 1, wantVar: "vault"},
		{name: "ANSIBLE_VAULT header marker",
			content: "$ANSIBLE_VAULT;1.1;AES256\n",
			wantRejected: true, wantLine: 1, wantVar: "vault"},
		{name: "winrm password is NOT in the narrow reject-set (accepted tradeoff)",
			content: "win1 ansible_winrm_password=secret\n"}, // allowed: not in the explicit set

		// ── reserved-namespace guard (W4 / N-D1) ─────────────────────────────
		{name: "referencing an AMADEUS_ name via lookup is allowed",
			content: "db1 ansible_become_pass=\"{{ lookup('env','AMADEUS_SECRET_DB_PW') }}\"\n"},
		{name: "literal AMADEUS_ assignment (standalone) is rejected",
			content: "[db:vars]\nAMADEUS_SECRET_X=hunter2\n",
			wantRejected: true, wantLine: 2, wantVar: "AMADEUS_SECRET_X"},
		{name: "literal AMADEUS_ assignment (inline host var) is rejected",
			content: "db1 ansible_host=10.0.0.1 AMADEUS_KEY_deploy=/tmp/k\n",
			wantRejected: true, wantLine: 1, wantVar: "AMADEUS_KEY_deploy"},
		{name: "defining an AMADEUS_ key even to a lookup value is rejected",
			content: "AMADEUS_VAR_X=\"{{ lookup('env','X') }}\"\n",
			wantRejected: true, wantLine: 1, wantVar: "AMADEUS_VAR_X"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateSecrets(tc.content, "inventory/test.ini")
			if tc.wantRejected {
				if len(errs) == 0 {
					t.Fatalf("expected rejection, got none")
				}
				if errs[0].Line != tc.wantLine {
					t.Errorf("first error line = %d, want %d", errs[0].Line, tc.wantLine)
				}
				if errs[0].Var != tc.wantVar {
					t.Errorf("first error var = %q, want %q", errs[0].Var, tc.wantVar)
				}
				if errs[0].File != "inventory/test.ini" {
					t.Errorf("error file = %q, want inventory/test.ini", errs[0].File)
				}
				if errs[0].Error() == "" {
					t.Errorf("Error() string is empty")
				}
			} else if len(errs) != 0 {
				t.Fatalf("expected no rejection, got %d: %v", len(errs), errs)
			}
		})
	}
}

// RX.9 (ansible-update.md, Phase 1) — EnvLookupNames extracts the env-var NAMES
// an inventory references via {{ lookup('env', NAME) }}, feeding the manifest
// env-passthrough allowlist.
func TestEnvLookupNames(t *testing.T) {
	content := `[web]
web1 ansible_ssh_pass="{{ lookup('env','WEB_PASS') }}" api_token="{{ lookup('env', 'API_TOKEN') }}"
web2 ansible_password="{{ lookup("env", "WEB_PASS") }}"

[db]
db1 becomes="{{ lookup('ansible.builtin.env', 'DB_BECOME') }}"
# a comment carrying {{ lookup('env','IN_COMMENT') }} still counts (names only — harmless over-inclusion)
plain1 ansible_host=10.0.0.1
`
	got := EnvLookupNames(content)
	want := []string{"API_TOKEN", "DB_BECOME", "IN_COMMENT", "WEB_PASS"}
	if len(got) != len(want) {
		t.Fatalf("EnvLookupNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("EnvLookupNames[%d] = %q, want %q (dedup + sorted)", i, got[i], want[i])
		}
	}

	if names := EnvLookupNames("[web]\nweb1 ansible_host=10.0.0.1\n"); len(names) != 0 {
		t.Errorf("no lookups expected, got %v", names)
	}
	// An invalid identifier is not a match (names must be env-var idents).
	if names := EnvLookupNames(`x="{{ lookup('env','9BAD') }}"`); len(names) != 0 {
		t.Errorf("invalid ident must not match, got %v", names)
	}
}

// Phase 3 (§6.2) — ScanSecretAssignments catches YAML-form (key: value) inline
// secrets that the INI-only ValidateSecrets never sees, while allowing env-lookup
// indirection and skipping comments.
func TestScanSecretAssignments(t *testing.T) {
	content := `# a comment: ansible_become_pass: notasecret
ansible_become_pass: hunter2
ansible_password: "{{ lookup('env','WEB_PASS') }}"
other_var: ordinary
ansible_ssh_pass = inivalue
`
	hits := ScanSecretAssignments(content)
	if len(hits) != 2 {
		t.Fatalf("want 2 hits (YAML literal + INI literal), got %d: %+v", len(hits), hits)
	}
	byVar := map[string]int{}
	for _, h := range hits {
		byVar[h.Var] = h.Line
	}
	if byVar["ansible_become_pass"] != 2 {
		t.Errorf("expected ansible_become_pass literal on line 2, got %+v", hits)
	}
	if byVar["ansible_ssh_pass"] != 5 {
		t.Errorf("expected ansible_ssh_pass literal on line 5, got %+v", hits)
	}
	// The env-lookup line and the comment must NOT be flagged.
	for _, h := range hits {
		if h.Line == 1 || h.Line == 3 {
			t.Errorf("comment / env-lookup must not be flagged: %+v", h)
		}
	}
}

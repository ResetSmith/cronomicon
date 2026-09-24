package execspec

import "testing"

// CA (the ssh-user plan §2.2) — the per-run "connect as" overlay.

func TestApplyIdentityOverride(t *testing.T) {
	base := func() []Target {
		return []Target{
			{Name: "web1", User: "hostuser", AuthCredentialID: "cred-host", Via: "bastion-1", HostKey: "hk"},
			{Name: "web2", User: "", AuthKeyEnvVar: "LEGACY_KEY"},
			{Name: "gone", ResolveErr: "no SSH host record"},
		}
	}

	t.Run("no override is identity", func(t *testing.T) {
		in := base()
		out := ApplyIdentityOverride(in, "", "", "")
		for i := range in {
			if out[i] != in[i] {
				t.Errorf("target %d changed with empty override: %+v", i, out[i])
			}
		}
	})

	t.Run("user only", func(t *testing.T) {
		out := ApplyIdentityOverride(base(), "deploy", "", "")
		if out[0].User != "deploy" || out[1].User != "deploy" {
			t.Errorf("user not applied: %+v", out)
		}
		// Key selection untouched.
		if out[0].AuthCredentialID != "cred-host" || out[1].AuthKeyEnvVar != "LEGACY_KEY" {
			t.Errorf("key selection must be untouched on user-only override: %+v", out)
		}
	})

	t.Run("credential id clears the legacy env-var fallback", func(t *testing.T) {
		out := ApplyIdentityOverride(base(), "", "cred-override", "")
		for _, tg := range out[:2] {
			if tg.AuthCredentialID != "cred-override" {
				t.Errorf("credential not applied on %q: %+v", tg.Name, tg)
			}
			if tg.AuthKeyEnvVar != "" {
				t.Errorf("AuthKeyEnvVar must be cleared so the fallback can't race the override: %+v", tg)
			}
		}
		if out[0].User != "hostuser" {
			t.Errorf("user must be untouched on credential-only override: %+v", out[0])
		}
	})

	t.Run("runner keyEnvVar clears the credential id", func(t *testing.T) {
		out := ApplyIdentityOverride(base(), "svc", "", "CRONOMICON_KEY_prod")
		for _, tg := range out[:2] {
			if tg.AuthKeyEnvVar != "CRONOMICON_KEY_prod" || tg.AuthCredentialID != "" {
				t.Errorf("derived key reference not applied on %q: %+v", tg.Name, tg)
			}
			if tg.User != "svc" {
				t.Errorf("user not applied on %q: %+v", tg.Name, tg)
			}
		}
	})

	t.Run("routing and unresolved targets untouched", func(t *testing.T) {
		out := ApplyIdentityOverride(base(), "deploy", "cred-override", "")
		// CA-Q4 — the bastion hop and host-key trust are routing, not identity.
		if out[0].Via != "bastion-1" || out[0].HostKey != "hk" {
			t.Errorf("routing fields must be untouched: %+v", out[0])
		}
		if out[2] != base()[2] {
			t.Errorf("ResolveErr target must pass through unchanged: %+v", out[2])
		}
	})
}

func TestValidSSHUser(t *testing.T) {
	valid := []string{"root", "deploy", "_svc", "a", "ubuntu-admin", "web.ops", "User2", "a234567890123456789012345678901e"}
	for _, u := range valid {
		if !ValidSSHUser(u) {
			t.Errorf("ValidSSHUser(%q) = false, want true", u)
		}
	}
	invalid := []string{"", "1abc", ".dot", "-dash", "has space", "semi;colon", "a$b", "sl/ash", "a2345678901234567890123456789012e" /* 33 */, "nul\x00byte", "ключ"}
	for _, u := range invalid {
		if ValidSSHUser(u) {
			t.Errorf("ValidSSHUser(%q) = true, want false", u)
		}
	}
}

// RP-7 — IdentityCapableRunType is the single fold gate shared by all four
// producers and both authoring boundaries. It is deliberately neither
// "ssh-family" (ansible applies the override as connection extra-vars, a real
// mechanism) nor "anything but terraform" (an unknown type must not inherit
// support nothing has been taught to apply).
func TestIdentityCapableRunType(t *testing.T) {
	for _, rt := range []string{"bash", "perl", "powershell", "python", "ansible"} {
		if !IdentityCapableRunType(rt) {
			t.Errorf("IdentityCapableRunType(%q) = false, want true", rt)
		}
	}
	for _, rt := range []string{"terraform", "", "nonesuch"} {
		if IdentityCapableRunType(rt) {
			t.Errorf("IdentityCapableRunType(%q) = true, want false", rt)
		}
	}
	// Every SSH-executor type must remain identity-capable: the in-app executor
	// has applied the override since v0.54.0.
	for rt := range SSHRunTypes {
		if !IdentityCapableRunType(rt) {
			t.Errorf("ssh-family %q lost identity capability", rt)
		}
	}
}

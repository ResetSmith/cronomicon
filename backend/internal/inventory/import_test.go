package inventory

import "testing"

// TestHostConns_TransitiveGroupVars: connection vars inherit from ANCESTOR groups
// (via [group:children]) and the implicit [all], not just the host's direct group
// — matching how ansible resolves them — so imported ssh_hosts get the right
// user/key (M4 review fix).
func TestHostConns_TransitiveGroupVars(t *testing.T) {
	content := "[web]\nweb1 ansible_host=10.0.0.1\n" +
		"[prod:children]\nweb\n" +
		"[prod:vars]\nansible_user=deploy\namadeus_auth_key_env_var=PROD_KEY\n" +
		"[all:vars]\nansible_port=2200\n"
	p := ParseProjection(content)
	if p.PreviewUnavailable {
		t.Fatalf("unexpected degrade: %s (line %d)", p.PreviewReason, p.PreviewLine)
	}
	conns := HostConns(p, "DEFAULT_KEY")
	if len(conns) != 1 {
		t.Fatalf("conns = %d, want 1 (web1 — group-var/children lines are not hosts)", len(conns))
	}
	c := conns[0]
	if c.Host != "web1" || c.Address != "10.0.0.1" {
		t.Errorf("host/address = %q/%q, want web1/10.0.0.1", c.Host, c.Address)
	}
	if c.User != "deploy" {
		t.Errorf("user = %q, want deploy (inherited from ancestor [prod:vars])", c.User)
	}
	if c.AuthKeyEnvVar != "PROD_KEY" {
		t.Errorf("auth-key = %q, want PROD_KEY (ancestor var beats the scope default)", c.AuthKeyEnvVar)
	}
	if c.Port != 2200 {
		t.Errorf("port = %d, want 2200 (from [all:vars])", c.Port)
	}
}

// TestHostConns_HostVarWins: host_vars override inherited group_vars.
func TestHostConns_HostVarWins(t *testing.T) {
	content := "[web]\nweb1 ansible_user=root\n[web:vars]\nansible_user=svc\namadeus_auth_key_env_var=WEB_KEY\n"
	conns := HostConns(ParseProjection(content), "")
	if len(conns) != 1 || conns[0].User != "root" {
		t.Errorf("conns = %+v, want web1 user=root (host_var beats group_var)", conns)
	}
	if conns[0].AuthKeyEnvVar != "WEB_KEY" {
		t.Errorf("auth-key = %q, want WEB_KEY (group var, no host override)", conns[0].AuthKeyEnvVar)
	}
}

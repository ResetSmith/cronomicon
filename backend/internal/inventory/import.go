package inventory

import "maps"

import "strconv"

// HostConn is one host's connection detail derived from a parsed inventory, ready
// to map into an ssh_hosts row (M4 / §9.1): ansible_host→Address, ansible_port→Port,
// ansible_user→User, and the auth-key NAME (per-host cronomicon_auth_key_env_var, else
// the scope default). NAMES only — never a secret value (D1).
type HostConn struct {
	Host          string
	Address       string // ansible_host ("" ⇒ dial by hostname)
	Port          int    // ansible_port (0 ⇒ default 22)
	User          string // ansible_user
	AuthKeyEnvVar string // env-var NAME the in-app loadSigner resolves
}

// HostConns computes the per-host connection detail for every real host in a
// projection (ConnHosts — the [group:vars]/[group:children] phantoms are
// excluded). Effective vars are group_vars of the host's containing groups
// overlaid by host_vars (host_vars win). defaultAuthKey (a per-scope sidecar
// NAME) is used when a host has no cronomicon_auth_key_env_var. ansible_ssh_private_
// key_file is intentionally NOT consulted — the in-app executor needs a secret
// NAME, not a runner-side path.
func HostConns(p Projection, defaultAuthKey string) []HostConn {
	groupsOf := map[string][]string{} // host → groups directly containing it
	for gname, g := range p.Groups {
		for _, h := range g.Hosts {
			groupsOf[h] = append(groupsOf[h], gname)
		}
	}
	// parentsOf: invert [group:children] so we can walk a host's group closure UP
	// for transitive var inheritance ([prod:vars] applies to web via [prod:children]).
	parentsOf := map[string][]string{}
	for gname, g := range p.Groups {
		for _, c := range g.Children {
			parentsOf[c] = append(parentsOf[c], gname)
		}
	}
	out := make([]HostConn, 0, len(p.ConnHosts))
	for _, h := range p.ConnHosts {
		v := map[string]string{}
		apply := func(g string) {
			maps.Copy(v, p.Groups[g].Vars)
		}
		// Broad → narrow so the more-specific value wins: implicit `all`, then the
		// transitive ancestors of the host's direct groups, then the direct groups,
		// then host_vars.
		if _, ok := p.Groups["all"]; ok {
			apply("all")
		}
		direct := groupsOf[h]
		ancSeen := map[string]bool{}
		var walkUp func(g string)
		walkUp = func(g string) {
			for _, par := range parentsOf[g] {
				if !ancSeen[par] {
					ancSeen[par] = true
					apply(par)
					walkUp(par)
				}
			}
		}
		for _, g := range direct {
			walkUp(g)
		}
		for _, g := range direct {
			apply(g)
		}
		// host_vars win over all group_vars
		maps.Copy(v, p.HostVars[h])
		hc := HostConn{
			Host:          h,
			Address:       v["ansible_host"],
			User:          v["ansible_user"],
			AuthKeyEnvVar: v["cronomicon_auth_key_env_var"],
		}
		if hc.AuthKeyEnvVar == "" {
			hc.AuthKeyEnvVar = defaultAuthKey
		}
		if ps := v["ansible_port"]; ps != "" {
			if n, err := strconv.Atoi(ps); err == nil {
				hc.Port = n
			}
		}
		out = append(out, hc)
	}
	return out
}

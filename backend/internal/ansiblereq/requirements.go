// Package ansiblereq parses and lints Ansible requirements.yml files. It is a
// leaf package (stdlib + yaml only) imported by BOTH the git-sync/validate path
// (internal/gitlab) and the runner agent (internal/agent), so the pin rules and
// the parse shape live in exactly one place and cannot drift
// (ansible-update.md RX.4/RX.5).
package ansiblereq

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Requirements is the parsed collections + roles of a requirements.yml.
type Requirements struct {
	Collections []Collection
	Roles       []Role
}

// Collection is one `collections:` entry. Version is the pin ("" if unpinned).
type Collection struct {
	Name    string
	Version string
	Source  string // optional galaxy source URL
}

// Role is one `roles:` entry. A git role has Src (a URL) or SCM set; a galaxy
// role has only Name. Version is the pin (a SHA for git roles).
type Role struct {
	Name    string
	Src     string
	SCM     string
	Version string
}

// UnmarshalYAML lets a collection be either a bare string ("- community.vmware")
// or a mapping ("- {name: community.vmware, version: 3.5.0}").
func (c *Collection) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		c.Name = n.Value
		return nil
	}
	var m struct {
		Name    string `yaml:"name"`
		Version string `yaml:"version"`
		Source  string `yaml:"source"`
	}
	if err := n.Decode(&m); err != nil {
		return err
	}
	c.Name, c.Version, c.Source = m.Name, m.Version, m.Source
	return nil
}

// UnmarshalYAML lets a role be either a bare string ("- geerlingguy.nginx") or a
// mapping ("- {src: ..., version: <sha>}").
func (r *Role) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		r.Name = n.Value
		return nil
	}
	var m struct {
		Name    string `yaml:"name"`
		Src     string `yaml:"src"`
		SCM     string `yaml:"scm"`
		Version string `yaml:"version"`
	}
	if err := n.Decode(&m); err != nil {
		return err
	}
	r.Name, r.Src, r.SCM, r.Version = m.Name, m.Src, m.SCM, m.Version
	return nil
}

// Parse reads requirements.yml content. It accepts both the modern
// collections/roles document and the legacy bare-list (roles-only) document.
func Parse(content []byte) (Requirements, error) {
	// Modern form.
	var doc struct {
		Collections []Collection `yaml:"collections"`
		Roles       []Role       `yaml:"roles"`
	}
	if err := yaml.Unmarshal(content, &doc); err == nil && (doc.Collections != nil || doc.Roles != nil) {
		return Requirements{Collections: doc.Collections, Roles: doc.Roles}, nil
	}
	// Legacy bare list = roles only.
	var roles []Role
	if err := yaml.Unmarshal(content, &roles); err != nil {
		return Requirements{}, fmt.Errorf("parse requirements.yml: %w", err)
	}
	return Requirements{Roles: roles}, nil
}

// Finding is one pinning-lint violation (RX.5).
type Finding struct {
	Kind    string // "collection" | "role"
	Name    string
	Message string
}

func (f Finding) String() string { return fmt.Sprintf("%s %q: %s", f.Kind, f.Name, f.Message) }

// Lint enforces the RX.5 reproducibility rule: every collection has an EXACT
// version, and every git role is pinned to a full commit SHA (galaxy roles need
// an exact version too). Ranges, wildcards, and unpinned entries are flagged —
// "same SHA, different Tuesday" is the failure this prevents.
func Lint(r Requirements) []Finding {
	var out []Finding
	for _, c := range r.Collections {
		name := c.Name
		if name == "" {
			name = "(unnamed)"
		}
		if c.Version == "" {
			out = append(out, Finding{"collection", name, "no version pin — require an exact version (e.g. version: 3.5.0)"})
			continue
		}
		if !isExactVersion(c.Version) {
			out = append(out, Finding{"collection", name, fmt.Sprintf("version %q is not an exact pin — ranges/wildcards break reproducibility", c.Version)})
		}
	}
	for _, ro := range r.Roles {
		name := ro.Name
		if name == "" {
			name = ro.Src
		}
		if name == "" {
			name = "(unnamed)"
		}
		if ro.Version == "" {
			out = append(out, Finding{"role", name, "no version pin — pin a git role to a full commit SHA, a galaxy role to an exact version"})
			continue
		}
		if isGitRole(ro) {
			if !isFullSHA(ro.Version) {
				out = append(out, Finding{"role", name, fmt.Sprintf("git role version %q is not a full commit SHA (a branch/tag is not reproducible)", ro.Version)})
			}
		} else if !isExactVersion(ro.Version) {
			out = append(out, Finding{"role", name, fmt.Sprintf("version %q is not an exact pin", ro.Version)})
		}
	}
	return out
}

// isExactVersion reports whether v is a single exact version, not a range,
// wildcard, comparator, or "latest".
func isExactVersion(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "latest") {
		return false
	}
	if strings.ContainsAny(v, "<>=~^!|*, ") {
		return false
	}
	return true
}

// isGitRole reports whether a role is fetched from a git source (so it must be
// SHA-pinned) rather than the galaxy server.
func isGitRole(r Role) bool {
	if strings.EqualFold(r.SCM, "git") {
		return true
	}
	s := r.Src
	return strings.Contains(s, "://") || strings.HasPrefix(s, "git@") || strings.HasSuffix(s, ".git")
}

// isFullSHA reports whether v is a full git commit SHA (40 hex / 64 hex).
func isFullSHA(v string) bool {
	if len(v) != 40 && len(v) != 64 {
		return false
	}
	for _, c := range v {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

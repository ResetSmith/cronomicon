package inventory

import "strings"

// Projection is the best-effort, ADVISORY parsed view of an INI inventory used
// for the UI and (M3) group targeting. It is NEVER authoritative for execution —
// ansible reads the raw file via `-i`. On the first out-of-subset construct the
// projection is suppressed (PreviewUnavailable) and the group tree is NOT built,
// rather than rendering a half-parsed (and therefore lying) tree.
type Projection struct {
	Hosts     []string                     // naive host membership (same semantics as the old parseInventoryHosts)
	ConnHosts []string                     // M4 — clean, mode-aware host set (real hosts only, NO [group:vars]/[group:children] phantoms); the ssh_hosts import source
	HostVars  map[string]map[string]string // host → key → value
	Groups    map[string]GroupProjection   // group name → members/children/vars

	PreviewUnavailable bool
	PreviewReason      string
	PreviewLine        int // 1-based line of the first out-of-subset construct
}

// GroupProjection is one parsed group.
type GroupProjection struct {
	Hosts    []string
	Children []string
	Vars     map[string]string
}

// ValidName reports whether a group/host name is safe to feed `ansible --limit`
// (§4.5). ansible's --limit pattern language interprets `, : ! & *` and
// whitespace; a name containing any of those could broaden a run beyond the
// operator's selection and desync the SSH host-set expansion. Allow only
// alphanumerics plus `- _ .` (covers hostnames, FQDNs, IPs); reject everything
// else — the parser treats a metachar-bearing name as degrade-loudly, never
// silent passthrough.
func ValidName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// Hosts returns the host names of an INI inventory using the naive,
// never-degrading rule the SSH-targeting path has always used (first
// whitespace-separated token of each non-blank, non-comment, non-`[section]`
// line; deduped, order-preserving). It is the single implementation behind both
// the scope_hosts membership write (via gitlab.parseInventoryHosts, which
// delegates here) and Projection.Hosts, so the two can't drift.
//
// WART (pre-existing, intentionally preserved — the locked single-membership
// rule): because it is section-unaware, a `key=value` body line under
// `[group:vars]` or a child name under `[group:children]` is also counted as a
// "host". This over-collection has always been in scope_hosts for grouped git
// inventories; changing it would alter SSH-targeting semantics, so it stays. The
// section-aware ParseProjection group tree is unaffected.
func Hosts(content string) []string {
	seen := map[string]bool{}
	var hosts []string
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
			continue
		}
		host := strings.Fields(line)[0]
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	return hosts
}

// InferTypes is the conservative heuristic that guesses a scope's run-type
// capability from raw inventory content when no pragma/sidecar declares it. Shared
// by git sync (gitlab.inferScopeTypes delegates here) and the in-app inventory
// upload handler (M5) so both infer identically. Always includes the bash floor.
func InferTypes(content string) []string {
	types := map[string]bool{}
	if strings.Contains(content, "[all:vars]") &&
		(strings.Contains(content, "ansible_user") || strings.Contains(content, "ansible_become") || strings.Contains(content, "ansible_host")) {
		types["ansible"] = true
		types["bash"] = true
	}
	if strings.Contains(content, "win-srv") || strings.Contains(content, "[windows]") {
		types["powershell"] = true
	}
	if len(types) == 0 {
		types["bash"] = true
	}
	out := make([]string, 0, len(types))
	for t := range types {
		out = append(out, t)
	}
	return out
}

// ParseProjection parses the SUPPORTED static-INI subset into a Projection:
//
//  1. plain host lines;
//  2. inline host vars `host key=value key2="quoted value"` (key=value only);
//  3. `[group]` section headers → membership;
//  4. `[group:children]` → child-group names;
//  5. `[group:vars]` → group-scoped key=value;
//  6. `#` / `;` comments, including inline trailing comments (outside quotes);
//  7. blank lines;
//  8. implicit top-level (ungrouped) hosts.
//
// Anything outside this subset — host ranges (`web[01:50]`), unknown section
// suffixes, bare (non key=value) tokens after a host, or a name with an ansible
// `--limit` metacharacter — sets PreviewUnavailable + reason + line and STOPS
// building the tree (the half-parsed Groups/HostVars are discarded). Hosts is
// always populated via the naive Hosts() rule so scope_hosts membership is
// unaffected by a projection degrade.
func ParseProjection(content string) Projection {
	p := Projection{
		Hosts:    Hosts(content),
		HostVars: map[string]map[string]string{},
		Groups:   map[string]GroupProjection{},
	}

	degrade := func(line int, reason string) {
		p.PreviewUnavailable = true
		p.PreviewReason = reason
		p.PreviewLine = line
	}
	ensureGroup := func(name string) {
		if _, ok := p.Groups[name]; !ok {
			p.Groups[name] = GroupProjection{}
		}
	}

	curGroup := "" // "" = top-level/ungrouped
	curMode := "hosts"
	connSeen := map[string]bool{} // dedup for ConnHosts (real hosts-mode hosts only)

	for i, rawLine := range strings.Split(content, "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(stripComment(rawLine))
		if line == "" {
			continue
		}

		// Section header.
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				degrade(lineNo, "malformed section header (no closing ']')")
				break
			}
			inner := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			name, mode := inner, "hosts"
			if before, after, ok := strings.Cut(inner, ":"); ok {
				name = before
				switch after {
				case "children":
					mode = "children"
				case "vars":
					mode = "vars"
				default:
					degrade(lineNo, "unsupported section suffix [:"+after+"]")
				}
				if p.PreviewUnavailable {
					break
				}
			}
			if !ValidName(name) {
				degrade(lineNo, "group name has an unsupported character: "+name)
				break
			}
			curGroup, curMode = name, mode
			ensureGroup(name)
			continue
		}

		switch curMode {
		case "children":
			fields, balanced := tokenizeFields(line)
			if !balanced || len(fields) != 1 || !ValidName(fields[0]) {
				degrade(lineNo, "unsupported [group:children] entry: "+line)
				break
			}
			child := fields[0]
			g := p.Groups[curGroup]
			g.Children = append(g.Children, child)
			p.Groups[curGroup] = g
			ensureGroup(child)

		case "vars":
			// One key=value per line (subset item 5). Tokenize so an extra token or
			// an unterminated quote degrades loudly, matching the hosts-mode rigor.
			fields, balanced := tokenizeFields(line)
			if !balanced || len(fields) != 1 {
				degrade(lineNo, "[group:vars] expects exactly one key=value per line: "+line)
				break
			}
			k, v, ok := parseKV(fields[0])
			if !ok {
				degrade(lineNo, "non key=value entry in [group:vars]: "+line)
				break
			}
			g := p.Groups[curGroup]
			if g.Vars == nil {
				g.Vars = map[string]string{}
			}
			g.Vars[k] = v
			p.Groups[curGroup] = g

		default: // hosts
			fields, balanced := tokenizeFields(line)
			if !balanced {
				degrade(lineNo, "unterminated quote: "+line)
				break
			}
			host := fields[0]
			if strings.ContainsAny(host, "[]") {
				degrade(lineNo, "host range patterns are unsupported in the preview: "+host)
				break
			}
			if !ValidName(host) {
				degrade(lineNo, "host name has an unsupported character: "+host)
				break
			}
			if !connSeen[host] {
				connSeen[host] = true
				p.ConnHosts = append(p.ConnHosts, host)
			}
			if curGroup != "" {
				g := p.Groups[curGroup]
				g.Hosts = append(g.Hosts, host)
				p.Groups[curGroup] = g
			}
			for _, tok := range fields[1:] {
				k, v, ok := parseKV(tok)
				if !ok {
					degrade(lineNo, "non key=value token after host: "+tok)
					break
				}
				if p.HostVars[host] == nil {
					p.HostVars[host] = map[string]string{}
				}
				p.HostVars[host][k] = v
			}
		}
		if p.PreviewUnavailable {
			break
		}
	}

	// Do not persist a half-parsed tree (§4.2): keep Hosts (naive, complete) but
	// discard the partial group/host-var projection.
	if p.PreviewUnavailable {
		p.Groups = map[string]GroupProjection{}
		p.HostVars = map[string]map[string]string{}
	}
	return p
}

// stripComment removes a trailing `#` or `;` comment that is OUTSIDE quotes (so a
// `#` inside a quoted value or a `'env'` lookup is preserved). A full-line
// comment becomes "".
func stripComment(line string) string {
	inS, inD := false, false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\'':
			if !inD {
				inS = !inS
			}
		case '"':
			if !inS {
				inD = !inD
			}
		case '#', ';':
			if !inS && !inD {
				return line[:i]
			}
		}
	}
	return line
}

// tokenizeFields splits a line on unquoted whitespace, keeping quotes inside each
// token (so `key="a b"` is one token). Quote-aware so an inline var value with
// spaces — including the documented `"{{ lookup('env','X') }}"` form — survives.
// balanced is false when the line ends with an open quote: callers MUST treat an
// unbalanced line as out-of-subset (degrade-loudly), because an unterminated
// quote swallows the rest of the line and would otherwise mask a bare-flag token
// that should have triggered a degrade.
func tokenizeFields(line string) (toks []string, balanced bool) {
	var cur strings.Builder
	inS, inD := false, false
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case ch == '\'' && !inD:
			inS = !inS
			cur.WriteByte(ch)
		case ch == '"' && !inS:
			inD = !inD
			cur.WriteByte(ch)
		case (ch == ' ' || ch == '\t') && !inS && !inD:
			flush()
		default:
			cur.WriteByte(ch)
		}
	}
	flush()
	return toks, !inS && !inD
}

// parseKV splits a `key=value` token at the first `=`, stripping one layer of
// surrounding matching quotes from the value. ok is false when there is no `=`
// (a bare flag) — the caller treats that as out-of-subset.
func parseKV(tok string) (key, val string, ok bool) {
	idx := strings.IndexByte(tok, '=')
	if idx <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(tok[:idx])
	val = strings.TrimSpace(tok[idx+1:])
	if len(val) >= 2 {
		if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
	}
	if key == "" {
		return "", "", false
	}
	return key, val, true
}

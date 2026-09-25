package runref

import (
	"regexp"
	"sort"

	"github.com/ResetSmith/cronomicon/internal/envref"
)

// refScanRe matches a derived reference token — one of the reserved prefixes
// followed by a POSIX identifier — anywhere in a script body or inventory. The
// reserved prefix is exactly what makes discovery precise (namespace plan W5): a
// bare env name is indistinguishable from any other identifier, but CRONOMICON_VAR_X
// can only be a reference. The leading \b prevents matching a reference embedded
// in a longer identifier (e.g. MY_CRONOMICON_SECRET_X must NOT suggest a binding X).
var refScanRe = regexp.MustCompile(`\bCRONOMICON_(?:SECRET|VAR|KEY)_[A-Za-z_][A-Za-z0-9_]*`)

// ScanBody extracts the reference bindings a script body declares by naming them
// in derived form (CRONOMICON_SECRET_X, …). Deduped, sorted (kind, name). This is the
// W5 scanner handoff feeding D2 explicit binding: the suggestions prefill a
// job/script's binding set.
func ScanBody(body string) []Binding {
	return scanTokens(refScanRe.FindAllString(body, -1))
}

func scanTokens(tokens []string) []Binding {
	seen := map[string]bool{}
	var out []Binding
	for _, tok := range tokens {
		section, bare, ok := envref.Split(tok)
		if !ok {
			continue
		}
		kind, ok := KindForSection(section)
		if !ok {
			continue // CRONOMICON_RUN_* is dispatcher context, not a binding
		}
		key := string(kind) + "\x00" + bare
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Binding{Kind: kind, Name: bare, Reference: kind.Reference(bare)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// bareWordRe matches a standalone POSIX-identifier token, for the migration lint.
// Alone it is far too noisy (it matches every word), so LintBareNames only keeps
// tokens that collide with a KNOWN row name — those are reference sites still on
// the bare-name fallback chain that an operator should migrate to the derived
// form.
var bareWordRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// LintBareNames flags bare tokens in body that match a known Env Vars row name
// (known maps bare name → the row's kind). Each hit is a reference site not yet
// migrated to the derived CRONOMICON_<SECTION>_<name> form; the returned Binding
// carries the derived reference the operator should switch to. Deduped, sorted.
// A token already written in derived form is skipped (it is not "bare").
func LintBareNames(body string, known map[string]Kind) []Binding {
	seen := map[string]bool{}
	var out []Binding
	for _, tok := range bareWordRe.FindAllString(body, -1) {
		if envref.HasCronomiconPrefix(tok) {
			continue // already a namespaced reference, not a bare site
		}
		kind, ok := known[tok]
		if !ok {
			continue
		}
		key := string(kind) + "\x00" + tok
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Binding{Kind: kind, Name: tok, Reference: kind.Reference(tok)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

package auditlog

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// C1 (2.5.0) — every activity row says whose it is: the agency of its object,
// never the agency the request was viewing (the owner's answer to question 74).
// WriteActivity works it out from a run, a workflow run, a job or a workflow
// the row names; a row with none of those keys must name its agency. So every
// ActivityParams literal outside tests sets AgencyID, TraceID, JobUID or
// WorkflowUID, either in the literal or by assigning the field on the variable
// it was built into, in the same function. (The change log's writers take the
// agency as an argument, so the compiler holds those.)
//
// If you are here because this failed: name the object's agency (AgencyID;
// agencyid.Global for an installation-wide change), or the key it is found by.
var agencyKeys = map[string]bool{"AgencyID": true, "TraceID": true, "JobUID": true, "WorkflowUID": true}

func TestEveryActivityRowNamesItsAgencyOrItsObject(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	fset := token.NewFileSet()
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				// Fields assigned on a variable after its literal: p.TraceID = …
				assigned := map[string]map[string]bool{}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					as, ok := n.(*ast.AssignStmt)
					if !ok {
						return true
					}
					for _, lhs := range as.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok {
							if id, ok := sel.X.(*ast.Ident); ok {
								if assigned[id.Name] == nil {
									assigned[id.Name] = map[string]bool{}
								}
								assigned[id.Name][sel.Sel.Name] = true
							}
						}
					}
					return true
				})
				// The variable each literal was built into, if any.
				into := map[*ast.CompositeLit]string{}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch s := n.(type) {
					case *ast.AssignStmt:
						for i, rhs := range s.Rhs {
							if cl, ok := rhs.(*ast.CompositeLit); ok && i < len(s.Lhs) {
								if id, ok := s.Lhs[i].(*ast.Ident); ok {
									into[cl] = id.Name
								}
							}
						}
					case *ast.ValueSpec:
						for i, v := range s.Values {
							if cl, ok := v.(*ast.CompositeLit); ok && i < len(s.Names) {
								into[cl] = s.Names[i].Name
							}
						}
					}
					return true
				})
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					cl, ok := n.(*ast.CompositeLit)
					if !ok || !isActivityParams(cl.Type) {
						return true
					}
					for _, el := range cl.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok && agencyKeys[k.Name] {
								return true
							}
						}
					}
					if v := into[cl]; v != "" {
						for k := range agencyKeys {
							if assigned[v][k] {
								return true
							}
						}
					}
					rel, _ := filepath.Rel(root, path)
					offenders = append(offenders, filepath.ToSlash(rel)+":"+itoa(fset.Position(cl.Pos()).Line)+" ("+fn.Name.Name+")")
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(offenders)
	if len(offenders) != 0 {
		t.Errorf("an activity row that names neither its agency nor a run, workflow run, job or workflow (C1, question 74):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// isActivityParams reports whether a literal's type is ActivityParams, from
// this package or through an alias (workflow.ActivityParams).
func isActivityParams(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "ActivityParams"
	case *ast.SelectorExpr:
		return t.Sel.Name == "ActivityParams"
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

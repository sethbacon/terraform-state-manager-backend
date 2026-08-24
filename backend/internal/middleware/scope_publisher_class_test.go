package middleware_test

// scope_publisher_class_test.go is the re-runnable signature for issue #476.
//
// The gin key "scopes" is what RequireScope reads, and the literal element
// "admin" in it is a grant-all wildcard that tenantscope turns into
// cross-organization reach. So every place that writes that key is deciding who
// is a platform administrator.
//
// Three of the four credential classes were already governed: sessions through
// platformadmin.Service.SessionScopes, API keys stripped by KeyScopes. mTLS was
// not — it published a subject→scope mapping's slice verbatim, so an `admin`
// written into a config file produced a platform administrator with no grant
// record, no audit entry, and no revocation short of a restart.
//
// This test stops a fourth publisher arriving ungoverned. It does not check that
// the scopes are CORRECT; it checks that the value published was produced by
// something whose job is to answer the authority question, rather than read from
// a token, a config file, or a database row and forwarded.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// governingFuncs answer "what authority does this principal actually have, right
// now?". Each either consults the carrier or removes `admin` outright.
//
// Adding a name here is a claim that it cannot return an unearned `admin`, and
// that claim has to survive review.
var governingFuncs = map[string]bool{
	"SessionScopes":     true, // carrier lookup; additive by design for the legacy session union
	"CertificateScopes": true, // carrier lookup, STRICT — no additive re-add (#476)
	"KeyScopes":         true, // strips `admin` unconditionally; an API key is never elevated
	"elevate":           true, // middleware's wrapper over SessionScopes
	"certificateScopes": true, // mTLS: carrier for a mapping naming a user, KeyScopes otherwise
}

// publishersGovernedByTheirCallers publish a PARAMETER rather than a value they
// computed, so the governing call is one frame up.
//
// An entry here is not an exemption. TestPublishersGovernedByCallersReallyAre
// checks every call site of each listed function and fails if any passes a value
// its own function did not govern — which is the check that makes listing one
// safe, and the reason this map is not simply a list of names to skip.
var publishersGovernedByTheirCallers = map[string]string{
	"setAuthContext": "publishes the `scopes` parameter its callers computed with elevate()",
}

const minimumPublishers = 3

func governingCallIn(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if found {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if governingFuncs[fn.Name] {
				found = true
			}
		case *ast.SelectorExpr:
			if governingFuncs[fn.Sel.Name] {
				found = true
			}
		}
		return !found
	})
	return found
}

// assignedFromGoverningCall reports whether ident was assigned from a governing
// call anywhere within fn.
func assignedFromGoverningCall(fn ast.Node, ident string) bool {
	ok := false
	ast.Inspect(fn, func(node ast.Node) bool {
		assign, isAssign := node.(*ast.AssignStmt)
		if !isAssign {
			return true
		}
		for _, lhs := range assign.Lhs {
			id, isIdent := lhs.(*ast.Ident)
			if !isIdent || id.Name != ident {
				continue
			}
			for _, rhs := range assign.Rhs {
				if governingCallIn(rhs) {
					ok = true
				}
			}
		}
		return true
	})
	return ok
}

type publisher struct {
	file string
	line int
	fn   string
}

func walkGo(t *testing.T, root string, visit func(path string, file *ast.File, fset *token.FileSet)) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		visit(path, f, fset)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

func TestEveryScopePublisherIsGoverned(t *testing.T) {
	root := repoRootForScopeSweep(t)
	var governed, ungoverned []publisher

	walkGo(t, filepath.Join(root, "internal"), func(path string, file *ast.File, fset *token.FileSet) {
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall || len(call.Args) != 2 {
					return true
				}
				sel, isSel := call.Fun.(*ast.SelectorExpr)
				if !isSel || sel.Sel.Name != "Set" {
					return true
				}
				key, isLit := call.Args[0].(*ast.BasicLit)
				if !isLit || key.Kind != token.STRING || strings.Trim(key.Value, `"`) != "scopes" {
					return true
				}

				p := publisher{file: rel, line: fset.Position(call.Pos()).Line, fn: fn.Name.Name}

				if _, listed := publishersGovernedByTheirCallers[fn.Name.Name]; listed {
					governed = append(governed, p)
					return true
				}
				if governingCallIn(call.Args[1]) {
					governed = append(governed, p)
					return true
				}
				if id, isIdent := call.Args[1].(*ast.Ident); isIdent && assignedFromGoverningCall(fn.Body, id.Name) {
					governed = append(governed, p)
					return true
				}
				ungoverned = append(ungoverned, p)
				return true
			})
		}
	})

	if total := len(governed) + len(ungoverned); total < minimumPublishers {
		t.Fatalf("found only %d publisher(s) of the \"scopes\" key, expected at least %d.\n"+
			"That is not a tidy estate, it is a blind matcher: the key was renamed, or the walk "+
			"stopped reaching the middleware. Fix this test before trusting its green.", total, minimumPublishers)
	}

	for _, p := range ungoverned {
		t.Errorf("%s:%d (%s) publishes the \"scopes\" gin key with a value no governing function produced.\n"+
			"That key is what RequireScope reads, and `admin` in it is a grant-all wildcard — so this "+
			"line decides who is a platform administrator. Route the value through SessionScopes, "+
			"CertificateScopes or KeyScopes.\n"+
			"This is issue #476: mTLS published a config file's slice verbatim while every other "+
			"credential class went through the carrier.", p.file, p.line, p.fn)
	}
	for _, p := range governed {
		t.Logf("governed: %s:%d (%s)", p.file, p.line, p.fn)
	}
}

// An entry in publishersGovernedByTheirCallers claims the governing call is one
// frame up. This checks it, so the map cannot become a place to make a publisher
// stop being reported.
func TestPublishersGovernedByCallersReallyAre(t *testing.T) {
	root := repoRootForScopeSweep(t)
	checked := 0

	walkGo(t, filepath.Join(root, "internal"), func(path string, file *ast.File, fset *token.FileSet) {
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		for _, decl := range file.Decls {
			caller, ok := decl.(*ast.FuncDecl)
			if !ok || caller.Body == nil {
				continue
			}
			ast.Inspect(caller.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				ident, isIdent := call.Fun.(*ast.Ident)
				if !isIdent {
					return true
				}
				if _, listed := publishersGovernedByTheirCallers[ident.Name]; !listed {
					return true
				}
				checked++

				// Every argument that is a plain identifier must have been
				// governed here, or be one this caller itself received governed.
				anyGoverned := false
				for _, arg := range call.Args {
					if id, ok := arg.(*ast.Ident); ok && assignedFromGoverningCall(caller.Body, id.Name) {
						anyGoverned = true
					}
					if governingCallIn(arg) {
						anyGoverned = true
					}
				}
				if !anyGoverned {
					t.Errorf("%s:%d %s calls %s but governs none of the scopes it passes.\n"+
						"%s is listed in publishersGovernedByTheirCallers on the claim that its "+
						"callers do the governing; this call site does not, so the claim is false "+
						"here and the published scopes are whatever the token said.",
						rel, fset.Position(call.Pos()).Line, caller.Name.Name, ident.Name, ident.Name)
				}
				return true
			})
		}
	})

	if checked == 0 {
		t.Fatalf("no call sites found for any of %v.\n"+
			"Either those functions are gone — remove them from the map — or this check stopped "+
			"finding calls, in which case the map is an unchecked exemption list.",
			keysOf(publishersGovernedByTheirCallers))
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func repoRootForScopeSweep(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the module root")
	return ""
}

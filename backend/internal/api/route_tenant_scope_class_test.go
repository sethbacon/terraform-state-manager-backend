package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A CLASS GUARD over route wiring: a handler that needs a tenant scope must be
// registered on a route that resolves one.
//
// This is the guard whose absence let a whole class of cross-tenant WRITE exist.
// #436 stamped every INSERT and the read side is scoped separately, but the
// mutating routes were never examined: DELETE /sources/:id had no
// middleware.TenantScope, so its handler had nothing to check ownership against
// and its repository deleted by id alone. A caller holding sources:manage in ANY
// organization could destroy another organization's source, and state_sources
// cascades to eight dependent tables.
//
// Nothing reported it, because nothing was looking at the join between "this
// handler reads a scope" and "this route resolves one". That join is what this
// checks.
//
// It reads the ROUTER as text rather than as an AST because what matters is the
// registration line: which middleware sits between RequireScope and the handler.

var (
	routeLine   = regexp.MustCompile(`(?m)^\s*[a-zA-Z_][\w]*\.(GET|POST|PUT|DELETE|PATCH)\(`)
	handlerCall = regexp.MustCompile(`([a-zA-Z_][\w]*)\.([A-Z][\w]*)\(\)`)
)

// scopeAwareHandlers finds every exported handler method in this package whose
// body resolves a tenant scope — via tenantscope.FromContext or the
// actingOrganization helper that wraps it.
//
// AST, not text slicing. A first version bounded each function at the next
// `func (h *X) Y() gin.HandlerFunc` and so swept in any plain helper declared
// between two handlers — which reported GetTransfer as scope-aware because
// transferEndpointsReachable sits below it. A false positive here is not
// harmless: it teaches whoever reads the failure that the guard is noisy.
//
// Matching is by METHOD NAME, which is deliberately conservative: two handler
// types both exporting CreateRun means BOTH routes must be scoped before this
// passes. Over-requiring a scope is safe; under-requiring one is the bug.
func scopeAwareHandlers(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := map[string]string{}
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || fn.Body == nil || !fn.Name.IsExported() {
					continue
				}
				resolves := false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch f := call.Fun.(type) {
					case *ast.SelectorExpr:
						if id, ok := f.X.(*ast.Ident); ok && id.Name == "tenantscope" && f.Sel.Name == "FromContext" {
							resolves = true
						}
					case *ast.Ident:
						if f.Name == "actingOrganization" {
							resolves = true
						}
					}
					return true
				})
				if resolves {
					out[fn.Name.Name] = filepath.Base(path)
				}
			}
		}
	}
	return out
}

func TestEveryScopeAwareHandlerIsRoutedWithTenantScope(t *testing.T) {
	need := scopeAwareHandlers(t)
	if len(need) == 0 {
		t.Fatal("no scope-aware handlers found: this scan is looking at the wrong thing, " +
			"and an empty enumeration passes for free")
	}

	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	// Registrations are one per line in this router; a continuation would show up
	// as a handler with no matching route, which fails loudly rather than silently.
	lines := strings.Split(string(src), "\n")

	routed := map[string]bool{}
	var unscoped []string
	for _, line := range lines {
		if !routeLine.MatchString(line) {
			continue
		}
		for _, m := range handlerCall.FindAllStringSubmatch(line, -1) {
			name := m[2]
			if need[name] == "" {
				continue
			}
			routed[name] = true
			if !strings.Contains(line, "tenantScope") {
				unscoped = append(unscoped, name+" ("+need[name]+")")
			}
		}
	}

	sort.Strings(unscoped)
	for _, h := range unscoped {
		t.Errorf("%s resolves a tenant scope but its route registers no middleware.TenantScope.\n"+
			"The handler will treat the missing scope as a wiring fault and 500 — or, worse, if it "+
			"ever stops doing that, fall back to an unscoped statement. This is the shape that let "+
			"DELETE /sources/:id destroy another organization's source.", h)
	}

	var unrouted []string
	for name, file := range need {
		if !routed[name] {
			unrouted = append(unrouted, name+" ("+file+")")
		}
	}
	sort.Strings(unrouted)
	for _, h := range unrouted {
		t.Errorf("%s resolves a tenant scope but no route registration in router.go mentions it. "+
			"Either it is dead, or it is registered in a form this scan cannot see — in which case "+
			"the scan is blind for that route and must be taught to read it.", h)
	}
}

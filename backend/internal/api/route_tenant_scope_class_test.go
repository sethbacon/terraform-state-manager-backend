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
// body resolves a tenant scope — directly via tenantscope.FromContext, or
// through a package-local helper that does.
//
// AST, not text slicing. A first version bounded each function at the next
// `func (h *X) Y() gin.HandlerFunc` and so swept in any plain helper declared
// between two handlers — which reported GetTransfer as scope-aware because
// transferEndpointsReachable sits below it. A false positive here is not
// harmless: it teaches whoever reads the failure that the guard is noisy.
//
// # The indirection is DERIVED, not listed, and that is the second thing this
// scan got wrong
//
// It used to recognise exactly two things: the literal tenantscope.FromContext
// call, and one helper named in the code here — `actingOrganization`. A helper
// is precisely what a handler reaches for once three of them need the same four
// lines, so the list was one refactor away from going quiet: the
// notification-channel flip extracted `channelScope`, and four handlers that
// resolve a scope through it would have been invisible to a guard that was
// looking for a different spelling.
//
// So the resolvers are computed to a FIXPOINT over this package's own call
// graph: a function resolves a scope if it calls tenantscope.FromContext, or if
// it calls something that does. `actingOrganization` is no longer named here —
// it is found, because it calls FromContext, which is the property that made it
// worth naming in the first place.
//
// Matching is by FUNCTION NAME throughout, which is deliberately conservative:
// two handler types both exporting CreateRun means BOTH routes must be scoped
// before this passes. Over-requiring a scope is safe; under-requiring one is the
// bug.
func scopeAwareHandlers(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	type decl struct {
		fn     *ast.FuncDecl
		file   string
		method bool
	}
	var decls []decl
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			for _, d := range file.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				decls = append(decls, decl{fn: fn, file: filepath.Base(path), method: fn.Recv != nil})
			}
		}
	}

	// calleeNames returns the package-local names a body invokes: bare calls
	// (helpers) and selector calls whose receiver is a local identifier (h.foo()).
	// A selector on a PACKAGE — tenantscope.FromContext — is handled separately,
	// and is what seeds the fixpoint.
	calleeNames := func(fn *ast.FuncDecl) (direct bool, callees []string) {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := call.Fun.(type) {
			case *ast.SelectorExpr:
				id, ok := f.X.(*ast.Ident)
				if !ok {
					return true
				}
				if id.Name == "tenantscope" && f.Sel.Name == "FromContext" {
					direct = true
					return true
				}
				callees = append(callees, f.Sel.Name)
			case *ast.Ident:
				callees = append(callees, f.Name)
			}
			return true
		})
		return direct, callees
	}

	resolves := map[string]bool{}
	edges := map[string][]string{}
	for _, d := range decls {
		direct, callees := calleeNames(d.fn)
		if direct {
			resolves[d.fn.Name.Name] = true
		}
		edges[d.fn.Name.Name] = append(edges[d.fn.Name.Name], callees...)
	}
	if len(resolves) == 0 {
		t.Fatal("nothing in this package calls tenantscope.FromContext, so the fixpoint below " +
			"starts empty and every handler passes for free. The scan is looking at the wrong " +
			"package, or the resolver was renamed.")
	}
	for changed := true; changed; {
		changed = false
		for name, callees := range edges {
			if resolves[name] {
				continue
			}
			for _, callee := range callees {
				if resolves[callee] {
					resolves[name] = true
					changed = true
					break
				}
			}
		}
	}

	out := map[string]string{}
	for _, d := range decls {
		if d.method && d.fn.Name.IsExported() && resolves[d.fn.Name.Name] {
			out[d.fn.Name.Name] = d.file
		}
	}
	return out
}

// TestTheScopeAwareScanSeesAnIndirectResolver asserts the fixpoint POSITIVELY,
// because a scan that resolved nothing indirectly and a tree with no indirect
// resolvers produce the same green.
//
// The canaries are the two shapes the direct-only version missed: a bare helper
// call (`channelScope`, extracted by the notification-channel flip) and a method
// call on the receiver (`sourceInScope`). Neither mentions tenantscope itself.
func TestTheScopeAwareScanSeesAnIndirectResolver(t *testing.T) {
	need := scopeAwareHandlers(t)
	for _, tc := range []struct{ handler, via string }{
		{"ListChannels", "channelScope"},
		{"StateHistory", "sourceInScope"},
	} {
		if need[tc.handler] == "" {
			t.Errorf("%s resolves a tenant scope through %s and the scan did not see it. "+
				"The scan is direct-only again, and every handler that reaches its scope "+
				"through a helper is invisible to the route check below.", tc.handler, tc.via)
		}
	}
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

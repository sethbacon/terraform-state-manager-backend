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

// THE INVERSE OF route_tenant_scope_class_test.go, and the reason that guard was
// not enough on its own.
//
// That one asks: does every handler that READS a tenant scope sit on a route
// that resolves one? It is a real check and it found a live bug. But it can only
// see handlers that already read a scope — and the handlers that leak are
// precisely the ones that do NOT. A handler calling `h.repo.GetByID(ctx, id)`
// with no scope anywhere is invisible to it, and passes.
//
// That is the blind-versus-clean failure: the guard reported success because it
// was not looking, and nothing distinguished that from being looking and finding
// nothing.
//
// This asks the opposite question, and derives the universe rather than
// transcribing it: for every repository method that HAS an *InScope twin, is any
// handler still calling the unscoped one? A twin existing is the repository's own
// statement that this read has a tenant-aware form. Calling the other one is then
// a decision, and it has to be written down.

// justifiedUnscoped records call sites that deliberately use the unscoped twin.
//
// An entry is a claim that this site has NO caller to scope by, or that it is
// comparing the two reads on purpose. "It was awkward to thread" is not a reason.
// The map is keyed "file:function".
var justifiedUnscoped = map[string]string{
	"tenant_dualread.go:observeSourceListScope":        "the dual-read measurement itself: it runs BOTH reads and reports where they disagree, so the unscoped one is half the instrument",
	"tenant_dualread.go:observeSourceGetScope":         "as observeSourceListScope",
	"tenant_dualread_analysis.go:observeAnalysisScope": "as observeSourceListScope, for the inherited analysis tables",
	"sources.go:ListSources":                           "Phase 2b: serves the unscoped answer and observes the scoped one beside it. Flipping this is Phase 3 and is gated on the estate having been re-owned",
	"sources.go:GetSource":                             "as ListSources",
	"reports.go:ReportStates":                          "PENDING (#455/#459): the scoped twin exists and is unused. Listed so the count is honest rather than absent",
	"reports.go:ReportStatesExport":                    "as ReportStates",
	"dashboard.go:DashboardOverview":                   "PENDING (#455/#459): scoped twins exist; the flip is gated on the re-own. observeAnalysisScope already measures the gap",
	"dashboard.go:StatesByVersion":                     "PENDING (#455/#459): as DashboardOverview, and with no observation yet",
	"dashboard.go:overviewAggregates":                  "PENDING (#455/#459): the cached aggregate behind DashboardOverview",
	"reconcile.go:ReconcileSources":                    "the statesync reconcile loop reads the whole fleet by design: it syncs every tenant's sources and has no caller to narrow to",
	"sources.go:Consumers":                             "#439: a suite service-token route with no principal at all. It needs an organization PARAMETER, not a caller scope",
}

// repoFieldTypes maps HANDLER STRUCT -> field name -> repository type, so a call
// can be attributed to the right repository.
//
// KEYED BY STRUCT, and that is not incidental. A first version keyed on field
// name alone, and `repo` is a field on SourcesHandlers (*SourceRepository), on
// NotificationHandlers (*NotificationChannelRepository) and others. Whichever
// parsed last won, so SourcesHandlers.repo resolved to the wrong type and every
// unscoped call in the state plane was silently skipped — the guard reported one
// offender when there were many, which is the blind-versus-clean failure a third
// time in this file's own lineage.
func repoFieldTypes(t *testing.T, files map[string]*ast.File) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok || st.Fields == nil {
					continue
				}
				if out[ts.Name.Name] == nil {
					out[ts.Name.Name] = map[string]string{}
				}
				for _, f := range st.Fields.List {
					star, ok := f.Type.(*ast.StarExpr)
					if !ok {
						continue
					}
					sel, ok := star.X.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "repositories" {
						continue
					}
					for _, n := range f.Names {
						out[ts.Name.Name][n.Name] = sel.Sel.Name
					}
				}
			}
		}
	}
	return out
}

// scopedTwins derives, from the repositories package, every (type, method) that
// has an *InScope sibling.
func scopedTwins(t *testing.T) map[string]bool {
	t.Helper()
	dir := filepath.Join("..", "db", "repositories")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read repositories: %v", err)
	}
	decl := regexp.MustCompile(`func \(r \*(\w+)\) (\w+)\(`)
	have, scoped := map[string]bool{}, map[string]bool{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range decl.FindAllStringSubmatch(string(src), -1) {
			recv, name := m[1], m[2]
			have[recv+"."+name] = true
			if strings.HasSuffix(name, "InScope") {
				scoped[recv+"."+strings.TrimSuffix(name, "InScope")] = true
			}
		}
	}
	out := map[string]bool{}
	for k := range scoped {
		if have[k] {
			out[k] = true
		}
	}
	return out
}

func TestNoHandlerCallsAnUnscopedTwinWithoutSayingWhy(t *testing.T) {
	twins := scopedTwins(t)
	if len(twins) == 0 {
		t.Fatal("no scoped twins derived: the scan is looking at the wrong package, and an " +
			"empty universe passes for free")
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var files map[string]*ast.File
	for _, p := range pkgs {
		files = p.Files
	}
	fieldType := repoFieldTypes(t, files)

	var offenders []string
	for path, file := range files {
		base := filepath.Base(path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := base + ":" + fn.Name.Name
			if justifiedUnscoped[key] != "" {
				continue
			}
			// The receiver's type selects which field map applies.
			recvType := ""
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				if star, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
					if id, ok := star.X.(*ast.Ident); ok {
						recvType = id.Name
					}
				}
			}
			fields := fieldType[recvType]
			if fields == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				inner, ok := sel.X.(*ast.SelectorExpr) // h.<field>.<Method>
				if !ok {
					return true
				}
				typ := fields[inner.Sel.Name]
				if typ == "" || !twins[typ+"."+sel.Sel.Name] {
					return true
				}
				offenders = append(offenders,
					key+" calls "+typ+"."+sel.Sel.Name+" (twin: "+sel.Sel.Name+"InScope)")
				return true
			})
		}
	}

	sort.Strings(offenders)
	seen := map[string]bool{}
	for _, o := range offenders {
		if seen[o] {
			continue
		}
		seen[o] = true
		t.Errorf("%s.\n"+
			"A scoped twin exists, which is the repository stating that this read has a "+
			"tenant-aware form. Use it, or add the site to justifiedUnscoped with the reason it "+
			"has no caller to scope by. Note that route_tenant_scope_class_test.go CANNOT see "+
			"this: that guard only inspects handlers which already read a scope, and this one "+
			"does not.", o)
	}
}

// TestJustifiedUnscopedEntriesAreLive keeps the allowlist from rotting into a
// list nobody re-examines. An entry naming a function that no longer exists is a
// carve-out with nothing under it.
// TestTheScanCanAttributeAField asserts the resolution step POSITIVELY.
//
// Without it, defeating repoFieldTypes is undetectable: the guard finds no
// offenders, and with the tree clean that is also what success looks like.
// "Not looking" and "looking and finding nothing" produce the same green.
//
// The canary is deliberately a field name that COLLIDES across handler structs —
// `repo` is a *SourceRepository on SourcesHandlers and a
// *NotificationChannelRepository on NotificationHandlers. Resolving it correctly
// is exactly what the first version of this guard got wrong, and it under-reported
// six offenders as a result.
func TestTheScanCanAttributeAField(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var files map[string]*ast.File
	for _, p := range pkgs {
		files = p.Files
	}
	fieldType := repoFieldTypes(t, files)

	for _, tc := range []struct{ recv, field, want string }{
		{"SourcesHandlers", "repo", "SourceRepository"},
		{"SourcesHandlers", "analysisRepo", "StateAnalysisRepository"},
		{"NotificationHandlers", "repo", "NotificationChannelRepository"},
	} {
		if got := fieldType[tc.recv][tc.field]; got != tc.want {
			t.Errorf("%s.%s resolved to %q, want %q. The scan cannot attribute a field to its "+
				"repository, so every call through it is invisible and this guard is reporting "+
				"a clean tree it never read.", tc.recv, tc.field, got, tc.want)
		}
	}
}

func TestJustifiedUnscopedEntriesAreLive(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	live := map[string]bool{}
	for _, p := range pkgs {
		for path, file := range p.Files {
			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok {
					live[filepath.Base(path)+":"+fn.Name.Name] = true
				}
			}
		}
	}
	var stale []string
	for k := range justifiedUnscoped {
		if !live[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		t.Errorf("justifiedUnscoped names %s, which no longer exists. Remove it: an exemption "+
			"for a site that is gone is a carve-out waiting to absorb an unrelated one.", k)
	}
}

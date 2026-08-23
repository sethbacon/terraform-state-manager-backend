package repositories

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// Every scoped reader must open with the same two branches, and this is a
// STRUCTURAL guard because the first of them cannot be tested behaviourally.
//
// Deleting `if scope.Empty() { return nothing }` changes no observable output:
// the query then runs with an empty id list, `organization_id = ANY('{}')`
// matches nothing, and the caller gets the same empty result. source_repository.go
// says exactly this about its own copy -- the early return "is not an
// optimisation ... it is here so that the 'reads nothing' answer does not depend
// on a Postgres subtlety that a later edit could change by accident."
//
// A behavioural test therefore passes with the guard removed; I confirmed that
// by removing it. The property is that the code SAYS it, so the code is what
// gets checked.
//
// The PlatformAdmin branch is included for the opposite reason: it IS
// behaviourally testable, and is asserted in the integration suite. Checking the
// shape here as well means a new reader that forgets it fails immediately rather
// than at whatever point someone writes its integration test.
func TestEveryScopedReaderHandlesEmptyAndPlatformAdmin(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	checked := 0
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || !strings.HasSuffix(fn.Name.Name, "InScope") || fn.Body == nil {
					continue
				}
				checked++
				src := renderBody(fset, fn)
				// A method may DELEGATE both decisions to scopeWrite, which folds
				// a Scope into the three outcomes a mutating statement has. That
				// is better than duplicating the branches, so it satisfies the
				// requirement — but only because scopeWrite is itself checked,
				// below. A delegate that did not do the work would otherwise be a
				// way to satisfy this guard without satisfying its point.
				if strings.Contains(src, "scopeWrite()") {
					continue
				}
				if !strings.Contains(src, "scope.Empty()") {
					t.Errorf("%s: %s has no scope.Empty() branch.\n"+
						"Without it a caller whose tenancy could not be established runs the query "+
						"with an empty id list. That happens to return nothing today, which is why "+
						"no behavioural test can catch this -- and why it must be stated in the code "+
						"rather than left to depend on how `= ANY('{}')` evaluates.", path, fn.Name.Name)
				}
				if !strings.Contains(src, "scope.PlatformAdmin") {
					t.Errorf("%s: %s has no scope.PlatformAdmin branch. A platform admin reads "+
						"unfiltered, including rows whose organization_id is still NULL, which the "+
						"organization predicate cannot match.", path, fn.Name.Name)
				}
			}
		}
	}

	// A floor, not a non-zero check: this package holds well over a dozen scoped
	// readers, and a scan that silently matched two of them would look identical
	// to a clean one.
	const minScopedReaders = 8
	if checked < minScopedReaders {
		t.Fatalf("only %d scoped reader(s) found; expected at least %d. The scan is not "+
			"seeing the methods it exists to check.", checked, minScopedReaders)
	}
}

// TestScopeWriteHandlesBothEnds is what makes delegating to scopeWrite
// acceptable above. Without it, "calls scopeWrite" would be an escape hatch
// rather than a shorthand.
func TestScopeWriteHandlesBothEnds(t *testing.T) {
	if got := scopeWrite(tenantscope.Scope{PlatformAdmin: true}); !got.Skip {
		t.Errorf("scopeWrite(platform admin) = %+v; want Skip, so the caller runs its "+
			"unscoped statement and can still reach rows whose organization_id is NULL", got)
	}
	if got := scopeWrite(tenantscope.Scope{}); !got.Deny {
		t.Errorf("scopeWrite(empty) = %+v; want Deny. A caller whose tenancy could not be "+
			"established must not run a mutating statement at all — running it with an empty "+
			"id list happens to match nothing today, and that is not a thing to depend on", got)
	}
	orgs := []string{"aaaaaaaa-0000-4000-8000-000000000001"}
	got := scopeWrite(tenantscope.Scope{OrgIDs: orgs})
	if got.Skip || got.Deny || len(got.OrgIDs) != 1 {
		t.Errorf("scopeWrite(one organization) = %+v; want the organization bound", got)
	}
}

func renderBody(fset *token.FileSet, fn *ast.FuncDecl) string {
	var b strings.Builder
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				b.WriteString(id.Name + "." + sel.Sel.Name + "\n")
			}
		}
		if call, ok := n.(*ast.CallExpr); ok {
			switch f := call.Fun.(type) {
			case *ast.SelectorExpr:
				if id, ok := f.X.(*ast.Ident); ok {
					b.WriteString(id.Name + "." + f.Sel.Name + "()\n")
				}
			case *ast.Ident:
				// Bare package-local calls, so a method that DELEGATES the scope
				// decision (scopeWrite) is visible to the check above. Without
				// this branch the render only saw selector expressions, and the
				// delegation looked like an absent decision.
				b.WriteString(f.Name + "()\n")
			}
		}
		return true
	})
	return b.String()
}

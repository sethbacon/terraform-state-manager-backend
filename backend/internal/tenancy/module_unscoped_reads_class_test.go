package tenancy

// EVERY UNSCOPED REPOSITORY READ IN THE MODULE, not just the ones in handlers.
//
// internal/api's unscoped-twin guard parses ONE package. It answers "no HANDLER
// calls an unscoped twin", which is not "nothing does" -- and the difference is
// not hypothetical. Nine packages outside internal/api hold repository fields:
// the scheduler, both reconcilers, statesync, notify, credlifecycle, auth,
// middleware and cmd/server. Every call any of them makes was outside every
// guard in the repository, which is why two fleet-wide reads in statesync were
// justified in a comment and recorded by nothing.
//
// This walks the whole module with the same shape of scan and requires every
// unscoped read OUTSIDE internal/api to be accounted for by name. It does not
// duplicate the handler guard: internal/api is skipped here precisely because
// that package has its own, stricter one.
//
// THE KEY IS PACKAGE-QUALIFIED -- "<dir>/<file>:<func>:<Type>.<Method>" --
// because file basenames repeat across packages and a bare basename would let
// an exemption written for one package silently excuse another's call.

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

// moduleUnscopedReads accounts for every unscoped repository read outside
// internal/api. An entry is a claim that the call is CORRECT unscoped, with the
// reason it cannot be narrowed -- not a note that somebody looked at it.
var moduleUnscopedReads = map[string]string{
	"internal/services/statesync/syncer.go:SyncAll:SourceRepository.List": "" +
		"the fleet-wide reconcile: it walks every tenant's sources by design and has no " +
		"caller to narrow to. The per-source work it dispatches is what carries an " +
		"organization, derived from the source row it enumerated.",
	"internal/services/statesync/syncer.go:SyncSources:SourceRepository.List": "" +
		"the same reconcile reached for a named subset; the List is still fleet-wide " +
		"because the subset is matched against it.",
}

// repositoryReadMethods are the unscoped readers that have an InScope twin. A
// call to one of these outside internal/api must be accounted for.
//
// Derived rather than listed: any exported method whose name has an "InScope"
// sibling on the same type IS the unscoped twin, so the set cannot go stale as
// twins are added -- which is what a hand-written list would do.
func repositoryReadMethods(t *testing.T, root string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, filepath.Join(root, "internal", "db", "repositories"),
		func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatalf("parse repositories: %v", err)
	}
	all := map[string]bool{}
	for _, p := range pkgs {
		for _, file := range p.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
					continue
				}
				var recv string
				switch e := fn.Recv.List[0].Type.(type) {
				case *ast.StarExpr:
					if id, ok := e.X.(*ast.Ident); ok {
						recv = id.Name
					}
				case *ast.Ident:
					recv = e.Name
				}
				if recv != "" {
					all[recv+"."+fn.Name.Name] = true
				}
			}
		}
	}
	twins := map[string]bool{}
	for k := range all {
		if strings.HasSuffix(k, "InScope") {
			twins[strings.TrimSuffix(k, "InScope")] = true
		}
	}
	if len(twins) == 0 {
		t.Fatal("no InScope twins found in internal/db/repositories: the derivation is blind, " +
			"and a blind derivation accounts for nothing while looking exhaustive")
	}
	return twins
}

func TestNothingOutsideTheAPIPackageReadsUnscoped(t *testing.T) {
	root := filepath.Join("..", "..")
	twins := repositoryReadMethods(t, root)

	var offenders []string
	seenKeys := map[string]bool{}
	scanned := 0

	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		// internal/api has its own stricter guard; internal/db is the
		// repositories themselves, where the unscoped readers are DEFINED.
		if rel == filepath.Join("internal", "api") || rel == filepath.Join("internal", "db") {
			return filepath.SkipDir
		}
		fset := token.NewFileSet()
		pkgs, perr := parser.ParseDir(fset, path,
			func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
		if perr != nil || len(pkgs) == 0 {
			return nil
		}
		for _, p := range pkgs {
			// COUNT EVERY FILE THE WALK REACHED, not only those in packages
			// that happen to hold a repository field. The floor is asking
			// "did the walk run", and counting only interesting files makes
			// it answer "were there interesting files" -- which is the
			// question the offender list already answers, and which reads as
			// a collapse when the module is simply clean.
			scanned += len(p.Files)
			fieldType := repoFieldTypesIn(p.Files)
			if len(fieldType) == 0 {
				continue
			}
			for fpath, file := range p.Files {
				keyBase := filepath.ToSlash(filepath.Join(rel, filepath.Base(fpath)))
				for _, decl := range file.Decls {
					fn, ok := decl.(*ast.FuncDecl)
					if !ok || fn.Body == nil || fn.Recv == nil || len(fn.Recv.List) != 1 {
						continue
					}
					var recvType string
					switch e := fn.Recv.List[0].Type.(type) {
					case *ast.StarExpr:
						if id, ok := e.X.(*ast.Ident); ok {
							recvType = id.Name
						}
					case *ast.Ident:
						recvType = e.Name
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
						inner, ok := sel.X.(*ast.SelectorExpr)
						if !ok {
							return true
						}
						typ := fields[inner.Sel.Name]
						if typ == "" || !twins[typ+"."+sel.Sel.Name] {
							return true
						}
						key := keyBase + ":" + fn.Name.Name + ":" + typ + "." + sel.Sel.Name
						seenKeys[key] = true
						if moduleUnscopedReads[key] != "" {
							return true
						}
						offenders = append(offenders, key)
						return true
					})
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// A FLOOR, not a non-zero check. If the walk collapses -- a rename, a moved
	// directory, a parse that silently yields nothing -- an empty offender list
	// reads exactly like a clean module. The module has well over this many
	// non-test files outside internal/api.
	const minScanned = 40
	if scanned < minScanned {
		t.Fatalf("scanned only %d file(s) outside internal/api: the walk has collapsed, and a "+
			"blind scan is indistinguishable from a clean one", scanned)
	}

	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("%s calls an unscoped repository reader outside internal/api, where no other "+
			"guard looks.\nEither call the InScope twin, or add the key to moduleUnscopedReads "+
			"with the reason the read CANNOT be narrowed -- a background sweep with no caller, or "+
			"a lookup that must run before the authority it derives exists.", o)
	}

	// And the accounting must stay live: an entry whose call is gone is a
	// carve-out waiting to absorb an unrelated one, the same failure the
	// handler guard's liveness check was tightened to catch.
	var stale []string
	for k := range moduleUnscopedReads {
		if !seenKeys[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		t.Errorf("moduleUnscopedReads names %s, which makes no such call any more. Remove it.", k)
	}
}

// repoFieldTypesIn maps struct name -> field name -> repository type for one
// package's files. It is the same question internal/api's repoFieldTypes asks,
// asked without a *testing.T so it can run inside a walk.
func repoFieldTypesIn(files map[string]*ast.File) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
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
				for _, f := range st.Fields.List {
					star, ok := f.Type.(*ast.StarExpr)
					if !ok {
						continue
					}
					selx, ok := star.X.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					pkgIdent, ok := selx.X.(*ast.Ident)
					if !ok || pkgIdent.Name != "repositories" {
						continue
					}
					for _, name := range f.Names {
						if out[ts.Name.Name] == nil {
							out[ts.Name.Name] = map[string]string{}
						}
						out[ts.Name.Name][name.Name] = selx.Sel.Name
					}
				}
			}
		}
	}
	return out
}

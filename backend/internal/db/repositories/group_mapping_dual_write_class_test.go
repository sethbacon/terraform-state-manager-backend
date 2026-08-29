package repositories

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

// Class guard for the group-mapping dual-write
// (terraform-suite-identity#206 phase 2, migration 000036).
//
// The property: EVERY path that can change the group-mapping list stored in
// sso_settings.oidc_group_mappings also makes TSM's own group_mappings table
// equal the new list. Nothing reads TSM's table yet, so no behavioural test
// can observe a path that forgets -- the divergence would surface at the read
// cutover, after the rows are already wrong. Same guard family as
// internal/approles/dual_write_class_test.go, and the same rule stands: a
// convention ("remember to mirror") is the weakest possible guard.
//
// The bypass shapes here, checked one test each:
//
//  1. A method on SSOSettingsRepository that writes sso_settings without
//     reaching the mirror -- today Upsert is the only writer, and a second
//     writer added later must mirror too or be classified with a reason.
//  2. Somebody writes sso_settings SQL by hand, outside the repository.
//
// Every test here fails on an empty universe.
//
// # The blind axes this file was written against
//
// A matcher that only reads a method body's string literals goes blind the day
// the SQL moves into a package-level const, and a matcher that spells the
// table name one way goes blind on any other LEGAL spelling -- quoted
// identifiers, a schema qualifier, or both (this repository's own
// partition-root guard was walked past by `INSERT INTO "public"."legal_holds"`
// before it learned the same lesson). So: the SQL text a function can reach is
// its body's literals PLUS the package-level string constants and variables it
// references PLUS the closure through functions it calls
// (ssoWriterReachableSQL), the table regex accepts every legal spelling
// (proven spelling-by-spelling below), and
// TestGroupMappingDualWriteClass_DerivationSeesEveryCarrierRoute proves each
// carrier route is load-bearing over a synthetic universe where the routes do
// not overlap.

// groupMappingMirrorCallNames are the accepted ways a repository method
// reaches TSM's own group_mappings table.
var groupMappingMirrorCallNames = map[string]bool{
	"Replace": true,
}

// ssoSettingsWriteSQL matches a statement that creates, updates or removes the
// sso_settings overlay row, in every legal spelling of the target.
var ssoSettingsWriteSQL = regexp.MustCompile(
	`(?is)(INSERT\s+INTO|UPDATE|DELETE\s+FROM)\s+(?:(?:"[a-z_]+"|[a-z_]+)\s*\.\s*)?(?:"sso_settings"|sso_settings\b)`)

// overlayWritersThatCannotChangeMappingContent classifies repository writers
// whose statements cannot alter the stored mapping LIST, with the reason.
// Checked in both directions: an entry naming a method that is no longer a
// derived writer fails the test. EMPTY TODAY -- Upsert writes the whole row,
// list included -- and that is the intended state; an exemption that stands
// ready to absorb a real miss is worse than none.
var overlayWritersThatCannotChangeMappingContent = map[string]string{}

// gmClassModuleRoot ascends from the test's working directory to go.mod.
func gmClassModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory -- the module root cannot be located")
		}
		dir = parent
	}
}

// gmClassParseDir parses every non-test .go file in dir.
func gmClassParseDir(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		files = append(files, file)
	}
	return files
}

// gmClassMethodsOn returns every method declared on typeName in dir's non-test
// files, keyed by method name.
func gmClassMethodsOn(t *testing.T, dir, typeName string) map[string]*ast.FuncDecl {
	t.Helper()
	out := map[string]*ast.FuncDecl{}
	for _, file := range gmClassParseDir(t, dir) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Body == nil {
				continue
			}
			recv := fn.Recv.List[0].Type
			if star, ok := recv.(*ast.StarExpr); ok {
				recv = star.X
			}
			if id, ok := recv.(*ast.Ident); ok && id.Name == typeName {
				out[fn.Name.Name] = fn
			}
		}
	}
	return out
}

// gmClassPackageFuncs returns every package-level function (no receiver) in
// dir's non-test files, keyed by name.
func gmClassPackageFuncs(t *testing.T, dir string) map[string]*ast.FuncDecl {
	t.Helper()
	out := map[string]*ast.FuncDecl{}
	for _, file := range gmClassParseDir(t, dir) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			out[fn.Name.Name] = fn
		}
	}
	return out
}

// gmClassPackageStrings collects every package-level string constant and
// variable declared in dir's non-test files, keyed by name. Concatenations of
// string literals ("a" + "b") are folded.
func gmClassPackageStrings(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	var foldString func(e ast.Expr) (string, bool)
	foldString = func(e ast.Expr) (string, bool) {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				return v.Value, true
			}
		case *ast.BinaryExpr:
			if v.Op == token.ADD {
				l, lok := foldString(v.X)
				r, rok := foldString(v.Y)
				if lok && rok {
					return l + r, true
				}
			}
		case *ast.ParenExpr:
			return foldString(v.X)
		}
		return "", false
	}
	for _, file := range gmClassParseDir(t, dir) {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if s, ok := foldString(vs.Values[i]); ok {
						out[name.Name] = s
					}
				}
			}
		}
	}
	return out
}

// gmClassCalledNames returns the bare names of everything fn calls -- both
// method selectors (x.Replace(...)) and package functions (helper(...)).
func gmClassCalledNames(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.SelectorExpr:
			out[f.Sel.Name] = true
		case *ast.Ident:
			out[f.Name] = true
		}
		return true
	})
	return out
}

// gmClassReachableSQL renders everything SQL-shaped a function can reach
// lexically: its body's string literals PLUS the values of package-level
// string constants/vars its body references by name. The second half is the
// point -- see the header on the blind axes.
func gmClassReachableSQL(fn *ast.FuncDecl, pkgStrings map[string]string) string {
	var sb strings.Builder
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				sb.WriteString(v.Value)
				sb.WriteString("\n")
			}
		case *ast.Ident:
			if s, ok := pkgStrings[v.Name]; ok {
				sb.WriteString(s)
				sb.WriteString("\n")
			}
		}
		return true
	})
	return sb.String()
}

// gmClassDeriveOverlayWriters derives, from the given directory's own source,
// every method on typeName that writes sso_settings -- in its own reachable
// SQL text, or by calling a package function or method that does.
func gmClassDeriveOverlayWriters(t *testing.T, dir, typeName string) map[string]bool {
	t.Helper()
	methods := gmClassMethodsOn(t, dir, typeName)
	if len(methods) == 0 {
		t.Fatalf("found no methods on %s under %s -- the layout changed and this guard has no universe", typeName, dir)
	}
	funcs := gmClassPackageFuncs(t, dir)
	pkgStrings := gmClassPackageStrings(t, dir)

	// One name space for the closure: methods and package functions together.
	all := map[string]*ast.FuncDecl{}
	for name, fn := range funcs {
		all[name] = fn
	}
	for name, fn := range methods {
		all[name] = fn
	}

	writers := map[string]bool{}
	for name, fn := range all {
		if ssoSettingsWriteSQL.MatchString(gmClassReachableSQL(fn, pkgStrings)) {
			writers[name] = true
		}
	}
	if len(writers) == 0 {
		t.Fatal("found no sso_settings write statements -- either the SQL moved or ssoSettingsWriteSQL " +
			"is stale. Refusing to certify the dual-write against an empty universe")
	}

	// Closure: anything calling a writer is a writer.
	for changed := true; changed; {
		changed = false
		for name, fn := range all {
			if writers[name] {
				continue
			}
			for called := range gmClassCalledNames(fn) {
				if writers[called] {
					writers[name] = true
					changed = true
					break
				}
			}
		}
	}

	// Only methods on the repository type are the universe the mirror test
	// walks; the package functions existed solely to carry SQL into the
	// closure.
	out := map[string]bool{}
	for name := range writers {
		if _, isMethod := methods[name]; isMethod {
			out[name] = true
		}
	}
	return out
}

func gmClassSortedNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestGroupMappingDualWriteClass_DerivationSeesEveryCarrierRoute proves each
// of the three routes SQL can reach a method through is load-bearing ON ITS
// OWN, over a synthetic universe where the routes do not overlap. The real
// package cannot prove this: Upsert's SQL is a body literal, so blinding the
// const route or the helper route there changes nothing -- which is exactly
// how a guard's mutation test fails to fail, and why this test exists.
func TestGroupMappingDualWriteClass_DerivationSeesEveryCarrierRoute(t *testing.T) {
	dir := t.TempDir()
	src := `package fakestore

const constCarried = ` + "`" + `INSERT INTO sso_settings (id) VALUES (1)` + "`" + `

func helperWrite() { _ = ` + "`" + `UPDATE sso_settings SET oidc_default_role = ''` + "`" + ` }

type FakeRepo struct{}

func (r *FakeRepo) ConstCarried() { _ = constCarried }
func (r *FakeRepo) ViaHelper()    { helperWrite() }
func (r *FakeRepo) BodyLiteral()  { _ = ` + "`" + `DELETE FROM sso_settings WHERE id = 1` + "`" + ` }
func (r *FakeRepo) QuotedSchema() { _ = ` + "`" + `UPDATE "public"."sso_settings" SET id = 1` + "`" + ` }
func (r *FakeRepo) ReadOnly()     { _ = ` + "`" + `SELECT id FROM sso_settings` + "`" + ` }
`
	if err := os.WriteFile(filepath.Join(dir, "store.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write synthetic store: %v", err)
	}

	writers := gmClassDeriveOverlayWriters(t, dir, "FakeRepo")
	for _, route := range []struct{ method, carrier string }{
		{"ConstCarried", "a package-level string const its body references"},
		{"ViaHelper", "a package function it calls"},
		{"BodyLiteral", "a string literal in its own body"},
		{"QuotedSchema", "a quoted, schema-qualified spelling of the table"},
	} {
		if !writers[route.method] {
			t.Errorf("the derivation missed %s, whose write SQL is carried by %s. That carrier route "+
				"has gone blind, and an overlay writer spelled that way would silently escape every "+
				"other test in this file.", route.method, route.carrier)
		}
	}
	if writers["ReadOnly"] {
		t.Error("the derivation classified ReadOnly (a SELECT) as a writer -- the SQL matcher is " +
			"over-broad, and the tests below would demand mirrors on reads")
	}
}

// TestGroupMappingDualWriteClass_WriteSQLSeesEverySpelling pins the matcher
// against each legal spelling of the target, one witness per axis, plus the
// non-writes it must NOT match.
func TestGroupMappingDualWriteClass_WriteSQLSeesEverySpelling(t *testing.T) {
	matches := []struct{ name, sql string }{
		{"bare insert", `INSERT INTO sso_settings (id) VALUES (1)`},
		{"bare update", `UPDATE sso_settings SET oidc_default_role = ''`},
		{"bare delete", `DELETE FROM sso_settings WHERE id = 1`},
		{"schema qualified", `INSERT INTO public.sso_settings (id) VALUES (1)`},
		{"quoted table", `UPDATE "sso_settings" SET id = 1`},
		{"quoted schema and table", `DELETE FROM "public"."sso_settings"`},
		{"case and whitespace", "insert\n\tinto\n\tSSO_SETTINGS (id)"},
	}
	for _, tc := range matches {
		if !ssoSettingsWriteSQL.MatchString(tc.sql) {
			t.Errorf("%s: %q escapes ssoSettingsWriteSQL -- a legal spelling the guard cannot see", tc.name, tc.sql)
		}
	}
	nonMatches := []struct{ name, sql string }{
		{"select", `SELECT oidc_group_mappings FROM sso_settings WHERE id = 1`},
		{"other table", `INSERT INTO sso_settings_archive (id) VALUES (1)`},
		{"upsert arm without the table", `ON CONFLICT (id) DO UPDATE SET oidc_default_role = EXCLUDED.oidc_default_role`},
	}
	for _, tc := range nonMatches {
		if ssoSettingsWriteSQL.MatchString(tc.sql) {
			t.Errorf("%s: %q matches ssoSettingsWriteSQL -- the matcher is over-broad", tc.name, tc.sql)
		}
	}
}

// TestGroupMappingDualWriteClass_UniverseSeesTheWriter pins the witness: the
// one overlay writer that exists today must be derived. If it stops being,
// either the write path genuinely moved (update the witness deliberately) or
// the derivation has gone blind, and every green result from the tests below
// means less than it claims.
func TestGroupMappingDualWriteClass_UniverseSeesTheWriter(t *testing.T) {
	writers := gmClassDeriveOverlayWriters(t, ".", "SSOSettingsRepository")
	if !writers["Upsert"] {
		t.Errorf("the derivation no longer sees SSOSettingsRepository.Upsert as an sso_settings writer; "+
			"derived writers: %v", gmClassSortedNames(writers))
	}
	t.Logf("derived overlay writers: %v", gmClassSortedNames(writers))
}

// TestGroupMappingDualWriteClass_EveryOverlayWriterReachesTheMirror is bypass
// 1: every method on SSOSettingsRepository that writes sso_settings must reach
// a mirror call, or be classified in
// overlayWritersThatCannotChangeMappingContent with a reason.
func TestGroupMappingDualWriteClass_EveryOverlayWriterReachesTheMirror(t *testing.T) {
	writers := gmClassDeriveOverlayWriters(t, ".", "SSOSettingsRepository")
	methods := gmClassMethodsOn(t, ".", "SSOSettingsRepository")

	var checked int
	for _, name := range gmClassSortedNames(writers) {
		if reason := overlayWritersThatCannotChangeMappingContent[name]; reason != "" {
			checked++
			continue
		}
		checked++
		var reaches bool
		for called := range gmClassCalledNames(methods[name]) {
			if groupMappingMirrorCallNames[called] {
				reaches = true
				break
			}
		}
		if !reaches {
			t.Errorf("SSOSettingsRepository.%s writes sso_settings but reaches none of the mirror calls "+
				"%v. The change lands in the overlay and never in TSM's own group_mappings, which is "+
				"exactly the half-a-dual-write this guard exists to prevent "+
				"(terraform-suite-identity#206). Mirror it, or classify it in "+
				"overlayWritersThatCannotChangeMappingContent with the reason.",
				name, gmClassSortedNames(groupMappingMirrorCallNames))
		}
	}
	if checked == 0 {
		t.Fatal("no repository method matched an sso_settings write -- the derivation has drifted " +
			"from the tree and this guard is vacuous")
	}

	for name := range overlayWritersThatCannotChangeMappingContent {
		if !writers[name] {
			t.Errorf("overlayWritersThatCannotChangeMappingContent names %q, which is no longer a "+
				"derived sso_settings writer -- drop the entry", name)
		}
	}
	t.Logf("checked %d overlay writer(s) against the mirror", checked)
}

// rawSSOSettingsWriteAllowlist names the files permitted to write sso_settings
// at all, with the reason. Exactly one: the repository whose Upsert carries
// the dual-write. Checked in both directions: a file here with no write is a
// stale entry, and a write in a file that is not here fails outright.
var rawSSOSettingsWriteAllowlist = map[string]string{
	"internal/db/repositories/sso_settings_repository.go": "the repository itself; its Upsert is the " +
		"dual-write choke point, and the tests above hold it to reaching the mirror",
}

// TestGroupMappingDualWriteClass_RawOverlayWritesAreConfined is bypass 2. A
// hand-written sso_settings statement anywhere else bypasses the mirror
// entirely and TSM's own group_mappings silently diverges.
func TestGroupMappingDualWriteClass_RawOverlayWritesAreConfined(t *testing.T) {
	root := gmClassModuleRoot(t)

	found := map[string]bool{}
	var scanned int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		if !ssoSettingsWriteSQL.Match(src) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		found[rel] = true
		if _, known := rawSSOSettingsWriteAllowlist[rel]; !known {
			t.Errorf("%s writes sso_settings and is not listed in rawSSOSettingsWriteAllowlist. A "+
				"statement outside the repository bypasses the group-mapping dual-write in "+
				"SSOSettingsRepository.Upsert entirely (terraform-suite-identity#206). Write it through "+
				"the repository instead.", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatal("scanned no Go files -- the walk is broken and this guard is vacuous")
	}
	if len(found) == 0 {
		t.Fatal("found no sso_settings write statements anywhere -- either the write path moved or " +
			"ssoSettingsWriteSQL is stale. It must not pass by matching nothing")
	}
	for rel := range rawSSOSettingsWriteAllowlist {
		if !found[rel] {
			t.Errorf("rawSSOSettingsWriteAllowlist lists %q, which no longer writes sso_settings -- "+
				"drop the entry", rel)
		}
	}
	t.Logf("scanned %d Go file(s); sso_settings writers: %v", scanned, gmClassSortedNames(found))
}

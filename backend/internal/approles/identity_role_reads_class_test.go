package approles_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"
	"testing"
)

// The class this file guards: A READ OF identity.role_templates ANYWHERE IN
// THIS APPLICATION.
//
// sethbacon/terraform-suite-identity#206 Phase 3 ends with this application's
// own role_templates as the ONLY source of what a role means here. The residual
// reads that survived Phase 3b — the ceiling check, the group-mapping guard,
// the admin fallback, the mirror's adopt-on-miss, the reconcile's adopt pass,
// and the drift comparison's template load — were retired together, and this
// guard is what keeps them retired: each of those sites was individually
// reasonable, which is exactly how the next one would get written.
//
// TWO SPELLINGS ARE COVERED, because both existed in this tree on the day the
// reads were retired:
//
//	AXIS A  the shared library's repository. Any mention of the identifier
//	        RoleTemplateRepository (the type, its constructor, a field of that
//	        type) in non-test code is refused. Matching the IDENTIFIER rather
//	        than the import path means a package alias, a dot-import, or a
//	        re-exported wrapper cannot spell it invisibly.
//	AXIS B  raw SQL. Any string literal in non-test code whose folded text
//	        reads role_templates in a read position (FROM / JOIN / RETURNING,
//	        with or without an identity qualifier, with or without quoted
//	        identifiers) is refused outside internal/approles/store.go — the
//	        app-side store, whose connection migration 000032's routing check
//	        and Store.Verify keep out of the identity schema. Literal folding
//	        covers concatenation chains, so splitting the statement across a
//	        `+` does not hide it.
//
// WRITES ARE NOT BANNED: bootstrap.seedSharedRoleTemplates still upserts the
// identity-side copy (gated by suite.role_seed_owner) for the rollback lever
// and the sibling, until Phase 4 drops the table. Its statement carries no
// read shape, so axis B passes it — and would refuse it the day someone adds
// a RETURNING clause.
//
// KNOWN BLIND AXES, stated rather than implied: SQL assembled at runtime from
// fragments that never co-occur in one literal (e.g. Sprintf over a table-name
// constant), and reads the shared identity library performs internally on the
// identity connection — the library's own membership queries join
// identity.role_templates and discard the columns under RoleSourceApp, which
// is library mechanism this repository cannot unsay and Phase 4 removes. The
// first is mitigated by axis A (the only convenient handle is banned) and by
// the matcher self-checks below, which keep both detectors provably alive.

// bannedIdentifier is the shared library's role-template repository, in any
// spelling that names it: the type, the constructor, or a method set reached
// through either.
const bannedIdentifier = "RoleTemplateRepository"

// identityRoleReadPattern matches a string literal that READS role_templates.
//
// The role_templates token is matched with an optional identity qualifier and
// optional quoted-identifier quotes, because quoted spellings are exactly how
// this estate's guards have been defeated before. Case-insensitive throughout.
var identityRoleReadPattern = regexp.MustCompile(
	`(?is)\b(from|join)\s+(?:"?identity"?\s*\.\s*)?"?role_templates\b` +
		`|\brole_templates\b[^;]*\breturning\b`)

// roleReadAllowedFiles are the files whose literals MAY read role_templates:
// the app-side store alone. Everything it runs is on the application
// connection, which cannot resolve identity's copy (migration 000032's routing
// pre-check and Store.Verify both refuse a search_path that reaches identity).
var roleReadAllowedFiles = map[string]bool{
	"internal/approles/store.go": true,
}

// TestNothingReadsIdentityRoleTemplates is the whole guard: both axes over the
// whole non-test tree, with the self-checks that keep the detectors honest.
func TestNothingReadsIdentityRoleTemplates(t *testing.T) {
	// SELF-CHECK FIRST, on synthetic sources, so a refactor that quietly stops
	// either detector from matching fails THIS test rather than certifying the
	// tree. A guard that cannot be made to fail is inert.
	if hits := identifierHits(t, parseSnippet(t,
		`package x
		import idstore "example.com/identity/store"
		var r = idstore.NewRoleTemplateRepository(nil)`)); len(hits) == 0 {
		t.Fatal("the identifier detector no longer sees idstore.NewRoleTemplateRepository: the guard is inert")
	}
	for _, snippet := range []string{
		"package x\nconst q = `SELECT id FROM role_templates WHERE name = $1`",
		"package x\nconst q = `select rt.scopes from identity.role_templates rt`",
		"package x\nconst q = `SELECT 1 FROM \"role_templates\"`",
		"package x\nconst q = `SELECT 1 FROM IDENTITY.\"ROLE_TEMPLATES\"`",
		"package x\nconst q = `SELECT s FROM \"identity\".\"role_templates\"`",
		"package x\nconst q = `LEFT JOIN role_templates t ON t.id = r.role_template_id`",
		"package x\nconst q = `INSERT INTO role_templates (id) VALUES ($1) RETURNING id`",
		"package x\nconst q = `SELECT id ` + `FROM role_` + `templates`",
	} {
		if hits := literalHits(t, parseSnippet(t, snippet)); len(hits) == 0 {
			t.Fatalf("the literal detector no longer sees a read of role_templates in %q: the guard is inert", snippet)
		}
	}

	files := scanTree(t)

	var allowedFileSeen bool
	var allowedFileReads int
	for _, f := range files {
		// AXIS A.
		for _, hit := range identifierHits(t, f.file) {
			t.Errorf("%s mentions %s (%s): the shared identity schema's role templates are not read by this application any more — resolve the role from internal/approles (Members.TemplateByID / TemplateByName / Store)",
				f.rel, bannedIdentifier, hit)
		}
		// AXIS B.
		hits := literalHits(t, f.file)
		if roleReadAllowedFiles[f.rel] {
			allowedFileSeen = true
			allowedFileReads += len(hits)
			continue
		}
		for _, lit := range hits {
			t.Errorf("%s contains SQL that reads role_templates outside the app-side store: %q — identity.role_templates is not read by this application any more, and the app connection's copy is only readable through internal/approles/store.go",
				f.rel, truncate(lit, 120))
		}
	}

	// EMPTY UNIVERSES ARE REFUSED. The allowlisted store must have been scanned
	// AND must still contain reads the pattern matches: if it stops matching
	// real code, every future violation would pass unseen.
	if !allowedFileSeen {
		t.Fatal("internal/approles/store.go was not scanned: the guard inspected the wrong universe")
	}
	if allowedFileReads == 0 {
		t.Fatal("the literal detector matched nothing in internal/approles/store.go, which reads role_templates on the app connection by design: the pattern has stopped matching real code and the guard is inert")
	}
}

// parseSnippet parses one synthetic source for the self-checks.
func parseSnippet(t *testing.T, src string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "snippet.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing the self-check snippet: %v", err)
	}
	return f
}

// identifierHits reports every mention of the banned identifier in one file, as
// short context strings for the error message.
func identifierHits(t *testing.T, f *ast.File) []string {
	t.Helper()
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if strings.Contains(id.Name, bannedIdentifier) {
			hits = append(hits, id.Name)
		}
		return true
	})
	return hits
}

// literalHits folds every string-literal expression in one file and reports the
// folded texts that read role_templates.
//
// Folding resolves `+` chains of string literals into the one text the database
// would receive, so a statement split across concatenation is matched exactly
// like one written whole. Chains involving non-literal operands fold their
// literal parts joined in order, which errs toward MATCHING (a literal head
// "SELECT ... FROM " glued to a variable still contributes its text).
func literalHits(t *testing.T, f *ast.File) []string {
	t.Helper()
	var hits []string
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.BinaryExpr:
			if e.Op == token.ADD {
				if folded, hasLit := foldStrings(e); hasLit && identityRoleReadPattern.MatchString(folded) {
					hits = append(hits, folded)
					return false // the parts are covered by the whole
				}
			}
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				if v, ok := unquote(e); ok && identityRoleReadPattern.MatchString(v) {
					hits = append(hits, v)
				}
			}
		}
		return true
	}
	ast.Inspect(f, visit)
	return hits
}

// foldStrings renders the concatenated string content of a `+` expression.
func foldStrings(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, lok := foldStrings(v.X)
		r, rok := foldStrings(v.Y)
		return l + r, lok || rok
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			if s, ok := unquote(v); ok {
				return s, true
			}
		}
	case *ast.ParenExpr:
		return foldStrings(v.X)
	}
	return "", false
}

// unquote strips the quotes off a string literal, handling both raw and
// interpreted forms without failing the fold on an exotic escape.
func unquote(l *ast.BasicLit) (string, bool) {
	s := l.Value
	if len(s) < 2 {
		return "", false
	}
	if s[0] == '`' {
		return strings.Trim(s, "`"), true
	}
	// Interpreted string: the escapes that matter to this pattern are none, so
	// the raw inner text is a faithful enough rendering to match against.
	return s[1 : len(s)-1], true
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

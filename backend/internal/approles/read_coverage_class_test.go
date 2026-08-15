package approles_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The class this file guards: A ROLE-CARRYING READ THAT STILL ANSWERS FROM
// IDENTITY AFTER PHASE 3B MOVED THE READS.
//
// # Why a guard, and not a list
//
// Go has no virtual dispatch. approles.Members embeds the shared organization
// repository, so a method it does not declare is PROMOTED — and, crucially, the
// library's own derived methods call the EMBEDDED receiver's implementation, not
// the wrapper's. GetUserCombinedScopes is implemented as
// `r.GetUserMemberships(...)` inside the library; overriding GetUserMemberships on
// Members and leaving GetUserCombinedScopes promoted produces a repository whose
// membership list comes from this application's tables and whose SESSION SCOPES
// come from identity's, compiles without a warning, and passes every test of
// either method taken alone.
//
// What that failure looks like in production is a principal whose /auth/me shows
// one role and whose token grants another. Nothing in the request path reports it.
//
// # The universe is DERIVED FROM THE LIBRARY'S SOURCE, not hand-listed
//
// A hand-written list of "the reads that carry a role" is correct on the day it
// is written and silently wrong the day terraform-suite-identity adds a method or
// threads a role column into an existing one — which is exactly the upgrade
// during which nobody re-reads this file. So the list is computed: parse the
// module in the build's own module cache, resolve the shared query constants,
// find every exported method on *OrganizationRepository whose statement or scan
// touches a role template, close that set over the methods that call one another,
// and require Members to declare each.
//
// The cost is that this test depends on `go list -m` and on the module source
// being present. Both hold anywhere the package can be built, and a FAILURE to
// find them is a fatal error rather than a skip: a guard that skips when its
// input is missing is a guard that reports green on the machine where it matters.

// roleToken is the normalised marker of a role-carrying statement or scan.
//
// Matched after lowercasing and removing underscores, so one token covers every
// spelling the library uses for the same thing: `role_template_id` in SQL,
// `RoleTemplateScopes` on a model, `roleTemplateName` in a parameter list.
const roleToken = "roletemplate"

// identityStoreDir locates the shared identity module's store package in this
// build's module cache.
func identityStoreDir(t *testing.T) string {
	t.Helper()
	root := backendRoot(t)
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/sethbacon/terraform-suite-identity")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("locating the terraform-suite-identity module (`go list -m`): %v\n"+
			"This guard derives the set of role-carrying reads from that module's source. It fails rather "+
			"than skips: a build that cannot see the library cannot certify that this package overrides all of it.", err)
	}
	dir := filepath.Join(strings.TrimSpace(string(out)), "identity", "store")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("expected the identity store package at %s: %v", dir, err)
	}
	return dir
}

// storePackage is the parsed identity store package plus its resolved
// package-level string constants.
type storePackage struct {
	files  []*ast.File
	fset   *token.FileSet
	consts map[string]string
	src    map[string][]byte
}

// parseIdentityStore parses the library's store package and resolves its
// package-level string constants.
//
// THE CONSTANTS MATTER MORE THAN THE STATEMENTS. Since the library extracted
// membership.go, the role JOIN does not appear in the method bodies at all — it
// appears in `userMembershipFrom`, which `userMembershipByUserQuery` concatenates
// and GetUserMemberships references by name. A scan that looked only at method
// bodies would find no role in any of the five accessors that carry one, report
// an empty universe, and certify nothing.
func parseIdentityStore(t *testing.T) *storePackage {
	t.Helper()
	dir := identityStoreDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	pkg := &storePackage{fset: token.NewFileSet(), consts: map[string]string{}, src: map[string][]byte{}}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("reading %s: %v", path, rerr)
		}
		file, perr := parser.ParseFile(pkg.fset, path, src, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parsing %s: %v", path, perr)
		}
		pkg.files = append(pkg.files, file)
		pkg.src[path] = src
	}
	if len(pkg.files) == 0 {
		t.Fatalf("no Go source under %s: the guard would derive its universe from nothing", dir)
	}
	pkg.resolveConsts()
	return pkg
}

// resolveConsts collects package-level string constants, evaluating the
// concatenations that build the shared query constants out of one another.
func (p *storePackage) resolveConsts() {
	raw := map[string]ast.Expr{}
	for _, f := range p.files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						raw[name.Name] = vs.Values[i]
					}
				}
			}
		}
	}
	// Iterate to a fixpoint: a constant may name another declared later, or in
	// another file.
	for range len(raw) + 1 {
		progress := false
		for name, expr := range raw {
			if _, done := p.consts[name]; done {
				continue
			}
			if v, ok := evalString(expr, p.consts); ok {
				p.consts[name] = v
				progress = true
			}
		}
		if !progress {
			break
		}
	}
}

// evalString evaluates a string literal or a concatenation of literals and
// already-resolved constants.
func evalString(expr ast.Expr, known map[string]string) (string, bool) {
	switch v := expr.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.Ident:
		s, ok := known[v.Name]
		return s, ok
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, lok := evalString(v.X, known)
		r, rok := evalString(v.Y, known)
		if !lok || !rok {
			return "", false
		}
		return l + r, true
	default:
		return "", false
	}
}

// normalise lowercases and strips underscores so one token matches every
// spelling of the same concept.
func normalise(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), "_", "")
}

// roleCarryingReads derives, from the library's source, the exported
// *OrganizationRepository methods whose answer includes a role.
//
// Two passes. DIRECT: the method's own body names a role template, or references
// a package constant whose resolved value does. TRANSITIVE, to a fixpoint: the
// method calls another such method on its own receiver — which is the case that
// matters most, because those are precisely the methods a wrapper leaves promoted
// without noticing.
func roleCarryingReads(t *testing.T, pkg *storePackage) map[string]bool {
	t.Helper()

	type method struct {
		body     string
		idents   []string
		receiver []string // methods called on the repository's own receiver
	}
	methods := map[string]*method{}

	for path, file := range fileByPath(pkg) {
		src := pkg.src[path]
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !isOrganizationRepositoryMethod(fn) || !fn.Name.IsExported() {
				continue
			}
			m := &method{}
			start := pkg.fset.Position(fn.Body.Pos()).Offset
			end := pkg.fset.Position(fn.Body.End()).Offset
			m.body = string(src[start:end])
			// The signature counts too: a method taking or returning a role
			// template names it there even when the body delegates.
			sigStart := pkg.fset.Position(fn.Type.Pos()).Offset
			sigEnd := pkg.fset.Position(fn.Type.End()).Offset
			m.body += string(src[sigStart:sigEnd])

			recvName := receiverName(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.Ident:
					m.idents = append(m.idents, v.Name)
				case *ast.CallExpr:
					sel, isSel := v.Fun.(*ast.SelectorExpr)
					if !isSel {
						return true
					}
					if id, isID := sel.X.(*ast.Ident); isID && id.Name == recvName {
						m.receiver = append(m.receiver, sel.Sel.Name)
					}
				}
				return true
			})
			methods[fn.Name.Name] = m
		}
	}

	if len(methods) == 0 {
		t.Fatal("no exported methods on *OrganizationRepository were found in the identity module: the guard derived an empty universe")
	}

	carries := map[string]bool{}
	for name, m := range methods {
		if strings.Contains(normalise(m.body), roleToken) {
			carries[name] = true
			continue
		}
		for _, ident := range m.idents {
			if v, ok := pkg.consts[ident]; ok && strings.Contains(normalise(v), roleToken) {
				carries[name] = true
				break
			}
		}
	}
	for range len(methods) + 1 {
		grew := false
		for name, m := range methods {
			if carries[name] {
				continue
			}
			for _, called := range m.receiver {
				if carries[called] {
					carries[name] = true
					grew = true
					break
				}
			}
		}
		if !grew {
			break
		}
	}

	if len(carries) == 0 {
		t.Fatal("no role-carrying method was derived from the identity module: either the library stopped joining role " +
			"templates (in which case this whole package is obsolete) or the constant resolution stopped working, and " +
			"the guard is certifying an empty universe")
	}
	return carries
}

func fileByPath(pkg *storePackage) map[string]*ast.File {
	out := make(map[string]*ast.File, len(pkg.files))
	for _, f := range pkg.files {
		out[pkg.fset.Position(f.Pos()).Filename] = f
	}
	return out
}

func isOrganizationRepositoryMethod(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "OrganizationRepository"
}

func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 || len(fn.Recv.List[0].Names) != 1 {
		return ""
	}
	return fn.Recv.List[0].Names[0].Name
}

// membersMethods returns the methods declared on *approles.Members, mapped to
// the file that declares each.
func membersMethods(t *testing.T) map[string]string {
	t.Helper()
	root := backendRoot(t)
	dir := filepath.Join(root, "internal", "approles")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !receiverIsMembers(fn) {
				continue
			}
			out[fn.Name.Name] = name
		}
	}
	if len(out) == 0 {
		t.Fatal("no methods on approles.Members were found: the guard inspected an empty universe")
	}
	return out
}

// TestEveryRoleCarryingReadIsOverridden is AXIS 5.
//
// Every method of the shared repository whose answer includes a role must be
// declared on Members. A method left promoted answers from
// identity.organization_members joined to identity.role_templates, which is the
// Phase 3a source — so a single omission here is a deployment that believes its
// authorization moved and, on one accessor, silently did not.
func TestEveryRoleCarryingReadIsOverridden(t *testing.T) {
	pkg := parseIdentityStore(t)
	carries := roleCarryingReads(t, pkg)
	overridden := membersMethods(t)

	var missing []string
	for name := range carries {
		if _, ok := overridden[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("these shared OrganizationRepository methods carry a role and are NOT overridden by approles.Members: %v.\n"+
			"Promoted, they answer from identity.organization_members joined to identity.role_templates — the Phase 3a source — "+
			"while every overridden accessor answers from this application's own tables. Go has no virtual dispatch, so a derived "+
			"method the library implements over its OWN receiver keeps reading identity even when the base read it calls is "+
			"overridden here. Override it in reads.go over m, or (if it genuinely carries no role) establish that and this guard "+
			"will stop deriving it.", missing)
	}
}

// TestNoOverriddenReadIsAPureForwarder is AXIS 6.
//
// Axis 5 guarantees the methods are declared. This guarantees declaring them was
// worth doing: a read override in reads.go must consult this application's tables
// — directly through the store, or through another override that does. An
// override that forwards to m.identityOrgs and returns satisfies axis 5 exactly
// while restoring the whole defect, and it is a plausible thing to write while
// stubbing one out.
func TestNoOverriddenReadIsAPureForwarder(t *testing.T) {
	root := backendRoot(t)
	path := filepath.Join(root, "internal", "approles", "reads.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading reads.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "reads.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing reads.go: %v", err)
	}

	pkg := parseIdentityStore(t)
	carries := roleCarryingReads(t, pkg)

	var checked, forwarders []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Recv == nil || !receiverIsMembers(fn) || !carries[fn.Name.Name] {
			continue
		}
		var consultsApp bool
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			recv, sel, ok := selectorName(call)
			if !ok {
				return true
			}
			// A store read, or another role-carrying override on m.
			if strings.Contains(recv, "m.store") {
				consultsApp = true
			}
			if recv == "m" && carries[sel] {
				consultsApp = true
			}
			return true
		})
		if consultsApp {
			checked = append(checked, fn.Name.Name)
		} else {
			forwarders = append(forwarders, fn.Name.Name)
		}
	}

	if len(checked)+len(forwarders) == 0 {
		t.Fatal("reads.go declares no role-carrying override at all: the guard inspected an empty universe " +
			"(the overrides moved to another file, or the derived set stopped matching their names)")
	}
	if len(forwarders) > 0 {
		sort.Strings(forwarders)
		t.Fatalf("these role-carrying overrides never consult this application's tables: %v.\n"+
			"An override that calls only m.identityOrgs and returns its rows is a forwarder: it satisfies "+
			"\"the method is declared\" while answering from exactly the source Phase 3b moved off. Read the role "+
			"through m.store, or derive it from another override that does.", forwarders)
	}
}

// TestTheDerivedReadSetCoversTheAccessorsAuthorizationActuallyUses is the
// NON-VACUITY check for the derivation itself.
//
// Every assertion above reports "clean" by finding nothing missing, so a
// derivation that silently stopped resolving the library's query constants —
// which is where the role JOIN actually lives since membership.go — would make
// both of them pass while checking almost nothing. These four are the accessors
// TSM's authorization is built on: the session scope union, the per-organization
// scope set, the tenancy resolver, and the membership list the other three are
// derived from. If the derivation cannot see that these carry a role, it cannot
// see anything.
func TestTheDerivedReadSetCoversTheAccessorsAuthorizationActuallyUses(t *testing.T) {
	pkg := parseIdentityStore(t)
	carries := roleCarryingReads(t, pkg)
	for _, name := range []string{
		"GetUserMemberships",
		"GetUserCombinedScopes",
		"GetUserScopesForOrg",
		"OrgScopeForUser",
		"GetMemberWithRole",
		"ListMembersWithUsers",
		"CheckMembership",
	} {
		if !carries[name] {
			t.Errorf("the derivation does not consider %s role-carrying. It is one of the accessors every "+
				"authorization decision in this repository is built on, so a derivation that misses it is "+
				"reporting on a universe that excludes the thing being guarded.", name)
		}
	}
}

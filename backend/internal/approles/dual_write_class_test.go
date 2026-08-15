package approles_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The class this file guards: A ROLE ASSIGNMENT THAT REACHES IDENTITY AND NOT
// TSM'S OWN TABLES.
//
// Phase 3a's whole content is that both places are written. Reads have not
// moved, so a missed mirror is invisible in every test that exercises behaviour:
// the request succeeds, the response is right, the audit entry is written, and
// the only thing that is wrong is a row that nobody looks at yet. It becomes
// visible in the phase that starts looking — as a user who has quietly lost
// their role, on a deployment that has been running for months.
//
// A convention ("remember to mirror") is the weakest possible guard, and this
// repo already recorded why: scim.deprovisionUser exists because four
// deactivation endpoints each had to remember to sweep credentials, three did,
// and the fourth did not. So the mirror is not a convention here either. It is
// structural — a caller holds *approles.Members and the unwrapped repository is
// unreachable — and these three axes are what keep the structure standing:
//
//	AXIS 1  the shared organization repository is CONSTRUCTED only in this
//	        package. That is the choke point: every membership write in TSM goes
//	        through a repository somebody constructed, so a new assignment path
//	        can only obtain one from NewMembers, whose overrides mirror.
//	AXIS 2  every Members method that calls an identity write ALSO calls the app
//	        store. A wrapper that forwards without mirroring passes axis 1 while
//	        defeating its entire purpose.
//	AXIS 3  a function that calls UserRepository.DeleteUser also calls
//	        PurgeUserRoles. That deletion withdraws every role by CASCADE without
//	        touching this repository at all, so it is the one authority-reducing
//	        path axis 1 cannot see.
//	AXIS 4  every Members method that takes an OrgScope and writes the mirror
//	        consults it. Added after the suite's tenant-scope signature (#719)
//	        found exactly this defect in this package's first CI run: a mirror
//	        leg that ignores tenancy writes another tenant's row, and on the
//	        revocation paths it does so BEFORE the identity leg could refuse.
//
// EMPTY UNIVERSES ARE REFUSED. Each axis asserts it actually inspected
// something: zero files scanned, zero Members methods found, zero DeleteUser
// call sites — any of those means the scan stopped matching the tree, and a scan
// that matches nothing certifies nothing. Every axis below fails on an empty
// universe rather than passing vacuously.

// identityWriteMethods are the shared OrganizationRepository methods that set,
// change or remove a member's role. Axis 2 requires each one that Members
// overrides to write the mirror too.
//
// Spelled here as data rather than derived from the wrapper, so that a wrapper
// method deleted along with its mirror does not take its own guard with it.
var identityWriteMethods = []string{
	"AddMemberWithRoleTemplate",
	"AddMemberWithParams",
	"UpdateMemberRoleTemplate",
	"UpdateMemberRole",
	"RemoveMember",
	"RemoveAllMembershipsForUser",
}

// mirrorHelpers are the app-side writes a Members method may satisfy axis 2
// with: either a Store method or one of the package's own mirror helpers, which
// call one.
var mirrorHelpers = map[string]bool{
	"mirrorSetByID": true, "mirrorSetByName": true, "mirrorDelete": true,
	"mirrorDeleteForUser": true, "mirrorDeleteForOrganization": true,
	"SetRole": true, "DeleteRole": true, "DeleteRolesForUser": true,
	"DeleteRolesForOrganization": true, "UpsertTemplate": true,
}

// backendRoot walks up from this package to the module root, so the scan covers
// the whole tree rather than whichever directory the test happened to run in.
func backendRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolving the backend root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("expected the backend module root at %s: %v", dir, err)
	}
	return dir
}

// goFile is one parsed non-test source file, with its path relative to the
// backend root.
type goFile struct {
	rel  string
	file *ast.File
}

// scanTree parses every non-test .go file under internal/ and cmd/.
//
// Non-test only: a test may legitimately construct the raw repository to drive
// the library directly, and forbidding that would push every such test into an
// exemption list — which is how an exemption list becomes the place a real
// bypass hides.
func scanTree(t *testing.T) []goFile {
	t.Helper()
	root := backendRoot(t)
	fset := token.NewFileSet()
	var out []goFile
	for _, sub := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, sub), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			parsed, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			out = append(out, goFile{rel: filepath.ToSlash(rel), file: parsed})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", sub, err)
		}
	}
	if len(out) == 0 {
		t.Fatal("the source scan matched no files: the guard would certify an empty universe")
	}
	return out
}

// selectorName returns the method name of a call's function expression and its
// receiver rendered as a dotted path.
//
// THE PATH IS RENDERED, NOT JUST THE LEADING IDENTIFIER. The receivers this
// guard has to recognise are all nested selectors — `m.identityOrgs.AddMember…`,
// `h.userRepo.DeleteUser` — so a version that only handled a bare identifier
// would return an empty receiver for every call that matters and report the
// tree as clean. It did, on the first run of this guard.
func selectorName(call *ast.CallExpr) (recv, sel string, ok bool) {
	s, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return "", "", false
	}
	return exprPath(s.X), s.Sel.Name, true
}

// exprPath renders an identifier/selector chain as "a.b.c". Anything else
// (an index, a call, a literal) renders empty, which the callers treat as an
// unrecognised receiver.
func exprPath(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		prefix := exprPath(v.X)
		if prefix == "" {
			return v.Sel.Name
		}
		return prefix + "." + v.Sel.Name
	case *ast.StarExpr:
		return exprPath(v.X)
	case *ast.ParenExpr:
		return exprPath(v.X)
	default:
		return ""
	}
}

// TestOnlyThisPackageConstructsTheSharedOrganizationRepository is AXIS 1.
//
// Every membership write in TSM is a method call on a repository somebody
// constructed with idstore.NewOrganizationRepository. Confining that constructor
// to this package makes the mirrored wrapper the ONLY organization repository a
// handler can hold — so a new assignment path does not have to remember to
// mirror, it has nothing to call that does not.
//
// Keyed on the constructor rather than on the write methods because the method
// names cannot carry a guard: `Delete` (the organization delete, whose CASCADE
// withdraws every member's role) is a name a dozen unrelated repositories in
// this tree also have, so a name-based rule would be either full of false
// positives or narrowed until it missed exactly that one.
func TestOnlyThisPackageConstructsTheSharedOrganizationRepository(t *testing.T) {
	const constructor = "NewOrganizationRepository"
	const allowed = "internal/approles/"

	files := scanTree(t)
	var offenders, permitted []string
	for _, f := range files {
		ast.Inspect(f.file, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			_, sel, ok := selectorName(call)
			if !ok || sel != constructor {
				return true
			}
			if strings.HasPrefix(f.rel, allowed) {
				permitted = append(permitted, f.rel)
			} else {
				offenders = append(offenders, f.rel)
			}
			return true
		})
	}

	// EMPTY UNIVERSE. If nothing constructs the repository at all, either the
	// wrapper stopped wrapping or the scan stopped matching; both make every
	// assertion below vacuous.
	if len(permitted)+len(offenders) == 0 {
		t.Fatalf("no call to idstore.%s anywhere in internal/ or cmd/: the guard inspected an empty universe", constructor)
	}
	if len(permitted) == 0 {
		t.Fatalf("idstore.%s is never called inside %s: approles.Members is not wrapping anything", constructor, allowed)
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("idstore.%s is constructed outside %s, in %v.\n"+
			"A repository obtained that way writes identity WITHOUT writing this application's own "+
			"organization_member_roles, and nothing observable in Phase 3a would report it. Use "+
			"approles.NewMembers(identityDB, appDB): its reads are the library's, promoted unchanged, "+
			"and its role writes are mirrored.", constructor, allowed, offenders)
	}
}

// TestEveryMirroredWriteWritesBothSides is AXIS 2.
//
// Axis 1 guarantees callers can only reach Members. This guarantees Members is
// worth reaching: for each of the shared repository's role-writing methods that
// Members overrides, the override's body must call BOTH the identity leg it
// replaces and an app-side write. A forwarding override that dropped its mirror
// would satisfy axis 1 completely while restoring exactly the defect this phase
// removes.
func TestEveryMirroredWriteWritesBothSides(t *testing.T) {
	files := scanTree(t)

	type body struct {
		identityCalls map[string]bool
		mirrorCalls   map[string]bool
	}
	methods := map[string]body{}

	for _, f := range files {
		if !strings.HasPrefix(f.rel, "internal/approles/") {
			continue
		}
		for _, decl := range f.file.Decls {
			fn, isFn := decl.(*ast.FuncDecl)
			if !isFn || fn.Recv == nil || fn.Body == nil {
				continue
			}
			if !receiverIsMembers(fn) {
				continue
			}
			b := body{identityCalls: map[string]bool{}, mirrorCalls: map[string]bool{}}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				recv, sel, ok := selectorName(call)
				if !ok {
					return true
				}
				// The identity leg is only ever reached through the embedded
				// alias field, which is what makes it recognisable here.
				if strings.Contains(recv, "identityOrgs") {
					b.identityCalls[sel] = true
				}
				if mirrorHelpers[sel] {
					b.mirrorCalls[sel] = true
				}
				return true
			})
			methods[fn.Name.Name] = b
		}
	}

	if len(methods) == 0 {
		t.Fatal("no methods on approles.Members were found: the guard inspected an empty universe")
	}

	var covered []string
	for _, want := range identityWriteMethods {
		b, overridden := methods[want]
		if !overridden {
			t.Errorf("approles.Members does not override %s, so callers reach the unmirrored identity write through the embedded repository", want)
			continue
		}
		if !b.identityCalls[want] {
			t.Errorf("approles.Members.%s does not call the identity leg it replaces (m.identityOrgs.%s)", want, want)
		}
		if len(b.mirrorCalls) == 0 {
			t.Errorf("approles.Members.%s writes identity but never writes this application's own tables: "+
				"that is the exact defect the mirror exists to remove, and no behavioural test can see it in Phase 3a", want)
			continue
		}
		covered = append(covered, want)
	}
	if len(covered) != len(identityWriteMethods) {
		return // the per-method errors above already say which
	}
	if len(covered) == 0 {
		t.Fatal("no mirrored write methods were verified: the guard inspected an empty universe")
	}
}

// TestEveryScopedWriteScopesItsMirror is AXIS 4.
//
// The suite's tenant-scope signature (#719) found this class in this very
// package on its first CI run, which is why it is an axis and not a comment: a
// mirror leg that ignores the caller's OrgScope writes another tenant's row, and
// a REVOCATION mirrors BEFORE the identity leg's predicate has refused anything.
// Nothing reads the mirror in Phase 3a, so the only thing standing between that
// and the phase which does is this check.
//
// The rule: any Members method that takes an OrgScope and writes the mirror must
// consult the scope. Deliberately keyed on "consults it at all" rather than on
// where — a call to m.permits in the body is a thing one either wrote or did not,
// and pinning the exact shape would fail on every legitimate refactor.
func TestEveryScopedWriteScopesItsMirror(t *testing.T) {
	files := scanTree(t)

	var checked, unscoped []string
	for _, f := range files {
		if !strings.HasPrefix(f.rel, "internal/approles/") {
			continue
		}
		for _, decl := range f.file.Decls {
			fn, isFn := decl.(*ast.FuncDecl)
			if !isFn || fn.Recv == nil || fn.Body == nil || !receiverIsMembers(fn) {
				continue
			}
			if !takesOrgScope(fn) {
				continue
			}
			var writesMirror, consultsScope bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				sel := calleeName(call)
				if sel == "" {
					return true
				}
				if mirrorHelpers[sel] {
					writesMirror = true
				}
				if scopeConsulters[sel] {
					consultsScope = true
				}
				return true
			})
			if !writesMirror {
				continue
			}
			if consultsScope {
				checked = append(checked, fn.Name.Name)
			} else {
				unscoped = append(unscoped, fn.Name.Name)
			}
		}
	}

	if len(checked)+len(unscoped) == 0 {
		t.Fatal("no scoped Members method that writes the mirror was found: the guard inspected an empty universe")
	}
	if len(unscoped) > 0 {
		sort.Strings(unscoped)
		t.Fatalf("these Members methods take the caller's OrgScope and mirror without consulting it: %v.\n"+
			"A revocation mirrors BEFORE the identity leg's scope predicate runs, so an out-of-tenancy caller "+
			"would have another tenant's mirrored row deleted and then be told the membership was not found. "+
			"Guard the mirror leg with m.permits(scope, orgID), or scopeOrganizations(scope) for the bulk paths.", unscoped)
	}
}

// scopeConsulters are the ways a method may satisfy axis 4: the per-organization
// predicate, or the bulk translation that narrows a strip to the scope's
// organizations.
var scopeConsulters = map[string]bool{"permits": true, "scopeOrganizations": true}

// calleeName returns a call's function name for BOTH forms — `x.Foo(...)` and
// the bare `Foo(...)`.
//
// selectorName alone handles only the first, so axis 4 reported
// RemoveAllMembershipsForUser as unscoped on its first run: that method consults
// the scope through `scopeOrganizations(scope)`, a package-level function called
// as an ARGUMENT, whose Fun is a plain identifier and not a selector. A guard
// that cannot see the compliant form condemns the compliant code.
func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	default:
		return ""
	}
}

// takesOrgScope reports whether a method accepts the shared store's tenancy
// parameter.
func takesOrgScope(fn *ast.FuncDecl) bool {
	for _, param := range fn.Type.Params.List {
		sel, isSel := param.Type.(*ast.SelectorExpr)
		if isSel && sel.Sel.Name == "OrgScope" {
			return true
		}
	}
	return false
}

// receiverIsMembers reports whether a method is declared on *Members.
func receiverIsMembers(fn *ast.FuncDecl) bool {
	if len(fn.Recv.List) != 1 {
		return false
	}
	star, isStar := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !isStar {
		return false
	}
	id, isID := star.X.(*ast.Ident)
	return isID && id.Name == "Members"
}

// TestUserDeletionPurgesTheMirror is AXIS 3.
//
// UserRepository.DeleteUser withdraws every role the principal held, by
// ON DELETE CASCADE on identity.organization_members — without a membership
// statement, on a different repository, so axis 1 cannot see it. TSM's own table
// has no foreign key to cascade with, because identity may be another database.
//
// Guarded as a PAIRING rather than by wrapping the user repository: wrapping it
// would put a second wrapper on every path that only ever reads users, to catch
// one method. Wherever DeleteUser is called, PurgeUserRoles must be called in
// the same function.
func TestUserDeletionPurgesTheMirror(t *testing.T) {
	files := scanTree(t)

	type site struct{ file, fn string }
	var deleters, paired []site

	for _, f := range files {
		for _, decl := range f.file.Decls {
			fn, isFn := decl.(*ast.FuncDecl)
			if !isFn || fn.Body == nil {
				continue
			}
			var callsDelete, callsPurge bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				recv, sel, ok := selectorName(call)
				if !ok {
					return true
				}
				// The SCIM handler set exposes a route handler of the same name;
				// only a call ON A USER REPOSITORY is the cascading delete. The
				// receiver is spelled userRepo everywhere it is one.
				if sel == "DeleteUser" && strings.Contains(strings.ToLower(recv), "user") {
					callsDelete = true
				}
				if sel == "PurgeUserRoles" {
					callsPurge = true
				}
				return true
			})
			if !callsDelete {
				continue
			}
			s := site{file: f.rel, fn: fn.Name.Name}
			deleters = append(deleters, s)
			if callsPurge {
				paired = append(paired, s)
			}
		}
	}

	if len(deleters) == 0 {
		t.Fatal("no call to a user repository's DeleteUser was found in internal/ or cmd/: " +
			"the guard inspected an empty universe (if the hard-delete path was genuinely removed, remove this axis with it)")
	}
	if len(paired) != len(deleters) {
		var unpaired []string
		for _, d := range deleters {
			found := false
			for _, p := range paired {
				if p == d {
					found = true
					break
				}
			}
			if !found {
				unpaired = append(unpaired, d.file+":"+d.fn)
			}
		}
		sort.Strings(unpaired)
		t.Fatalf("these functions delete a user without purging this application's mirrored roles: %v.\n"+
			"identity.organization_members cascades; organization_member_roles cannot (no foreign key across the "+
			"identity boundary), so the rows survive the principal. Call orgRepo.PurgeUserRoles(ctx, userID) after "+
			"the delete.", unpaired)
	}
}

// TestTheEmbeddedRepositoryIsUnreachable pins the mechanism the other axes rest
// on: Members embeds the shared repository under an UNEXPORTED alias, so no
// package outside this one can name the field and call through it.
//
// Asserted on the source rather than trusted, because the difference between
// `*idstore.OrganizationRepository` and `*identityOrgs` is one word, produces an
// identical-looking struct, compiles either way, and silently reopens the bypass
// for every caller in the tree.
func TestTheEmbeddedRepositoryIsUnreachable(t *testing.T) {
	root := backendRoot(t)
	src, err := os.ReadFile(filepath.Join(root, "internal/approles/members.go"))
	if err != nil {
		t.Fatalf("reading members.go: %v", err)
	}
	text := string(src)

	if !strings.Contains(text, "type identityOrgs = idstore.OrganizationRepository") {
		t.Fatal("the unexported alias for the shared repository is gone: without it the embedded field is named " +
			"OrganizationRepository, is exported, and any caller can reach the unmirrored write through it")
	}
	if strings.Contains(text, "\t*idstore.OrganizationRepository\n") {
		t.Fatal("Members embeds *idstore.OrganizationRepository directly: the promoted field is exported, so " +
			"members.OrganizationRepository.AddMemberWithParams(...) writes identity and skips the mirror")
	}
}

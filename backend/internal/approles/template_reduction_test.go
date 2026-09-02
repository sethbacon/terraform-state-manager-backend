package approles

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// Behavioural guards for the boot-time role-template authority reduction (#557).
//
// THE DEFECT: this build's role definitions are written by a blind upsert that
// overwrites `scopes`. When the new list grants LESS than the row already there,
// every member holding that template loses authority — and every session token
// they already hold keeps the scopes that were removed, because a JWT freezes
// its claims at login and nothing re-derives them. The per-JTI denylist cannot
// help: a template narrowing knows no JTIs.
//
// What these tests hold in place is a sequence, not a value: read the pre-image,
// decide whether authority was REDUCED rather than merely changed, invalidate
// the credentials, and only then write. sqlmock's expectations are ordered, so
// "before" and "not at all" are things this harness can assert about real
// statements.
//
// Every test names the mutation it exists to catch.

// reductionEnv wires the two ordered mocks and stages everything Reconcile does
// around reconcileTemplates, so each test below writes only the part it is about.
type reductionEnv struct {
	*reconcileEnv
	reduced [][]ReducedTemplate
	fail    error
}

func newReductionEnv(t *testing.T) *reductionEnv {
	t.Helper()
	return &reductionEnv{reconcileEnv: newReconcileEnv(t)}
}

// reducer records what it was handed, in call order, and can be made to fail.
//
// IT ISSUES A STATEMENT, and that is what makes its POSITION observable rather
// than merely its arguments. The real reducer writes revocation watermarks to
// the application connection, so a double that touched no database was not a
// simplification — it was invisible to an ordered mock, and the ordering test
// that named "write first, reduce afterwards" as its mutation passed against
// exactly that build. The probe below occupies the reducer's place in the
// sequence, so moving the call moves the statement and the mock says so.
func (e *reductionEnv) reducer() TemplateAuthorityReducer {
	return func(ctx context.Context, reduced []ReducedTemplate) error {
		if _, err := e.appDB.ExecContext(ctx, reducerProbe); err != nil {
			return fmt.Errorf("reducer probe out of sequence: %w", err)
		}
		e.reduced = append(e.reduced, reduced)
		return e.fail
	}
}

// reducerProbe is the statement the reducer double issues so its position in
// the ordered sequence is checkable. It is not SQL anybody runs; the mock only
// ever matches it.
const reducerProbe = `UPDATE credentials_invalidated_by_the_reducer SET at = now()`

// expectReducerRuns stages the reducer's probe at the point in the sequence the
// reducer must run.
func expectReducerRuns(env *reductionEnv) {
	env.app.ExpectExec(regexp.QuoteMeta(reducerProbe)).WillReturnResult(sqlmock.NewResult(0, 1))
}

// definer supplies definitions without writing them, which is the whole point of
// TemplateDefiner since #557.
func definer(defs ...Template) TemplateDefiner {
	return func(context.Context) ([]Template, error) { return defs, nil }
}

func tmpl(name string, scopes ...string) Template {
	return Template{Name: name, DisplayName: name, Scopes: scopes, IsSystem: true}
}

const (
	editorID = "aaaaaaaa-0000-0000-0000-000000000001"
	viewerID = "aaaaaaaa-0000-0000-0000-000000000002"
)

// priorRows is the pre-image: what the deployment already holds.
func priorRows(rows ...[2]interface{}) *sqlmock.Rows {
	out := appTemplateRows()
	for _, r := range rows {
		id := r[0].(string)
		t := r[1].(Template)
		out.AddRow(id, t.Name, t.DisplayName, nil, []byte(scopesJSON(t.Scopes)), true, time.Now(), time.Now())
	}
	return out
}

func scopesJSON(scopes []string) string {
	out := "["
	for i, s := range scopes {
		if i > 0 {
			out += ","
		}
		out += `"` + s + `"`
	}
	return out + "]"
}

// stageThrough stages everything Reconcile issues up to and including the
// pre-image read, which is where each test's own expectations continue.
func stageThroughPreImage(env *reductionEnv, prior *sqlmock.Rows) {
	expectVerifyOK(env.app)
	expectDriftProbe(env.reconcileEnv)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	expectTemplatePreImage(env.reconcileEnv, prior)
}

// stageTail stages the readback, the membership scan and the sweep, so a test
// that reaches the write can run Reconcile to completion.
func stageTail(env *reductionEnv, held *sqlmock.Rows) {
	expectForeignTemplateReadback(env.reconcileEnv, held)
	expectMembershipScan(env.reconcileEnv, membershipScanRows())
	env.app.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE mirrored_at < $1`)).
		WillReturnResult(sqlmock.NewResult(0, 0))
}

func expectHolders(env *reductionEnv, templateID string, userIDs ...string) {
	rows := sqlmock.NewRows([]string{"user_id"})
	for _, u := range userIDs {
		rows.AddRow(u)
	}
	env.app.ExpectQuery(`SELECT DISTINCT user_id::text FROM organization_member_roles WHERE role_template_id = \$1`).
		WithArgs(templateID).WillReturnRows(rows)
}

func expectDefineTemplate(env *reductionEnv) {
	env.app.ExpectExec(regexp.QuoteMeta(`INSERT INTO role_templates`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// ---------------------------------------------------------------------------
// The sequence
// ---------------------------------------------------------------------------

// GUARD reduction-invalidates-before-it-writes. The contract in one test: the
// holders are read and the reducer is called while the OLD definition is still
// in the table, and the write follows. The mocks are ordered, so staging the
// write last is the assertion.
//
// MUTATION: write the definitions first and reduce afterwards; or move the
// pre-image read below the write, which makes the narrowing undetectable rather
// than merely undetected.
func TestReduction_InvalidatesBeforeItWrites(t *testing.T) {
	env := newReductionEnv(t)
	stageThroughPreImage(env, priorRows([2]interface{}{editorID, tmpl("editor", "state:read", "state:write")}))
	expectHolders(env, editorID, "user-a", "user-b")
	expectReducerRuns(env)
	expectDefineTemplate(env)
	stageTail(env, appTemplateRows())

	_, err := Reconcile(context.Background(), env.appDB, env.identityDB,
		definer(tmpl("editor", "state:read")), env.reducer())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(env.reduced) != 1 {
		t.Fatalf("the reducer was called %d times, want exactly once", len(env.reduced))
	}
	got := env.reduced[0]
	if len(got) != 1 {
		t.Fatalf("reduced = %+v, want exactly the narrowed template", got)
	}
	r := got[0]
	if r.Name != "editor" || r.ID != editorID {
		t.Errorf("reduced template = %s/%s, want editor/%s", r.Name, r.ID, editorID)
	}
	if len(r.Was) != 2 || len(r.Now) != 1 {
		t.Errorf("Was/Now = %v/%v, want the old and new scope lists", r.Was, r.Now)
	}
	if len(r.Holders) != 2 || r.Holders[0] != "user-a" || r.Holders[1] != "user-b" {
		t.Errorf("Holders = %v, want the principals holding the template", r.Holders)
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("statements or their order did not match: %v", err)
	}
}

// GUARD reduction-failure-stops-the-narrowing. A build whose credentials cannot
// be invalidated does not get to reduce the role.
//
// THE ASSERTION IS WHICH ERROR, not that there was one. Staging no INSERT means
// a mutant that ignores the reducer's error still fails — on the unexpected
// statement — and an earlier version of this test accepted that as a pass,
// which made it inert against the exact mutation it names. Requiring the
// reducer's own error to come back distinguishes "the narrowing was refused"
// from "the mock refused the narrowing".
//
// MUTATION: log the reducer's error and carry on to the write.
func TestReduction_FailureStopsTheNarrowing(t *testing.T) {
	env := newReductionEnv(t)
	env.fail = errSweepFailed
	stageThroughPreImage(env, priorRows([2]interface{}{editorID, tmpl("editor", "state:read", "state:write")}))
	expectHolders(env, editorID, "user-a")
	expectReducerRuns(env)
	// Nothing further: the narrowing must not be written.

	_, err := Reconcile(context.Background(), env.appDB, env.identityDB,
		definer(tmpl("editor", "state:read")), env.reducer())
	if !errors.Is(err, errSweepFailed) {
		t.Fatalf("error = %v, want the reducer's own failure: the narrowing must be refused BECAUSE the "+
			"credentials could not be invalidated, not because some later statement happened to fail", err)
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("a statement was issued after the reduction failed: %v", err)
	}
}

// GUARD reduction-requires-a-decision. A nil reducer is a caller that did not
// decide, and this is the axis where "did not decide" means "every holder keeps
// exercising scopes the build removed".
//
// Like the test above, this asserts WHICH error: with no expectations staged at
// all, a build that accepted a nil reducer still fails on its first statement,
// and asserting "an error came back" accepted that.
//
// MUTATION: treat nil as NoTemplateAuthorityReduction.
func TestReduction_RefusesWithoutAReducer(t *testing.T) {
	env := newReductionEnv(t)
	_, err := Reconcile(context.Background(), env.appDB, env.identityDB, definer(tmpl("editor", "state:read")), nil)
	if !errors.Is(err, ErrNoTemplateAuthorityReducer) {
		t.Fatalf("error = %v, want ErrNoTemplateAuthorityReducer: a nil reducer must be refused before "+
			"anything runs, not discovered when a statement fails", err)
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("a refusal issued statements: %v", err)
	}
}

// ---------------------------------------------------------------------------
// What is NOT a reduction
// ---------------------------------------------------------------------------

// GUARD reduction-is-not-mere-change. Widening a role, reordering its scope
// list, and re-seeding the identical definition all take nothing away, so none
// of them may invalidate anything: ending every holder's session for a boot that
// granted MORE is pure damage. No holders query is staged, so reaching one is a
// failure.
//
// MUTATION: key the decision off "the scope list differs" instead of
// AuthorityRetained.
func TestReduction_WideningReorderingAndReseedingInvalidateNothing(t *testing.T) {
	cases := []struct {
		name  string
		prior Template
		next  Template
	}{
		{"widening", tmpl("editor", "state:read"), tmpl("editor", "state:read", "state:write")},
		{"reordering", tmpl("editor", "state:read", "state:write"), tmpl("editor", "state:write", "state:read")},
		{"identical re-seed", tmpl("editor", "state:read"), tmpl("editor", "state:read")},
		{"read promoted to write", tmpl("editor", "state:read"), tmpl("editor", "state:write")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newReductionEnv(t)
			stageThroughPreImage(env, priorRows([2]interface{}{editorID, tc.prior}))
			expectDefineTemplate(env)
			stageTail(env, appTemplateRows())

			if _, err := Reconcile(context.Background(), env.appDB, env.identityDB,
				definer(tc.next), env.reducer()); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if len(env.reduced) != 0 {
				t.Errorf("the reducer ran for a change that took nothing away: %+v", env.reduced)
			}
			if err := env.app.ExpectationsWereMet(); err != nil {
				t.Errorf("statements did not match — a holders query means the change was read as a reduction: %v", err)
			}
		})
	}
}

// GUARD reduction-honours-the-read-write-grammar. "state:write" grants
// "state:read", so promoting a role from read to write is a WIDENING. The
// predicate is only told that by the application's read/write pairs; with nil
// pairs it reads as a reduction and logs every holder out of a change that gave
// them more.
//
// The case above covers the behaviour; this one pins the reason, so a future
// edit that passes nil fails with a message naming the grammar rather than a
// mysterious extra query.
//
// MUTATION: pass nil instead of auth.ReadWritePairs() to AuthorityRetained.
func TestReduction_ReadWriteGrammarIsSupplied(t *testing.T) {
	env := newReductionEnv(t)
	stageThroughPreImage(env, priorRows([2]interface{}{editorID, tmpl("editor", "state:read")}))
	expectDefineTemplate(env)
	stageTail(env, appTemplateRows())

	if _, err := Reconcile(context.Background(), env.appDB, env.identityDB,
		definer(tmpl("editor", "state:write")), env.reducer()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(env.reduced) != 0 {
		t.Fatalf("promoting state:read to state:write was read as a reduction: the read/write pairs "+
			"were not passed to AuthorityRetained, so write no longer implies read: %+v", env.reduced)
	}
}

// GUARD reduction-skips-a-name-the-deployment-does-not-hold. On a first boot
// every name is new: there is no prior authority to reduce, and nobody holds the
// template, so nothing may be invalidated.
//
// MUTATION: drop the "does this deployment already hold the name" check and
// treat an absent prior as an empty scope list, which makes every definition on
// a fresh install look like a narrowing.
func TestReduction_FirstBootInvalidatesNothing(t *testing.T) {
	env := newReductionEnv(t)
	stageThroughPreImage(env, appTemplateRows()) // holds nothing at all
	expectDefineTemplate(env)
	expectDefineTemplate(env)
	stageTail(env, appTemplateRows())

	if _, err := Reconcile(context.Background(), env.appDB, env.identityDB,
		definer(tmpl("editor", "state:read"), tmpl("viewer", "state:read")), env.reducer()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(env.reduced) != 0 {
		t.Errorf("a first boot invalidated credentials: %+v", env.reduced)
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("statements did not match: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Reporting
// ---------------------------------------------------------------------------

// GUARD reduction-is-reported. A boot that narrows a role changes what every
// holder may do, and the operator's only other evidence is a diff between two
// images. The report carries it so LogReport can say so at WARN.
//
// MUTATION: call the reducer but leave Report.TemplatesReduced empty.
func TestReduction_IsReported(t *testing.T) {
	env := newReductionEnv(t)
	stageThroughPreImage(env, priorRows(
		[2]interface{}{editorID, tmpl("editor", "state:read", "state:write")},
		[2]interface{}{viewerID, tmpl("viewer", "state:read")},
	))
	expectHolders(env, editorID, "user-a")
	expectReducerRuns(env)
	expectDefineTemplate(env)
	expectDefineTemplate(env)
	stageTail(env, appTemplateRows())

	rep, err := Reconcile(context.Background(), env.appDB, env.identityDB,
		definer(tmpl("editor", "state:read"), tmpl("viewer", "state:read")), env.reducer())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.TemplatesReduced) != 1 {
		t.Fatalf("TemplatesReduced = %+v, want the one narrowed role", rep.TemplatesReduced)
	}
	if rep.TemplatesReduced[0].Name != "editor" {
		t.Errorf("reported %q, want editor", rep.TemplatesReduced[0].Name)
	}
	if got := len(rep.TemplatesReduced[0].Holders); got != 1 {
		t.Errorf("reported %d holders, want 1 — the blast radius is the point of the line", got)
	}
	// LogReport must not panic on the new field.
	LogReport(rep)
}

// GUARD holders-query-binds-the-tenancy. organization_member_roles carries an
// organization_id, and every statement over it binds the caller's scope —
// axis 4 of the dual-write class guard. The reconcile's scope is platform-wide
// and reviewed as such; what must not happen is a statement with no predicate
// at all, which would read one tenant's rows on a deployment whose scope was
// narrowed later.
//
// MUTATION: drop the andScope splice from HoldersOfTemplate.
func TestReduction_HoldersQueryBindsTheScope(t *testing.T) {
	env := newReductionEnv(t)
	stageThroughPreImage(env, priorRows([2]interface{}{editorID, tmpl("editor", "state:read", "state:write")}))
	// The platform-wide scope renders as a literal TRUE rather than as no
	// clause at all, which is what makes an undecided caller's zero value
	// distinguishable from "every organization".
	env.app.ExpectQuery(`WHERE role_template_id = \$1 AND TRUE`).
		WithArgs(editorID).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("user-a"))
	expectReducerRuns(env)
	expectDefineTemplate(env)
	stageTail(env, appTemplateRows())

	if _, err := Reconcile(context.Background(), env.appDB, env.identityDB,
		definer(tmpl("editor", "state:read")), env.reducer()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("the holders query did not carry the tenancy predicate: %v", err)
	}
}

// GUARD every-definition-is-written. The definer no longer writes, so the
// reconcile must — once per definition. A loop that wrote only the narrowed ones
// would leave every other role at whatever an earlier build left behind.
//
// MUTATION: write only the templates in `reduced`.
func TestReduction_EveryDefinitionIsWritten(t *testing.T) {
	env := newReductionEnv(t)
	stageThroughPreImage(env, priorRows([2]interface{}{editorID, tmpl("editor", "state:read")}))
	// Three definitions, three writes, none of them a reduction.
	for i := 0; i < 3; i++ {
		expectDefineTemplate(env)
	}
	stageTail(env, appTemplateRows())

	if _, err := Reconcile(context.Background(), env.appDB, env.identityDB,
		definer(tmpl("editor", "state:read"), tmpl("viewer", "state:read"), tmpl("operator", "state:read")),
		env.reducer()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("not every definition reached the table: %v", err)
	}
}

// GUARD definer-error-writes-nothing. The supplier runs first; if it cannot say
// what this build defines, nothing is compared and nothing is written.
//
// MUTATION: ignore the definer's error and carry on with a nil slice, which
// would write nothing and report six roles defined.
func TestReduction_DefinerErrorWritesNothing(t *testing.T) {
	env := newReductionEnv(t)
	expectVerifyOK(env.app)
	expectDriftProbe(env.reconcileEnv)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))

	boom := errors.New("cannot read this build's roles")
	failing := func(context.Context) ([]Template, error) { return nil, boom }

	_, err := Reconcile(context.Background(), env.appDB, env.identityDB, failing, env.reducer())
	if err == nil {
		t.Fatal("Reconcile succeeded although the definer failed")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap the definer's own failure", err)
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("statements were issued after the definer failed: %v", err)
	}
}

// A compile-time statement of the contract: a definer supplies, it does not
// hold a store to write through. If TemplateDefiner ever regains a *Store
// parameter this stops compiling, which is the point — that signature is what
// let a definition be written without being compared.
var _ TemplateDefiner = func(context.Context) ([]Template, error) { return nil, nil }

// errSweepFailed is the reducer's own failure, distinguishable from every other
// error a reconcile can return.
var errSweepFailed = errors.New("watermark write failed")

// GUARD no-exported-path-writes-a-role-definition. AXIS: the bypass.
//
// The reduction is enforced by the reconcile, and the reconcile is reachable
// only through Reconcile. That is worth nothing if another package can obtain a
// *Store and write a definition directly — which is exactly what
// `approles.NewStore(db).DefineTemplate(...)` was: it compiled, it looked like
// the correct path in review, and it narrowed a role with nothing invalidated.
//
// Keyed on the STATEMENT rather than on a list of method names, so a new writer
// is covered the moment it is written rather than when somebody remembers to add
// it here. The rule: a *Store method whose body issues an INSERT or UPDATE
// against role_templates must be unexported.
//
// MUTATION: re-export defineTemplate or upsertTemplate.
func TestNoExportedStoreMethodWritesARoleDefinition(t *testing.T) {
	const definitionTable = "role_templates"

	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("reading store.go: %v", err)
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "store.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing store.go: %v", err)
	}

	var checked, exported []string
	for _, decl := range parsed.Decls {
		fn, isFn := decl.(*ast.FuncDecl)
		if !isFn || fn.Recv == nil || fn.Body == nil {
			continue
		}
		body := string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
		if !strings.Contains(body, "INSERT INTO "+definitionTable) && !strings.Contains(body, "UPDATE "+definitionTable) {
			continue
		}
		checked = append(checked, fn.Name.Name)
		if ast.IsExported(fn.Name.Name) {
			exported = append(exported, fn.Name.Name)
		}
	}

	// A guard that enumerated nothing passes for the same reason a clean one
	// does: if the writers moved out of store.go, this stops being evidence.
	if len(checked) == 0 {
		t.Fatalf("no method in store.go writes %s: the guard inspected an empty universe "+
			"(the table was renamed, or the definition writers moved elsewhere)", definitionTable)
	}
	if len(exported) > 0 {
		sort.Strings(exported)
		t.Fatalf("these methods write a role definition and are EXPORTED: %v.\n"+
			"An exported writer is a complete bypass of the reduction in Reconcile — a caller can narrow a "+
			"role with nothing invalidated, and the call compiles and reviews cleanly. Unexport it and route "+
			"the write through Reconcile's definer.", exported)
	}
}

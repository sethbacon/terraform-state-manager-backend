//go:build integration

package approles

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// The half of the role-template authority reduction (#557) that only a real
// PostgreSQL can establish.
//
// The sqlmock suite in template_reduction_test.go pins the sequence: pre-image,
// decide, invalidate, write. What it cannot show is that the decision is made
// against scopes a real jsonb column round-trips, that the holders query finds
// the rows migration 000032's schema actually stores, or that the watermark the
// application writes lands in the table middleware.AuthMiddleware reads. Those
// are properties of Postgres and of two packages agreeing, so they are asserted
// against Postgres.
//
// Run with:
//
//	TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' \
//	  go test -tags integration ./internal/approles/...

// reductionFixture is one role template held by two members in two
// organizations, plus a member holding a DIFFERENT template who must never be
// touched.
type reductionFixture struct {
	templateID  string
	otherID     string
	orgA, orgB  string
	holderA     string
	holderB     string
	otherHolder string
}

// seedReductionFixture puts this build's own role definitions in place through
// the reconcile itself, then assigns them, so the ids are the ones production
// would carry rather than ones the test invented.
func seedReductionFixture(t *testing.T, e *env) reductionFixture {
	t.Helper()
	ctx := context.Background()

	// A first reconcile defines the roles; nothing exists yet, so it reduces
	// nothing. NoTemplateAuthorityReduction is the honest opt-out here: there
	// are no credentials in this fixture until the assignments below.
	if _, err := Reconcile(ctx, e.appDB, e.identityDB, ownTemplates, NoTemplateAuthorityReduction); err != nil {
		t.Fatalf("seed reconcile: %v", err)
	}

	f := reductionFixture{
		orgA:        e.newOrg(t, "reduction-a"),
		orgB:        e.newOrg(t, "reduction-b"),
		holderA:     e.newUser(t, "holder-a@example.test"),
		holderB:     e.newUser(t, "holder-b@example.test"),
		otherHolder: e.newUser(t, "other@example.test"),
	}
	f.templateID = templateIDByName(t, e, "editor")
	f.otherID = templateIDByName(t, e, "viewer")

	assignRole(t, e, f.orgA, f.holderA, f.templateID)
	assignRole(t, e, f.orgB, f.holderB, f.templateID)
	// The same principal holding the narrowed template in a SECOND organization:
	// one principal, one set of credentials, and the watermark write must not
	// try to touch their row twice.
	assignRole(t, e, f.orgB, f.holderA, f.templateID)
	assignRole(t, e, f.orgA, f.otherHolder, f.otherID)

	return f
}

func templateIDByName(t *testing.T, e *env, name string) string {
	t.Helper()
	var id string
	if err := e.appDB.QueryRow(`SELECT id FROM role_templates WHERE name = $1`, name).Scan(&id); err != nil {
		t.Fatalf("reading template id for %q: %v", name, err)
	}
	return id
}

func assignRole(t *testing.T, e *env, orgID, userID, templateID string) {
	t.Helper()
	if _, err := e.appDB.Exec(`
		INSERT INTO organization_member_roles (organization_id, user_id, role_template_id, mirrored_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (organization_id, user_id) DO UPDATE SET role_template_id = EXCLUDED.role_template_id`,
		orgID, userID, templateID); err != nil {
		t.Fatalf("assign role: %v", err)
	}
}

// narrowedEditor is this build's definitions with `editor` reduced to a single
// read scope — the deploy an operator makes when they take a permission away.
func narrowedEditor(ctx context.Context) ([]Template, error) {
	defs, err := ownTemplates(ctx)
	if err != nil {
		return nil, err
	}
	for i := range defs {
		if defs[i].Name == "editor" {
			defs[i].Scopes = []string{"state:read"}
		}
	}
	return defs, nil
}

// widenedEditor adds a scope instead of removing one.
func widenedEditor(ctx context.Context) ([]Template, error) {
	defs, err := ownTemplates(ctx)
	if err != nil {
		return nil, err
	}
	for i := range defs {
		if defs[i].Name == "editor" {
			defs[i].Scopes = append(append([]string{}, defs[i].Scopes...), "admin")
		}
	}
	return defs, nil
}

func watermark(t *testing.T, e *env, userID string) (time.Time, bool) {
	t.Helper()
	var at time.Time
	err := e.appDB.QueryRow(`SELECT revoked_before FROM user_token_revocations WHERE user_id = $1`, userID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false
	}
	if err != nil {
		t.Fatalf("reading watermark for %s: %v", userID, err)
	}
	return at, true
}

func templateScopesNamed(t *testing.T, e *env, name string) string {
	t.Helper()
	var scopes string
	if err := e.appDB.QueryRow(`SELECT scopes::text FROM role_templates WHERE name = $1`, name).Scan(&scopes); err != nil {
		t.Fatalf("reading scopes for %q: %v", name, err)
	}
	return scopes
}

// GUARD narrowing-ends-exactly-its-holders-sessions. The end-to-end claim: a
// deploy that takes a scope away from a role ends the sessions of everyone
// holding it, in every organization, and nobody else's — and the role really is
// narrowed afterwards.
//
// The reducer here writes the same watermark row the production one does, but
// as raw SQL: internal/db/repositories imports THIS package, so its repository
// cannot be reached from these tests at all — the import cycle is the layering
// working. internal/bootstrap's own integration suite runs the real adapter,
// through the real repository, from the real bootstrap.Run.
//
// MUTATION: sweep every member rather than the template's holders; or narrow the
// template without invalidating anything.
func TestIntegrationNarrowingEndsExactlyItsHoldersSessions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	f := seedReductionFixture(t, e)
	var seen []ReducedTemplate
	reduce := func(ctx context.Context, reduced []ReducedTemplate) error {
		seen = reduced
		// The watermark write itself belongs to the application — this package
		// must not know what a credential is, and internal/db/repositories
		// imports this one, so its repository cannot even be reached from here.
		// Spelled as raw SQL against the same table the real adapter writes;
		// internal/bootstrap's integration suite runs the real one.
		for _, r := range reduced {
			for _, u := range r.Holders {
				if _, err := e.appDB.ExecContext(ctx, `
					INSERT INTO user_token_revocations (user_id, revoked_before, updated_at)
					VALUES ($1, NOW(), NOW())
					ON CONFLICT (user_id) DO UPDATE SET revoked_before = EXCLUDED.revoked_before`, u); err != nil {
					return err
				}
			}
		}
		return nil
	}

	rep, err := Reconcile(ctx, e.appDB, e.identityDB, narrowedEditor, reduce)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(seen) != 1 || seen[0].Name != "editor" {
		t.Fatalf("reduced = %+v, want exactly the narrowed editor role", seen)
	}
	// holderA holds it in BOTH organizations and must appear once.
	if got := len(seen[0].Holders); got != 2 {
		t.Errorf("Holders = %v (%d), want 2 distinct principals — a principal holding the role in two "+
			"organizations is one set of credentials", seen[0].Holders, got)
	}
	if len(rep.TemplatesReduced) != 1 {
		t.Errorf("Report.TemplatesReduced = %+v, want the narrowing reported", rep.TemplatesReduced)
	}

	for _, u := range []string{f.holderA, f.holderB} {
		if _, ok := watermark(t, e, u); !ok {
			t.Errorf("holder %s kept every session it had when the role stopped granting the scopes they were minted with", u)
		}
	}
	if _, ok := watermark(t, e, f.otherHolder); ok {
		t.Errorf("a member holding a DIFFERENT role was logged out by another role's narrowing")
	}

	if got := templateScopesNamed(t, e, "editor"); got != `["state:read"]` {
		t.Errorf("editor scopes = %s, want the narrowed list — the write did not land", got)
	}
	// Every other role this build defines is still written on the same boot.
	if got := templateScopesNamed(t, e, "viewer"); got == "" || got == "[]" {
		t.Errorf("viewer scopes = %s, want this build's definition: a boot writes every role, not only the narrowed one", got)
	}
}

// GUARD widening-ends-nothing. Adding a scope takes nothing away, so no session
// may be ended. Under a build that keys off "the definition changed" this test
// logs the whole estate out.
//
// MUTATION: compare the scope lists for equality instead of asking
// AuthorityRetained.
func TestIntegrationWideningEndsNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	f := seedReductionFixture(t, e)

	called := false
	reduce := func(context.Context, []ReducedTemplate) error {
		called = true
		return nil
	}

	if _, err := Reconcile(ctx, e.appDB, e.identityDB, widenedEditor, reduce); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if called {
		t.Error("a widening invalidated credentials")
	}
	for _, u := range []string{f.holderA, f.holderB, f.otherHolder} {
		if _, ok := watermark(t, e, u); ok {
			t.Errorf("%s was logged out by a boot that granted MORE authority", u)
		}
	}
}

// GUARD failed-invalidation-leaves-the-role-alone. The ordering's whole purpose:
// when the credentials cannot be invalidated, the narrowing does not land, so
// the next boot faces the same decision with the same evidence rather than a
// deployment where authority was reduced and nobody was told.
//
// MUTATION: write the definitions first and invalidate afterwards; or treat the
// reducer's error as non-fatal.
func TestIntegrationFailedInvalidationLeavesTheRoleAlone(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	f := seedReductionFixture(t, e)

	before := templateScopesNamed(t, e, "editor")
	boom := errors.New("watermark write failed")
	_, err := Reconcile(ctx, e.appDB, e.identityDB, narrowedEditor,
		func(context.Context, []ReducedTemplate) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the reducer's own failure", err)
	}

	if after := templateScopesNamed(t, e, "editor"); after != before {
		t.Errorf("editor scopes = %s, want the unchanged %s: the narrowing landed although its "+
			"credentials could not be invalidated", after, before)
	}
	for _, u := range []string{f.holderA, f.holderB} {
		if _, ok := watermark(t, e, u); ok {
			t.Errorf("%s was logged out by a reduction that was refused", u)
		}
	}
}

// Package approles is TSM's per-app authorization store: this deployment's own
// role definitions and its own record of which role each organization member
// holds HERE.
//
// # Why this exists
//
// TSM owns no identity tables. It constructs the shared library's repositories
// against the identity connection and uses them directly, so "the editor role"
// and "alice is an editor of acme" are, today, rows in identity.role_templates
// and identity.organization_members — state the registry reads and writes too.
// identity.role_templates.name is globally UNIQUE, so both apps seed the same
// six names into the same six rows and overwrite each other's scopes on restart;
// TSM_SUITE_ROLE_SEED_OWNER exists only to arbitrate that. Under
// sethbacon/terraform-suite-identity#206 identity is SHARED and authorization is
// PER-APP: membership stays a fact in identity, the role that member holds in
// this application moves here.
//
// # What this package is in Phase 3a
//
// A mirror, and nothing else. Every authorization decision TSM makes still comes
// from identity.organization_members joined to identity.role_templates. This
// package writes the same facts a second time, into TSM's own tables, so that
// the phase which switches reads has something correct to switch to. Nothing
// observable changes.
//
// Three things keep the mirror honest:
//
//   - Members (members.go) wraps the shared organization repository and
//     overrides every method that can set, change or remove a role. A caller
//     holds a *Members and cannot reach the unwrapped repository, so writing a
//     new assignment path that skips the mirror is not something one can forget
//     to do — it is something one cannot spell.
//   - Reconcile (reconcile.go) rebuilds both tables from identity at every
//     startup, so a mirror write that failed, a row removed by CASCADE, and a
//     row written by the sibling registry all converge instead of accumulating.
//   - dual_write_class_test.go refuses to certify a tree in which the shared
//     repository is constructed anywhere else, or in which a Members method
//     writes one side and not the other.
//
// # Two connections
//
// These tables live on the APP connection, because per-app authorization belongs
// in this app's schema. Identity lives on the identity connection, which may be
// another schema or another database (TSM_IDENTITY_DATABASE_*). The two cannot
// share a transaction, which is why the mirror is a second write rather than a
// second statement, why the ordering rule in members.go exists, and why
// Reconcile is a reader of one and a writer of the other rather than a SQL join.
package approles

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// The tables this package addresses.
//
// SPELLED ONCE, AND UNQUALIFIED, like internal/platformadmin's carrier names:
// the app connection's search_path places them, which is the routing every other
// unqualified name in this repo uses. Migration 000031 refuses to run on a
// connection whose search_path reaches identity, so "unqualified" cannot quietly
// mean "identity's copy".
const (
	// TemplatesTable holds TSM's own role -> scope mapping.
	TemplatesTable = "role_templates"
	// AssignmentsTable holds (organization_id, user_id) -> role_template_id for
	// this application.
	AssignmentsTable = "organization_member_roles"

	// identityOwnedTable is a relation identity owns and TSM never creates. Its
	// visibility from the app connection means the search_path bridges the
	// boundary this package draws, and is what Verify refuses on.
	identityOwnedTable = "organization_members"
)

// ErrNoTemplate reports a role template name or id that does not resolve in
// TSM's own role_templates.
//
// A SENTINEL RATHER THAN A BARE ERROR because the mirror has to tell two cases
// apart that a string comparison would merge: "this deployment has no such role"
// (the identity write about to run, or already run, will report it authoritatively
// — the mirror must not invent a second, differently-worded failure) and "the
// mirror could not reach its own database" (a real fault worth logging as one).
var ErrNoTemplate = errors.New("approles: no such role template in this application's schema")

// ErrMisrouted reports an app connection whose search_path resolves identity's
// tables, so writes intended for TSM's own schema would land in the shared one.
var ErrMisrouted = errors.New("approles: the app connection resolves identity's tables unqualified")

// Store is TSM's per-app authorization tables on the app connection.
type Store struct {
	db *sql.DB
}

// NewStore constructs the store over the APPLICATION connection.
//
// It performs no I/O. A connection that is down, or routed into identity, is a
// startup failure to report from Verify with the resolved names, not a
// constructor that half-works — the same shape internal/platformadmin.New takes.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Template is one role definition as this package moves it between the two
// connections. It is deliberately not the shared library's models.RoleTemplate:
// that type is the identity schema's row, and this table is TSM's.
type Template struct {
	ID          string
	Name        string
	DisplayName string
	Description *string
	Scopes      []string
	IsSystem    bool
}

// Verify asserts at startup that the two tables exist on the connection this
// store was given, that they are TSM's and not identity's, and returns where
// each one actually resolved.
//
// THE RESOLVED NAMES ARE THE POINT, as they are for the platform-admin carrier.
// Both names are unqualified and placed by the connection's search_path, so a
// deployment that changes that path sees it in the startup line rather than
// discovering it as a mirror that has been writing into the shared identity
// table all along — which is the one failure this whole phase exists to prevent
// and the one an empty diff would never reveal.
//
// A failure is fatal to startup by design.
func (s *Store) Verify(ctx context.Context) (templates, assignments string, err error) {
	if s == nil || s.db == nil {
		return "", "", fmt.Errorf("%w: no application database connection", ErrMisrouted)
	}

	var identityVisible sql.NullString
	if err := s.db.QueryRowContext(ctx, qualifiedNameQuery, identityOwnedTable).Scan(&identityVisible); err != nil {
		return "", "", fmt.Errorf("approles: probing for identity tables on the app connection: %w", err)
	}
	if identityVisible.Valid {
		var searchPath string
		_ = s.db.QueryRowContext(ctx, `SHOW search_path`).Scan(&searchPath)
		return "", "", fmt.Errorf(
			"%w: %s resolves to %s (search_path=%s); remove the identity search_path override from the application connection",
			ErrMisrouted, identityOwnedTable, identityVisible.String, searchPath)
	}

	if templates, err = s.resolve(ctx, TemplatesTable); err != nil {
		return "", "", err
	}
	if assignments, err = s.resolve(ctx, AssignmentsTable); err != nil {
		return "", "", err
	}
	return templates, assignments, nil
}

// qualifiedNameQuery resolves an unqualified table name through the
// connection's search_path and returns it SCHEMA-QUALIFIED, or NULL.
//
// Not `to_regclass($1)::text`, which is the obvious spelling and the wrong one:
// regclass's text output OMITS the schema whenever the relation is visible on
// the search_path — which is always, here — so it would answer "role_templates"
// to the question "which role_templates?". The startup line and the misrouting
// error exist precisely to answer that question, and a value that can never
// name the identity schema cannot report the failure this guard is for.
//
// Wrapped in a scalar sub-select so it ALWAYS returns exactly one row: the join
// form returns none when to_regclass is NULL, and "no rows" for "the table is
// absent" is the same answer QueryRow gives for "the query is broken".
const qualifiedNameQuery = `
	SELECT (SELECT n.nspname || '.' || c.relname
	          FROM pg_class c
	          JOIN pg_namespace n ON n.oid = c.relnamespace
	         WHERE c.oid = to_regclass($1))`

// resolve returns the schema-qualified name an unqualified table resolves to on
// this connection, or an error naming the table that is missing.
func (s *Store) resolve(ctx context.Context, table string) (string, error) {
	var qualified sql.NullString
	if err := s.db.QueryRowContext(ctx, qualifiedNameQuery, table).Scan(&qualified); err != nil {
		return "", fmt.Errorf("approles: resolving %s: %w", table, err)
	}
	if !qualified.Valid {
		return "", fmt.Errorf("approles: %s does not exist on the application connection (migration 000031 has not been applied)", table)
	}
	return qualified.String, nil
}

// UpsertTemplate writes a role definition, keyed by id.
//
// KEYED BY ID, NOT BY NAME, because the id is carried over from identity and is
// what organization_member_roles stores. A conflict on name with a DIFFERENT id
// would be identity having recreated the template — the ON CONFLICT (name) arm
// re-points the row at the new id rather than failing, so the reconcile
// converges instead of wedging on a template somebody dropped and recreated.
func (s *Store) UpsertTemplate(ctx context.Context, t Template) error {
	scopes, err := json.Marshal(nonNilScopes(t.Scopes))
	if err != nil {
		return fmt.Errorf("approles: encoding scopes for role template %q: %w", t.Name, err)
	}
	const q = `
		INSERT INTO role_templates (id, name, display_name, description, scopes, is_system, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, now(), now())
		ON CONFLICT (id) DO UPDATE
		   SET name         = EXCLUDED.name,
		       display_name = EXCLUDED.display_name,
		       description  = EXCLUDED.description,
		       scopes       = EXCLUDED.scopes,
		       is_system    = EXCLUDED.is_system,
		       updated_at   = now()`
	if _, err := s.db.ExecContext(ctx, q, t.ID, t.Name, t.DisplayName, t.Description, string(scopes), t.IsSystem); err != nil {
		return fmt.Errorf("approles: upserting role template %q: %w", t.Name, err)
	}
	return nil
}

// RepointTemplateName moves a name onto a new id, for the case UpsertTemplate's
// id conflict cannot cover: identity dropped a template and recreated it under
// the same name with a fresh uuid, so the app table holds the old id and the
// unique index on name rejects the insert.
//
// The old row's assignments are NOT rewritten here. The reconcile's own
// assignment pass restates every (organization_id, user_id) from identity
// afterwards, which re-points them at the new id as a side effect of restating
// them; doing it twice would be two answers to one question.
func (s *Store) RepointTemplateName(ctx context.Context, name, newID string) error {
	const q = `DELETE FROM role_templates WHERE name = $1 AND id <> $2`
	if _, err := s.db.ExecContext(ctx, q, name, newID); err != nil {
		return fmt.Errorf("approles: repointing role template %q: %w", name, err)
	}
	return nil
}

// TemplateIDByName resolves a role name to TSM's own template id.
//
// Returns ErrNoTemplate when the name does not resolve. The mirror's callers
// treat that as "let the identity write report it": the shared repository does
// the authoritative name lookup and produces the message the operator sees, and
// a second message from the mirror would be a different wording for the same
// fact.
func (s *Store) TemplateIDByName(ctx context.Context, name string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM role_templates WHERE name = $1`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %q", ErrNoTemplate, name)
	}
	if err != nil {
		return "", fmt.Errorf("approles: looking up role template %q: %w", name, err)
	}
	return id, nil
}

// TemplateExists reports whether an id is present in TSM's own role_templates.
func (s *Store) TemplateExists(ctx context.Context, id string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM role_templates WHERE id = $1`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("approles: checking role template %s: %w", id, err)
	}
	return true, nil
}

// andScope splices the caller's tenancy into a statement over
// organization_member_roles, mirroring the shared store's own andScope.
//
// TENANCY IS A PROPERTY OF THE STATEMENT, NOT OF THE CALLER. An earlier version
// of this package checked the scope in the layer above (Members.permits) and
// left these statements unqualified. That closed the hole, but it is the shape
// the identity module's #138/#162 rejected: a hand-rolled guard per call site,
// where every new access axis has to remember to re-apply it and an omission is
// invisible. OrgScope.SQL is exported for exactly this — "the predicate builder,
// for a consumer's own tables" — so the predicate lives in the SQL and an
// unscoped caller cannot express a statement that reaches another tenant.
//
// FAIL-CLOSED BY THE TYPE'S CONTRACT. SQL never returns an empty clause: the
// platform-wide scope is the literal TRUE and the empty set is the literal
// FALSE, both visible in the statement the database receives. So a zero-value
// OrgScope — the value a caller who has not decided holds — matches nothing
// rather than everything.
//
// Callers MUST pass args as it stands at the splice point: the placeholder index
// is derived from its length. Apply the scope FIRST, before any other filter, as
// the shared store does.
func andScope(query string, scope idstore.OrgScope, column string, args []interface{}) (string, []interface{}) {
	clause, scopeArgs := scope.SQL(column, len(args)+1)
	// #nosec G202 -- clause comes from OrgScope.SQL: one of "TRUE", "FALSE", or a
	// fixed template over a column constant spelled in this file and a $N
	// placeholder. Scope values travel as query arguments and are never
	// interpolated.
	return query + " AND " + clause, append(args, scopeArgs...)
}

// SetRole records that a member holds roleTemplateID in an organization.
//
// A nil roleTemplateID is a member with no role, which identity represents too
// (AddMemberWithRoleTemplate takes a *string and organization_members.role_template_id
// is nullable). It is stored as NULL rather than refused, so the mirror can
// represent every state the thing it mirrors can.
//
// created_at is preserved across an update: this is the same assignment being
// restated, and overwriting it would make every reconcile look like a fresh
// grant to anybody reading the table for provenance.
// INSERT ... SELECT ... WHERE <scope>, not a plain VALUES, for the same reason
// the shared store's AddMemberWithRoleTemplate sources its insert from a scoped
// SELECT: the create axis has no existing row for a WHERE to filter, so without
// this the strongest of the three axes would be the one left open. Recording a
// role is a privilege GRANT. Out of scope, the SELECT produces no row, the
// insert writes nothing, and the ON CONFLICT arm is never reached.
func (s *Store) SetRole(ctx context.Context, orgID, userID string, roleTemplateID *string, scope idstore.OrgScope) error {
	query := `
		INSERT INTO organization_member_roles (organization_id, user_id, role_template_id, created_at, updated_at, mirrored_at)
		SELECT v.organization_id, v.user_id, v.role_template_id, now(), now(), now()
		FROM (VALUES ($1::uuid, $2::uuid, $3::uuid)) AS v(organization_id, user_id, role_template_id)
		WHERE TRUE`
	args := []interface{}{orgID, userID, roleTemplateID}
	query, args = andScope(query, scope, "v.organization_id", args)
	query += `
		ON CONFLICT (organization_id, user_id) DO UPDATE
		   SET role_template_id = EXCLUDED.role_template_id,
		       updated_at       = now(),
		       mirrored_at      = now()`
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("approles: recording role for org=%s user=%s: %w", orgID, userID, err)
	}
	return nil
}

// DeleteRole removes one member's role record.
//
// Removing nothing is not an error. This runs BEFORE its identity counterpart
// (see members.go's ordering rule) and is replayed by the reconcile, so "the row
// was already gone" is the desired end state, not a miss.
func (s *Store) DeleteRole(ctx context.Context, orgID, userID string, scope idstore.OrgScope) error {
	query := `DELETE FROM organization_member_roles WHERE organization_id = $1 AND user_id = $2`
	args := []interface{}{orgID, userID}
	query, args = andScope(query, scope, "organization_id", args)
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("approles: removing role for org=%s user=%s: %w", orgID, userID, err)
	}
	return nil
}

// DeleteRolesForUser removes a user's role records within scope.
//
// The scope IS the "which organizations" argument: an earlier version took a nil
// slice for "everywhere" and an empty one for "nowhere", which is the same
// distinction OrgScope already makes and makes better — nil versus empty is one
// character and reads identically at a call site, while
// OrgScopeAllOrganizations() is greppable and is exactly what
// TestPlatformWideOrgScopeSitesAreReviewed enumerates.
func (s *Store) DeleteRolesForUser(ctx context.Context, userID string, scope idstore.OrgScope) error {
	query := `DELETE FROM organization_member_roles WHERE user_id = $1`
	args := []interface{}{userID}
	query, args = andScope(query, scope, "organization_id", args)
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("approles: removing roles for user=%s: %w", userID, err)
	}
	return nil
}

// DeleteRolesForOrganization removes every role record in an organization.
//
// identity.organization_members.organization_id is ON DELETE CASCADE, so
// deleting an organization there withdraws every member's authority with no
// membership statement of its own. This table has no foreign key to reach it
// (identity may be another database), so the cascade has to be mirrored
// explicitly — see Members.Delete.
func (s *Store) DeleteRolesForOrganization(ctx context.Context, orgID string, scope idstore.OrgScope) error {
	query := `DELETE FROM organization_member_roles WHERE organization_id = $1`
	args := []interface{}{orgID}
	query, args = andScope(query, scope, "organization_id", args)
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("approles: removing roles for org=%s: %w", orgID, err)
	}
	return nil
}

// Generation returns the APP DATABASE's clock, for the reconcile's sweep.
//
// The database's clock and not Go's: mirrored_at is set by now() inside the
// statement, so a generation taken from the process would be comparing two
// clocks, and on a replica whose clock runs a little ahead it would sweep away
// rows the reconcile had just written.
func (s *Store) Generation(ctx context.Context) (time.Time, error) {
	var t time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT now()`).Scan(&t); err != nil {
		return time.Time{}, fmt.Errorf("approles: reading the database clock: %w", err)
	}
	return t, nil
}

// SweepStaleAssignments removes role records not restated since generation, and
// returns how many it removed.
//
// This is how an assignment that vanished from identity WITHOUT passing through
// this application's code stops being mirrored: an organization or user deleted
// by CASCADE in another process, a membership removed by the sibling registry, a
// mirror write whose identity leg committed and whose app leg did not.
//
// ONLY EVER CALLED AFTER A COMPLETE PASS. A read failure part-way through the
// identity stream would leave the un-restated remainder looking stale, and this
// would delete every one of them — turning a transient identity outage into a
// wiped mirror. Reconcile enforces that; this method cannot, which is why it is
// stated here rather than only there.
func (s *Store) SweepStaleAssignments(ctx context.Context, generation time.Time, scope idstore.OrgScope) (int64, error) {
	query := `DELETE FROM organization_member_roles WHERE mirrored_at < $1`
	args := []interface{}{generation}
	query, args = andScope(query, scope, "organization_id", args)
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("approles: sweeping stale role records: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // the sweep ran; the driver just cannot say how much
	}
	return n, nil
}

// nonNilScopes keeps an absent scope list encoding as [] rather than null, so the
// column's JSONB always holds an array and a reader never has to handle two
// spellings of "no scopes".
func nonNilScopes(scopes []string) []string {
	if scopes == nil {
		return []string{}
	}
	return scopes
}

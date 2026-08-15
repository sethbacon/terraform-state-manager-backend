package approles

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The equivalence check this phase is gated on.
//
// # Why DriftQuery was not enough
//
// Phase 3a shipped DriftQuery, a single SQL statement naming both schemas. It
// answered the question it was written for, and its own doc recorded why that
// question was low-stakes: "In Phase 3a no authorization decision reads the
// mirrored table, so drift denies nobody and grants nobody." That sentence stops
// being true in this phase, and re-reading it is what turned up two things it
// cannot see.
//
//	1. A SEPARATE IDENTITY DATABASE. With TSM_IDENTITY_DATABASE_* set there is no
//	   statement that can join the two, so DriftQuery does not run at all — on
//	   exactly the topology where the two halves are furthest apart and a mirror
//	   write is most likely to have been lost. Its doc told the operator to dump
//	   both sides and diff them by hand. A gate an operator performs by hand on
//	   the hardest topology is not a gate. This compares two ordered cursors in
//	   process, so it runs identically on one database or two.
//
//	2. ROLE DEFINITIONS. DriftQuery compares role_template_id and nothing else.
//	   Two rows can agree on that id while identity.role_templates and this
//	   application's role_templates grant DIFFERENT SCOPES for it — which, from
//	   the moment reads move, is precisely a principal holding the wrong
//	   permissions with the right role name. That is the failure mode this phase
//	   is most exposed to and the one an id comparison is blind to. So the scopes
//	   are compared too, per template (TemplateDrift) and per affected principal
//	   (ScopeDivergent).
//
// # What it deliberately does not do
//
// It does not correct anything. Reconcile corrects; this reports. Keeping the two
// apart is what lets the same code be the pre-flip gate, the boot-time report of
// what the reconcile is ABOUT to change, and the standing detector — three uses of
// one comparison rather than three implementations that can disagree.

// driftSampleLimit bounds the records carried back for logging and for the CLI.
//
// The COUNTS are exact and unbounded; only the naming of individual pairs is
// capped. An estate that has genuinely lost thousands of assignments needs the
// number, and needs the first few to diagnose with — it does not need a log line
// per principal, which is how a real incident becomes an unreadable one.
const driftSampleLimit = 50

// DriftKind classifies one disagreement between identity and this application.
type DriftKind string

const (
	// DriftMissing: identity has the membership, this application records no
	// role for it. The principal LOSES access they should have.
	DriftMissing DriftKind = "missing"
	// DriftStale: this application records a role for a membership identity no
	// longer has. The principal KEEPS access they should not have.
	DriftStale DriftKind = "stale"
	// DriftMismatched: both sides have it, naming different role templates.
	DriftMismatched DriftKind = "mismatched"
	// DriftScopeDivergent: both sides name the SAME role template id, and the
	// two schemas define that template with different scopes. The role name
	// agrees and the effective permissions do not.
	DriftScopeDivergent DriftKind = "scope-divergent"
)

// DriftRecord is one disagreement, named well enough to act on.
type DriftRecord struct {
	Kind           DriftKind
	OrganizationID string
	UserID         string
	// IdentityRole and AppRole are the role template ids each side records, or
	// "" for absent/NULL.
	IdentityRole string
	AppRole      string
	// Detail carries the scope difference for DriftScopeDivergent, which is not
	// expressible in the two id fields (they are equal, by definition of the
	// kind).
	Detail string
}

// TemplateDrift is one role definition the two schemas disagree about.
type TemplateDrift struct {
	Name string
	// IdentityScopes and AppScopes are sorted, for a stable and diffable report.
	IdentityScopes []string
	AppScopes      []string
	// OnlyIn names the side that has this template at all, when the other does
	// not: "identity" or "app". Empty when both have it.
	OnlyIn string
}

// DriftResult is one complete comparison.
type DriftResult struct {
	// Compared is the number of DISTINCT (organization_id, user_id) pairs seen
	// on either side. Reported so a clean result cannot be confused with a
	// comparison that scanned nothing.
	Compared int

	Missing        int
	Stale          int
	Mismatched     int
	ScopeDivergent int

	// Sample holds up to driftSampleLimit records, in scan order.
	Sample []DriftRecord
	// TemplateDrift names every role definition the two schemas disagree about.
	TemplateDrift []TemplateDrift
}

// AssignmentDrift is the number of principals whose ROLE ASSIGNMENT disagrees.
//
// Separated from template drift because the two mean different things after the
// flip. An assignment disagreement is always a fault: the dual write missed, or a
// row vanished from one side. A template disagreement is a fault before the flip
// and an INTENDED state after it — a coupled deployment's identity schema holds
// the sibling's definition of `editor` and this application now holds its own,
// which is the entire point of sethbacon/terraform-suite-identity#206.
func (r DriftResult) AssignmentDrift() int {
	return r.Missing + r.Stale + r.Mismatched
}

// Clean reports whether the two sides are equivalent in every respect.
//
// THE GATE'S CONDITION, and it includes template drift on purpose. Before the
// flip the two schemas' templates are verbatim copies of each other (Phase 3a's
// reconcile made them so), so requiring equality is requiring the state the flip
// is safe from. An operator who cannot get this to zero must not upgrade onto a
// build that reads the mirror.
func (r DriftResult) Clean() bool {
	return r.AssignmentDrift() == 0 && r.ScopeDivergent == 0 && len(r.TemplateDrift) == 0
}

// String renders the result for the CLI and the startup line.
func (r DriftResult) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "compared=%d missing=%d stale=%d mismatched=%d scope_divergent=%d template_drift=%d",
		r.Compared, r.Missing, r.Stale, r.Mismatched, r.ScopeDivergent, len(r.TemplateDrift))
	for _, t := range r.TemplateDrift {
		if t.OnlyIn != "" {
			fmt.Fprintf(&b, "\n  template %q exists only in %s", t.Name, t.OnlyIn)
			continue
		}
		fmt.Fprintf(&b, "\n  template %q: identity=%v app=%v", t.Name, t.IdentityScopes, t.AppScopes)
	}
	for _, s := range r.Sample {
		fmt.Fprintf(&b, "\n  %s org=%s user=%s identity_role=%s app_role=%s%s",
			s.Kind, s.OrganizationID, s.UserID, orNone(s.IdentityRole), orNone(s.AppRole), s.Detail)
	}
	if len(r.Sample) == driftSampleLimit {
		fmt.Fprintf(&b, "\n  (sample truncated at %d records; the counts above are exact)", driftSampleLimit)
	}
	return b.String()
}

func orNone(v string) string {
	if v == "" {
		return "<none>"
	}
	return v
}

// CheckDrift compares this application's authorization tables against identity's
// and reports every way they disagree.
//
// It writes nothing, on either connection.
func CheckDrift(ctx context.Context, appDB, identityDB *sql.DB) (DriftResult, error) {
	var res DriftResult
	if appDB == nil {
		return res, fmt.Errorf("%w: no application database connection", ErrMisrouted)
	}
	if identityDB == nil {
		return res, errors.New("approles: no identity database connection to compare against")
	}

	identityTemplates, appTemplates, err := loadTemplates(ctx, appDB, identityDB)
	if err != nil {
		return res, err
	}
	res.TemplateDrift = compareTemplates(identityTemplates, appTemplates)

	identityScopes := scopesByID(identityTemplates)
	appScopes := scopesByID(appTemplates)

	// ORDERED IDENTICALLY ON BOTH SIDES, AND IN THE ORDER GO COMPARES STRINGS.
	// The merge below advances whichever cursor is behind, so a single ordering
	// disagreement between the two connections does not produce a wrong answer —
	// it produces a comparison that reports every row as both missing and stale.
	// `::text COLLATE "C"` is byte order, which is what Go's `<` on the scanned
	// strings is; the database's default collation is not (a locale collation can
	// order the hyphens in a uuid differently), and neither is uuid order once it
	// has been rendered to text.
	const orderBy = ` ORDER BY organization_id::text COLLATE "C", user_id::text COLLATE "C"`
	identityRows, err := identityDB.QueryContext(ctx,
		`SELECT organization_id::text, user_id::text, COALESCE(role_template_id::text, '') FROM organization_members`+orderBy)
	if err != nil {
		return res, fmt.Errorf("approles: reading identity memberships for the drift comparison: %w", err)
	}
	defer func() { _ = identityRows.Close() }()

	appRows, err := appDB.QueryContext(ctx,
		`SELECT organization_id::text, user_id::text, COALESCE(role_template_id::text, '') FROM organization_member_roles`+orderBy)
	if err != nil {
		return res, fmt.Errorf("approles: reading this application's role records for the drift comparison: %w", err)
	}
	defer func() { _ = appRows.Close() }()

	left := newAssignmentCursor(identityRows)
	right := newAssignmentCursor(appRows)
	if err := left.next(); err != nil {
		return res, fmt.Errorf("approles: reading identity memberships for the drift comparison: %w", err)
	}
	if err := right.next(); err != nil {
		return res, fmt.Errorf("approles: reading this application's role records for the drift comparison: %w", err)
	}

	for left.ok || right.ok {
		switch {
		case right.ok && (!left.ok || right.less(left)):
			res.Compared++
			res.Stale++
			res.record(DriftRecord{Kind: DriftStale, OrganizationID: right.orgID, UserID: right.userID, AppRole: right.role})
			if err := right.next(); err != nil {
				return res, fmt.Errorf("approles: reading this application's role records for the drift comparison: %w", err)
			}
		case left.ok && (!right.ok || left.less(right)):
			res.Compared++
			res.Missing++
			res.record(DriftRecord{Kind: DriftMissing, OrganizationID: left.orgID, UserID: left.userID, IdentityRole: left.role})
			if err := left.next(); err != nil {
				return res, fmt.Errorf("approles: reading identity memberships for the drift comparison: %w", err)
			}
		default:
			res.Compared++
			switch {
			case left.role != right.role:
				res.Mismatched++
				res.record(DriftRecord{Kind: DriftMismatched, OrganizationID: left.orgID, UserID: left.userID,
					IdentityRole: left.role, AppRole: right.role})
			case left.role != "" && !sameScopeSet(identityScopes[left.role], appScopes[right.role]):
				res.ScopeDivergent++
				res.record(DriftRecord{Kind: DriftScopeDivergent, OrganizationID: left.orgID, UserID: left.userID,
					IdentityRole: left.role, AppRole: right.role,
					Detail: fmt.Sprintf(" identity_scopes=%v app_scopes=%v",
						sorted(identityScopes[left.role]), sorted(appScopes[right.role]))})
			}
			if err := left.next(); err != nil {
				return res, fmt.Errorf("approles: reading identity memberships for the drift comparison: %w", err)
			}
			if err := right.next(); err != nil {
				return res, fmt.Errorf("approles: reading this application's role records for the drift comparison: %w", err)
			}
		}
	}
	return res, nil
}

// record appends to the bounded sample. The counts are incremented by the caller
// and are never bounded.
func (r *DriftResult) record(rec DriftRecord) {
	if len(r.Sample) < driftSampleLimit {
		r.Sample = append(r.Sample, rec)
	}
}

// assignmentCursor is one side of the merge: the current row, and whether there
// is one.
type assignmentCursor struct {
	rows                *sql.Rows
	orgID, userID, role string
	ok                  bool
}

func newAssignmentCursor(rows *sql.Rows) *assignmentCursor { return &assignmentCursor{rows: rows} }

// next advances to the following row, or clears ok at the end of the stream.
//
// rows.Err() IS CHECKED HERE rather than after the loop. A read that fails
// part-way looks exactly like the end of the stream to Next(), and the merge
// would then report every remaining row on the OTHER side as drift — turning a
// transient database fault into a report claiming the whole estate is unmirrored,
// which is the shape that would send an operator rolling back a correct flip.
func (c *assignmentCursor) next() error {
	if !c.rows.Next() {
		c.ok = false
		return c.rows.Err()
	}
	if err := c.rows.Scan(&c.orgID, &c.userID, &c.role); err != nil {
		c.ok = false
		return err
	}
	c.ok = true
	return nil
}

// less orders two cursors by (organization_id, user_id), matching the query's
// C-collated ordering.
func (c *assignmentCursor) less(other *assignmentCursor) bool {
	if c.orgID != other.orgID {
		return c.orgID < other.orgID
	}
	return c.userID < other.userID
}

// loadTemplates reads both role definition tables.
//
// Identity's is read with raw SQL rather than through the shared
// RoleTemplateRepository so that the two sides are read the same way, from the
// same columns, and a difference in the report is a difference in the data rather
// than in two decoding paths.
func loadTemplates(ctx context.Context, appDB, identityDB *sql.DB) (identityTemplates, appTemplates map[string]Template, err error) {
	identityTemplates = make(map[string]Template)
	rows, err := identityDB.QueryContext(ctx,
		`SELECT id::text, name, COALESCE(display_name, ''), description, COALESCE(scopes, '[]'::jsonb), is_system FROM role_templates`)
	if err != nil {
		return nil, nil, fmt.Errorf("approles: reading identity role templates for the drift comparison: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		t, serr := scanTemplate(rows)
		if serr != nil {
			return nil, nil, serr
		}
		identityTemplates[t.Name] = t
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("approles: reading identity role templates for the drift comparison: %w", err)
	}

	appTemplates, err = NewStore(appDB).ListTemplates(ctx)
	if err != nil {
		return nil, nil, err
	}
	return identityTemplates, appTemplates, nil
}

// scanTemplate decodes one role definition row, including its JSONB scopes.
//
// It does NOT read created_at/updated_at: this shape exists only to be compared,
// and the comparison is over names and scopes. Store.ListTemplates does read them,
// because its result is also what GET /admin/roles serves.
func scanTemplate(rows *sql.Rows) (Template, error) {
	var t Template
	var scopes []byte
	if err := rows.Scan(&t.ID, &t.Name, &t.DisplayName, &t.Description, &scopes, &t.IsSystem); err != nil {
		return Template{}, fmt.Errorf("approles: reading a role template for the drift comparison: %w", err)
	}
	if err := json.Unmarshal(scopes, &t.Scopes); err != nil {
		return Template{}, fmt.Errorf("approles: decoding scopes for role template %q: %w", t.Name, err)
	}
	return t, nil
}

// compareTemplates reports every role definition the two schemas disagree about,
// keyed by NAME.
//
// By name and not by id, because the name is what an assignment means to an
// operator and what both apps seed. A template present on one side only is
// reported as such rather than as an empty scope set, so "the sibling defines a
// role we do not" reads differently from "we define it with nothing in it".
func compareTemplates(identityTemplates, appTemplates map[string]Template) []TemplateDrift {
	names := make(map[string]struct{}, len(identityTemplates)+len(appTemplates))
	for n := range identityTemplates {
		names[n] = struct{}{}
	}
	for n := range appTemplates {
		names[n] = struct{}{}
	}

	drift := make([]TemplateDrift, 0)
	for name := range names {
		it, inIdentity := identityTemplates[name]
		at, inApp := appTemplates[name]
		switch {
		case inIdentity && !inApp:
			drift = append(drift, TemplateDrift{Name: name, IdentityScopes: sorted(it.Scopes), OnlyIn: "identity"})
		case !inIdentity && inApp:
			drift = append(drift, TemplateDrift{Name: name, AppScopes: sorted(at.Scopes), OnlyIn: "app"})
		case !sameScopeSet(it.Scopes, at.Scopes):
			drift = append(drift, TemplateDrift{Name: name, IdentityScopes: sorted(it.Scopes), AppScopes: sorted(at.Scopes)})
		}
	}
	sort.Slice(drift, func(i, j int) bool { return drift[i].Name < drift[j].Name })
	return drift
}

// scopesByID keys a template set by id, for the per-membership scope comparison.
func scopesByID(templates map[string]Template) map[string][]string {
	out := make(map[string][]string, len(templates))
	for _, t := range templates {
		out[t.ID] = t.Scopes
	}
	return out
}

// sorted returns a sorted copy, so a report is stable and two runs are diffable.
func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

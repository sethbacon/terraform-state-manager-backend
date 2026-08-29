package approles

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
//	2. IT RAN AS ONE STATEMENT, which also meant it could not be reused as the
//	   boot-time report and the periodic detector without a connection that sees
//	   both schemas.
//
// # What it no longer compares: role definitions
//
// Until sethbacon/terraform-suite-identity#206 Phase 3 finished here, this
// comparison also read identity.role_templates and reported scope divergence per
// template and per affected principal. That read is retired with the rest of
// them: under per-app authorization this application DEFINES its own roles, so
// "identity's copy differs" is the designed end state on a coupled deployment,
// not drift — a detector that stays red by design trains an operator to ignore
// the series that also carries real faults. What remains compared is exactly
// what the two sides still share: the membership fact, and the vestigial
// role_template_id column identity carries until Phase 4 drops it.
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
	//
	// Compared by id, and the two id spaces stay aligned only where this
	// application's identity-side seed runs (bootstrap.seedSharedRoleTemplates
	// pushes THIS table's uuids). On a fresh deployment whose identity seed is
	// owned by the sibling, a TSM grant records the sibling's uuid on the
	// identity leg and this application's on the mirror leg, and this kind
	// reads as standing divergence until Phase 4 drops the identity column.
	DriftMismatched DriftKind = "mismatched"
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
	// Detail carries any extra context a kind wants rendered after the ids.
	Detail string
}

// DriftResult is one complete comparison.
type DriftResult struct {
	// Compared is the number of DISTINCT (organization_id, user_id) pairs seen
	// on either side. Reported so a clean result cannot be confused with a
	// comparison that scanned nothing.
	Compared int

	Missing    int
	Stale      int
	Mismatched int

	// Sample holds up to driftSampleLimit records, in scan order.
	Sample []DriftRecord
}

// AssignmentDrift is the number of principals whose ROLE RECORD disagrees.
//
// `missing` and `stale` are always faults of presence: the dual write missed, or
// a row vanished from one side. `mismatched` is a lost mirror role update on an
// id-aligned deployment, and standing divergence on one whose identity seed the
// sibling owns — see DriftMismatched.
func (r DriftResult) AssignmentDrift() int {
	return r.Missing + r.Stale + r.Mismatched
}

// Clean reports whether the two sides agree about every membership.
func (r DriftResult) Clean() bool {
	return r.AssignmentDrift() == 0
}

// String renders the result for the CLI and the startup line.
func (r DriftResult) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "compared=%d missing=%d stale=%d mismatched=%d",
		r.Compared, r.Missing, r.Stale, r.Mismatched)
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
			if left.role != right.role {
				res.Mismatched++
				res.record(DriftRecord{Kind: DriftMismatched, OrganizationID: left.orgID, UserID: left.userID,
					IdentityRole: left.role, AppRole: right.role})
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

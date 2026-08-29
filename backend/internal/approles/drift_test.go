package approles

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// driftEnv is the two-connection rig CheckDrift compares across.
type driftEnv struct {
	env *reconcileEnv
}

func newDriftEnv(t *testing.T) *driftEnv { return &driftEnv{env: newReconcileEnv(t)} }

// stage sets up one comparison: the two assignment streams, in the order
// CheckDrift reads them.
//
// EXACTLY TWO QUERIES, and the mocks are strict: since the template comparison
// was retired with the identity.role_templates reads, a CheckDrift that loaded
// either side's role definitions again would issue a query no test here stages,
// and every test in this file would fail on it.
type assignment struct{ org, user, role string }

func (d *driftEnv) stage(identityRows, appRows []assignment) {
	d.env.identity.ExpectQuery(`FROM organization_members ORDER BY`).WillReturnRows(assignmentRows(identityRows))
	d.env.app.ExpectQuery(`FROM organization_member_roles ORDER BY`).WillReturnRows(assignmentRows(appRows))
}

func assignmentRows(rows []assignment) *sqlmock.Rows {
	out := sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"})
	for _, r := range rows {
		out.AddRow(r.org, r.user, r.role)
	}
	return out
}

func (d *driftEnv) check(t *testing.T) DriftResult {
	t.Helper()
	res, err := CheckDrift(context.Background(), d.env.appDB, d.env.identityDB)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	return res
}

// Two sides that agree are clean, and the comparison says how much it looked at.
// The Compared count is asserted because a merge that read nothing also reports
// no drift, and the two are the same value in every field but that one.
func TestCheckDrift_AgreementIsClean(t *testing.T) {
	d := newDriftEnv(t)
	rows := []assignment{
		{"11111111-0000-0000-0000-000000000001", "22222222-0000-0000-0000-000000000001", adminTemplateID},
		{"11111111-0000-0000-0000-000000000002", "22222222-0000-0000-0000-000000000002", adminTemplateID},
	}
	d.stage(rows, rows)

	res := d.check(t)
	if !res.Clean() {
		t.Fatalf("Clean() = false for two identical sides: %s", res.String())
	}
	if res.Compared != 2 {
		t.Fatalf("Compared = %d, want 2: a comparison that scanned nothing also reports no drift", res.Compared)
	}
}

// THE MERGE IS A MERGE, not a pair of set lookups, so the case that matters is
// interleaving: rows present on one side only, on both sides, in an order that
// forces each branch of the merge to be taken at least once.
func TestCheckDrift_ClassifiesEveryDisagreement(t *testing.T) {
	d := newDriftEnv(t)
	const (
		orgA = "11111111-0000-0000-0000-00000000000a"
		orgB = "11111111-0000-0000-0000-00000000000b"
		orgC = "11111111-0000-0000-0000-00000000000c"
		orgD = "11111111-0000-0000-0000-00000000000d"
		user = "22222222-0000-0000-0000-000000000001"
	)
	d.stage(
		[]assignment{
			{orgA, user, adminTemplateID}, // missing here
			{orgB, user, adminTemplateID}, // agrees
			{orgC, user, adminTemplateID}, // mismatched
		},
		[]assignment{
			{orgB, user, adminTemplateID},
			{orgC, user, editorTemplateID},
			{orgD, user, adminTemplateID}, // stale
		})

	res := d.check(t)
	if res.Missing != 1 || res.Stale != 1 || res.Mismatched != 1 {
		t.Fatalf("missing/stale/mismatched = %d/%d/%d, want 1/1/1: %s", res.Missing, res.Stale, res.Mismatched, res.String())
	}
	if res.Compared != 4 {
		t.Errorf("Compared = %d, want 4 distinct pairs", res.Compared)
	}
	if res.AssignmentDrift() != 3 {
		t.Errorf("AssignmentDrift() = %d, want 3", res.AssignmentDrift())
	}
	kinds := map[DriftKind]string{}
	for _, s := range res.Sample {
		kinds[s.Kind] = s.OrganizationID
	}
	if kinds[DriftMissing] != orgA {
		t.Errorf("missing record names org %q, want %q", kinds[DriftMissing], orgA)
	}
	if kinds[DriftStale] != orgD {
		t.Errorf("stale record names org %q, want %q", kinds[DriftStale], orgD)
	}
	if kinds[DriftMismatched] != orgC {
		t.Errorf("mismatched record names org %q, want %q", kinds[DriftMismatched], orgC)
	}
}

// A READ THAT FAILS PART-WAY LOOKS EXACTLY LIKE THE END OF THE STREAM to
// rows.Next(). Unchecked, the merge would then report every remaining row on the
// OTHER side as drift — turning a transient fault into a report claiming the
// estate is unmirrored, which is the shape that would send an operator rolling
// back a correct flip.
func TestCheckDrift_DoesNotReadAMidStreamFailureAsTheEndOfTheStream(t *testing.T) {
	d := newDriftEnv(t)
	const user = "22222222-0000-0000-0000-000000000001"

	d.env.identity.ExpectQuery(`FROM organization_members ORDER BY`).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}).
			AddRow("11111111-0000-0000-0000-00000000000a", user, adminTemplateID).
			RowError(0, errors.New("connection reset mid-stream")))
	d.env.app.ExpectQuery(`FROM organization_member_roles ORDER BY`).
		WillReturnRows(assignmentRows([]assignment{
			{"11111111-0000-0000-0000-00000000000a", user, adminTemplateID},
			{"11111111-0000-0000-0000-00000000000b", user, adminTemplateID},
		}))

	_, err := CheckDrift(context.Background(), d.env.appDB, d.env.identityDB)
	if err == nil {
		t.Fatal("CheckDrift reported a result from a stream that broke midway: every un-read identity row " +
			"would have been counted as stale on the other side")
	}
	if !strings.Contains(err.Error(), "connection reset mid-stream") {
		t.Fatalf("the row error was swallowed: %v", err)
	}
}

// Both sides empty is clean and honest about it: Compared is zero, which is what
// distinguishes "they agree" from "there was nothing to compare".
func TestCheckDrift_EmptyEstateIsCleanAndSaysSo(t *testing.T) {
	d := newDriftEnv(t)
	d.stage(nil, nil)
	res := d.check(t)
	if !res.Clean() {
		t.Fatalf("Clean() = false on an empty estate: %s", res.String())
	}
	if res.Compared != 0 {
		t.Fatalf("Compared = %d, want 0", res.Compared)
	}
	if !strings.Contains(res.String(), "compared=0") {
		t.Errorf("the report does not disclose that nothing was compared: %s", res.String())
	}
}

// The sample is bounded and the counts are not. An estate that has genuinely lost
// thousands of assignments needs the number and the first few to diagnose with;
// it does not need a log line per principal.
func TestCheckDrift_BoundsTheSampleWithoutBoundingTheCounts(t *testing.T) {
	d := newDriftEnv(t)
	const want = driftSampleLimit + 25
	rows := make([]assignment, 0, want)
	for i := range want {
		rows = append(rows, assignment{
			org:  fmt.Sprintf("11111111-0000-0000-0000-%012d", i),
			user: "22222222-0000-0000-0000-000000000001",
			role: adminTemplateID,
		})
	}
	d.stage(rows, nil)

	res := d.check(t)
	if res.Missing != want {
		t.Fatalf("Missing = %d, want %d: the count must not be capped by the sample", res.Missing, want)
	}
	if len(res.Sample) != driftSampleLimit {
		t.Fatalf("len(Sample) = %d, want %d", len(res.Sample), driftSampleLimit)
	}
	if !strings.Contains(res.String(), "sample truncated") {
		t.Error("the report does not disclose that the sample was truncated, so it reads as the complete list")
	}
}

// A NULL role on both sides is agreement, not two absences that happen to look
// alike. identity.organization_members.role_template_id is nullable and the
// mirror reproduces that, so a member with no role must not be reported as drift
// on every single run.
func TestCheckDrift_ARoleLessMembershipOnBothSidesAgrees(t *testing.T) {
	d := newDriftEnv(t)
	rows := []assignment{{"11111111-0000-0000-0000-00000000000a", "22222222-0000-0000-0000-000000000001", ""}}
	d.stage(rows, rows)
	res := d.check(t)
	if !res.Clean() {
		t.Fatalf("Clean() = false for a role-less membership present on both sides: %s", res.String())
	}
}

// CheckDrift writes nothing, and refuses rather than guessing when it has only
// one side to look at — a comparison against a connection that is not there would
// report the entire other side as drift.
func TestCheckDrift_RefusesWithOnlyOneSide(t *testing.T) {
	d := newDriftEnv(t)
	if _, err := CheckDrift(context.Background(), nil, d.env.identityDB); !errors.Is(err, ErrMisrouted) {
		t.Fatalf("CheckDrift with no app connection: got %v, want ErrMisrouted", err)
	}
	if _, err := CheckDrift(context.Background(), d.env.appDB, nil); err == nil {
		t.Fatal("CheckDrift succeeded with no identity connection to compare against")
	}
}

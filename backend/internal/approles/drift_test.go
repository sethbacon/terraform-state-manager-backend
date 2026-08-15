package approles

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// driftEnv is the two-connection rig CheckDrift compares across.
type driftEnv struct {
	env *reconcileEnv
}

func newDriftEnv(t *testing.T) *driftEnv { return &driftEnv{env: newReconcileEnv(t)} }

// stage sets up one comparison: the two template tables, then the two assignment
// streams, in the order CheckDrift reads them.
type assignment struct{ org, user, role string }

func (d *driftEnv) stage(identityTemplates, appTemplates []Template, identityRows, appRows []assignment) {
	identityTemplateRows := sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system"})
	for _, t := range identityTemplates {
		identityTemplateRows.AddRow(t.ID, t.Name, t.DisplayName, t.Description, []byte(scopeJSON(t.Scopes)), t.IsSystem)
	}
	appTemplateRows := sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system"})
	for _, t := range appTemplates {
		appTemplateRows.AddRow(t.ID, t.Name, t.DisplayName, t.Description, []byte(scopeJSON(t.Scopes)), t.IsSystem)
	}
	d.env.identity.ExpectQuery(regexp.QuoteMeta(`SELECT id::text, name`)).WillReturnRows(identityTemplateRows)
	d.env.app.ExpectQuery(regexp.QuoteMeta(`SELECT id, name, COALESCE(display_name`)).WillReturnRows(appTemplateRows)
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

func scopeJSON(scopes []string) string {
	quoted := make([]string, 0, len(scopes))
	for _, s := range scopes {
		quoted = append(quoted, `"`+s+`"`)
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

func (d *driftEnv) check(t *testing.T) DriftResult {
	t.Helper()
	res, err := CheckDrift(context.Background(), d.env.appDB, d.env.identityDB)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	return res
}

func tmpl(id, name string, scopes ...string) Template {
	return Template{ID: id, Name: name, DisplayName: name, Scopes: scopes, IsSystem: true}
}

// Two sides that agree are clean, and the comparison says how much it looked at.
// The Compared count is asserted because a merge that read nothing also reports
// no drift, and the two are the same value in every field but that one.
func TestCheckDrift_AgreementIsClean(t *testing.T) {
	d := newDriftEnv(t)
	both := []Template{tmpl(adminTemplateID, "admin", "admin")}
	rows := []assignment{
		{"11111111-0000-0000-0000-000000000001", "22222222-0000-0000-0000-000000000001", adminTemplateID},
		{"11111111-0000-0000-0000-000000000002", "22222222-0000-0000-0000-000000000002", adminTemplateID},
	}
	d.stage(both, both, rows, rows)

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
	templates := []Template{tmpl(adminTemplateID, "admin", "admin"), tmpl(editorTemplateID, "editor", "state:write")}
	d.stage(templates, templates,
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

// THE CASE PHASE 3A'S DriftQuery IS BLIND TO. Both sides name the same role
// template id for the same principal, and the two schemas define that id with
// different scopes — so an id comparison reports agreement while the principal
// holds different permissions depending on which table is read. After the flip
// that is the whole failure mode, so it is a first-class kind here.
func TestCheckDrift_SeesScopesDivergingUnderAnIdenticalRoleID(t *testing.T) {
	d := newDriftEnv(t)
	const orgA = "11111111-0000-0000-0000-00000000000a"
	const user = "22222222-0000-0000-0000-000000000001"
	d.stage(
		[]Template{tmpl(adminTemplateID, "editor", "modules:read", "providers:read")},
		[]Template{tmpl(adminTemplateID, "editor", "state:read", "state:write")},
		[]assignment{{orgA, user, adminTemplateID}},
		[]assignment{{orgA, user, adminTemplateID}})

	res := d.check(t)
	if res.AssignmentDrift() != 0 {
		t.Fatalf("AssignmentDrift() = %d, want 0: the ids agree", res.AssignmentDrift())
	}
	if res.ScopeDivergent != 1 {
		t.Fatalf("ScopeDivergent = %d, want 1: the two schemas grant different scopes for the same role id", res.ScopeDivergent)
	}
	if len(res.TemplateDrift) != 1 || res.TemplateDrift[0].Name != "editor" {
		t.Fatalf("TemplateDrift = %+v, want one entry naming editor", res.TemplateDrift)
	}
	if res.Clean() {
		t.Fatal("Clean() = true while a principal's effective permissions differ between the two sources: " +
			"the gate would pass a deployment the flip is not safe on")
	}
	// The report must carry BOTH scope sets, or an operator cannot tell which
	// direction the change goes.
	out := res.String()
	if !strings.Contains(out, "modules:read") || !strings.Contains(out, "state:write") {
		t.Errorf("the report does not show both sides of the divergence:\n%s", out)
	}
}

// A template only one side has is reported as such, not as an empty scope set:
// "the sibling defines a role we do not" is a different fact from "we define it
// with nothing in it", and only one of them is a reason to act.
func TestCheckDrift_ReportsATemplateOnlyOneSideHas(t *testing.T) {
	d := newDriftEnv(t)
	d.stage(
		[]Template{tmpl(adminTemplateID, "admin", "admin"), tmpl(editorTemplateID, "registry_publisher", "modules:write")},
		[]Template{tmpl(adminTemplateID, "admin", "admin")},
		nil, nil)

	res := d.check(t)
	if len(res.TemplateDrift) != 1 {
		t.Fatalf("TemplateDrift = %+v, want one entry", res.TemplateDrift)
	}
	got := res.TemplateDrift[0]
	if got.Name != "registry_publisher" || got.OnlyIn != "identity" {
		t.Fatalf("TemplateDrift[0] = %+v, want registry_publisher only in identity", got)
	}
	if len(got.AppScopes) != 0 {
		t.Errorf("AppScopes = %v, want empty for a template this application does not have", got.AppScopes)
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
	templates := []Template{tmpl(adminTemplateID, "admin", "admin")}

	identityTemplateRows := sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system"}).
		AddRow(adminTemplateID, "admin", "admin", nil, []byte(`["admin"]`), true)
	appTemplateRows := sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system"}).
		AddRow(adminTemplateID, "admin", "admin", nil, []byte(`["admin"]`), true)
	_ = templates
	d.env.identity.ExpectQuery(regexp.QuoteMeta(`SELECT id::text, name`)).WillReturnRows(identityTemplateRows)
	d.env.app.ExpectQuery(regexp.QuoteMeta(`SELECT id, name, COALESCE(display_name`)).WillReturnRows(appTemplateRows)
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
	d.stage(nil, nil, nil, nil)
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
	templates := []Template{tmpl(adminTemplateID, "admin", "admin")}
	d.stage(templates, templates, rows, nil)

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
	d.stage(nil, nil, rows, rows)
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

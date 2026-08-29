package repositories

import (
	"context"
	"errors"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// Drift-gate tests that need no database. The Postgres integration file
// falsifies the gate against real rows; this file pins the CLASSIFICATION --
// which disagreement produces which kind, in which order -- and the
// structural refusals, on the runs that gate merges.

// newDriftMocks queues the five reads CheckGroupMappingDrift performs, in
// order, and returns a runner.
func newDriftMocks(t *testing.T, templates []string, overlay []string, mirror [][5]interface{}) func() (GroupMappingDriftReport, error) {
	t.Helper()
	sourceDB, sourceMock := newMock(t)
	appDB, appMock := newMock(t)
	expectGroupMappingMirrorVerified(appMock)
	expectGroupMappingSourceVerified(sourceMock)
	expectAppTemplateNames(appMock, templates...)
	expectStoredOverlay(sourceMock, overlay...)
	expectMirroredGroupMappings(appMock, mirror...)
	return func() (GroupMappingDriftReport, error) {
		return CheckGroupMappingDrift(context.Background(), sourceDB, appDB)
	}
}

func TestCheckGroupMappingDrift_CleanWhenBothCopiesAgree(t *testing.T) {
	run := newDriftMocks(t,
		[]string{gmRoleID, "editor"},
		[]string{gmOneMapping},
		[][5]interface{}{{0, "eng", "alpha", "editor", gmRoleID}})

	report, err := run()
	if err != nil {
		t.Fatalf("CheckGroupMappingDrift: %v", err)
	}
	if !report.Clean() {
		t.Fatalf("want clean, got %+v", report.Rows)
	}
	if !report.OverlayPresent || report.SourceMappings != 1 || report.MirroredMappings != 1 {
		t.Fatalf("scope counters must prove the gate looked at something: %+v", report)
	}
	if !strings.Contains(report.String(), "0 disagreement") {
		t.Fatalf("scope rendering: %q", report.String())
	}
}

// TestCheckGroupMappingDrift_ClassifiesEveryDisagreement asserts each of the
// four classifications and the report order. Two estates rather than the
// registry sibling's one, because TSM's source is a SINGLE list: "mirror has
// fewer rows" (not mirrored) and "mirror has more rows" (orphaned) are
// mutually exclusive in one run, where the registry's per-config keying could
// exhibit both at once.
func TestCheckGroupMappingDrift_ClassifiesEveryDisagreement(t *testing.T) {
	t.Run("mirror short and wrong", func(t *testing.T) {
		const overlay = `[` +
			`{"group":"eng","organization":"alpha","role":"editor"},` +
			`{"group":"ops","organization":"beta","role":"editor"},` +
			`{"group":"sec","organization":"gamma","role":"editor"}]`
		run := newDriftMocks(t,
			[]string{gmRoleID, "editor"},
			[]string{overlay},
			[][5]interface{}{
				{0, "eng", "WRONG", "editor", gmRoleID}, // fields differ
				{1, "ops", "beta", "editor", nil},       // role resolution stale
				// position 2 missing: not mirrored
			})

		report, err := run()
		if err != nil {
			t.Fatalf("CheckGroupMappingDrift: %v", err)
		}
		var kinds []string
		for _, row := range report.Rows {
			kinds = append(kinds, row.Kind)
			if row.String() == "" {
				t.Error("a drift row rendered empty")
			}
		}
		want := []string{
			GroupMappingDriftFieldsDiffer,
			GroupMappingDriftNotMirrored,
			GroupMappingDriftRoleRefStale,
		}
		if strings.Join(kinds, ",") != strings.Join(want, ",") {
			t.Fatalf("want kinds %v worst-first, got %v", want, kinds)
		}
	})
	t.Run("mirror holds extra rows", func(t *testing.T) {
		run := newDriftMocks(t,
			[]string{gmRoleID, "editor"},
			[]string{gmOneMapping},
			[][5]interface{}{
				{0, "eng", "alpha", "editor", gmRoleID},
				{1, "ghost", "alpha", "editor", gmRoleID}, // beyond the source list: orphaned
			})

		report, err := run()
		if err != nil {
			t.Fatalf("CheckGroupMappingDrift: %v", err)
		}
		if len(report.Rows) != 1 || report.Rows[0].Kind != GroupMappingDriftMirrorOrphaned {
			t.Fatalf("want one orphaned row -- the direction that GRANTS after the cutover -- got %+v", report.Rows)
		}
	})
}

// TestCheckGroupMappingDrift_StaleResolutionRendersBothSides pins the
// operator-facing rendering of the one kind whose two sides are DERIVED
// values: the unresolved side must say so rather than print an empty id.
func TestCheckGroupMappingDrift_StaleResolutionRendersBothSides(t *testing.T) {
	run := newDriftMocks(t,
		[]string{}, // "editor" resolves to nothing
		[]string{gmOneMapping},
		[][5]interface{}{{0, "eng", "alpha", "editor", gmRoleID}})

	report, err := run()
	if err != nil {
		t.Fatalf("CheckGroupMappingDrift: %v", err)
	}
	if len(report.Rows) != 1 || report.Rows[0].Kind != GroupMappingDriftRoleRefStale {
		t.Fatalf("want one stale-resolution row, got %+v", report.Rows)
	}
	if !strings.Contains(report.Rows[0].Stored, "unresolved") {
		t.Fatalf("the unresolved side must say so: %q", report.Rows[0].Stored)
	}
}

// TestCheckGroupMappingDrift_UnparseableOverlayIsScopeNotDrift pins the
// counting duty: a value the overlay read path ignores decodes to "no
// mappings" on both sides, so an empty mirror is CLEAN -- but the report must
// say what it saw, or the gate would hide a corrupt stored overlay.
func TestCheckGroupMappingDrift_UnparseableOverlayIsScopeNotDrift(t *testing.T) {
	run := newDriftMocks(t, []string{}, []string{`"garbage"`}, nil)
	report, err := run()
	if err != nil {
		t.Fatalf("CheckGroupMappingDrift: %v", err)
	}
	if !report.Clean() || !report.OverlayUnparseable {
		t.Fatalf("unparseable overlay mishandled: %+v", report)
	}
}

func TestCheckGroupMappingDrift_RefusesStructuralFailures(t *testing.T) {
	t.Run("mirror unreachable", func(t *testing.T) {
		sourceDB, _ := newMock(t)
		appDB, appMock := newMock(t)
		appMock.ExpectQuery("to_regclass").
			WithArgs("organization_members").
			WillReturnRows(sqlmock.NewRows([]string{"qualified"}).AddRow(nil))
		appMock.ExpectQuery("to_regclass").
			WithArgs("group_mappings").
			WillReturnRows(sqlmock.NewRows([]string{"qualified"}).AddRow(nil))
		if _, err := CheckGroupMappingDrift(context.Background(), sourceDB, appDB); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("source unreachable", func(t *testing.T) {
		sourceDB, sourceMock := newMock(t)
		appDB, appMock := newMock(t)
		expectGroupMappingMirrorVerified(appMock)
		sourceMock.ExpectQuery("to_regclass").
			WillReturnRows(sqlmock.NewRows([]string{"qualified"}).AddRow(nil))
		if _, err := CheckGroupMappingDrift(context.Background(), sourceDB, appDB); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("template names read fails", func(t *testing.T) {
		sourceDB, sourceMock := newMock(t)
		appDB, appMock := newMock(t)
		expectGroupMappingMirrorVerified(appMock)
		expectGroupMappingSourceVerified(sourceMock)
		appMock.ExpectQuery("SELECT id, name FROM role_templates").WillReturnError(errors.New("boom"))
		if _, err := CheckGroupMappingDrift(context.Background(), sourceDB, appDB); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("source read fails", func(t *testing.T) {
		sourceDB, sourceMock := newMock(t)
		appDB, appMock := newMock(t)
		expectGroupMappingMirrorVerified(appMock)
		expectGroupMappingSourceVerified(sourceMock)
		expectAppTemplateNames(appMock)
		sourceMock.ExpectQuery("SELECT oidc_group_mappings FROM sso_settings").WillReturnError(errors.New("boom"))
		if _, err := CheckGroupMappingDrift(context.Background(), sourceDB, appDB); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("mirror read fails", func(t *testing.T) {
		sourceDB, sourceMock := newMock(t)
		appDB, appMock := newMock(t)
		expectGroupMappingMirrorVerified(appMock)
		expectGroupMappingSourceVerified(sourceMock)
		expectAppTemplateNames(appMock)
		expectStoredOverlay(sourceMock)
		appMock.ExpectQuery("SELECT position, group_name").WillReturnError(errors.New("boom"))
		if _, err := CheckGroupMappingDrift(context.Background(), sourceDB, appDB); err == nil {
			t.Fatal("want error")
		}
	})
}

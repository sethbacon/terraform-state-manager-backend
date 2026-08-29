// group_mapping_drift.go compares TSM's own group_mappings table against the
// sso_settings overlay list it was derived from, and reports every way they
// disagree.
//
// THIS IS THE GATE ON THE GROUP-MAPPING READ CUTOVER
// (sethbacon/terraform-suite-identity#206, phase 2 -- migration 000036), the
// same role approles.CheckDrift plays for the role tables. The runnable verb
// is `tsm-server authz-drift`, which runs both checks and exits non-zero while
// either reports a disagreement.
//
// It is Go rather than SQL for the same reason approles.CheckDrift is: the two
// sides may live on different connections -- and in the split topology
// different DATABASES -- where no statement can join them; and the source here
// is not even a table of rows but a JSON list inside a JSONB column, compared
// through the same decode the overlay read path uses.
package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// Group-mapping drift kinds. Ordered by blast radius, worst first, which is
// also the order GroupMappingDriftReport sorts them into.
const (
	// GroupMappingDriftMirrorOrphaned: TSM holds more mapping rows than the
	// stored overlay list. Inert today; the moment the read cutover ships it
	// GRANTS memberships the configured policy never issued -- the one
	// direction that grants.
	GroupMappingDriftMirrorOrphaned = "group_mapping_mirror_orphaned"
	// GroupMappingDriftFieldsDiffer: both sides have a mapping at this
	// position and disagree on group, organization or role. May grant or
	// withhold.
	GroupMappingDriftFieldsDiffer = "group_mapping_fields_differ"
	// GroupMappingDriftNotMirrored: the overlay has a mapping TSM has no row
	// for. That group's members are served no membership after the cutover.
	GroupMappingDriftNotMirrored = "group_mapping_not_mirrored"
	// GroupMappingDriftRoleRefStale: the mirrored role_template_id is not what
	// the mapped role name currently resolves to in role_templates -- a
	// template was created, renamed or deleted after the row was written and
	// no dual-write or reconcile has re-resolved it yet.
	GroupMappingDriftRoleRefStale = "group_mapping_role_ref_stale"
)

// groupMappingDriftKindOrder ranks the kinds for the report's ordering. A kind
// missing from this map sorts last rather than panicking.
var groupMappingDriftKindOrder = map[string]int{
	GroupMappingDriftMirrorOrphaned: 0,
	GroupMappingDriftFieldsDiffer:   1,
	GroupMappingDriftNotMirrored:    2,
	GroupMappingDriftRoleRefStale:   3,
}

// GroupMappingDriftRow is one disagreement.
type GroupMappingDriftRow struct {
	Kind     string
	Position int
	// Stored / Mirrored are the two disagreeing values, rendered for a human.
	// "absent" means the side had no entry at all.
	Stored   string
	Mirrored string
}

// String renders one row for the verb's output.
func (d GroupMappingDriftRow) String() string {
	return fmt.Sprintf("%-33s position=%d  stored=%s  mirrored=%s",
		d.Kind, d.Position, d.Stored, d.Mirrored)
}

// GroupMappingDriftReport is the whole comparison.
type GroupMappingDriftReport struct {
	// Rows is every disagreement found, worst first.
	Rows []GroupMappingDriftRow
	// What was actually compared -- reported because a gate that passes MUST
	// be able to prove it looked at something. Two empty databases agree
	// perfectly.
	OverlayPresent   bool
	SourceMappings   int
	MirroredMappings int
	// OverlayUnparseable reports an oidc_group_mappings value that does not
	// decode. Not drift -- the read path ignores the whole overlay then, and
	// the mirror faithfully holds no rows -- but a gate that hid a corrupt
	// stored overlay would be lying about what it compared.
	OverlayUnparseable bool
}

// Clean reports whether the two copies agree.
func (r GroupMappingDriftReport) Clean() bool { return len(r.Rows) == 0 }

// String renders the comparison's scope for the verb and the boot log.
func (r GroupMappingDriftReport) String() string {
	return fmt.Sprintf("group mappings: %d disagreement(s); compared %d stored (overlay present=%t, unparseable=%t) vs %d mirrored",
		len(r.Rows), r.SourceMappings, r.OverlayPresent, r.OverlayUnparseable, r.MirroredMappings)
}

// CheckGroupMappingDrift compares TSM's own group_mappings table against the
// sso_settings overlay list, through the two connections the application
// itself uses.
//
// sourceDB MUST be the connection the application resolves the overlay through
// and appDB the one migration 000036 created the table on -- the same pair
// ReconcileGroupMappings takes, and for the same reason.
func CheckGroupMappingDrift(ctx context.Context, sourceDB, appDB *sql.DB) (GroupMappingDriftReport, error) {
	var report GroupMappingDriftReport

	if err := NewGroupMappingMirror(appDB).Verify(ctx); err != nil {
		return report, fmt.Errorf("this application's group-mapping table unusable: %w", err)
	}
	if err := verifyGroupMappingSource(ctx, sourceDB); err != nil {
		return report, err
	}

	nameToID, err := appRoleTemplateNames(ctx, appDB)
	if err != nil {
		return report, err
	}
	mappings, overlayPresent, unparseable, err := readStoredGroupMappings(ctx, sourceDB)
	if err != nil {
		return report, err
	}
	have, err := readMirroredGroupMappings(ctx, appDB)
	if err != nil {
		return report, err
	}

	report.OverlayPresent = overlayPresent
	report.OverlayUnparseable = unparseable
	report.SourceMappings = len(mappings)
	report.MirroredMappings = len(have)

	renderRole := func(name string, id *string) string {
		if id == nil {
			return fmt.Sprintf("role=%q (unresolved)", name)
		}
		return fmt.Sprintf("role=%q id=%s", name, *id)
	}

	for i, m := range mappings {
		var wantID *string
		if id, ok := nameToID[m.Role]; ok {
			idCopy := id
			wantID = &idCopy
		}
		stored := fmt.Sprintf("group=%q organization=%q role=%q", m.Group, m.Organization, m.Role)
		if i >= len(have) {
			report.Rows = append(report.Rows, GroupMappingDriftRow{
				Kind: GroupMappingDriftNotMirrored, Position: i,
				Stored: stored, Mirrored: "absent",
			})
			continue
		}
		h := have[i]
		if h.Position != i || h.Group != m.Group || h.Organization != m.Organization || h.RoleName != m.Role {
			report.Rows = append(report.Rows, GroupMappingDriftRow{
				Kind: GroupMappingDriftFieldsDiffer, Position: i,
				Stored:   stored,
				Mirrored: fmt.Sprintf("position=%d group=%q organization=%q role=%q", h.Position, h.Group, h.Organization, h.RoleName),
			})
			continue
		}
		if !sameGroupMappingRoleID(h.RoleTemplateID, wantID) {
			report.Rows = append(report.Rows, GroupMappingDriftRow{
				Kind: GroupMappingDriftRoleRefStale, Position: i,
				Stored: renderRole(m.Role, wantID), Mirrored: renderRole(h.RoleName, h.RoleTemplateID),
			})
		}
	}
	for i := len(mappings); i < len(have); i++ {
		report.Rows = append(report.Rows, GroupMappingDriftRow{
			Kind: GroupMappingDriftMirrorOrphaned, Position: have[i].Position,
			Stored:   "absent",
			Mirrored: fmt.Sprintf("group=%q organization=%q role=%q", have[i].Group, have[i].Organization, have[i].RoleName),
		})
	}

	sort.SliceStable(report.Rows, func(i, j int) bool {
		a, aOK := groupMappingDriftKindOrder[report.Rows[i].Kind]
		b, bOK := groupMappingDriftKindOrder[report.Rows[j].Kind]
		if !aOK {
			a = len(groupMappingDriftKindOrder)
		}
		if !bOK {
			b = len(groupMappingDriftKindOrder)
		}
		if a != b {
			return a < b
		}
		return report.Rows[i].Position < report.Rows[j].Position
	})
	return report, nil
}

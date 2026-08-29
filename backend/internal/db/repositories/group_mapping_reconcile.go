// group_mapping_reconcile.go derives TSM's own group_mappings table from
// whatever this application resolves the sso_settings overlay through TODAY
// (sethbacon/terraform-suite-identity#206, migration 000036).
//
// This is the backfill. It is Go rather than SQL inside the migration for the
// reason spelled out in 000036_group_mappings.up.sql (and before it in
// 000032): the EFFECTIVE source -- which connection, and in the split topology
// which DATABASE, resolves the live sso_settings row -- is chosen at process
// start, and is not the connection the app migrations run on. Reading through
// the very *sql.DB the auth handlers resolve the overlay through makes the
// effective source identical BY CONSTRUCTION.
package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
)

// GroupMappingReconcileReport is what one reconcile did, for the boot log.
type GroupMappingReconcileReport struct {
	// OverlayPresent reports whether the sso_settings overlay row exists at
	// all. False means the deployment's OIDC mappings are file-configured and
	// there is nothing DB-stored to mirror.
	OverlayPresent bool
	// OverlayUnparseable reports an oidc_group_mappings value that does not
	// decode as a mapping list. The read path ignores the whole overlay then
	// (effectiveOIDCGroupConfig falls back to file config), so the mirror
	// faithfully holds no rows; reported because a corrupt stored overlay is
	// worth an operator's attention even though it is not divergence.
	OverlayUnparseable bool
	// SourceMappings is how many mapping entries the stored overlay carries.
	SourceMappings int
	// Rewritten reports whether the mirrored rows were replaced because they
	// differed. Steady state is false: an unchanged list is left alone, so a
	// quiet boot writes nothing.
	Rewritten bool
	// UnresolvedRoleRefs counts mappings whose role name resolves to no
	// role_templates row. They are mirrored with a NULL role_template_id
	// rather than skipped -- a faithful copy of a mapping that confers nothing
	// at login today.
	UnresolvedRoleRefs int
}

// LogValue renders the report for slog without a field per counter.
func (r GroupMappingReconcileReport) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("overlay_present", r.OverlayPresent),
		slog.Bool("overlay_unparseable", r.OverlayUnparseable),
		slog.Int("source_mappings", r.SourceMappings),
		slog.Bool("rewritten", r.Rewritten),
		slog.Int("unresolved_role_refs", r.UnresolvedRoleRefs),
	)
}

// mirroredGroupMapping is one row of TSM's group_mappings table, in the shape
// the comparison below works on.
type mirroredGroupMapping struct {
	// Position is the row's stored position. For a wanted list it is the slice
	// index; for mirrored rows it is read back and COMPARED rather than
	// assumed, so a gap or duplicate the table should never contain still
	// forces the rewrite that repairs it.
	Position       int
	Group          string
	Organization   string
	RoleName       string
	RoleTemplateID *string
}

// ReconcileGroupMappings makes TSM's own group_mappings table equal the
// effective sso_settings overlay, and returns what it did.
//
// sourceDB MUST be the connection the application resolves the overlay through
// -- the same handle NewAuthHandlers hands NewSSOSettingsRepository (the
// identity pool; sso_settings sits on its search_path). That is the whole
// correctness argument: this function never decides which connection holds the
// live row, it inherits that decision.
//
// appDB is the connection migration 000036 created the table on.
//
// It runs on every boot, not once, for the same three reasons
// approles.Reconcile does: re-deriving is cheap when nothing changed (an
// unchanged list is not touched), it repairs whatever a transient dual-write
// failure left behind, and it converges a deployment that upgraded before the
// table existed. That standing behaviour is correct only while
// sso_settings.oidc_group_mappings is still authoritative; the read cutover
// must remove this call.
//
// It returns an error for STRUCTURAL failures -- tables that do not resolve, a
// misrouted app connection, a query that fails -- and counts per-row problems
// into the report instead.
func ReconcileGroupMappings(ctx context.Context, sourceDB, appDB *sql.DB) (GroupMappingReconcileReport, error) {
	var report GroupMappingReconcileReport

	mirror := NewGroupMappingMirror(appDB)
	if err := mirror.Verify(ctx); err != nil {
		return report, fmt.Errorf("this application's group-mapping table unusable: %w", err)
	}
	if err := verifyGroupMappingSource(ctx, sourceDB); err != nil {
		return report, err
	}

	// The role-name resolution the mirror rows should carry, read ONCE.
	// role_templates is TSM's own table on appDB -- since the Phase 3 read
	// switch it is what login-time role resolution actually consults, which
	// makes it the right target for the derived id column.
	nameToID, err := appRoleTemplateNames(ctx, appDB)
	if err != nil {
		return report, err
	}

	mappings, overlayPresent, unparseable, err := readStoredGroupMappings(ctx, sourceDB)
	if err != nil {
		return report, err
	}
	report.OverlayPresent = overlayPresent
	report.OverlayUnparseable = unparseable
	report.SourceMappings = len(mappings)

	want := make([]mirroredGroupMapping, len(mappings))
	for i, m := range mappings {
		var roleID *string
		if id, ok := nameToID[m.Role]; ok {
			idCopy := id
			roleID = &idCopy
		} else {
			report.UnresolvedRoleRefs++
		}
		want[i] = mirroredGroupMapping{Position: i, Group: m.Group, Organization: m.Organization, RoleName: m.Role, RoleTemplateID: roleID}
	}

	// The mirror as it stands, read ONCE; the decision below is a diff against
	// it, so a boot where nothing changed issues no writes at all.
	have, err := readMirroredGroupMappings(ctx, appDB)
	if err != nil {
		return report, err
	}
	if sameGroupMappingList(have, want) {
		return report, nil
	}
	if err := mirror.Replace(ctx, mappings); err != nil {
		return report, fmt.Errorf("mirror the overlay's group mappings: %w", err)
	}
	report.Rewritten = true
	return report, nil
}

// verifyGroupMappingSource refuses to reconcile from a connection that does
// not resolve sso_settings.
//
// Refusing loudly is the point, same as the mirror's Verify: an empty source
// and an unreachable source are indistinguishable from the row count alone,
// and a reconcile that quietly found nothing would empty the mirror.
func verifyGroupMappingSource(ctx context.Context, sourceDB *sql.DB) error {
	if sourceDB == nil {
		return errors.New("no source connection; refusing to reconcile this application's group_mappings from a source that is not there")
	}
	var resolved sql.NullString
	if err := sourceDB.QueryRowContext(ctx, groupMappingQualifiedNameQuery, "sso_settings").Scan(&resolved); err != nil {
		return fmt.Errorf("probe effective sso_settings: %w", err)
	}
	if !resolved.Valid {
		return errors.New("effective sso_settings does not resolve on the source connection; " +
			"refusing to reconcile this application's group_mappings from a source that is not there")
	}
	slog.DebugContext(ctx, "group-mapping reconcile source resolved", "table", "sso_settings", "resolved_to", resolved.String)
	return nil
}

// appRoleTemplateNames loads TSM's own role-template name->id map -- through
// approles.Store, the one funnel this application reads role_templates
// through (the identity-role-reads class guard refuses the SQL spelled
// anywhere else).
func appRoleTemplateNames(ctx context.Context, appDB *sql.DB) (map[string]string, error) {
	templates, err := approles.NewStore(appDB).ListTemplates(ctx)
	if err != nil {
		return nil, fmt.Errorf("read this application's role template names: %w", err)
	}
	out := make(map[string]string, len(templates))
	for name, t := range templates {
		out[name] = t.ID
	}
	return out, nil
}

// readStoredGroupMappings loads the overlay's mapping list through the source
// connection -- the same column, the same singleton row, and the same
// json.Unmarshal into the same type as SSOSettingsRepository.Get, so what the
// mirror is compared against is by construction what the overlay read path
// would decode from the same bytes.
//
// A value that does not decode is reported as unparseable and read as "no
// mappings": Get errors on it, effectiveOIDCGroupConfig then ignores the whole
// overlay and falls back to file config, so no DB-stored mapping is in force
// and the mirror faithfully holds none.
func readStoredGroupMappings(ctx context.Context, sourceDB *sql.DB) (mappings []SSOGroupMapping, overlayPresent, unparseable bool, err error) {
	var raw []byte
	err = sourceDB.QueryRowContext(ctx,
		`SELECT oidc_group_mappings FROM sso_settings WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, fmt.Errorf("read the stored group-mapping overlay: %w", err)
	}
	if jsonErr := json.Unmarshal(raw, &mappings); jsonErr != nil {
		slog.WarnContext(ctx, "sso_settings.oidc_group_mappings does not decode as a mapping list; "+
			"the overlay is ignored at login and its group mappings read as none", "error", jsonErr)
		return nil, true, true, nil
	}
	return mappings, true, false, nil
}

// readMirroredGroupMappings loads TSM's group_mappings table, ordered.
func readMirroredGroupMappings(ctx context.Context, appDB *sql.DB) ([]mirroredGroupMapping, error) {
	rows, err := appDB.QueryContext(ctx, `
		SELECT position, group_name, organization_name, role_template_name, role_template_id
		  FROM group_mappings
		 ORDER BY position`)
	if err != nil {
		return nil, fmt.Errorf("read mirrored group mappings: %w", err)
	}
	defer rows.Close()

	var out []mirroredGroupMapping
	for rows.Next() {
		var row mirroredGroupMapping
		var roleID sql.NullString
		if err := rows.Scan(&row.Position, &row.Group, &row.Organization, &row.RoleName, &roleID); err != nil {
			return nil, fmt.Errorf("scan mirrored group mapping: %w", err)
		}
		if roleID.Valid {
			id := roleID.String
			row.RoleTemplateID = &id
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// sameGroupMappingList reports whether the mirrored rows equal the wanted
// rows, field for field, in order.
func sameGroupMappingList(have, want []mirroredGroupMapping) bool {
	if len(have) != len(want) {
		return false
	}
	for i := range have {
		if have[i].Position != want[i].Position ||
			have[i].Group != want[i].Group ||
			have[i].Organization != want[i].Organization ||
			have[i].RoleName != want[i].RoleName ||
			!sameGroupMappingRoleID(have[i].RoleTemplateID, want[i].RoleTemplateID) {
			return false
		}
	}
	return true
}

// sameGroupMappingRoleID compares two nullable role-template ids.
func sameGroupMappingRoleID(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

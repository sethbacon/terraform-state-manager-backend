// group_mapping_mirror.go writes TSM's own per-app group-mapping table --
// `group_mappings`, migration 000036.
//
// Design: sethbacon/terraform-suite-identity#206, phase 2. Identity is shared,
// authorization is per-app. An IdP group mapping is authorization policy --
// "members of IdP group G get THIS APP's role R in organization O" -- and it
// moves into TSM's own schema, next to the role tables 000032 created.
//
// The authoritative store is still the `oidc_group_mappings` JSONB list in the
// single `sso_settings` overlay row, and every read still comes from there
// (effectiveOIDCGroupConfig in internal/api/auth.go, falling back to file
// config when no overlay row exists). NOTHING READS THIS TABLE YET. This file
// exists so that the read cutover has a populated, continuously reconciled
// copy to switch onto. Every write below happens AFTER the authoritative write
// has succeeded, and a failure here is logged rather than returned -- see
// groupMappingMirrorFailed.
package repositories

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

// ErrGroupMappingMirrorUnreachable reports a connection on which the mirror's
// tables do not resolve -- migration 000036 (or 000032) has not run there, or
// the connection is routed somewhere else entirely.
var ErrGroupMappingMirrorUnreachable = errors.New("repositories: group-mapping mirror tables do not resolve on this connection")

// ErrGroupMappingMirrorMisrouted reports an app connection whose search_path
// resolves identity's tables, so mirror writes intended for TSM's own schema
// would land in the shared one. Same discriminator, same reasoning as
// approles.ErrMisrouted and migration 000032's pre-check.
var ErrGroupMappingMirrorMisrouted = errors.New("repositories: the app connection resolves identity's tables unqualified; group-mapping mirror writes would cross the per-app boundary")

// GroupMappingMirror writes TSM's own copy of the group->role mappings that
// currently live in sso_settings.oidc_group_mappings.
//
// It deliberately exposes no reads beyond Verify, for the same reason
// approles.Store grew no identity reads back: a read method here would be the
// first step of the cutover, and the cutover is a separate, separately
// reviewable change. Leaving the type write-only makes "nothing observable
// changes" checkable by looking at the type rather than at every caller.
type GroupMappingMirror struct {
	db *sql.DB
}

// NewGroupMappingMirror builds a mirror over the given connection -- the
// APPLICATION connection, the one migration 000036 created the table on.
//
// It performs no I/O; a connection that is down, nil, or routed into identity
// is reported by Verify (and, absorbed, by every write). Nil-tolerant on
// purpose: test rigs construct AuthHandlers with no app connection, and a
// mirror that panicked there would make the dual-write the thing that breaks
// "nothing observable changes".
func NewGroupMappingMirror(db *sql.DB) *GroupMappingMirror {
	return &GroupMappingMirror{db: db}
}

// groupMappingQualifiedNameQuery resolves an unqualified table name through
// the connection's search_path and returns it SCHEMA-QUALIFIED, or NULL.
// The spelling (and the reason it is not the shorter `to_regclass($1)::text`,
// which omits the schema exactly when the answer matters) is
// approles.qualifiedNameQuery's; see the comment there.
const groupMappingQualifiedNameQuery = `
	SELECT (SELECT n.nspname || '.' || c.relname
	          FROM pg_class c
	          JOIN pg_namespace n ON n.oid = c.relnamespace
	         WHERE c.oid = to_regclass($1))`

// groupMappingMirrorTables are the tables Verify resolves. role_templates is
// included because every insert below resolves the mapped role name against
// it; a connection that resolves one but not the other is misconfigured.
var groupMappingMirrorTables = []string{"group_mappings", "role_templates"}

// Verify reports whether this connection is safe to mirror onto: it must NOT
// resolve identity's organization_members (the 000032 misrouting
// discriminator -- a routed connection would mirror into the shared schema),
// and it MUST resolve TSM's own group_mappings and role_templates.
//
// Same shape as approles.Store.Verify and for the same reason: a mirror that
// silently writes nowhere -- or, worse, into identity -- is worse than one
// that refuses at boot, because the divergence is only discovered at the
// cutover.
func (m *GroupMappingMirror) Verify(ctx context.Context) error {
	if m == nil || m.db == nil {
		return fmt.Errorf("%w: no application database connection", ErrGroupMappingMirrorUnreachable)
	}
	var identityVisible sql.NullString
	if err := m.db.QueryRowContext(ctx, groupMappingQualifiedNameQuery, "organization_members").Scan(&identityVisible); err != nil {
		return fmt.Errorf("probe for identity tables on the app connection: %w", err)
	}
	if identityVisible.Valid {
		return fmt.Errorf("%w: organization_members resolves to %s", ErrGroupMappingMirrorMisrouted, identityVisible.String)
	}
	for _, table := range groupMappingMirrorTables {
		var resolved sql.NullString
		if err := m.db.QueryRowContext(ctx, groupMappingQualifiedNameQuery, table).Scan(&resolved); err != nil {
			return fmt.Errorf("probe %s: %w", table, err)
		}
		if !resolved.Valid {
			return fmt.Errorf("%w: %s does not resolve", ErrGroupMappingMirrorUnreachable, table)
		}
	}
	return nil
}

// Replace makes the mirrored rows equal the given ordered list, wholesale, in
// one transaction.
//
// Wholesale rather than diffed ON PURPOSE: the source is one ordered JSON list
// whose positions shift on any edit, the list is small (it is typed into an
// admin form), and a partial update that got the order wrong would corrupt the
// one property the cutover cannot reconstruct -- first-match-wins resolution
// order (terraform-suite-identity#269, this repo's #488).
//
// Each row's role_template_id is re-resolved from the mapped role name AT
// WRITE TIME -- through approles.Store, the ONE funnel this application reads
// role_templates through (the identity-role-reads class guard refuses the SQL
// spelled anywhere else, and rightly: the funnel is what keeps every
// resolution on the verified app connection). NULL when the name does not
// resolve -- a mapping naming a missing template is a legal, faithful row (it
// confers nothing at login today), and the boot reconcile re-resolves it once
// the template appears.
//
// An empty (or nil) list deletes every row: "no DB-stored mappings" is
// represented by no rows, exactly like the overlay list being empty. Note that
// an ABSENT overlay row and an empty overlay list therefore mirror alike;
// which of the two is in force -- and so whether the FILE config's mappings
// apply -- stays sso_settings' own fact until the read cutover decides how to
// carry it.
func (m *GroupMappingMirror) Replace(ctx context.Context, mappings []SSOGroupMapping) error {
	if m == nil || m.db == nil {
		return fmt.Errorf("%w: no application database connection", ErrGroupMappingMirrorUnreachable)
	}
	var nameToID map[string]string
	if len(mappings) > 0 {
		var err error
		if nameToID, err = appRoleTemplateNames(ctx, m.db); err != nil {
			return err
		}
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin group-mapping mirror replace: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM group_mappings`); err != nil {
		return fmt.Errorf("clear mirrored group mappings: %w", err)
	}
	for i, mapping := range mappings {
		var roleID interface{} // string or NULL
		if id, ok := nameToID[mapping.Role]; ok {
			roleID = id
		}
		// Columns named explicitly, so a positional statement can never let a
		// later column addition's DEFAULT decide anything silently.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO group_mappings
			       (position, group_name, organization_name, role_template_name, role_template_id)
			VALUES ($1, $2, $3, $4, $5)
		`, i, mapping.Group, mapping.Organization, mapping.Role, roleID); err != nil {
			return fmt.Errorf("mirror group mapping %d: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit group-mapping mirror replace: %w", err)
	}
	return nil
}

// groupMappingMirrorFailed is the single place a group-mapping mirror error is
// absorbed. Absorbed rather than returned ON PURPOSE, on the same safety
// argument as the approles mirror's boot repair: the authoritative write has
// already committed, reads still come from sso_settings (or the file config),
// so the caller's request succeeded in every sense the caller can observe --
// and the boot reconcile re-derives this table from the authoritative source
// on the next start. The group-mapping check in `authz-drift` must report
// clean before the read cutover ships; that check, not this function, is the
// gate.
func groupMappingMirrorFailed(ctx context.Context, op string, err error) {
	slog.ErrorContext(ctx, "group-mapping mirror write failed",
		"operation", op, "error", err,
		"impact", "TSM's own group_mappings table has diverged from sso_settings.oidc_group_mappings; "+
			"the request itself succeeded. Restarting the backend repairs it; run `authz-drift` "+
			"and do not enable the read cutover while it reports group-mapping drift.")
}

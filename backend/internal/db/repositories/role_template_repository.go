package repositories

import (
	"database/sql"

	"github.com/jmoiron/sqlx"

	identitystore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// RoleTemplateRepository is aliased from the shared identity store.
type RoleTemplateRepository = identitystore.RoleTemplateRepository

// NewRoleTemplateRepository constructs a RoleTemplateRepository. The shared store
// requires *sqlx.DB; TSM call sites hold *sql.DB, so we wrap with a thin sqlx
// adapter over the same underlying connection pool.
func NewRoleTemplateRepository(db *sql.DB) *RoleTemplateRepository {
	return identitystore.NewRoleTemplateRepository(sqlx.NewDb(db, "postgres"))
}

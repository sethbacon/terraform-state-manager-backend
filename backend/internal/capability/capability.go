// Package capability defines a lightweight extension contract for the Terraform
// State Manager backend. A Capability bundles the seams a self-contained feature
// needs to plug in at startup: an optional scheduled task type (with its handler),
// the RBAC scopes it introduces, and an optional HTTP route registrar.
//
// The contract is deliberately minimal — a plain struct plus an in-memory
// Registry assembled at startup. There is no reflection and there are no plugins;
// capabilities are ordinary Go values constructed in wiring code (router.go) and
// handed to the scheduler and router. The first worked example is the
// version-no-op-test capability (internal/capability/versiontest).
package capability

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// TaskHandler executes a scheduled task owned by a capability. It receives the
// due task (its Config JSONB carries the per-task parameters) and returns one of
// the models.TaskRunStatus* constants. The scheduler records that status against
// the task exactly as it does for built-in task types.
type TaskHandler func(ctx context.Context, task *models.ScheduledTask) string

// RouteRegistrar mounts a capability's HTTP routes onto an authenticated router
// group. The capability is responsible for applying its own scope middleware to
// the routes it adds.
type RouteRegistrar func(group *gin.RouterGroup)

// Capability is the registration record for a pluggable feature. All fields
// except Name and Key are optional: a capability may contribute only a scheduled
// task type, only routes, only scopes, or any combination.
type Capability struct {
	// Name is a human-readable label shown in the discovery endpoint.
	Name string
	// Key is a stable, machine-readable identifier (e.g. "versiontest").
	Key string

	// TaskType, when non-empty, is the scheduled_task.task_type this capability
	// handles. The scheduler dispatches due tasks of this type to TaskHandler via
	// the registry fallback. TaskHandler must be set when TaskType is set.
	TaskType    string
	TaskHandler TaskHandler

	// Scopes are the RBAC scope strings this capability introduces. They are
	// merged into the auth scope set at startup so RequireScope can enforce them.
	Scopes []string

	// RegisterRoutes, when non-nil, mounts the capability's HTTP routes.
	RegisterRoutes RouteRegistrar
}

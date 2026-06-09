<!-- markdownlint-disable MD013 -->
# Authoring a Capability

This guide is the step-by-step contract for adding a **capability** — a
self-contained backend feature that plugs into the scheduler, the RBAC scope set,
and (optionally) the HTTP router at startup. It is the practical companion to
[ADR 006 — Capability Contract](../../docs/adr/006-capability-contract.md), which
records *why* the contract exists; this document records *how* to use it.

The worked example throughout is the **environment-drift** capability
(`internal/capability/envdrift`), added alongside this guide. Where the
**version-no-op-test** capability (`internal/capability/versiontest`) illustrates
a point better, it is called out.

## The contract in one struct

A capability is a plain value of type `capability.Capability`
(`internal/capability/capability.go`):

```go
type Capability struct {
    Name string // human-readable label for discovery
    Key  string // stable machine identifier, e.g. "envdrift"

    TaskType    string      // scheduled_task.task_type this capability owns (optional)
    TaskHandler TaskHandler // runs a due task of that type; required iff TaskType set

    Scopes []string // RBAC scope strings this capability introduces (optional)

    RegisterRoutes RouteRegistrar // mounts HTTP routes (optional)
}
```

Every field except `Name`/`Key` is optional. A capability may contribute only a
task type, only scopes, only routes, or any mix. There is no plugin system and no
reflection — capabilities are ordinary Go values constructed in `router.go` and
handed to the scheduler and router.

## Steps

### 1. Implement the feature in its own package

Create `internal/capability/<key>/`. Keep the capability a thin wrapper over a
reusable **engine** (a service under `internal/services/...`) so the engine stays
independently testable. Depend on the engine through a **narrow interface**, not
the concrete type, so the capability can be unit-tested with a fake.

The env-drift capability wraps the `internal/services/envdrift` engine through
this interface:

```go
// internal/capability/envdrift/envdrift.go
type Engine interface {
    DetectForState(ctx context.Context, orgID, workspaceName string,
        state *hcp.StateFile, stateProps map[string]map[string]string) (*envdriftsvc.Result, error)
}
```

> **Typed-nil gotcha.** The constructor accepts the `Engine` *interface* (not the
> concrete `*envdrift.Service`) precisely so wiring code can pass an **untyped
> nil** to mean "unconfigured". A typed-nil concrete pointer stored in an
> interface is **not** `== nil`, which would make a `Configured()` check wrongly
> report true.

### 2. Declare the scheduled task type

If the capability runs on a schedule, give it a `TaskType` string and mirror that
literal into `models.ValidTaskTypes()` so the scheduler API accepts API-created
tasks of that type. The `models` package cannot import `capability` (import
cycle), so the literal lives in `db/models/scheduled_task.go`:

```go
// db/models/scheduled_task.go
const TaskTypeEnvDrift = "envdrift"
// ... and add TaskTypeEnvDrift: true to ValidTaskTypes()
```

The capability references it back: `const TaskType = models.TaskTypeEnvDrift`.

### 3. Write the `TaskHandler`

The handler receives the due `*models.ScheduledTask` (its `Config` JSONB carries
the per-task parameters) and returns one of `models.TaskRunStatusSuccess`,
`...Failed`, or `...Skipped`. The scheduler records that status as-is.

Conventions the env-drift handler follows:

- **Decode `task.Config`** into a typed struct and validate it; return
  `TaskRunStatusFailed` on a bad config.
- **Skip gracefully when unconfigured.** If the capability has no live credential,
  return `TaskRunStatusSkipped` (mirroring the backup task's nil-service skip)
  rather than failing or panicking. `envdrift.Handler.Configured()` reports
  whether an engine is wired.
- **Reuse shared sinks.** Env-drift writes `drift_events` rows
  (`drift_source = "environment"`) via the same `DriftEventRepository` and
  `ClassifyDriftSeverity` classifier as the inbound drift-ingest path, so existing
  consumers render the result uniformly.

### 4. Choose RBAC scopes (or reuse one)

Put new scope strings in `Capability.Scopes`; `auth.RegisterCapabilityScopes`
merges them into the scope set at startup so `RequireScope` enforces them with no
per-constant edit. The version-no-op-test capability introduces `versiontest:admin`
this way.

The drift capabilities instead **reuse the existing `drift:write` scope**
(`auth/scopes.go`) — they set `Scopes: nil` and gate their routes with
`middleware.RequireScope(auth.ScopeDriftWrite)`. Reuse an existing scope when the
new feature is the same permission concern as an existing one.

### 5. (Optional) Add HTTP routes

Two ways to mount routes:

- Set `Capability.RegisterRoutes` to mount them from the capability itself, or
- Wire them into an existing handler group in `router.go` (what the drift
  capabilities do, so they sit next to the existing `/drift` routes).

The env-drift and outbound-trigger triggers live on the existing drift handler
(`internal/api/drift/handlers.go`):

```go
driftGroup.POST("/env-check", middleware.RequireScope(auth.ScopeDriftWrite), driftHandlers.EnvCheck)
driftGroup.POST("/trigger",   middleware.RequireScope(auth.ScopeDriftWrite), driftHandlers.Trigger)
```

**Unconfigured ⇒ 503, never 500.** When a capability's credential is absent its
endpoint must return `503 Service Unavailable` with a clear message — mirroring
the inbound `POST /drift/ingest`, which returns 503 when its OIDC issuer is unset.
`EnvCheck`/`Trigger` check `handler == nil || !handler.Configured()` first and 503
before touching the engine. This keeps the routes UAT-able with placeholder
credentials.

Every new or changed handler also needs a swag annotation block (`@Summary`,
`@Tags Drift`, `@Security`, `@Router`); regenerate `docs/swagger.json` with
`swag init -g cmd/server/main.go --outputTypes json` (see
[ADR 005](../../docs/adr/005-openapi-spec-from-swag-annotations.md)). CI fails on
swagger drift.

### 6. Register it in `router.go`

Construct the engine (returning an **untyped nil** when its credential is absent),
then register the capability and, if it has manual routes, build a handler over
the **same** engine:

```go
// internal/api/router.go
envDriftEngine := buildEnvDriftEngine(driftRepo)        // nil today: no Azure credential
capabilityRegistry.Register(capenvdrift.New(envDriftEngine))
// ... and for the HTTP trigger, share the same engine:
driftHandlers := driftAPI.NewHandlers(...).WithCapabilities(
    capenvdrift.NewHandler(envDriftEngine),
    captrigger.NewHandler(driftTriggerEngine),
)
```

The scheduler, scope set, and `GET /api/v1/capabilities` discovery endpoint pick
the capability up automatically from the registry. The `buildEnvDriftEngine` /
`buildDriftTriggerEngine` helpers in `router.go` are where a live engine is wired
once a real credential exists; today they return nil, so the capabilities register
but report themselves unconfigured.

### 7. Test it

- **Capability unit tests** drive the handler with a fake engine: assert
  skipped-when-unconfigured, failed-on-bad-config, success on the happy path, and
  failed when the engine errors (see `envdrift_test.go`).
- **Handler tests** use `httptest` + `go-sqlmock`, asserting the 503-when-
  unconfigured path, the 403 cross-org guard, and the success status/body (see
  `internal/api/drift/triggers_test.go`).
- **Dispatch test** confirms a `scheduled_task` of the capability's task type
  reaches its handler through the registry fallback (see
  `services/scheduler/capability_drift_dispatch_test.go`).

## Checklist

- [ ] Package under `internal/capability/<key>` wrapping a service via a narrow interface.
- [ ] `TaskType` constant added to `db/models/scheduled_task.go` and `ValidTaskTypes()`.
- [ ] `TaskHandler` returns a `TaskRunStatus*`; skips gracefully when unconfigured.
- [ ] Scopes declared (or an existing scope reused).
- [ ] Routes mounted with the right scope; **503, not 500**, when unconfigured.
- [ ] Swag annotations added and `docs/swagger.json` regenerated.
- [ ] `Register`ed in `router.go`; the same engine backs the scheduled and HTTP paths.
- [ ] Unit + handler + dispatch tests pass (`go test ./...`).

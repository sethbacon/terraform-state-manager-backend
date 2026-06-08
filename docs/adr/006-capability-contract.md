<!-- markdownlint-disable MD013 -->
# 6. Capability Contract for Pluggable Features

**Status**: Accepted

## Context

The backend grows by adding self-contained features (a scheduled task type, the RBAC scopes it needs, and a few HTTP routes). Today each addition is wired by hand in several places: a new `case` in the scheduler's `executeTask` switch, new scope constants in `auth/scopes.go`, and new route groups in `api/router.go`. The wiring is spread out, easy to get partially wrong, and gives no single place to see "what features does this deployment expose?"

Phase 8 asks for a lightweight way to register a feature's seams in one place, proven by a single worked example — the **version-no-op-test** capability, which asks whether bumping a dependency version produces a no-op Terraform plan or drifts infrastructure.

The constraint is deliberately narrow: prove the contract with one real capability, without a plugin system, reflection, or a rewrite of the existing built-in features.

## Decision

Introduce a **capability contract** — a plain `Capability` struct plus an in-memory `Registry` (`internal/capability`). A capability bundles the optional seams a feature plugs into at startup:

- `Name` / `Key` — discovery metadata.
- `TaskType` + `TaskHandler` — a scheduled task type and its handler.
- `Scopes` — RBAC scope strings the feature introduces.
- `RegisterRoutes` — an optional HTTP route registrar.

All fields except `Name`/`Key` are optional; a capability may contribute only a task type, only scopes, only routes, or any mix. There is no reflection and there are no plugins — capabilities are ordinary Go values constructed in `router.go` and handed to the scheduler and router.

The three seams are wired **least-invasively**:

1. **Scheduler** — the existing built-in task types keep their fast-path `switch`. Only the `default` branch changed: it now falls back to `Registry.LookupByTaskType` and dispatches to the matching capability's handler. A capability adds a scheduled task type purely by registering. `models.ValidTaskTypes()` is extended with the capability's task type so the scheduler API accepts it.
2. **Scopes** — `auth.RegisterCapabilityScopes` merges capability scope strings into `AllScopes`/`ValidScopes`, so `RequireScope` enforces them with no per-scope constant edits.
3. **Discovery** — `GET /api/v1/capabilities` (authenticated read) lists registered capabilities with their task type and scopes.

### Worked example: version-no-op-test

`internal/capability/versiontest` registers the `versiontest` capability (scope `versiontest:admin`). Its handler reads a task's `Config` JSONB `{ repo_url, candidate_versions, plan_fixture }`, obtains a Terraform plan through a `PlanProvider` interface, and reuses `services/driftingest.SummarizePlan` to classify the plan:

- **zero resource changes → no-op**: record nothing, report success.
- **any change → drift**: write a code-sourced `drift_events` row via the existing `DriftEventRepository` sink (reusing `ClassifyDriftSeverity` and the same JSONB shape as the inbound drift-ingest path).

The live provider that triggers a plan in an external CI system (Azure DevOps) is deferred (O6). The shipped `FixturePlanProvider` reads a recorded `terraform show -json` fixture, so the capability is exercisable and unit-tested end-to-end without an outbound connection. No database migration is needed — the capability reuses `drift_events` and the scheduled-task `Config` column.

### How to add a capability

1. Implement the feature in its own package under `internal/capability/<key>`.
2. Construct a `capability.Capability` (set `TaskType`/`TaskHandler` for a scheduled task, `Scopes` for new RBAC scopes, `RegisterRoutes` for HTTP routes).
3. In `router.go`, `Register` it on the startup `capability.Registry`. The scheduler, scope set, and discovery endpoint pick it up automatically.
4. If it adds a scheduled task type, add the literal to `models.ValidTaskTypes()` so API-created tasks of that type validate.

### Alternatives considered

- **Full plugin system (Go `plugin`, or out-of-process):** rejected — far more than the one worked example needs, with build/version-coupling and operational cost the project does not want now.
- **Convert existing built-in features (analysis/snapshot/backup) into capabilities:** rejected for this slice — it is a broad refactor of working code for no immediate benefit; only the seams plus one worked example are in scope.
- **Reflection-driven auto-registration:** rejected — explicit registration in `router.go` is simpler to read, debug, and test than a reflective discovery mechanism.

## Consequences

**Easier**:

- A new self-contained feature registers its scheduled task type, scopes, and routes in one place; the scheduler, auth scope set, and discovery endpoint pick it up with no scattered edits.
- `GET /api/v1/capabilities` gives operators and the frontend a single source of truth for what a deployment exposes.
- The version-no-op-test logic is testable without any external system via the fixture provider.

**Harder**:

- A capability's scheduled task type must still be mirrored into `models.ValidTaskTypes()` (the `models` package cannot import `capability` without a cycle), so that one literal is duplicated.
- The contract is intentionally minimal; capabilities needing richer lifecycle hooks (migrations, background loops beyond the scheduler) are not covered and would extend the contract later.
- The live ADO `PlanProvider` (O6) and converting existing features to capabilities remain follow-ups.

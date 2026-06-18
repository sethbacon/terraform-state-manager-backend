<!-- markdownlint-disable MD013 -->
# 4. Role-Seed Ownership in a Shared Identity Schema

**Status**: Accepted

## Context

On every boot the State Manager seeds the role→scope mappings it owns — `admin`, `editor`, `operator`, `viewer` — into the shared identity schema's `role_templates` table (`internal/bootstrap/bootstrap.go`, scopes defined in `internal/auth/scopes.go`). The seed is an idempotent upsert by role name, so a restart simply re-asserts the app's current scope definitions. This is correct and harmless when the State Manager owns its identity store outright (the standalone default).

It becomes a hazard under suite coupling. When the State Manager and the sibling Terraform Registry are pointed at the **same** identity database ([ADR 001](001-suite-coupling-shared-identity.md)), both apps define role templates by the same names (`admin`, `editor`, `viewer`, …) but with **different scope sets** — the registry's `editor` grants `modules:write`/`providers:write`; the State Manager's `editor` grants `state:write`/`state:transfer`/`sources:manage`. If both apps blindly seed on boot, each restart overwrites the other's scopes for the shared role names. The result is a flapping authorization model where a user's effective permissions depend on which app rebooted last.

The seed also cannot simply be skipped for the non-owner, because role templates must still exist for the store to function. And the upsert uses direct SQL rather than the identity module's `RoleTemplateRepository` precisely because that repository guards updates to system rows — the app intentionally owns these mappings, so it writes them directly.

## Decision

Elect a single **role-seed owner** per shared identity store, configured by `TSM_SUITE_ROLE_SEED_OWNER` (`self` | `registry` | `tsm`, default `self`).

The decision is computed by `SuiteConfig.ShouldSeedRoles(app)` (`internal/config/config.go`):

```go
func (s SuiteConfig) ShouldSeedRoles(app string) bool {
    return s.RoleSeedOwner == "self" || s.RoleSeedOwner == app
}
```

`main.go` passes `cfg.Suite.ShouldSeedRoles("tsm")` into `bootstrap.Run`, which seeds role templates **only** when that returns true:

- `self` (default) — every app seeds its own store. This matches standalone behavior exactly: the State Manager owns and seeds its own `role_templates`.
- `registry` or `tsm` — exactly one app is the designated owner. When the State Manager is *not* the owner, it skips `seedRoleTemplates` entirely and leaves the owner's scope definitions untouched, so the two apps stop clobbering each other on every restart.

Critically, the **default organization is always ensured** regardless of seed ownership (`ensureDefaultOrg` runs unconditionally in `bootstrap.Run`). The default org is a single-tenant *app* concern, not a cross-app collision: both apps want a `default` organization to exist, and the `INSERT … WHERE NOT EXISTS` guard makes ensuring it idempotent and conflict-free. Only the role-template *scopes* are contended, so only the role seed is gated.

## Consequences

**Easier**:

- Standalone deployments need zero configuration — `self` is the default and behaves exactly as a single-owner app always has.
- A shared identity store gets one authoritative source of role scopes; restarts stop flapping the authorization model.
- The split between "always ensure" (default org) and "owner-only seed" (role templates) keeps the bootstrap minimal — only the genuinely contended rows are gated.

**Harder**:

- The setting must be configured consistently across the suite: exactly one app set as owner, the other set to the same owner value. A misconfiguration where both are `self` (or both name themselves) reintroduces the clobber; where neither owns, role-scope drift is frozen at whatever was last seeded.
- The non-owner's role-scope definitions in its own code become advisory under a shared store — the owner's definitions win. Operators reasoning about effective scopes must know which app owns the seed.
- Role-template changes shipped by the non-owner app do not take effect in a shared store until the owner app also ships them, coupling the two apps' release cadence for role definitions.

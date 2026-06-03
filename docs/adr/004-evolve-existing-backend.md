<!-- markdownlint-disable MD013 -->
# 4. Evolve the Existing Backend

**Status**: Accepted

## Context

`terraform-state-manager-backend` is described as a first draft, but it is substantial and largely sound. It already implements, with the repository/service patterns the suite favors:

- services for `analyzer`, `snapshot` (snapshot-vs-snapshot diff), `backup` (+ retention), `migration` (state-file-between-backends), `compliance` (custom naming/tagging/version rules), `notification`, `reporter`, and `scheduler`;
- state-source clients for HCP, S3, Azure, GCS, Consul, PostgreSQL, Kubernetes, HTTP, and local;
- JWT/API-key/OIDC auth, RBAC, audit, telemetry, and 10 SQL migrations.

The question was whether to keep this and extend it, partially rewrite it, or restart from the registry backend as a template for maximum structural uniformity.

## Decision

**Evolve** the existing backend. Keep the working services, repositories, models, and clients, and extend them for the new MVP goals:

- repo-linked version metadata for analysis (ADR-adjacent: `required_version` / `.terraform.lock.hcl` from a linked ADO repo),
- plan-based and environment drift (per [ADR 003](003-hybrid-terraform-execution.md)),
- ADO repo migration,
- a policy abstraction with OPA as a fast-follow,

and **align its auth/admin layer to the shared identity component** ([ADR 002](002-shared-identity-component.md)) rather than maintaining the local subset.

### Alternatives considered

- **Greenfield restart from the registry-backend template:** cleanest structural alignment with the suite, but discards a large amount of working, tested code and restarts capabilities that already function.
- **Selective rewrite** (keep data/repository layer, rewrite services that don't fit): a middle path, but most existing services *do* fit the target model and only need extension, so a broad rewrite is unjustified.

## Consequences

**Easier**:

- Preserves working capabilities and momentum; new work is additive on a known-good base.
- Aligns with the suite incrementally (auth via the shared identity module) instead of via a disruptive restart.

**Harder**:

- The local auth/admin layer must be refactored to consume the shared identity component, including a data path for any existing identity rows.
- First-draft inconsistencies must be reconciled as they surface — for example the OpenAPI documentation gap addressed in [ADR 005](005-openapi-spec-from-swag-annotations.md).
- Test coverage is low (~4.5% at baseline); it must be raised area-by-area as code is touched, ratcheting toward the suite's 80% target (the CI floor starts low by design — see `REVITALIZATION-PLAN.md`).

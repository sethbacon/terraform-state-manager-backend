<!-- markdownlint-disable MD013 -->
# Architecture Decision Records

This directory contains Architecture Decision Records (ADRs) for the Terraform State Manager backend.

ADRs follow the [Michael Nygard template](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions): **Title**, **Status**, **Context**, **Decision**, **Consequences**. This matches the convention used across the suite (see `terraform-registry-backend/docs/adr`).

The first ADRs (001–005) record the decisions that frame the revitalization initiative; see `REVITALIZATION-PLAN.md` at the repo root for the full plan and phasing.

## Index

| ADR                                              | Title                                          | Status   |
| ------------------------------------------------ | ---------------------------------------------- | -------- |
| [001](001-frontend-rebaseline.md)                | Frontend Re-baseline on the Registry Stack     | Accepted |
| [002](002-shared-identity-component.md)          | Shared Identity Component Owned by Neither App | Accepted |
| [003](003-hybrid-terraform-execution.md)         | Hybrid Terraform Execution                     | Accepted |
| [004](004-evolve-existing-backend.md)            | Evolve the Existing Backend                    | Accepted |
| [005](005-openapi-spec-from-swag-annotations.md) | OpenAPI Spec Generated from swag Annotations   | Accepted |
| [006](006-capability-contract.md)                | Capability Contract for Pluggable Features      | Accepted |

## Creating a New ADR

1. Copy the template below into a new file named `NNN-short-title.md`.
2. Fill in all sections.
3. Add an entry to the index table above.

### Template

```markdown
# NNN. Short Title

**Status**: Accepted | Deprecated | Superseded by [NNN](NNN-xxx.md)

## Context

[Describe the forces at play]

## Decision

[Describe what we decided]

## Consequences

[Describe what becomes easier or harder]
```

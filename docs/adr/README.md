<!-- markdownlint-disable MD013 -->
# Architecture Decision Records

This directory contains Architecture Decision Records (ADRs) for the Terraform State Manager Backend project.

## What is an ADR?

An Architecture Decision Record captures an important architectural decision made along with its context and consequences. We use the [Michael Nygard template](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions) with the following sections:

- **Title** -- A short noun phrase describing the decision
- **Status** -- Accepted, Deprecated, or Superseded
- **Context** -- The forces at play, including technical, political, social, and project constraints
- **Decision** -- The change we are proposing or have agreed to implement
- **Consequences** -- What becomes easier or harder as a result of this decision

## Index

| ADR                                                  | Title                                          | Status   |
| ---------------------------------------------------- | ---------------------------------------------- | -------- |
| [001](001-suite-coupling-shared-identity.md)         | Suite Coupling via Runtime Discovery + Shared Identity | Accepted |
| [002](002-fail-closed-state-writes.md)               | Fail-Closed State Writes                       | Accepted |
| [003](003-advisory-lock-ttl.md)                      | Advisory Locks with a 15-Minute Stale TTL      | Accepted |
| [004](004-role-seed-ownership.md)                    | Role-Seed Ownership in a Shared Identity Schema | Accepted |
| [005](005-per-app-authorization-tables.md)           | Per-App Authorization Tables                   | Accepted |
| [006](006-per-app-authorization-reads.md)            | Per-App Authorization Reads                    | Accepted |

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

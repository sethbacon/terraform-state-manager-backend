<!-- markdownlint-disable MD013 -->
# 3. Hybrid Terraform Execution

**Status**: Accepted

## Context

Two MVP capabilities require *running* Terraform, not just reading state:

- **Drift health-check** — detecting drift of state from **code** (a `terraform plan` against the repository) and drift of state from the **environment** (comparing recorded state to live cloud resources).
- **Version no-op testing** — scheduled `plan` runs against candidate Terraform/provider versions to validate that the plan is a no-op.

The existing snapshot service only does state-vs-state diffs, so this is genuinely new. The organization already owns a mature Azure DevOps extension, `azure-pipelines-terraform`, that installs Terraform and runs `init/plan/apply/...` with Workload Identity Federation (OIDC) across Azure, AWS, GCP, and OCI, and emits a `changesPresent` output from `plan -detailed-exitcode`. Putting Terraform execution and cloud credentials *inside* the State Manager backend would duplicate that investment and force the backend to own runner isolation, secret handling, and concurrency.

## Decision

Use a **hybrid** execution model:

- **State-vs-code drift and version no-op testing** are performed by **orchestrating Azure DevOps pipelines** that use the existing `azure-pipelines-terraform` extension. The State Manager triggers a pipeline (`plan -out` with `-detailed-exitcode`), and the result — `changesPresent` plus the plan JSON — is reported back via a webhook callback and ingested as drift events. No Terraform binary or cloud credential lives in the backend.
- **State-vs-environment drift** is performed by **backend read-only cloud API clients** (Azure, AWS, GCP, OCI) that reconcile in-state resources against live resources. For HCP Terraform-backed workspaces, consume HCP's native **health assessments** (drift detection) where available rather than reimplementing it, falling back to a pipeline `plan` when not.

### Alternatives considered

- **Backend-executed Terraform** (run `terraform` in sandboxed workers/containers with injected cloud creds): self-contained and not dependent on ADO, but re-owns authentication, secret management, runner isolation, and concurrency — exactly what the existing extension already solves with WIF.
- **Pipelines only** (every drift check is a full pipeline `plan`): uniform, but a full plan is heavyweight for environment-drift checks that a read-only cloud query can answer directly, and it couples all drift signal to ADO availability.

## Consequences

**Easier**:

- Reuses the existing WIF/OIDC auth and the maintained extension; no Terraform execution or long-lived cloud secrets in the backend.
- Plan-based results (`changesPresent`, plan JSON) are auditable and run in the same pipelines teams already trust.
- Lightweight environment-drift checks don't require a full plan.

**Harder**:

- The backend must implement ADO pipeline orchestration plus secure webhook ingestion of plan results.
- Per-cloud read-only clients (Azure/AWS/GCP/OCI) must be built and credentialed for read access.
- Plan-based drift depends on ADO availability and pipeline turnaround; HCP-native drift depends on the workspace's HCP tier (with a pipeline fallback).

# Terraform State Manager — Backend

Control-plane API for analyzing, manipulating, and watching Terraform state **where it already lives** — no state migration required.

[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev/)
[![Coverage](https://img.shields.io/endpoint?url=https://gist.githubusercontent.com/sethbacon/04a5fcc9b19b7b263059a3c62f5481bc/raw/coverage.json)](https://github.com/sethbacon/terraform-state-manager-backend/actions/workflows/ci.yml)

This repository contains the backend API, database migrations, and deployment infrastructure. The frontend UI is a separate React SPA: **[terraform-state-manager-frontend](https://github.com/sethbacon/terraform-state-manager-frontend)**. It deliberately shares operational conventions (config layering, embedded migrations, side-channel metrics, release tooling) with the sibling [terraform-registry-backend](https://github.com/sethbacon/terraform-registry-backend).

## Features

### State Sources

Connect to state in place across ten backend types: **HCP Terraform / Terraform Cloud**, **AWS S3**, **Azure Blob Storage**, **Google Cloud Storage**, **Consul**, **PostgreSQL**, **Kubernetes secrets**, **HTTP remote state**, **Git repositories**, and **local filesystem**. Source credentials are encrypted at rest (AES-256-GCM) and never returned by the API. A background sync maintains a persistent state-analysis store, so dashboards and reports stay fast at thousands of state files.

### State Analysis

- Per-state resource/provider/module breakdown, Terraform version tracking, and serial/lineage metadata
- **RUM (Resources Under Management) analyzer** — verified bit-exact against HCP Terraform billing counts
- Append-only per-state analysis history, recorded only when observed content actually changes
- Cross-source dashboard aggregation (RUM, providers, resource types, Terraform versions)

### State Manipulation

- Guided state edits and restores with **fail-closed guards** — writes are refused unless the target's existence can be positively verified
- Locking with a 15-minute TTL, admin force-unlock, HCP lock-then-verify, and Consul check-and-set writes

### Drift Detection at Scale

- Drift records with acknowledgement workflow: one live record per state, re-detections collapse into it, clean runs auto-resolve
- `POST /api/v1/drift/ingest` — idempotent CI ingestion endpoint (external-ref deduplicated) that parses Terraform plan JSON
- Severity scoring (destroys are critical) and Slack / generic-webhook notifications on findings

### Version Lab

Validate repositories against newer module, provider, and Terraform versions by orchestrating runs through your existing CI (GitHub Actions / Azure DevOps), with results posted back via callback.

### Scheduling & Reporting

- Cron-style schedules for sync, drift, and version-health runs
- Markdown/report generation per source and state
- State transfers between sources (scope-gated)

### Authentication & Authorization

- Cookie-based JWT sessions (HttpOnly, CSRF-protected) — tokens are never exposed to page JavaScript
- Five SSO providers: **OIDC** (with IdP-authoritative group mapping), **LDAP**, **SAML**, **mTLS**, and **SCIM** provisioning
- **API keys** (`tsm_` Bearer tokens) — bcrypt-hashed, secret shown once, rotation with up to 72 h grace, scopes capped at the creator's own
- Scoped RBAC with organizations, roles, users, and audit logs (shared `terraform-suite-identity` module)

### Observability & Operations

- `/health` (liveness), `/ready` (DB-checked readiness), Prometheus metrics on a side-channel port (9090)
- Swagger/OpenAPI served at `/swagger.json` (regenerate with `make swag`)
- Embedded, advisory-locked database migrations run on boot; also available standalone via the `migrate` subcommand
- `TSM_WORKERS_ENABLED` gate splits API replicas from the single background worker for horizontal scaling

## Tech Stack

| Component  | Technology |
|------------|------------|
| Language   | Go 1.26+ |
| Framework  | Gin |
| Database   | PostgreSQL 14+ (golang-migrate, embedded) |
| Auth       | JWT sessions, OIDC/LDAP/SAML/mTLS/SCIM, API keys |
| Telemetry  | slog (JSON), Prometheus |
| Docs       | swaggo / Swagger 2.0 |

## Repository Layout

```
backend/
├── cmd/server/main.go        # serve | migrate | version
├── internal/
│   ├── api/                  # Gin router + HTTP handlers (incl. SCIM)
│   ├── auth/                 # JWT + OIDC / LDAP / SAML / mTLS providers
│   ├── config/               # Viper config (TSM_ env prefix)
│   ├── crypto/               # AES-256-GCM credential encryption
│   ├── db/                   # pool + embedded migrations
│   ├── middleware/           # auth, CSRF, request id, security headers, metrics
│   ├── repositories/         # data access
│   ├── services/             # statesync, scheduler, drift ingest, notify, reporter
│   └── statesource/          # the ten state-backend connectors
├── deployments/              # helm, kustomize, ACA, ECS, Cloud Run, compose, terraform, binary
└── docs/                     # deployment, configuration, operations
```

## Quick Start

### Local development

```bash
# Requires Go 1.26+ and a reachable PostgreSQL (see config.example.yaml).
make tidy          # resolve dependencies (first run)
make test          # unit tests (race detector + coverage)
make vet           # static checks
make build         # build ./backend/terraform-state-manager
make run           # run with config.example.yaml
make swag          # regenerate docs/swagger.json (commit it; CI checks sync)

# Migrations run automatically on serve; standalone:
cd backend && CONFIG_PATH=../config.example.yaml go run ./cmd/server migrate up
```

### Full local stack

A complete dev stack (PostgreSQL + backend + frontend + Keycloak IdP, with seed states) lives in the frontend repo's [`deployments/`](https://github.com/sethbacon/terraform-state-manager-frontend/tree/main/deployments) Docker Compose.

## Configuration

Layered: built-in defaults < YAML file (`CONFIG_PATH`) < `TSM_`-prefixed environment variables (e.g. `TSM_DATABASE_HOST`). The complete variable reference, secret-handling guidance, and worker-topology notes are in **[docs/configuration.md](docs/configuration.md)**; first-run secrets and admin bootstrap in **[docs/initial-setup.md](docs/initial-setup.md)**.

> ⚠️ `TSM_ENCRYPTION_KEY` protects stored source/CI credentials and has **no re-encryption tooling** — escrow it. See [docs/disaster-recovery.md](docs/disaster-recovery.md).

## Deployment

Production deployment options with step-by-step docs (AKS is the primary target):

| Method | Where |
|--------|-------|
| Helm chart (AKS / EKS / GKE values) | [`deployments/helm/`](deployments/helm/) |
| Kustomize base + overlays | [`deployments/kubernetes/`](deployments/kubernetes/) |
| Azure Container Apps (Bicep) | [`deployments/azure-container-apps/`](deployments/azure-container-apps/) |
| AWS ECS (CloudFormation) | [`deployments/aws-ecs/`](deployments/aws-ecs/) |
| Google Cloud Run | [`deployments/google-cloud-run/`](deployments/google-cloud-run/) |
| Docker Compose (production) | [`deployments/docker-compose.prod.yml`](deployments/docker-compose.prod.yml) |
| systemd binary install | [`deployments/binary/`](deployments/binary/) |
| Cloud landing zones (Terraform) | [`deployments/terraform/`](deployments/terraform/) |

Start at **[docs/deployment.md](docs/deployment.md)** for the full matrix, prerequisites, and operations guides.

## Testing & Quality

- Unit tests with the race detector; CI enforces a filtered-coverage floor (functions requiring live external dependencies are marked `coverage:skip:` and covered elsewhere)
- gosec security scanning against a reviewed baseline
- Deployment artifacts validated in CI (`helm lint`/`template` + kubeconform, kustomize builds, compose config)
- Conventional Commits + release-please drive versioning and releases

## Documentation

Operations and integration docs live in [`docs/`](docs/) — deployment guides per platform, complete `TSM_*` configuration reference, observability, secrets rotation, disaster recovery, upgrades, and troubleshooting.

## History

This codebase is the second-generation implementation. The original draft is preserved on the [`archive/ogtsm`](https://github.com/sethbacon/terraform-state-manager-backend/tree/archive/ogtsm) branch; releases up to v0.9.0 were cut from that lineage.

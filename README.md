# Terraform State Manager — Backend

Go/Gin control-plane API for the Terraform State Manager. It connects to where
Terraform state already lives (HCP/TFC, Azure Blob, S3, GCS, local, Git), exposes
the state-analyzer's capabilities over HTTP, and orchestrates drift and version
health runs through existing CI pipelines (GitHub Actions / Azure DevOps).

This service deliberately mirrors the sibling
[`terraform-registry-backend`](../terraform-registry-backend) — same config
layering, embedded migrations, side-channel metrics port, and graceful shutdown —
so the two share operational conventions. See
[`terraform-state-manager-OVERVIEW.md`](../terraform-state-manager-OVERVIEW.md)
for the full project plan.

> **Status: Phase 0 scaffold.** System endpoints, config, database + migrations,
> middleware, and metrics are in place and build/run locally. Domain features
> (state sources, analysis, editing, drift, health, transfers) and auth are added
> in subsequent phases.

## Layout

```
backend/
├── cmd/server/main.go        # serve | migrate | version
├── internal/
│   ├── api/                  # Gin router + system handlers (health/ready/version)
│   ├── config/               # Viper config (TSM_ env prefix), DSN helper
│   ├── db/                   # connection pool + embedded golang-migrate migrations
│   ├── middleware/           # request id, security headers, request metrics
│   └── telemetry/            # slog setup + Prometheus metrics
├── Dockerfile
└── config.example.yaml
```

## Endpoints (Phase 0)

| Method | Path               | Purpose                                  |
|--------|--------------------|------------------------------------------|
| GET    | `/health`          | Liveness (no dependency checks)          |
| GET    | `/ready`           | Readiness (verifies database is reachable) |
| GET    | `/api/v1/version`  | Build metadata                           |
| GET    | `/metrics`         | Prometheus metrics (side-channel port 9090) |

## Configuration

Layered: built-in defaults < YAML file (`CONFIG_PATH`) < `TSM_`-prefixed env vars
(e.g. `TSM_DATABASE_HOST`). See [`config.example.yaml`](config.example.yaml).

## Local development

```bash
# Requires Go 1.26+ and a reachable Postgres (see config.example.yaml).
make tidy          # resolve dependencies (first run)
make test          # unit tests
make vet           # static checks
make build         # build ./backend/terraform-state-manager
make run           # run with config.example.yaml

# Database migrations (run automatically on serve; also available standalone):
cd backend && CONFIG_PATH=../config.example.yaml go run ./cmd/server migrate up
```

A full local stack (Postgres + backend + frontend) lives in the frontend repo's
`deployments/` Docker Compose, mirroring the registry setup.

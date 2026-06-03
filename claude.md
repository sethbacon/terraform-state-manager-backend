# CLAUDE.md — Terraform State Manager Backend

## Development Workflow

All changes follow this workflow. Do not deviate from it.

### Branches

- `main` — the single long-lived branch. Production-ready; releases are tagged here. **Must always exist — never delete.**
- Work happens on short-lived branches created **from `main`** and merged back **into `main`** via PR. There is no `development` integration branch (standardized on the registry workflow).
- Branch naming follows the registry convention:

  | Type          | Pattern                  | Example                         |
  | ------------- | ------------------------ | ------------------------------- |
  | Feature       | `feat/short-description` | `feat/repo-metadata-extraction` |
  | Bug fix       | `fix/issue-description`  | `fix/snapshot-diff-nil-panic`   |
  | Documentation | `docs/topic`             | `docs/drift-runbook`            |
  | Refactor      | `refactor/area`          | `refactor/analyzer-counter`     |

- Delete the branch after its PR is squash-merged:

```bash
# After a feature/fix PR is merged:
git push origin --delete fix/short-description   # remove remote branch
git branch -d fix/short-description              # remove local branch
git remote prune origin                          # prune stale remote-tracking refs
```

### Conventional Commits

PR titles and commit messages follow [Conventional Commits](https://www.conventionalcommits.org/). This drives automated releases (see below) and is validated in CI.

| Type       | When to use                                |
| ---------- | ------------------------------------------ |
| `feat`     | New user-facing feature (minor bump)       |
| `fix`      | Bug fix, incl. security fixes (patch bump) |
| `perf`     | Performance improvement (patch bump)       |
| `refactor` | Code restructure, no behaviour change      |
| `docs`     | Documentation only                         |
| `style`    | Whitespace/formatting only                 |
| `test`     | Adding or fixing tests                     |
| `build`    | Build system / dependency updates          |
| `ci`       | CI/CD workflow changes                     |
| `chore`    | Maintenance, tooling                       |
| `revert`   | Reverts a previous commit                  |

Append `!` (`feat!:`) or add a `BREAKING CHANGE:` footer for a major bump. Keep the subject under 72 characters; reference the issue in the body with `Closes #<n>`.

### Step-by-step

1. **Open a GitHub issue** describing the bug or feature before writing any code (for substantial changes).

2. **Create a branch from `main`**:

   ```bash
   git fetch origin
   git checkout -b feat/short-description origin/main
   # or: fix/short-description
   ```

3. **Implement the change.**

4. **Before committing — run the full local quality gate**:

   ```bash
   cd backend

   # Format & vet
   go fmt ./...
   go vet ./...

   # Tests with race detector and coverage
   go test ./... -race -coverprofile=coverage.out -covermode=atomic
   go tool cover -func=coverage.out | grep "^total:"

   # Security scan — fix or suppress new findings before pushing
   gosec ./...
   ```

   Do not push until all of the above pass locally.

5. **Commit with a Conventional Commit message — no co-author attribution**:

   ```bash
   git add <specific files>
   git commit -m "fix: short description of what was fixed

   Closes #<issue-number>"
   ```

6. **Rebase onto `main` before pushing** to minimise merge conflicts with sibling branches:

   ```bash
   git fetch origin
   git rebase origin/main
   ```

7. **Push and open a PR targeting `main`** with a Conventional Commit title:

   ```bash
   git push -u origin feat/short-description
   gh pr create --base main --title "feat: short description" --body "$(cat <<'EOF'
   Closes #<issue>

   <what changed, why, and how it was tested>
   EOF
   )"
   ```

   Do **not** add a `## Changelog` section and do **not** edit `CHANGELOG.md` — release-please generates the changelog from Conventional Commit titles (see Releasing).

8. **Merge:** once all CI checks are green and at least one reviewer approves, **squash-merge into `main`**. The PR title becomes the commit message; delete the branch afterward.

### Parallel agents — coordination rules

When multiple agents run concurrently, follow these rules to avoid conflicts:

- **Never assign two agents to work on the same files at the same time.** If their scopes overlap (e.g. both touch the same handler or config file), serialise them.
- **Do not edit `CHANGELOG.md` in any branch** — release-please owns it. This eliminates the most common parallel-agent conflict.
- **Each agent rebases on `origin/main` immediately before pushing** (step 6 above). After any sibling PR is merged, remaining open branches must rebase again before their own merge.

### Releasing a version

Releases are automated with **release-please** (Conventional Commits → version bump + `CHANGELOG.md` + tag + GitHub release), with **goreleaser** building artifacts — matching the registry. The flow is:

1. Conventional Commits merged into `main` cause release-please to open and maintain a **release PR** that bumps the version and updates `CHANGELOG.md`.

2. **Merging that release PR** creates the version tag and GitHub release on `main`. Do not hand-edit `CHANGELOG.md` and do not create tags manually.

> **Adoption note:** the registry's CI/release automation — release-please, Conventional-Commit PR-title validation, the `gosec` baseline comparison, and the coverage gate — is being wired into this repo as part of the revitalization (Phase 0). Until those workflows land, run the local quality gate manually and keep commits Conventional so release-please works the moment it is enabled.

---

## Project Overview

An enterprise-grade Terraform State Manager backend providing centralised state file management, analytics, and operations across multiple cloud backends.

Core capabilities:

- **State Source Management** — Connect to HCP Terraform, S3, Azure Blob, GCS, Consul, PostgreSQL, Kubernetes, HTTP, and local backends
- **State Analysis** — Parse and analyse Terraform state files for resource counts, RUM, provider distribution, and version tracking
- **Drift Detection** — Capture state snapshots on a schedule and detect configuration drift between snapshots
- **Backup & Restore** — On-demand and scheduled backups with configurable retention policies and integrity verification
- **State Migration** — Move state files between backends with dry-run validation
- **Compliance** — Policy-based compliance evaluation (tagging, naming, version, custom rules)
- **Reports & Dashboards** — Generate and export reports; real-time dashboard aggregations
- **Alerts & Notifications** — Rule-based alerting with email, Slack, webhook, and PagerDuty channels
- **Task Scheduler** — Cron-based scheduling for analysis, snapshot, backup, and report tasks

Current version: **v0.1.0**.

Frontend UI lives in a separate repository.

---

## Repository Structure

```txt
terraform-state-manager-backend/
├── Makefile                          # Local dev targets (dev-up, dev-down)
├── backend/                          # Go 1.25 backend service
│   ├── cmd/server/main.go            # Entry point (serve, migrate, version)
│   ├── config.example.yaml           # Configuration template
│   ├── docker-compose.yml            # Development environment
│   ├── Dockerfile                    # Multi-stage Go build
│   ├── nginx.conf                    # Reverse proxy config for combined deployments
│   ├── docs/
│   │   ├── swagger.yaml              # OpenAPI specification (source of truth)
│   │   └── embed.go                  # go:embed directive for binary inclusion
│   ├── internal/
│   │   ├── api/                      # Gin HTTP handlers, organised by feature
│   │   │   ├── admin/                # Users, API keys, OIDC config, org management
│   │   │   ├── alerts/               # Alert management and alert rules
│   │   │   ├── analysis/             # Analysis run endpoints
│   │   │   ├── backups/              # Backup operations and retention policies
│   │   │   ├── compliance/           # Compliance policies and results
│   │   │   ├── dashboards/           # Dashboard aggregation endpoints
│   │   │   ├── migrations/           # State migration jobs
│   │   │   ├── notifications/        # Notification channel management
│   │   │   ├── reports/              # Report generation and download
│   │   │   ├── scheduler/            # Scheduled task management
│   │   │   ├── setup/                # First-run setup wizard
│   │   │   ├── snapshots/            # Snapshot capture and comparison
│   │   │   ├── sources/              # State source management
│   │   │   ├── webhooks/             # Webhook-triggered analysis
│   │   │   ├── auth_handlers.go      # OAuth/OIDC login, callback, logout
│   │   │   ├── health.go             # Liveness and readiness probes
│   │   │   └── router.go             # Route configuration and middleware wiring
│   │   ├── auth/
│   │   │   ├── jwt.go                # JWT generation and validation
│   │   │   ├── apikey.go             # API key generation and validation
│   │   │   ├── scopes.go             # Permission scope definitions
│   │   │   └── oidc/provider.go      # OIDC provider implementation
│   │   ├── clients/                  # Terraform state backend clients
│   │   │   ├── azure/                # Azure Blob state client
│   │   │   ├── consul/               # Consul state client
│   │   │   ├── gcs/                  # Google Cloud Storage state client
│   │   │   ├── hcp/                  # HCP Terraform state client
│   │   │   ├── http/ & http_backend/ # Generic HTTP state client
│   │   │   ├── k8s/                  # Kubernetes state client
│   │   │   ├── pg/                   # PostgreSQL state client
│   │   │   ├── s3/                   # AWS S3 state client
│   │   │   └── client.go             # Client interface definition
│   │   ├── config/config.go          # Viper-based configuration
│   │   ├── crypto/tokencipher.go     # AES-256 encryption for sensitive fields
│   │   ├── db/
│   │   │   ├── db.go                 # Database connection and migration runner
│   │   │   ├── models/               # Data models (22 files)
│   │   │   ├── repositories/         # Data access layer — repository pattern (20+ files)
│   │   │   └── migrations/           # Versioned SQL migration files (10 migrations)
│   │   ├── middleware/
│   │   │   ├── auth.go               # JWT and API key authentication
│   │   │   ├── rbac.go               # Role-based access control
│   │   │   ├── audit.go              # Audit logging
│   │   │   ├── metrics.go            # Prometheus request metrics
│   │   │   ├── ratelimit.go          # Rate limiting
│   │   │   ├── requestid.go          # Request ID injection
│   │   │   ├── security.go           # Security headers
│   │   │   └── setup.go              # Setup token validation
│   │   ├── services/                 # Business logic
│   │   │   ├── analyzer/             # State file parsing and analysis
│   │   │   ├── backup/               # Backup and restore operations
│   │   │   ├── compliance/           # Policy evaluation engine
│   │   │   ├── migration/            # State migration service
│   │   │   ├── notification/         # Alert notification dispatch
│   │   │   ├── reporter/             # Report generation
│   │   │   ├── scheduler/            # Cron-based task scheduler
│   │   │   └── snapshot/             # Snapshot capture and drift detection
│   │   ├── storage/                  # File storage backend abstraction
│   │   │   ├── azure/                # Azure Blob Storage
│   │   │   ├── gcs/                  # Google Cloud Storage
│   │   │   ├── local/                # Local filesystem
│   │   │   ├── s3/                   # AWS S3 / S3-compatible
│   │   │   └── backend.go            # Storage factory
│   │   ├── telemetry/                # Prometheus metrics and pprof profiling
│   │   └── validation/               # Input validation helpers
│   └── scripts/
│       └── seed-demo-data.sql        # Demo data for development
└── deployments/
    ├── kubernetes/
    │   ├── base/                     # Base Kustomization (deployment, service, ingress)
    │   └── overlays/dev/ & prod/     # Environment-specific patches
    ├── helm/tsm/                     # Helm chart (Chart.yaml, values.yaml, templates/)
    └── dex/config.yaml               # Dex OIDC provider config for development
```

---

## Tech Stack

| Concern        | Technology                                                  |
| -------------- | ----------------------------------------------------------- |
| Language       | Go 1.25.0                                                   |
| HTTP Framework | Gin v1.10.0                                                 |
| Database       | PostgreSQL 16+ via sqlx v1.4.0                              |
| Migrations     | golang-migrate v4.17.0 (10 migrations, embedded in binary)  |
| Auth           | JWT (golang-jwt/jwt v5), API keys, OIDC (coreos/go-oidc v3) |
| Config         | Viper v1.18.2 (`TSM_` env prefix overrides YAML)            |
| Storage        | Local filesystem, Azure Blob, S3-compatible, GCS            |
| Scheduling     | robfig/cron v3 — cron-expression task scheduler             |
| Encryption     | AES-256 (golang.org/x/crypto) for stored secrets            |
| UUID           | google/uuid v1.6.0                                          |
| Metrics        | prometheus/client_golang v1.23.2                            |
| Logging        | log/slog (stdlib, structured JSON)                          |

---

## Common Commands

### Backend

```bash
cd backend

# Install dependencies
go mod download

# Start development server (also runs migrations automatically)
go run cmd/server/main.go serve

# Run database migrations manually
go run cmd/server/main.go migrate up
go run cmd/server/main.go migrate down

# Print version info
go run cmd/server/main.go version

# Build production binary (Linux)
CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o server ./cmd/server

# Run all tests
go test ./...

# Run tests with race detector and coverage
go test ./... -race -coverprofile=coverage.out -covermode=atomic
go tool cover -func=coverage.out | grep "^total:"

# Format code
go fmt ./...

# Vet code
go vet ./...
```

### Docker Compose (Quickstart)

```bash
# Start development stack (PostgreSQL + backend)
make dev-up

# Stop development stack
make dev-down

# Stop and remove volumes
make dev-down-volumes
```

---

## Configuration

Copy and edit the template before running the backend:

```bash
cp backend/config.example.yaml backend/config.yaml
```

Key environment variables (all prefixed `TSM_`):

```bash
# Database
TSM_DATABASE_HOST=localhost
TSM_DATABASE_PORT=5432
TSM_DATABASE_NAME=tsm
TSM_DATABASE_USER=tsm
TSM_DATABASE_PASSWORD=<password>
TSM_DATABASE_SSLMODE=disable          # use "require" in production

# Server
TSM_SERVER_PORT=8080
TSM_SERVER_HOST=0.0.0.0

# Security (required in production)
TSM_AUTH_JWT_SECRET=<32+ byte secret>
ENCRYPTION_KEY=<32-byte key>          # encrypts stored OIDC client secrets and tokens

# Storage backend: local | s3 | azure | gcs
TSM_STORAGE_DEFAULT_BACKEND=local

# Auth providers
TSM_AUTH_API_KEYS_ENABLED=true        # API key prefix defaults to "tsm"
TSM_OIDC_ENABLED=false

# Multi-tenancy
TSM_MULTI_TENANCY_ENABLED=false
TSM_MULTI_TENANCY_DEFAULT_ORGANIZATION=default

# Telemetry / Prometheus
TSM_TELEMETRY_ENABLED=true
TSM_TELEMETRY_METRICS_PROMETHEUS_PORT=9090

# Optional: write setup token to a file on first boot
SETUP_TOKEN_FILE=/run/secrets/setup-token
```

---

## Architecture Conventions

### Backend Layering

```txt
HTTP Handler (api/)
  → Middleware chain: Recovery → RequestID → Metrics → Logger → CORS → Security
  → Auth middleware (JWT / API key) → Rate limit → RBAC
  → Service layer (services/)
  → Repository (db/repositories/)
  → Database (db/models/, PostgreSQL)
  → Storage Backend (storage/) or State Client (clients/)
```

- **Repository pattern** for all database access — never query the DB directly from handlers.
- **Service layer** for all business logic — handlers delegate to services, not repositories.
- **Factory pattern** for storage backends and state source clients.
- **Interface-based** abstractions for both storage (`storage.Backend`) and state clients (`clients.Client`); add new implementations by satisfying the interface.
- **UUID primary keys** throughout.
- **JSONB columns** used for flexible config fields (backend configs, rule configs, resource type breakdowns, violations).
- All responses follow a consistent JSON envelope; errors include `status` and `message`.

### Database

- 10 versioned SQL migrations in `backend/internal/db/migrations/`.
- Migrations are embedded in the binary at compile time via `go:embed`.
- Migrations run automatically at startup; use `migrate up/down` for manual control.
- Always add a new migration file rather than editing existing ones.

### API Endpoints (summary)

- Health/readiness: `GET /api/v1/health`, `GET /api/v1/ready`, `GET /api/v1/version`
- Setup wizard: `GET|POST /api/v1/setup/*` (setup token required)
- Auth: `GET /api/v1/auth/login`, `GET /api/v1/auth/callback`, `POST /api/v1/auth/refresh`
- Sources: `GET|POST|PUT|DELETE /api/v1/sources`
- Analysis: `GET|POST /api/v1/analysis/runs`, `GET /api/v1/analysis/summary`
- Snapshots: `GET|POST /api/v1/snapshots`, `GET /api/v1/drift/events`
- Backups: `GET|POST /api/v1/backups`, `POST /api/v1/backups/:id/restore`
- Retention: `GET|POST|PUT|DELETE /api/v1/retention-policies`
- Migrations: `GET|POST /api/v1/migrations`, `POST /api/v1/migrations/dry-run`
- Compliance: `GET|POST|PUT|DELETE /api/v1/compliance/policies`, `GET /api/v1/compliance/results`
- Reports: `GET|POST /api/v1/reports`, `GET /api/v1/reports/:id/download`
- Dashboards: `GET /api/v1/dashboards/overview|resources|providers|trends|...`
- Alerts: `GET /api/v1/alerts`, `PUT /api/v1/alerts/:id/acknowledge`
- Alert rules: `GET|POST|PUT|DELETE /api/v1/alert-rules`
- Notifications: `GET|POST|PUT|DELETE /api/v1/notifications`
- Scheduler: `GET|POST|PUT|DELETE /api/v1/scheduler`, `POST /api/v1/scheduler/:id/trigger`
- Webhooks: `POST /api/v1/webhooks/trigger`
- Admin: `GET|POST|PUT|DELETE /api/v1/admin/{users,organizations,api-keys,role-templates,oidc,...}`
- OpenAPI spec: `GET /api/v1/swagger.yaml`, `GET /api/v1/swagger.json`

---

## Authentication & Authorization

- **JWT** — issued at OIDC login, stateless, short-lived (24h default). HMAC-SHA256 signed.
- **API Keys** — format `tsm_<random>`, scoped bearer tokens for CI/CD; stored as bcrypt hash with prefix index for fast lookup.
- **OIDC** — generic OpenID Connect provider support. Configured via setup wizard (DB-stored, encrypted) or config file. Provider swapped at runtime via `atomic.Pointer` — no restart required.
- **RBAC** — scopes assigned per organization via role templates. Middleware variants: `RequireScope`, `RequireAnyScope`, `RequireOrgScope`.
- **Setup Token** — one-time `Authorization: SetupToken <token>` scheme for first-run configuration, separate from JWT/API key auth.
- Audit logs record every mutating action with user ID, IP, and timestamp.

### RBAC Scopes

Read/write pairs (write implies read): `analysis`, `sources`, `backups`, `migrations`, `reports`, `dashboard`, `compliance`, `users`, `organizations`.

Standalone scopes: `scheduler:admin`, `alerts:admin`, `api_keys:manage`, `audit:read`, `admin` (wildcard — all permissions).

### Built-in Role Templates

| Role     | Primary Scopes                                                                                                                             |
| -------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| admin    | `admin` (all permissions)                                                                                                                  |
| analyst  | `analysis:*`, `reports:*`, `dashboard:read`, `sources:read`, `compliance:read`                                                             |
| viewer   | `analysis:read`, `reports:read`, `dashboard:read`, `sources:read`                                                                          |
| operator | `analysis:*`, `reports:*`, `dashboard:*`, `sources:*`, `compliance:*`, `users:read`, `organizations:read`, `api_keys:manage`, `audit:read` |

### Setup Wizard (First-Run)

- On first startup, a one-time setup token is generated and printed to stderr.
- Setup endpoints (`/api/v1/setup/*`) are authenticated via `SetupTokenMiddleware`.
- Configured OIDC is stored encrypted in `oidc_config` table (DB takes precedence over config file).
- After `POST /api/v1/setup/complete`, setup token is invalidated and endpoints return 403 permanently.
- Set `SETUP_TOKEN_FILE` to write the token to a file instead of relying on log capture.

---

## State Source Clients

Configured per-source via `source_type` and `config` (JSONB) in the `state_sources` table.

| Source Type     | Client Location            | Backend              |
| --------------- | -------------------------- | -------------------- |
| `hcp_terraform` | `internal/clients/hcp/`    | HCP Terraform Cloud  |
| `s3`            | `internal/clients/s3/`     | AWS S3 / compatible  |
| `azure_blob`    | `internal/clients/azure/`  | Azure Blob Storage   |
| `gcs`           | `internal/clients/gcs/`    | Google Cloud Storage |
| `consul`        | `internal/clients/consul/` | HashiCorp Consul     |
| `pg`            | `internal/clients/pg/`     | PostgreSQL           |
| `kubernetes`    | `internal/clients/k8s/`    | Kubernetes (etcd)    |
| `http`          | `internal/clients/http/`   | HTTP/HTTPS backend   |
| `local`         | (local FS)                 | Local filesystem     |

Add new source types by implementing the `clients.Client` interface and registering in the client factory.

---

## Storage Backends

Used for backups, reports, and exported artifacts. Configured via `TSM_STORAGE_DEFAULT_BACKEND`.

| Backend              | Config Prefix         |
| -------------------- | --------------------- |
| Local filesystem     | `TSM_STORAGE_LOCAL_*` |
| AWS S3 / compatible  | `TSM_STORAGE_S3_*`    |
| Azure Blob Storage   | `TSM_STORAGE_AZURE_*` |
| Google Cloud Storage | `TSM_STORAGE_GCS_*`   |

Implement `storage.Backend` interface to add new backends.

---

## Background Services

The task scheduler (`internal/services/scheduler/`) polls the `scheduled_tasks` table every 60 seconds and executes due tasks concurrently.

| Task Type  | Service              | Description                                 |
| ---------- | -------------------- | ------------------------------------------- |
| `analysis` | `services/analyzer/` | Fetch and analyse state files from sources  |
| `snapshot` | `services/snapshot/` | Capture state snapshots, detect drift       |
| `backup`   | `services/backup/`   | Write state file backups to storage backend |
| `report`   | `services/reporter/` | Generate and persist reports                |

Additional background concerns:

- **Drift detection** — Snapshot service compares newly captured snapshots to previous ones and writes `drift_events` rows.
- **Compliance** — Compliance service evaluates active policies against the latest analysis results.
- **Notifications** — Notification service dispatches alerts via email, Slack, webhook, or PagerDuty when alert rules fire.
- **API key expiry warnings** — Configurable interval check (default 24h); notifies owners before keys expire.
- **Graceful shutdown** — Background services listen for SIGINT/SIGTERM and stop cleanly.

---

## Deployment Options

| Option                 | Location                     |
| ---------------------- | ---------------------------- |
| Docker Compose (dev)   | `backend/docker-compose.yml` |
| Standalone binary      | `go build` + systemd unit    |
| Kubernetes + Kustomize | `deployments/kubernetes/`    |
| Helm Chart             | `deployments/helm/tsm/`      |
| Nginx reverse proxy    | `backend/nginx.conf`         |

---

## API Documentation (OpenAPI / Swagger)

The API is documented with an OpenAPI 3.0 specification.

**Architecture:**

- Source spec lives at `backend/docs/swagger.yaml`
- Embedded into the binary at compile time via `go:embed` in `backend/docs/embed.go`
- Served at `GET /api/v1/swagger.yaml` and `GET /api/v1/swagger.json`

**Conventions (mandatory):**

- **Every new handler** must have a corresponding entry in `swagger.yaml` before it is committed.
- **Every modified handler** must have its spec entry updated to match.
- All authenticated endpoints must declare the `BearerAuth` security scheme.
- Use `{param}` notation in path templates.
- Tags must be title-cased and drawn from the established vocabulary:
  `Authentication`, `Setup`, `Health`, `API Keys`, `Users`, `Organizations`, `Role Templates`,
  `OIDC`, `Sources`, `Analysis`, `Snapshots`, `Drift`, `Backups`, `Retention Policies`,
  `Migrations`, `Compliance`, `Reports`, `Dashboards`, `Alerts`, `Alert Rules`,
  `Notifications`, `Scheduler`, `Webhooks`, `Admin`

---

## Development Notes

- No `.github/workflows/` CI pipeline exists yet. Run the quality gate manually before pushing (see step 4 above).
- `backend/scripts/seed-demo-data.sql` inserts demo organisations, users, and roles for local testing.
- The `deployments/dex/config.yaml` provides a local Dex OIDC provider for end-to-end auth testing.
- `CHANGELOG.md` tracks version history; do not edit it in feature branches.
- In development mode (`TSM_AUTH_JWT_SECRET` not set), the server auto-generates a random JWT secret on startup — do not rely on this in production.

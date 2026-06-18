<!-- markdownlint-disable MD013 -->
# Development Guide

This document describes how to set up a local development environment for the State Manager backend, regenerate the OpenAPI spec, and run the tests.

## Prerequisites

- Go 1.26+
- A reachable PostgreSQL 14+ (16 recommended) for `make run` and integration paths
- Make (optional, but the targets below assume it)

The full local stack — Keycloak (OIDC), seeded state fixtures, and the React UI — lives in the [terraform-state-manager-frontend](https://github.com/sethbacon/terraform-state-manager-frontend) repository under `deployments/`. It is the easiest way to exercise the cookie-auth and SSO flows end to end.

## Install Tools

The OpenAPI generator (`swag`) is pinned to the version CI uses:

```bash
go install github.com/swaggo/swag/cmd/swag@v1.16.4
```

## Generate the OpenAPI Spec

`backend/docs/swagger.json` is generated from the handler `swag` annotations (the annotations are the source of truth) and is committed. CI fails if the committed file is stale, so regenerate and commit after changing any `@Summary`/`@Router`/`@Param` annotation:

```bash
make swag
# equivalently:
cd backend && swag init -g cmd/server/main.go --outputTypes json
# then commit backend/docs/swagger.json if it changed
```

## Run the Backend (Dev)

`make run` builds and runs the server against the database in `config.example.yaml`. Provide the two required secrets via the environment first (in dev you can let `DEV_MODE` mint an ephemeral JWT secret instead):

```bash
export TSM_ENCRYPTION_KEY=$(openssl rand -hex 32)

# DEV_MODE enables an ephemeral JWT secret and POST /api/v1/dev/login (used by E2E).
DEV_MODE=true make run
```

`make run` resolves to `CONFIG_PATH=../config.example.yaml go run ./cmd/server serve`. On `serve`, the backend runs both schema migration sets (app + identity) under advisory locks before listening. To run migrations imperatively instead:

```bash
cd backend && go run ./cmd/server migrate up   # or: down
```

For every `TSM_*` environment variable and its YAML equivalent, see the [Configuration Reference](configuration.md). **Never enable `DEV_MODE` in production** — it weakens JWT secret handling and exposes the dev-login endpoint.

## Makefile Targets

| Target | What it does |
| --- | --- |
| `make build` | Build the server binary (`CGO_ENABLED=0`, version/build-date via ldflags) |
| `make run` | Run the server against `config.example.yaml` (expects a reachable Postgres) |
| `make test` | `go test ./...` |
| `make vet` | `go vet ./...` |
| `make tidy` | `go mod tidy` |
| `make swag` | Regenerate `backend/docs/swagger.json` from annotations |
| `make docker` | Build the backend Docker image |

## Tests and Coverage

CI runs the unit suite with the race detector over the internal packages and enforces a filtered-coverage floor:

```bash
cd backend
go clean -testcache
go test ./internal/... -race -coverprofile=coverage.out -covermode=atomic
```

### Coverage Floor

CI computes coverage on the **filtered** profile and fails the build if total statement coverage drops below the hard floor of **79%**. The floor is a ratchet: every change that touches a package should add tests and raise it. The target is parity with the sibling registry backend's 80% floor.

The `coverfilter` step excludes integration-only functions — those whose doc comment carries a `coverage:skip:` marker (live database, real OIDC issuer, and similar paths that cannot be unit-tested) — from the denominator, mirroring the registry pattern.

### Measure Coverage Locally

To match CI exactly, run the same package set with `-race`, then apply the filter:

```bash
cd backend
go test ./internal/... -race -coverprofile=coverage.out -covermode=atomic
# Filter integration-only functions, mirroring CI
go run ./scripts/coverfilter -in coverage.out -out coverage.filtered.out -root .
# Total (the number CI compares against the 79% floor)
go tool cover -func=coverage.filtered.out | grep "^total:"
# HTML report (opens in browser)
go tool cover -html=coverage.filtered.out
# Per-package, lowest-covered first
go tool cover -func=coverage.filtered.out | grep -v "^total:" | awk '{print $NF, $0}' | sort -V
```

Risk-based component targets (the floor is the whole-suite minimum; security and core-logic packages should sit well above it):

| Component category | Packages | Target |
| --- | --- | --- |
| Security & core auth | `internal/auth`, `internal/middleware`, `internal/crypto` | 85–95% |
| State engine | `internal/statesource`, `internal/stateops`, `internal/analyzer` | 80–90% |
| Core business logic | `internal/db/repositories`, `internal/bootstrap` | 80–90% |
| APIs & handlers | `internal/api/...` | 75–85% |
| Background services | `internal/services/...` | 70–80% |
| Config & utilities | `internal/config`, `internal/telemetry` | 70–80% |

## Other CI Gates

Beyond tests and coverage, CI runs `go vet`, a `gosec` scan compared against `backend/gosec-baseline.json`, and a check that the committed `docs/swagger.json` matches the annotations. Run `go vet ./...` and `make swag` locally before pushing to avoid surprises.

## Troubleshooting

- **`TSM_JWT_SECRET is required in production`** at boot — set the secret, or run with `DEV_MODE=true` for an ephemeral one.
- **Setup endpoints return `403`** — setup is already complete (the wizard is permanently disabled). To re-run setup in a throwaway dev database, clear `setup_completed`/`setup_token_hash` from `system_settings` and restart.
- **`swag init` reports missing annotations** — a handler is missing its `@Router`/`@Summary` block; add it and re-run `make swag`.

For frontend development and E2E setup, see [terraform-state-manager-frontend](https://github.com/sethbacon/terraform-state-manager-frontend).

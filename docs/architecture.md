<!-- markdownlint-disable MD013 -->
# Architecture

This document describes the design of the Terraform State Manager (TSM) backend — what the components are, how they interact, and why key architectural decisions were made.

## Overview

The State Manager is a control plane for Terraform state **where it already lives** — it analyzes, edits, and watches state across existing backends without migrating it into a proprietary store. The backend exposes a single REST API (`/api/v1`) covering:

- **State sources** — connect to ten backend types, read state, and analyze it (resources, providers, modules, Terraform version, serial/lineage, RUM).
- **State editing** — guarded raw replace and `rm`/`mv` operations through a validate → backup → write → audit pipeline, plus backup/restore and locking.
- **Drift detection** — durable, acknowledgeable drift records, CI-dispatched runs, and an idempotent push-style ingest endpoint.
- **Version Lab** — dispatch `terraform init/plan` against pinned Terraform/provider/module versions through existing CI and ingest results via callback.
- **Scheduling, transfers, and reporting** — cron-driven runs, cross-source state copy/migrate, and Markdown reports.
- **Identity and provisioning** — cookie-based JWT sessions, five SSO providers, API keys, scoped RBAC, and audit logs over the shared identity schema.

Design goals, in order: **safety of state mutations first**, then security, then operational simplicity. The application layer is stateless except for the single background worker (see [Background Workers](#background-workers)); all durable state lives in PostgreSQL, and source credentials are encrypted at rest. This makes the API tier horizontally scalable.

The backend deliberately shares operational conventions — config layering, embedded migrations, side-channel metrics, release tooling — with the sibling [terraform-registry-backend](https://github.com/sethbacon/terraform-registry-backend).

---

## Repository Structure

The project is split across two repositories:

| Repository | Contents | Docker image |
| --- | --- | --- |
| [`terraform-state-manager-backend`](https://github.com/sethbacon/terraform-state-manager-backend) | Go backend, database migrations, all deployment configs (Helm, Kustomize, Terraform IaC, cloud scripts) | `ghcr.io/sethbacon/terraform-state-manager-backend` |
| [`terraform-state-manager-frontend`](https://github.com/sethbacon/terraform-state-manager-frontend) | React SPA, nginx, the local dev stack (Keycloak, seeded states), E2E tests | `ghcr.io/sethbacon/terraform-state-manager-frontend` |

All production deployment infrastructure lives in this (backend) repository. The local development stack (Keycloak realm, seeded state fixtures, `DEV_MODE`) lives in the frontend repository and is not a production artifact.

---

## Component Diagram

```txt
┌─────────────────────────────────────────────────────────────┐
│   Browser (SPA)   │   CI runners   │   IdP (OIDC/SAML/LDAP)  │
└─────────┬───────────────┬───────────────────┬───────────────┘
          │ HTTPS         │ HTTPS             │ TLS
          ▼               ▼                   ▼
┌─────────────────────────────────────────────────────────────┐
│        Ingress / Reverse Proxy (nginx / ALB / etc.)         │
│  Terminates TLS, serves SPA at /, routes /api/* to backend  │
└──────────┬───────────────────────────────────┬──────────────┘
           │                                   │
           ▼                                   ▼
┌──────────────────────┐           ┌───────────────────────────┐
│   Go Backend (Gin)   │           │   React SPA (nginx)       │
│   :8080              │◄──────────│   :80                     │
│                      │  REST API │                           │
│  /health  /ready     │           │  /sources /drift /admin   │
│  /api/v1/sources/*   │           │  /health-lab /api-docs    │
│  /api/v1/drift/*     │           └───────────────────────────┘
│  /api/v1/health-lab/*│
│  /api/v1/admin/*     │     ┌─── side channel ───┐
│  /scim/v2/* (opt.)   │     │  :9090 /metrics    │ ← internal only
│  /swagger.json       │     └────────────────────┘
└──────┬───────────────┴──────────────┬───────────────────────┘
       │                              │  ConnectSource (per request)
       ▼                              ▼
┌──────────────┐        ┌──────────────────────────────────────┐
│  PostgreSQL  │        │      State Sources (10 connectors)    │
│  app schema  │        │  HCP/TFC · S3 · Azure Blob · GCS ·    │
│  + identity  │        │  Consul · PostgreSQL · Kubernetes ·   │
│   schema     │        │  HTTP · Git · local filesystem        │
└──────────────┘        └──────────────────────────────────────┘
```

The backend connects *outward* to each configured state source on demand (`ConnectSource`); state is never copied into TSM's own database except as time-bounded analysis records and pre-write backups.

---

## Backend Layer Architecture

The router (`internal/api/router.go`) builds a fixed global middleware chain, then layers per-group and per-route guards:

```txt
Gin Engine
  └─► Recovery               (panic → 500, never crashes the server)
        └─► RequestID         (X-Request-ID echo or fresh UUID)
              └─► SecurityHeaders   (CSP, HSTS, frame-options, CORP/COEP/COOP)
                    └─► Metrics      (request count + latency histograms)
                          └─► [mTLS AuthMiddleware]   (only if auth.mtls.enabled)
                                └─► /api/v1 group: CSRFProtect
                                      └─► AuthMiddleware       (JWT cookie/header or API key)
                                            └─► RequireScope   (per-route scope gate)
                                                  └─► Handler
                                                        └─► Repository (sqlx / database/sql)
                                                              └─► Connector (state source I/O)
```

This ordering is intentional:

- **Recovery and RequestID** run first so every response — including panics and error responses — carries a request ID and the server never dies on a handler panic.
- **Security headers** are applied before any application logic so they appear on all responses. The API CSP is the strictest possible: `default-src 'none'; frame-ancestors 'none'` (`internal/middleware/security.go`) — the API serves JSON, not HTML, so it needs no script/style sources.
- **mTLS auth** (when enabled) runs before JWT auth and *additively* authenticates a request that presented a verified client certificate. `AuthMiddleware` then short-circuits for an already-`mtls`-authenticated request.
- **CSRF** is scoped to `/api/v1` and only fires for cookie-authenticated mutations (see [Authentication Flow](#authentication-flow)).
- **Authentication** populates `user_id`, `scopes`, and `auth_method` on the Gin context. Every later middleware and handler reads from this context — they never re-authenticate.
- **RBAC** (`RequireScope`) runs after auth and reads the scopes auth set. Declaring the required scope at route-registration time keeps authorization visible alongside the route table.

Handlers follow the **repository pattern**: they call repository methods in `internal/db/repositories/` rather than constructing SQL inline, keeping queries in one place and testable in isolation.

---

## State Source Abstraction

The ten supported backends sit behind one interface (`internal/statesource/statesource.go`):

```go
type Connector interface {
    List(ctx context.Context) ([]StateRef, error)
    Read(ctx context.Context, key string) (*RawState, error)
    Write(ctx context.Context, key string, data []byte) error
}

// Optional: implemented only by backends with native locking.
type Locker interface {
    Lock(ctx context.Context, key string) (lockID string, err error)
    Unlock(ctx context.Context, key, lockID string) error
}
```

`statesource.New(sourceType, config, credentials)` is a factory switch over the ten types: `local`, `hcp`, `s3`, `azureblob`, `gcs`, `git`, `consul`, `pg`, `kubernetes`, `http`. Config holds non-secret connection details; `credentials` is the decrypted secret map (nil for backends that need none, e.g. `local`).

Two cross-cutting contracts make the abstraction safe:

- **`ErrNotFound` is load-bearing.** `IsNotFound(err)` distinguishes "the state does not exist" (safe to treat as a first write) from "the backend failed" (must abort). This underpins the fail-closed write guard — see [ADR 002](adr/002-fail-closed-state-writes.md).
- **Locking is per-connector.** Backends that implement `Locker` (HCP workspace lock-then-verify, Consul session locks, local lock files, `http` backends with a lock address) use native locks; the rest (S3, GCS, Azure Blob, git) fall back to an application-level advisory lock in PostgreSQL with a 15-minute stale TTL — see [ADR 003](adr/003-advisory-lock-ttl.md). Consul's lock is a session (TTL 15m, auto-released on crash) acquiring `<key>/.lock` — the **same key Terraform's consul backend locks** — so an edit and a concurrent `terraform apply` mutually exclude. HCP and Consul additionally guard against TOCTOU races: HCP verifies serial/lineage *under* the workspace lock, and Consul writes with `?cas=<ModifyIndex>` under its lock as defense-in-depth against writers that bypass locking.

Source credentials are encrypted with AES-256-GCM before they touch the database and are never returned by the API — see [Credential Encryption](#credential-encryption).

---

## State Edit Pipeline

Every mutating state operation runs the same pipeline (`internal/api/edit.go`): **acquire lock → read current (fail-closed) → validate → backup → write → audit → async refresh**.

1. **Lock.** `acquireLock` picks the native `Locker` if the connector supports it, else the app-level advisory lock.
2. **Read current, fail closed.** Only a genuine `IsNotFound` may skip the backup and serial check; any other read error aborts with `502 Bad Gateway` ([ADR 002](adr/002-fail-closed-state-writes.md)).
3. **Validate.** The replacement must parse as valid Terraform state. Unless `?force=true`, the new serial must not regress below the current serial and the lineage must match — otherwise `409 Conflict`.
4. **Backup.** The current state is recorded as a restorable backup before any write, so every edit is one-click reversible.
5. **Write.** The connector writes the new bytes; structured `rm`/`mv` edits are computed in `internal/stateops` (the serial is bumped on success; `import` is intentionally out of scope because it needs a provider run).
6. **Audit + refresh.** The action is written to the audit log, and the analysis store is refreshed asynchronously so dashboards reflect the change without blocking the response.

`DELETE /sources/:id/state/lock` (`admin` scope) is the force-unlock escape hatch for an orphaned app-level lock; it never touches native backend locks.

---

## Authentication Flow

The backend supports **session JWTs** (browser), **API keys** (automation), **mTLS** (machine-to-machine), and SSO login via **OIDC**, **SAML 2.0**, and **LDAP**. SCIM 2.0 provisioning is available behind a dedicated scope.

Browser sessions do **not** use a `Bearer` header. After login the JWT is set as an **HttpOnly `tsm_auth_token` cookie** (inaccessible to page JavaScript). `AuthMiddleware` (`internal/middleware/auth.go`) tags cookie-authenticated requests with `auth_method = jwt_cookie`, so the CSRF middleware can require a double-submit token on cookie-authenticated mutations. Programmatic clients send `Authorization: Bearer <token>` (a JWT or an API key) and are not CSRF-eligible.

Token resolution order (`extractToken` + `AuthMiddleware`):

```txt
Request arrives
  │
  ├─► mTLS already authenticated (auth_method=mtls)? → continue
  │
  ├─► Authorization: Bearer <token>?
  │     ├─► Validate as JWT (stateless, no DB)
  │     │     ├─► valid → check revocation (JTI) → load user → continue
  │     │     └─► invalid → if token starts "tsm_" → API-key path
  │     └─► API-key path: prefix lookup → bcrypt compare → expiry → owner exists → continue
  │
  └─► tsm_auth_token cookie? → validate as JWT only (cookies never carry API keys)
```

### Why JWT Is Tried First

JWT validation is stateless — an HMAC check against `TSM_JWT_SECRET` plus a revocation lookup by JTI. API-key validation always costs a database round-trip (indexed prefix lookup + bcrypt compare). JWT is attempted first as the lower-latency path; only a Bearer token that fails JWT validation and starts with the `tsm_` prefix falls through to the key path. API keys never arrive via cookie.

### API Key Design

API keys are bcrypt-hashed and never stored in plaintext (logic lives in the shared `terraform-suite-identity` module). The first characters form an indexed **prefix** for fast candidate lookup; the full key is bcrypt-compared. The raw key (a `tsm_` token) is shown once at creation. Keys can carry an expiry and are scoped — a creator may only grant scopes they themselves hold. `UpdateLastUsed` runs in a background goroutine so last-used tracking never adds latency to the request path.

### CSRF

`CSRFProtect` (`internal/middleware/csrf.go`) enforces a double-submit check: a non-HttpOnly `tsm_csrf` cookie is set alongside the session cookie, and cookie-authenticated mutating requests must echo it in the `X-CSRF-Token` header (constant-time compared). Requests without the session cookie — API-key/Bearer clients and per-run machine callbacks — are not forgeable cross-site and pass through.

---

## Role-Based Access Control (RBAC)

### Scope-Based

Authorization checks **scopes** (`<domain>:<action>` strings), not role names. The application defines eight scopes (`internal/auth/scopes.go`):

| Scope | Grants |
| --- | --- |
| `state:read` | Read state, analysis, history, reports, backups |
| `state:write` | Edit state (raw replace, `rm`/`mv`, restore) — implies `state:read` |
| `state:drift` | Create/ingest drift runs, acknowledge/resolve records |
| `state:execute` | Dispatch Version Lab (health) runs |
| `state:transfer` | Cross-source state backup/migrate |
| `sources:manage` | Create/update/delete sources, pipelines, CI sources, schedules |
| `scim:provision` | SCIM 2.0 provisioning (admin satisfies it) |
| `admin` | Wildcard — grants every scope |

The `admin` wildcard and the write-implies-read relationship are evaluated by the shared identity scope checker; the app only injects its own scope set and the single read/write pair (`state:read` ⇐ `state:write`). `RequireScope` enforces the gate at request time, reading scopes from the auth context.

### Role Templates

The app owns four role templates (`AppRoleTemplates`): `admin`, `editor`, `operator`, `viewer`. They are upserted into the shared identity schema at boot (`internal/bootstrap`). A user's effective scopes come from their role-template assignment in an organization, loaded at request time — a role change takes effect on the next request without reissuing tokens.

### Shared Identity Module

JWT issuance/validation (`TokenManager`), API-key generation/validation, scope evaluation, and the user/org/role/audit repositories are owned by the shared `github.com/sethbacon/terraform-suite-identity` module. The files in `internal/auth` are thin adapters that inject TSM's scope set and delegate. Under suite coupling, the identity schema can be physically shared with the sibling registry — see [ADR 001](adr/001-suite-coupling-shared-identity.md) and [ADR 004](adr/004-role-seed-ownership.md).

---

## Credential Encryption

Per-source secrets (HCP tokens, cloud credentials, CI tokens, notification targets) are encrypted at rest with **AES-256-GCM** (`internal/crypto/crypto.go`). The key is read from `TSM_ENCRYPTION_KEY` (or the unprefixed `ENCRYPTION_KEY`), accepted as either 32 raw bytes or 64 hex characters. Ciphertext is laid out as `nonce || GCM(seal)`, so it is self-describing on decrypt. The API never returns stored credentials. Losing the key orphans every stored credential, so it must be escrowed (see `docs/disaster-recovery.md`).

The signing secret `TSM_JWT_SECRET` is deliberately separate from `TSM_ENCRYPTION_KEY`: session tokens and long-lived credentials have different lifetimes and sensitivities, and rotating one must not invalidate the other.

---

## Background Workers

The application runs **one** category of periodic background work, gated so it runs on exactly one replica:

| Worker | Trigger | Purpose |
| --- | --- | --- |
| Schedule runner (`internal/services/scheduler`) | Polls every 60s | Fire due cron-style schedules; dispatch drift runs. Overdue schedules fire once then reschedule from now — no catch-up storm. |
| State-sync reconcile loop (`internal/services/statesync`) | Every 5 min (per-source 10 min timeout) | Reconcile the persistent state-analysis store against each source so dashboards/reports stay fast. |

Both periodic loops start only when `TSM_WORKERS_ENABLED=true`. The schedule runner has **no cross-replica claim** — `GetDue` is a plain query — so two enabled replicas would double-fire schedules. The deployment rule (see [ADR 001](adr/001-suite-coupling-shared-identity.md) context and `docs/deployment.md`) is therefore: run `TSM_WORKERS_ENABLED=true` on **exactly one** dedicated worker replica and `false` on all API replicas.

On-demand syncs are independent of this gate: the `statesync` *object* is always attached so post-write analysis refreshes and source-create backfills work on every replica. Goroutine panics are recovered, not fatal.

---

## Database Design

### Two Schemas, One or Two Databases

The backend uses two logical schemas:

- The **app schema** (`internal/db/migrations/`) holds sources, state analysis history, backups, locks, drift records/runs, pipelines, schedules, notification channels, and system settings.
- The **identity schema** (owned by `terraform-suite-identity`) holds users, organizations, role templates, tokens, OIDC config, and audit logs.

By default both schemas live in the same database (standalone). Setting `TSM_IDENTITY_DATABASE_*` points identity at a separate or shared database; the identity connection is opened with `search_path=identity,public` so the shared repositories resolve to the identity schema. Any unset identity field inherits from the primary database config (`resolveIdentityDatabase`).

### Migrations Run on Boot, Under Advisory Locks

Both schema migration sets run automatically on `serve` startup (`main.go`), each guarded by a database advisory lock so concurrent multi-replica rollouts are safe. They are also available imperatively via the `migrate up|down` subcommand. Never edit an applied migration file — the runner tracks applied versions and an edited file causes a dirty-state error.

### Notable Choices

- **State is not warehoused.** TSM stores per-state *analysis* (resource/provider/module counts, RUM, serial/lineage) appended only when observed content actually changes, plus pre-write backups — not a full copy of every state file on every sync.
- **App-level locks** live in `state_locks` with a `UNIQUE(source_id, state_key)` constraint and a reaped 15-minute TTL ([ADR 003](adr/003-advisory-lock-ttl.md)).

---

## Suite Coupling

The State Manager runs standalone by default. When `TSM_SUITE_SIBLING_URL` is set, it starts a background `suite.DiscoveryClient` that exchanges manifests with the sibling registry (`/api/v1/suite/manifest`) and exposes the live snapshot to the SPA via `/api/v1/ui/config`. Two server-to-server endpoints — `GET /api/v1/consumers` (powers the registry's "Consumed by" panel) and `POST /api/v1/audit/ingest` (audit federation) — are gated by a shared `X-Suite-Service-Token` and are off until an operator provisions a matching secret. Audit federation is advertised only when both apps assert a shared identity store. See [ADR 001](adr/001-suite-coupling-shared-identity.md).

---

## Security Model

Defense-in-depth, from outer to inner:

1. **TLS** (at ingress, or directly via `TSM_SERVER_TLS_*`) — encrypts all traffic.
2. **Security headers** — strict API CSP (`default-src 'none'`), HSTS on TLS, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, and cross-origin isolation headers on every response.
3. **Authentication** — JWT (HttpOnly cookie or header), API key, mTLS, or an SSO provider for all non-public endpoints.
4. **CSRF protection** — cookie-authenticated mutations require the `tsm_csrf` double-submit token.
5. **RBAC** — scope checking before any state mutation.
6. **Fail-closed state writes** — a write never proceeds when the current state cannot be verified ([ADR 002](adr/002-fail-closed-state-writes.md)).
7. **Locking** — native or app-level advisory locks guarantee single-writer mutual exclusion.
8. **Credential encryption** — source/CI/notification secrets sealed with AES-256-GCM; never returned by the API.
9. **Bcrypt for API keys and the setup token** — compromise of the database does not expose working keys.
10. **Audit logging** — mutating actions recorded with actor, resource, and metadata.
11. **Suite service token** — cross-app reads gated by a shared secret, constant-time compared, off by default.

See [threat-model.md](threat-model.md) for the full STRIDE analysis.

---

## Observability Architecture

Prometheus metrics are exposed on a dedicated **side-channel port** (`:9090`, `TSM_TELEMETRY_METRICS_PROMETHEUS_PORT`) started as a separate `http.Server` in `main.go` — off the public API ingress so the scrape path is never rate-limited and never publicly exposed. The `/metrics` endpoint is unauthenticated and must be firewalled to the monitoring network.

Health surfaces are split:

- `GET /health` — liveness; no database touch.
- `GET /ready` — readiness; pings the database and returns `503` when it is unreachable.
- `GET /api/v1/version` — build info (version, build date), unauthenticated.

Structured logs are written to stdout as JSON (`TSM_LOGGING_FORMAT`, configurable level). Each request carries a `request_id` set by `RequestID` (honoring an upstream `X-Request-ID` or minting a UUID) so log lines correlate across the request. A `DBStatsCollector` exposes connection-pool metrics, and an `AppInfo` gauge labels the running version for fleet inventory. For metric names, alert rules, and dashboards see [observability.md](observability.md) and `deployments/observability/`.

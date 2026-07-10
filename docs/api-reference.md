<!-- markdownlint-disable MD013 -->
# API Reference

The Terraform State Manager (TSM) backend serves a machine-readable OpenAPI
(Swagger 2.0) spec that is always synchronized with the running binary. This
document is the **narrative** companion to that spec: it maps the API surface,
explains the authentication model, and documents conventions the generated spec
does not spell out. For the exact request/response schema of any endpoint, use
the spec.

## Table of Contents

1. [The OpenAPI spec](#the-openapi-spec)
2. [Authentication](#authentication)
3. [Scopes & RBAC](#scopes--rbac)
4. [API groups overview](#api-groups-overview)
5. [System & observability endpoints](#system--observability-endpoints)
6. [Common patterns](#common-patterns)
7. [Regenerating the spec](#regenerating-the-spec)

---

## The OpenAPI spec

The backend embeds the spec at compile time and serves it unauthenticated:

```http
GET /swagger.json     # OpenAPI 2.0 (Swagger) JSON
GET /swagger.yaml     # the same spec as YAML
```

Both are read-only and unauthenticated. The frontend's API-docs page renders the
JSON; you can also import it into Postman or feed it to a client-SDK generator.
The spec reflects the **exact version of the running binary** — it is generated
from `// @`-annotation comments in the handler source and embedded via the `docs`
package.

> TSM does **not** bundle a Swagger UI route or an OpenAPI 3 conversion endpoint.
> The interactive docs view lives in the frontend SPA, backed by `/swagger.json`.

---

## Authentication

TSM accepts two credential types, plus two machine-only paths.

### Cookie session (browser)

Interactive users authenticate through a configured identity provider (OIDC,
SAML, or LDAP). The session is an HMAC-signed JWT delivered as an **HttpOnly,
CSRF-protected cookie** — never exposed to page JavaScript. There is **no
local-password login**.

```bash
# OIDC: GET redirects the browser to the IdP; the callback sets the cookie.
#   GET /api/v1/auth/login
# SAML: GET /api/v1/auth/login?provider=saml
# LDAP: POST credentials to obtain a session
curl -X POST https://tsm.example.com/api/v1/auth/ldap/login \
     -H "Content-Type: application/json" \
     -d '{"username": "user", "password": "..."}'
```

Cookie-authenticated, state-changing requests under `/api/v1` are protected by
**double-submit CSRF** — the SPA echoes the CSRF cookie in a header. Bearer-token
calls are exempt (they carry no cookie). See [sso.md](sso.md) for the full session
model.

### API key (Bearer)

API keys are `tsm_…` Bearer tokens — the recommended credential for CI and
automation. They are bcrypt-hashed, the secret is shown **once** at creation,
scopes are capped at the creator's own, and rotation supports up to a 72-hour
grace window. Manage them under `/api/v1/apikeys` (self-service; admins see all).

```bash
curl -H "Authorization: Bearer tsm_your_api_key" \
     https://tsm.example.com/api/v1/sources
```

### Machine-only paths

- **mTLS** — a verified client certificate authenticates additively, before JWT
  auth, when the backend terminates TLS. See [sso.md](sso.md).
- **One-shot run tokens** — CI result callbacks
  (`POST /api/v1/drift/runs/{id}/results`, `POST /api/v1/health-lab/runs/{id}/results`)
  authenticate with the per-run token issued when the run was dispatched, not a
  user session.
- **Suite service token** — cross-app server-to-server reads
  (`GET /api/v1/consumers`, `POST /api/v1/audit/ingest`) require the
  `X-Suite-Service-Token` header. See [suite-coupling.md](suite-coupling.md).

---

## Scopes & RBAC

Authorization is scope-based. A user or key holds scopes (via role templates);
each endpoint requires one. Holding `state:write` implicitly satisfies
`state:read`. `admin` is the wildcard — it satisfies every scope.

| Scope            | Grants                                                                         |
| ---------------- | ------------------------------------------------------------------------------ |
| `state:read`     | Read sources, states, analysis, history, reports, drift/health runs, dashboard |
| `state:write`    | Edit/restore state, state operations (implies `state:read`)                    |
| `state:transfer` | Cross-source backup (copy) and migrate (move)                                  |
| `state:drift`    | Create drift runs, push drift ingest, acknowledge/resolve records              |
| `state:execute`  | Create Version Lab (health) runs                                               |
| `sources:manage` | Create/update/delete sources, pipelines, CI sources, schedules                 |
| `scim:provision` | SCIM provisioning endpoints (when enabled)                                     |
| `admin`          | Everything, incl. identity admin, notifications, force-unlock                  |

The bundled role templates map to these scopes:

| Role       | Scopes                                                                                          |
| ---------- | ----------------------------------------------------------------------------------------------- |
| `admin`    | `admin` (all)                                                                                   |
| `editor`   | `state:read`, `state:write`, `state:transfer`, `state:drift`, `state:execute`, `sources:manage` |
| `operator` | `state:read`, `state:drift`, `state:execute`                                                    |
| `viewer`   | `state:read`                                                                                    |

When creating an API key, request the minimum scopes it needs — a key with only
`state:drift` cannot read users or manage sources, which limits blast radius if
the key leaks.

---

## API groups overview

The full endpoint list is in the spec; here is the conceptual map. All paths are
under `/api/v1` unless noted.

### Auth & session

| Path                                        | Auth    | Purpose                                        |
| ------------------------------------------- | ------- | ---------------------------------------------- |
| `/auth/providers`                           | none    | List configured providers for the login picker |
| `/auth/login`, `/auth/callback`             | none    | OIDC/SAML browser flows                        |
| `/auth/ldap/login`                          | none    | LDAP username/password login                   |
| `/auth/saml/metadata`, `/auth/saml/acs`     | none    | SAML SP metadata + assertion consumer          |
| `/auth/me`, `/auth/refresh`, `/auth/logout` | session | Current user, refresh, logout                  |

### First-run setup wizard

`/setup/status` is public; the mutating `/setup/*` endpoints sit behind the
setup-token middleware and are **permanently disabled once setup completes**.
Covers admin bootstrap, OIDC test/save, and source test/save.

### Sources, state & analysis

| Path                                                                                   | Scope                           | Purpose                                                     |
| -------------------------------------------------------------------------------------- | ------------------------------- | ----------------------------------------------------------- |
| `GET /sources` · `POST /sources`                                                       | `state:read` · `sources:manage` | List / create state sources                                 |
| `GET/PUT/DELETE /sources/{id}`                                                         | `state:read` · `sources:manage` | Source CRUD                                                 |
| `POST /sources/{id}/test`                                                              | `state:read`                    | Test connectivity                                           |
| `GET /sources/{id}/states`                                                             | `state:read`                    | List state files in the source                              |
| `GET /sources/{id}/state/analysis`                                                     | `state:read`                    | Per-state resource/provider/module breakdown                |
| `GET /sources/{id}/state/raw` · `…/resources` · `…/outputs` · `…/history` · `…/report` | `state:read`                    | Raw state, resources, outputs, history, downloadable report |
| `GET /sources/{id}/modules`                                                            | `state:read`                    | Captured module provenance                                  |
| `GET /sources/{id}/modules/freshness`                                                  | `state:read`                    | Locked module versions vs the sibling registry's latest     |
| `POST /analyze`                                                                        | `state:read`                    | Ad-hoc analysis of an uploaded state (no stored source)     |
| `GET /dashboard/overview`                                                              | `state:read`                    | Cross-source aggregated overview                            |

### State manipulation (edit plane)

| Path                                                  | Scope                   | Purpose                                                                                                                                                              |
| ----------------------------------------------------- | ----------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `PUT /sources/{id}/state/raw`                         | `state:write`           | Guided state edit (validate → backup → write → audit)                                                                                                                |
| `POST /sources/{id}/state/operations`                 | `state:write` · `admin` | State operations: `rm`/`mv` (`state:write`); `delete` removes the state object (`admin` only, lock → final backup → delete → audit; `purge=true` also drops backups) |
| `GET /sources/{id}/state/backups`                     | `state:read`            | List backups                                                                                                                                                         |
| `GET /sources/{id}/state/backups/{backupId}`          | `state:read`            | Backup content (full state JSON)                                                                                                                                     |
| `GET /sources/{id}/state/backups/{backupId}/diff`     | `state:read`            | Restore preview: resources a restore would add/remove/change vs the current state                                                                                    |
| `POST /sources/{id}/state/backups/{backupId}/restore` | `state:write`           | Restore a backup                                                                                                                                                     |
| `DELETE /sources/{id}/state/lock`                     | `admin`                 | Admin force-unlock                                                                                                                                                   |
| `POST /sources/{id}/state/backup` · `…/migrate`       | `state:transfer`        | Cross-source copy / move                                                                                                                                             |
| `GET /transfers/{id}`                                 | `state:read`            | Transfer status                                                                                                                                                      |

> Writes are **fail-closed**: a write is refused unless the target's existence
> can be positively verified.

### Drift detection

| Path                                                            | Scope                        | Purpose                                                                |
| --------------------------------------------------------------- | ---------------------------- | ---------------------------------------------------------------------- |
| `GET/POST/DELETE /pipelines`                                    | `sources:manage`             | CI pipeline connections (+ callback preflight)                         |
| `GET/POST/DELETE /ci-sources/...`                               | `sources:manage`             | Org-level CI credentials + repo/workflow discovery + repo-setup wizard |
| `POST /drift/runs` · `GET /drift/runs` · `GET /drift/runs/{id}` | `state:drift` · `state:read` | Dispatch and read drift runs                                           |
| `POST /drift/ingest`                                            | `state:drift`                | Idempotent push-style CI drift intake (parses Terraform plan JSON)     |
| `GET /drift/records` · `…/{id}`                                 | `state:read`                 | Durable, acknowledgeable drift records                                 |
| `POST /drift/records/{id}/acknowledge` · `…/resolve`            | `state:drift`                | Record workflow                                                        |
| `POST /drift/runs/{id}/results`                                 | one-shot run token           | CI machine callback                                                    |

### Version Lab (health)

| Path                                                       | Scope                          | Purpose                               |
| ---------------------------------------------------------- | ------------------------------ | ------------------------------------- |
| `GET /health-lab/workflow`                                 | `state:read`                   | Workflow template                     |
| `POST /health-lab/runs` · `GET …/runs` · `GET …/runs/{id}` | `state:execute` · `state:read` | Dispatch and read version-health runs |
| `POST /health-lab/runs/{id}/results`                       | one-shot run token             | CI machine callback                   |

### Scheduling & notifications

| Path                                                          | Scope                           | Purpose                                                     |
| ------------------------------------------------------------- | ------------------------------- | ----------------------------------------------------------- |
| `GET/POST/PUT/DELETE /schedules` · `POST /schedules/{id}/run` | `state:read` / `sources:manage` | Cron-style schedules that dispatch drift runs               |
| `/notifications/channels/...`                                 | `admin`                         | Alert destinations (target URLs are secrets, so admin-only) |

### Identity administration

| Path                                                                                                             | Scope   | Purpose                                                          |
| ---------------------------------------------------------------------------------------------------------------- | ------- | ---------------------------------------------------------------- |
| `/admin/users`, `/admin/organizations`                                                                           | `admin` | User & org CRUD, memberships, GDPR export/erase                  |
| `/admin/roles`, `/admin/audit-logs`, `/admin/stats`                                                              | `admin` | Role templates, audit trail, stats                               |
| `/admin/sso`, `/admin/oidc/config`, `/admin/oidc/group-mapping`, `/admin/identity-group-mappings`, `/admin/mtls` | `admin` | Read configured providers; manage the OIDC group-mapping overlay |

### API keys

`/apikeys` (list/create/get/update/delete/rotate) — any authenticated user
manages their own; admins see all. Ownership and scope-grant limits are enforced
in the handlers (no separate scope gate).

### Suite (cross-app)

| Path                  | Auth                    | Purpose                                            |
| --------------------- | ----------------------- | -------------------------------------------------- |
| `GET /suite/manifest` | none                    | Capability manifest for sibling discovery          |
| `GET /ui/config`      | none (SPA)              | Live coupling state for the frontend               |
| `GET /consumers`      | `X-Suite-Service-Token` | States consuming a registry module ("Consumed by") |
| `POST /audit/ingest`  | `X-Suite-Service-Token` | Receive a sibling's federated audit entry          |

See [suite-coupling.md](suite-coupling.md) for the full coupling model.

### SCIM 2.0 (when enabled)

Mounted at the top-level `/scim/v2` (outside `/api/v1`), only when
`TSM_AUTH_SCIM_ENABLED` is set. Bearer-token auth + `scim:provision` scope; no
cookie auth. `Users` (CRUD) and `Groups` (read).

---

## System & observability endpoints

These are unversioned and **not** part of `/api/v1`. The metrics endpoint is
served on a **separate port** and is not in the OpenAPI spec — bind it to an
internal interface only.

| Endpoint              | Port   | Auth | Purpose                                          |
| --------------------- | ------ | ---- | ------------------------------------------------ |
| `GET /health`         | API    | none | Liveness — does **not** touch the database       |
| `GET /ready`          | API    | none | Readiness — pings the DB; `503` when unreachable |
| `GET /api/v1/version` | API    | none | Build metadata (`name`, `version`, `build_date`) |
| `GET /metrics`        | `9090` | none | Prometheus exposition format                     |

```bash
curl -s http://localhost:9090/metrics | grep '^http_requests_total'
```

The metrics port is `TSM_TELEMETRY_METRICS_PROMETHEUS_PORT` (default **9090**),
enabled by `TSM_TELEMETRY_METRICS_ENABLED` (default true). It is **unauthenticated
— never expose it publicly**; restrict it at the network level. See
[observability.md](observability.md) for the metric catalogue and alerts.

---

## Common patterns

### Error responses

Handler errors return JSON with an `error` field:

```json
{ "error": "source not found" }
```

HTTP status carries the category: `400` bad request, `401` unauthenticated,
`403` insufficient scope, `404` not found, `422` unprocessable (e.g. an
unparseable state file), `503` a dependency is unavailable.

### Empty is not an error

Several read surfaces return an empty collection rather than `404` when nothing
has been captured yet — e.g. captured module provenance (`/sources/{id}/modules`)
is empty until a full plan is ingested, and module freshness returns `200` with
`no_registry` verdicts when running standalone. Treat these as normal.

### CI callbacks are token-authenticated

The drift and Version Lab result callbacks are authenticated by the **per-run
token** embedded in the dispatched workflow, not a user session or API key. The
callback URL must be reachable from your CI runner — see
`TSM_SERVER_CALLBACK_URL` in [configuration.md](configuration.md).

---

## Regenerating the spec

The spec is generated from `// @` annotations on Go handler source and embedded
in the binary at compile time. After adding or changing annotations:

```bash
cd backend

# Regenerate docs/swagger.json (and swagger.yaml)
make swag

# Rebuild to embed the updated spec
go build ./cmd/server
```

CI checks that the committed spec is in sync, so commit the regenerated
`docs/swagger.json` alongside handler changes.

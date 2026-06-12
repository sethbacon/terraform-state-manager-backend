# Configuration reference

Configuration is layered (lowest to highest precedence): built-in defaults →
optional YAML file (path via `CONFIG_PATH`; see `backend/config.example.yaml`)
→ environment variables prefixed `TSM_` (dots become underscores:
`database.host` → `TSM_DATABASE_HOST`). Environment variables always win.

**Secret** values must come from a secret store (Key Vault / Secrets Manager /
Secret Manager / Kubernetes Secrets) — never config files or Helm values.

## Server

| Variable | Default | Required | Secret | Description |
|---|---|---|---|---|
| `TSM_SERVER_HOST` | `0.0.0.0` | | | Bind address |
| `TSM_SERVER_PORT` | `8080` | | | API port |
| `TSM_SERVER_BASE_URL` | `http://localhost:8080` | prod | | Backend base URL (internal references; callback fallback) |
| `TSM_SERVER_PUBLIC_URL` | `http://localhost:3000` | prod | | Browser-facing URL — drives post-login redirects and the `Secure` cookie flag (HTTPS ⇒ Secure) |
| `TSM_SERVER_CALLBACK_URL` | (empty ⇒ BASE_URL) | | | URL CI runners POST drift/version-lab results to — **must be reachable from your CI runners** |
| `TSM_SERVER_READ_TIMEOUT` | `30s` | | | HTTP read timeout |
| `TSM_SERVER_WRITE_TIMEOUT` | `30s` | | | HTTP write timeout |
| `TSM_SERVER_TLS_CERT_FILE` | (empty) | | | Serve HTTPS directly (required for mTLS); empty = terminate TLS upstream |
| `TSM_SERVER_TLS_KEY_FILE` | (empty) | | | Pairs with CERT_FILE |

## Database

| Variable | Default | Required | Secret | Description |
|---|---|---|---|---|
| `TSM_DATABASE_HOST` | `localhost` | yes | | PostgreSQL host |
| `TSM_DATABASE_PORT` | `5432` | | | |
| `TSM_DATABASE_NAME` | `terraform_state_manager` | | | |
| `TSM_DATABASE_USER` | `tsm` | | | |
| `TSM_DATABASE_PASSWORD` | (empty) | yes | **yes** | |
| `TSM_DATABASE_SSL_MODE` | `prefer` | | | `disable` \| `prefer` \| `require` \| `verify-full` (use `require`+ in production) |
| `TSM_DATABASE_MAX_CONNECTIONS` | `25` | | | Pool cap **per replica** — size your server for replicas × this |
| `TSM_DATABASE_MIN_IDLE_CONNECTIONS` | `5` | | | |

PostgreSQL 14+ (16 recommended). Migrations (app schema + shared identity
schema) run on every boot under advisory locks — multi-replica rollouts are
safe — and are also available imperatively: `terraform-state-manager migrate up|down`.

## Core secrets

| Variable | Default | Required | Secret | Description |
|---|---|---|---|---|
| `TSM_JWT_SECRET` | (none) | **yes (prod)** | **yes** | HMAC key for session JWTs, min 32 chars (`openssl rand -hex 32`). Boot fails without it unless `DEV_MODE=true` |
| `TSM_ENCRYPTION_KEY` | (none) | yes* | **yes** | AES-256-GCM key for credentials at rest: exactly **32 raw bytes or 64 hex chars** (`openssl rand -hex 32`). *Required to store source/CI credentials. **Losing it orphans them — escrow it** ([disaster-recovery.md](disaster-recovery.md)) |
| `ENCRYPTION_KEY` | (none) | | **yes** | Fallback name for `TSM_ENCRYPTION_KEY` |
| `DEV_MODE` | `false` | | | `true` enables an ephemeral JWT secret and `POST /api/v1/dev/login`. **Never in production** |

## Workers

| Variable | Default | Required | Secret | Description |
|---|---|---|---|---|
| `TSM_WORKERS_ENABLED` | `true` | | | Gates the periodic schedule runner + state-sync loop. Multi-replica: `false` on API replicas, `true` on exactly ONE worker replica ([why](deployment/README.md#worker-topology)) |

## Authentication — OIDC

| Variable | Default | Description |
|---|---|---|
| `TSM_AUTH_OIDC_ENABLED` | `false` | Enable OIDC (Entra ID, Keycloak, Okta, …) |
| `TSM_AUTH_OIDC_ISSUER_URL` | | e.g. `https://login.microsoftonline.com/<tenant>/v2.0` |
| `TSM_AUTH_OIDC_CLIENT_ID` | | |
| `TSM_AUTH_OIDC_CLIENT_SECRET` | | **secret** |
| `TSM_AUTH_OIDC_REDIRECT_URL` | | `https://<host>/api/v1/auth/callback` |
| `TSM_AUTH_OIDC_SCOPES` | `openid,email,profile` | |
| `TSM_AUTH_OIDC_GROUP_CLAIM_NAME` | `groups` | IdP claim carrying group memberships |
| `TSM_AUTH_OIDC_DEFAULT_ROLE` | (empty) | Role granted on FIRST login when no group mapping matches (admin/editor/operator/viewer) |

Group→role mappings are managed in the UI (Administration → OIDC groups) and
reconcile on every login — users removed from mapped groups are deprovisioned
from the mapped orgs.

## Authentication — LDAP / Active Directory

| Variable | Default | Description |
|---|---|---|
| `TSM_AUTH_LDAP_ENABLED` | `false` | Search-bind authentication |
| `TSM_AUTH_LDAP_HOST` | | |
| `TSM_AUTH_LDAP_PORT` | `0` | 0 auto-selects 636 (LDAPS) / 389 (StartTLS) |
| `TSM_AUTH_LDAP_USE_TLS` | `false` | LDAPS from connect |
| `TSM_AUTH_LDAP_START_TLS` | `false` | Upgrade plain LDAP |
| `TSM_AUTH_LDAP_INSECURE_SKIP_VERIFY` | `false` | Dev only |
| `TSM_AUTH_LDAP_BASE_DN` | | Search base |
| `TSM_AUTH_LDAP_BIND_DN` | | Service account DN |
| `TSM_AUTH_LDAP_BIND_PASSWORD` | | **secret** |
| `TSM_AUTH_LDAP_USER_FILTER` | | Must contain `%s` (escaped username), e.g. `(sAMAccountName=%s)` |
| `TSM_AUTH_LDAP_USER_ATTR_EMAIL` | `mail` | |
| `TSM_AUTH_LDAP_USER_ATTR_NAME` | `displayName` | |
| `TSM_AUTH_LDAP_GROUP_BASE_DN` | (empty) | Enables group resolution |
| `TSM_AUTH_LDAP_GROUP_FILTER` | (empty) | Optional; `%s` = escaped user DN |
| `TSM_AUTH_LDAP_GROUP_MEMBER_ATTR` | `member` | |
| `TSM_AUTH_LDAP_DEFAULT_ROLE` | (empty) | As OIDC |

## Authentication — SAML 2.0

| Variable | Default | Description |
|---|---|---|
| `TSM_AUTH_SAML_ENABLED` | `false` | |
| `TSM_AUTH_SAML_ENTITY_ID` | (derived) | Defaults to ACS URL minus `/saml/acs` |
| `TSM_AUTH_SAML_ACS_URL` | | Public ACS URL, e.g. `https://<host>/api/v1/auth/saml/acs` |
| `TSM_AUTH_SAML_CERT_FILE` / `KEY_FILE` | (empty) | SP signing pair (file paths — mount as files) |
| `TSM_AUTH_SAML_ALLOW_IDP_INITIATED` | `false` | Keep `false` (replay surface) |
| `TSM_AUTH_SAML_GROUP_ATTRIBUTE_NAME` | (empty) | Assertion attribute carrying groups |
| `TSM_AUTH_SAML_DEFAULT_ROLE` | (empty) | As OIDC |

## Authentication — mTLS and SCIM

| Variable | Default | Description |
|---|---|---|
| `TSM_AUTH_MTLS_ENABLED` | `false` | Client-cert auth; requires the backend to terminate TLS itself (`TSM_SERVER_TLS_*`) |
| `TSM_AUTH_MTLS_CLIENT_CA_FILE` | | PEM bundle of trusted client CAs |
| `TSM_AUTH_SCIM_ENABLED` | `false` | Mounts `/scim/v2` (RFC 7644). Bearer token with `scim:provision` scope; rate-limit at the proxy |

API keys (`tsm_…` Bearer tokens, self-service under `/admin/apikeys`) are always
enabled and are the recommended credential for CI calls such as
`POST /api/v1/drift/ingest`.

## Logging & telemetry

| Variable | Default | Description |
|---|---|---|
| `TSM_LOGGING_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` — note `warn` suppresses boot lines |
| `TSM_LOGGING_FORMAT` | `json` | `json` (production) \| `text` (dev) |
| `TSM_TELEMETRY_METRICS_ENABLED` | `true` | Prometheus exporter |
| `TSM_TELEMETRY_METRICS_PROMETHEUS_PORT` | `9090` | `/metrics` listens here, **unauthenticated** — never expose publicly |

## Endpoints a deployment should know

| Endpoint | Auth | Use |
|---|---|---|
| `GET /health` | none | Liveness (no DB touch) |
| `GET /ready` | none | Readiness (DB ping; 503 when unreachable) |
| `GET /metrics` (`:9090`) | none | Prometheus |
| `GET /api/v1/version` | none | Build info |
| `POST /api/v1/drift/runs/{id}/results` | one-shot run token | CI drift callback |
| `POST /api/v1/drift/ingest` | Bearer (`state:drift`) | Push-style drift intake |
| `/scim/v2/*` | Bearer (`scim:provision`) | IdP provisioning (when enabled) |

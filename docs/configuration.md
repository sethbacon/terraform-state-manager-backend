# Configuration reference

Configuration is layered (lowest to highest precedence): built-in defaults →
optional YAML file (path via `CONFIG_PATH`; see `backend/config.example.yaml`)
→ environment variables prefixed `TSM_` (dots become underscores:
`database.host` → `TSM_DATABASE_HOST`). Environment variables always win.

**Secret** values must come from a secret store (Key Vault / Secrets Manager /
Secret Manager / Kubernetes Secrets) — never config files or Helm values.

**Sections:** [Server](#server) · [Database](#database) ·
[Identity database](#identity-database-shared--standalone) ·
[Core secrets](#core-secrets) · [Workers](#workers) ·
[OIDC](#authentication--oidc) · [LDAP](#authentication--ldap--active-directory) ·
[SAML](#authentication--saml-20) · [mTLS & SCIM](#authentication--mtls-and-scim) ·
[Logging & telemetry](#logging--telemetry) · [Suite coupling](#suite-coupling) ·
[Endpoints](#endpoints-a-deployment-should-know)

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
| `TSM_SERVER_TRUSTED_PROXIES` | (empty) | | | Comma-separated CIDRs/IPs of reverse proxies allowed to set `X-Forwarded-For`. Empty = trust no proxy (client IP in audit logs is the connecting peer). Set to your ingress/load-balancer CIDR to honour `X-Forwarded-For` |

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

## Identity database (shared / standalone)

The identity schema (users, organizations, roles, tokens) can optionally point
at a **separate or shared** database, independent of the app database above.
Any unset field falls back to the corresponding `TSM_DATABASE_*` value, so
leaving the whole section unset = use the app database (the standalone default).
Set `TSM_IDENTITY_DATABASE_*` to share one identity store across the suite
(e.g. one identity DB used by both this app and the sibling registry).

| Variable | Default | Required | Secret | Description |
|---|---|---|---|---|
| `TSM_IDENTITY_DATABASE_HOST` | (⇒ `TSM_DATABASE_HOST`) | | | Identity DB host |
| `TSM_IDENTITY_DATABASE_PORT` | (⇒ `TSM_DATABASE_PORT`) | | | |
| `TSM_IDENTITY_DATABASE_NAME` | (⇒ `TSM_DATABASE_NAME`) | | | |
| `TSM_IDENTITY_DATABASE_USER` | (⇒ `TSM_DATABASE_USER`) | | | |
| `TSM_IDENTITY_DATABASE_PASSWORD` | (⇒ `TSM_DATABASE_PASSWORD`) | | **yes** | |
| `TSM_IDENTITY_DATABASE_SSL_MODE` | (⇒ `TSM_DATABASE_SSL_MODE`) | | | As `TSM_DATABASE_SSL_MODE` |
| `TSM_IDENTITY_DATABASE_MAX_CONNECTIONS` | (⇒ `TSM_DATABASE_MAX_CONNECTIONS`) | | | Pool cap **per replica** |
| `TSM_IDENTITY_DATABASE_MIN_IDLE_CONNECTIONS` | (⇒ `TSM_DATABASE_MIN_IDLE_CONNECTIONS`) | | | |

When two suite apps share **one** identity database, exactly one must own the
role-template seed — see [`TSM_SUITE_ROLE_SEED_OWNER`](#suite-coupling) below or
they overwrite each other's role scopes on every restart.

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
| `TSM_WORKERS_ENABLED` | `true` | | | Gates the periodic schedule runner + state-sync loop + drift-run & health-run reconcilers. Multi-replica: `false` on API replicas, `true` on exactly ONE worker replica ([why](deployment/README.md#worker-topology)) |
| `TSM_DRIFT_RUN_TTL` | `2h` | | | How long a drift run may sit in `dispatched` before the reconciler fails it (the CI job never posted a result callback). Anchored on dispatch time; since the job's only callback is its final step (no heartbeat) this bounds **total CI wall-clock**, not idle time — raise it above your worst-case plan duration for large states so slow plans aren't expired mid-flight. The version-lab health-run reconciler shares this TTL (same dispatch model) |
| `TSM_DRIFT_RECONCILE_INTERVAL` | `5m` | | | How often the reconcilers sweep for expired dispatched drift runs and version-lab health runs |

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

OIDC group→role mappings (`auth.oidc.group_mappings`) are managed in the UI
(Administration → OIDC groups) and reconcile on every login — users removed from
mapped groups are deprovisioned from the mapped orgs. They may also be declared
in the YAML file (see [`config.example.yaml`](../backend/config.example.yaml)).
OIDC is the only provider whose group mappings are UI-editable; the LDAP/SAML
mappings below are **file config only**.

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

LDAP group→role mappings (`auth.ldap.group_mappings`) are a YAML list (group DN →
organization + role) and are **file config only** — there is no `TSM_` scalar for
them. See [`config.example.yaml`](../backend/config.example.yaml).

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

The IdP list (`auth.saml.idps`, one or more IdPs by metadata URL or inline XML)
and the SAML group→role mappings (`auth.saml.group_mappings`) are YAML lists and
are **file config only** — there is no `TSM_` scalar for them. At least one IdP
must be declared. See [`config.example.yaml`](../backend/config.example.yaml).

## Authentication — mTLS and SCIM

| Variable | Default | Description |
|---|---|---|
| `TSM_AUTH_MTLS_ENABLED` | `false` | Client-cert auth; requires the backend to terminate TLS itself (`TSM_SERVER_TLS_*`) |
| `TSM_AUTH_MTLS_CLIENT_CA_FILE` | | PEM bundle of trusted client CAs |
| `TSM_AUTH_SCIM_ENABLED` | `false` | Mounts `/scim/v2` (RFC 7644). Bearer token with `scim:provision` scope; rate-limit at the proxy |

A verified client cert is granted scopes via `auth.mtls.mappings` (**file config
only** — see [`config.example.yaml`](../backend/config.example.yaml)), which maps
a subject to a list of scopes. The subject may be `CN=<name>`, `dns:<san>`, or a
full DN. With no matching mapping the cert authenticates but carries **no
scopes** (so it can reach nothing) — define a mapping for every client cert.

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

## Suite coupling

Optional runtime coupling to the sibling suite app (the Terraform registry).
Leaving `TSM_SUITE_SIBLING_URL` empty (the default) runs this app **standalone**.
When set, the app discovers the sibling's manifest and lights up cross-app
features (module freshness, the "Consumed by" join, audit federation).

| Variable | Default | Required | Secret | Description |
|---|---|---|---|---|
| `TSM_SUITE_SIBLING_URL` | (empty) | | | Base URL of the sibling registry. Empty = standalone (no discovery, cross-app features inert) |
| `TSM_SUITE_POLL_INTERVAL` | `60s` | | | How often the sibling manifest is re-fetched |
| `TSM_SUITE_ROLE_SEED_OWNER` | `self` | | | Which app seeds the shared identity schema's system role templates: `self` \| `registry` \| `tsm`. **See the hazard below** |
| `TSM_SUITE_IDENTITY_SHARED_STORE` | `false` | | | Operator assertion that this app uses the shared identity store + single IdP. Advertised in the manifest; the SPA only drops the "you may need to sign in" hint when **both** apps assert it |
| `TSM_SUITE_SERVICE_TOKEN` | (empty) | | **yes** | Shared secret the sibling presents (`X-Suite-Service-Token`) for server-to-server reads (`GET /consumers`). Empty = that endpoint stays disabled. Set to the **same** value as the sibling registry's `TFR_SUITE_SIBLING_TOKEN` to enable the "Consumed by" join |

> **Role-seed hazard.** `TSM_SUITE_ROLE_SEED_OWNER=self` (default) means every
> app seeds its own identity store — correct for standalone, and when each app
> has its own identity database. But when **two apps share one identity
> database**, exactly one must own the seed (`registry` or `tsm`); otherwise they
> overwrite each other's role scopes on **every restart**. Set this together with
> `TSM_IDENTITY_DATABASE_*` whenever you share an identity store.

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
| `GET /api/v1/suite/manifest` | none | Suite capability manifest the sibling app discovers (advertises shared-store + cross-app features) |
| `GET /api/v1/consumers` | `X-Suite-Service-Token` (`TSM_SUITE_SERVICE_TOKEN`) | States consuming a given registry module; the sibling registry proxies this to power its "Consumed by" panel |
| `POST /api/v1/audit/ingest` | `X-Suite-Service-Token` (`TSM_SUITE_SERVICE_TOKEN`) | Receiving side of audit federation — a sibling app federates its audit entries here (shared-store only) |

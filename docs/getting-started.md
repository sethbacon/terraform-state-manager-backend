<!-- markdownlint-disable MD013 -->
# Getting Started

This tutorial walks you through running the Terraform State Manager (TSM) backend, completing first-run setup, connecting your first state source, and exercising the read and drift flows from the API. By the end you will have a running backend, an admin owner, a connected state source you can browse, and an API key wired up for CI drift ingestion.

> This guide drives the **backend API** directly with `curl`. The companion React UI (and a batteries-included local dev stack with Keycloak and seeded states) lives in [terraform-state-manager-frontend](https://github.com/sethbacon/terraform-state-manager-frontend). For a production install, follow [deployment.md](deployment.md) and [initial-setup.md](initial-setup.md) instead.

## Table of Contents

1. [Part 1: Run the Backend](#part-1-run-the-backend)
2. [Part 2: First-Run Setup](#part-2-first-run-setup)
3. [Part 3: Connect a State Source](#part-3-connect-a-state-source)
4. [Part 4: Browse State and Analysis](#part-4-browse-state-and-analysis)
5. [Part 5: Create an API Key](#part-5-create-an-api-key)
6. [Part 6: Ingest Drift from CI](#part-6-ingest-drift-from-ci)

---

## Part 1: Run the Backend

### Prerequisites

- Go 1.26+ (or the published Docker image)
- A reachable PostgreSQL 14+ (16 recommended)
- `curl` and `jq` for API interaction

### Generate the Two Secrets

The backend needs a JWT signing secret and a credential-encryption key. Both are read **only** from the environment:

```bash
export TSM_JWT_SECRET=$(openssl rand -hex 32)       # signs session JWTs (>= 32 chars)
export TSM_ENCRYPTION_KEY=$(openssl rand -hex 32)    # AES-256-GCM key (32 raw bytes / 64 hex chars)
```

`TSM_ENCRYPTION_KEY` encrypts every stored credential (state-source secrets, CI tokens, notification targets). Losing it orphans them — escrow it (see [disaster-recovery.md](disaster-recovery.md)).

### Point It at PostgreSQL and Start

The repository ships `config.example.yaml`; override any value with a `TSM_`-prefixed environment variable. From the repo root:

```bash
export TSM_DATABASE_HOST=localhost
export TSM_DATABASE_USER=tsm
export TSM_DATABASE_PASSWORD=<your-db-password>
export TSM_DATABASE_NAME=terraform_state_manager

make run   # CONFIG_PATH=config.example.yaml go run ./cmd/server serve
```

On `serve`, the backend runs its app-schema and identity-schema migrations (both under advisory locks, so multi-replica rollouts are safe), then starts. Verify liveness and readiness:

```bash
curl -s http://localhost:8080/health | jq .   # liveness; no DB touch
curl -s http://localhost:8080/ready  | jq .   # readiness; pings the DB (503 if unreachable)
```

`GET /api/v1/version` returns the build info.

### Get the Setup Token

On first boot — when setup is not yet complete — the backend prints a one-time setup token to stdout:

```text
==================================================================
  INITIAL SETUP REQUIRED
  Setup Token: tsm_setup_AbCdEfGh...
  Complete setup in the browser at /setup, or:
    POST /api/v1/setup/validate-token   (Authorization: SetupToken <token>)
  Single-use; invalidated after setup. Treat it like a root password.
==================================================================
```

Save this token — you need it in Part 2. (In containers, set `SETUP_TOKEN_FILE` to mirror the token to a mounted file, or supply your own via `SETUP_TOKEN`.)

```bash
export SETUP_TOKEN="tsm_setup_<your-token-here>"
```

---

## Part 2: First-Run Setup

Setup endpoints use a dedicated auth scheme — `Authorization: SetupToken <token>` — and are **permanently disabled** once setup completes. The browser wizard at `/setup` does the same calls; here we use the API.

### Step 1: Validate the Token

```bash
curl -s -X POST http://localhost:8080/api/v1/setup/validate-token \
  -H "Authorization: SetupToken ${SETUP_TOKEN}" | jq .
```

### Step 2: Create the Admin Owner

```bash
curl -s -X POST http://localhost:8080/api/v1/setup/admin \
  -H "Authorization: SetupToken ${SETUP_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{ "email": "admin@example.com" }' | jq .
```

This creates an **email-only** owner in the identity store and grants it the `admin` role in the `default` organization. There is no local password — the admin signs in through your configured IdP (OIDC/SAML/LDAP), which links the email to the IdP subject on first login.

> In coupled mode (a shared identity store owned by the sibling registry), this step returns `409` and is hidden in the wizard — create the owner in the registry instead. See [ADR 001](adr/001-suite-coupling-shared-identity.md).

### Step 3: Complete Setup

```bash
curl -s -X POST http://localhost:8080/api/v1/setup/complete \
  -H "Authorization: SetupToken ${SETUP_TOKEN}" | jq .
```

This burns the setup token and permanently disables the wizard endpoints.

### Get an Admin Session

In production, log in through your IdP to obtain the HttpOnly `tsm_auth_token` session cookie. For local exploration, start the backend with `DEV_MODE=true` and use the dev-login endpoint, which sets the session cookies (and the `tsm_csrf` double-submit cookie) for a seeded admin user. Capture them in a cookie jar and reuse it on subsequent calls:

```bash
# (restart the backend with DEV_MODE=true to enable this endpoint)
curl -s -c cookies.txt -X POST http://localhost:8080/api/v1/dev/login | jq .

# read the CSRF token from the jar; echo it on mutating (POST/PUT/DELETE) requests
CSRF=$(awk '/tsm_csrf/{print $7}' cookies.txt)
```

Because this is a **cookie** session, mutating requests must send both the cookie jar and the `X-CSRF-Token` header — for example:

```bash
curl -s -b cookies.txt -H "X-CSRF-Token: ${CSRF}" -X POST ... 
```

Read-only (`GET`) requests need only `-b cookies.txt`. `DEV_MODE` also lets the backend run with an ephemeral JWT secret. **Never enable it in production.**

> The remaining examples use `-H "Authorization: Bearer ${TOKEN}"` for brevity. With a cookie session, substitute `-b cookies.txt` (plus the `X-CSRF-Token` header on mutations); with the API key from [Part 5](#part-5-create-an-api-key), use that `tsm_` token as the Bearer value (no CSRF needed).

---

## Part 3: Connect a State Source

A source is a connection to a backend where Terraform state already lives. The backend supports ten types: `hcp`, `s3`, `azureblob`, `gcs`, `consul`, `pg`, `kubernetes`, `http`, `git`, and `local`. Credentials are AES-256-GCM encrypted before storage and are never returned by the API.

The example below registers a `local` source (no credentials required) pointing at a directory of state files. Creating a source requires the `sources:manage` scope.

> `local` sources are confined to the directories the operator listed in `TSM_STATESOURCE_LOCAL_ROOTS` (comma-separated absolute paths), and that list is empty by default — so start the backend with e.g. `TSM_STATESOURCE_LOCAL_ROOTS=/path/to/your/state/dir` or the call below is rejected. See [configuration.md](configuration.md#state-sources).

```bash
curl -s -X POST http://localhost:8080/api/v1/sources \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "local-dev",
    "type": "local",
    "config": { "base_path": "/path/to/your/state/dir" }
  }' | jq .

export SOURCE_ID="<id-from-response>"
```

Verify connectivity before relying on it:

```bash
curl -s -X POST "http://localhost:8080/api/v1/sources/${SOURCE_ID}/test" \
  -H "Authorization: Bearer ${TOKEN}" | jq .
```

> For a cloud source, supply its `config` and a `credentials` object instead — for example an `s3` source with a bucket/region in `config` and access keys in `credentials`. The credential fields are encrypted on save and omitted from every response.

---

## Part 4: Browse State and Analysis

List the state files the source exposes:

```bash
curl -s "http://localhost:8080/api/v1/sources/${SOURCE_ID}/states" \
  -H "Authorization: Bearer ${TOKEN}" | jq .
```

Pick a state key and analyze it — resource/provider/module breakdown, Terraform version, serial/lineage, and RUM (Resources Under Management):

```bash
curl -s "http://localhost:8080/api/v1/sources/${SOURCE_ID}/state/analysis?key=<state-key>" \
  -H "Authorization: Bearer ${TOKEN}" | jq .
```

Other read endpoints (all `state:read`-scoped) on the same source:

| Endpoint | Returns |
| --- | --- |
| `GET /sources/:id/state/resources?key=` | Per-resource listing |
| `GET /sources/:id/state/outputs?key=` | State outputs |
| `GET /sources/:id/state/history?key=` | Append-only analysis history (recorded when content changes) |
| `GET /sources/:id/state/report?key=` | Markdown report |
| `GET /sources/:id/modules?key=` | Modules referenced by the state |
| `GET /dashboard/overview` | Cross-source aggregate (RUM, providers, resource types, versions) |

The background **state-sync** worker keeps the analysis store reconciled (every 5 minutes), so the dashboard stays fast across thousands of states — but you can browse a source the moment it is created.

> **Editing state** (`PUT /sources/:id/state/raw`, `POST /sources/:id/state/operations`) goes through the guarded **validate → backup → write → audit** pipeline and requires `state:write`. Every edit backs up the prior state first, so it is one-click reversible. See [architecture.md](architecture.md#state-edit-pipeline), [ADR 002](adr/002-fail-closed-state-writes.md), and [ADR 003](adr/003-advisory-lock-ttl.md).

---

## Part 5: Create an API Key

API keys are the recommended credential for automation (CI drift ingestion, scripts). Any authenticated user manages their own keys; the raw key is shown **once**. A key can only carry scopes the creator already holds.

```bash
curl -s -X POST http://localhost:8080/api/v1/apikeys \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "ci-drift",
    "scopes": ["state:read", "state:drift"]
  }' | jq .

export API_KEY="tsm_<your-key-here>"
```

The key is a `tsm_` Bearer token. Programmatic clients send it as `Authorization: Bearer tsm_…` and are not subject to CSRF. Keys can be listed, updated, rotated (`POST /apikeys/:id/rotate`, with a grace window), and deleted.

---

## Part 6: Ingest Drift from CI

TSM detects drift two ways: it can **dispatch** runs through your CI, or your CI can **push** plan results to the idempotent ingest endpoint. The push path needs only an API key with `state:drift` and a stable per-run `external_ref` for deduplication.

In your pipeline, after `terraform plan -out plan.bin && terraform show -json plan.bin > plan.json`:

```bash
curl -s -X POST http://localhost:8080/api/v1/drift/ingest \
  -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  -d @- <<EOF
{
  "source_id": "${SOURCE_ID}",
  "state_key": "<state-key>",
  "external_ref": "ci-run-12345",
  "plan": $(cat plan.json)
}
EOF
```

Supply the raw `terraform show -json` plan in the `plan` field (parsed server-side, capped at 5 MiB), or send pre-computed `added`/`changed`/`destroyed`/`summary` counts instead. Re-ingesting the same `external_ref` collapses into the existing live drift record rather than creating duplicates; a clean plan auto-resolves it. Browse and acknowledge records:

```bash
curl -s "http://localhost:8080/api/v1/drift/records" \
  -H "Authorization: Bearer ${API_KEY}" | jq .
```

Destroys score as critical, and configured Slack/webhook channels fire on findings (notification channels are `admin`-scoped because target URLs are secrets).

---

## Next Steps

- **Set up SSO**: configure OIDC (Entra ID, Keycloak, Okta), SAML, or LDAP — see [initial-setup.md](initial-setup.md) and [configuration.md](configuration.md). Map IdP groups to roles in Administration → OIDC groups.
- **Connect CI for dispatched drift and Version Lab**: add a CI source, run the repo-setup wizard, and confirm the callback preflight is green.
- **Deploy for real**: use the Helm chart or a cloud target in `deployments/` — see [deployment.md](deployment.md). Remember the worker topology: exactly one replica with `TSM_WORKERS_ENABLED=true`.
- **Enable monitoring**: scrape `:9090/metrics` from your internal Prometheus; dashboards and alert rules ship under `deployments/observability/`.
- **Read the ADRs**: the load-bearing decisions are documented in [docs/adr/](adr/README.md).

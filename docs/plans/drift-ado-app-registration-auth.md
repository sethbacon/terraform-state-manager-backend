# Plan: Azure DevOps App-Registration Auth for Drift CI Sources (PAT secondary)

> **Status:** Proposed
> **Repo:** `terraform-state-manager-backend` (+ `terraform-state-manager-frontend`)
> **Scope:** Add Microsoft Entra **app-registration** auth as the primary credential
> for Azure DevOps drift CI sources, keeping the existing **PAT** as a secondary
> fallback. Follows "Option B" (true service/app auth, not user-delegated OAuth).

## 1. Motivation

Drift CI sources today store a single **Personal Access Token** per source
(`ci_sources.encrypted_token`, AES-256-GCM) and dispatch Azure DevOps pipelines
with `Authorization: Basic base64(":"+pat)`
([`internal/pipelines/azuredevops.go`](../backend/internal/pipelines/azuredevops.go)).
PATs are user-bound, max-1-year, and break when the creator leaves. Drift
dispatch is **headless and scheduled**, so a non-expiring, app-owned credential
is the correct model.

Azure DevOps supports OAuth 2.0 **client-credentials** against Microsoft Entra
using the ADO resource id `499b84ac-1321-427f-aa17-267ca6975798`. An admin
registers one app, grants it the needed ADO permissions, and TSM mints
short-lived bearer tokens on demand — no human in the loop, auto-renewed.

## 2. Goals / Non-goals

### Goals

- Admin-created, reusable Entra app credential per ADO org; used by all drift
  dispatches for that org.
- Auto-mint + cache app access tokens; transparent renewal on expiry.
- Keep PAT as a per-source fallback (`auth_method = 'pat' | 'app'`).
- Zero change to the drift **dispatch** call sites beyond token resolution.

### Non-goals

- GitHub (covered by the separate GitHub App plan).
- User-delegated OAuth (explicitly rejected; we want shared service creds).
- Migrating existing PAT sources automatically (they keep working as-is).

## 3. Current-state anchors (verified)

- Dispatch takes a **plaintext token string**:
  `DispatchAzureDevOps(ctx, cfg, token, vars)` in
  [`internal/pipelines/azuredevops.go`](../backend/internal/pipelines/azuredevops.go)
  builds `Authorization: Basic base64(":"+token)`.
- Single token-resolution chokepoint:
  `resolvePipelineToken(ctx, ciRepo, conn)` in
  [`internal/api/ci_sources.go`](../backend/internal/api/ci_sources.go) returns the
  connection token else the referenced CI source's token.
- Schema: [`000011_ci_sources.up.sql`](../backend/internal/db/migrations/000011_ci_sources.up.sql)
  (`provider`, `organization`, `project`, `encrypted_token`).
- Crypto: `crypto.Encrypt/Decrypt` (AES-256-GCM) in
  [`internal/crypto/crypto.go`](../backend/internal/crypto/crypto.go); key from
  `TSM_ENCRYPTION_KEY`.
- RBAC: all `/ci-sources` routes gated by `auth.ScopeSourcesManage`
  ([`internal/api/router.go`](../backend/internal/api/router.go)).
- Next migration number: **`000019`**.

## 4. Design

### 4.1 Auth abstraction

Introduce a provider-agnostic token resolver so dispatch stays unchanged:

```go
// internal/pipelines/credential.go (new)
type DispatchToken struct {
    Value     string    // bearer/PAT to inject
    ExpiresAt time.Time // zero => non-expiring (PAT)
}

type TokenMinter interface {
    // Mint returns a usable token for a CI source, refreshing if needed.
    Mint(ctx context.Context, src *repositories.CISource) (DispatchToken, error)
}
```

`resolvePipelineToken` becomes a thin wrapper that selects a minter by
`ci_sources.auth_method`:

- `pat` → decrypt `encrypted_token` (existing behaviour, unchanged).
- `app` → Entra client-credentials minter (new).

### 4.2 Entra client-credentials minter

```go
// internal/pipelines/entra/minter.go (new)
// POST https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token
//   grant_type=client_credentials
//   client_id={app_client_id}
//   client_secret={app_client_secret}
//   scope=499b84ac-1321-427f-aa17-267ca6975798/.default
// -> { access_token, expires_in }  (~60 min, NO refresh token needed —
//     client-credentials just re-mints)
```

Token cache: in-memory `map[ciSourceID]DispatchToken` guarded by a mutex, with a
60-second safety margin before `expires_at`. Cold start or cache miss re-mints.
No persistence required (re-mintable from stored client secret).

> Reuse note: the registry's Entra **endpoint/scope constants** in
> `terraform-registry-backend/internal/scm/azuredevops/connector.go`
> (`azureDevOpsResourceID`, `.default` handling) are the reference; we implement
> the **client-credentials** grant (the registry does **authorization-code**).
> Keep this minter self-contained in TSM — do not import the registry module.

### 4.3 Schema change — migration `000019_ci_sources_app_auth`

```sql
-- up
ALTER TABLE ci_sources ADD COLUMN auth_method TEXT NOT NULL DEFAULT 'pat'
    CHECK (auth_method IN ('pat', 'app'));

-- Entra app-registration fields (used when auth_method = 'app').
-- tenant_id + client_id are non-secret; client secret is encrypted.
ALTER TABLE ci_sources ADD COLUMN tenant_id TEXT;
ALTER TABLE ci_sources ADD COLUMN client_id TEXT;
ALTER TABLE ci_sources ADD COLUMN encrypted_client_secret BYTEA;

-- encrypted_token (the PAT) becomes nullable: app sources have no PAT.
ALTER TABLE ci_sources ALTER COLUMN encrypted_token DROP NOT NULL;

-- Integrity: exactly the right secret for the chosen method.
ALTER TABLE ci_sources ADD CONSTRAINT ci_sources_auth_shape CHECK (
    (auth_method = 'pat' AND encrypted_token IS NOT NULL)
 OR (auth_method = 'app' AND tenant_id IS NOT NULL
     AND client_id IS NOT NULL AND encrypted_client_secret IS NOT NULL)
);
```

`down`: drop the constraint, columns, and restore `NOT NULL` (only safe if no
`app` rows exist — document that the down migration fails loudly otherwise).

### 4.4 Repository + API

- `CISource` struct ([`ci_source_repository.go`](../backend/internal/db/repositories/ci_source_repository.go)):
  add `AuthMethod string`, `TenantID *string`, `ClientID *string`,
  `EncryptedClientSecret []byte` (JSON `-`).
- `ciSourceJSON` ([`ci_sources.go`](../backend/internal/api/ci_sources.go)): expose
  `auth_method`, `tenant_id`, `client_id`, and `has_client_secret` (bool). **Never**
  expose secrets.
- `CreateCISource` request gains the following fields:

  ```go
  AuthMethod   string `json:"auth_method"`   // "pat" (default) | "app"
  TenantID     string `json:"tenant_id"`     // app only
  ClientID     string `json:"client_id"`     // app only
  ClientSecret string `json:"client_secret"` // app only, encrypted on write
  Token        string `json:"token"`         // pat only (existing)
  ```

  Validate the shape server-side (mirror the DB CHECK) and `crypto.Encrypt` the
  chosen secret.
- New optional endpoint `POST /ci-sources/:id/verify` — mints a token and calls a
  cheap ADO API (`GET /_apis/projects?api-version=7.1`) to confirm the app works;
  returns `{ ok, expires_at }`. Gated by `ScopeSourcesManage`.

### 4.5 Dispatch path

`dispatchDrift` ([`internal/api/drift.go`](../backend/internal/api/drift.go)) keeps
calling `resolvePipelineToken`; only its internals change. `DispatchAzureDevOps`
is unchanged — a minted bearer works with `Basic base64(":"+token)` exactly like a
PAT (ADO accepts Entra access tokens via Basic with empty user, and also via
`Bearer`; keep Basic for a single code path, or branch to `Bearer` for `app`).
**Decide one** — recommended: `Bearer <token>` for `app`, `Basic` for `pat`
(cleanest, matches Entra docs). This is the only dispatch-side edit.

## 5. Frontend (`terraform-state-manager-frontend`)

- `CISourcesDialog` in [`pages/DriftPage.tsx`](../../terraform-state-manager-frontend/frontend/src/pages/DriftPage.tsx):
  add an **Auth method** toggle (App registration | Personal access token).
  - App: tenant id, client id, client secret fields + a "Test connection" button
    hitting `/verify`.
  - PAT: existing token field.
- `api.ts` `createCISource` input: add `auth_method`, `tenant_id`, `client_id`,
  `client_secret`; keep `token` optional.
- `CISource` type: add `auth_method`, `tenant_id`, `client_id`, `has_client_secret`.
- Help text: link to the admin runbook (§7) for app registration setup.

## 6. Security

- Client secret encrypted at rest (AES-256-GCM), `-` in JSON, only `has_client_secret`
  surfaced — same treatment as the PAT today.
- Minted tokens live in memory only; never logged, never persisted.
- Least privilege: document the **minimum ADO permissions** the app registration
  needs (Build: Read & Execute; Project and Team: Read). The app is org-scoped.
- `auth_method` + secret-shape enforced by both API validation and a DB CHECK.

## 7. Operator runbook (docs/deployment)

Add `docs/deployment/ado-app-registration.md`:

1. Create an Entra app registration in the ADO org's tenant.
2. Add a client secret; note tenant id + client id.
3. In Azure DevOps org settings → grant the app's service principal access with the
   minimum permissions above (and add it to the org if "third-party app access via
   OAuth" policy applies).
4. In TSM: Drift → Add CI source → Auth method = App registration → paste
   tenant/client/secret → Test connection.

## 8. Testing

- **Unit:** minter token-cache (hit/miss/expiry margin), client-credentials request
  shaping, error mapping (401/403/AADSTS codes). Table-driven, `httptest` server.
- **Unit:** `resolvePipelineToken` selects minter by `auth_method`; PAT path
  unchanged (regression).
- **Unit:** API validation of `app` vs `pat` request shapes; secret never echoed.
- **Migration:** up creates columns+constraint; existing PAT rows still satisfy the
  CHECK; down fails loudly if `app` rows exist.
- **Frontend:** dialog toggles fields; `createCISource` payload per method;
  "Test connection" calls `/verify`.
- Run `make test` (Go) + `npx vitest run` for the touched FE files; `gosec` baseline
  unchanged.

## 9. Rollout

1. Ship migration + backend behind no flag (additive; default `auth_method='pat'`).
2. Ship FE toggle.
3. Document runbook; pilot one ADO org with an app credential.
4. Existing PAT sources keep working indefinitely (no forced migration).

## 10. Effort / sequencing

1. Migration `000019` + repo struct/columns.
2. `TokenMinter` + Entra minter + cache (most logic; unit-tested in isolation).
3. API create/verify + JSON exposure.
4. Dispatch `Bearer` branch for `app`.
5. Frontend dialog + api/types.
6. Runbook + tests.

Steps 1–4 are backend-only and independently mergeable behind the additive schema.

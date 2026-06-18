# Plan: GitHub App Auth for Drift CI Sources

> **Status:** Proposed
> **Repo:** `terraform-state-manager-backend` (+ `terraform-state-manager-frontend`)
> **Scope:** Add **GitHub App** authentication as the primary credential for GitHub
> drift CI sources, keeping the existing **PAT** as a secondary fallback. Follows
> "Option B" (true service/app auth). Companion to the Azure DevOps app plan.

## 1. Motivation

GitHub drift dispatch today uses a **Personal Access Token** with a `Bearer`
header to call the workflow-dispatch API
([`internal/pipelines/github.go`](../backend/internal/pipelines/github.go)). PATs
are user-bound and expire. The idiomatic GitHub answer for unattended automation
is a **GitHub App**: an admin installs one app on the org/repos, and the backend
mints **short-lived installation access tokens** (~1 h) on demand from the app's
private key. This is *not* the same as the registry's GitHub **OAuth App**
(user-delegated) — a GitHub App with installation tokens is app-owned and headless,
which is exactly what scheduled drift needs.

## 2. Goals / Non-goals

### Goals

- Admin-created, reusable GitHub App credential; one install serves all drift
  dispatches for that org's repos.
- Mint + cache installation tokens; transparent renewal on expiry.
- Keep PAT as a per-source fallback (`auth_method = 'pat' | 'app'`, shared column
  set with the ADO plan).
- No change to drift **dispatch** call sites beyond token resolution.

### Non-goals

- Azure DevOps (separate plan).
- GitHub **OAuth App** / user-delegated flows (rejected; we want shared service auth).
- Webhook-based publish (registry concern, not drift).

## 3. Current-state anchors (verified)

- `DispatchGitHub(ctx, cfg, token, vars)` in
  [`internal/pipelines/github.go`](../backend/internal/pipelines/github.go) sends
  `Authorization: Bearer <token>` to
  `POST /repos/{owner}/{repo}/actions/workflows/{workflow_id}/dispatches`.
  An installation token works verbatim in that header — **no dispatch change**.
- Token chokepoint: `resolvePipelineToken` in
  [`internal/api/ci_sources.go`](../backend/internal/api/ci_sources.go).
- Schema: [`000011_ci_sources.up.sql`](../backend/internal/db/migrations/000011_ci_sources.up.sql).
- Crypto: `crypto.Encrypt/Decrypt` (AES-256-GCM),
  [`internal/crypto/crypto.go`](../backend/internal/crypto/crypto.go).
- RBAC: `auth.ScopeSourcesManage` on all `/ci-sources` routes.
- **Shared migration:** this plan reuses the `auth_method` column introduced by the
  ADO plan's migration `000019`. If the GitHub App plan ships **first**, it owns
  `000019` instead and the ADO plan reuses it. Whichever ships first introduces
  `auth_method` + nullable `encrypted_token`; the second adds only its own columns.

## 4. Design

### 4.1 GitHub App credential model

A GitHub App is defined by: **App ID**, a **private key** (PEM, RSA), and a
per-target **Installation ID**. To mint an installation token:

1. Build a short-lived **app JWT** (RS256), `iss = app_id`, `iat`/`exp` ≤ 10 min,
   signed with the private key.
2. `POST /app/installations/{installation_id}/access_tokens` with
   `Authorization: Bearer <app_jwt>` → `{ token, expires_at }` (~1 h, repo/perm
   scoped to the installation).

Store on the CI source (when `auth_method='app'`):

- `github_app_id` (TEXT, non-secret)
- `github_installation_id` (TEXT, non-secret)
- `encrypted_app_private_key` (BYTEA, the PEM, AES-256-GCM)

### 4.2 Minter (reuses the ADO plan's abstraction)

Implement the same `TokenMinter` interface from the ADO plan
(`internal/pipelines/credential.go`):

```go
// internal/pipelines/githubapp/minter.go (new)
// 1. appJWT := signRS256({iss: appID, iat: now, exp: now+9m}, privateKeyPEM)
// 2. POST /app/installations/{installID}/access_tokens  (Bearer appJWT)
//    -> { token, expires_at }
// returns DispatchToken{Value: token, ExpiresAt: expires_at}
```

Token cache: same in-memory `map[ciSourceID]DispatchToken` with a 60-second margin
as the ADO minter; re-mint on miss/expiry. JWT signing via `golang-jwt` (already in
`go.sum`: `github.com/golang-jwt/jwt`). Confirm the v5 module path before use and
pin explicitly in `go.mod`.

`resolvePipelineToken` selects the minter by `(provider, auth_method)`:
`(github_actions, app)` → GitHub App minter; `(azure_devops, app)` → Entra minter;
`*, pat` → decrypt `encrypted_token`.

### 4.3 Schema — migration (`000019` or `000020`, see §3)

Columns added (GitHub-specific):

```sql
ALTER TABLE ci_sources ADD COLUMN github_app_id TEXT;
ALTER TABLE ci_sources ADD COLUMN github_installation_id TEXT;
ALTER TABLE ci_sources ADD COLUMN encrypted_app_private_key BYTEA;
```

Extend the `ci_sources_auth_shape` CHECK so an `app` source is valid when **either**
the Entra triple **or** the GitHub triple is fully present, keyed on `provider`:

```sql
-- app + github_actions  => github_app_id, github_installation_id, key present
-- app + azure_devops    => tenant_id, client_id, encrypted_client_secret present
-- pat                   => encrypted_token present
```

(If shipping before the ADO plan, this migration also introduces `auth_method` and
makes `encrypted_token` nullable.)

### 4.4 Repository + API

- `CISource`: add `GithubAppID *string`, `GithubInstallationID *string`,
  `EncryptedAppPrivateKey []byte` (JSON `-`).
- `ciSourceJSON`: expose `github_app_id`, `github_installation_id`, and
  `has_app_private_key` (bool). Never expose the key.
- `CreateCISource` request (GitHub app branch):

  ```go
  AuthMethod           string `json:"auth_method"`            // "app"
  GithubAppID          string `json:"github_app_id"`
  GithubInstallationID string `json:"github_installation_id"`
  AppPrivateKey        string `json:"app_private_key"`        // PEM, encrypted on write
  ```

  Validate PEM parses as RSA before storing; `crypto.Encrypt` the key.
- `POST /ci-sources/:id/verify` (shared with ADO plan): mint an installation token
  and call `GET /installation/repositories` (or `GET /app`) to confirm; return
  `{ ok, expires_at, repository_count }`.

### 4.5 Dispatch path

Unchanged: `DispatchGitHub` already uses `Bearer <token>`. The minted installation
token flows through `resolvePipelineToken` exactly like a PAT.

> **Permissions caveat:** dispatching a workflow needs the installation to grant
> **Actions: read & write** (and **Contents: read**). Document this; a missing perm
> surfaces as 403 at dispatch. The `/verify` endpoint should check for it where
> feasible.

## 5. Frontend (`terraform-state-manager-frontend`)

- `CISourcesDialog` ([`pages/DriftPage.tsx`](../../terraform-state-manager-frontend/frontend/src/pages/DriftPage.tsx)):
  when provider = GitHub and Auth method = App, show **App ID**, **Installation ID**,
  and a **private key** textarea (PEM) + "Test connection".
- `api.ts` `createCISource`: add `github_app_id`, `github_installation_id`,
  `app_private_key` (all optional).
- `CISource` type: add `github_app_id`, `github_installation_id`,
  `has_app_private_key`.
- The Auth-method toggle is shared with the ADO plan; the per-method field set keys
  off the selected provider.

## 6. Security

- Private key (PEM) encrypted at rest (AES-256-GCM), `-` in JSON, only
  `has_app_private_key` surfaced.
- App JWTs and installation tokens are in-memory only, short-lived, never logged.
- Least privilege: document the minimum GitHub App permissions (**Actions: R/W**,
  **Contents: R**, **Metadata: R**) and that the app should be installed only on the
  repos drift targets.
- Consider supporting key rotation by allowing the secret to be replaced via
  `PUT /ci-sources/:id` (re-encrypt; invalidate cache entry).

## 7. Operator runbook (docs/deployment)

Add `docs/deployment/github-app.md`:

1. Create a GitHub App (org-owned). Set permissions: Actions R/W, Contents R,
   Metadata R. No webhook needed for drift dispatch.
2. Generate a private key (PEM); note the App ID.
3. Install the app on the target org/repos; note the Installation ID
   (from the install URL or `GET /app/installations`).
4. In TSM: Drift → Add CI source → GitHub → Auth method = App → paste App ID,
   Installation ID, private key → Test connection.

## 8. Testing

- **Unit:** app-JWT signing (RS256, exp window), installation-token request shaping,
  cache hit/miss/expiry, error mapping (401 bad JWT, 404 bad install, 403 perms).
  `httptest` server with a throwaway RSA key.
- **Unit:** `resolvePipelineToken` routing by `(provider, auth_method)`; PAT path
  regression-tested.
- **Unit:** PEM validation rejects non-RSA / malformed keys; key never echoed.
- **Migration:** CHECK accepts each valid shape and rejects partial app configs.
- **Frontend:** provider+method field gating; payload per method; verify call.
- `make test` + targeted `npx vitest run`; `gosec` baseline unchanged (the new
  `crypto/rand`-free JWT signing path may need a baseline note if gosec flags it).

## 9. Rollout

1. Land the shared `auth_method` migration (this plan or the ADO plan, whichever is
   first) + GitHub columns.
2. Backend minter + API (additive, default `pat`).
3. Frontend.
4. Runbook; pilot one org install.
5. Existing PAT GitHub sources keep working.

## 10. Effort / sequencing

1. Shared credential abstraction (`TokenMinter`, `DispatchToken`) — coordinate with
   ADO plan so it lands once.
2. GitHub App minter + JWT signing + cache (unit-tested standalone).
3. Migration columns + CHECK extension.
4. API create/verify + JSON exposure.
5. Frontend.
6. Runbook + tests.

> **Coordination:** the ADO and GitHub App plans share `auth_method`, the
> `TokenMinter` interface, the in-memory cache, and the `/verify` endpoint. Land the
> shared scaffolding in whichever plan ships first; the second plan adds only its
> provider-specific minter, columns, CHECK branch, and FE fields.

# Initial setup

## 1. Generate secrets

```bash
openssl rand -hex 32   # TSM_JWT_SECRET (min 32 chars)
openssl rand -hex 32   # TSM_ENCRYPTION_KEY (64 hex chars = 32 bytes)
```

`TSM_ENCRYPTION_KEY` accepts either 32 raw bytes or 64 hex characters. It
encrypts every stored credential (state-source secrets, CI tokens,
notification targets). **Escrow it** — see
[disaster-recovery.md](disaster-recovery.md).

## 2. Entra ID OIDC {#entra-id-oidc}

1. Entra ID → App registrations → New: name "Terraform State Manager",
   redirect URI (Web) `https://<hostname>/api/v1/auth/callback`.
2. Certificates & secrets → new client secret → store it as the
   `oidc-client-secret` (Key Vault) / `TSM_AUTH_OIDC_CLIENT_SECRET`.
3. Token configuration → Add groups claim → Security groups (the app reads
   the claim named by `TSM_AUTH_OIDC_GROUP_CLAIM_NAME`, default `groups`;
   Entra emits group **object IDs** — map those IDs in the UI).
4. Configure:
   - `TSM_AUTH_OIDC_ISSUER_URL=https://login.microsoftonline.com/<tenant-id>/v2.0`
   - `TSM_AUTH_OIDC_CLIENT_ID=<app-client-id>`
   - `TSM_AUTH_OIDC_REDIRECT_URL=https://<hostname>/api/v1/auth/callback`
   - optionally `TSM_AUTH_OIDC_DEFAULT_ROLE=viewer`

Any OIDC-compliant IdP (Keycloak, Okta, Auth0) works the same way; the dev
stack's Keycloak realm shows a working reference configuration.

## 3. First login and roles

Boot seeds the role templates (`admin`, `editor`, `operator`, `viewer`) and a
default organization. The first user gets a role via either:

- a **group mapping** (Administration → OIDC groups → map IdP group → org +
  role) — create the admin mapping before rollout, or
- `TSM_AUTH_*_DEFAULT_ROLE` on first login (then tighten).

Group mappings reconcile on every login; leaving a mapped group removes the
membership it granted.

## 4. Add your first state source

Sources (HCP Terraform, Azure Blob, S3, GCS, git, Consul, PostgreSQL,
Kubernetes, HTTP, local) are created in the UI; credentials are encrypted
with `TSM_ENCRYPTION_KEY` before storage. Use **Test connection** on the
source card, then watch the dashboard populate as the state-sync worker
backfills the analysis store.

## 5. CI integration

- **Dispatched drift / Version Lab**: connect a CI source (GitHub/ADO), run
  the repo-setup wizard, and confirm the callback preflight is green
  (`TSM_SERVER_CALLBACK_URL` reachable from runners).
- **Push-style drift**: create an **API key** (`/admin/apikeys`, scopes
  `state:read,state:drift`) and POST plan JSON to `/api/v1/drift/ingest`
  with `Authorization: Bearer tsm_…` and a per-run `external_ref`.

## 6. Verify end to end

Browse a state (Analysis/Resources/Outputs/History tabs) → acknowledge a
drift record → rotate the API key → check `/metrics` shows traffic. The e2e
smoke pack in the frontend repo (`e2e/`) automates this against any base URL
via `TSM_E2E_BASE_URL` (cookie-auth flows need DEV_MODE, so against
production run the read-only page specs only).

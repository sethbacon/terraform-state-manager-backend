# Azure DevOps app-registration auth for drift CI sources

Drift CI sources can authenticate to Azure DevOps with a **Microsoft Entra app
registration** instead of a personal access token (PAT). This is the headless,
app-owned credential recommended for scheduled drift: it has no user binding, no
one-year PAT expiry, and is minted on demand by the backend via the OAuth 2.0
**client-credentials** grant. The PAT path remains fully supported as a fallback.

## When to use which

| | PAT (`auth_method: pat`) | App registration (`auth_method: app`) |
| --- | --- | --- |
| Owner | A user | An Entra app (no user) |
| Expiry | Up to 1 year, manual rotation | Tokens auto-minted (~1 h), re-minted on demand |
| Best for | Quick setup, trials | Production / scheduled drift |

## 1. Create the Entra app registration

In the Microsoft Entra tenant that backs your Azure DevOps organization:

1. **Entra admin center → App registrations → New registration.** Name it
   (e.g. `tsm-drift-dispatch`), single-tenant. No redirect URI is needed
   (client-credentials is non-interactive).
2. Note the **Application (client) ID** and the **Directory (tenant) ID**.
3. **Certificates & secrets → New client secret.** Copy the secret **value**
   (shown once). Set an expiry per your rotation policy.

## 2. Grant the app access in Azure DevOps

The app's service principal must be allowed to act in the ADO organization:

1. **Organization settings → Users → Add users**, add the app registration
   (search by its name or client id) as a member.
2. Give it the minimum permissions drift needs:
   - **Build: Read & execute** — to dispatch pipeline runs.
   - **Project and team: Read** — for the discovery/verify calls.
   If your org enforces the *"third-party application access via OAuth"* policy,
   ensure it does not block Entra-issued tokens for the app.

> Least privilege: scope the app to the specific project(s) drift targets where
> your ADO permissions model allows it.

## 3. Add the CI source in TSM

In the app: **Drift → CI sources → Add**, choose **Azure DevOps**, then
**Auth method → App registration**, and enter:

- **Organization** and **Project** (as for a PAT source).
- **Tenant ID**, **Client ID**, **Client secret** from step 1.

Click **Test connection** — TSM mints a token and calls a cheap ADO API to
confirm the credential works (`POST /api/v1/ci-sources/:id/verify`). A failure
usually means the app lacks org access or the secret is wrong/expired.

The client secret is encrypted at rest (AES-256-GCM) and never returned by the
API; only `has_client_secret: true` is reported.

## 4. Rotation

When the Entra client secret nears expiry, create a new secret in the app
registration and update the CI source's credential. The backend caches minted
access tokens only in memory and keys the cache on the credential, so a rotated
secret takes effect on the next dispatch with no restart required.

## Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| `Test connection` 401 / `AADSTS7000215` | Wrong client secret, or it expired. |
| `Test connection` 403 | App lacks org access or the required ADO permissions. |
| Dispatch fails with 401/403 but verify passed | The target pipeline needs Build **execute**; verify only checks read. |
| `encryption key not configured` | `TSM_ENCRYPTION_KEY` is unset/invalid — required to store the secret. |

See also: [CI callback reachability](README.md#ci-callback-reachability) — the
drift CI job still POSTs results back to `TSM_SERVER_CALLBACK_URL` regardless of
which credential dispatched it.

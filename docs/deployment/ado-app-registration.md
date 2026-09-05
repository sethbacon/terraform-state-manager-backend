# Azure DevOps app-registration auth for drift CI sources

Drift CI sources can authenticate to Azure DevOps with a **Microsoft Entra app
registration** instead of a personal access token (PAT). This is the headless,
app-owned credential recommended for scheduled drift: it has no user binding, no
one-year PAT expiry, and is minted on demand by the backend via the OAuth 2.0
**client-credentials** grant. The PAT path remains fully supported as a fallback.

On AKS, **Workload Identity** (`auth_method: workload_identity`) is the preferred
option over an app registration: TSM's own pod identity federates directly to a
dedicated Azure managed identity, so there is no client secret anywhere — not in
TSM, not in Key Vault, not to rotate. See §5.

## When to use which

| | PAT (`auth_method: pat`) | App registration (`auth_method: app`) | Managed identity (`auth_method: workload_identity`) |
| --- | --- | --- | --- |
| Owner | A user | An Entra app (no user) | An Azure managed identity (no user, no secret) |
| Credential stored in TSM | Encrypted PAT | Encrypted client secret | None — only the non-secret client id |
| Expiry | Up to 1 year, manual rotation | Tokens auto-minted (~1 h), re-minted on demand | Tokens auto-minted (~1 h); the federated trust itself does not expire |
| Requires | Nothing beyond ADO | An Entra tenant you can register an app in | AKS with Workload Identity enabled (`docs/deployment/aks-prerequisites.md`) |
| Best for | Quick setup, trials | Production / scheduled drift, non-AKS installs | Production / scheduled drift on AKS |

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
registration and send it to TSM via `PUT /api/v1/ci-sources/{id}` with the
SAME `tenant_id`/`client_id` and the new `client_secret` — this replaces the
credential **in place**, so the row's id
(and every pipeline connection that borrows it via `config.ci_source_id`)
keeps working with no re-registration. The backend re-encrypts the new secret,
evicts whatever token was cached under the old one, and audits the change as
`ci_source.update`. A rotated secret is therefore effective on the very next
dispatch, with no restart required and no window where a stale cached token
could be served under the credential being replaced.

The same endpoint is how an existing `app` source is **moved to Workload
Identity** (§5) without deleting it — send `auth_method: workload_identity`
and `client_id` (the managed identity's), with no secret fields.

## 5. Managed identity (Workload Identity)

On AKS, prefer a **user-assigned managed identity federated to TSM's own pod
identity** over an Entra app registration: no client secret is minted, stored,
or rotated at all — TSM exchanges its Kubernetes ServiceAccount token directly
for an Azure DevOps access token via `azidentity.NewWorkloadIdentityCredential`.
This is the same federation recipe the chart's Key Vault identity already uses
(`docs/deployment/aks-prerequisites.md` §5), applied to a **separate** managed
identity so a Key Vault compromise and an ADO-dispatch compromise stay
independent blast radii.

1. **Create the managed identity and federate it to TSM's ServiceAccount**
   (adjust the release name / namespace to match your deployment):

   ```bash
   az identity create -n tsm-ado-dispatch -g $RG
   export ADO_CLIENT_ID=$(az identity show -n tsm-ado-dispatch -g $RG --query clientId -o tsv)
   az identity federated-credential create -n tsm-ado-dispatch-sa \
     --identity-name tsm-ado-dispatch -g $RG \
     --issuer "$OIDC_ISSUER" \
     --subject "system:serviceaccount:terraform-state-manager:tsm-terraform-state-manager" \
     --audiences api://AzureADTokenExchange
   ```

   One Kubernetes ServiceAccount token can be exchanged for any identity that
   federates its subject, so this stays a **separate** managed identity from
   `tsm-app-identity` (the Key Vault one) even though the subject is the same.

2. **Add the managed identity to the Azure DevOps organization.** Organization
   settings → Users → Add users, search by the identity's name or
   `$ADO_CLIENT_ID`, access level **Basic**. Managed identities have been
   first-class ADO users since 2023.

3. **Grant the minimum permissions** — the same set app-registration auth
   needs (§2): **Build: Read & execute** (dispatch) and **Project and team:
   Read** (discovery/verify). Grant **Code: Read** on the repos any discovery
   call enumerates. **Do not grant Contribute** — TSM never writes to a repo
   in this design; the repo-setup wizard commits under the operator's own
   credential, not the dispatch identity's.

4. **Add (or move) the CI source in TSM.** For a new source: **Drift → CI
   sources → Add**, provider **Azure DevOps**, **Auth method → Managed
   identity (Workload Identity)**, enter **Organization**, **Project**, and
   the identity's **Client ID** (`$ADO_CLIENT_ID` above) — no tenant id, no
   secret. To move an *existing* `app` source instead of creating a new row,
   see §4's last paragraph. Click **Test connection**.

   No pod restart or extra Kubernetes RBAC is needed beyond what the AKS
   Workload Identity webhook already grants the chart's ServiceAccount
   (`docs/deployment/aks-prerequisites.md` §5) — `AZURE_TENANT_ID` and
   `AZURE_FEDERATED_TOKEN_FILE` are injected into the pod automatically, and
   only the client id above is TSM's own configuration.

## Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| `Test connection` 401 / `AADSTS7000215` | Wrong client secret, or it expired (app registration). |
| `Test connection` 403 | The app/managed identity lacks org access or the required ADO permissions. |
| Dispatch fails with 401/403 but verify passed | The target pipeline needs Build **execute**; verify only checks read. |
| `encryption key not configured` | `TSM_ENCRYPTION_KEY` is unset/invalid — required to store an app registration's secret (not needed for Workload Identity, which stores no secret). |
| `Test connection` fails with `AADSTS70021` or "no matching federated identity record found" | The federated credential's `--subject` does not match the pod's actual ServiceAccount (namespace/release name typo), or it was created on the wrong managed identity. |
| `Test connection` fails with `AADSTS700213` | The federated token's issuer/audience does not match what the federated credential expects — usually a stale `$OIDC_ISSUER` from a cluster that was recreated (AKS's OIDC issuer URL changes if you disable and re-enable the feature). |
| "identity is not a member of the organization" | Step 2 (adding the managed identity as an ADO user) was skipped, or it was added to a different organization than the one the CI source names. |
| Workload Identity works from a shell (`az account get-access-token`) but not from TSM's pod | That command uses YOUR login, not the pod's federated identity — confirm the pod actually has the `azure.workload.identity/use: "true"` label and the ServiceAccount annotation from `aks-prerequisites.md`, not just that the identity exists. |

See also: [CI callback reachability](README.md#ci-callback-reachability) — the
drift CI job still POSTs results back to `TSM_SERVER_CALLBACK_URL` regardless of
which credential dispatched it.

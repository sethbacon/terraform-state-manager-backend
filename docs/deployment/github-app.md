# GitHub App auth for drift CI sources

Drift CI sources can authenticate to GitHub with a **GitHub App** instead of a
personal access token (PAT). This is the headless, app-owned credential
recommended for scheduled drift: the backend mints short-lived **installation
access tokens** on demand by signing an app JWT with the app's private key — no
user binding, no PAT expiry. The PAT path remains fully supported as a fallback.

> Distinct from a GitHub **OAuth App** (user-delegated). A GitHub App with
> installation tokens is app-owned and non-interactive, which is what scheduled
> drift needs.

## When to use which

|          | PAT (`auth_method: pat`) | GitHub App (`auth_method: app`)        |
| -------- | ------------------------ | -------------------------------------- |
| Owner    | A user                   | A GitHub App installation (no user)    |
| Expiry   | Manual rotation          | Installation tokens auto-minted (~1 h) |
| Best for | Quick setup, trials      | Production / scheduled drift           |

## 1. Create the GitHub App

In the org that owns the repos drift targets: **Settings → Developer settings →
GitHub Apps → New GitHub App.**

1. Name it (e.g. `tsm-drift-dispatch`). A homepage URL is required but unused.
2. **Permissions → Repository permissions:**
   - **Actions: Read and write** — to dispatch workflow runs.
   - **Contents: Read-only** — for repo/workflow discovery.
   - **Metadata: Read-only** (mandatory).
   No webhook is needed for drift dispatch — uncheck **Active** under Webhook.
3. Create the app and note the **App ID**.
4. **Generate a private key** (Settings → the app → Private keys). A `.pem`
   downloads — keep it safe; GitHub shows it once.

## 2. Install the app

1. From the app's page, **Install App** on the org, scoped to **only** the repos
   drift targets (least privilege).
2. Note the **Installation ID** — it is the number in the install settings URL:
   `https://github.com/organizations/<org>/settings/installations/<INSTALLATION_ID>`.

## 3. Add the CI source in TSM

**Drift → CI sources → Add**, choose **GitHub Actions**, then **Auth method →
App registration**, and enter:

- **Owner** (org or user) as for a PAT source.
- **App ID**, **Installation ID**, and the **private key** (paste the PEM
  contents).

Click **Test connection** — TSM mints an installation token and lists the
installation's repositories to confirm (`POST /api/v1/ci-sources/:id/verify`). A
failure usually means a wrong app/installation id, a bad/rotated key, or the app
is not installed on any repo.

The private key is encrypted at rest (AES-256-GCM) and never returned by the
API; only `has_app_private_key: true` is reported.

## 4. Rotation

Generate a new private key on the GitHub App, then update the CI source's key.
The backend caches installation tokens only in memory and keys the cache on the
credential, so a rotated key takes effect on the next dispatch with no restart.

## Troubleshooting

| Symptom                              | Likely cause                                                       |
| ------------------------------------ | ------------------------------------------------------------------ |
| `Test connection` 401                | Wrong App ID, or the private key does not match the app.           |
| `Test connection` 404                | Wrong Installation ID, or the app is not installed.                |
| Dispatch fails 403 but verify passed | The installation lacks **Actions: write** on the target repo.      |
| `encryption key not configured`      | `TSM_ENCRYPTION_KEY` is unset/invalid — required to store the key. |

See also: [Azure DevOps app-registration auth](ado-app-registration.md) for the
equivalent ADO setup, and [CI callback reachability](README.md#ci-callback-reachability).

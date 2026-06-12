# Azure Container Apps

Serverless Azure without a cluster: `deployments/azure-container-apps`
(Bicep). Three apps — `backend` (internal), `tsm-worker` (no ingress, pinned
min=max=1), `tsm-frontend` (external).

**Why the backend app is named literally `backend`:** the frontend image's
nginx proxies to `http://backend:8080` (the compose-era service name). ACA
name-based resolution plus an `additionalPortMappings` entry exposing 8080
makes that work without modifying the image. Don't rename the app.

```bash
# Prereqs: RG, ACR with both images, Azure Database for PostgreSQL Flexible.
cd deployments/azure-container-apps
cp parameters.json my-parameters.json   # fill in every <PLACEHOLDER>
RESOURCE_GROUP=<rg> ./deploy.sh -p @my-parameters.json
```

Outputs include the frontend URL — set a custom domain + managed cert on the
frontend app for production and put that hostname in `customDomain`
(it feeds `TSM_SERVER_*_URL`, which the CI drift callbacks rely on).

Caveats vs AKS: secrets live as ACA secrets (use Key Vault references in the
portal/CLI for vault-backed values); scale-to-zero is disabled for the worker
by design (a sleeping worker fires no schedules); the backend autoscales on
HTTP concurrency only.

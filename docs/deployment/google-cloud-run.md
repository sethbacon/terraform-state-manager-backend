# Google Cloud Run

`deployments/google-cloud-run` — three services: `tsm-backend`, `tsm-worker`
(min=max=1 with CPU always allocated: the schedule/sync loops must run
between requests), `tsm-frontend`.

**Frontend nginx caveat:** the image's baked config proxies to
`http://backend:8080`, which cannot resolve on Cloud Run — nginx would fail
at startup. `deploy.sh` therefore generates a corrected `default.conf`
pointing at the backend's `run.app` URL (HTTPS with SNI), stores it in Secret
Manager (`tsm-frontend-nginx`), and the frontend spec mounts it over
`/etc/nginx/conf.d`. Re-run `deploy.sh` if the backend URL ever changes.

```bash
# Prereqs: Cloud SQL PostgreSQL, Secret Manager secrets tsm-jwt-secret /
# tsm-encryption-key / tsm-database-password, images in Artifact Registry,
# and the Cloud Run service account granted secretAccessor on those secrets.
cd deployments/google-cloud-run
PROJECT_ID=<project> REGION=<region> ./deploy.sh
```

**Reaching Cloud SQL:** the backend/worker manifests connect over the
instance's IP with TLS — set `TSM_DATABASE_HOST=<CLOUD_SQL_IP_OR_DNS>` and keep
`TSM_DATABASE_SSL_MODE=require` (both are placeholders in `backend-service.yaml`
/ `worker-service.yaml`). Prefer the private-IP path; if you instead use the
Cloud SQL Auth Proxy, add `--add-cloudsql-instances <conn-name>` to the service
and point `TSM_DATABASE_HOST` at the proxy socket/host.

For a single custom domain, front both services with an external HTTPS load
balancer (serverless NEGs) routing `/api/*`, `/scim/*` → backend and `/*` →
frontend, or simply use the frontend URL (it proxies API paths) and map your
domain to it.

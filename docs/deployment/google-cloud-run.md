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

For a single custom domain, front both services with an external HTTPS load
balancer (serverless NEGs) routing `/api/*`, `/scim/*` → backend and `/*` →
frontend, or simply use the frontend URL (it proxies API paths) and map your
domain to it.
